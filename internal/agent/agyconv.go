package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kevinlincg/myriagent/internal/acp"
	"github.com/kevinlincg/myriagent/internal/render"
)

// Antigravity（agy acp --stdio，ACP；agy 1.5.4 起）。共用流程在 acpconv.go；這裡是 agy 自己的參數、方法與帳戶查詢。
// C# 版的對話是 stream-json（agy --print=）：那個模式沒有對話中途的核准協定，拿不到核准的工具被 agy 默默略過；
// ACP 有 session/request_permission、session/cancel、即時切換模型與模式，所以 Go 版直接走 ACP（規則照 AntigravityAcpRules）。
// 帳戶查詢照 AccountManager.RefreshAntigravityAsync（agy models＋記錄檔裡的登入信箱）。

type agyFlavor struct{}

func init() {
	registerProvider("Antigravity", acpProvider{agyFlavor{}})
	registerPermissionModes("Antigravity", []Option{
		{Value: "Ask", Label: "每步詢問", Description: "執行指令、修改檔案前先問你"},
		{Value: "AcceptEdits", Label: "自動接受編輯", Description: "工作區內修改檔案不再詢問，執行指令仍會問"},
		{Value: "Plan", Label: "規劃模式", Description: "只讀取與分析，先提出計畫給你確認"},
		{Value: "Auto", Label: "自動", Description: "由 agy 自動核准所有工具"},
		{Value: "FullAccess", Label: "完全自動", Description: "一律放行，不再詢問"},
	})
}

func (agyFlavor) key() string   { return "Antigravity" }
func (agyFlavor) label() string { return "Antigravity" }

// agyEnv：app 啟動的 agy 一律不自動更新；隔離帳戶的 HOME 換成它自己的家（先整理好）
func agyEnv(a *Account, extra map[string]string) []string {
	set := map[string]string{"AGY_CLI_DISABLE_AUTO_UPDATE": "true"}
	for k, v := range extra {
		set[k] = v
	}
	var unset []string
	if agyShadowed(a) {
		prepareAgyHome(a.ConfigDir)
		s, u := agyShadowEnv(a.ConfigDir, os.Getenv)
		for k, v := range s {
			set[k] = v
		}
		unset = u
	}
	return childEnv(set, unset...)
}

var agyACPArgs = []string{"acp", "--stdio"}

func (agyFlavor) launch(h *Hub, c *Conv, a *Account, wd string) ([]string, []string, error) {
	return agyACPArgs, agyEnv(a, h.agentEnvMap(c, map[string]string{"PWD": wd})), nil
}

// initializeParams：宣告支援 elicitation.form 與 terminal-auth；檔案讀寫與終端機設為 false，讓 agy 用內建工具並透過 session/request_permission 請求核准
func (agyFlavor) initializeParams() map[string]any {
	return map[string]any{
		"protocolVersion": 1,
		"clientInfo":      map[string]any{"name": "Myriagent", "version": Version},
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
			"auth":     map[string]any{"terminal": true},
			"_meta":    map[string]any{"elicitation": map[string]any{"form": true}, "terminal-auth": true},
		},
	}
}

// agySessionMeta：對話資料夾（附件）不在工作資料夾裡時加進工作區（_meta.antigravity.additionalDirectories）
func agySessionMeta(wd, convDir string) map[string]any {
	if convDir == "" || wd == convDir || strings.HasPrefix(wd, convDir+string(filepath.Separator)) {
		return nil
	}
	return map[string]any{"antigravity": map[string]any{"additionalDirectories": []any{convDir}}}
}

func (agyFlavor) newSessionParams(wd, convDir, preferredID string, cfg acpConfig) map[string]any {
	out := map[string]any{"cwd": wd, "mcpServers": []any{}}
	if m := agySessionMeta(wd, convDir); m != nil {
		out["_meta"] = m
	}
	return out
}

func (agyFlavor) resumeParams(sid, wd, convDir string, cfg acpConfig) map[string]any {
	out := map[string]any{"sessionId": sid, "cwd": wd, "mcpServers": []any{}}
	if m := agySessionMeta(wd, convDir); m != nil {
		out["_meta"] = m
	}
	return out
}

