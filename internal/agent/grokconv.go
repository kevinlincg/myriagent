package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kevinlincg/myriagent/internal/acp"
	"github.com/kevinlincg/myriagent/internal/render"
)

// Grok Build（grok agent --no-leader stdio，ACP）。共用流程在 acpconv.go；這裡是 grok 自己的參數、方法與帳戶查詢。
// 照 C# 版的 GrokAcpRules、GrokAgentSession、AccountManager.RefreshGrokAsync。

type grokFlavor struct{}

func init() {
	registerProvider("Grok", acpProvider{grokFlavor{}})
	// grok 沒有「自動接受編輯」模式，由 app 代為核准工作資料夾內的編輯；「自動」要帳戶有開放（遠端設定），沒開放時 grok 會留在每步詢問
	registerPermissionModes("Grok", []Option{
		{Value: "Ask", Label: "每步詢問", Description: "執行指令、修改檔案前先問你"},
		{Value: "AcceptEdits", Label: "自動接受編輯", Description: "工作資料夾內修改檔案不再詢問，執行指令仍會問"},
		{Value: "Plan", Label: "規劃模式", Description: "只讀取與分析，先提出計畫給你確認"},
		{Value: "Auto", Label: "自動", Description: "由 Grok 判斷，高風險操作才問（帳戶沒開放時照每步詢問）"},
		{Value: "FullAccess", Label: "完全自動", Description: "一律放行，不再詢問"},
	})
}

func (grokFlavor) key() string   { return "Grok" }
func (grokFlavor) label() string { return "Grok" }

// grokIsolated：隔離帳戶（自己的 GROK_HOME）
func grokIsolated(a *Account) bool { return !a.IsSystemDefault && a.ConfigDir != "" }

// grokEnv：app 啟動的 grok 一律不在背景自動更新；隔離帳戶帶 GROK_HOME（資料夾先放好 config.toml）
func grokEnv(a *Account, extra map[string]string) []string {
	set := map[string]string{"GROK_DISABLE_AUTOUPDATER": "1"}
	for k, v := range extra {
		set[k] = v
	}
	if grokIsolated(a) {
		ensureGrokIsolatedHome(a.ConfigDir)
		set["GROK_HOME"] = a.ConfigDir
	}
	return childEnv(set)
}

// grokIsolatedConfig：隔離帳戶預先寫好的 config.toml：標記「Claude 設定已匯入過」，grok 才不會讀使用者的 ~/.claude
const grokIsolatedConfig = "[claude_compat]\nimported = true\n"

// ensureGrokIsolatedHome：建立資料夾，config.toml 不存在就寫入；已存在但沒有 [claude_compat] 段就補上（失敗只是會匯入使用者的 Claude 設定）
func ensureGrokIsolatedHome(home string) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return
	}
	cfg := filepath.Join(home, "config.toml")
	b, err := os.ReadFile(cfg)
	if err != nil {
		_ = os.WriteFile(cfg, []byte(grokIsolatedConfig), 0o600)
		return
	}
	if hasClaudeCompatSection(string(b)) {
		return
	}
	sep := "\n\n"
	if len(b) == 0 || strings.HasSuffix(string(b), "\n") {
		sep = "\n"
	}
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.WriteString(sep + grokIsolatedConfig)
		_ = f.Close()
	}
}

func hasClaudeCompatSection(toml string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(toml, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "[claude_compat]" {
			return true
		}
	}
	return false
}

var grokArgs = []string{"agent", "--no-leader", "stdio"}

func (grokFlavor) launch(h *Hub, c *Conv, a *Account, wd string) ([]string, []string, error) {
	return grokArgs, grokEnv(a, map[string]string{"MYRIAGENT_AGENT_CONVERSATION": c.Meta.ID, "PWD": wd}), nil
}

// initializeParams：不提供檔案與終端（grok 用自己的工具），也不宣告互動式資料夾信任
func (grokFlavor) initializeParams() map[string]any {
	return map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]any{"name": "myriagent", "title": "Myriagent", "version": Version},
	}
}

// grokSessionMeta：askUserQuestion＝開提問工具（由 app 畫問題卡）；權限模式（yoloMode／autoMode）在建立時就帶上
func grokSessionMeta(mode string) map[string]any {
	return map[string]any{"askUserQuestion": true, "yoloMode": mode == "FullAccess", "autoMode": mode == "Auto"}
}

// newSessionParams：sessionId 由 app 預先產生（續接鍵不必等回應）
func (grokFlavor) newSessionParams(wd, convDir, preferredID string, cfg acpConfig) map[string]any {
	meta := grokSessionMeta(cfg.mode)
	meta["sessionId"] = preferredID
	if cfg.model != "" {
		meta["modelId"] = cfg.model
	}
	return map[string]any{"cwd": wd, "mcpServers": []any{}, "_meta": meta}
}

