package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func credJSON(token string, expiresAt any) string {
	tok := "null"
	if token != "<none>" {
		tok = fmt.Sprintf("%q", token)
	}
	exp := ""
	if expiresAt != nil {
		exp = fmt.Sprintf(`,"expiresAt":%v`, expiresAt)
	}
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%s%s,"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`, tok, exp)
}

func TestInspectClaudeToken(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	fresh := now.Add(time.Hour).UnixMilli()
	soon := now.Add(9 * time.Minute).UnixMilli()
	cases := []struct {
		name, json, want string
	}{
		{"沒有檔案", "", tokenNone},
		{"沒有 claudeAiOauth", `{"x":1}`, tokenNone},
		{"accessToken 是 null", credJSON("<none>", fresh), tokenNone},
		{"空權杖", credJSON("", fresh), tokenEmpty},
		{"沒有 expiresAt", credJSON("t", nil), tokenMissingExpiry},
		{"新鮮", credJSON("t", fresh), tokenFresh},
		{"10 分鐘內到期", credJSON("t", soon), tokenExpiring},
		{"讀不懂", `{"claudeAiOauth":`, tokenUnreadable},
	}
	for _, c := range cases {
		if got := inspectClaudeToken(c.json, now); got != c.want {
			t.Errorf("%s：%s，應該是 %s", c.name, got, c.want)
		}
	}
}

// 決定要不要跑 claude auth status：權杖快到期時絕不能跑（會把 refresh token 用掉、之後被登出）
func TestDecideClaudeStatusCheck(t *testing.T) {
	now := time.Now()
	fresh := credJSON("t", now.Add(time.Hour).UnixMilli())
	expiring := credJSON("t", now.Add(5*time.Minute).UnixMilli())
	cred := func(j string) claudeCredentials {
		return claudeCredentials{exists: true, size: int64(len(j)), modTime: 42, json: j}
	}
	cache := &identityCache{Status: "ready", Email: "a@b.c", CredSize: int64(len(fresh)), CredMod: 42, ExpiresAt: readClaudeExpiresAt(fresh)}
	readable := &claudeGlobalConfig{readable: true}
	credman := &claudeGlobalConfig{readable: true, credMan: true, hasOAuth: true}
	cases := []struct {
		name   string
		cred   claudeCredentials
		cache  *identityCache
		reason string
		global *claudeGlobalConfig
		want   string
	}{
		{"沒有憑證、設定讀得到", claudeCredentials{}, nil, reasonStartup, readable, decideRunAuthStatus},
		{"沒有憑證、設定讀不到", claudeCredentials{}, nil, reasonStartup, &claudeGlobalConfig{}, decideConfigUnread},
		{"沒有憑證、認證管理員", claudeCredentials{}, nil, reasonManual, credman, decideConservative},
		{"憑證被占用", claudeCredentials{exists: true}, nil, reasonManual, nil, decideCredUnreadable},
		{"空權杖", cred(credJSON("", now.Add(time.Hour).UnixMilli())), nil, reasonManual, nil, decideLoginExpired},
		{"快到期：手動也不跑", cred(expiring), nil, reasonManual, nil, decideTokenPending},
		{"快到期：憑證變了也不跑", cred(expiring), nil, reasonCredentials, nil, decideTokenPending},
		{"沒有 expiresAt", cred(credJSON("t", nil)), nil, reasonManual, nil, decideTokenPending},
		{"API Key（沒有 OAuth）", cred(`{"x":1}`), nil, reasonStartup, nil, decideRunAuthStatus},
		{"新鮮＋快取有效＋啟動", cred(fresh), cache, reasonStartup, nil, decideUseCache},
		{"新鮮＋快取有效＋定期", cred(fresh), cache, reasonPeriodic, nil, decideUseCache},
		{"新鮮＋快取有效＋手動", cred(fresh), cache, reasonManual, nil, decideRunAuthStatus},
		{"新鮮＋快取有效＋憑證變了", cred(fresh), cache, reasonCredentials, nil, decideRunAuthStatus},
		{"新鮮＋快取不符", claudeCredentials{exists: true, size: 1, modTime: 42, json: fresh}, cache, reasonStartup, nil, decideRunAuthStatus},
	}
	for _, c := range cases {
		if got := decideClaudeStatusCheck(c.cred, c.cache, c.reason, now, c.global); got != c.want {
			t.Errorf("%s：%s，應該是 %s", c.name, got, c.want)
		}
	}
}

