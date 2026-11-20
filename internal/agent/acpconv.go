package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kevinlincg/myriagent/internal/acp"
	"github.com/kevinlincg/myriagent/internal/render"
)

// 這個檔是 ACP 代理（Grok、Antigravity）共用的對話流程：一個對話一個常駐程序，
// 第一則訊息 session/new、程序重開後 session/resume（找不到就改開新對話並附上前文節錄）；
// 一輪＝session/prompt（要等這一輪結束才回應：stopReason＋用量），串流內容由 session/update 通知送來；
// 核准（session/request_permission）、提問、計畫確認畫成卡片；停止＝session/cancel（通知），等不到回應就結束程序樹。
// 模型、推理強度、權限模式在同一個程序裡即時切換（各家的方法不同，見 grokconv.go、agyconv.go）。
// 行為照 C# 版的 GrokAgentSession、GrokUpdateParser、AntigravityAcpRules。

// acpFlavor 是各家 ACP 代理的差異
type acpFlavor interface {
	key() string   // providers 的鍵（Grok、Antigravity）
	label() string // 給使用者看的名稱
	// launch：啟動參數與環境（隔離帳戶的設定資料夾在這裡準備）
	launch(h *Hub, c *Conv, a *Account, wd string) (args []string, env []string, err error)
	initializeParams() map[string]any
	// newSessionParams：preferredID 是 app 預先產生的 session id（presetSessionID 為 true 的才用）
	newSessionParams(wd, convDir, preferredID string, cfg acpConfig) map[string]any
	resumeParams(sid, wd, convDir string, cfg acpConfig) map[string]any
	presetSessionID() bool
	isSessionNotFound(e *acp.Error) bool
	isAuthRequired(e *acp.Error) bool
	notSignedIn() string
	// sessionOpened：session/new、session/resume 的結果（模型、方案；新對話建立時已套用的設定記進 applied）。持有 h.mu 與 p.cfgMu
	sessionOpened(h *Hub, c *Conv, p *acpProc, result map[string]any, created bool, cfg acpConfig)
	// applyConfig：模型、推理強度、權限模式和程序上已套用的不同就送（不持有 h.mu、持有 p.cfgMu）；回傳要顯示的提示
	applyConfig(ctx context.Context, p *acpProc, sid string, cfg acpConfig) []string
	// modeFromID：代理自己換了模式（current_mode_update）→ app 的權限模式；空＝不改
	modeFromID(modeID, current string) string
	// clientApprove：這個核准請求由 app 代為放行（持有 h.mu）
	clientApprove(h *Hub, c *Conv, p *acpProc, perm acpPermission) bool
	allowSessionLabel(kind string) string
	promptParams(sid, text string, images [][2]string) map[string]any
	// request／update：各家自己的請求與通知；處理了回 true（持有 h.mu）
	request(h *Hub, c *Conv, p *acpProc, m *acp.Message) bool
	update(h *Hub, c *Conv, p *acpProc, method string, u map[string]any) bool
	refreshAccount(h *Hub, a *Account)
}

// acpConfig 是這一輪要套用的設定（在 h.mu 底下從對話讀出）
type acpConfig struct {
	model        string // 空＝預設
	effort       string // 空＝不指定
	mode         string // app 的權限模式（Ask、AcceptEdits、Plan、Auto、FullAccess）
	defaultModel string // 代理的預設模型（選「預設」時要切回它）
	effortOK     bool   // 這個模型支援這個推理強度
}

// acpProc 是一個對話的 ACP 程序（c.proc）。欄位在 h.mu 底下改，applied 與 initialized 在 cfgMu 底下改
type acpProc struct {
	fl   acpFlavor
	conn *acp.Conn
	sig  string // 帳戶｜執行檔｜工作資料夾：變了就重開

	cfgMu       sync.Mutex
	initialized bool
	applied     map[string]string // 程序上已套用的設定（model、effort、mode、plan、level…）；沒有鍵＝不確定

	sessionID    string
	opened       bool
	promptImages bool
	models       []Option
	defaultModel string

	preparing  bool
	prepCancel context.CancelFunc
	promptID   int64
	compact    bool
	interrupt  bool
	cancelSent bool
	lastWD     string

	textItem    *Item
	thoughtItem *Item
	planItem    *Item
	tools       map[string]*acpTool
	reqs        map[string]*acpReq // 卡片項目 id → 等使用者回應的請求
	allowKinds  map[string]bool    // 「本對話都允許」之後由 app 代為核准的 kind（grok 的 edit、execute）
	denials     []string           // 拒絕時附的說明（這一輪結束後當下一則訊息送）
}

type acpTool struct {
	call     map[string]any
	item     *Item
	hidden   bool
	finished bool
	output   string
	hooked   bool // 已經呼叫過工具開始的掛鉤（指令內容出來的那一次）
}

// acpReq 是一張等使用者回應的卡片對應的請求
type acpReq struct {
	id        json.RawMessage
	kind      string // permission、question、plan、elicitation
	cancel    any    // 停止、程序要關閉時的回覆
	perm      acpPermission
	questions []Question
}

const (
	acpSessionTimeout = 90 * time.Second
	acpConfigTimeout  = 20 * time.Second
)

// acpCancelTimeout：送出取消後這麼久還沒回應就結束程序樹（測試可調短）
var acpCancelTimeout = 10 * time.Second