// resumeParams：session/resume 不重播歷史（畫面上已有內容）
func (grokFlavor) resumeParams(sid, wd, convDir string, cfg acpConfig) map[string]any {
	return map[string]any{"sessionId": sid, "cwd": wd, "mcpServers": []any{}, "_meta": grokSessionMeta(cfg.mode)}
}

func (grokFlavor) presetSessionID() bool { return true }

// isSessionNotFound：grok 1.0.52：-32603 Path not found、data.code FS_NOT_FOUND；prompt 帶未知 id 是 unknown session id
func (grokFlavor) isSessionNotFound(e *acp.Error) bool {
	t := strings.ToLower(e.Text)
	return strings.EqualFold(e.DataCode, "FS_NOT_FOUND") || strings.Contains(t, "unknown session") || strings.Contains(t, "session not found")
}

// isAuthRequired：沒有登入（session/new、billing 回 -32000 Authentication required）
func (grokFlavor) isAuthRequired(e *acp.Error) bool {
	return e != nil && strings.Contains(strings.ToLower(e.Text), "authentication required")
}

func (grokFlavor) notSignedIn() string {
	return "Grok 尚未登入，請到「帳戶」頁按登入，完成登入後再送出"
}

// grokLevel：grok 層級的權限：完全自動＝always-approve、自動＝auto、其他都是 ask（自動接受編輯由 app 代為核准、規劃模式另外切）
func grokLevel(mode string) string {
	switch mode {
	case "FullAccess":
		return "always-approve"
	case "Auto":
		return "auto"
	}
	return "ask"
}

func (grokFlavor) sessionOpened(h *Hub, c *Conv, p *acpProc, result map[string]any, created bool, cfg acpConfig) {
	models, current := parseGrokModels(result["models"])
	if len(models) > 0 {
		p.models = models
		if a := h.account(c.Meta.AccountID); a != nil {
			a.models = models
			h.acpAccountChanged(a)
		}
	}
	if current != "" && cfg.model == "" {
		p.defaultModel = current
	}
	if created {
		// 建立時的推理強度就是預設（使用者指定的強度在建立之後才送）
		if d := acpConfigCurrent(result, "reasoning_effort", "thought_level"); d != "" {
			p.defaultEffort = d
		}
		// 建立時已帶上模型（_meta.modelId）與權限（yoloMode／autoMode），規劃模式要另外切
		p.applied.set("model", cfg.model)
		p.applied.set("level", grokLevel(cfg.mode))
		p.applied.set("plan", "default")
		p.applied.del("effort")
	} else {
		// 續接後權限模式與規劃模式不確定：下次一律送
		p.applied.del("level")
		p.applied.del("plan")
	}
}

// applyConfig：模型、思考強度（session/set_config_option，值直接是字串）與權限模式（session/set_mode、_x.ai/yolo_mode_changed）
func (grokFlavor) applyConfig(ctx context.Context, p *acpProc, sid string, cfg acpConfig) []string {
	var notices []string
	request := func(method string, params map[string]any) *acp.Error {
		rctx, cancel := context.WithTimeout(ctx, acpConfigTimeout)
		defer cancel()
		_, e := p.conn.Request(rctx, method, params)
		return e
	}
	sentModel := false
	if m, ok := p.applied.get("model"); !ok || m != cfg.model {
		// 換回「預設」：送 grok 的預設模型（不知道就不送）
		send := firstNonBlank(cfg.model, cfg.defaultModel)
		if send != "" {
			if e := request("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "value": send}); e != nil {
				notices = append(notices, "Grok 無法切換到模型 "+send+"："+e.Text)
			} else {
				sentModel = true
			}
		}
		p.applied.set("model", cfg.model)
	}
	// 換模型時一併重送思考強度：不確定 grok 換模型會不會把強度重設成新模型的預設
	if send, notice := cfg.effortToSend(p.applied, sentModel, "Grok"); send != "" {
		if e := request("session/set_config_option", map[string]any{"sessionId": sid, "configId": "reasoning_effort", "value": send}); e != nil {
			notices = append(notices, "Grok 無法切換思考強度（"+send+"）："+e.Text)
		}
		p.applied.set("effort", cfg.effort)
	} else if notice != "" {
		notices = append(notices, notice)
	}
	plan := "default"
	if cfg.mode == "Plan" {
		plan = "plan"
	}
	if p.applied.val("plan") != plan {
		if e := request("session/set_mode", map[string]any{"sessionId": sid, "modeId": plan}); e != nil {
			verb := "離開"
			if plan == "plan" {
				verb = "進入"
			}
			notices = append(notices, "Grok 無法"+verb+"規劃模式："+e.Text)
		} else {
			p.applied.set("plan", plan)
		}
	}
	if level := grokLevel(cfg.mode); p.applied.val("level") != level {
		params := map[string]any{"yolo_mode": level == "always-approve", "permission_mode": level}
		_ = p.conn.Notify("_x.ai/yolo_mode_changed", params)
		p.applied.set("level", level)
	}
	return notices
}

