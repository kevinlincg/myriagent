package claude

import (
	"strings"
	"testing"
)

func TestArgsNewAndResume(t *testing.T) {
	a := strings.Join(Args(Options{SessionID: "s1", PermissionMode: "plan", Model: "opus", Effort: "high"}), " ")
	for _, want := range []string{"-p", "--input-format stream-json", "--output-format stream-json", "--permission-prompt-tool stdio",
		"--permission-mode plan", "--session-id s1", "--model opus", "--effort high", `"CLAUDE_CODE_EFFORT_LEVEL":"high"`, "--disallowedTools CronCreate,"} {
		if !strings.Contains(a, want) {
			t.Errorf("新對話的參數少了 %q：%s", want, a)
		}
	}
	r := strings.Join(Args(Options{SessionID: "s1", Resume: true}), " ")
	if !strings.Contains(r, "--resume s1") || strings.Contains(r, "--session-id") || strings.Contains(r, "--model") {
		t.Errorf("續接的參數不對：%s", r)
	}
}

func TestEnvDropsInheritedConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/somewhere/else")
	env := strings.Join(buildEnv(Options{}), "\n")
	if strings.Contains(env, "/somewhere/else") {
		t.Fatal("繼承來的 CLAUDE_CONFIG_DIR 沒有拿掉：所有帳戶都會變成同一個")
	}
	env = strings.Join(buildEnv(Options{ConfigDir: "/acct"}), "\n")
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR=/acct") {
		t.Fatal("隔離帳戶的設定資料夾沒有帶給 CLI")
	}
}