func (c *Conv) acp() *acpProc {
	p, _ := c.proc.(*acpProc)
	return p
}

// acpProvider 把 acpFlavor 接成 Provider
type acpProvider struct{ fl acpFlavor }

// Steer：ACP 的 session/prompt 在一輪進行中不能再送：執行中送出的一律排進佇列
func (acpProvider) Steer() bool { return false }

func (ap acpProvider) RefreshAccount(h *Hub, a *Account) { ap.fl.refreshAccount(h, a) }

func (acpProvider) Alive(c *Conv) bool { p := c.acp(); return p != nil && p.conn.Alive() }

func (acpProvider) Close(c *Conv) {
	p := c.acp()
	if p == nil {
		return
	}
	c.proc = nil
	if p.prepCancel != nil {
		p.prepCancel()
	}
	go p.conn.Close(2 * time.Second)
}

// Send：程序不在或設定（帳戶、執行檔、工作資料夾）變了就重開；準備（初始化、建立或續接對話、套用設定）在背景做，做完送 session/prompt
func (ap acpProvider) Send(h *Hub, c *Conv, text string, images [][2]string) error {
	fl := ap.fl
	a := h.account(c.Meta.AccountID)
	if a == nil {
		return fmt.Errorf("這個對話的帳戶已經移除，請先換一個帳戶")
	}
	cli := h.clis[fl.key()]
	if cli == nil || cli.path == "" {
		return fmt.Errorf("這台電腦沒有安裝 %s（%s）", providerNames[fl.key()], providerExecutables[fl.key()])
	}
	wd := c.workDir(h)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return err
	}
	sig := a.ID + "|" + cli.path + "|" + wd
	p := c.acp()
	if p != nil && (!p.conn.Alive() || p.sig != sig) {
		ap.Close(c)
		p = nil
	}
	if p == nil {
		args, env, err := fl.launch(h, c, a, wd)
		if err != nil {
			return err
		}
		p = &acpProc{fl: fl, sig: sig, applied: map[string]string{}, tools: map[string]*acpTool{}, reqs: map[string]*acpReq{}, allowKinds: map[string]bool{}}
		convID := c.Meta.ID
		conn, err := acp.Start(acp.Options{Executable: cli.path, Args: args, Dir: wd, Env: env},
			func(m *acp.Message) { h.onAcpMessage(convID, p, m) },
			func(err error, stderr string) { h.onAcpExit(convID, p, err, stderr) })
		if err != nil {
			return err
		}
		p.conn = conn
		c.proc = p
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.preparing, p.prepCancel = true, cancel
	p.interrupt, p.cancelSent, p.denials = false, false, nil
	go h.acpPrepare(c.Meta.ID, p, ctx, text, images)
	return nil
}

// acpConfigOf：這一輪要套用的設定（持有 h.mu）
func (h *Hub) acpConfigOf(c *Conv, p *acpProc) acpConfig {
	cfg := acpConfig{model: normalizeDefault(c.Meta.Model), effort: normalizeDefault(c.Meta.Effort), mode: orDefault(c.Meta.Permission, "Ask"), defaultModel: p.defaultModel}
	cfg.effortOK = cfg.effort != "" && supportsEffort(p.models, cfg.model, cfg.effort)
	return cfg
}

func normalizeDefault(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "default") {
		return ""
	}
	return strings.TrimSpace(s)
}

// supportsEffort：這個模型支援的推理強度（清單裡沒有這個模型或它沒列強度就照送）
func supportsEffort(models []Option, model, effort string) bool {
	id := model
	if id == "" {
		id = "default"
	}
	for _, m := range models {
		if m.Value == id {
			if len(m.Efforts) == 0 {
				return true
			}
			for _, e := range m.Efforts {
				if strings.EqualFold(e, effort) {
					return true
				}
			}
			return false
		}
	}
	return true
}

// acpPrepare：初始化、建立或續接對話、套用設定（不持有 h.mu），最後送出 session/prompt
func (h *Hub) acpPrepare(convID string, p *acpProc, ctx context.Context, text string, images [][2]string) {
	p.cfgMu.Lock()
	created, lost, notices, err := h.acpEnsureSession(convID, p, ctx)
	if err == nil {
		h.mu.Lock()
		c := h.convs[convID]
		var cfg acpConfig
		ok := c != nil && c.proc == p
		if ok {
			cfg = h.acpConfigOf(c, p)
		}
		sid := p.sessionID
		h.mu.Unlock()
		if ok {
			notices = append(notices, p.fl.applyConfig(ctx, p, sid, cfg)...)
		}
	}
	p.cfgMu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	c := h.convs[convID]
	if c == nil || c.proc != p {
		return
	}
	p.preparing, p.prepCancel = false, nil
	for _, n := range notices {
		c.notice(h, "", n)
	}
	if p.interrupt {
		// 準備期間按了停止：放棄這一輪
		p.interrupt = false
		h.endTurn(c)
		return
	}
	if err != nil {
		c.errorItem("沒辦法開始這一輪：" + err.Error())
		h.endTurn(c)
		return
	}
	p.beginTurn()
	method := "session/prompt"
	var params map[string]any
	if p.fl.key() == "Grok" && strings.TrimSpace(text) == "/compact" {
		// 手動壓縮：同一個收尾流程，改送 _x.ai/compact_conversation（GrokAcpRules.MethodCompact）
		method, params, p.compact = "_x.ai/compact_conversation", map[string]any{"sessionId": p.sessionID}, true
	} else {
		var parts []string
		if created {
			if ins := strings.TrimSpace(h.instructions(c)); ins != "" {
				parts = append(parts, "<myriagent_instructions>\n"+ins+"\n</myriagent_instructions>")
			}
		}
		if lost {
			if r := historyRecap(c); r != "" {
				parts = append(parts, r)
			}
		}
		wd := c.workDir(h)
		if !created && p.lastWD != "" && p.lastWD != wd {
			parts = append(parts, "（系統通知：工作資料夾已改為 "+wd+"，之後的檔案與指令請以這個資料夾為準）")
		}
		p.lastWD = wd
		parts = append(parts, text)
		if !p.promptImages {
			images = nil
		}
		params = p.fl.promptParams(p.sessionID, strings.Join(parts, "\n\n"), images)
	}
	id, werr := p.conn.Send(method, params, func(id int64, result any, e *acp.Error) { h.onAcpPromptDone(convID, p, id, result, e) })
	if werr != nil {
		// 寫入失敗：程序剛好結束，onAcpExit 會收尾
		return
	}
	p.promptID = id
}

