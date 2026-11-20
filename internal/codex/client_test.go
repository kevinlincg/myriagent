package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildEnvDropsKeysAndSetsHome(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-秘密")
	t.Setenv("CODEX_HOME", "/inherited")
	env := strings.Join(BuildEnv("", nil), "\n")
	if strings.Contains(env, "sk-秘密") {
		t.Fatal("API Key 不能傳給 codex（會改走 API 計費）")
	}
	if !strings.Contains(env, "CODEX_HOME=/inherited") {
		t.Fatal("預設帳戶要保留使用者自己設的 CODEX_HOME")
	}
	env = strings.Join(BuildEnv("/acct", []string{"X=1"}), "\n")
	if strings.Contains(env, "/inherited") || !strings.Contains(env, "CODEX_HOME=/acct") || !strings.Contains(env, "X=1") {
		t.Fatalf("隔離帳戶要以自己的 CODEX_HOME 啟動：%s", env)
	}
}

func TestIDKey(t *testing.T) {
	for raw, want := range map[string]string{`5`: "5", `"abc"`: "abc", ` 12 `: "12"} {
		if got := IDKey(json.RawMessage(raw)); got != want {
			t.Errorf("IDKey(%s)=%q，要 %q", raw, got, want)
		}
	}
}

func TestServerArgsUseConfigFlags(t *testing.T) {
	a := strings.Join(ServerArgs(), " ")
	// --enable 遇到不認得的功能名稱會讓 app-server 結束；一律用 -c
	if !strings.HasPrefix(a, "app-server ") || strings.Contains(a, "--enable") || !strings.Contains(a, "-c features.default_mode_request_user_input=true") {
		t.Fatalf("app-server 參數不對：%s", a)
	}
}
