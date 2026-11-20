package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/kevinlincg/myriagent/internal/claude"
	"github.com/kevinlincg/myriagent/internal/render"
)

// 這個檔把 Claude CLI 的串流（stream-json）轉成對話的項目：一輪的開始與結束、文字串流、工具卡、核准與問卷。
// 事件的意義照 C# 版（src/Myriagent/Agents/ClaudeAgentSession.cs、ClaudeStreamRules.cs）。

// ensureSession 確保這個對話有活著的 claude 程序（呼叫時持有 h.mu）
func (h *Hub) ensureSession(c *Conv) error {
	if c.sess != nil && c.sess.Alive() {
		return nil
	}
	a := h.account(c.Meta.AccountID)
	if a == nil {
		return fmt.Errorf("這個對話的帳戶已經移除，請先換一個帳戶")
	}
	cli := h.clis["Claude"]
	if cli == nil || cli.path == "" {
		return fmt.Errorf("這台電腦沒有安裝 Claude Code（claude）")
	}
	wd := c.workDir(h)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return err
	}
	if c.Meta.SessionID == "" {
		c.Meta.SessionID = newUUID()
	}
	model := c.Meta.Model
	if model == "default" {
		model = ""
	}
	effort := c.Meta.Effort
	if effort == "default" {
		effort = ""
	}
	opts := claude.Options{
		Executable: cli.path, WorkDir: wd, SessionID: c.Meta.SessionID, Resume: c.Meta.SessionStarted,
		Model: model, Effort: effort, PermissionMode: claudePermission(c.Meta.Permission), ConfigDir: a.ConfigDir,
		AppendSystem: h.instructions(c),
		Env:          h.agentEnv(c),
	}
	convID := c.Meta.ID
	var sess *claude.Session
	sess, err := claude.Start(opts,
		func(msg map[string]any) { h.onClaudeMessage(convID, sess, msg) },
		func(err error, stderr string) { h.onClaudeExit(convID, sess, err, stderr) })
	if err != nil {
		return err
	}
	c.sess = sess
	c.Meta.SessionStarted = true
	c.needSave = true
	// initialize：拿 / 指令、模型清單與帳戶資訊（不擋住送出：CLI 會依序處理）
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		resp, err := sess.Control(ctx, map[string]any{"subtype": "initialize", "hooks": nil})
		if err != nil {
			log.Printf("claude initialize：%v", err)
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if a := h.account(c.Meta.AccountID); a != nil {
			a.applyInitialize(resp)
			for _, other := range h.convs {
				if other.Meta.AccountID == a.ID {
					other.dirtyHeader = true
				}
			}
			h.pushAccounts()
			h.broadcast(map[string]any{"type": "settings", "settings": h.settingsView()})
		}
		h.kick()
	}()
	return nil
}

// instructions：附加給代理的說明（設定頁的 instructions；{cwd} 代換成工作資料夾）
func (h *Hub) instructions(c *Conv) string {
	s, _ := h.setting("instructions").(string)
	return strings.ReplaceAll(s, "{cwd}", c.workDir(h)) + h.extraInstructions(c)
}

// claudeProvider：Claude Code CLI（claude -p，stream-json）
type claudeProvider struct{}

func init() { registerProvider("Claude", claudeProvider{}) }

func (claudeProvider) Steer() bool { return true }

func (claudeProvider) Send(h *Hub, c *Conv, text string, images [][2]string) error {
	if err := h.ensureSession(c); err != nil {
		return err
	}
	return c.sess.SendUser(text, newUUID(), images)
}

func (claudeProvider) Stop(h *Hub, c *Conv) {
	if c.sess != nil {
		_ = c.sess.Interrupt()
	}
}

func (claudeProvider) Alive(c *Conv) bool { return c.sess != nil && c.sess.Alive() }

func (claudeProvider) Close(c *Conv) {
	if s := c.sess; s != nil {
		c.sess = nil
		go s.Close(2 * time.Second)
	}
}

func (claudeProvider) RefreshAccount(h *Hub, a *Account) { refreshClaudeAccount(h, a) }