// acpEnsureSession：初始化（第一次）、建立或續接對話。持有 p.cfgMu、不持有 h.mu
func (h *Hub) acpEnsureSession(convID string, p *acpProc, ctx context.Context) (created, lost bool, notices []string, err error) {
	fl := p.fl
	if !p.initialized {
		ictx, cancel := context.WithTimeout(ctx, acpSessionTimeout)
		res, e := p.conn.Request(ictx, "initialize", fl.initializeParams())
		cancel()
		if e != nil {
			if ctx.Err() != nil {
				// 初始化做到一半的程序不能留著重用
				p.conn.Kill()
			}
			return false, false, nil, fmt.Errorf("%s 初始化失敗：%s", fl.label(), e.Text)
		}
		p.initialized = true
		rm, _ := res.(map[string]any)
		caps, _ := rm["agentCapabilities"].(map[string]any)
		pc, _ := caps["promptCapabilities"].(map[string]any)
		img, _ := pc["image"].(bool)
		h.mu.Lock()
		p.promptImages = img
		h.mu.Unlock()
	}

	h.mu.Lock()
	c := h.convs[convID]
	if c == nil || c.proc != p {
		h.mu.Unlock()
		return false, false, nil, fmt.Errorf("對話已關閉")
	}
	if p.opened {
		h.mu.Unlock()
		return false, false, nil, nil
	}
	known := ""
	if c.Meta.SessionStarted {
		known = c.Meta.SessionID
	}
	preferred := c.Meta.SessionID
	started := c.Meta.SessionStarted
	cfg := h.acpConfigOf(c, p)
	wd, convDir := c.workDir(h), h.convDir(c.Meta.ID)
	h.mu.Unlock()

	var result map[string]any
	if known != "" {
		rctx, cancel := context.WithTimeout(ctx, acpSessionTimeout)
		res, e := p.conn.Request(rctx, "session/resume", fl.resumeParams(known, wd, convDir, cfg))
		cancel()
		switch {
		case e == nil:
			result, _ = res.(map[string]any)
			h.mu.Lock()
			if c.proc == p {
				p.sessionID, p.opened = known, true
				fl.sessionOpened(h, c, p, result, false, cfg)
			}
			h.mu.Unlock()
			return false, false, nil, nil
		case fl.isAuthRequired(e):
			return false, false, nil, fmt.Errorf("%s", fl.notSignedIn())
		case ctx.Err() != nil:
			return false, false, nil, fmt.Errorf("已停止")
		case !fl.isSessionNotFound(e):
			return false, false, nil, fmt.Errorf("無法續接 %s 對話：%s", fl.label(), e.Text)
		}
		lost = true
		notices = append(notices, providerExecutables[fl.key()]+" 找不到這個對話先前的紀錄，已改開新對話並附上前文節錄")
	}

	if lost || started || preferred == "" || !fl.presetSessionID() {
		preferred = newUUID()
	}
	nctx, cancel := context.WithTimeout(ctx, acpSessionTimeout)
	res, e := p.conn.Request(nctx, "session/new", fl.newSessionParams(wd, convDir, preferred, cfg))
	cancel()
	if e != nil && !fl.isAuthRequired(e) && fl.presetSessionID() && ctx.Err() == nil {
		// 預先產生的 id 可能已經被上一次沒做完的建立用掉了：換一個新的 id 再試一次
		log.Printf("%s 以預先產生的 id 建立對話失敗（%s），改用新的 id", fl.label(), e.Text)
		preferred = newUUID()
		nctx, cancel := context.WithTimeout(ctx, acpSessionTimeout)
		res, e = p.conn.Request(nctx, "session/new", fl.newSessionParams(wd, convDir, preferred, cfg))
		cancel()
	}
	if e != nil {
		if fl.isAuthRequired(e) {
			return false, lost, notices, fmt.Errorf("%s", fl.notSignedIn())
		}
		return false, lost, notices, fmt.Errorf("無法建立 %s 對話：%s", fl.label(), e.Text)
	}
	result, _ = res.(map[string]any)
	sid := firstNonBlank(str(result, "sessionId"), preferred)
	h.mu.Lock()
	if c.proc == p {
		p.sessionID, p.opened = sid, true
		c.Meta.SessionID, c.Meta.SessionStarted = sid, true
		c.needSave = true
		fl.sessionOpened(h, c, p, result, true, cfg)
	}
	h.mu.Unlock()
	return true, lost, notices, nil
}

