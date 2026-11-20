package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 帳戶的查詢排程（照 C# 的 Accounts/AccountManager.cs 的狀態與用量部分）：每個帳戶定期（Claude 2／10 分鐘、其他 5／15 分鐘，
// 對話在跑時用短的）重新檢查登入狀態與用量；檢查失敗以 1 → 2 → 4 分鐘退避；憑證檔一變就重查；一輪對話結束 10 秒後查一次用量。
// 查詢在 goroutine 上跑（不持有 h.mu），結果回來時拿鎖套用。
//
// 各家怎麼查：
//   - Claude：讀憑證（.credentials.json，macOS 的在鑰匙圈）判斷能不能跑 claude auth status（權杖快到期時不跑：它會開始換發卻不等結果，
//     可能把 refresh token 用掉、之後被登出）；用量查官方端點 GET /api/oauth/usage（只讀權杖、絕不自己換發）
//   - Codex：短命的 codex app-server：account/read、account/rateLimits/read
//   - Antigravity：agy --log-file … -p /usage --output-format json（記錄裡有登入的信箱）
//   - Grok：短命的 grok agent stdio：_x.ai/auth/info、_x.ai/billing

const (
	accountTickInterval = 20 * time.Second
	claudeUsageURL      = "https://api.anthropic.com/api/oauth/usage"
	pendingDeleteSuffix = ".deleting"
)

// acctRuntime：中樞層級的帳戶相依（h.ar() 取得；測試換掉 http、鑰匙圈、終端機、瀏覽器）
type acctRuntime struct {
	// http：查 Claude 用量端點用；nil＝不查（測試預設：不連網）
	http           *http.Client
	claudeUsageURL string
	// keychain：macOS 的 Claude 憑證在鑰匙圈；參數是設定資料夾，回憑證 JSON（沒有回空字串）。nil＝只看檔案
	keychain func(configDir string) string
	history  *usageHistory
	// openBrowser：登入流程開瀏覽器（測試換成記錄）
	openBrowser func(url string)
	// console：開終端機視窗執行登入指令（測試換成直接執行）
	console   consoleLauncher
	loginGate chan struct{}
	cacheDir  string
	// usageObservers：存下一份新看到的用量快照時呼叫（持有 h.mu；每週 5% 分段紀錄用）
	usageObservers []func(h *Hub, a *Account, s *usageSnapshot)
	// probeTimeout：短命 CLI 查詢的時限（測試可縮短）
	probeTimeout time.Duration
	// fetchText：向官方查 CLI 最新版的 GET；nil＝不查（測試預設：不連網）
	fetchText func(url string) (string, error)
	// findNpm：npm 的位置；nil＝沒有（測試預設：不會碰到真的 npm）
	findNpm func() string
	// cliPaths：測試指定的 CLI 位置（有設定就只看它）
	cliPaths map[string]string
}

// ar 取得帳戶相依（第一次用時建立，預設是不連網、不碰鑰匙圈的安全值；呼叫時持有 h.mu）
func (h *Hub) ar() *acctRuntime {
	if h.acct == nil {
		h.acct = &acctRuntime{
			claudeUsageURL: claudeUsageURL,
			history:        newUsageHistory(filepath.Join(h.root, "usage")),
			openBrowser:    func(string) {},
			console:        noConsole{},
			loginGate:      make(chan struct{}, 1),
			cacheDir:       filepath.Join(h.root, "cache"),
			probeTimeout:   30 * time.Second,
		}
		h.acct.usageObservers = append(h.acct.usageObservers, usageObserverHooks...)
	}
	return h.acct
}

// usageObserverHooks：各功能登記的用量觀察者（init 時 append）
var usageObserverHooks []func(h *Hub, a *Account, s *usageSnapshot)

func init() {
	registerStartHook(func(h *Hub) {
		h.mu.Lock()
		r := h.ar()
		r.http = &http.Client{Timeout: 30 * time.Second}
		if runtime.GOOS == "darwin" {
			r.keychain = readClaudeKeychain
		}
		r.openBrowser = openBrowser
		r.console = systemConsole{}
		for _, a := range h.accounts {
			h.requestRefresh(a, true, reasonStartup, false)
		}
		h.mu.Unlock()
		h.accountTickLoop()
	})
}

func (h *Hub) accountTickLoop() {
	t := time.NewTicker(accountTickInterval)
	defer t.Stop()
	for range t.C {
		h.mu.Lock()
		h.accountTick(time.Now())
		h.mu.Unlock()
		h.kick()
	}
}