// SetOption：模型與權限模式在執行中也能即時套用；推理強度只能在啟動時帶
func (claudeProvider) SetOption(h *Hub, c *Conv, key, value string) bool {
	switch key {
	case "model":
		m := any(value)
		if value == "default" {
			m = nil
		}
		_ = c.sess.ControlAsync(map[string]any{"subtype": "set_model", "model": m})
		return true
	case "permission":
		_ = c.sess.ControlAsync(map[string]any{"subtype": "set_permission_mode", "mode": claudePermission(value)})
		return true
	}
	return false
}

func (h *Hub) onClaudeExit(convID string, sess *claude.Session, err error, stderr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.convs[convID]
	if c == nil || c.sess != sess {
		return
	}
	c.sess = nil
	if c.running {
		c.running = false
		msg := "Claude 程序意外結束"
		if err != nil {
			msg += "（" + err.Error() + "）"
		}
		if t := strings.TrimSpace(lastLines(stderr, 8)); t != "" {
			msg += "：\n\n```\n" + t + "\n```"
		}
		c.errorItem(msg)
	}
	h.closeOpenCards(c, "CLI 已重新啟動，這張卡已失效；代理會視需要重新提出")
	for _, it := range c.Items {
		if it.Status == "Running" {
			it.Status = "Cancelled"
			c.touch(it)
		}
	}
	c.streamText = map[string][]*Item{}
	c.changed()
	h.kick()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (h *Hub) closeOpenCards(c *Conv, reason string) {
	for id := range c.pending {
		if it := c.item(id); it != nil && it.Status == "Waiting" {
			it.Status = "Cancelled"
			it.Output = reason
			c.touch(it)
		}
		delete(c.pending, id)
	}
}

func (h *Hub) onClaudeMessage(convID string, sess *claude.Session, msg map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.convs[convID]
	if c == nil || c.sess != sess {
		return
	}
	switch str(msg, "type") {
	case "system":
		h.onClaudeSystem(c, msg)
	case "stream_event":
		h.onClaudeStream(c, msg)
	case "assistant":
		h.onClaudeAssistant(c, msg)
	case "user":
		h.onClaudeUser(c, msg)
	case "result":
		h.onClaudeResult(c, msg)
	case "control_request":
		h.onClaudeControlRequest(c, msg)
	}
	h.kick()
}

func (h *Hub) onClaudeSystem(c *Conv, msg map[string]any) {
	switch str(msg, "subtype") {
	case "init":
		if mode := fromClaudePermission(str(msg, "permissionMode")); mode != "" && mode != c.Meta.Permission {
			c.Meta.Permission = mode
			c.changed()
		}
	case "status":
		if s := str(msg, "status"); s == "compacting" {
			c.status = "壓縮對話中…"
			c.dirtyHeader = true
		}
	case "compact_boundary":
		c.notice(h, "", "對話已壓縮")
	}
}

// 子代理的事件（parent_tool_use_id 有值）先不畫成主對話的項目
func isSubagent(msg map[string]any) bool {
	p, _ := msg["parent_tool_use_id"].(string)
	return p != ""
}

func (h *Hub) onClaudeStream(c *Conv, msg map[string]any) {
	if isSubagent(msg) {
		return
	}
	ev, _ := msg["event"].(map[string]any)
	switch str(ev, "type") {
	case "message_start":
		m, _ := ev["message"].(map[string]any)
		c.curMessage = str(m, "id")
	case "content_block_start":
		block, _ := ev["content_block"].(map[string]any)
		switch str(block, "type") {
		case "text":
			it := c.addItem("Assistant")
			it.Status = "Running"
			c.streamText[c.curMessage] = append(c.streamText[c.curMessage], it)
			c.curBlock = it
		case "thinking":
			it := c.addItem("Thinking")
			it.Status = "Running"
			c.streamText[c.curMessage] = append(c.streamText[c.curMessage], it)
			c.curBlock = it
			c.status = "思考中…"
			c.dirtyHeader = true
		case "tool_use":
			c.curBlock = nil
			id := str(block, "id")
			name := str(block, "name")
			if id != "" && c.toolItems[id] == nil && name != "AskUserQuestion" && name != "ExitPlanMode" {
				it := c.addItem("Tool")
				it.Status = "Running"
				it.ToolUseID = id
				d := describeTool(name, nil)
				it.ToolKind, it.ToolName = d.Kind, d.Name
				c.toolItems[id] = it
			}
		default:
			c.curBlock = nil
		}
	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		if c.curBlock == nil {
			return
		}
		switch str(delta, "type") {
		case "text_delta":
			c.curBlock.Text += str(delta, "text")
			c.touch(c.curBlock)
			if c.status != "回覆中…" {
				c.status = "回覆中…"
				c.dirtyHeader = true
			}
		case "thinking_delta":
			c.curBlock.Text += str(delta, "thinking")
			c.touch(c.curBlock)
		}
	case "content_block_stop":
		c.curBlock = nil
	}
}

// takeStream 取出這則訊息下一個還沒收尾的同種串流項目
func (c *Conv) takeStream(msgID, kind string) *Item {
	list := c.streamText[msgID]
	for i, it := range list {
		if it.Kind == kind {
			c.streamText[msgID] = append(list[:i:i], list[i+1:]...)
			return it
		}
	}
	return nil
}

func (h *Hub) onClaudeAssistant(c *Conv, msg map[string]any) {
	if isSubagent(msg) {
		return
	}
	m, _ := msg["message"].(map[string]any)
	msgID := str(m, "id")
	content, _ := m["content"].([]any)
	for _, b := range content {
		block, _ := b.(map[string]any)
		switch str(block, "type") {
		case "text":
			text := str(block, "text")
			it := c.takeStream(msgID, "Assistant")
			if it == nil {
				if strings.TrimSpace(text) == "" {
					continue
				}
				it = c.addItem("Assistant")
			}
			it.Text = text
			it.Status = "Done"
			it.HTML = render.Markdown(text)
			c.touch(it)
		case "thinking":
			text := str(block, "thinking")
			it := c.takeStream(msgID, "Thinking")
			if it == nil {
				if strings.TrimSpace(text) == "" {
					continue
				}
				it = c.addItem("Thinking")
			}
			it.Text = text
			it.Status = "Done"
			it.HTML = render.Markdown(text)
			c.touch(it)
		case "tool_use":
			h.onToolUse(c, block)
		}
	}
}

func (h *Hub) onToolUse(c *Conv, block map[string]any) {
	id := str(block, "id")
	name := str(block, "name")
	input, _ := block["input"].(map[string]any)
	if name == "AskUserQuestion" || name == "ExitPlanMode" {
		return // 卡片在 can_use_tool 時建立
	}
	if name == "TodoWrite" {
		it := c.addItem("Plan")
		it.Plan = parseTodos(input["todos"])
		c.toolItems[id] = it
		return
	}
	it := c.toolItems[id]
	if it == nil {
		it = c.addItem("Tool")
		it.Status = "Running"
		it.ToolUseID = id
		c.toolItems[id] = it
	}
	d := describeTool(name, input)
	it.ToolKind, it.ToolName, it.Title, it.Detail, it.FilePath, it.Diff, it.DiffStats = d.Kind, d.Name, d.Title, d.Detail, d.File, d.Diff, d.Stats
	c.touch(it)
	c.status = d.Name + "：" + truncate(d.Title, 80)
	c.dirtyHeader = true
	h.runToolStartHooks(c, it)
}

func (h *Hub) onClaudeUser(c *Conv, msg map[string]any) {
	if isSubagent(msg) {
		return
	}
	m, _ := msg["message"].(map[string]any)
	content, _ := m["content"].([]any)
	for _, b := range content {
		block, _ := b.(map[string]any)
		if str(block, "type") != "tool_result" {
			continue
		}
		it := c.toolItems[str(block, "tool_use_id")]
		if it == nil || it.Kind != "Tool" {
			continue
		}
		it.Output = truncateTail(toolResultText(block["content"]), 20000)
		if isErr, _ := block["is_error"].(bool); isErr {
			it.Status = "Failed"
		} else if it.Status != "Declined" {
			it.Status = "Done"
		}
		c.touch(it)
	}
}

func toolResultText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, p := range x {
			pm, _ := p.(map[string]any)
			if t := str(pm, "text"); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func truncateTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return fmt.Sprintf("⋯（前面省略 %d 字）\n", len(r)-n) + string(r[len(r)-n:])
}

func (h *Hub) onClaudeResult(c *Conv, msg map[string]any) {
	c.curBlock = nil
	for _, list := range c.streamText {
		for _, it := range list {
			it.Status = "Done"
			c.touch(it)
		}
	}
	c.streamText = map[string][]*Item{}
	for _, it := range c.toolItems {
		if it.Status == "Running" {
			it.Status = "Cancelled"
			c.touch(it)
		}
	}
	c.toolItems = map[string]*Item{}
	if usage, ok := msg["usage"].(map[string]any); ok {
		in := num(usage["input_tokens"]) + num(usage["cache_creation_input_tokens"]) + num(usage["cache_read_input_tokens"])
		out := num(usage["output_tokens"])
		c.Meta.LastTurn = fmt.Sprintf("輸入 %s・輸出 %s tokens", commas(in), commas(out))
	}
	if isErr, _ := msg["is_error"].(bool); isErr {
		text := str(msg, "result")
		if text == "" {
			text = "這一輪失敗了（" + str(msg, "subtype") + "）"
		}
		c.errorItem(text)
	}
	h.endTurn(c)
}

func num(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	}
	return 0
}

