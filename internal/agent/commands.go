package agent

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// handle 處理網頁送來的一則指令（docs/web-protocol.md 第二節）
func (h *Hub) handle(v *View, msg map[string]any) {
	cmd := str(msg, "cmd")
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	if v.closed {
		return
	}
	id := str(msg, "conversationId")
	c := h.convs[id]
	switch cmd {
	case "ready":
		v.mode = orDefault(str(msg, "mode"), "main")
		v.ready = true
		h.sendTo(v, h.initMsg(v, str(msg, "conversationId")))
	case "layout":
		if v.mode == "main" {
			h.settings.Layout = msg["layout"]
			h.saveSettings()
		}
	case "visible":
		v.visible = map[string]bool{}
		ids, _ := msg["conversationIds"].([]any)
		for _, x := range ids {
			if s, ok := x.(string); ok {
				v.visible[s] = true
				if cc := h.convs[s]; cc != nil && cc.unread {
					cc.unread = false
					cc.dirtyList = true
				}
			}
		}
	case "newConversation":
		h.newConversation(v, str(msg, "accountId"), msg["paneId"])
	case "open":
		if c == nil {
			h.sendTo(v, map[string]any{"type": "conversationClosed", "conversationId": id})
			return
		}
		v.open[id] = true
		if c.unread && v.visible[id] {
			c.unread = false
			c.dirtyList = true
		}
		h.sendTo(v, h.conversationMsg(c, nil))
	case "close":
		delete(v.open, id)
		delete(v.visible, id)
		if c != nil && c.Meta.IsDraft && !h.openElsewhere(id, nil) {
			delete(h.convs, id)
		}
	case "loadOlder":
		if c != nil {
			h.sendTo(v, h.olderItems(c, str(msg, "beforeId")))
		}
	case "send":
		if c != nil {
			h.send(v, c, str(msg, "text"))
		}
	case "stop":
		if c != nil && c.running {
			c.stopping = true
			c.status = "停止中…"
			c.dirtyHeader = true
			if p := h.provider(c); p != nil {
				p.Stop(h, c)
			}
		}
	case "cancelQueued":
		if c != nil {
			qid := str(msg, "id")
			for i, q := range c.queue {
				if q.ID == qid {
					c.queue = append(c.queue[:i:i], c.queue[i+1:]...)
					c.dirtyComp = true
					break
				}
			}
		}
	case "approve", "approvePlan", "answer", "skipQuestion":
		if c != nil {
			h.respondCard(c, cmd, msg)
		}
	case "rename":
		if c != nil {
			t := strings.TrimSpace(str(msg, "title"))
			if t != "" {
				c.Meta.Title = truncateRunes(t, 80)
				c.changed()
			}
		}
	case "delete":
		if c != nil {
			h.deleteConversation(c)
		}
	case "setOption":
		if c != nil {
			h.setOption(v, c, str(msg, "key"), str(msg, "value"))
		}
	case "setAccount":
		if c != nil {
			h.setAccount(v, c, str(msg, "accountId"))
		}
	case "setFolder":
		if c != nil {
			h.setFolder(v, c, str(msg, "path"))
		}
	case "resetFolder":
		if c != nil && !c.running {
			c.Meta.CustomFolder, c.Meta.WorkingDirectory = false, ""
			h.restartSession(c)
			c.changed()
		}
	case "chooseFolder":
		if c != nil {
			go h.chooseFolder(v, c.Meta.ID)
		}
	case "openFolder":
		if c != nil {
			openInFileManager(c.workDir(h))
		}
	case "openPath", "revealPath":
		if p := str(msg, "path"); p != "" {
			if cmd == "revealPath" {
				revealInFileManager(p)
			} else {
				openInFileManager(p)
			}
		}
	case "openUrl":
		if u := str(msg, "url"); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
			openInFileManager(u)
		}
	case "setting":
		h.applySetting(v, str(msg, "key"), msg["value"])
	case "setConversationDefault":
		h.setConversationDefault(str(msg, "provider"), str(msg, "key"), msg["value"])
	case "clientError":
		log.Printf("網頁錯誤：%v", msg["message"])
	case "monitor", "promptHistory", "listExternalConversations":
		// 還沒移植的功能：不回應（網頁照舊顯示，不會卡住）
	default:
		if f := commandHandlers[cmd]; f != nil {
			f(h, v, c, msg)
		} else if cmd != "" {
			log.Printf("還沒支援的指令：%s", cmd)
		}
	}
}

// commandHandlers：各功能自己登記的指令（在各自檔案的 init 裡呼叫 registerCommand），不必改上面的 switch。
// 呼叫時持有 h.mu；c 是 msg.conversationId 對到的對話（沒有是 nil）
var commandHandlers = map[string]func(h *Hub, v *View, c *Conv, msg map[string]any){}

func registerCommand(name string, f func(h *Hub, v *View, c *Conv, msg map[string]any)) {
	commandHandlers[name] = f
}

