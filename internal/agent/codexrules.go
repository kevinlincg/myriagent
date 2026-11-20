package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kevinlincg/myriagent/internal/render"
)

// 這個檔是 Codex app-server 的純函式規則：權限模式對應、核准決策、問卷答案、指令與改檔的說明、模型清單。
// 照 C# 版的 CodexAppServerRules.cs（協定實測見 docs/protocol-notes.md）；codex 0.170.0 的型別以 `codex app-server generate-ts` 核對過。

// codexPolicy：一種權限模式對應的核准政策與沙箱
type codexPolicy struct {
	approval string // untrusted | on-request | never
	sandbox  string // workspace-write | danger-full-access
}

// normalizeCodexPermission：Codex 沒有的模式換成最接近的（自動接受編輯→自動、規劃模式→每步詢問）
func normalizeCodexPermission(mode string) string {
	switch mode {
	case "AcceptEdits":
		return "Auto"
	case "Plan", "", "default":
		return "Ask"
	}
	return mode
}

// codexPolicyFor：權限模式 → 核准政策＋沙箱。macOS／Linux 的沙箱（Seatbelt、Landlock）不必另外設定，一律當成就緒
func codexPolicyFor(mode string) codexPolicy {
	switch normalizeCodexPermission(mode) {
	case "FullAccess":
		return codexPolicy{"never", "danger-full-access"}
	case "Auto":
		return codexPolicy{"on-request", "workspace-write"}
	default:
		return codexPolicy{"untrusted", "workspace-write"}
	}
}

// codexSandboxPolicy：turn/start 的 sandboxPolicy（物件形式；workspace-write 開放網路，讓自動化工作能連線）
func codexSandboxPolicy(sandbox string, writableRoots []string) map[string]any {
	switch sandbox {
	case "workspace-write":
		roots := []any{}
		for _, r := range writableRoots {
			if strings.TrimSpace(r) != "" {
				roots = append(roots, r)
			}
		}
		return map[string]any{"type": "workspaceWrite", "writableRoots": roots, "networkAccess": true, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	case "read-only":
		return map[string]any{"type": "readOnly", "networkAccess": true}
	}
	return map[string]any{"type": "dangerFullAccess"}
}

func codexThreadStartParams(cwd, model string, p codexPolicy, instructions string) map[string]any {
	m := map[string]any{
		"cwd": cwd, "approvalPolicy": p.approval, "sandbox": p.sandbox,
		"config": map[string]any{"sandbox_workspace_write.network_access": true},
	}
	if model != "" {
		m["model"] = model
	}
	if strings.TrimSpace(instructions) != "" {
		m["developerInstructions"] = instructions
	}
	return m
}

// codexThreadResumeParams：新程序續接既有對話（只帶 metadata，不載入整段歷史）
func codexThreadResumeParams(thread, cwd, model string, p codexPolicy) map[string]any {
	m := map[string]any{"threadId": thread, "cwd": cwd, "approvalPolicy": p.approval, "sandbox": p.sandbox, "excludeTurns": true}
	if model != "" {
		m["model"] = model
	}
	return m
}

// codexTurnStartParams：一則使用者訊息；每輪都帶權限、沙箱、模型、強度，切換設定下一輪就生效
func codexTurnStartParams(thread, text string, images []string, p codexPolicy, writableRoots []string, model, effort string) map[string]any {
	m := map[string]any{
		"threadId": thread, "input": codexUserInput(text, images),
		"approvalPolicy": p.approval, "sandboxPolicy": codexSandboxPolicy(p.sandbox, writableRoots),
	}
	if model != "" {
		m["model"] = model
	}
	if effort != "" {
		m["effort"] = effort
	}
	return m
}

func codexUserInput(text string, images []string) []any {
	in := []any{map[string]any{"type": "text", "text": text, "text_elements": []any{}}}
	for _, p := range images {
		in = append(in, map[string]any{"type": "localImage", "path": p})
	}
	return in
}

// codexOptionValue：「default」與空白都是不指定
func codexOptionValue(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "default") {
		return ""
	}
	return strings.TrimSpace(v)
}

