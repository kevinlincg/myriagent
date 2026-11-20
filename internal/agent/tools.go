package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// toolDesc：工具卡要顯示的東西（照 C# 版 ClaudeStreamRules.DescribeTool）
type toolDesc struct {
	Name, Kind, Title, Detail, File string
	Diff                            []DiffLine
	Stats                           *DiffStats
}

const maxToolDetail = 4000

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func describeTool(name string, input map[string]any) toolDesc {
	if input == nil {
		input = map[string]any{}
	}
	switch name {
	case "Bash", "PowerShell":
		cmd := str(input, "command")
		detail := cmd
		if d := str(input, "description"); strings.TrimSpace(d) != "" {
			detail = d + "\n\n" + cmd
		}
		label := "執行指令"
		if name == "PowerShell" {
			label = "執行 PowerShell"
		}
		return toolDesc{Name: label, Kind: "bash", Title: firstLine(cmd), Detail: detail}
	case "Read":
		p := str(input, "file_path")
		kind := "read"
		if isMedia(p) {
			kind = "image"
		}
		return toolDesc{Name: "讀取檔案", Kind: kind, Title: p, File: p}
	case "Write":
		p := str(input, "file_path")
		d, st := diffNewFile(str(input, "content"))
		return toolDesc{Name: "寫入檔案", Kind: "write", Title: p, File: p, Diff: d, Stats: st}
	case "Edit":
		p := str(input, "file_path")
		d, st := diffLines(str(input, "old_string"), str(input, "new_string"))
		return toolDesc{Name: "修改檔案", Kind: "edit", Title: p, File: p, Diff: d, Stats: st}
	case "MultiEdit":
		p := str(input, "file_path")
		var all []DiffLine
		stats := &DiffStats{}
		if edits, ok := input["edits"].([]any); ok {
			for _, e := range edits {
				em, _ := e.(map[string]any)
				d, st := diffLines(str(em, "old_string"), str(em, "new_string"))
				all = append(all, d...)
				stats.Add += st.Add
				stats.Del += st.Del
			}
		}
		return toolDesc{Name: "修改檔案", Kind: "edit", Title: p, File: p, Diff: all, Stats: stats}
	case "NotebookEdit":
		p := str(input, "notebook_path")
		d, st := diffNewFile(str(input, "new_source"))
		return toolDesc{Name: "修改筆記本", Kind: "edit", Title: p, File: p, Diff: d, Stats: st}
	case "Glob":
		return toolDesc{Name: "搜尋檔案", Kind: "search", Title: joinNonEmpty(str(input, "pattern"), str(input, "path"))}
	case "Grep":
		where := str(input, "path")
		if where == "" {
			where = str(input, "glob")
		}
		return toolDesc{Name: "搜尋內容", Kind: "search", Title: joinNonEmpty(str(input, "pattern"), where)}
	case "WebFetch":
		return toolDesc{Name: "讀取網頁", Kind: "web", Title: str(input, "url"), Detail: str(input, "prompt")}
	case "WebSearch":
		return toolDesc{Name: "網路搜尋", Kind: "web", Title: str(input, "query")}
	case "Task", "Agent":
		t := str(input, "description")
		if t == "" {
			t = "子代理工作"
		}
		return toolDesc{Name: "子代理", Kind: "agent", Title: t, Detail: truncate(str(input, "prompt"), maxToolDetail)}
	case "TodoWrite":
		return toolDesc{Name: "更新待辦", Kind: "todo", Title: describeTodos(input["todos"])}
	case "TaskCreate":
		return toolDesc{Name: "新增待辦", Kind: "todo", Title: firstNonBlank(str(input, "subject"), str(input, "title"), str(input, "name")), Detail: str(input, "description")}
	case "TaskUpdate":
		return toolDesc{Name: "更新待辦", Kind: "todo", Title: joinNonEmpty(str(input, "subject"), str(input, "status"))}
	case "TaskGet":
		return toolDesc{Name: "查看待辦", Kind: "todo", Title: firstNonBlank(str(input, "taskId"), str(input, "id"))}
	case "TaskList":
		return toolDesc{Name: "列出待辦", Kind: "todo"}
	case "Skill":
		return toolDesc{Name: "使用技能", Kind: "other", Title: firstNonBlank(str(input, "skill"), str(input, "command")), Detail: str(input, "args")}
	case "ToolSearch":
		return toolDesc{Name: "載入工具", Kind: "other", Title: str(input, "query")}
	case "ExitPlanMode":
		return toolDesc{Name: "確認執行計畫", Kind: "plan", Title: "代理已規劃好做法，請確認是否照計畫執行", Detail: str(input, "plan")}
	}
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.SplitN(name, "__", 3)
		server, tool := name, name
		if len(parts) > 1 {
			server = parts[1]
		}
		if len(parts) > 2 {
			tool = parts[2]
		}
		return toolDesc{Name: "MCP：" + server, Kind: "mcp", Title: tool, Detail: truncate(indentJSON(input), maxToolDetail)}
	}
	first := ""
	for _, v := range input {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			first = s
			break
		}
	}
	detail := ""
	if len(input) > 0 {
		detail = truncate(indentJSON(input), maxToolDetail)
	}
	return toolDesc{Name: name, Kind: "other", Title: firstLine(first), Detail: detail}
}