// accountTick：顯示的用量到了重置時間就先推算歸零；憑證檔變了就重查；輪詢到期的帳戶重查（未登入、未安裝、上次失敗的連狀態一起）
func (h *Hub) accountTick(now time.Time) {
	r := h.ar()
	changed := false
	for _, a := range h.accounts {
		if reset := applyDueResets(a.rt.usage, now.UnixMilli()); reset != nil {
			a.rt.usage = reset
			changed = true
		}
		if f := a.credentialFile(); f != "" {
			sig := fileSignature(f)
			if a.rt.credSig != "" && sig != a.rt.credSig {
				a.rt.credSig = sig
				h.requestRefresh(a, true, reasonCredentials, false)
				continue
			}
			a.rt.credSig = sig
		}
		if !a.rt.nextPoll.IsZero() && a.rt.nextPoll.After(now) {
			continue
		}
		reason := reasonPeriodic
		include := shouldRecheckStatusOnTick(a.status)
		if a.rt.againReason != "" && !a.rt.refreshing {
			reason = strongerReason(reason, a.rt.againReason)
			include = true
		}
		h.requestRefresh(a, include, reason, true)
	}
	r.history.flush()
	if changed {
		h.pushAccounts()
	}
}

func fileSignature(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
}

// lifetime：帳戶的登入與查詢共用的取消來源（移除帳戶時取消）
func (a *Account) lifetime() context.Context {
	if a.rt.lifetime == nil {
		a.rt.lifetime, a.rt.cancel = context.WithCancel(context.Background())
	}
	return a.rt.lifetime
}

// pollInterval：對話在跑時用短的間隔
func (h *Hub) pollInterval(a *Account) time.Duration {
	for _, c := range h.convs {
		if c.Meta.AccountID == a.ID && (c.running || h.alive(c)) {
			return activePoll(a.Provider)
		}
	}
	return idlePoll(a.Provider)
}

// pollSoon：delay 之後查一次（不晚於原本排定的時間）
func (h *Hub) pollSoon(a *Account, delay time.Duration) {
	due := time.Now().Add(delay)
	if a.rt.soonPoll.IsZero() || a.rt.soonPoll.After(due) {
		a.rt.soonPoll = due
	}
	if a.rt.nextPoll.IsZero() || a.rt.nextPoll.After(due) {
		a.rt.nextPoll = due
	}
}

// requestRefresh：重新檢查（includeStatus＝連登入狀態一起查；Codex、agy、Grok 的查詢本來就包含帳戶資訊）。
// 已有查詢進行中：skipIfRunning（定期輪詢）就略過；否則記下，做完再查一次（呼叫時持有 h.mu）
func (h *Hub) requestRefresh(a *Account, includeStatus bool, reason string, skipIfRunning bool) {
	if a.rt.removed {
		return
	}
	if a.rt.refreshing {
		if skipIfRunning {
			return
		}
		a.rt.again = true
		a.rt.againStatus = a.rt.againStatus || includeStatus
		a.rt.againReason = strongerReason(a.rt.againReason, reason)
		return
	}
	h.startRefresh(a, includeStatus, reason)
}

// refreshAndWait：重新檢查並回傳一個在查詢（含之後補做的）做完時關閉的 channel（呼叫時持有 h.mu）
func (h *Hub) refreshAndWait(a *Account, includeStatus bool, reason string) <-chan struct{} {
	ch := make(chan struct{})
	a.rt.waiters = append(a.rt.waiters, ch)
	h.requestRefresh(a, includeStatus, reason, false)
	return ch
}

// probeInput：查詢需要的資料（在鎖內複製，查詢在鎖外跑）
type probeInput struct {
	acct          Account // 複本（只用來算環境與資料夾）
	provider      string
	status        string
	includeStatus bool
	reason        string
	cliPath       string
	env           []string
	cache         *identityCache
	http          *http.Client
	usageURL      string
	keychain      func(string) string
	ctx           context.Context
	cacheDir      string
	timeout       time.Duration
}

// probeResult：查詢的結果（拿鎖之後由 applyProbe 套用）
type probeResult struct {
	statusSet     bool
	status        string
	statusText    string
	email, plan   string
	expSet        bool
	accessExp     int64
	loginExp      int64
	identitySet   bool
	identity      *identityCache
	usage         *usageSnapshot
	staleSet      bool
	staleErr      string
	pendingStatus string // 這次沒做成、下次輪詢以這個理由補查
}

func (r *probeResult) setStatus(status, text string) {
	r.statusSet, r.status, r.statusText = true, status, text
}

func (r *probeResult) stale(err string) { r.staleSet, r.staleErr = true, err }