func TestEvaluateClaudeAuthStatus(t *testing.T) {
	st, email, plan := evaluateClaudeAuthStatus("警告 {不是 json}\n" + `{"loggedIn":true,"authMethod":"claude.ai","email":"me@x.com","subscriptionType":"max"}` + "\n尾巴")
	if st != "ready" || email != "me@x.com" || plan != "Max" {
		t.Fatalf("%s %s %s", st, email, plan)
	}
	if st, _, plan := evaluateClaudeAuthStatus(`{"loggedIn":true,"authMethod":"console"}`); st != "apiKey" || plan != "console" {
		t.Fatalf("API 計費：%s %s", st, plan)
	}
	if st, _, _ := evaluateClaudeAuthStatus(`{"loggedIn":false}`); st != "notLoggedIn" {
		t.Fatal(st)
	}
	// 讀不到結果是檢查失敗，不能當成未登入
	if st, _, _ := evaluateClaudeAuthStatus("timeout"); st != "error" {
		t.Fatal(st)
	}
}

func TestDescribeClaudePlan(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"max", "default_claude_max_20x"}: "Max 20x",
		{"Claude Pro", ""}:                "Pro",
		{"team", ""}:                      "Team",
		{"", "x"}:                         "",
		{"ultra", ""}:                     "Ultra",
	} {
		if got := describeClaudePlan(in[0], in[1]); got != want {
			t.Errorf("%v：%q，應該是 %q", in, got, want)
		}
	}
}

func TestNextPollDelayBackoff(t *testing.T) {
	normal := 10 * time.Minute
	if d := nextPollDelay("ready", 5, normal); d != normal {
		t.Fatal(d)
	}
	want := []time.Duration{time.Minute, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if d := nextPollDelay("error", i, normal); d != w {
			t.Errorf("連續 %d 次失敗：%v，應該是 %v", i, d, w)
		}
	}
}

func TestApplyAccountEnv(t *testing.T) {
	base := map[string]string{
		"PATH": "/bin", "ANTHROPIC_API_KEY": "k", "OPENAI_API_KEY": "k", "CLAUDECODE": "1", "CLAUDE_CODE_ENTRYPOINT": "x",
		"CLAUDE_CONFIG_DIR": "/mine", "ANTIGRAVITY_CONV": "c", "XAI_API_KEY": "x", "GEMINI_API_KEY": "g", "MYRIAGENT_AGENT_CONVERSATION": "c1",
	}
	clone := func() map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	// 系統預設的 Claude：保留使用者自己設的 CLAUDE_CONFIG_DIR，剝掉 API Key 與父 session 的變數
	env := clone()
	applyAccountEnv(&Account{Provider: "Claude", IsSystemDefault: true}, env)
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "ANTIGRAVITY_CONV", "MYRIAGENT_AGENT_CONVERSATION"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s 應該被剝掉", k)
		}
	}
	if env["CLAUDE_CONFIG_DIR"] != "/mine" || env["PATH"] != "/bin" {
		t.Fatalf("預設帳戶的環境不對：%v", env)
	}
	// 隔離的 Codex：CODEX_HOME 指向自己的資料夾
	env = clone()
	applyAccountEnv(&Account{Provider: "Codex", ConfigDir: "/data/accounts/x/codex"}, env)
	if env["CODEX_HOME"] != "/data/accounts/x/codex" {
		t.Fatal(env["CODEX_HOME"])
	}
	// Grok：剝掉 xAI API Key、關掉自動更新、GROK_HOME
	env = clone()
	applyAccountEnv(&Account{Provider: "Grok", ConfigDir: "/g"}, env)
	if _, ok := env["XAI_API_KEY"]; ok || env["GROK_DISABLE_AUTOUPDATER"] != "1" || env["GROK_HOME"] != "/g" {
		t.Fatalf("Grok 的環境不對：%v", env)
	}
	// Antigravity 隔離帳戶：HOME 換成它的家、剝掉 Gemini API Key、關掉自動更新
	env = clone()
	applyAccountEnv(&Account{Provider: "Antigravity", ConfigDir: "/a/agy-home"}, env)
	if env["HOME"] != "/a/agy-home" || env["AGY_CLI_DISABLE_AUTO_UPDATE"] != "true" || env["ZDOTDIR"] != "/a/agy-home/.myriagent-shell" {
		t.Fatalf("agy 的環境不對：%v", env)
	}
	if _, ok := env["GEMINI_API_KEY"]; ok {
		t.Fatal("GEMINI_API_KEY 應該被剝掉")
	}
	// 短命的子指令另帶 DISABLE_AUTOUPDATER
	env = clone()
	applyCommandEnv(nil, env)
	if env["DISABLE_AUTOUPDATER"] != "1" || env["CLAUDE_CONFIG_DIR"] != "/mine" {
		t.Fatal(env)
	}
}

