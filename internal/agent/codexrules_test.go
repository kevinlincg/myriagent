package agent

import (
	"strings"
	"testing"
)

func TestCodexPolicyForModes(t *testing.T) {
	cases := map[string]codexPolicy{
		"Ask": {"untrusted", "workspace-write"}, "": {"untrusted", "workspace-write"}, "Plan": {"untrusted", "workspace-write"},
		"Auto": {"on-request", "workspace-write"}, "AcceptEdits": {"on-request", "workspace-write"},
		"FullAccess": {"never", "danger-full-access"},
	}
	for mode, want := range cases {
		if got := codexPolicyFor(mode); got != want {
			t.Errorf("%q → %+v，要 %+v", mode, got, want)
		}
	}
}

func TestCodexDecision(t *testing.T) {
	onlyCancel := []any{"accept", "acceptWithExecpolicyAmendment", "cancel"}
	cases := []struct {
		avail  []any
		choice string
		want   string
	}{
		{nil, "allow", "accept"},
		{nil, "allowSession", "acceptForSession"},
		{nil, "deny", "decline"},
		{onlyCancel, "allowSession", "accept"}, // 沒有 acceptForSession：宿主自己記住
		{onlyCancel, "deny", "cancel"},
		{[]any{"accept", "acceptForSession", "decline"}, "deny", "decline"},
	}
	for _, c := range cases {
		if got := codexDecision(c.avail, c.choice); got != c.want {
			t.Errorf("%v %s → %s，要 %s", c.avail, c.choice, got, c.want)
		}
	}
}

func TestCodexSessionApprovalKeyIsExact(t *testing.T) {
	m := "item/commandExecution/requestApproval"
	a := codexSessionApprovalKey(m, map[string]any{"command": "npm test", "cwd": "/a"})
	if a == "" || a != codexSessionApprovalKey(m, map[string]any{"command": "npm test", "cwd": "/a"}) {
		t.Fatal("同樣的指令要得到同樣的鍵")
	}
	for _, other := range []map[string]any{
		{"command": "npm test", "cwd": "/b"},
		{"command": "npm test -- --watch", "cwd": "/a"},
		{"command": "npm test", "cwd": "/a", "additionalPermissions": map[string]any{"network": map[string]any{"enabled": true}}},
	} {
		if codexSessionApprovalKey(m, other) == a {
			t.Errorf("不同的請求不能共用「本對話都允許」：%v", other)
		}
	}
	if codexSessionApprovalKey(m, map[string]any{}) != "" {
		t.Error("沒有指令就不記")
	}
	net := codexSessionApprovalKey(m, map[string]any{"networkApprovalContext": map[string]any{"host": "example.com", "protocol": "https"}})
	if !strings.Contains(net, "example.com") || strings.HasPrefix(net, "command") {
		t.Errorf("網路存取以主機為鍵：%q", net)
	}
}

func TestDiffFromUnified(t *testing.T) {
	d := diffFromUnified("--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n ctx\n--- 註解\n+++i;\n\\ No newline at end of file\n", "update")
	want := []DiffLine{{"hunk", "@@ -1,2 +1,2 @@"}, {"ctx", "ctx"}, {"del", "-- 註解"}, {"add", "++i;"}}
	if len(d) != len(want) {
		t.Fatalf("行數不對：%v", d)
	}
	for i := range want {
		if d[i] != want[i] {
			t.Errorf("第 %d 行 %v，要 %v", i, d[i], want[i])
		}
	}
	if a := diffFromUnified("新檔\n第二行\n", "add"); len(a) != 2 || a[0].T != "add" {
		t.Errorf("新增的檔案整段是新增：%v", a)
	}
}

func TestCodexDescribeCommand(t *testing.T) {
	if got := codexDescribeCommand(map[string]any{"command": `/bin/zsh -lc 'echo '\''hi'\'''`}); got != "echo 'hi'" {
		t.Errorf("剝掉 shell 包裝：%q", got)
	}
	got := codexDescribeCommand(map[string]any{"command": "x", "commandActions": []any{map[string]any{"command": "ls"}, map[string]any{"command": "cat a"}}})
	if got != "ls && cat a" {
		t.Errorf("有 commandActions 用它：%q", got)
	}
}

func TestCodexDenyFeedbackMessage(t *testing.T) {
	if got := denyFeedbackMessage([]deniedOperation{{"執行指令", "rm -rf", "不要刪"}}); got != "（我剛才拒絕了那個操作）不要刪" {
		t.Errorf("一則：%q", got)
	}
	got := denyFeedbackMessage([]deniedOperation{{"執行指令", "rm -rf", "不要刪"}, {"修改檔案", "a.txt", "別改"}})
	if !strings.HasPrefix(got, "（我剛才拒絕了這幾個操作）\n1. 「執行指令：rm -rf」：不要刪\n2. 「修改檔案：a.txt」：別改") {
		t.Errorf("多則：%q", got)
	}
}

func TestCodexDoctorReportRejectsUnknownSchema(t *testing.T) {
	if _, err := codexDoctorReport([]byte("not json")); err == nil {
		t.Error("不是 JSON 不能當成報告")
	}
	if _, err := codexDoctorReport([]byte(`{"schemaVersion":2,"checks":{}}`)); err == nil {
		t.Error("不認得的 schemaVersion 要回錯誤")
	}
}
