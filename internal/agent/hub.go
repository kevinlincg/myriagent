package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Hub 是 agent 的中樞（對應 C# 的 AgentHub）：狀態全在這裡，網頁只是顯示層。
// 所有狀態都在 mu 底下改；送給網頁的訊息先排進各 view 自己的佇列，由各 view 的 goroutine 寫出去，不會卡住中樞。
type Hub struct {
	mu   sync.Mutex
	root string

	settings Settings
	accounts []*Account
	convs    map[string]*Conv
	views    map[*View]struct{}
	nextView int
	clis     map[string]*cliInfo

	flushKick chan struct{}
}

// View 是一個連上的網頁（一個視窗或瀏覽器分頁）
type View struct {
	id      int
	out     chan []byte
	mode    string
	ready   bool
	open    map[string]bool
	visible map[string]bool
	closed  bool
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func nowMs() int64 { return time.Now().UnixMilli() }

func itoa(i int) string { return strconv.Itoa(i) }

func newHub(root string) *Hub {
	h := &Hub{root: root, convs: map[string]*Conv{}, views: map[*View]struct{}{}, clis: map[string]*cliInfo{}, flushKick: make(chan struct{}, 1)}
	h.settings = loadSettings(root)
	h.detectCLIs()
	h.loadAccounts()
	h.loadConversations()
	go h.flushLoop()
	for _, f := range startHooks {
		go f(h)
	}
	return h
}

// ---- views ----

func (h *Hub) addView(write func([]byte) error) *View {
	h.mu.Lock()
	h.nextView++
	v := &View{id: h.nextView, out: make(chan []byte, 4096), open: map[string]bool{}, visible: map[string]bool{}}
	h.views[v] = struct{}{}
	h.mu.Unlock()
	go func() {
		for b := range v.out {
			if err := write(b); err != nil {
				return
			}
		}
	}()
	return v
}

func (h *Hub) removeView(v *View) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.views[v]; !ok {
		return
	}
	delete(h.views, v)
	v.closed = true
	close(v.out)
	// 只開在這個 view 的草稿跟著丟掉
	for id := range v.open {
		if c := h.convs[id]; c != nil && c.Meta.IsDraft && !h.openElsewhere(id, v) {
			delete(h.convs, id)
		}
	}
}

func (h *Hub) openElsewhere(id string, except *View) bool {
	for v := range h.views {
		if v != except && v.open[id] {
			return true
		}
	}
	return false
}

// sendTo 排一則訊息給一個 view（呼叫時持有 mu）
func (h *Hub) sendTo(v *View, msg any) {
	if v.closed {
		return
	}
	b, err := json.Marshal(msg)
	if err != nil {
		log.Printf("訊息轉 JSON 失敗：%v", err)
		return
	}
	select {
	case v.out <- b:
	default:
		log.Printf("view %d 的送出佇列滿了，丟掉一則 %T", v.id, msg)
	}
}

func (h *Hub) broadcast(msg any) {
	for v := range h.views {
		if v.ready {
			h.sendTo(v, msg)
		}
	}
}

func (h *Hub) sendToOpen(convID string, msg any) {
	for v := range h.views {
		if v.ready && v.open[convID] {
			h.sendTo(v, msg)
		}
	}
}

func (h *Hub) toast(v *View, text string) {
	h.sendTo(v, map[string]any{"type": "toast", "text": text})
}

// ---- 清單 ----

func groupLabel(updated, now time.Time) string {
	y1, m1, d1 := updated.Date()
	y2, m2, d2 := now.Date()
	days := time.Date(y2, m2, d2, 0, 0, 0, 0, time.Local).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, time.Local)).Hours() / 24
	switch {
	case days <= 0:
		return "今天"
	case days <= 1:
		return "昨天"
	case days <= 7:
		return "過去 7 天"
	case days <= 30:
		return "過去 30 天"
	case y1 == y2:
		return fmt.Sprintf("%d 月", int(m1))
	default:
		return fmt.Sprintf("%d 年 %d 月", y1, int(m1))
	}
}

func (h *Hub) conversationList() []ConversationSummary {
	now := time.Now()
	list := []ConversationSummary{}
	for _, c := range h.convs {
		if c.Meta.IsDraft {
			continue
		}
		list = append(list, ConversationSummary{
			ID: c.Meta.ID, AccountID: c.Meta.AccountID, Provider: c.Meta.Provider, Title: c.Meta.Title,
			Group: groupLabel(time.UnixMilli(c.Meta.UpdatedAt), now), UpdatedAt: c.Meta.UpdatedAt,
			WorkingDirectory: c.workDir(h), CustomFolder: c.Meta.CustomFolder,
			Running: c.running, Waiting: c.waiting(), Unread: c.unread,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	if len(list) > 1000 {
		list = list[:1000]
	}
	return list
}

func (h *Hub) pushConversations() {
	h.broadcast(map[string]any{"type": "conversations", "items": h.conversationList()})
}

func (h *Hub) pushAccounts() {
	h.broadcast(map[string]any{"type": "accounts", "accounts": h.accountViews()})
}

// ---- 定時把串流中的變更送出去（同 C# 每 50 ms 合併一次） ----

func (h *Hub) kick() {
	select {
	case h.flushKick <- struct{}{}:
	default:
	}
}

func (h *Hub) flushLoop() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	save := time.NewTicker(2 * time.Second)
	defer save.Stop()
	for {
		select {
		case <-t.C:
		case <-h.flushKick:
		case <-save.C:
			h.mu.Lock()
			for _, c := range h.convs {
				if c.needSave {
					c.save(h)
				}
			}
			h.mu.Unlock()
			continue
		}
		h.mu.Lock()
		listChanged := false
		for _, c := range h.convs {
			if c.flush(h) {
				listChanged = true
			}
		}
		if listChanged {
			h.pushConversations()
			h.pushAccounts()
		}
		h.mu.Unlock()
	}
}

// shutdown 停掉所有 CLI、存檔
func (h *Hub) shutdown() {
	h.mu.Lock()
	closing := 0
	for _, c := range h.convs {
		c.save(h)
		if h.alive(c) {
			closing++
		}
		h.closeSession(c)
	}
	h.mu.Unlock()
	// 各家的 Close 不等程序結束：給它們一點時間收尾
	if closing > 0 {
		time.Sleep(2500 * time.Millisecond)
	}
}

// initExtras：各功能在 init 訊息裡加的欄位（registerInitField 登記；呼叫時持有 h.mu）
var initExtras = map[string]func(h *Hub, v *View) any{}

func registerInitField(key string, f func(h *Hub, v *View) any) { initExtras[key] = f }

// startHooks：中樞建好之後各功能要啟動的背景工作（registerStartHook 登記；不持有 h.mu）
var startHooks []func(h *Hub)

func registerStartHook(f func(h *Hub)) { startHooks = append(startHooks, f) }
