package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// acctEnv：帳戶管理的測試環境：暫存的 HOME 與資料夾、四種 CLI 都是假的（acctfake_test.go）、Claude 用量端點是本機的假伺服器、
// 開瀏覽器只記下網址、終端機改成直接在背景執行
type acctEnv struct {
	t        *testing.T
	h        *Hub
	tv       *testView
	home     string
	specPath string
	logPath  string
	srv      *httptest.Server
	mu       sync.Mutex
	usageReq []*http.Request
	usageRes string
	browsed  []string
}

func (e *acctEnv) spec(s fakeSpec) {
	e.t.Helper()
	s.Log = e.logPath
	b, _ := json.Marshal(s)
	if err := os.WriteFile(e.specPath, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// calls：假 CLI 被呼叫過的參數（每行：參數\t環境）
func (e *acctEnv) calls() []string {
	b, _ := os.ReadFile(e.logPath)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (e *acctEnv) called(prefix string) []string {
	var out []string
	for _, l := range e.calls() {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// withProviders：讓這幾種代理在測試期間算「接上了」（有本機預設帳戶、可以新增帳戶）
func withProviders(t *testing.T, ps ...string) {
	old := map[string]bool{}
	for k, v := range implementedProviders {
		old[k] = v
	}
	for _, p := range ps {
		implementedProviders[p] = true
	}
	t.Cleanup(func() {
		for k := range implementedProviders {
			delete(implementedProviders, k)
		}
		for k, v := range old {
			implementedProviders[k] = v
		}
	})
}

func newAcctEnv(t *testing.T, s fakeSpec, setup func(home string)) *acctEnv {
	t.Helper()
	e := &acctEnv{t: t, home: t.TempDir()}
	t.Setenv("HOME", e.home)
	t.Setenv("ANTHROPIC_API_KEY", "should-be-stripped")
	t.Setenv("XAI_API_KEY", "should-be-stripped")
	dir := t.TempDir()
	e.specPath, e.logPath = filepath.Join(dir, "spec.json"), filepath.Join(dir, "calls.log")
	e.spec(s)
	t.Setenv("MYRIAGENT_FAKE_ACCT", e.specPath)
	oldPoll := loginPollInterval
	loginPollInterval = 100 * time.Millisecond
	t.Cleanup(func() { loginPollInterval = oldPoll })
	e.usageRes = `{"limits":[{"kind":"session","percent":12},{"kind":"weekly_all","percent":34}]}`
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.usageReq = append(e.usageReq, r)
		res := e.usageRes
		e.mu.Unlock()
		if res == "401" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(res))
	}))
	t.Cleanup(e.srv.Close)
	if setup != nil {
		setup(e.home)
	}
	root := t.TempDir()
	h := &Hub{root: root, convs: map[string]*Conv{}, views: map[*View]struct{}{}, clis: map[string]*cliInfo{}, flushKick: make(chan struct{}, 1)}
	h.settings = loadSettings(root)
	r := h.ar()
	r.cliPaths = map[string]string{}
	for _, p := range providerKeys {
		h.clis[p] = &cliInfo{path: os.Args[0], version: "1.0.0"}
		r.cliPaths[p] = os.Args[0]
	}
	r.http = e.srv.Client()
	r.claudeUsageURL = e.srv.URL + "/api/oauth/usage"
	r.console = directConsole{}
	r.openBrowser = func(u string) {
		e.mu.Lock()
		e.browsed = append(e.browsed, u)
		e.mu.Unlock()
	}
	r.probeTimeout = 20 * time.Second
	h.mu.Lock()
	h.loadAccounts()
	h.mu.Unlock()
	go h.flushLoop()
	e.h = h
	e.tv = &testView{t: t, h: h}
	e.tv.v = h.addView(func(b []byte) error {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		e.tv.mu.Lock()
		e.tv.msgs = append(e.tv.msgs, m)
		e.tv.mu.Unlock()
		return nil
	})
	e.tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	t.Cleanup(h.shutdown)
	return e
}

// refresh：重新檢查一個帳戶並等它做完
func (e *acctEnv) refresh(id string, includeStatus bool, reason string) {
	e.t.Helper()
	e.h.mu.Lock()
	a := e.h.account(id)
	if a == nil {
		e.h.mu.Unlock()
		e.t.Fatalf("沒有帳戶 %s", id)
	}
	ch := e.h.refreshAndWait(a, includeStatus, reason)
	e.h.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		e.t.Fatal("查詢逾時")
	}
}