// parseTodos：TodoWrite 的 todos → 待辦清單（in_progress 以 activeForm 顯示）
func parseTodos(v any) []PlanStep {
	arr, _ := v.([]any)
	out := []PlanStep{}
	for _, t := range arr {
		m, _ := t.(map[string]any)
		status := "pending"
		switch str(m, "status") {
		case "completed":
			status = "completed"
		case "in_progress":
			status = "inProgress"
		}
		text := str(m, "content")
		if status == "inProgress" && strings.TrimSpace(str(m, "activeForm")) != "" {
			text = str(m, "activeForm")
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, PlanStep{Text: text, Status: status})
	}
	return out
}

func describeTodos(v any) string {
	steps := parseTodos(v)
	done := 0
	for _, s := range steps {
		if s.Status == "completed" {
			done++
		}
	}
	if len(steps) == 0 {
		return ""
	}
	return fmt.Sprintf("%d／%d 完成", done, len(steps))
}

// parseQuestions：AskUserQuestion 的輸入 → 問卷（題目 id 就是題目文字，作答時以它當鍵）
func parseQuestions(input map[string]any) []Question {
	arr, _ := input["questions"].([]any)
	out := []Question{}
	for _, t := range arr {
		q, _ := t.(map[string]any)
		text := str(q, "question")
		if text == "" {
			continue
		}
		multi, _ := q["multiSelect"].(bool)
		qq := Question{ID: text, Header: str(q, "header"), Text: text, Multi: multi, AllowOther: true, Options: []QuestionOption{}}
		opts, _ := q["options"].([]any)
		for _, o := range opts {
			om, _ := o.(map[string]any)
			label := str(om, "label")
			if strings.TrimSpace(label) == "" {
				continue
			}
			var preview *string
			if p := str(om, "preview"); p != "" {
				preview = &p
			}
			qq.Options = append(qq.Options, QuestionOption{Label: label, Description: str(om, "description"), Preview: preview})
		}
		out = append(out, qq)
	}
	return out
}

// ---- 差異 ----

const maxDiffLines = 800

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func diffNewFile(content string) ([]DiffLine, *DiffStats) {
	lines := splitLines(content)
	out := []DiffLine{{T: "hunk", S: fmt.Sprintf("@@ -0,0 +1,%d @@", len(lines))}}
	for _, l := range lines {
		out = append(out, DiffLine{T: "add", S: l})
	}
	return capDiff(out, &DiffStats{Add: len(lines)})
}

// diffLines：以最長共同子序列算逐行差異（太大時退回整段刪除＋整段新增）
func diffLines(oldS, newS string) ([]DiffLine, *DiffStats) {
	a, b := splitLines(oldS), splitLines(newS)
	stats := &DiffStats{}
	var body []DiffLine
	if len(a)*len(b) > 4_000_000 {
		for _, l := range a {
			body = append(body, DiffLine{T: "del", S: l})
		}
		for _, l := range b {
			body = append(body, DiffLine{T: "add", S: l})
		}
		stats.Del, stats.Add = len(a), len(b)
	} else {
		n, m := len(a), len(b)
		lcs := make([][]int32, n+1)
		for i := range lcs {
			lcs[i] = make([]int32, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if a[i] == b[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else if lcs[i+1][j] >= lcs[i][j+1] {
					lcs[i][j] = lcs[i+1][j]
				} else {
					lcs[i][j] = lcs[i][j+1]
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && a[i] == b[j]:
				body = append(body, DiffLine{T: "ctx", S: a[i]})
				i++
				j++
			case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
				body = append(body, DiffLine{T: "add", S: b[j]})
				stats.Add++
				j++
			default:
				body = append(body, DiffLine{T: "del", S: a[i]})
				stats.Del++
				i++
			}
		}
	}
	out := append([]DiffLine{{T: "hunk", S: fmt.Sprintf("@@ -1,%d +1,%d @@", len(a), len(b))}}, body...)
	return capDiff(out, stats)
}

func capDiff(lines []DiffLine, st *DiffStats) ([]DiffLine, *DiffStats) {
	if len(lines) <= maxDiffLines {
		return lines, st
	}
	rest := len(lines) - maxDiffLines
	lines = append(lines[:maxDiffLines:maxDiffLines], DiffLine{T: "hunk", S: fmt.Sprintf("⋯ 還有 %d 行未顯示（+%d −%d）", rest, st.Add, st.Del)})
	return lines, st
}

// ---- 小工具 ----

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func joinNonEmpty(xs ...string) string {
	var out []string
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			out = append(out, x)
		}
	}
	return strings.Join(out, "  ")
}

func firstNonBlank(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

func indentJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

var mediaExt = map[string]string{".png": "image", ".jpg": "image", ".jpeg": "image", ".gif": "image", ".webp": "image", ".bmp": "image", ".svg": "image", ".mp4": "video", ".mov": "video", ".webm": "video", ".m4v": "video"}

func isMedia(p string) bool { _, ok := mediaExt[strings.ToLower(filepath.Ext(p))]; return ok }