// ---- thread/resume 的錯誤 ----

const (
	resumeOther = iota
	resumeNotFound
	resumeActiveWriter
)

// classifyResumeError：實測（0.160.1）同一個 CODEX_HOME 的第二個 app-server 續接已載入的 thread 會回
// 「thread {id} already has an active writer」；真的找不到紀錄是「no rollout found for thread id {id}」
func classifyResumeError(msg string) int {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "already has an active writer"):
		return resumeActiveWriter
	case strings.Contains(l, "no rollout found"), strings.Contains(l, "thread not found"):
		return resumeNotFound
	}
	return resumeOther
}

// ---- 核准 ----

// codexDecision：核准卡選擇 → 回應的 decision。允許＝accept；本對話都允許＝有 acceptForSession 用它，否則 accept（宿主自己記住）；
// 拒絕＝有 decline 用 decline（只拒這一步），否則 cancel（會結束本輪）。availableDecisions 沒帶（0.170.0 起都不帶）時照完整的清單。
// acceptWithExecpolicyAmendment 會把規則寫進使用者的 Codex 設定，一律不用
func codexDecision(available []any, choice string) string {
	has := map[string]bool{}
	for _, d := range available {
		if s, ok := d.(string); ok {
			has[s] = true
		}
	}
	unknown := len(available) == 0
	switch choice {
	case "allow":
		return "accept"
	case "allowSession":
		if unknown || has["acceptForSession"] {
			return "acceptForSession"
		}
		return "accept"
	}
	if unknown || has["decline"] {
		return "decline"
	}
	return "cancel"
}

// codexLegacyDecision：舊版（v1）execCommandApproval／applyPatchApproval 的 decision
func codexLegacyDecision(choice string) any {
	switch choice {
	case "allow":
		return "approved"
	case "allowSession":
		return "approved_for_session"
	}
	return map[string]any{"denied": map[string]any{"rejection": "使用者拒絕"}}
}

// codexApprovalResult：回應核准請求的 result
func codexApprovalResult(method string, params map[string]any, choice string) map[string]any {
	switch method {
	case "item/permissions/requestApproval":
		if choice == "deny" {
			return map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		}
		granted := map[string]any{}
		if req, ok := params["permissions"].(map[string]any); ok {
			for _, k := range []string{"network", "fileSystem"} {
				if v, ok := req[k].(map[string]any); ok {
					granted[k] = v
				}
			}
		}
		scope := "turn"
		if choice == "allowSession" {
			scope = "session"
		}
		return map[string]any{"permissions": granted, "scope": scope}
	case "execCommandApproval", "applyPatchApproval":
		return map[string]any{"decision": codexLegacyDecision(choice)}
	case "mcpServer/elicitation/request":
		// 允許＝accept；本對話都允許＝accept 加 _meta.persist＝session（Codex 自己記住）；拒絕＝decline（這一輪繼續）
		action := "accept"
		if choice == "deny" {
			action = "decline"
		}
		var meta any
		if choice == "allowSession" {
			meta = map[string]any{"persist": "session"}
		}
		return map[string]any{"action": action, "content": nil, "_meta": meta}
	}
	avail, _ := params["availableDecisions"].([]any)
	return map[string]any{"decision": codexDecision(avail, choice)}
}

// codexDeclineResult：沒有人回應就收掉的請求（按停止、這一輪結束）：問卷回空答案，表單取消，核准一律拒絕
func codexDeclineResult(method string, params map[string]any) map[string]any {
	switch method {
	case "item/tool/requestUserInput":
		return codexUserInputResponse(nil)
	case "mcpServer/elicitation/request":
		if codexMcpToolApproval(params) == nil {
			return map[string]any{"action": "cancel", "content": nil, "_meta": nil}
		}
	}
	return codexApprovalResult(method, params, "deny")
}