func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// viewing：有沒有 view 正在顯示這個對話
func (h *Hub) viewing(id string) bool {
	for v := range h.views {
		if v.visible[id] {
			return true
		}
	}
	return false
}

// ---- 核准、問卷、計畫確認（CLI 的 can_use_tool） ----

func (h *Hub) onClaudeControlRequest(c *Conv, msg map[string]any) {
	reqID := str(msg, "request_id")
	req, _ := msg["request"].(map[string]any)
	if str(req, "subtype") != "can_use_tool" {
		_ = c.sess.RespondError(reqID, "這個 app 還不支援 "+str(req, "subtype"))
		return
	}
	name := str(req, "tool_name")
	input, _ := req["input"].(map[string]any)
	suggestions, _ := req["permission_suggestions"].([]any)
	p := &pendingRequest{requestID: reqID, toolName: name, input: input, suggestions: suggestions}
	var it *Item
	switch name {
	case "AskUserQuestion":
		it = c.addItem("Question")
		it.Questions = parseQuestions(input)
		it.Answers = map[string][]string{}
	case "ExitPlanMode":
		it = c.addItem("PlanApproval")
		it.DetailHTML = render.Markdown(str(input, "plan"))
		it.ToolKind, it.ToolName, it.Title = "plan", "確認執行計畫", "代理已規劃好做法，請確認是否照計畫執行"
	default:
		it = c.addItem("Approval")
		d := describeTool(name, input)
		it.ToolKind, it.ToolName, it.Title, it.Detail, it.FilePath, it.Diff, it.DiffStats = d.Kind, d.Name, d.Title, d.Detail, d.File, d.Diff, d.Stats
		if len(suggestions) > 0 {
			it.CanAllowForSession = true
			it.AllowSessionLabel = "本對話都允許"
		}
	}
	it.Status = "Waiting"
	it.RequestID = reqID
	c.pending[it.ID] = p
	h.requestAttention(c, it, str(input, "plan"))
	c.status = "等你回應"
	if !h.viewing(c.Meta.ID) {
		c.unread = true
		switch it.Kind {
		case "Question":
			h.notify(c, "代理有問題要問你")
		case "PlanApproval":
			h.notify(c, "代理規劃好了，等你確認")
		default:
			h.notify(c, "要你核准："+it.ToolName+" "+truncate(it.Title, 60))
		}
	}
	c.changed()
}