func (e *acctEnv) acct(id string) Account {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	a := e.h.account(id)
	if a == nil {
		e.t.Fatalf("沒有帳戶 %s", id)
	}
	c := *a
	return c
}

func (e *acctEnv) view(id string) AccountView {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	for _, v := range e.h.accountViews() {
		if v.ID == id {
			return v
		}
	}
	e.t.Fatalf("沒有帳戶 %s", id)
	return AccountView{}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const loggedInClaude = `{"loggedIn":true,"authMethod":"claude.ai","email":"me@example.com","subscriptionType":"max"}`

func freshCredentials(token string) string {
	return credJSON(token, time.Now().Add(2*time.Hour).UnixMilli())
}

// Claude：權杖新鮮時跑 auth status、以權杖查用量端點（只讀；帶 Bearer 與 beta 標頭），用量有走勢點
func TestClaudeStatusAndUsage(t *testing.T) {
	e := newAcctEnv(t, fakeSpec{ClaudeAuthStatus: loggedInClaude}, func(home string) {
		writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), freshCredentials("tok-123"))
	})
	e.refresh("local-claude", true, reasonManual)
	v := e.view("local-claude")
	if v.Status != "ready" || v.Email == nil || *v.Email != "me@example.com" || v.Plan == nil || *v.Plan != "Max 20x" {
		t.Fatalf("帳戶：%+v", v)
	}
	if v.Usage == nil || len(v.Usage.Windows) != 2 || v.Usage.Windows[1].Percent != 34 || v.Usage.Source != "api" || len(v.Usage.Windows[0].History) != 1 {
		t.Fatalf("用量：%+v", v.Usage)
	}
	e.mu.Lock()
	req := e.usageReq[len(e.usageReq)-1]
	e.mu.Unlock()
	if req.Header.Get("Authorization") != "Bearer tok-123" || req.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
		t.Fatalf("標頭：%v", req.Header)
	}
	calls := e.called("auth status --json")
	if len(calls) != 1 || strings.Contains(calls[0], "ANTHROPIC_API_KEY") || !strings.Contains(calls[0], "DISABLE_AUTOUPDATER=1") {
		t.Fatalf("auth status 的呼叫：%v", calls)
	}
	// 身分快取：之後的定期檢查不再跑 auth status
	e.refresh("local-claude", true, reasonPeriodic)
	if n := len(e.called("auth status --json")); n != 1 {
		t.Fatalf("快取有效時不該再跑 auth status（跑了 %d 次）", n)
	}
	// 用量查詢被拒：保留上次的數字、標成過時
	e.mu.Lock()
	e.usageRes = "401"
	e.mu.Unlock()
	e.refresh("local-claude", false, reasonPeriodic)
	v = e.view("local-claude")
	if v.Usage == nil || !v.Usage.Stale || v.Usage.Error == nil || !strings.Contains(*v.Usage.Error, "權杖失效") || len(v.Usage.Windows) != 2 {
		t.Fatalf("過時的用量：%+v", v.Usage)
	}
}