// codexApprovalKind：「本對話都允許」怎麼記：檔案修改整類放行；指令與權限只放行完全相同的請求
func codexApprovalKind(method string) string {
	switch method {
	case "item/fileChange/requestApproval", "applyPatchApproval":
		return "fileChange"
	case "item/permissions/requestApproval":
		return "permissions"
	}
	return "command"
}

// codexSessionApprovalKey：「本對話都允許」記住的完全相同比對鍵；空＝不記。
// 指令：種類＋原始指令＋工作資料夾＋額外權限（Codex 自己的快取鍵也含額外權限，多要權限時會刻意再問）；網路存取：主機＋通訊協定；權限請求：請求的權限內容
func codexSessionApprovalKey(method string, p map[string]any) string {
	const sep = "\x01"
	switch method {
	case "item/commandExecution/requestApproval":
		if network, ok := p["networkApprovalContext"].(map[string]any); ok {
			host := str(network, "host")
			if strings.TrimSpace(host) == "" {
				return ""
			}
			return strings.Join([]string{"network", host, str(network, "protocol")}, sep)
		}
		cmd := str(p, "command")
		if strings.TrimSpace(cmd) == "" {
			return ""
		}
		return strings.Join([]string{"command", orDefault(str(p, "kind"), "command"), cmd, str(p, "cwd"), canonicalJSON(p["additionalPermissions"])}, sep)
	case "execCommandApproval":
		argv, _ := p["command"].([]any)
		if len(argv) == 0 {
			return ""
		}
		return strings.Join([]string{"command", "command", canonicalJSON(argv), str(p, "cwd"), ""}, sep)
	case "item/permissions/requestApproval":
		if perms, ok := p["permissions"].(map[string]any); ok {
			return "permissions" + sep + canonicalJSON(perms)
		}
	}
	return ""
}

// canonicalJSON：比對用的 JSON 原文（Go 的 map 序列化依鍵排序，同樣內容一定得到同一串）；沒有或 null 回空字串
func canonicalJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// codexMcpApproval：MCP 工具呼叫的核准（mcpServer/elicitation/request 裡 _meta.codex_approval_kind 是 mcp_tool_call）
type codexMcpApproval struct {
	server, title, detail string
	forSession            bool
}

func codexMcpToolApproval(p map[string]any) *codexMcpApproval {
	meta, _ := p["_meta"].(map[string]any)
	if meta == nil || str(meta, "codex_approval_kind") != "mcp_tool_call" {
		return nil
	}
	message := str(p, "message")
	title := firstNonBlank(str(meta, "tool_title"), firstLine(message), "MCP 工具")
	var parts []string
	if title != message && strings.TrimSpace(message) != "" {
		parts = append(parts, message)
	}
	if d := str(meta, "tool_description"); strings.TrimSpace(d) != "" {
		parts = append(parts, d)
	}
	if tp, ok := meta["tool_params"]; ok && tp != nil {
		parts = append(parts, truncate(indentJSON(tp), 4000))
	}
	// persist 可能是 "session"、"always" 或兩者的陣列；只用 session（always 會寫進使用者的 Codex 設定）
	forSession := false
	switch v := meta["persist"].(type) {
	case string:
		forSession = v == "session"
	case []any:
		for _, x := range v {
			if s, _ := x.(string); s == "session" {
				forSession = true
			}
		}
	}
	return &codexMcpApproval{server: orDefault(str(p, "serverName"), "MCP"), title: title, detail: strings.Join(parts, "\n\n"), forSession: forSession}
}

// codexUnsupportedElicitation：宿主不支援的 MCP 請求（真正要填欄位的表單、開網址、驗證身分）：婉拒時給使用者看的說明
func codexUnsupportedElicitation(p map[string]any) string {
	kind := "送來不支援的請求"
	switch str(p, "mode") {
	case "form", "openai/form", "openaiForm":
		kind = "要求填寫表單"
	case "url":
		kind = "要求開啟網址"
	case "openai/userVerification":
		kind = "要求驗證身分"
	}
	ctx := ""
	if m := firstNonBlank(str(p, "message"), str(p, "title")); m != "" {
		ctx = "（" + firstLine(m) + "）"
	}
	return fmt.Sprintf("MCP 伺服器「%s」%s%s，目前不支援，已婉拒", orDefault(str(p, "serverName"), "MCP"), kind, ctx)
}