func (agyFlavor) presetSessionID() bool { return false }

// isSessionNotFound：agy 續接失敗的錯誤格式沒有固定的代碼（對話存在本機，失敗多半就是找不到）：登入以外的錯誤一律改開新對話並附上前文
func (agyFlavor) isSessionNotFound(e *acp.Error) bool { return true }

var (
	agyNotSignedIn = regexp.MustCompile(`(?i)please sign in|not logged in(to)?|sign in to view|authentication (required|failed)`)
	agySignedIn    = regexp.MustCompile(`(?m)OAuth: authenticated successfully as ([^\s@]+@[^\s@]+\.[^\s@]+)\s*$`)
	agyEffortTail  = regexp.MustCompile(`(?i)-(minimal|low|medium|high|xhigh|max)$`)
	agyEffortLabel = regexp.MustCompile(`(?i)\s*\((minimal|low|medium|high|xhigh|max|extra high)\)\s*$`)
	agyModelID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
)

func (agyFlavor) isAuthRequired(e *acp.Error) bool {
	return e != nil && agyNotSignedIn.MatchString(e.Text)
}

func (agyFlavor) notSignedIn() string {
	return "Antigravity 尚未登入，請到「帳戶」頁按登入，完成 Google 登入後再送出"
}

// agyModeID：app 的權限模式 → ACP 的 modeId（每步詢問＝default；自動接受編輯＝accept-edits；自動、完全自動＝auto；規劃＝plan）
func agyModeID(mode string) string {
	switch mode {
	case "Ask":
		return "default"
	case "Plan":
		return "plan"
	case "Auto", "FullAccess":
		return "auto"
	}
	return "accept-edits"
}

// agyEfforts：推理強度由低到高（模型選單的排序）
var agyEffortOrder = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

var agyDefaultEfforts = []string{"low", "medium", "high", "xhigh", "max"}

func (agyFlavor) sessionOpened(h *Hub, c *Conv, p *acpProc, result map[string]any, created bool, cfg acpConfig) {
	efforts := agyThoughtLevels(result)
	modelsObj, _ := result["models"].(map[string]any)
	current := str(modelsObj, "currentModelId")
	var models []Option
	if arr, ok := modelsObj["availableModels"].([]any); ok {
		for _, x := range arr {
			m, _ := x.(map[string]any)
			id := strings.TrimSpace(str(m, "modelId"))
			if id == "" {
				continue
			}
			models = append(models, Option{Value: id, Label: firstNonBlank(str(m, "name"), id), Description: str(m, "description"), Efforts: efforts})
		}
	}
	if len(models) > 0 {
		label := current
		for _, m := range models {
			if m.Value == current {
				label = m.Label
			}
		}
		def := Option{Value: "default", Label: "預設", Description: "agy 的預設模型", Efforts: efforts}
		if label != "" {
			def.Description = "agy 的預設模型（目前是 " + label + "）"
		}
		models = append([]Option{def}, models...)
		p.models = models
	}
	if current != "" && cfg.model == "" {
		p.defaultModel = current
	}
	if a := h.account(c.Meta.AccountID); a != nil {
		if len(models) > 0 {
			a.models = models
		}
		meta, _ := result["_meta"].(map[string]any)
		ag, _ := meta["antigravity"].(map[string]any)
		if tier, ok := ag["tier"].(map[string]any); ok {
			if name := firstNonBlank(str(tier, "name"), str(tier, "id")); name != "" {
				a.plan = strings.TrimSpace(name)
			}
		}
		h.acpAccountChanged(a)
	}
	if modes, ok := result["modes"].(map[string]any); ok {
		if id := str(modes, "currentModeId"); id != "" {
			p.applied["mode"] = id
		}
	}
	if created {
		// 新對話用 agy 的預設模型：選「預設」就不必再送
		p.applied["model"] = ""
		if cfg.model != "" && cfg.model == current {
			p.applied["model"] = cfg.model
		}
	}
}