// historyRecap：續接不到、改開新對話時附上的前文節錄（最近的幾則使用者與代理的訊息）
func historyRecap(c *Conv) string {
	var lines []string
	total := 0
	for i := len(c.Items) - 1; i >= 0 && total < 6000; i-- {
		it := c.Items[i]
		who := ""
		switch it.Kind {
		case "User":
			who = "使用者"
		case "Assistant":
			who = "你"
		default:
			continue
		}
		t := strings.TrimSpace(it.Text)
		if t == "" {
			continue
		}
		t = truncate(t, 800)
		total += len([]rune(t))
		lines = append([]string{who + "：" + t}, lines...)
	}
	// 最後一則是這一次要送的訊息（中樞已加進對話），不重複
	if len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "使用者：") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return "<previous_conversation>\n（先前的對話紀錄已遺失，以下是畫面上最近的內容節錄，請接著這段對話繼續）\n" + strings.Join(lines, "\n\n") + "\n</previous_conversation>"
}

// beginTurn：新的一輪開始，串流狀態重新起算
func (p *acpProc) beginTurn() {
	p.textItem, p.thoughtItem, p.planItem = nil, nil, nil
	p.tools = map[string]*acpTool{}
	p.compact = false
}

// ---- 收到的訊息 ----

func (h *Hub) onAcpMessage(convID string, p *acpProc, m *acp.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	c := h.convs[convID]
	if c == nil || c.proc != p {
		if m.IsRequest() {
			// 已換掉的程序：它的請求不顯示，直接以取消回覆
			_ = p.conn.Respond(m.ID, acpCancelFor(m.Method))
		}
		return
	}
	if m.IsRequest() {
		if m.Method == "session/request_permission" {
			h.onAcpPermission(c, p, m)
			return
		}
		if !p.fl.request(h, c, p, m) {
			_ = p.conn.RespondError(m.ID, -32601, "Myriagent 不支援 "+m.Method)
		}
		return
	}
	u, _ := m.Params["update"].(map[string]any)
	if m.Method == "session/update" && u != nil {
		h.onAcpUpdate(c, p, u)
		return
	}
	p.fl.update(h, c, p, m.Method, m.Params)
}

// streaming：這一輪還在進行（停止後晚到的內容不再畫）
func (p *acpProc) streaming(c *Conv) bool { return c.running && (p.promptID != 0 || p.preparing) }

func (h *Hub) onAcpUpdate(c *Conv, p *acpProc, u map[string]any) {
	switch kind := str(u, "sessionUpdate"); kind {
	case "agent_message_chunk":
		delta := acpChunkText(u)
		if delta == "" || !p.streaming(c) {
			return
		}
		p.closeThought(c)
		if p.textItem == nil {
			p.textItem = c.addItem("Assistant")
			p.textItem.Status = "Running"
		}
		p.textItem.Text += delta
		c.touch(p.textItem)
		if c.status != "回覆中…" {
			c.status = "回覆中…"
			c.dirtyHeader = true
		}
	case "agent_thought_chunk":
		delta := acpChunkText(u)
		if delta == "" || !p.streaming(c) {
			return
		}
		p.closeText(c)
		if p.thoughtItem == nil {
			p.thoughtItem = c.addItem("Thinking")
			p.thoughtItem.Status = "Running"
			c.status = "思考中…"
			c.dirtyHeader = true
		}
		p.thoughtItem.Text += delta
		c.touch(p.thoughtItem)
	case "tool_call", "tool_call_update":
		if !p.streaming(c) {
			return
		}
		h.onAcpTool(c, p, u)
	case "plan":
		if !p.streaming(c) {
			return
		}
		steps := acpPlanSteps(u["entries"])
		if len(steps) == 0 {
			return
		}
		if p.planItem == nil {
			p.planItem = c.addItem("Plan")
		}
		p.planItem.Plan = steps
		c.touch(p.planItem)
	case "available_commands_update":
		if a := h.account(c.Meta.AccountID); a != nil {
			a.commands = acpCommands(u["availableCommands"])
			h.acpAccountChanged(a)
		}
	case "current_mode_update":
		if mode := p.fl.modeFromID(str(u, "currentModeId"), c.Meta.Permission); mode != "" && mode != c.Meta.Permission {
			c.Meta.Permission = mode
			c.changed()
		}
	default:
		p.fl.update(h, c, p, "session/update", u)
	}
}

// acpCancelFor：不顯示的請求（已換掉的程序）以取消回覆，形狀照請求的種類
func acpCancelFor(method string) any {
	switch method {
	case "x.ai/ask_user_question", "x.ai/ask_user_questions":
		return map[string]any{"outcome": "cancelled"}
	case "x.ai/exit_plan_mode":
		return map[string]any{"outcome": "abandoned"}
	case "x.ai/folder_trust/request":
		return map[string]any{"outcome": "reject"}
	case "elicitation/create":
		return map[string]any{"action": "cancel"}
	}
	return acpCancelledPermission()
}