// 權杖快到期：絕不跑 claude auth status（會開始換發卻不等結果，可能把 refresh token 用掉），也不拿過期的權杖查用量
func TestClaudeExpiringTokenNeverRunsAuthStatus(t *testing.T) {
	e := newAcctEnv(t, fakeSpec{ClaudeAuthStatus: loggedInClaude}, func(home string) {
		writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), credJSON("tok", time.Now().Add(time.Minute).UnixMilli()))
	})
	e.refresh("local-claude", true, reasonManual)
	if calls := e.called("auth status"); len(calls) != 0 {
		t.Fatalf("不該跑 auth status：%v", calls)
	}
	v := e.view("local-claude")
	if v.Status != "ready" || v.StatusText != "已登入（權杖待更新）" {
		t.Fatalf("%+v", v)
	}
	if v.Usage == nil || v.Usage.Error == nil || !strings.Contains(*v.Usage.Error, "權杖已過期") {
		t.Fatalf("用量：%+v", v.Usage)
	}
	e.mu.Lock()
	n := len(e.usageReq)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("過期的權杖不該送出去")
	}
	// 空權杖＝登入已失效
	writeFile(t, filepath.Join(e.home, ".claude", ".credentials.json"), credJSON("", time.Now().Add(time.Hour).UnixMilli()))
	e.refresh("local-claude", true, reasonManual)
	if v := e.view("local-claude"); v.Status != "notLoggedIn" || v.StatusText != "登入已失效，請重新登入" || len(e.called("auth status")) != 0 {
		t.Fatalf("%+v", v)
	}
}

// macOS：沒有憑證檔時從鑰匙圈讀（測試換成假的）
func TestClaudeKeychainFallback(t *testing.T) {
	e := newAcctEnv(t, fakeSpec{ClaudeAuthStatus: loggedInClaude}, nil)
	var asked []string
	e.h.mu.Lock()
	e.h.ar().keychain = func(dir string) string { asked = append(asked, dir); return freshCredentials("kc-tok") }
	e.h.mu.Unlock()
	e.refresh("local-claude", true, reasonManual)
	if len(asked) == 0 || asked[0] != filepath.Join(e.home, ".claude") {
		t.Fatalf("鑰匙圈：%v", asked)
	}
	e.mu.Lock()
	auth := e.usageReq[len(e.usageReq)-1].Header.Get("Authorization")
	e.mu.Unlock()
	if auth != "Bearer kc-tok" {
		t.Fatal(auth)
	}
}

// 新增 Codex 隔離帳戶：建立 accounts/{id}/codex、以 app-server 在 app 內登入（開瀏覽器）、登入後重新檢查、用量
func TestAddCodexAccountLogsInAndQueriesUsage(t *testing.T) {
	withProviders(t, "Codex")
	e := newAcctEnv(t, fakeSpec{
		CodexAccount: map[string]any{"account": map[string]any{"type": "chatgpt", "email": "c@x.com", "planType": "pro"}},
		CodexLimits:  map[string]any{"rateLimits": map[string]any{"limitId": "codex", "primary": map[string]any{"usedPercent": 20.0, "windowDurationMins": 10080.0}}},
		CodexLogin:   true,
	}, nil)
	e.tv.send(map[string]any{"cmd": "accountAdd", "provider": "Codex", "label": "工作", "color": "#4fa3f7"})
	done := e.tv.wait("登入完成", func(m map[string]any) bool { return m["type"] == "accountLogin" && m["done"] == true })
	if done["success"] != true || !strings.Contains(done["message"].(string), "已登入") {
		t.Fatalf("登入結果：%v", done)
	}
	e.h.mu.Lock()
	var a *Account
	for _, x := range e.h.accounts {
		if x.Label == "工作" {
			a = x
		}
	}
	e.h.mu.Unlock()
	if a == nil || a.Color != "#4FA3F7" || a.ConfigDir != filepath.Join(e.h.root, "accounts", a.ID, "codex") || !isAccountFolderName(a.ID) {
		t.Fatalf("新帳戶：%+v", a)
	}
	v := e.view(a.ID)
	if v.Status != "ready" || *v.Email != "c@x.com" || *v.Plan != "Pro" || v.LoginBusy || v.Usage == nil || v.Usage.Windows[0].Key != "codex:10080" {
		t.Fatalf("帳戶：%+v", v)
	}
	for _, c := range e.called("app-server") {
		if !strings.Contains(c, "CODEX_HOME="+a.ConfigDir) {
			t.Fatalf("app-server 沒帶隔離資料夾：%s", c)
		}
	}
	e.mu.Lock()
	browsed := append([]string{}, e.browsed...)
	e.mu.Unlock()
	if len(browsed) != 1 || !strings.HasPrefix(browsed[0], "https://auth.openai.com/") {
		t.Fatalf("瀏覽器：%v", browsed)
	}
	if v := e.view("local-codex"); v.IsSystemDefault != true {
		t.Fatal("本機預設的 Codex 帳戶")
	}
}