// agyThoughtLevels：configOptions 的 thought_level 選項（沒有就用預設的強度清單）
func agyThoughtLevels(result map[string]any) []string {
	arr, _ := result["configOptions"].([]any)
	for _, x := range arr {
		m, _ := x.(map[string]any)
		if str(m, "id") != "thought_level" && str(m, "category") != "thought_level" {
			continue
		}
		var out []string
		opts, _ := m["options"].([]any)
		for _, o := range opts {
			om, _ := o.(map[string]any)
			if v := strings.TrimSpace(str(om, "value")); v != "" {
				out = append(out, v)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return agyDefaultEfforts
}

// applyConfig：模型（session/set_model）、推理強度（session/set_config_option 的 thought_level）、權限模式（session/set_mode）
func (agyFlavor) applyConfig(ctx context.Context, p *acpProc, sid string, cfg acpConfig) []string {
	var notices []string
	request := func(method string, params map[string]any) *acp.Error {
		rctx, cancel := context.WithTimeout(ctx, acpConfigTimeout)
		defer cancel()
		_, e := p.conn.Request(rctx, method, params)
		return e
	}
	sentModel := false
	if m, ok := p.applied["model"]; !ok || m != cfg.model {
		send := firstNonBlank(cfg.model, cfg.defaultModel)
		if send != "" {
			if e := request("session/set_model", map[string]any{"sessionId": sid, "modelId": send}); e != nil {
				notices = append(notices, "Antigravity 無法切換到模型 "+send+"："+e.Text)
			} else {
				sentModel = true
			}
		}
		p.applied["model"] = cfg.model
	}
	if cfg.effort != "" && cfg.effortOK && (sentModel || p.applied["effort"] != cfg.effort) {
		if e := request("session/set_config_option", map[string]any{"sessionId": sid, "configId": "thought_level", "value": cfg.effort}); e != nil {
			notices = append(notices, "Antigravity 無法切換推理強度（"+cfg.effort+"）："+e.Text)
		}
		p.applied["effort"] = cfg.effort
	}
	if id := agyModeID(cfg.mode); p.applied["mode"] != id {
		if e := request("session/set_mode", map[string]any{"sessionId": sid, "modeId": id}); e != nil {
			notices = append(notices, "Antigravity 無法切換權限模式："+e.Text)
		} else {
			p.applied["mode"] = id
		}
	}
	return notices
}

// modeFromID：agy 自己換了模式 → app 的權限模式（auto 對到自動或完全自動，目前是其中之一就不改）
func (agyFlavor) modeFromID(id, current string) string {
	switch id {
	case "default":
		return "Ask"
	case "accept-edits":
		return "AcceptEdits"
	case "plan":
		return "Plan"
	case "auto":
		if current == "Auto" || current == "FullAccess" {
			return ""
		}
		return "Auto"
	}
	return ""
}

// clientApprove：完全自動時 agy 萬一還來問就直接放行（其他模式由 agy 自己的模式決定要不要問）
func (agyFlavor) clientApprove(h *Hub, c *Conv, p *acpProc, perm acpPermission) bool {
	return c.Meta.Permission == "FullAccess"
}

func (agyFlavor) allowSessionLabel(kind string) string { return "" }

// promptParams：inline base64 圖片區塊在前、文字區塊在後（AntigravityAcpRules.BuildPromptParams）
func (agyFlavor) promptParams(sid, text string, images [][2]string) map[string]any {
	prompt := []any{}
	for _, im := range images {
		prompt = append(prompt, map[string]any{"type": "image", "mimeType": im[0], "data": im[1]})
	}
	prompt = append(prompt, map[string]any{"type": "text", "text": text})
	return map[string]any{"sessionId": sid, "prompt": prompt}
}

func (agyFlavor) request(h *Hub, c *Conv, p *acpProc, m *acp.Message) bool {
	if m.Method != "elicitation/create" {
		return false
	}
	qs := parseElicitation(m.Params)
	if len(qs) == 0 {
		_ = p.conn.Respond(m.ID, map[string]any{"action": "decline"})
		return true
	}
	it := h.acpAddCard(c, p, "Question", &acpReq{id: m.ID, kind: "elicitation", cancel: map[string]any{"action": "decline"}, questions: qs})
	it.Questions = qs
	it.Answers = map[string][]string{}
	if msg := strings.TrimSpace(str(m.Params, "message")); msg != "" && len(qs) > 1 {
		it.DetailHTML = render.Text(msg)
	}
	return true
}

// parseElicitation：elicitation/create（agy 的 ask_question 工具）：requestedSchema.properties 的每個屬性一題；
// enum／oneOf／anyOf 是選項，array 是複選
func parseElicitation(params map[string]any) []Question {
	top := strings.TrimSpace(str(params, "message"))
	schema, _ := params["requestedSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		if top == "" {
			return nil
		}
		return []Question{{ID: "answer", Header: "問題", Text: top, AllowOther: true, Options: []QuestionOption{}}}
	}
	// 屬性的順序：有 required 照 required，其餘照名稱（JSON 物件解開後沒有順序）
	var names []string
	seen := map[string]bool{}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok && props[s] != nil && !seen[s] {
				names = append(names, s)
				seen[s] = true
			}
		}
	}
	var rest []string
	for k := range props {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	names = append(names, rest...)
	var out []Question
	for _, name := range names {
		ps, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		multi := strings.EqualFold(str(ps, "type"), "array")
		title := strings.TrimSpace(str(ps, "title"))
		text := firstNonBlank(strings.TrimSpace(str(ps, "description")), title, top, name)
		src := ps
		if multi {
			if items, ok := ps["items"].(map[string]any); ok {
				src = items
			}
		}
		out = append(out, Question{ID: name, Header: firstNonBlank(title, name), Text: text, Multi: multi, AllowOther: true, Options: schemaOptions(src)})
	}
	return out
}

func schemaOptions(schema map[string]any) []QuestionOption {
	out := []QuestionOption{}
	if enum, ok := schema["enum"].([]any); ok {
		for _, v := range enum {
			if s := strings.TrimSpace(anyString(v)); s != "" {
				out = append(out, QuestionOption{Label: s})
			}
		}
		return out
	}
	choices, ok := schema["oneOf"].([]any)
	if !ok {
		choices, _ = schema["anyOf"].([]any)
	}
	for _, x := range choices {
		m, _ := x.(map[string]any)
		val := strings.TrimSpace(str(m, "const"))
		if val == "" {
			if e, ok := m["enum"].([]any); ok && len(e) > 0 {
				val = strings.TrimSpace(anyString(e[0]))
			}
		}
		title := strings.TrimSpace(str(m, "title"))
		label := firstNonBlank(val, title)
		if label == "" {
			continue
		}
		desc := strings.TrimSpace(str(m, "description"))
		if desc == "" && title != "" && title != label {
			desc = title
		}
		out = append(out, QuestionOption{Label: label, Description: desc})
	}
	return out
}

func anyString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	}
	return strings.TrimSpace(strings.Trim(indentJSON(v), `"`))
}