func acpChunkText(u map[string]any) string {
	content, _ := u["content"].(map[string]any)
	if str(content, "type") != "text" {
		return ""
	}
	return str(content, "text")
}

// acpCommands：available_commands_update → 「/」指令（always-approve 不列：權限模式由 app 的選單切換，兩邊會不一致）
func acpCommands(v any) []Command {
	arr, _ := v.([]any)
	out := []Command{}
	for _, x := range arr {
		m, _ := x.(map[string]any)
		name := strings.TrimPrefix(str(m, "name"), "/")
		if strings.TrimSpace(name) == "" || strings.EqualFold(name, "always-approve") {
			continue
		}
		in, _ := m["input"].(map[string]any)
		out = append(out, Command{Name: name, Description: str(m, "description"), ArgumentHint: str(in, "hint")})
	}
	return out
}

// acpAccountChanged：帳戶的模型、指令、方案變了：用這個帳戶的對話標頭重送、帳戶清單重送
func (h *Hub) acpAccountChanged(a *Account) {
	for _, other := range h.convs {
		if other.Meta.AccountID == a.ID {
			other.dirtyHeader = true
		}
	}
	h.pushAccounts()
}

func (p *acpProc) closeText(c *Conv) {
	if p.textItem != nil {
		p.textItem.Status = "Done"
		p.textItem.HTML = render.Markdown(p.textItem.Text)
		c.touch(p.textItem)
		p.textItem = nil
	}
}

func (p *acpProc) closeThought(c *Conv) {
	if p.thoughtItem != nil {
		p.thoughtItem.Status = "Done"
		c.touch(p.thoughtItem)
		p.thoughtItem = nil
	}
}

// acpToolState：這個工具呼叫的狀態（第一次看到就建立；提問與計畫確認的工具不畫成卡片）
func (p *acpProc) toolState(id string, first map[string]any) *acpTool {
	st := p.tools[id]
	if st == nil {
		st = &acpTool{call: map[string]any{}, hidden: acpHiddenTool(acpToolName(first))}
		p.tools[id] = st
	}
	return st
}

// announce：工具卡還沒畫就畫出來，並以合併到目前的內容更新它
func (p *acpProc) announce(c *Conv, id string, st *acpTool) {
	if st.hidden {
		return
	}
	if st.item == nil {
		st.item = c.addItem("Tool")
		st.item.Status = "Running"
		st.item.ToolUseID = id
	}
	d := describeAcpTool(st.call)
	it := st.item
	it.ToolKind, it.ToolName, it.Title, it.Detail, it.FilePath = d.Kind, d.Name, d.Title, d.Detail, d.File
	if d.Diff != nil {
		// 結束更新的 content 是輸出、不再帶差異：差異留著之前算好的
		it.Diff, it.DiffStats = d.Diff, d.Stats
	}
	c.touch(it)
	if !st.finished {
		c.status = d.Name + "：" + truncate(d.Title, 80)
		c.dirtyHeader = true
	}
}

func (h *Hub) onAcpTool(c *Conv, p *acpProc, u map[string]any) {
	id := str(u, "toolCallId")
	if id == "" {
		return
	}
	p.closeText(c)
	p.closeThought(c)
	st := p.toolState(id, u)
	acpMerge(st.call, u)
	if st.hidden || st.finished {
		return
	}
	p.announce(c, id, st)
	// 工具開始的掛鉤（turnhooks.go；排程的來源記錄等）：指令內容出來時呼叫一次
	if st.item != nil && !st.hooked && (st.item.Detail != "" || st.item.Title != "") {
		st.hooked = true
		h.runToolStartHooks(c, st.item)
	}
	status := str(u, "status")
	// 沒有 status 的更新（補上 kind、標題、差異）content 放的是工具的說明，不是輸出（grok 實測）
	if status == "" || (str(u, "sessionUpdate") == "tool_call" && status != "completed" && status != "failed") {
		return
	}
	output := acpToolOutput(u)
	it := st.item
	switch status {
	case "completed", "failed":
	default:
		// 執行中的更新帶的是到目前為止的完整輸出
		if output != "" && len(output) > len(st.output) && strings.HasPrefix(output, st.output) {
			st.output = output
			it.Output = truncateTail(output, 20000)
			c.touch(it)
		}
		return
	}
	st.finished = true
	if acpRejected(u) {
		// 使用者在核准卡拒絕：核准卡已經顯示決定，工具卡只標「已拒絕」
		it.Status = "Declined"
		c.touch(it)
		return
	}
	final := "Done"
	if status == "failed" {
		final = "Failed"
	} else if code, ok := acpExitCode(u); ok && code != 0 {
		final = "Failed"
	}
	kind := describeAcpTool(st.call).Kind
	if final == "Done" && (kind == "write" || kind == "edit") && acpHasDiff(st.call) {
		// 編輯類成功時有差異就夠了（輸出只是「已修改檔案」）
		output = ""
	} else if output == "" {
		output = st.output
	}
	it.Status = final
	it.Output = truncateTail(output, 20000)
	c.touch(it)
}