func (h *Hub) startRefresh(a *Account, includeStatus bool, reason string) {
	r := h.ar()
	now := time.Now()
	a.rt.refreshing = true
	a.rt.again, a.rt.againStatus, a.rt.againReason = false, false, ""
	a.rt.nextPoll = now.Add(h.pollInterval(a))
	if !a.rt.soonPoll.IsZero() && !a.rt.soonPoll.After(now) {
		a.rt.soonPoll = time.Time{}
	}
	in := probeInput{
		acct: *a, provider: a.Provider, status: a.status, includeStatus: includeStatus, reason: reason,
		env: commandEnv(a), http: r.http, usageURL: r.claudeUsageURL, keychain: r.keychain, ctx: a.lifetime(),
		cacheDir: r.cacheDir, timeout: r.probeTimeout,
	}
	in.acct.rt = accountRuntime{}
	if a.LastIdentity != nil {
		c := *a.LastIdentity
		in.cache = &c
	}
	if c := h.clis[a.Provider]; c != nil {
		in.cliPath = c.path
	}
	if a.agyShadowed() {
		prepareAgyHome(a.ConfigDir)
	}
	go func() {
		var res probeResult
		func() {
			defer func() {
				if p := recover(); p != nil {
					res = probeResult{}
					res.setStatus("error", fmt.Sprintf("檢查失敗：%v", p))
				}
			}()
			res = runProbe(in)
		}()
		h.mu.Lock()
		defer h.mu.Unlock()
		defer h.kick()
		h.finishRefresh(a, res)
	}()
}

func runProbe(in probeInput) probeResult {
	switch in.provider {
	case "Claude":
		return probeClaudeAccount(in)
	case "Codex":
		return probeCodexAccount(in)
	case "Antigravity":
		return probeAgyAccount(in)
	case "Grok":
		return probeGrokAccount(in)
	}
	var r probeResult
	r.setStatus("notInstalled", "")
	return r
}

func (h *Hub) finishRefresh(a *Account, res probeResult) {
	a.rt.refreshing = false
	if a.rt.removed || h.account(a.ID) != a {
		for _, w := range a.rt.waiters {
			close(w)
		}
		a.rt.waiters = nil
		return
	}
	h.applyProbe(a, res)
	if a.status == "error" {
		a.rt.errorCount++
	} else {
		a.rt.errorCount = 0
	}
	delay := nextPollDelay(a.status, a.rt.errorCount, h.pollInterval(a))
	next := time.Now().Add(delay)
	if !a.rt.soonPoll.IsZero() && a.rt.soonPoll.Before(next) {
		next = a.rt.soonPoll
	}
	a.rt.soonPoll = time.Time{}
	a.rt.nextPoll = next
	a.rt.refreshGen++
	if a.rt.again {
		h.startRefresh(a, a.rt.againStatus, orDefault(a.rt.againReason, reasonPeriodic))
	} else {
		for _, w := range a.rt.waiters {
			close(w)
		}
		a.rt.waiters = nil
	}
	h.pushAccounts()
}

// applyProbe：把查詢的結果套到帳戶（持有 h.mu）
func (h *Hub) applyProbe(a *Account, r probeResult) {
	save := false
	if r.email != "" && a.email != r.email {
		a.email = r.email
		save = true
	}
	if r.plan != "" && a.plan != r.plan {
		a.plan = r.plan
		save = true
	}
	if r.statusSet {
		a.status, a.rt.statusText = r.status, r.statusText
		if r.status == "notLoggedIn" {
			a.rt.usage = nil
		}
	}
	if r.expSet {
		a.rt.accessExpiresAt, a.rt.loginExpiresAt = r.accessExp, r.loginExp
	}
	if r.identitySet && !sameIdentity(a.LastIdentity, r.identity) {
		a.LastIdentity = r.identity
		save = true
	}
	if r.pendingStatus != "" {
		a.rt.againReason = strongerReason(a.rt.againReason, r.pendingStatus)
		h.pollSoon(a, time.Minute)
	}
	if r.usage != nil {
		h.storeUsage(a, r.usage)
	} else if r.staleSet {
		h.markStale(a, r.staleErr)
	}
	if save {
		h.saveAccounts()
	}
}

func sameIdentity(a, b *identityCache) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	x, y := *a, *b
	x.CheckedAt, y.CheckedAt = 0, 0
	return x == y
}

// storeUsage：存下一份新看到的用量快照：記進歷史、通知觀察者（每週 5% 分段紀錄）
func (h *Hub) storeUsage(a *Account, s *usageSnapshot) {
	a.rt.usage = s
	h.ar().history.record(a.ID, s, s.FetchedAt)
	for _, f := range h.ar().usageObservers {
		f(h, a, s)
	}
}