// ---- 問卷 ----

// codexUserInputResponse：item/tool/requestUserInput 的回應：題目 id → { answers: [...] }
func codexUserInputResponse(answers map[string][]string) map[string]any {
	out := map[string]any{}
	for q, vals := range answers {
		clean := []any{}
		for _, v := range vals {
			if s := strings.TrimSpace(v); s != "" {
				clean = append(clean, s)
			}
		}
		out[q] = map[string]any{"answers": clean}
	}
	return map[string]any{"answers": out}
}

func codexUserInputQuestions(p map[string]any) []Question {
	list, _ := p["questions"].([]any)
	out := []Question{}
	for _, x := range list {
		q, _ := x.(map[string]any)
		id := str(q, "id")
		if strings.TrimSpace(id) == "" {
			continue
		}
		other, _ := q["isOther"].(bool)
		secret, _ := q["isSecret"].(bool)
		qq := Question{ID: id, Header: str(q, "header"), Text: str(q, "question"), AllowOther: other, Secret: secret, Options: []QuestionOption{}}
		opts, _ := q["options"].([]any)
		for _, o := range opts {
			om, _ := o.(map[string]any)
			if l := str(om, "label"); strings.TrimSpace(l) != "" {
				qq.Options = append(qq.Options, QuestionOption{Label: l, Description: str(om, "description")})
			}
		}
		if len(qq.Options) == 0 {
			qq.AllowOther = true
		}
		out = append(out, qq)
	}
	return out
}

// codexAsyncQuestions：agentMessage.questions（非同步提問，不擋住本輪）→ 問卷；作答改以下一則訊息送出
func codexAsyncQuestions(v any, itemID string) []Question {
	list, _ := v.([]any)
	out := []Question{}
	for _, x := range list {
		q, _ := x.(map[string]any)
		title := str(q, "title")
		if strings.TrimSpace(title) == "" {
			continue
		}
		qq := Question{ID: fmt.Sprintf("%s:%d", itemID, len(out)), Text: title, AllowOther: true, Options: []QuestionOption{}}
		opts, _ := q["options"].([]any)
		for _, o := range opts {
			if l, _ := o.(string); strings.TrimSpace(l) != "" {
				qq.Options = append(qq.Options, QuestionOption{Label: l})
			}
		}
		out = append(out, qq)
	}
	return out
}

// answerMessagePrefix：非同步提問的答案當作下一則訊息送出時的開頭（同 C# 的 AnswerMessageRules）
const answerMessagePrefix = "我的回答：\n"

// skippedAsyncQuestionMessage：為了等回答而停下的這一輪，使用者略過了那個問題：送給代理的接續訊息
const skippedAsyncQuestionMessage = "我略過了你剛才的問題，請依你的判斷繼續。"

// composeAnswerMessage：每題一行「題目：答案、答案」，依題目的順序
func composeAnswerMessage(questions []Question, answers map[string][]string) string {
	var lines []string
	for _, q := range questions {
		lines = append(lines, q.Text+"："+strings.Join(answers[q.ID], "、"))
	}
	return answerMessagePrefix + strings.Join(lines, "\n")
}

// ---- 拒絕的說明（Codex 沒有欄位可放，這一輪結束後當下一則訊息送出）----

type deniedOperation struct{ toolName, title, feedback string }

func denyFeedbackMessage(list []deniedOperation) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return "（我剛才拒絕了那個操作）" + list[0].feedback
	}
	var lines []string
	for i, d := range list {
		op := joinWith("：", d.toolName, strings.TrimSuffix(firstLine(d.title), " …"))
		if op != "" {
			lines = append(lines, fmt.Sprintf("%d. 「%s」：%s", i+1, op, d.feedback))
		} else {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, d.feedback))
		}
	}
	return "（我剛才拒絕了這幾個操作）\n" + strings.Join(lines, "\n")
}