// noteMode：grok 自己進出規劃模式（enter_plan_mode、計畫確認同意後回到 default）
func (grokFlavor) noteMode(p *acpProc, id string) {
	if id == "plan" || id == "default" {
		p.applied.set("plan", id)
	}
}

// modeFromID：grok 自己換了模式：計畫確認同意後回到 default、代理用 enter_plan_mode 進入 plan
func (grokFlavor) modeFromID(id, current string) string {
	switch id {
	case "plan":
		return "Plan"
	case "default":
		if current == "Plan" {
			return "Ask"
		}
	}
	return ""
}

// clientApprove：「本對話都允許」過的 kind、完全自動、自動接受編輯（工作資料夾內的編輯）由 app 代為核准
func (grokFlavor) clientApprove(h *Hub, c *Conv, p *acpProc, perm acpPermission) bool {
	if perm.kind != "" && p.allowKinds[perm.kind] {
		return true
	}
	switch c.Meta.Permission {
	case "FullAccess":
		return true
	case "AcceptEdits":
		return acpCanAutoApproveEdit(perm, c.workDir(h), h.convDir(c.Meta.ID))
	}
	return false
}

// allowSessionLabel：grok 的 allow_always：編輯是整個對話的編輯、指令是之後的 bash 指令都不問
func (grokFlavor) allowSessionLabel(kind string) string {
	switch kind {
	case "edit":
		return "本對話的編輯都允許"
	case "execute":
		return "本對話的指令都不再詢問"
	}
	return ""
}

// promptParams：文字區塊永遠在第一個；圖片接在後面（mimeType＋base64）
func (grokFlavor) promptParams(sid, text string, images [][2]string) map[string]any {
	prompt := []any{map[string]any{"type": "text", "text": text}}
	for _, im := range images {
		prompt = append(prompt, map[string]any{"type": "image", "mimeType": im[0], "data": im[1]})
	}
	return map[string]any{"sessionId": sid, "prompt": prompt}
}

func (grokFlavor) request(h *Hub, c *Conv, p *acpProc, m *acp.Message) bool {
	switch m.Method {
	case "x.ai/ask_user_question", "x.ai/ask_user_questions":
		qs := parseGrokQuestions(m.Params)
		if len(qs) == 0 {
			_ = p.conn.Respond(m.ID, map[string]any{"outcome": "cancelled"})
			return true
		}
		it := h.acpAddCard(c, p, "Question", &acpReq{id: m.ID, kind: "question", cancel: map[string]any{"outcome": "cancelled"}, questions: qs})
		it.Questions = qs
		it.Answers = map[string][]string{}
		return true
	case "x.ai/exit_plan_mode":
		plan := str(m.Params, "planContent")
		if plan == "" {
			plan = "（計畫內容在上面的回覆裡）"
		}
		it := h.acpAddCard(c, p, "PlanApproval", &acpReq{id: m.ID, kind: "plan", cancel: map[string]any{"outcome": "abandoned"}})
		it.DetailHTML = render.Markdown(plan)
		it.ToolKind, it.ToolName, it.Title = "plan", "確認執行計畫", "代理已規劃好做法，請確認是否照計畫執行"
		return true
	case "x.ai/folder_trust/request":
		// 初始化時沒有宣告互動式資料夾信任，grok 不會送；萬一送了，工作資料夾是使用者自己選的，回信任
		_ = p.conn.Respond(m.ID, map[string]any{"outcome": "trust"})
		return true
	}
	return false
}

// parseGrokQuestions：x.ai/ask_user_question 的題目：{questions:[{question, options:[{label, description, preview}], multiSelect}]}；答案以題目文字為鍵
func parseGrokQuestions(params map[string]any) []Question {
	qs := parseQuestions(params)
	arr, _ := params["questions"].([]any)
	for i := range qs {
		// grok 也可能用 multi_select
		for _, x := range arr {
			q, _ := x.(map[string]any)
			if str(q, "question") == qs[i].ID {
				if b, ok := q["multi_select"].(bool); ok && b {
					qs[i].Multi = true
				}
			}
		}
	}
	return qs
}

