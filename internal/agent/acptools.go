package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// 這個檔把 ACP（Grok、Antigravity）的工具呼叫轉成工具卡：名稱與種類、一行摘要、細節、檔案、紅綠差異、輸出。
// 規則照 C# 版的 GrokAcpRules（DescribeTool、ParseDiff、ParseToolOutput、IsRejectedUpdate…）與 AntigravityAcpRules（ParsePermissionRequest）。

// acpKnownTools：工具卡的畫面名稱與種類（決定圖示）；先看 x.ai/tool 的名稱（grok），agy 的內建工具名稱也列在這裡
var acpKnownTools = map[string][2]string{
	"run_terminal_command": {"執行指令", "bash"}, "run_terminal_cmd": {"執行指令", "bash"}, "bash": {"執行指令", "bash"},
	"get_task_output": {"查看背景工作", "bash"}, "get_command_or_subagent_output": {"查看背景工作", "bash"},
	"kill_task": {"停止背景工作", "bash"}, "kill_command_or_subagent": {"停止背景工作", "bash"}, "monitor": {"監看輸出", "bash"},
	"read": {"讀取檔案", "read"}, "read_file": {"讀取檔案", "read"},
	"write": {"寫入檔案", "write"}, "edit": {"修改檔案", "edit"}, "search_replace": {"修改檔案", "edit"}, "hashline_edit": {"修改檔案", "edit"},
	"list_dir": {"列出資料夾", "search"}, "glob": {"尋找檔案", "search"}, "grep": {"搜尋內容", "search"}, "lsp": {"程式碼導覽", "search"},
	"web_search": {"網路搜尋", "web"}, "web_fetch": {"讀取網頁", "web"},
	"todo_write": {"待辦清單", "todo"}, "todowrite": {"待辦清單", "todo"},
	"task": {"子代理", "agent"}, "spawn_subagent": {"子代理", "agent"},
	"workflow": {"流程", "other"}, "scheduler_create": {"排程", "other"}, "scheduler_list": {"排程清單", "other"}, "scheduler_delete": {"取消排程", "other"},
	"skill":     {"技能", "other"},
	"image_gen": {"產生圖片", "image"}, "image_edit": {"修改圖片", "image"}, "image_to_video": {"圖片轉影片", "image"},
	"reference_to_video": {"參考生成影片", "image"}, "video_gen": {"產生影片", "image"},
	"enter_plan_mode": {"進入規劃模式", "plan"},
	// agy（Antigravity）的內建工具
	"run_command": {"執行指令", "bash"}, "command_status": {"查看背景工作", "bash"}, "send_command_input": {"輸入到指令", "bash"},
	"view_file": {"讀取檔案", "read"}, "view_file_outline": {"讀取檔案", "read"},
	"write_to_file": {"寫入檔案", "write"}, "replace_file_content": {"修改檔案", "edit"}, "multi_replace_file_content": {"修改檔案", "edit"},
	"grep_search": {"搜尋內容", "search"}, "find_by_name": {"尋找檔案", "search"}, "codebase_search": {"搜尋程式碼", "search"},
	"read_url_content": {"讀取網頁", "web"}, "search_web": {"網路搜尋", "web"}, "generate_image": {"產生圖片", "image"},
}

// acpKindNames：沒有工具名稱時依 ACP 的 kind 給畫面名稱（agy 的 title 是一整句說明，不適合當名稱）
var acpKindNames = map[string][2]string{
	"execute": {"執行指令", "bash"}, "read": {"讀取檔案", "read"}, "edit": {"修改檔案", "edit"}, "delete": {"刪除檔案", "edit"},
	"move": {"移動檔案", "edit"}, "search": {"搜尋", "search"}, "fetch": {"讀取網頁", "web"}, "think": {"思考", "other"},
}

// acpHiddenTool：不畫成工具卡的工具（提問與計畫確認由各自的卡片呈現）
func acpHiddenTool(name string) bool { return name == "ask_user_question" || name == "exit_plan_mode" }

func acpToolMeta(call map[string]any) map[string]any {
	meta, _ := call["_meta"].(map[string]any)
	tool, _ := meta["x.ai/tool"].(map[string]any)
	return tool
}

// acpToolName：x.ai/tool 的名稱（grok）；沒有就是 title
func acpToolName(call map[string]any) string {
	if n := str(acpToolMeta(call), "name"); n != "" {
		return n
	}
	return str(call, "title")
}