// Grok：_x.ai/auth/info＋_x.ai/billing；權杖失效（billing 要求登入）＝未登入
func TestGrokStatusAndBilling(t *testing.T) {
	withProviders(t, "Grok")
	e := newAcctEnv(t, fakeSpec{
		GrokInfo:    map[string]any{"email": "g@x.ai"},
		GrokBilling: map[string]any{"subscription_tier": "SuperGrok", "config": map[string]any{"creditUsagePercent": 42.0, "currentPeriod": map[string]any{"type": "USAGE_PERIOD_TYPE_WEEKLY"}}},
	}, nil)
	e.refresh("local-grok", true, reasonManual)
	v := e.view("local-grok")
	if v.Status != "ready" || *v.Email != "g@x.ai" || *v.Plan != "SuperGrok" || v.Usage == nil || v.Usage.Windows[0].Percent != 42 || v.Usage.Windows[0].Label != "每週" {
		t.Fatalf("%+v %+v", v, v.Usage)
	}
	for _, c := range e.called("agent --no-leader stdio") {
		if strings.Contains(c, "XAI_API_KEY") || !strings.Contains(c, "GROK_DISABLE_AUTOUPDATER=1") {
			t.Fatalf("grok 的環境：%s", c)
		}
	}
	e.spec(fakeSpec{GrokInfo: map[string]any{"email": "g@x.ai"}, GrokBillingError: "Authentication required"})
	e.refresh("local-grok", true, reasonManual)
	if v := e.view("local-grok"); v.Status != "notLoggedIn" || v.Usage != nil {
		t.Fatalf("%+v", v)
	}
}

// Antigravity：agy --log-file … -p /usage：信箱取自記錄、額度鎖定時補查點數
func TestAntigravityUsage(t *testing.T) {
	withProviders(t, "Antigravity")
	usage := `{"command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":0}]}]}}}`
	e := newAcctEnv(t, fakeSpec{AgyUsage: usage, AgyEmail: "a@g.com", AgyCredits: `{"command":{"name":"credits","data":{"remaining_credits":12.5}}}`}, nil)
	e.refresh("local-antigravity", true, reasonManual)
	v := e.view("local-antigravity")
	if v.Status != "ready" || v.Email == nil || *v.Email != "a@g.com" || v.Usage == nil || v.Usage.Windows[0].Severity != "max" {
		t.Fatalf("%+v %+v", v, v.Usage)
	}
	if v.Usage.Credits == nil || *v.Usage.Credits != "AI 點數餘額 12.5" {
		t.Fatalf("點數：%v", v.Usage.Credits)
	}
	e.spec(fakeSpec{AgyUsage: "Error: authentication required"})
	e.refresh("local-antigravity", true, reasonManual)
	if v := e.view("local-antigravity"); v.Status != "notLoggedIn" {
		t.Fatal(v.Status)
	}
}