func (claudeProvider) Approve(h *Hub, c *Conv, it *Item, p *pendingRequest, choice, message string) {
	if c.sess == nil {
		return
	}
	switch choice {
	case "allow", "allowSession":
		resp := map[string]any{"behavior": "allow", "updatedInput": orEmpty(p.input)}
		if choice == "allowSession" {
			var perms []any
			for _, s := range p.suggestions {
				sm, _ := s.(map[string]any)
				if sm == nil || str(sm, "type") == "setMode" {
					continue
				}
				cp := map[string]any{}
				for k, v := range sm {
					cp[k] = v
				}
				cp["destination"] = "session"
				perms = append(perms, cp)
			}
			if len(perms) > 0 {
				resp["updatedPermissions"] = perms
			}
		}
		_ = c.sess.Respond(p.requestID, resp)
		it.Status, it.Decision = "Done", choice
	default:
		text := "使用者拒絕了這個操作。不要改用其他方式繞過，請說明你原本要做什麼，並詢問使用者希望怎麼處理。"
		if strings.TrimSpace(message) != "" {
			text = "使用者拒絕了這個操作，並說明：「" + strings.TrimSpace(message) + "」。請依照使用者的說明調整做法。"
			it.Output = "你的說明：" + strings.TrimSpace(message)
		}
		_ = c.sess.Respond(p.requestID, map[string]any{"behavior": "deny", "message": text, "interrupt": false})
		it.Status, it.Decision = "Declined", "deny"
	}
	c.status = "思考中…"
	c.touch(it)
	c.changed()
}