// markStale：查詢失敗：保留上次的資料（推算重置），標成過時並寫上原因
func (h *Hub) markStale(a *Account, errText string) {
	last := a.rt.usage
	if last == nil {
		last = h.ar().history.lastSnapshot(a.ID)
	}
	if last == nil {
		a.rt.usage = emptyUsage(errText)
		return
	}
	n := *last
	n.Windows = applyUsageResets(last.Windows, nowMs())
	n.Stale, n.Error, n.Source = true, errText, "cache"
	a.rt.usage = &n
}

// usageView：AccountView.usage（含每個時間窗的走勢點）
func (h *Hub) usageView(a *Account) *Usage {
	s := a.rt.usage
	if s == nil {
		if last := h.ar().history.lastSnapshot(a.ID); last != nil && a.status != "notLoggedIn" {
			n := *last
			n.Windows = applyUsageResets(last.Windows, nowMs())
			n.Stale, n.Source = true, "cache"
			a.rt.usage = &n
			s = &n
		} else {
			return nil
		}
	}
	u := &Usage{Stale: s.Stale, Source: s.Source, Windows: []UsageWindow{}}
	if s.FetchedAt > 0 {
		f := s.FetchedAt
		u.FetchedAt = &f
	}
	if s.Error != "" {
		e := s.Error
		u.Error = &e
	}
	if s.Credits != "" {
		c := s.Credits
		u.Credits = &c
	}
	if s.Note != "" {
		n := s.Note
		u.Note = &n
	}
	if s.ResetCredits != nil {
		c := *s.ResetCredits
		u.ResetCredits = &c
		if s.ResetCreditExpiresAt > 0 {
			e := s.ResetCreditExpiresAt
			u.ResetCreditExpiresAt = &e
		}
	}
	now := nowMs()
	for _, w := range s.Windows {
		v := UsageWindow{Key: w.Key, Label: w.Label, Percent: w.Percent, Severity: usageSeverity(w.Percent, w.Locked), History: h.ar().history.view(a.ID, w.Key, now)}
		if v.History == nil {
			v.History = [][2]float64{}
		}
		if w.ResetsAt > 0 {
			r := w.ResetsAt
			v.ResetsAt = &r
		}
		u.Windows = append(u.Windows, v)
	}
	return u
}

// ---- 對話回報（各家的 Provider 呼叫；持有 h.mu） ----

// accountTurnCompleted：一輪對話完成：10 秒後查一次用量（讓伺服器端統計跟上）
func (h *Hub) accountTurnCompleted(accountID string) {
	if a := h.account(accountID); a != nil {
		h.pollSoon(a, 10*time.Second)
	}
}

// accountQuotaExhausted：對話回報額度已用完：立即重查讓側欄圓環更新
func (h *Hub) accountQuotaExhausted(accountID string) {
	if a := h.account(accountID); a != nil {
		h.pollSoon(a, 0)
		h.requestRefresh(a, false, reasonPeriodic, true)
	}
}

// accountRateLimit：對話程序回報的額度（Claude 的 rate_limit_event.rate_limit_info、Codex 的 rateLimits/updated）：即時合併進快照。
// 每個 API 請求都回報一次、數字通常沒變：畫面上看得到的都一樣就不通知
func (h *Hub) accountRateLimit(accountID string, raw map[string]any) {
	a := h.account(accountID)
	if a == nil {
		return
	}
	var updates []usageWindow
	credits, note := "", ""
	switch a.Provider {
	case "Claude":
		updates = parseClaudeRateLimitEvent(raw)
	case "Codex":
		updates, credits = parseCodexRateLimits(raw)
		note = describeCodexBlock(raw)
	default:
		return
	}
	if len(updates) == 0 {
		return
	}
	cur := a.rt.usage
	next := &usageSnapshot{FetchedAt: nowMs(), Source: "session", Credits: credits, Note: note}
	if cur != nil {
		next.Windows = mergeUsageWindows(cur.Windows, updates)
		next.Credits = firstNonBlank(credits, cur.Credits)
		next.Note = firstNonBlank(note, cur.Note)
		next.ResetCredits, next.ResetCreditExpiresAt = cur.ResetCredits, cur.ResetCreditExpiresAt
	} else {
		next.Windows = mergeUsageWindows(nil, updates)
	}
	h.storeUsage(a, next)
	if !sameUsageForView(cur, next) {
		h.pushAccounts()
	}
}

// ---- Claude ----