// finishStream：這一輪結束：文字收尾；還在執行的工具結束成 Cancelled（停止＝「已停止」；程序結束＝被中斷）
func (p *acpProc) finishStream(c *Conv, unfinished string) {
	p.closeText(c)
	p.closeThought(c)
	for _, st := range p.tools {
		if st.finished || st.hidden || st.item == nil {
			continue
		}
		st.finished = true
		st.item.Status = "Cancelled"
		if st.output != "" {
			st.item.Output = truncateTail(strings.TrimRight(st.output, "\n")+"\n\n"+unfinished, 20000)
		} else {
			st.item.Output = unfinished
		}
		c.touch(st.item)
	}
}

// cancelRequests：還沒回應的卡片一律以取消回覆、卡片標成已取消
func (p *acpProc) cancelRequests(c *Conv, reason string) {
	for itemID, r := range p.reqs {
		_ = p.conn.Respond(r.id, r.cancel)
		if it := c.item(itemID); it != nil && it.Status == "Waiting" {
			it.Status = "Cancelled"
			if reason != "" {
				it.Output = reason
			}
			c.touch(it)
		}
		delete(c.pending, itemID)
		delete(p.reqs, itemID)
	}
}

// onAcpPromptDone：session/prompt 回應了：這一輪結束（stopReason＋用量）
func (h *Hub) onAcpPromptDone(convID string, p *acpProc, id int64, result any, e *acp.Error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	c := h.convs[convID]
	if c == nil || c.proc != p || p.promptID != id {
		return
	}
	interrupted := p.interrupt
	compacting := p.compact
	p.promptID, p.interrupt, p.cancelSent, p.compact = 0, false, false, false
	p.finishStream(c, "已停止")
	p.cancelRequests(c, "")
	rm, _ := result.(map[string]any)
	stop := str(rm, "stopReason")
	if e != nil {
		if !interrupted {
			if p.fl.isAuthRequired(e) {
				c.errorItem(p.fl.notSignedIn())
			} else {
				c.errorItem(p.fl.label() + " 執行失敗：" + e.Text)
			}
		}
	} else {
		switch stop {
		case "refusal":
			c.errorItem(p.fl.label() + " 拒絕回應這個要求")
		case "max_tokens":
			c.notice(h, "", p.fl.label()+" 的回覆達到輸出長度上限而中斷")
		case "max_turn_requests":
			c.notice(h, "", p.fl.label()+" 這一輪達到工具呼叫次數上限而停下")
		}
		if in, out, ok := acpUsage(rm); ok {
			c.Meta.LastTurn = fmt.Sprintf("輸入 %s・輸出 %s tokens", commas(in), commas(out))
		}
		if compacting && stop != "cancelled" {
			c.notice(h, "", "已壓縮較早的對話內容")
		}
	}
	// 拒絕時附的說明：正常結束就當下一則訊息接著送；按停止就不送
	if !interrupted && stop != "cancelled" && len(p.denials) > 0 {
		c.queue = append([]QueuedMessage{{ID: newID("q"), Text: buildDenyFeedback(p.denials)}}, c.queue...)
		c.dirtyComp = true
	}
	p.denials = nil
	h.endTurn(c)
}

// buildDenyFeedback：拒絕核准時附的說明（ACP 的拒絕沒有欄位能帶說明，這一輪結束後當下一則訊息送出；同 C# 的 AgentHubRules.BuildDenyFeedbackMessage）
func buildDenyFeedback(denials []string) string {
	if len(denials) == 1 {
		return "（我剛才拒絕了那個操作）" + denials[0]
	}
	var lines []string
	for i, d := range denials {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, d))
	}
	return "（我剛才拒絕了這幾個操作）\n" + strings.Join(lines, "\n")
}

// acpUsage：這一輪的用量：result.usage 或 result._meta.usage（inputTokens、outputTokens）
func acpUsage(rm map[string]any) (int64, int64, bool) {
	u, ok := rm["usage"].(map[string]any)
	if !ok {
		meta, _ := rm["_meta"].(map[string]any)
		u, ok = meta["usage"].(map[string]any)
	}
	if !ok {
		return 0, 0, false
	}
	pick := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := u[k].(float64); ok {
				return int64(v)
			}
		}
		return 0
	}
	in := pick("inputTokens", "input_tokens")
	out := pick("outputTokens", "output_tokens")
	if in == 0 && out == 0 {
		return 0, 0, false
	}
	return in, out, true
}

func (h *Hub) onAcpExit(convID string, p *acpProc, err error, stderr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	c := h.convs[convID]
	if c == nil || c.proc != p {
		return
	}
	c.proc = nil
	if p.prepCancel != nil {
		p.prepCancel()
	}
	if c.running && (p.promptID != 0 || p.preparing) {
		interrupted := p.interrupt
		if interrupted {
			p.finishStream(c, "已停止")
		} else {
			p.finishStream(c, p.fl.label()+" 程序已結束，這個工具被中斷，結果未知")
		}
		p.promptID, p.preparing = 0, false
		h.closeOpenCards(c, "CLI 已重新啟動，這張卡已失效；代理會視需要重新提出")
		if interrupted {
			h.endTurn(c)
			return
		}
		msg := p.fl.label() + " 程序意外結束"
		if err != nil {
			msg += "（" + err.Error() + "）"
		}
		if t := strings.TrimSpace(lastLines(stderr, 8)); t != "" {
			msg += "：\n\n```\n" + t + "\n```"
		}
		c.errorItem(msg)
		c.running, c.stopping = false, false
		c.status = ""
		c.changed()
		return
	}
	h.closeOpenCards(c, "CLI 已重新啟動，這張卡已失效；代理會視需要重新提出")
	c.changed()
}

