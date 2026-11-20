package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kevinlincg/myriagent/internal/claude"
	"github.com/kevinlincg/myriagent/internal/render"
)

// ConvMeta 是 conversations/{id}/meta.json
type ConvMeta struct {
	ID               string `json:"id"`
	AccountID        string `json:"accountId"`
	Provider         string `json:"provider"`
	Title            string `json:"title"`
	Model            string `json:"model"`
	Effort           string `json:"effort"`
	Permission       string `json:"permission"`
	WorkingDirectory string `json:"workingDirectory,omitempty"` // 自訂資料夾；空＝對話自己的 workspace
	CustomFolder     bool   `json:"customFolder"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
	SessionID        string `json:"sessionId"`
	SessionStarted   bool   `json:"sessionStarted"`
	Seq              int64  `json:"seq"`
	LastTurn         string `json:"lastTurn,omitempty"`
	IsDraft          bool   `json:"-"`
}

// Conv 是一個對話：存檔的部分（Meta、Items）與執行中的狀態
type Conv struct {
	Meta  ConvMeta
	Items []*Item

	sess      *claude.Session // Claude 的程序（claudeconv.go）
	proc      any             // 其他代理種類的程序狀態（各家自己的型別）
	running   bool
	stopping  bool
	status    string
	startedAt int64
	queue     []QueuedMessage
	unread    bool

	// 串流中：訊息 id → 依序還沒收尾的文字／思考項目；工具 id → 工具卡
	streamText map[string][]*Item
	curMessage string
	curBlock   *Item
	toolItems  map[string]*Item
	// 等使用者回應的 CLI 請求：項目 id → 請求
	pending map[string]*pendingRequest

	dirtyItems  map[string]bool
	dirtyHeader bool
	dirtyComp   bool
	dirtyList   bool
	needSave    bool
}

type pendingRequest struct {
	requestID   string
	toolName    string
	input       map[string]any
	suggestions []any
}

func (h *Hub) convDir(id string) string { return filepath.Join(h.root, "conversations", id) }

func (c *Conv) workDir(h *Hub) string {
	if c.Meta.CustomFolder && c.Meta.WorkingDirectory != "" {
		return c.Meta.WorkingDirectory
	}
	return filepath.Join(h.convDir(c.Meta.ID), "workspace")
}

func (c *Conv) init() {
	c.streamText = map[string][]*Item{}
	c.toolItems = map[string]*Item{}
	c.pending = map[string]*pendingRequest{}
	c.dirtyItems = map[string]bool{}
}

func (h *Hub) loadConversations() {
	dirs, _ := os.ReadDir(filepath.Join(h.root, "conversations"))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(h.root, "conversations", d.Name())
		var meta ConvMeta
		b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil || json.Unmarshal(b, &meta) != nil || meta.ID == "" {
			continue
		}
		c := &Conv{Meta: meta}
		c.init()
		if b, err := os.ReadFile(filepath.Join(dir, "items.json")); err == nil {
			_ = json.Unmarshal(b, &c.Items)
		}
		// 上次結束時還在跑或等回應的卡片：程序已經不在了
		for _, it := range c.Items {
			if it.Status == "Running" || it.Status == "Waiting" {
				it.Status = "Cancelled"
				if it.Kind == "Approval" || it.Kind == "Question" || it.Kind == "PlanApproval" {
					it.Output = "CLI 已重新啟動，這張卡已失效；代理會視需要重新提出"
				}
			}
		}
		h.convs[meta.ID] = c
	}
}

func (c *Conv) save(h *Hub) {
	if c.Meta.IsDraft {
		return
	}
	dir := h.convDir(c.Meta.ID)
	writeJSON(filepath.Join(dir, "meta.json"), c.Meta)
	writeJSON(filepath.Join(dir, "items.json"), c.Items)
	c.needSave = false
}

func (c *Conv) waiting() bool {
	for _, it := range c.Items {
		if it.Status == "Waiting" && (it.Kind == "Approval" || it.Kind == "Question" || it.Kind == "PlanApproval") {
			return true
		}
	}
	return false
}

// ---- 項目 ----

func (c *Conv) addItem(kind string) *Item {
	c.Meta.Seq++
	it := &Item{ID: fmt.Sprintf("i%d", c.Meta.Seq), Kind: kind, Status: "Done", Time: nowMs(), Seq: c.Meta.Seq}
	c.Items = append(c.Items, it)
	c.touch(it)
	return it
}

func (c *Conv) touch(it *Item) {
	c.dirtyItems[it.ID] = true
	c.needSave = true
}

func (c *Conv) item(id string) *Item {
	for i := len(c.Items) - 1; i >= 0; i-- {
		if c.Items[i].ID == id {
			return c.Items[i]
		}
	}
	return nil
}

func (c *Conv) notice(h *Hub, kind, text string) {
	it := c.addItem("Notice")
	it.NoticeKind = kind
	it.HTML = render.Text(text)
}

func (c *Conv) errorItem(text string) {
	it := c.addItem("Error")
	it.Status = "Failed"
	it.HTML = render.Markdown(text)
	it.Text = text // 原文（語音通知念失敗時取第一行）
}

// flush 把累積的變更送給開著這個對話的 view；回傳清單是不是要重送
func (c *Conv) flush(h *Hub) bool {
	if len(c.dirtyItems) > 0 {
		ids := make([]string, 0, len(c.dirtyItems))
		for id := range c.dirtyItems {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return seqOf(c, ids[i]) < seqOf(c, ids[j]) })
		for _, id := range ids {
			if it := c.item(id); it != nil {
				if it.Text != "" && (it.Kind == "Assistant" || it.Kind == "Thinking") {
					it.HTML = render.Markdown(it.Text)
				}
				h.sendToOpen(c.Meta.ID, map[string]any{"type": "item", "conversationId": c.Meta.ID, "item": it})
			}
		}
		c.dirtyItems = map[string]bool{}
	}
	if c.dirtyHeader {
		h.sendToOpen(c.Meta.ID, map[string]any{"type": "header", "conversationId": c.Meta.ID, "header": h.header(c)})
		c.dirtyHeader = false
	}
	if c.dirtyComp {
		h.sendToOpen(c.Meta.ID, map[string]any{"type": "composer", "conversationId": c.Meta.ID, "composer": c.composer()})
		c.dirtyComp = false
	}
	list := c.dirtyList
	c.dirtyList = false
	return list
}

func seqOf(c *Conv, id string) int64 {
	if it := c.item(id); it != nil {
		return it.Seq
	}
	return 0
}

// changed 標記標頭與清單要重送（狀態改變時）
func (c *Conv) changed() {
	c.dirtyHeader = true
	c.dirtyList = true
	c.needSave = true
}

// ---- 送給網頁的形狀 ----

func (h *Hub) header(c *Conv) Header {
	a := h.account(c.Meta.AccountID)
	hd := Header{
		ID: c.Meta.ID, AccountID: c.Meta.AccountID, Provider: c.Meta.Provider, Title: c.Meta.Title,
		Model: orDefault(c.Meta.Model, "default"), Effort: orDefault(c.Meta.Effort, "default"),
		Permission: orDefault(c.Meta.Permission, "Ask"), PermissionModes: permissionModes(c.Meta.Provider),
		Models: []Option{defaultModelOption}, Commands: []Command{}, WorkingDirectory: c.workDir(h), CustomFolder: c.Meta.CustomFolder,
		RecentFolders: append([]string{}, h.settings.RecentFolders...), IsDraft: c.Meta.IsDraft, Running: c.running,
		Steer: h.provider(c) != nil && h.provider(c).Steer(), AccountMissing: a == nil,
		AccountDelegate: false,
	}
	if a != nil {
		hd.Models = a.modelOptions()
		hd.Commands = append(hd.Commands, a.commands...)
	}
	if !hasCommand(hd.Commands, "clear") {
		hd.Commands = append(hd.Commands, Command{Name: "clear", Description: "清除對話，從頭開始"})
	}
	if c.running {
		st := c.status
		if st == "" {
			st = "思考中…"
		}
		hd.Status = &st
		sa := c.startedAt
		hd.StartedAt = &sa
	}
	if c.Meta.LastTurn != "" {
		lt := c.Meta.LastTurn
		hd.LastTurn = &lt
	}
	hd.LastPrompt = c.lastPrompt()
	hd.Programs = h.headerPrograms(c)
	return hd
}

func hasCommand(list []Command, name string) bool {
	for _, c := range list {
		if c.Name == name {
			return true
		}
	}
	return false
}

func (c *Conv) lastPrompt() *Prompt {
	for i := len(c.Items) - 1; i >= 0; i-- {
		it := c.Items[i]
		if it.Kind == "Notice" && it.NoticeKind == "clear" {
			return nil
		}
		if it.Kind == "User" {
			text := strings.ReplaceAll(it.Text, "\r", "")
			if utf8.RuneCountInString(text) > 2000 {
				text = string([]rune(text)[:2000]) + "…"
			}
			return &Prompt{ID: it.ID, Seq: it.Seq, Time: it.Time, Text: text, Attachments: len(it.Attachments)}
		}
	}
	return nil
}

func (c *Conv) composer() Composer {
	return Composer{Attachments: []Attachment{}, Queue: append([]QueuedMessage{}, c.queue...)}
}

const pageSize = 80

// conversationMsg：整個對話的最後一頁
func (h *Hub) conversationMsg(c *Conv, paneID any) map[string]any {
	items := c.Items
	hasMore := false
	if len(items) > pageSize {
		items = items[len(items)-pageSize:]
		hasMore = true
	}
	return map[string]any{
		"type": "conversation", "conversationId": c.Meta.ID, "paneId": paneID, "header": h.header(c),
		"items": items, "hasMore": hasMore, "composer": c.composer(), "draft": nil,
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