// readClaudeCredentials：讀 Claude 的憑證檔（大小、修改時間與內容）；沒有檔案時問鑰匙圈（macOS）
func readClaudeCredentials(configDir string, keychain func(string) string) claudeCredentials {
	file := filepath.Join(configDir, ".credentials.json")
	fi, err := os.Stat(file)
	if err != nil {
		if os.IsNotExist(err) && keychain != nil {
			if j := keychain(configDir); j != "" {
				return claudeCredentials{exists: true, size: int64(len(j)), json: j}
			}
		}
		if os.IsNotExist(err) {
			return claudeCredentials{}
		}
		return claudeCredentials{exists: true}
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return claudeCredentials{exists: true, size: fi.Size(), modTime: fi.ModTime().UnixNano()}
	}
	return claudeCredentials{exists: true, size: fi.Size(), modTime: fi.ModTime().UnixNano(), json: string(b)}
}

func readClaudeGlobal(a *Account) claudeGlobalConfig {
	legacy, config := a.claudeGlobalConfigPaths()
	path := config
	if pathExists(legacy) {
		path = legacy
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return claudeGlobalConfig{readable: true} // 不存在：CLI 自己也當成沒有啟用認證管理員
	}
	if err != nil {
		return claudeGlobalConfig{}
	}
	return parseClaudeGlobalConfig(string(b))
}

func probeClaudeAccount(in probeInput) probeResult {
	var r probeResult
	a := &in.acct
	status := in.status
	configDir := a.resolveConfigDir()
	cred := readClaudeCredentials(configDir, in.keychain)
	if in.includeStatus || status == "checking" || status == "error" || status == "notInstalled" {
		if in.cliPath == "" {
			r.setStatus("notInstalled", "")
			return r
		}
		var global *claudeGlobalConfig
		if !cred.exists {
			g := readClaudeGlobal(a)
			global = &g
		}
		r.expSet, r.accessExp, r.loginExp = true, readClaudeExpiresAt(cred.json), readClaudeLoginExpiresAt(cred.json)
		detailedPlan := readClaudePlanFromCredentials(cred.json)
		cache := in.cache
		switch decideClaudeStatusCheck(cred, cache, in.reason, time.Now(), global) {
		case decideUseCache:
			r.email, r.plan = cache.Email, firstNonBlank(detailedPlan, cache.Plan)
			r.setStatus(cache.Status, "")
		case decideTokenPending:
			// 權杖已過期或快到期：auth status 會開始換發卻不等結果（可能把 refresh token 用掉），先不查；CLI 在對話中自己換發後監看會再查
			r.plan = detailedPlan
			if status != "apiKey" {
				r.setStatus("ready", "已登入（權杖待更新）")
			}
		case decideLoginExpired:
			r.identitySet = true
			r.setStatus("notLoggedIn", "登入已失效，請重新登入")
		case decideConservative:
			kept := "ready"
			if cache != nil {
				kept = cache.Status
				r.plan = cache.Plan
			}
			r.email = firstNonBlank(global.oauthEmail, emailOf(cache))
			text := "已登入（憑證在系統的認證管理員）"
			if kept == "apiKey" {
				text = ""
			}
			r.setStatus(kept, text)
		case decideCredUnreadable:
			r.setStatus("error", "檢查失敗：暫時讀不到登入憑證，稍後再查")
			r.pendingStatus = in.reason
		case decideConfigUnread:
			if cache != nil && (cache.Status == "ready" || cache.Status == "apiKey") {
				r.email, r.plan = cache.Email, cache.Plan
				text := "已登入（暫時讀不到 CLI 設定，稍後再確認）"
				if cache.Status == "apiKey" {
					text = ""
				}
				r.setStatus(cache.Status, text)
			} else {
				r.setStatus("error", "檢查失敗：暫時讀不到 Claude Code 的設定檔，稍後再查")
			}
			r.pendingStatus = in.reason
		default:
			res := runTool(in.ctx, in.cliPath, []string{"auth", "status", "--json"}, in.env, ensureDir(in.cacheDir), in.timeout)
			if in.ctx.Err() != nil {
				return probeResult{}
			}
			st, email, plan := evaluateClaudeAuthStatus(res.stdout + "\n" + res.stderr)
			r.email, r.plan = email, firstNonBlank(detailedPlan, plan)
			if st == "error" {
				r.setStatus("error", "檢查失敗："+describeClaudeAuthStatusFailure(res.stderr, res.timedOut))
			} else {
				r.setStatus(st, "")
				r.identitySet = true
				if st == "ready" || st == "apiKey" {
					r.identity = &identityCache{Status: st, Email: email, Plan: r.plan, CredSize: cred.size, CredMod: cred.modTime,
						ExpiresAt: readClaudeExpiresAt(cred.json), CheckedAt: nowMs()}
				}
			}
		}
		if r.statusSet {
			status = r.status
		}
	}
	if status != "ready" && status != "apiKey" {
		return r
	}
	if in.http == nil {
		return r
	}
	usage, errText := fetchClaudeUsage(in.ctx, in.http, in.usageURL, cred.json)
	switch {
	case usage != nil:
		r.usage = usage
	default:
		r.stale(errText)
	}
	return r
}