func (claudeProvider) Answer(h *Hub, c *Conv, it *Item, p *pendingRequest, answers map[string][]string, skip bool) {
	if c.sess == nil {
		return
	}
	if skip {
		_ = c.sess.Respond(p.requestID, map[string]any{"behavior": "deny", "message": "使用者略過了這個問題，請依你的判斷繼續。", "interrupt": false})
		it.Status, it.Decision = "Done", "skip"
	} else {
		updated := map[string]any{}
		for k, v := range p.input {
			updated[k] = v
		}
		ans := map[string]any{}
		for q, vals := range answers {
			var clean []string
			for _, v := range vals {
				if s := strings.TrimSpace(v); s != "" {
					clean = append(clean, s)
				}
			}
			if len(clean) > 0 {
				ans[q] = strings.Join(clean, ", ")
			}
		}
		updated["answers"] = ans
		_ = c.sess.Respond(p.requestID, map[string]any{"behavior": "allow", "updatedInput": updated})
		it.Status, it.Decision, it.Answers = "Done", "answered", answers
	}
	c.status = "思考中…"
	c.touch(it)
	c.changed()
}

func (claudeProvider) ApprovePlan(h *Hub, c *Conv, it *Item, p *pendingRequest, choice, message string) {
	if c.sess == nil {
		return
	}
	switch choice {
	case "acceptEdits", "manual":
		_ = c.sess.Respond(p.requestID, map[string]any{"behavior": "allow", "updatedInput": orEmpty(p.input)})
		mode := "AcceptEdits"
		if choice == "manual" {
			mode = "Ask"
		}
		c.Meta.Permission = mode
		_ = c.sess.ControlAsync(map[string]any{"subtype": "set_permission_mode", "mode": claudePermission(mode)})
		it.Status, it.Decision = "Done", choice
	default:
		text := "使用者還不同意這個計畫，請繼續規劃，必要時用 AskUserQuestion 詢問使用者想調整的地方。"
		if strings.TrimSpace(message) != "" {
			text = "使用者還不同意這個計畫，並說明：「" + strings.TrimSpace(message) + "」。請依照說明修改計畫後再提出。"
			it.Output = "你的說明：" + strings.TrimSpace(message)
		}
		_ = c.sess.Respond(p.requestID, map[string]any{"behavior": "deny", "message": text, "interrupt": false})
		it.Status, it.Decision = "Done", "keepPlanning"
	}
	c.touch(it)
	c.changed()
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
