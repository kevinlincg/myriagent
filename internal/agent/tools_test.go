package agent

import (
	"testing"
	"time"
)

func TestDescribeToolBashAndEdit(t *testing.T) {
	d := describeTool("Bash", map[string]any{"command": "npm test\nnpm run lint", "description": "跑測試"})
	if d.Name != "執行指令" || d.Kind != "bash" || d.Title != "npm test …" || d.Detail != "跑測試\n\nnpm test\nnpm run lint" {
		t.Fatalf("Bash：%+v", d)
	}
	e := describeTool("Edit", map[string]any{"file_path": "/a.go", "old_string": "a\nb\nc", "new_string": "a\nB\nc"})
	if e.Kind != "edit" || e.File != "/a.go" || e.Stats.Add != 1 || e.Stats.Del != 1 {
		t.Fatalf("Edit：%+v", e)
	}
	kinds := []string{}
	for _, l := range e.Diff {
		kinds = append(kinds, l.T+":"+l.S)
	}
	want := []string{"hunk:@@ -1,3 +1,3 @@", "ctx:a", "del:b", "add:B", "ctx:c"}
	if len(kinds) != len(want) {
		t.Fatalf("差異 %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("差異第 %d 行 %q，要 %q", i, kinds[i], want[i])
		}
	}
	m := describeTool("mcp__github__create_issue", map[string]any{"title": "x"})
	if m.Name != "MCP：github" || m.Title != "create_issue" || m.Kind != "mcp" {
		t.Fatalf("MCP：%+v", m)
	}
}

func TestParseTodosUsesActiveForm(t *testing.T) {
	steps := parseTodos([]any{
		map[string]any{"content": "寫測試", "activeForm": "正在寫測試", "status": "in_progress"},
		map[string]any{"content": "上線", "status": "pending"},
		map[string]any{"content": "讀需求", "status": "completed"},
	})
	if len(steps) != 3 || steps[0].Text != "正在寫測試" || steps[0].Status != "inProgress" || steps[2].Status != "completed" {
		t.Fatalf("%+v", steps)
	}
}

func TestGroupLabel(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	cases := map[string]time.Time{
		"今天": now.Add(-time.Hour), "昨天": now.AddDate(0, 0, -1), "過去 7 天": now.AddDate(0, 0, -5),
		"過去 30 天": now.AddDate(0, 0, -20), "8 月": now.AddDate(0, -3, 0), "2025 年 11 月": now.AddDate(-1, 0, 0),
	}
	for want, at := range cases {
		if got := groupLabel(at, now); got != want {
			t.Errorf("%v：%q，要 %q", at, got, want)
		}
	}
}