func joinWith(sep string, xs ...string) string {
	var out []string
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			out = append(out, x)
		}
	}
	return strings.Join(out, sep)
}

// joinLines：非空的各段以換行接起來
func joinLines(xs ...string) string { return joinWith("\n", xs...) }

// ---- 指令、改檔、MCP 的說明 ----

// codexDescribeCommand：commandActions 有原始指令就用它；否則剝掉 shell 包裝（-lc '…'、Windows 的 -Command '…'）
func codexDescribeCommand(item map[string]any) string {
	actions, _ := item["commandActions"].([]any)
	var cmds []string
	for _, a := range actions {
		am, _ := a.(map[string]any)
		if c := str(am, "command"); strings.TrimSpace(c) != "" {
			cmds = append(cmds, c)
		}
	}
	if len(cmds) > 0 {
		return strings.Join(cmds, " && ")
	}
	cmd := str(item, "command")
	for _, marker := range []string{"-Command '", "-lc '", "-c '"} {
		if at := strings.Index(cmd, marker); at >= 0 && strings.HasSuffix(cmd, "'") && len(cmd) > at+len(marker) {
			inner := cmd[at+len(marker) : len(cmd)-1]
			if marker == "-Command '" {
				return strings.ReplaceAll(inner, "''", "'")
			}
			return strings.ReplaceAll(inner, `'\''`, "'")
		}
	}
	return cmd
}

// diffFromUnified：一段 unified diff → 行差異。kind 是 add／delete 時整段都是新增／刪除
func diffFromUnified(diff, kind string) []DiffLine {
	lines := splitLines(diff)
	out := []DiffLine{}
	if kind == "add" || kind == "delete" {
		t := "add"
		if kind == "delete" {
			t = "del"
		}
		for _, l := range lines {
			out = append(out, DiffLine{T: t, S: l})
		}
		return out
	}
	// 檔頭（---／+++／index 等）只出現在每個檔案的第一個 @@ 之前；進了 @@ 區塊後一律依第一個字元分類
	inHunk := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "diff --git"):
			inHunk = false
		case strings.HasPrefix(l, `\ No newline`):
		case !inHunk && (strings.HasPrefix(l, "index ") || strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") ||
			strings.HasPrefix(l, "new file mode") || strings.HasPrefix(l, "deleted file mode")):
		case strings.HasPrefix(l, "@@"):
			inHunk = true
			out = append(out, DiffLine{T: "hunk", S: l})
		case strings.HasPrefix(l, "+"):
			out = append(out, DiffLine{T: "add", S: l[1:]})
		case strings.HasPrefix(l, "-"):
			out = append(out, DiffLine{T: "del", S: l[1:]})
		default:
			out = append(out, DiffLine{T: "ctx", S: strings.TrimPrefix(l, " ")})
		}
	}
	return out
}

// codexFileChanges：fileChange 的 changes → 標題、第一個檔案路徑、合併後的行差異（多個檔案時每個檔案前加一行檔名）
func codexFileChanges(item map[string]any) (title, path string, diff []DiffLine, stats *DiffStats) {
	changes, _ := item["changes"].([]any)
	var paths []string
	for _, c := range changes {
		cm, _ := c.(map[string]any)
		if p := str(cm, "path"); strings.TrimSpace(p) != "" {
			paths = append(paths, p)
		}
	}
	switch len(paths) {
	case 0:
		title = "修改檔案"
	case 1:
		title = paths[0]
	default:
		title = fmt.Sprintf("%s 等 %d 個檔案", paths[0], len(paths))
	}
	if len(paths) > 0 {
		path = paths[0]
	}
	st := &DiffStats{}
	for _, c := range changes {
		cm, _ := c.(map[string]any)
		kind := ""
		if k, ok := cm["kind"].(map[string]any); ok {
			kind = str(k, "type")
		} else {
			kind = str(cm, "kind")
		}
		if len(changes) > 1 {
			diff = append(diff, DiffLine{T: "hunk", S: str(cm, "path")})
		}
		for _, l := range diffFromUnified(str(cm, "diff"), kind) {
			switch l.T {
			case "add":
				st.Add++
			case "del":
				st.Del++
			}
			diff = append(diff, l)
		}
	}
	if len(diff) == 0 {
		return title, path, nil, nil
	}
	diff, stats = capDiff(diff, st)
	return title, path, diff, stats
}