// Stop：還沒送出 prompt（準備中）就放棄準備；已送出就 session/cancel，等不到回應就結束程序樹
func (ap acpProvider) Stop(h *Hub, c *Conv) {
	p := c.acp()
	if p == nil {
		return
	}
	p.interrupt = true
	p.cancelRequests(c, "")
	if p.promptID == 0 {
		if p.prepCancel != nil {
			p.prepCancel()
		}
		return
	}
	if p.cancelSent {
		return
	}
	p.cancelSent = true
	_ = p.conn.Notify("session/cancel", map[string]any{"sessionId": p.sessionID})
	prompt, convID := p.promptID, c.Meta.ID
	go func() {
		select {
		case <-p.conn.Done():
			return
		case <-time.After(acpCancelTimeout):
		}
		h.mu.Lock()
		cc := h.convs[convID]
		stuck := cc != nil && cc.proc == p && p.promptID == prompt
		h.mu.Unlock()
		if stuck {
			log.Printf("%s 在 %v 內沒有回應停止，結束程序樹", p.fl.label(), acpCancelTimeout)
			p.conn.Kill()
		}
	}()
}

// SetOption：模型、推理強度、權限模式都能在程序裡即時切換：對話已建立就在背景套用，還沒建立的下一輪準備時套用
func (acpProvider) SetOption(h *Hub, c *Conv, key, value string) bool {
	p := c.acp()
	if p == nil || !p.opened || p.preparing {
		return true
	}
	convID := c.Meta.ID
	go func() {
		p.cfgMu.Lock()
		h.mu.Lock()
		cc := h.convs[convID]
		if cc == nil || cc.proc != p {
			h.mu.Unlock()
			p.cfgMu.Unlock()
			return
		}
		cfg, sid := h.acpConfigOf(cc, p), p.sessionID
		h.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), acpConfigTimeout*2)
		notices := p.fl.applyConfig(ctx, p, sid, cfg)
		cancel()
		p.cfgMu.Unlock()
		if len(notices) == 0 {
			return
		}
		h.mu.Lock()
		if cc := h.convs[convID]; cc != nil {
			for _, n := range notices {
				cc.notice(h, "", n)
			}
			cc.changed()
		}
		h.mu.Unlock()
		h.kick()
	}()
	return true
}

// ---- 核准、提問、計畫確認 ----

// addCard：建立等使用者回應的卡片（持有 h.mu）
func (h *Hub) acpAddCard(c *Conv, p *acpProc, kind string, r *acpReq) *Item {
	it := c.addItem(kind)
	it.Status = "Waiting"
	it.RequestID = string(r.id)
	p.reqs[it.ID] = r
	c.pending[it.ID] = &pendingRequest{requestID: string(r.id)}
	c.status = "等你回應"
	if !h.viewing(c.Meta.ID) {
		c.unread = true
	}
	c.changed()
	return it
}

func (h *Hub) onAcpPermission(c *Conv, p *acpProc, m *acp.Message) {
	perm := parseAcpPermission(m.Params)
	call, _ := m.Params["toolCall"].(map[string]any)
	merged := call
	if perm.toolCallID != "" && call != nil {
		// 工具卡還沒畫就先畫（卡片要在核准卡之前），並合併請求裡的工具內容
		p.closeText(c)
		p.closeThought(c)
		st := p.toolState(perm.toolCallID, call)
		acpMerge(st.call, call)
		if !st.finished {
			p.announce(c, perm.toolCallID, st)
		}
		merged = st.call
	}
	if p.fl.clientApprove(h, c, p, perm) && perm.allowOnce != "" {
		_ = p.conn.Respond(m.ID, acpSelected(perm.allowOnce))
		return
	}
	r := &acpReq{id: m.ID, kind: "permission", cancel: acpCancelledPermission(), perm: perm}
	it := h.acpAddCard(c, p, "Approval", r)
	d := describeAcpTool(orEmpty(merged))
	it.ToolKind, it.ToolName, it.Title, it.Detail, it.FilePath, it.Diff, it.DiffStats = d.Kind, d.Name, d.Title, d.Detail, d.File, d.Diff, d.Stats
	it.ToolUseID = perm.toolCallID
	if perm.allowAlways != "" {
		it.CanAllowForSession = true
		it.AllowSessionLabel = firstNonBlank(p.fl.allowSessionLabel(perm.kind), "本對話都允許")
	}
	h.requestAttention(c, it, "") // 閃對話、語音通知（speech.go）
}