// 移除帳戶時只刪 app 自己建的 accounts/{id}：系統預設、使用者原有的資料夾、位置不符一律不刪
func TestDeletableAccountRoot(t *testing.T) {
	dir := "/data/accounts"
	id := strings.Repeat("ab", 16)
	ok := &Account{ID: id, Provider: "Claude", ConfigDir: filepath.Join(dir, id, "claude")}
	if got := deletableAccountRoot(ok, dir); got != filepath.Join(dir, id) {
		t.Fatalf("app 建的資料夾：%q", got)
	}
	cases := map[string]*Account{
		"系統預設":      {ID: "local-claude", Provider: "Claude", IsSystemDefault: true},
		"現有資料夾":     {ID: id, Provider: "Claude", ConfigDir: filepath.Join(dir, id, "claude"), ExistingFolder: true},
		"不在帳戶底下":    {ID: id, Provider: "Claude", ConfigDir: "/Users/u/.claude-work"},
		"名稱不是 Id":   {ID: id, Provider: "Claude", ConfigDir: filepath.Join(dir, "other", "claude")},
		"Id 不是十六進位": {ID: "local-claude", Provider: "Claude", ConfigDir: filepath.Join(dir, "local-claude", "claude")},
		"使用者資料夾":    {ID: id, Provider: "Claude", ConfigDir: "/Users/u"},
	}
	for name, a := range cases {
		if got := deletableAccountRoot(a, dir); got != "" {
			t.Errorf("%s 不能刪，卻回 %q", name, got)
		}
	}
}

func TestDefaultLabelAndColor(t *testing.T) {
	existing := []*Account{{Label: "Claude 帳戶", Color: "#D97757"}, {Label: "claude 帳戶 2", Color: "#E5A04B"}}
	if l := defaultAccountLabel("Claude", existing); l != "Claude 帳戶 3" {
		t.Fatal(l)
	}
	if c := defaultAccountColor("Claude", existing); c != "#E06C75" {
		t.Fatal(c)
	}
	if normalizeColor(" #abcdef ") != "#ABCDEF" || normalizeColor("red") != "" || normalizeColor("#abc") != "" {
		t.Fatal("normalizeColor")
	}
}

func TestExistingFolderRules(t *testing.T) {
	home := "/Users/u"
	if got := cleanFolderInput(` "~/.claude-work/" `, home); got != "/Users/u/.claude-work" {
		t.Fatal(got)
	}
	if got := cleanFolderInput("~//x/../y", home); got != "/Users/u/y" {
		t.Fatal(got)
	}
	exists := func(string) bool { return true }
	yes := func(string) bool { return true }
	defaults := []string{"/Users/u/.claude"}
	cases := []struct {
		p, dir string
		used   []string
		looks  func(string) bool
		want   string
	}{
		{"Claude", "/Users/u/.claude-work", nil, yes, folderOK},
		{"Antigravity", "/Users/u/.gemini", nil, yes, folderUnsupported},
		{"Claude", "", nil, yes, folderEmpty},
		{"Claude", "rel/path", nil, yes, folderNotAbsolute},
		{"Claude", "/Users/u/.claude/", nil, yes, folderIsDefault},
		{"Claude", "/Users/u/.CLAUDE-WORK", []string{"/Users/u/.claude-work"}, yes, folderInUse},
		{"Claude", "/", nil, yes, folderForbidden},
		{"Claude", "/Users/u", nil, yes, folderForbidden},
		{"Claude", "/Users/u/Documents", nil, func(string) bool { return false }, folderNotConfigFolder},
	}
	for _, c := range cases {
		got := checkExistingFolder(c.p, c.dir, exists, defaults, c.used, []string{home}, c.looks)
		if got != c.want {
			t.Errorf("%s %q：%q，應該是 %q", c.p, c.dir, got, c.want)
		}
	}
	if checkExistingFolder("Claude", "/nope", func(string) bool { return false }, nil, nil, nil, yes) != folderMissing {
		t.Fatal("不存在")
	}
	for name, want := range map[string]bool{".claude": true, ".claude-work": true, ".claude_alt": true, ".claudex": false, "claude": false} {
		if isCandidateFolderName("Claude", name) != want {
			t.Errorf("%s 應該是 %v", name, want)
		}
	}
	marker := map[string]bool{"/Users/u/.claude-work/projects": true, "/Users/u/.codex-2/auth.json": true}
	found := findExistingFolders("Claude", []string{"/Users/u/.claude", "/Users/u/.claude-work", "/Users/u/.claude-empty", "/Users/u/.codex-2"},
		func(p string) bool { return marker[p] }, defaults, nil)
	if len(found) != 1 || found[0] != "/Users/u/.claude-work" {
		t.Fatalf("找到的資料夾不對：%v", found)
	}
	if l := existingFolderLabel("Claude", "/Users/u/.claude-work", nil); l != "Claude work" {
		t.Fatal(l)
	}
	if l := existingFolderLabel("Claude", "/Users/u/.claude-work", []*Account{{Label: "Claude work"}}); l != "Claude 帳戶" {
		t.Fatal(l)
	}
	if d := displayFolder("/Users/u/.claude-work", home); d != "~/.claude-work" {
		t.Fatal(d)
	}
}