// 檢查失敗：記成 error（不是未登入）、1 分鐘後重試、連續失敗加倍
func TestProbeErrorBacksOff(t *testing.T) {
	withProviders(t, "Codex")
	e := newAcctEnv(t, fakeSpec{}, nil)
	e.h.mu.Lock()
	e.h.clis["Codex"].path = filepath.Join(e.home, "沒有這個檔")
	e.h.mu.Unlock()
	start := time.Now()
	e.refresh("local-codex", true, reasonManual)
	a := e.acct("local-codex")
	if a.status != "error" || !strings.HasPrefix(a.statusText(), "檢查失敗") || a.rt.errorCount != 1 {
		t.Fatalf("%s %s %d", a.status, a.statusText(), a.rt.errorCount)
	}
	if d := a.rt.nextPoll.Sub(start); d < 50*time.Second || d > 70*time.Second {
		t.Fatalf("第一次失敗後 1 分鐘重試，實際 %v", d)
	}
	e.refresh("local-codex", true, reasonManual)
	if d := e.acct("local-codex").rt.nextPoll.Sub(time.Now()); d < 110*time.Second || d > 130*time.Second {
		t.Fatalf("第二次失敗後 2 分鐘重試，實際 %v", d)
	}
	// 沒安裝
	e.h.mu.Lock()
	e.h.clis["Codex"].path = ""
	e.h.mu.Unlock()
	e.refresh("local-codex", true, reasonManual)
	if a := e.acct("local-codex"); a.status != "notInstalled" || a.rt.errorCount != 0 {
		t.Fatal(a.status)
	}
}

// 現有資料夾：原封不動加成帳戶、不開始登入；移除時就算要求刪資料也絕不刪
func TestExistingFolderAccountNeverDeleted(t *testing.T) {
	e := newAcctEnv(t, fakeSpec{ClaudeAuthStatus: loggedInClaude}, func(home string) {
		writeFile(t, filepath.Join(home, ".claude-work", "projects", "x.jsonl"), "{}")
		writeFile(t, filepath.Join(home, ".claude-work", ".credentials.json"), freshCredentials("w"))
		_ = os.MkdirAll(filepath.Join(home, "Documents", "stuff"), 0o700)
	})
	// init 的 providers 列出它
	init := e.tv.wait("init", func(m map[string]any) bool { return m["type"] == "init" })
	ps, _ := init["providers"].([]any)
	folders := ps[0].(map[string]any)["existingFolders"].([]any)
	if len(folders) != 1 || folders[0].(map[string]any)["display"] != "~/.claude-work" {
		t.Fatalf("existingFolders：%v", folders)
	}
	// 不像設定資料夾的不收
	e.tv.send(map[string]any{"cmd": "accountAdd", "provider": "Claude", "directory": "~/Documents"})
	e.tv.wait("錯誤提示", func(m map[string]any) bool {
		return m["type"] == "toast" && m["level"] == "error" && strings.Contains(m["text"].(string), "看起來不是")
	})
	e.tv.send(map[string]any{"cmd": "accountAdd", "provider": "Claude", "directory": "~/.claude-work/", "label": ""})
	e.tv.wait("已加入", func(m map[string]any) bool { return m["type"] == "toast" && m["level"] == "success" })
	e.h.mu.Lock()
	a := e.h.accounts[len(e.h.accounts)-1]
	id := a.ID
	e.h.mu.Unlock()
	if a.Label != "Claude work" || !a.ExistingFolder || a.ConfigDir != filepath.Join(e.home, ".claude-work") || a.shares() {
		t.Fatalf("%+v", a)
	}
	if len(e.called("auth login")) != 0 {
		t.Fatal("現有資料夾不開始登入")
	}
	// 同一個資料夾不能再加一次
	e.tv.send(map[string]any{"cmd": "accountAdd", "provider": "Claude", "directory": filepath.Join(e.home, ".claude-work")})
	e.tv.wait("已經有帳戶在用", func(m map[string]any) bool {
		return m["type"] == "toast" && strings.Contains(m["text"].(string), "已經有帳戶在用")
	})
	e.tv.send(map[string]any{"cmd": "accountRemove", "accountId": id, "deleteData": true})
	e.tv.wait("已移除", func(m map[string]any) bool {
		return m["type"] == "toast" && strings.Contains(m["text"].(string), "已移除")
	})
	if _, err := os.Stat(filepath.Join(e.home, ".claude-work", "projects", "x.jsonl")); err != nil {
		t.Fatal("使用者原有的資料夾被刪掉了")
	}
	// 系統預設帳戶也絕不刪
	e.tv.send(map[string]any{"cmd": "accountRemove", "accountId": "local-claude", "deleteData": true})
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(e.home, ".claude-work")); err != nil {
		t.Fatal("被刪掉了")
	}
}