func (ap acpProvider) Approve(h *Hub, c *Conv, it *Item, pr *pendingRequest, choice, message string) {
	p := c.acp()
	if p == nil {
		return
	}
	r := p.reqs[it.ID]
	delete(p.reqs, it.ID)
	if r == nil || r.kind != "permission" {
		return
	}
	switch choice {
	case "allow", "allowSession":
		if choice == "allowSession" && r.perm.allowAlways != "" && (r.perm.kind == "edit" || r.perm.kind == "execute") {
			// 只有說明過效力範圍的 edit（之後的編輯）與 execute（之後的指令）由 app 記著代為核准
			p.allowKinds[r.perm.kind] = true
		}
		_ = p.conn.Respond(r.id, r.perm.outcome(choice))
		it.Status, it.Decision = "Done", choice
	default:
		_ = p.conn.Respond(r.id, r.perm.outcome("deny"))
		it.Status, it.Decision = "Declined", "deny"
		if msg := strings.TrimSpace(message); msg != "" {
			it.Output = "你的說明：" + msg
			op := joinNonEmpty(it.ToolName, firstLine(it.Title))
			if op != "" && len(p.denials) > 0 {
				msg = "「" + op + "」：" + msg
			}
			p.denials = append(p.denials, msg)
		}
	}
	c.touch(it)
	c.changed()
}

func (ap acpProvider) Answer(h *Hub, c *Conv, it *Item, pr *pendingRequest, answers map[string][]string, skip bool) {
	p := c.acp()
	if p == nil {
		return
	}
	r := p.reqs[it.ID]
	delete(p.reqs, it.ID)
	if r == nil {
		return
	}
	var result any
	switch {
	case skip:
		result = r.cancel
		it.Status, it.Decision = "Done", "skip"
	case r.kind == "elicitation":
		result = elicitationAccept(r.questions, answers)
		it.Status, it.Decision, it.Answers = "Done", "answered", answers
	case r.kind == "question":
		result = grokQuestionAnswer(r.questions, answers)
		it.Status, it.Decision, it.Answers = "Done", "answered", answers
	default:
		return
	}
	_ = p.conn.Respond(r.id, result)
	c.touch(it)
	c.changed()
}

// ApprovePlan：同意＝approved，再把權限模式切出規劃模式（照計畫執行、之後改檔案不再詢問＝自動接受編輯；仍要詢問＝每步詢問）；繼續規劃＝cancelled（附說明）
func (ap acpProvider) ApprovePlan(h *Hub, c *Conv, it *Item, pr *pendingRequest, choice, message string) {
	p := c.acp()
	if p == nil {
		return
	}
	r := p.reqs[it.ID]
	delete(p.reqs, it.ID)
	if r == nil || r.kind != "plan" {
		return
	}
	if choice != "acceptEdits" && choice != "manual" {
		res := map[string]any{"outcome": "cancelled"}
		if msg := strings.TrimSpace(message); msg != "" {
			res["feedback"] = msg
			it.Output = "你的說明：" + msg
		}
		_ = p.conn.Respond(r.id, res)
		it.Status, it.Decision = "Done", "keepPlanning"
		c.touch(it)
		return
	}
	// 同意後 grok 立刻回到 default 模式並在同一輪接著執行（實測）：先改好 app 的模式再回覆，接下來的核准請求才照新模式判斷
	c.Meta.Permission = "Ask"
	if choice == "acceptEdits" {
		c.Meta.Permission = "AcceptEdits"
	}
	_ = p.conn.Respond(r.id, map[string]any{"outcome": "approved"})
	it.Status, it.Decision = "Done", choice
	c.touch(it)
	c.changed()
	ap.SetOption(h, c, "permission", c.Meta.Permission)
}

// ---- 提問的答案 ----

// grokQuestionAnswer：問卷的答案 → {outcome:"accepted", answers:{題目:[選項…]}, annotations:{題目:{notes}}}；
// 不是選項的文字（使用者自填）照 grok 的格式寫成 "Other"，文字放進 annotations 的 notes
func grokQuestionAnswer(questions []Question, answers map[string][]string) map[string]any {
	ans := map[string]any{}
	notes := map[string]any{}
	for _, q := range questions {
		var labels []any
		var extra []string
		hasOther := false
		for _, v := range answers[q.ID] {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			isOption := false
			for _, o := range q.Options {
				if o.Label == v {
					isOption = true
				}
			}
			if isOption {
				labels = append(labels, v)
			} else {
				extra = append(extra, v)
				if !hasOther {
					labels = append(labels, "Other")
					hasOther = true
				}
			}
		}
		if len(labels) == 0 {
			continue
		}
		ans[q.ID] = labels
		if len(extra) > 0 {
			notes[q.ID] = map[string]any{"notes": strings.Join(extra, "\n")}
		}
	}
	out := map[string]any{"outcome": "accepted", "answers": ans}
	if len(notes) > 0 {
		out["annotations"] = notes
	}
	return out
}

// elicitationAccept：問卷的答案 → elicitation/create 的 {action:"accept", content:{屬性: 值}}（複選是陣列、單選是字串）
func elicitationAccept(questions []Question, answers map[string][]string) map[string]any {
	content := map[string]any{}
	for _, q := range questions {
		var list []string
		for _, v := range answers[q.ID] {
			if s := strings.TrimSpace(v); s != "" {
				list = append(list, s)
			}
		}
		if q.Multi {
			arr := []any{}
			for _, s := range list {
				arr = append(arr, s)
			}
			content[q.ID] = arr
		} else if len(list) > 0 {
			content[q.ID] = list[0]
		} else {
			content[q.ID] = ""
		}
	}
	return map[string]any{"action": "accept", "content": content}
}