func emailOf(c *identityCache) string {
	if c == nil {
		return ""
	}
	return c.Email
}

// claudeUsageExpirySkew：權杖到期前多久就當作過期（避免查到一半剛好過期）
const claudeUsageExpirySkew = 2 * time.Minute

// readClaudeToken：憑證 → 權杖與到期時間（毫秒，0＝沒有）；沒有權杖回空字串
func readClaudeToken(credentialsJSON string) (string, int64) {
	oauth, ok := claudeOAuth(credentialsJSON)
	if !ok || oauth == nil {
		return "", 0
	}
	t, _ := oauth["accessToken"].(string)
	if strings.TrimSpace(t) == "" {
		return "", 0
	}
	exp, _ := oauth["expiresAt"].(float64)
	return t, int64(exp)
}

// fetchClaudeUsage：GET /api/oauth/usage（與 CLI 的 /usage 相同）。只讀權杖、絕不更新：自己 refresh 會輪換 refresh token，
// CLI 手上的舊 token 失效就等於被登出。回（快照, nil）或（nil, 給人看的原因）
func fetchClaudeUsage(ctx context.Context, client *http.Client, url, credentialsJSON string) (*usageSnapshot, string) {
	token, exp := readClaudeToken(credentialsJSON)
	if token == "" {
		return nil, "找不到登入憑證（API Key 或第三方登入沒有訂閱用量）"
	}
	if exp > 0 && time.UnixMilli(exp).Add(-claudeUsageExpirySkew).Before(time.Now()) {
		return nil, "登入權杖已過期，下次使用這個帳戶時會自動更新"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "用量查詢失敗：" + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "用量查詢失敗：" + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, "用量查詢被拒（權杖失效），請重新登入這個帳戶"
	case http.StatusTooManyRequests:
		return nil, "用量查詢太頻繁，稍後再試"
	default:
		return nil, fmt.Sprintf("用量查詢失敗（HTTP %d）", resp.StatusCode)
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return nil, "用量回應的格式無法解析"
	}
	windows, note := parseClaudeUsage(doc)
	// 回了 200 卻解析不出任何時間窗（端點改版）：當成查詢失敗，保留上次的資料與走勢
	if len(windows) == 0 {
		return nil, "用量回應的格式無法解析"
	}
	return &usageSnapshot{FetchedAt: nowMs(), Source: "api", Windows: windows, Note: note}, ""
}

// ---- macOS 鑰匙圈 ----

// claudeKeychainService：鑰匙圈項目的名稱（照 Claude Code 自己的規則）：預設的設定資料夾（沒有設 CLAUDE_CONFIG_DIR）是
// 「Claude Code-credentials」；指定了設定資料夾的後面再加「-」與資料夾路徑 SHA-256 的前 8 個十六進位字元
func claudeKeychainService(configDir, inheritedConfigDir, home string) string {
	const name = "Claude Code-credentials"
	if strings.TrimSpace(inheritedConfigDir) == "" && strings.TrimRight(configDir, "/") == filepath.Join(home, ".claude") {
		return name
	}
	sum := sha256.Sum256([]byte(configDir))
	return name + "-" + hex.EncodeToString(sum[:])[:8]
}

var (
	keychainMu    sync.Mutex
	keychainCache = map[string]struct {
		at   time.Time
		json string
	}{}
)

// readClaudeKeychain：以系統的 security 工具讀（那些項目本來就是 security 建的，由它讀不會跳出允許存取的詢問）。
// 只讀、不寫；讀到的內容不記進記錄檔。同一個帳戶 2 秒內只讀一次
func readClaudeKeychain(configDir string) string {
	service := claudeKeychainService(configDir, os.Getenv("CLAUDE_CONFIG_DIR"), userHome())
	keychainMu.Lock()
	if c, ok := keychainCache[service]; ok && time.Since(c.at) < 2*time.Second {
		keychainMu.Unlock()
		return c.json
	}
	keychainMu.Unlock()
	account := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		account = u.Username
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w").Output()
	text := ""
	if err == nil {
		if t := strings.TrimSpace(string(out)); strings.HasPrefix(t, "{") {
			text = t
		}
	}
	keychainMu.Lock()
	keychainCache[service] = struct {
		at   time.Time
		json string
	}{time.Now(), text}
	keychainMu.Unlock()
	return text
}

// ---- Codex ----