// 移除 app 建立的隔離帳戶並刪資料：整個 accounts/{id} 刪掉、待刪標記清掉；用量歷史一起刪
func TestRemoveIsolatedAccountDeletesItsFolder(t *testing.T) {
	e := newAcctEnv(t, fakeSpec{ClaudeAuthStatus: `{"loggedIn":false}`, LoginExit: 1}, nil)
	e.tv.send(map[string]any{"cmd": "accountAdd", "provider": "Claude", "label": "", "color": ""})
	e.tv.wait("登入結束", func(m map[string]any) bool { return m["type"] == "accountLogin" && m["done"] == true })
	e.h.mu.Lock()
	a := e.h.accounts[len(e.h.accounts)-1]
	id, root := a.ID, filepath.Join(e.h.root, "accounts", a.ID)
	e.h.mu.Unlock()
	if a.Label != "Claude 帳戶" {
		t.Fatal(a.Label)
	}
	login := e.called("auth login --claudeai")
	if len(login) != 1 || !strings.Contains(login[0], "CLAUDE_CONFIG_DIR="+filepath.Join(root, "claude")) {
		t.Fatalf("登入沒帶隔離資料夾：%v", login)
	}
	writeFile(t, filepath.Join(e.h.root, "usage", id+".json"), "{}")
	e.tv.send(map[string]any{"cmd": "accountRemove", "accountId": id, "deleteData": true})
	e.tv.wait("已移除", func(m map[string]any) bool { return m["type"] == "toast" && m["level"] == "success" })
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("帳戶的資料夾沒刪掉")
	}
	if _, err := os.Stat(root + pendingDeleteSuffix); !os.IsNotExist(err) {
		t.Fatal("待刪標記沒清掉")
	}
	if _, err := os.Stat(filepath.Join(e.h.root, "usage", id+".json")); !os.IsNotExist(err) {
		t.Fatal("用量歷史沒刪")
	}
	b, _ := os.ReadFile(accountsFile(e.h.root))
	if strings.Contains(string(b), id) {
		t.Fatal("accounts.json 還有這個帳戶")
	}
}

// 登入可以取消；沒有終端機時顯示指令
func TestLoginCancelAndNoTerminal(t *testing.T) {
	withProviders(t, "Antigravity")
	e := newAcctEnv(t, fakeSpec{AgyModelsOut: "Please sign in", AgyUsage: "Error: authentication required"}, nil)
	e.tv.send(map[string]any{"cmd": "accountLogin", "accountId": "local-antigravity"})
	e.tv.wait("開了視窗", func(m map[string]any) bool {
		return m["type"] == "accountLogin" && strings.Contains(m["message"].(string), "已開啟 agy 視窗")
	})
	e.tv.send(map[string]any{"cmd": "accountLoginCancel", "accountId": "local-antigravity"})
	e.tv.wait("已取消", func(m map[string]any) bool { return m["type"] == "accountLogin" && m["message"] == "已取消登入" })
	if v := e.view("local-antigravity"); v.LoginBusy {
		t.Fatal("取消後 loginBusy 要清掉")
	}
	// 沒有終端機：顯示指令，登入（在外部）完成後自動結束
	e.h.mu.Lock()
	e.h.ar().console = noConsole{}
	e.h.mu.Unlock()
	e.tv.send(map[string]any{"cmd": "accountLogin", "accountId": "local-claude"})
	e.tv.wait("顯示指令", func(m map[string]any) bool {
		return m["type"] == "accountLogin" && strings.Contains(m["message"].(string), "請在終端機執行") && strings.Contains(m["message"].(string), "auth")
	})
	e.spec(fakeSpec{ClaudeAuthStatus: loggedInClaude})
	done := e.tv.wait("登入完成", func(m map[string]any) bool {
		return m["type"] == "accountLogin" && m["done"] == true && m["accountId"] == "local-claude"
	})
	if done["success"] != true {
		t.Fatalf("%v", done)
	}
}