func codexMcpResult(v any) string {
	r, _ := v.(map[string]any)
	content, _ := r["content"].([]any)
	var parts []string
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if str(cm, "type") == "text" {
			if t := str(cm, "text"); t != "" {
				parts = append(parts, t)
			}
		} else {
			parts = append(parts, "[非文字內容]")
		}
	}
	return strings.Join(parts, "\n")
}

// ---- 模型 ----

// codexModels：model/list 的結果 → 模型選單（略過隱藏的；預設模型排最前；最前面加「預設」）
func codexModels(res map[string]any) []Option {
	data, _ := res["data"].([]any)
	type row struct {
		o   Option
		def bool
	}
	var rows []row
	for _, x := range data {
		m, _ := x.(map[string]any)
		if hidden, _ := m["hidden"].(bool); hidden {
			continue
		}
		id := firstNonBlank(str(m, "model"), str(m, "id"))
		if id == "" {
			continue
		}
		o := Option{Value: id, Label: firstNonBlank(str(m, "displayName"), id), Description: str(m, "description")}
		efforts, _ := m["supportedReasoningEfforts"].([]any)
		for _, e := range efforts {
			em, _ := e.(map[string]any)
			if s := str(em, "reasoningEffort"); s != "" {
				o.Efforts = append(o.Efforts, s)
			}
		}
		// 不指定強度時用的強度（2026-11-12 實測 model/list 每個模型都有 defaultReasoningEffort）
		if d := str(m, "defaultReasoningEffort"); d != "" {
			o.Effort = &d
		}
		def, _ := m["isDefault"].(bool)
		rows = append(rows, row{o, def})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].def && !rows[j].def })
	if len(rows) == 0 {
		return nil
	}
	out := []Option{defaultModelOption}
	for _, r := range rows {
		out = append(out, r.o)
	}
	return out
}

// codexPlanName：account/read 的 planType → 顯示的方案名稱
func codexPlanName(planType string) string {
	switch planType {
	case "", "unknown":
		return ""
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "prolite":
		return "Pro Lite"
	case "promax":
		return "Pro Max"
	case "team":
		return "Team"
	case "business":
		return "Business"
	case "enterprise":
		return "Enterprise"
	case "edu":
		return "Edu"
	case "free":
		return "Free"
	case "go":
		return "Go"
	}
	return planType
}

// codexAccountStatus：account/read 的結果 → 狀態、信箱、方案
func codexAccountStatus(res map[string]any) (status, email, plan string) {
	acc, _ := res["account"].(map[string]any)
	if acc == nil {
		return "notLoggedIn", "", ""
	}
	switch str(acc, "type") {
	case "chatgpt":
		return "ready", str(acc, "email"), codexPlanName(str(acc, "planType"))
	case "apiKey":
		return "apiKey", "", "API Key"
	}
	return "apiKey", "", str(acc, "type")
}

// usageLimitError：error 是訂閱額度用完（codexErrorInfo usageLimitExceeded；字串或物件形式）
func usageLimitError(v any) bool {
	e, _ := v.(map[string]any)
	switch info := e["codexErrorInfo"].(type) {
	case string:
		return info == "usageLimitExceeded"
	case map[string]any:
		_, ok := info["usageLimitExceeded"]
		return ok
	}
	return false
}

func markdownHTML(s string) string { return render.Markdown(s) }