func (h *Hub) initMsg(v *View, popout string) map[string]any {
	m := map[string]any{
		"type": "init", "mode": v.mode, "windowRole": nil, "popoutConversationId": nilIfEmpty(popout),
		"app":            map[string]any{"version": Version, "build": Build, "release": Version + "-" + Build, "commitAt": nil},
		"edition":        "standalone",
		"settings":       h.settingsView(),
		"accounts":       h.accountViews(),
		"conversations":  h.conversationList(),
		"cli":            h.cliViews(),
		"providers":      h.providerViews(),
		"devicesCli":     nil,
		"appUpdate":      nil,
		"layout":         h.settings.Layout,
		"restoredDrafts": []string{},
		"movedIn":        []string{},
		"remote":         nil,
		"syncStarting":   false,
		"tokens":         nil,
		"health":         nil,
		"schedules":      nil,
	}
	for k, f := range initExtras {
		m[k] = f(h, v)
	}
	return m
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func toAnswers(v any) map[string][]string {
	out := map[string][]string{}
	m, _ := v.(map[string]any)
	for k, x := range m {
		arr, _ := x.([]any)
		for _, a := range arr {
			if s, ok := a.(string); ok {
				out[k] = append(out[k], s)
			}
		}
	}
	return out
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---- 對話 ----

func (h *Hub) pickAccount(id string) *Account {
	if a := h.account(id); a != nil {
		return a
	}
	if a := h.account(h.settings.LastAccount); a != nil {
		return a
	}
	for _, a := range h.accounts {
		if a.status == "ready" {
			return a
		}
	}
	if len(h.accounts) > 0 {
		return h.accounts[0]
	}
	return nil
}

func (h *Hub) newConversation(v *View, accountID string, paneID any) {
	a := h.pickAccount(accountID)
	if a == nil {
		h.toast(v, "還沒有可以用的帳戶")
		return
	}
	d := h.conversationDefault(a.Provider)
	model := "default"
	if d.Model != nil {
		model = *d.Model
	} else if m := h.settings.LastModel[a.ID]; m != "" {
		model = m
	}
	c := &Conv{Meta: ConvMeta{
		ID: newID("c"), AccountID: a.ID, Provider: a.Provider, Title: "新對話", Model: model, Effort: "default",
		Permission: d.Permission, CreatedAt: nowMs(), UpdatedAt: nowMs(), IsDraft: true,
	}}
	c.init()
	h.convs[c.Meta.ID] = c
	v.open[c.Meta.ID] = true
	h.sendTo(v, h.conversationMsg(c, paneID))
}

func (h *Hub) send(v *View, c *Conv, text string) {
	text = strings.TrimRight(text, " \t\r\n")
	if strings.TrimSpace(text) == "" {
		// 空送：佇列暫停時從第一則接著送
		if !c.running && len(c.queue) > 0 {
			next := c.queue[0]
			c.queue = c.queue[1:]
			c.dirtyComp = true
			h.startTurn(c, next.Text)
		}
		return
	}
	if strings.TrimSpace(text) == "/clear" {
		h.clearConversation(v, c)
		return
	}
	if c.Meta.IsDraft {
		c.Meta.IsDraft = false
		c.Meta.Title = titleFrom(text)
		c.Meta.CreatedAt = nowMs()
		h.settings.LastAccount = c.Meta.AccountID
		h.saveSettings()
	}
	if c.running {
		c.queue = append(c.queue, QueuedMessage{ID: newID("q"), Text: text})
		c.dirtyComp = true
		return
	}
	h.startTurn(c, text)
}

func titleFrom(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	if line == "" {
		return "新對話"
	}
	return truncateRunes(line, 40)
}

func (h *Hub) clearConversation(v *View, c *Conv) {
	if c.Meta.IsDraft {
		h.toast(v, "還沒有內容")
		return
	}
	if c.running {
		h.sendTo(v, map[string]any{"type": "sendRejected", "conversationId": c.Meta.ID, "reason": "running"})
		h.toast(v, "執行中不能清除，請先停止")
		return
	}
	h.restartSession(c)
	c.Meta.SessionID = ""
	c.Meta.SessionStarted = false
	c.Meta.LastTurn = ""
	c.notice(h, "clear", "已清除對話，接下來從頭開始")
	c.changed()
}

// restartSession 關掉目前的程序（下一則訊息以新的設定重開）
func (h *Hub) restartSession(c *Conv) {
	h.closeSession(c)
}

func (h *Hub) deleteConversation(c *Conv) {
	h.restartSession(c)
	delete(h.convs, c.Meta.ID)
	if !c.Meta.IsDraft {
		_ = os.RemoveAll(h.convDir(c.Meta.ID))
	}
	for v := range h.views {
		delete(v.open, c.Meta.ID)
		delete(v.visible, c.Meta.ID)
	}
	h.broadcast(map[string]any{"type": "conversationClosed", "conversationId": c.Meta.ID})
	h.pushConversations()
	h.pushAccounts()
}

func (h *Hub) setOption(v *View, c *Conv, key, value string) {
	if value == "" {
		value = "default"
	}
	switch key {
	case "model":
		c.Meta.Model = value
		h.settings.LastModel[c.Meta.AccountID] = value
		h.saveSettings()
		if p := h.provider(c); p != nil && p.Alive(c) && !p.SetOption(h, c, key, value) {
			h.closeSession(c)
		}
	case "effort":
		c.Meta.Effort = value
		if p := h.provider(c); p != nil && p.Alive(c) && !p.SetOption(h, c, key, value) {
			if c.running {
				h.toast(v, "新的推理強度從下一則訊息開始生效")
			} else {
				h.closeSession(c)
			}
		}
	case "permission":
		if value == "default" {
			value = h.conversationDefault(c.Meta.Provider).Permission
		}
		c.Meta.Permission = value
		if p := h.provider(c); p != nil && p.Alive(c) && !p.SetOption(h, c, key, value) {
			if c.running {
				h.toast(v, "新的權限模式從下一則訊息開始生效")
			} else {
				h.closeSession(c)
			}
		}
	default:
		return
	}
	c.changed()
}

func (h *Hub) setAccount(v *View, c *Conv, accountID string) {
	a := h.account(accountID)
	if a == nil || a.ID == c.Meta.AccountID {
		return
	}
	if !c.Meta.IsDraft {
		h.toast(v, "已經開始的對話還不能換帳戶（之後的版本補上）")
		return
	}
	c.Meta.AccountID, c.Meta.Provider = a.ID, a.Provider
	h.sendTo(v, h.conversationMsg(c, nil))
}

func (h *Hub) setFolder(v *View, c *Conv, path string) {
	if c.running {
		h.toast(v, "執行中不能換資料夾")
		return
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		h.toast(v, "找不到這個資料夾："+path)
		return
	}
	c.Meta.CustomFolder, c.Meta.WorkingDirectory = true, path
	h.rememberFolder(path)
	h.restartSession(c)
	c.changed()
}

func (h *Hub) rememberFolder(path string) {
	list := []string{path}
	for _, p := range h.settings.RecentFolders {
		if p != path && len(list) < 10 {
			list = append(list, p)
		}
	}
	h.settings.RecentFolders = list
	h.saveSettings()
}

// chooseFolder 開系統的選資料夾對話框（macOS：osascript；Linux：zenity）；在 goroutine 上跑，不持有鎖
func (h *Hub) chooseFolder(v *View, convID string) {
	var out []byte
	var err error
	switch runtime.GOOS {
	case "darwin":
		out, err = exec.Command("osascript", "-e", `POSIX path of (choose folder with prompt "選擇工作資料夾")`).Output()
	default:
		out, err = exec.Command("zenity", "--file-selection", "--directory").Output()
	}
	if err != nil {
		return // 取消
	}
	path := strings.TrimRight(strings.TrimSpace(string(out)), "/")
	if path == "" {
		path = "/"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.convs[convID]; c != nil {
		h.setFolder(v, c, path)
	}
	h.kick()
}

func openInFileManager(p string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", p).Start()
	} else {
		_ = exec.Command("xdg-open", p).Start()
	}
}

func revealInFileManager(p string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", "-R", p).Start()
	} else {
		_ = exec.Command("xdg-open", filepath.Dir(p)).Start()
	}
}

func (h *Hub) olderItems(c *Conv, beforeID string) map[string]any {
	end := len(c.Items)
	for i, it := range c.Items {
		if it.ID == beforeID {
			end = i
			break
		}
	}
	start := end - pageSize
	if start < 0 {
		start = 0
	}
	return map[string]any{"type": "olderItems", "conversationId": c.Meta.ID, "beforeId": nilIfEmpty(beforeID), "items": c.Items[start:end], "hasMore": start > 0}
}

func (h *Hub) applySetting(v *View, key string, value any) {
	kind, ok := settingKinds[key]
	if !ok {
		h.toast(v, "這個設定在這一版還不能改："+key)
		return
	}
	switch kind {
	case "bool":
		if _, ok := value.(bool); !ok {
			return
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return
		}
	case "theme":
		if s, _ := value.(string); s != "dark" && s != "light" && s != "system" {
			return
		}
	case "string":
		if value != nil {
			if _, ok := value.(string); !ok {
				return
			}
		}
	}
	h.settings.Values[key] = value
	h.saveSettings()
	h.broadcast(map[string]any{"type": "settings", "settings": h.settingsView()})
}

func (h *Hub) setConversationDefault(provider, key string, value any) {
	all, _ := h.settings.Values["conversationDefaults"].(map[string]any)
	if all == nil {
		all = map[string]any{}
	}
	pm, _ := all[provider].(map[string]any)
	if pm == nil {
		pm = map[string]any{}
	}
	pm[key] = value
	all[provider] = pm
	h.settings.Values["conversationDefaults"] = all
	h.saveSettings()
	h.broadcast(map[string]any{"type": "settings", "settings": h.settingsView()})
}

// serveMedia：對話裡的圖片與影片（/media?path=…）
func (h *Hub) serveMedia(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" || !filepath.IsAbs(p) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, p)
}