// 登出：在帳戶自己的資料夾跑 CLI 的登出，清掉身分快取、重新檢查
func TestLogoutClearsIdentity(t *testing.T) {
	e := newAcctEnv(t, fakeSpec{ClaudeAuthStatus: loggedInClaude}, func(home string) {
		writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), freshCredentials("t"))
	})
	e.refresh("local-claude", true, reasonManual)
	if e.acct("local-claude").LastIdentity == nil {
		t.Fatal("要有身分快取")
	}
	e.spec(fakeSpec{ClaudeAuthStatus: `{"loggedIn":false}`})
	e.tv.send(map[string]any{"cmd": "accountLogout", "accountId": "local-claude"})
	deadline := time.Now().Add(10 * time.Second)
	for a := e.acct("local-claude"); time.Now().Before(deadline) && a.status != "notLoggedIn"; a = e.acct("local-claude") {
		time.Sleep(50 * time.Millisecond)
	}
	a := e.acct("local-claude")
	if a.status != "notLoggedIn" || a.LastIdentity != nil || len(e.called("auth logout")) != 1 {
		t.Fatalf("%s %+v %v", a.status, a.LastIdentity, e.calls())
	}
}

func TestAccountUpdateReorderAndRateLimit(t *testing.T) {
	withProviders(t, "Codex")
	e := newAcctEnv(t, fakeSpec{}, nil)
	e.tv.send(map[string]any{"cmd": "accountUpdate", "accountId": "local-claude", "label": "  我的 Claude  ", "color": "#3fb950", "safeMode": true})
	a := e.acct("local-claude")
	if a.Label != "我的 Claude" || a.Color != "#3FB950" || !a.SafeMode {
		t.Fatalf("%+v", a)
	}
	// 系統預設帳戶不能改共用設定
	e.tv.send(map[string]any{"cmd": "accountUpdate", "accountId": "local-claude", "shareCustomizations": false})
	if a := e.acct("local-claude"); !a.shares() {
		t.Fatal("系統預設帳戶不看 shareCustomizations")
	}
	e.tv.send(map[string]any{"cmd": "accountReorder", "ids": []any{"local-codex", "nope"}})
	e.h.mu.Lock()
	first := e.h.accounts[0].ID
	e.h.mu.Unlock()
	if first != "local-codex" {
		t.Fatal(first)
	}
	// 對話回報的額度即時合併
	e.h.mu.Lock()
	e.h.accountRateLimit("local-claude", map[string]any{"status": "allowed", "unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": 0.5}}})
	e.h.mu.Unlock()
	e.tv.wait("用量", func(m map[string]any) bool {
		if m["type"] != "accounts" {
			return false
		}
		for _, x := range m["accounts"].([]any) {
			am := x.(map[string]any)
			if am["id"] == "local-claude" && am["usage"] != nil {
				u := am["usage"].(map[string]any)
				return u["source"] == "session" && u["windows"].([]any)[0].(map[string]any)["percent"] == 50.0
			}
		}
		return false
	})
	// 重開（換新的中樞）之後先顯示上次的用量（標成過時）
	e.h.mu.Lock()
	e.h.ar().history.flush()
	root := e.h.root
	e.h.mu.Unlock()
	h2 := &Hub{root: root, convs: map[string]*Conv{}, views: map[*View]struct{}{}, clis: map[string]*cliInfo{}, flushKick: make(chan struct{}, 1)}
	h2.mu.Lock()
	h2.loadAccounts()
	var u *Usage
	for _, v := range h2.accountViews() {
		if v.ID == "local-claude" {
			u = v.Usage
		}
	}
	h2.mu.Unlock()
	if u == nil || !u.Stale || u.Source != "cache" || u.Windows[0].Percent != 50 {
		t.Fatalf("重開後的用量：%+v", u)
	}
}