func (grokFlavor) update(h *Hub, c *Conv, p *acpProc, method string, u map[string]any) bool {
	if method == "x.ai/models/update" {
		models, _ := parseGrokModels(u)
		if len(models) > 0 {
			p.models = models
			if a := h.account(c.Meta.AccountID); a != nil {
				a.models = models
				h.acpAccountChanged(a)
			}
		}
		return true
	}
	return false
}

// parseGrokModels：{currentModelId, availableModels:[{modelId, name, description, _meta:{reasoningEfforts:[{value}]}}]}；
// 前面加一個「預設」（grok 自己的預設模型，思考強度取它的清單）
func parseGrokModels(v any) ([]Option, string) {
	state, _ := v.(map[string]any)
	arr, _ := state["availableModels"].([]any)
	current := str(state, "currentModelId")
	var out []Option
	for _, x := range arr {
		m, _ := x.(map[string]any)
		id := str(m, "modelId")
		if strings.TrimSpace(id) == "" {
			continue
		}
		meta, _ := m["_meta"].(map[string]any)
		efs, _ := meta["reasoningEfforts"].([]any)
		var efforts []string
		for i := len(efs) - 1; i >= 0; i-- { // grok 由高到低列，選單由低到高
			em, _ := efs[i].(map[string]any)
			if e := firstNonBlank(str(em, "value"), str(em, "id")); e != "" {
				efforts = append(efforts, e)
			}
		}
		out = append(out, Option{Value: id, Label: firstNonBlank(str(m, "name"), id), Description: str(m, "description"), Efforts: efforts})
	}
	if len(out) == 0 {
		return nil, current
	}
	fallback := out[0]
	for _, o := range out {
		if o.Value == current {
			fallback = o
		}
	}
	def := Option{Value: "default", Label: "預設", Description: "grok 的預設模型（目前是 " + fallback.Label + "）", Efforts: fallback.Efforts}
	return append([]Option{def}, out...), current
}

// ---- 帳戶 ----

// refreshAccount：短命的 grok agent（ACP）查 _x.ai/auth/info（只讀，判斷登入與信箱）與 _x.ai/billing（方案）；
// 模型清單在 initialize 的回應裡（不必建立對話）。不用 check_subscription：它會順便 refresh 權杖
func (grokFlavor) refreshAccount(h *Hub, a *Account) {
	cli := h.clis["Grok"]
	if cli == nil || cli.path == "" {
		a.status = "notInstalled"
		return
	}
	a.status = "checking"
	exe, env := cli.path, grokEnv(a, nil)
	dir := filepath.Join(h.root, "cache")
	go func() {
		status, email, plan, models := probeGrok(exe, env, dir)
		h.mu.Lock()
		defer h.mu.Unlock()
		a.status = status
		if email != "" {
			a.email = email
		}
		if plan != "" {
			a.plan = plan
		}
		if len(models) > 0 {
			a.models = models
		}
		h.acpAccountChanged(a)
		h.kick()
	}()
}

func probeGrok(exe string, env []string, dir string) (status, email, plan string, models []Option) {
	_ = os.MkdirAll(dir, 0o755)
	conn, err := acp.Start(acp.Options{Executable: exe, Args: grokArgs, Dir: dir, Env: env}, func(m *acp.Message) {}, nil)
	if err != nil {
		return "error", "", "", nil
	}
	defer conn.Close(3 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	init, e := conn.Request(ctx, "initialize", grokFlavor{}.initializeParams())
	if e != nil {
		return "error", "", "", nil
	}
	im, _ := init.(map[string]any)
	meta, _ := im["_meta"].(map[string]any)
	models, _ = parseGrokModels(meta["modelState"])
	info, e := conn.Request(ctx, "_x.ai/auth/info", map[string]any{})
	if e != nil {
		return "error", "", "", models
	}
	status, email = grokAuthStatus(info)
	if status != "ready" {
		return status, email, "", models
	}
	billing, e := conn.Request(ctx, "_x.ai/billing", map[string]any{})
	// 有信箱但權杖已失效、refresh 也失敗（例如在別處登出）：billing 回 Authentication required
	if (grokFlavor{}).isAuthRequired(e) {
		return "notLoggedIn", email, "", models
	}
	bm, _ := billing.(map[string]any)
	return "ready", email, strings.TrimSpace(str(bm, "subscription_tier")), models
}

// grokAuthStatus：_x.ai/auth/info → 有信箱＝已登入；沒有信箱但登入方式是 API Key＝API Key；都沒有＝未登入
func grokAuthStatus(v any) (string, string) {
	m, _ := v.(map[string]any)
	if e := strings.TrimSpace(str(m, "email")); e != "" {
		return "ready", e
	}
	if strings.Contains(strings.ToLower(str(m, "methodId")), "api") {
		return "apiKey", ""
	}
	return "notLoggedIn", ""
}