func TestClaudeKeychainService(t *testing.T) {
	if s := claudeKeychainService("/Users/u/.claude", "", "/Users/u"); s != "Claude Code-credentials" {
		t.Fatal(s)
	}
	// 指定了設定資料夾：加上路徑 SHA-256 的前 8 碼（echo -n /Users/u/.claude-work | shasum -a 256）
	s := claudeKeychainService("/Users/u/.claude-work", "", "/Users/u")
	if !strings.HasPrefix(s, "Claude Code-credentials-") || len(s) != len("Claude Code-credentials-")+8 {
		t.Fatal(s)
	}
	// 預設資料夾但有設 CLAUDE_CONFIG_DIR：也加後綴
	if s := claudeKeychainService("/Users/u/.claude", "/Users/u/.claude", "/Users/u"); s == "Claude Code-credentials" {
		t.Fatal(s)
	}
}

// 真的整理一個 agy 的家：連結、小腳本；刪除帳戶資料夾前拿掉連結，絕不刪到真正的家
func TestAntigravityHomeOnDisk(t *testing.T) {
	real := t.TempDir()
	_ = os.MkdirAll(filepath.Join(real, ".gemini", "antigravity-cli"), 0o700)
	_ = os.WriteFile(filepath.Join(real, ".gitconfig"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(real, ".gemini", "antigravity-cli", "GEMINI.md"), []byte("指示"), 0o600)
	t.Setenv("HOME", real)
	oldKC := agyKeychain
	agyKeychain = func(string) error { return nil }
	t.Cleanup(func() { agyKeychain = oldKC })

	root := filepath.Join(t.TempDir(), strings.Repeat("c", 32))
	home := filepath.Join(root, agyHomeFolder)
	_ = os.MkdirAll(home, 0o700)
	prepareAgyHome(home)
	if target, _ := os.Readlink(filepath.Join(home, ".gitconfig")); target != filepath.Join(real, ".gitconfig") {
		t.Fatalf(".gitconfig 的連結：%q", target)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "GEMINI.md")); err != nil || string(b) != "指示" {
		t.Fatal("GEMINI.md 沒有連回去")
	}
	if b, _ := os.ReadFile(filepath.Join(home, agyShellFolder, ".zshenv")); !strings.Contains(string(b), "export HOME='"+real+"'") {
		t.Fatal(".zshenv 沒有換回真正的家")
	}
	if !deleteAccountFolder(root) {
		t.Fatal("刪不掉")
	}
	if _, err := os.Stat(filepath.Join(real, ".gitconfig")); err != nil {
		t.Fatal("真正的家的檔案被刪掉了")
	}
	if _, err := os.Stat(filepath.Join(real, ".gemini", "antigravity-cli", "GEMINI.md")); err != nil {
		t.Fatal("真正的家的 GEMINI.md 被刪掉了")
	}
}