func firstKey(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

var (
	acpCommandKeys = []string{"command", "cmd", "CommandLine", "Command"}
	acpPathKeys    = []string{"file_path", "path", "target_file", "absolute_path", "AbsolutePath", "TargetFile", "FilePath", "Path", "DirectoryPath"}
	acpQueryKeys   = []string{"pattern", "query", "url", "glob", "search_term", "directory", "Query", "Pattern", "Url", "SearchPath", "SearchDirectory"}
	acpDescKeys    = []string{"description", "prompt", "task", "title", "Description"}
)

// describeAcpTool：工具呼叫（tool_call 與之後 tool_call_update 合併的內容）→ 工具卡
func describeAcpTool(call map[string]any) toolDesc {
	meta := acpToolMeta(call)
	metaName := str(meta, "name")
	acpTitle := str(call, "title")
	acpKind := strings.ToLower(str(call, "kind"))
	var name, kind string
	switch {
	case metaName != "" && acpKnownTools[strings.ToLower(metaName)][0] != "":
		k := acpKnownTools[strings.ToLower(metaName)]
		name, kind = k[0], k[1]
	case metaName == "" && acpKnownTools[strings.ToLower(acpTitle)][0] != "":
		k := acpKnownTools[strings.ToLower(acpTitle)]
		name, kind = k[0], k[1]
	default:
		name, kind = guessAcpTool(metaName, meta, acpKind, acpTitle)
	}
	raw, _ := call["rawInput"].(map[string]any)
	command := firstKey(raw, acpCommandKeys...)
	path := firstKey(raw, acpPathKeys...)
	if path == "" {
		if locs, ok := call["locations"].([]any); ok && len(locs) > 0 {
			lm, _ := locs[0].(map[string]any)
			path = str(lm, "path")
		}
	}
	query := firstKey(raw, acpQueryKeys...)
	description := firstKey(raw, acpDescKeys...)
	titleText := ""
	if acpTitle != "" && acpTitle != metaName {
		titleText = strings.ReplaceAll(acpTitle, "`", "")
	}
	search := ""
	if kind == "search" && query != "" {
		search = joinNonEmpty(query, path)
	}
	title := firstNonBlank(command, search, path, query, description, titleText, name)
	detail := ""
	switch {
	case command != "":
		detail = command
		if strings.TrimSpace(description) != "" {
			detail = description + "\n\n" + command
		}
	case kind == "read" || kind == "write" || kind == "edit":
	case len(raw) > 0:
		cp := map[string]any{}
		for k, v := range raw {
			if k != "variant" {
				cp[k] = v
			}
		}
		detail = truncate(indentJSON(cp), maxToolDetail)
	}
	d := toolDesc{Name: name, Kind: kind, Title: firstLine(title), Detail: detail}
	if kind == "read" || kind == "write" || kind == "edit" {
		d.File = path
	}
	d.Diff, d.Stats = acpDiff(call["content"])
	return d
}

// guessAcpTool：沒列在表上的工具：MCP、x.ai/tool 的 kind、ACP 的 kind
func guessAcpTool(toolName string, meta map[string]any, acpKind, acpTitle string) (string, string) {
	if strings.HasPrefix(strings.ToLower(toolName), "mcp__") || str(meta, "namespace") == "mcp" {
		return "MCP：" + toolName, "mcp"
	}
	k := strings.ToLower(firstNonBlank(str(meta, "kind"), acpKind))
	if toolName == "" {
		if n, ok := acpKindNames[k]; ok {
			return n[0], n[1]
		}
	}
	label := firstNonBlank(str(meta, "label"), toolName, acpTitle, "工具")
	kind := "other"
	switch k {
	case "execute", "background_task_action", "kill_task_action":
		kind = "bash"
	case "read":
		kind = "read"
	case "write":
		kind = "write"
	case "edit", "delete", "move":
		kind = "edit"
	case "search", "list_dir", "list", "lsp":
		kind = "search"
	case "web_search", "web_fetch", "fetch":
		kind = "web"
	case "task":
		kind = "agent"
	case "plan":
		kind = "todo"
	case "image_gen", "image_to_video", "reference_to_video", "video_gen":
		kind = "image"
	}
	return label, kind
}

// acpDiff：content 裡的 diff 區塊（{type:"diff", path, oldText, newText}）→ 行差異；oldText 空白＝新檔案
func acpDiff(v any) ([]DiffLine, *DiffStats) {
	arr, _ := v.([]any)
	var diffs []map[string]any
	for _, x := range arr {
		if m, ok := x.(map[string]any); ok && str(m, "type") == "diff" {
			diffs = append(diffs, m)
		}
	}
	if len(diffs) == 0 {
		return nil, nil
	}
	var all []DiffLine
	stats := &DiffStats{}
	for _, d := range diffs {
		var lines []DiffLine
		var st *DiffStats
		if str(d, "oldText") == "" {
			lines, st = diffNewFile(str(d, "newText"))
		} else {
			lines, st = diffLines(str(d, "oldText"), str(d, "newText"))
		}
		all = append(all, lines...)
		stats.Add += st.Add
		stats.Del += st.Del
	}
	return capDiff(all, stats)
}

func acpHasDiff(call map[string]any) bool {
	arr, _ := call["content"].([]any)
	for _, x := range arr {
		if m, ok := x.(map[string]any); ok && str(m, "type") == "diff" {
			return true
		}
	}
	return false
}

// acpToolOutput：content 裡 {type:"content", content:{type:"text", text}} 的文字；沒有就看 rawOutput（指令的 output_for_prompt、錯誤訊息）。
// 指令的 content 一定有（沒輸出時是空字串）：這時不退回 rawOutput（output_for_prompt 是給模型看的「exit: 0」）
func acpToolOutput(update map[string]any) string {
	arr, _ := update["content"].([]any)
	var texts []string
	hasContent := false
	for _, x := range arr {
		m, _ := x.(map[string]any)
		if str(m, "type") != "content" {
			continue
		}
		hasContent = true
		inner, _ := m["content"].(map[string]any)
		if t := str(inner, "text"); t != "" {
			texts = append(texts, t)
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" && !hasContent {
		if raw, ok := update["rawOutput"].(map[string]any); ok {
			text = firstKey(raw, "output_for_prompt", "error", "message")
			if text == "" {
				for _, v := range raw {
					if inner, ok := v.(map[string]any); ok {
						if t := str(inner, "tool_output_for_prompt"); t != "" {
							text = t
							break
						}
					}
				}
			}
		} else if s, ok := update["rawOutput"].(string); ok {
			text = s
		}
	}
	return strings.ReplaceAll(text, "\r\n", "\n")
}

// acpRejected：使用者拒絕而沒執行的工具（結束更新的 _meta.toolNotRun 是 rejected）
func acpRejected(update map[string]any) bool {
	meta, _ := update["_meta"].(map[string]any)
	return str(meta, "toolNotRun") == "rejected"
}

// acpExitCode：指令的結束碼（rawOutput.type 是 Bash 的 exit_code）；不是指令回 -1 以外的 ok=false
func acpExitCode(update map[string]any) (int, bool) {
	raw, _ := update["rawOutput"].(map[string]any)
	if str(raw, "type") != "Bash" {
		return 0, false
	}
	if f, ok := raw["exit_code"].(float64); ok {
		return int(f), true
	}
	return 0, false
}

// acpMerge：合併工具更新：後到的欄位蓋過先到的（null 不蓋）；_meta 逐鍵合併（內層物件再逐欄）；sessionUpdate 不留
func acpMerge(target, update map[string]any) {
	for k, v := range update {
		if k == "sessionUpdate" || v == nil {
			continue
		}
		if k == "_meta" {
			meta, ok1 := v.(map[string]any)
			existing, ok2 := target["_meta"].(map[string]any)
			if ok1 && ok2 {
				for mk, mv := range meta {
					inner, ok3 := mv.(map[string]any)
					old, ok4 := existing[mk].(map[string]any)
					if ok3 && ok4 {
						for ik, iv := range inner {
							if iv != nil {
								old[ik] = iv
							}
						}
					} else {
						existing[mk] = mv
					}
				}
				continue
			}
		}
		target[k] = v
	}
}

// acpPlanSteps：ACP plan 更新的 entries → 待辦清單（status 是 pending／in_progress／completed）
func acpPlanSteps(v any) []PlanStep {
	arr, _ := v.([]any)
	out := []PlanStep{}
	for _, x := range arr {
		m, _ := x.(map[string]any)
		text := str(m, "content")
		if strings.TrimSpace(text) == "" {
			continue
		}
		status := "pending"
		switch str(m, "status") {
		case "in_progress":
			status = "inProgress"
		case "completed":
			status = "completed"
		}
		out = append(out, PlanStep{Text: text, Status: status})
	}
	return out
}

// ---- 核准請求 ----

// acpPermission：session/request_permission 的內容
type acpPermission struct {
	toolCallID  string
	kind        string // ACP 的 kind（edit、execute…）
	allowOnce   string
	allowAlways string
	rejectOnce  string
	paths       []string
}

// parseAcpPermission：選項依 kind 對應；沒附 options 時用 ACP 的預設代碼（AntigravityAcpRules 的 DefaultAllowOnceOptionId…）
func parseAcpPermission(params map[string]any) acpPermission {
	call, _ := params["toolCall"].(map[string]any)
	p := acpPermission{toolCallID: str(call, "toolCallId"), kind: str(call, "kind"), paths: acpCollectPaths(call)}
	opts, hasOpts := params["options"].([]any)
	if !hasOpts {
		p.allowOnce, p.allowAlways, p.rejectOnce = "allow_once", "allow_always", "reject_once"
		return p
	}
	for _, o := range opts {
		om, _ := o.(map[string]any)
		id := str(om, "optionId")
		if id == "" {
			continue
		}
		switch str(om, "kind") {
		case "allow_once":
			if p.allowOnce == "" {
				p.allowOnce = id
			}
		case "allow_always":
			if p.allowAlways == "" {
				p.allowAlways = id
			}
		case "reject_once":
			if p.rejectOnce == "" {
				p.rejectOnce = id
			}
		}
	}
	return p
}

// outcome：核准卡的選擇 → 回覆（選項不存在時退回允許一次；都沒有就以 cancelled 回覆）
func (p acpPermission) outcome(choice string) map[string]any {
	opt := ""
	switch choice {
	case "allowSession":
		opt = firstNonBlank(p.allowAlways, p.allowOnce)
	case "allow":
		opt = p.allowOnce
	default:
		opt = p.rejectOnce
	}
	if opt == "" {
		return acpCancelledPermission()
	}
	return acpSelected(opt)
}

func acpSelected(optionID string) map[string]any {
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}
}

func acpCancelledPermission() map[string]any {
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
}

// acpCollectPaths：工具呼叫涉及的檔案：locations[].path、diff 內容的 path、rawInput 的路徑
func acpCollectPaths(call map[string]any) []string {
	var out []string
	add := func(p string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		for _, x := range out {
			if x == p {
				return
			}
		}
		out = append(out, p)
	}
	if locs, ok := call["locations"].([]any); ok {
		for _, l := range locs {
			lm, _ := l.(map[string]any)
			add(str(lm, "path"))
		}
	}
	if content, ok := call["content"].([]any); ok {
		for _, x := range content {
			m, _ := x.(map[string]any)
			if str(m, "type") == "diff" {
				add(str(m, "path"))
			}
		}
	}
	if raw, ok := call["rawInput"].(map[string]any); ok {
		for _, k := range []string{"file_path", "path", "target_file", "TargetFile", "AbsolutePath", "FilePath"} {
			add(str(raw, k))
		}
	}
	return out
}

var acpProtectedFolders = map[string]bool{".git": true, ".grok": true, ".claude": true, ".gemini": true}

// acpCanAutoApproveEdit：自動接受編輯（grok 沒有這個模式，由 app 代為核准）：kind 是 edit、至少一個路徑、
// 每個路徑都在工作資料夾或對話資料夾之內（完整路徑比對，擋掉 ../ 跳出；任何一層的 .git、.grok、.claude 不算；經過符號連結的不算）
func acpCanAutoApproveEdit(p acpPermission, workDir, convDir string) bool {
	if p.kind != "edit" || len(p.paths) == 0 || p.allowOnce == "" {
		return false
	}
	roots := []string{workDir, convDir}
	// 代理回報的可能是解開連結後的路徑（macOS 的 /var → /private/var）：工作資料夾本身解開連結後也算
	for _, r := range []string{workDir, convDir} {
		if real, err := filepath.EvalSymlinks(r); err == nil && real != r {
			roots = append(roots, real)
		}
	}
	for _, path := range p.paths {
		inside := false
		for _, r := range roots {
			if acpInside(path, r) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

func acpInside(path, root string) bool {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(root) == "" || !filepath.IsAbs(path) {
		return false
	}
	full := filepath.Clean(path)
	base := strings.TrimRight(filepath.Clean(root), string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(full, base) {
		return false
	}
	segments := strings.Split(full[len(base):], string(filepath.Separator))
	if len(segments) == 0 || (len(segments) == 1 && segments[0] == "") {
		return false
	}
	for _, s := range segments {
		if acpProtectedFolders[strings.ToLower(strings.TrimRight(s, ". "))] {
			return false
		}
	}
	// 工作資料夾之下的符號連結會讓寫入落到別處：一層層查已存在的部分（不存在的部分之後也不會存在）
	cur := strings.TrimRight(base, string(filepath.Separator))
	for _, s := range segments {
		cur = cur + string(filepath.Separator) + s
		fi, err := os.Lstat(cur)
		if err != nil {
			break
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}