func (agyFlavor) update(h *Hub, c *Conv, p *acpProc, method string, u map[string]any) bool {
	// session_info_update（agy 自動產生的對話標題）：app 的標題由使用者的第一句決定，不跟著改
	return false
}

// ---- 帳戶 ----

// refreshAccount：agy --log-file <檔> models：輸出判斷登入狀態、模型清單；記錄檔裡讀登入的信箱（讀完刪掉）
func (agyFlavor) refreshAccount(h *Hub, a *Account) {
	cli := h.clis["Antigravity"]
	if cli == nil || cli.path == "" {
		a.status = "notInstalled"
		return
	}
	a.status = "checking"
	exe, env := cli.path, agyEnv(a, nil)
	dir := filepath.Join(h.root, "cache", "agy-empty")
	go func() {
		status, email, models := probeAgy(exe, env, dir)
		h.mu.Lock()
		defer h.mu.Unlock()
		a.status = status
		if email != "" {
			a.email = email
		}
		if len(models) > 0 {
			a.models = append([]Option{{Value: "default", Label: "預設", Description: "agy 的預設模型", Efforts: agyDefaultEfforts}}, models...)
		}
		h.acpAccountChanged(a)
		h.kick()
	}()
}

// agyCommandTimeout：查詢的時限（逾時重試一次）
var agyCommandTimeout = 30 * time.Second