func probeCodexAccount(in probeInput) probeResult {
	var r probeResult
	if in.cliPath == "" {
		r.setStatus("notInstalled", "")
		return r
	}
	c, err := startCodexAppServer(in.ctx, in.cliPath, in.env, ensureDir(in.cacheDir))
	if err != nil {
		if in.ctx.Err() != nil {
			return probeResult{}
		}
		r.setStatus("error", "檢查失敗："+err.Error())
		return r
	}
	defer c.close()
	acc, err := c.callMap(in.ctx, "account/read", map[string]any{"refreshToken": false}, 20*time.Second)
	if in.ctx.Err() != nil {
		return probeResult{}
	}
	if err != nil {
		r.setStatus("error", "檢查失敗："+err.Error())
		return r
	}
	st, email, plan := evaluateCodexAccount(acc)
	r.setStatus(st, "")
	r.email, r.plan = email, plan
	if st != "ready" {
		return r
	}
	limits, err := c.callMap(in.ctx, "account/rateLimits/read", nil, 30*time.Second)
	if in.ctx.Err() != nil {
		return probeResult{}
	}
	switch {
	case err != nil:
		r.stale(err.Error())
	default:
		r.usage, r.staleErr = codexSnapshot(limits)
		r.staleSet = r.usage == nil
	}
	return r
}

// codexSnapshot：account/rateLimits/read 的結果 → 快照；看不懂（沒有任何額度桶）回 nil 與原因
func codexSnapshot(limits map[string]any) (*usageSnapshot, string) {
	windows, credits := parseCodexRateLimits(limits)
	if len(windows) == 0 && !hasCodexRateLimits(limits) {
		return nil, "用量回應的格式無法解析"
	}
	count, exp := parseCodexResetCredits(limits)
	return &usageSnapshot{FetchedAt: nowMs(), Source: "api", Windows: windows, Credits: credits, Note: describeCodexBlock(limits),
		ResetCredits: count, ResetCreditExpiresAt: exp}, ""
}

// ---- Antigravity ----

// agyJSONPayload：輸出裡的 JSON（整段，或最後一行以 { 開頭的）
func agyJSONPayload(stdout string) map[string]any {
	t := strings.TrimSpace(stdout)
	var m map[string]any
	if strings.HasPrefix(t, "{") && json.Unmarshal([]byte(t), &m) == nil {
		return m
	}
	lines := strings.Split(strings.ReplaceAll(t, "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && json.Unmarshal([]byte(l), &m) == nil {
			return m
		}
	}
	return nil
}

// evaluateAgyUsage：agy -p /usage --output-format json 的結果 → 狀態與 command.data
func evaluateAgyUsage(res cliResult) (status string, data map[string]any, errText string) {
	all := res.stdout + "\n" + res.stderr
	if agyNotSignedIn.MatchString(all) {
		return "notLoggedIn", nil, ""
	}
	if res.timedOut {
		return "error", nil, "agy -p /usage 逾時沒有回應"
	}
	root := agyJSONPayload(res.stdout)
	if root != nil && strings.EqualFold(jstr(root, "status"), "ERROR") {
		e := agyErrorText(root["error"])
		if e != "" && agyNotSignedIn.MatchString(e) {
			return "notLoggedIn", nil, ""
		}
		return "error", nil, firstNonBlank(e, "agy -p /usage 執行失敗")
	}
	if res.exitCode != 0 {
		last := ""
		for _, l := range strings.Split(strings.ReplaceAll(all, "\r", ""), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "{") {
				last = l
			}
		}
		return "error", nil, firstNonBlank(last, fmt.Sprintf("agy -p /usage 結束碼 %d", res.exitCode))
	}
	if cmd := jobj(root, "command"); cmd != nil && strings.EqualFold(jstr(cmd, "name"), "usage") {
		if d := jobj(cmd, "data"); d != nil {
			return "ready", d, ""
		}
	}
	return "ready", nil, "用量回應的格式無法解析"
}

func agyErrorText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		return firstNonBlank(jstr(x, "message"), jstr(x, "error"))
	}
	return ""
}

// parseAgyCredits：agy -p /credits 的結果 → 點數餘額說明；≤0 或讀不到回空字串
func parseAgyCredits(res cliResult) string {
	if res.exitCode != 0 {
		return ""
	}
	cmd := jobj(agyJSONPayload(res.stdout), "command")
	if cmd == nil || !strings.EqualFold(jstr(cmd, "name"), "credits") {
		return ""
	}
	d := jobj(cmd, "data")
	for _, k := range []string{"remaining_credits", "remainingCredits", "balance"} {
		if v, ok := d[k]; ok {
			n, ok := jnum(v)
			if s, isStr := v.(string); isStr {
				_, err := fmt.Sscanf(s, "%g", &n)
				ok = err == nil
			}
			if ok && n > 0 {
				return "AI 點數餘額 " + fmtNum2(n)
			}
			return ""
		}
	}
	return ""
}