// 帳戶清單損毀：備份原檔，從資料夾還原隔離帳戶（Id 沿用資料夾名稱），本機預設帳戶用固定 Id
func TestAccountsRecoveredFromFolders(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	id := strings.Repeat("d", 32)
	_ = os.MkdirAll(filepath.Join(root, "accounts", id, "claude"), 0o700)
	gone := strings.Repeat("e", 32)
	_ = os.MkdirAll(filepath.Join(root, "accounts", gone, "claude"), 0o700)
	_ = os.WriteFile(filepath.Join(root, "accounts", gone+pendingDeleteSuffix), []byte(gone), 0o600)
	_ = os.WriteFile(accountsFile(root), []byte("{壞掉"), 0o600)
	h := &Hub{root: root, convs: map[string]*Conv{}, clis: map[string]*cliInfo{}}
	h.loadAccounts()
	if len(h.accounts) != 2 || h.accounts[0].ID != id || h.accounts[1].ID != "local-claude" {
		t.Fatalf("%+v", h.accounts)
	}
	matches, _ := filepath.Glob(accountsFile(root) + ".bad-*")
	if len(matches) != 1 {
		t.Fatal("損毀的原檔要備份")
	}
	retryPendingDeletes(filepath.Join(root, "accounts"))
	if _, err := os.Stat(filepath.Join(root, "accounts", gone)); !os.IsNotExist(err) {
		t.Fatal("留有待刪標記的資料夾要再刪一次")
	}
	if _, err := os.Stat(filepath.Join(root, "accounts", id)); err != nil {
		t.Fatal("其他帳戶的資料夾不能刪")
	}
}

// Codex 的免費重置：送 consume、重讀用量、以 toast 告訴送出的視窗；次數明確是 0 就不送
func TestCodexResetCredit(t *testing.T) {
	withProviders(t, "Codex")
	limits := map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 0.0, "windowDurationMins": 300.0}},
		"rateLimitResetCredits": map[string]any{"availableCount": 1.0}}
	e := newAcctEnv(t, fakeSpec{
		CodexAccount: map[string]any{"account": map[string]any{"type": "chatgpt", "email": "c@x.com"}},
		CodexLimits:  limits, ResetOutcome: "reset",
	}, nil)
	e.refresh("local-codex", true, reasonManual)
	e.tv.send(map[string]any{"cmd": "accountResetCredit", "accountId": "local-codex"})
	e.tv.wait("已重置", func(m map[string]any) bool {
		return m["type"] == "toast" && m["level"] == "success" && strings.Contains(m["text"].(string), "已使用 1 次")
	})
	if len(e.called("rpc account/rateLimitResetCredit/consume")) != 1 {
		t.Fatal(e.calls())
	}
	limits["rateLimitResetCredits"] = map[string]any{"availableCount": 0.0}
	e.spec(fakeSpec{CodexAccount: map[string]any{"account": map[string]any{"type": "chatgpt"}}, CodexLimits: limits})
	e.refresh("local-codex", true, reasonManual)
	e.tv.send(map[string]any{"cmd": "accountResetCredit", "accountId": "local-codex"})
	e.tv.wait("沒有次數", func(m map[string]any) bool {
		return m["type"] == "toast" && m["text"] == "沒有可用的免費重置次數"
	})
	if len(e.called("rpc account/rateLimitResetCredit/consume")) != 1 {
		t.Fatal("次數是 0 不該送")
	}
}