func probeAgy(exe string, env []string, dir string) (status, email string, models []Option) {
	_ = os.MkdirAll(dir, 0o755)
	for attempt := 0; attempt < 2; attempt++ {
		logFile := filepath.Join(os.TempDir(), "myriagent-agy-"+newID("")+".log")
		stdout, stderr, code, timedOut := runCLI(exe, []string{"--log-file", logFile, "models"}, env, dir, agyCommandTimeout)
		b, _ := os.ReadFile(logFile)
		_ = os.Remove(logFile)
		if timedOut && attempt == 0 {
			continue
		}
		status, models = evaluateAgyModels(code, stdout, stderr, timedOut)
		return status, parseAgySignedInEmail(string(b)), models
	}
	return "error", "", nil
}

// runCLI：跑一次 CLI（自己一個程序群組，逾時連子程序一起結束）
func runCLI(exe string, args, env []string, dir string, timeout time.Duration) (stdout, stderr string, code int, timedOut bool) {
	cmd := exec.Command(exe, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		return "", err.Error(), -1, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code = 0
		if err != nil {
			code = -1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		return out.String(), errb.String(), code, false
	case <-time.After(timeout):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return out.String(), errb.String(), -1, true
	}
}

// evaluateAgyModels：輸出含「Please sign in」之類＝未登入；結束碼 0＝已登入（模型清單可能是空的）；逾時或其他失敗＝檢查失敗
func evaluateAgyModels(code int, stdout, stderr string, timedOut bool) (string, []Option) {
	if agyNotSignedIn.MatchString(stdout + "\n" + stderr) {
		return "notLoggedIn", nil
	}
	if timedOut || code != 0 {
		return "error", nil
	}
	return "ready", parseAgyModels(stdout)
}

// parseAgySignedInEmail：agy 記錄裡登入成功的信箱（有好幾行時取最後一行；沒有回空）
func parseAgySignedInEmail(log string) string {
	ms := agySignedIn.FindAllStringSubmatch(log, -1)
	if len(ms) == 0 {
		return ""
	}
	e := ms[len(ms)-1][1]
	if len(e) > 320 {
		return ""
	}
	return e
}

// parseAgyModels：agy models 的每行「id<TAB>顯示名稱」（也接受空白分隔或只有 id；id 必須含連字號）。
// 每個 id 都帶推理強度，同一個模型的幾個強度合併成一項：值是去掉強度的基本 id、顯示名稱去掉「(Low)」
func parseAgyModels(stdout string) []Option {
	type group struct {
		id, label string
		efforts   []string
	}
	var groups []*group
	seen := map[string]bool{}
	for _, raw := range strings.Split(strings.ReplaceAll(stdout, "\r", ""), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var id, label string
		if i := strings.IndexByte(line, '\t'); i > 0 {
			id, label = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		} else {
			f := strings.SplitN(line, " ", 2)
			id = f[0]
			if len(f) > 1 {
				label = strings.TrimSpace(f[1])
			}
		}
		if !strings.Contains(id, "-") || !agyModelID.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		base, effort := id, ""
		if m := agyEffortTail.FindStringSubmatchIndex(id); m != nil {
			base, effort = id[:m[0]], strings.ToLower(id[m[2]:m[3]])
			label = strings.TrimSpace(agyEffortLabel.ReplaceAllString(label, ""))
		}
		var g *group
		for _, x := range groups {
			if x.id == base {
				g = x
			}
		}
		if g == nil {
			g = &group{id: base, label: label}
			groups = append(groups, g)
		}
		if effort != "" && !contains(g.efforts, effort) {
			g.efforts = append(g.efforts, effort)
		}
	}
	var out []Option
	for _, g := range groups {
		sort.SliceStable(g.efforts, func(i, j int) bool { return effortRank(g.efforts[i]) < effortRank(g.efforts[j]) })
		out = append(out, Option{Value: g.id, Label: firstNonBlank(g.label, g.id), Efforts: g.efforts})
	}
	return out
}

func effortRank(e string) int {
	for i, x := range agyEffortOrder {
		if x == e {
			return i
		}
	}
	return len(agyEffortOrder)
}