func probeAgyAccount(in probeInput) probeResult {
	var r probeResult
	if in.cliPath == "" {
		r.setStatus("notInstalled", "")
		return r
	}
	work := ensureDir(filepath.Join(in.cacheDir, "agy-empty"))
	logDir := ensureDir(filepath.Join(in.cacheDir, "agy-log"))
	var res cliResult
	email := ""
	for attempt := 0; attempt < 2; attempt++ {
		logFile := filepath.Join(logDir, "usage-"+newID("")+".log")
		res = runTool(in.ctx, in.cliPath, []string{"--log-file", logFile, "-p", "/usage", "--output-format", "json"}, in.env, work, in.timeout)
		if b, err := os.ReadFile(logFile); err == nil {
			email = parseAgySignedInEmail(string(b))
		}
		_ = os.Remove(logFile)
		if !res.timedOut || in.ctx.Err() != nil {
			break
		}
	}
	if in.ctx.Err() != nil {
		return probeResult{}
	}
	st, data, errText := evaluateAgyUsage(res)
	r.email = email
	if st == "error" {
		r.setStatus("error", "檢查失敗："+errText)
		r.stale(errText)
		return r
	}
	r.setStatus(st, "")
	if st != "ready" {
		return r
	}
	if data == nil {
		r.stale(errText)
		return r
	}
	windows, note, ok := parseAntigravityUsage(data)
	if !ok {
		r.stale("用量回應的格式無法解析")
		return r
	}
	credits := ""
	for _, w := range windows {
		if w.Locked {
			// 只有額度窗耗盡鎖定時才補查一次 AI 點數餘額（避免每次輪詢多起一個 language server）
			credits = parseAgyCredits(runTool(in.ctx, in.cliPath, []string{"-p", "/credits", "--output-format", "json"}, in.env, work, in.timeout))
			break
		}
	}
	r.usage = &usageSnapshot{FetchedAt: nowMs(), Source: "api", Windows: windows, Credits: credits, Note: note}
	return r
}

// ---- Grok ----

// evaluateGrokAuthInfo：_x.ai/auth/info → 登入狀態：有信箱＝已登入；沒有信箱但有 API 類的登入方式＝API Key；都沒有＝未登入
func evaluateGrokAuthInfo(info map[string]any) (string, string) {
	if e := strings.TrimSpace(jstr(info, "email")); e != "" {
		return "ready", e
	}
	if strings.Contains(strings.ToLower(jstr(info, "methodId")), "api") {
		return "apiKey", ""
	}
	return "notLoggedIn", ""
}

func probeGrokAccount(in probeInput) probeResult {
	var r probeResult
	if in.cliPath == "" {
		r.setStatus("notInstalled", "")
		return r
	}
	c, _, err := startGrokAgent(in.ctx, in.cliPath, in.env, ensureDir(in.cacheDir))
	if err != nil {
		if in.ctx.Err() != nil {
			return probeResult{}
		}
		r.setStatus("error", "檢查失敗："+err.Error())
		return r
	}
	defer c.close()
	info, err := c.callMap(in.ctx, "_x.ai/auth/info", map[string]any{}, 20*time.Second)
	if in.ctx.Err() != nil {
		return probeResult{}
	}
	if err != nil {
		r.setStatus("error", "檢查失敗："+err.Error())
		return r
	}
	st, email := evaluateGrokAuthInfo(info)
	r.email = email
	if st != "ready" {
		r.setStatus(st, "")
		return r
	}
	billing, err := c.callMap(in.ctx, "_x.ai/billing", map[string]any{}, 30*time.Second)
	if in.ctx.Err() != nil {
		return probeResult{}
	}
	if grokAuthRequired(err) {
		// 有信箱但權杖已失效、refresh 也失敗（例如在別處登出）
		r.setStatus("notLoggedIn", "")
		return r
	}
	r.setStatus("ready", "")
	if tier := strings.TrimSpace(jstr(billing, "subscription_tier")); tier != "" {
		r.plan = tier
	}
	if windows, note, ok := parseGrokBilling(billing); ok && err == nil {
		r.usage = &usageSnapshot{FetchedAt: nowMs(), Source: "api", Windows: windows, Note: note}
	} else if err != nil {
		r.stale(err.Error())
	} else {
		r.stale("用量回應的格式無法解析")
	}
	return r
}

func ensureDir(d string) string {
	if err := os.MkdirAll(d, 0o700); err != nil {
		log.Printf("建立資料夾失敗：%v", err)
	}
	return d
}
