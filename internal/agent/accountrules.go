package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// 帳戶的純函式規則（照 C# 的 Accounts/AccountRules.cs）：子程序環境變數、設定資料夾與憑證檔位置、登入狀態判讀、
// 方案名稱、預設顏色與名稱、輪詢的退避。這裡不碰中樞的狀態，測試直接呼叫。

// accountPalette：帳戶代表色調色盤（畫面上的選色也用這組）
var accountPalette = []string{"#D97757", "#E5A04B", "#E06C75", "#C678DD", "#7C8CF8", "#4FA3F7", "#19C3A6", "#3FB950", "#A3B83A", "#9AA4B2"}

// providerShortNames：給使用者看的產品名稱（帳戶名稱、提示文字）
var providerShortNames = map[string]string{"Claude": "Claude", "Codex": "Codex", "Antigravity": "Antigravity", "Grok": "Grok"}

func providerShortName(p string) string {
	if n := providerShortNames[p]; n != "" {
		return n
	}
	return p
}

// systemDefaultID：本機預設帳戶的固定 Id：帳戶清單遺失或損毀後重建時沿用同一個 Id，既有對話、用量歷史與模型設定才接得回來
func systemDefaultID(p string) string { return "local-" + strings.ToLower(p) }

// isWindows 可在測試換掉（Go 版只在 macOS／Linux 跑，留著是為了照 C# 的規則寫清楚）
var isWindows = runtime.GOOS == "windows"

// supportsIsolatedAccounts：能不能新增隔離帳戶。Windows 的 Antigravity 登入存在認證管理員的固定名稱，一台只能一個；
// macOS／Linux 的 agy 登入跟著 HOME 走，每個帳戶給它自己的家（antigravityhome.go）
func supportsIsolatedAccounts(p string) bool {
	switch p {
	case "Claude", "Codex", "Grok":
		return true
	case "Antigravity":
		return !isWindows
	}
	return false
}

// supportsSafeMode：精簡模式（Grok 沒有「不載入使用者外掛與 MCP」的單一旗標，不提供）
func supportsSafeMode(p string) bool { return p == "Claude" || p == "Codex" || p == "Antigravity" }

// supportsSharedCustomizations：隔離帳戶能不能共用系統預設帳戶的設定（Antigravity 沒有這個開關）
func supportsSharedCustomizations(p string) bool { return p == "Claude" || p == "Codex" || p == "Grok" }

// describeAccountStatus：狀態的說明文字（AccountView.statusText）
func describeAccountStatus(p, status string) string {
	switch status {
	case "checking":
		return "檢查中…"
	case "ready":
		return "已登入"
	case "notLoggedIn":
		switch p {
		case "Claude":
			return "尚未登入 Claude 帳戶"
		case "Codex":
			return "尚未登入 ChatGPT 帳戶"
		case "Antigravity":
			return "尚未登入 Antigravity（Google 帳戶）"
		case "Grok":
			return "尚未登入 Grok（grok.com 帳戶）"
		}
		return "尚未登入"
	case "apiKey":
		return "以 API Key 登入，會走 API 計費而不是訂閱額度"
	case "notInstalled":
		switch p {
		case "Claude":
			return "這台電腦尚未安裝 Claude Code CLI"
		case "Codex":
			return "這台電腦尚未安裝 Codex CLI"
		case "Antigravity":
			return "這台電腦尚未安裝 Antigravity CLI"
		case "Grok":
			return "這台電腦尚未安裝 Grok Build CLI"
		}
		return "沒有安裝"
	}
	return "檢查失敗"
}

// defaultAccountLabel：新帳戶的預設名稱：「Claude 帳戶」「Claude 帳戶 2」；同類已有幾個就往後編
func defaultAccountLabel(p string, existing []*Account) string {
	name := providerShortName(p)
	used := map[string]bool{}
	for _, a := range existing {
		used[strings.ToLower(a.Label)] = true
	}
	for i := 1; i < 1000; i++ {
		label := name + " 帳戶"
		if i > 1 {
			label = name + " 帳戶 " + itoa(i)
		}
		if !used[strings.ToLower(label)] {
			return label
		}
	}
	return truncateRunes(name+" 帳戶 "+newID(""), 20)
}

// defaultAccountColor：新帳戶的預設顏色：Claude 暖色、Codex 冷色、Antigravity 藍紫色、Grok 黃綠色，避開已用過的
func defaultAccountColor(p string, existing []*Account) string {
	used := map[string]bool{}
	for _, a := range existing {
		used[strings.ToUpper(a.Color)] = true
	}
	var preferred []string
	switch p {
	case "Claude":
		preferred = []string{"#D97757", "#E5A04B", "#E06C75", "#C678DD"}
	case "Codex":
		preferred = []string{"#19C3A6", "#4FA3F7", "#3FB950", "#7C8CF8"}
	case "Antigravity":
		preferred = []string{"#7C8CF8", "#C678DD", "#4FA3F7", "#3FB950"}
	default:
		preferred = []string{"#A3B83A", "#3FB950", "#E5A04B", "#9AA4B2"}
	}
	for _, c := range append(append([]string{}, preferred...), accountPalette...) {
		if !used[c] {
			return c
		}
	}
	return preferred[0]
}

var colorPattern = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// normalizeColor：只接受 #RRGGBB（轉大寫）；其他回空字串
func normalizeColor(c string) string {
	c = strings.TrimSpace(c)
	if !colorPattern.MatchString(c) {
		return ""
	}
	return strings.ToUpper(c)
}

// cleanAccountLabel：本機輸入的名稱去頭尾空白、最多 40 字
func cleanAccountLabel(s string) string { return truncateRunes(strings.TrimSpace(s), 40) }

// recoveredColor：從資料夾還原的帳戶用第二順位的顏色，和本機預設帳戶區分
func recoveredColor(p string) string {
	switch p {
	case "Claude":
		return "#E5A04B"
	case "Codex":
		return "#4FA3F7"
	case "Antigravity":
		return "#C678DD"
	}
	return "#3FB950"
}

// isolatedFolderName：隔離帳戶的設定資料夾在 accounts/{id}/ 底下的名稱
func isolatedFolderName(p string) string {
	switch p {
	case "Claude":
		return "claude"
	case "Codex":
		return "codex"
	case "Grok":
		return "grok"
	case "Antigravity":
		return agyHomeFolder
	}
	return strings.ToLower(p)
}

// isAccountFolderName：app 建立的帳戶資料夾名稱（帳戶 Id：32 碼十六進位）
func isAccountFolderName(name string) bool {
	if len(name) != 32 {
		return false
	}
	for _, r := range name {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// newAccountID：隔離帳戶的 Id（32 碼十六進位，同時是資料夾名稱）
func newAccountID() string { return newID("") + newID("") }

// ---- 設定資料夾 ----

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// builtInConfigDir：這種 CLI 自己預設的設定資料夾（沒有設 CLAUDE_CONFIG_DIR／CODEX_HOME／GROK_HOME 時用的那個）
func builtInConfigDir(p, home string) string {
	switch p {
	case "Claude":
		return filepath.Join(home, ".claude")
	case "Codex":
		return filepath.Join(home, ".codex")
	case "Antigravity":
		return filepath.Join(home, ".gemini", "antigravity-cli")
	case "Grok":
		return filepath.Join(home, ".grok")
	}
	return ""
}

// configHomeVariable：指定設定資料夾的環境變數（Antigravity 沒有：換的是整個 HOME）
func configHomeVariable(p string) string {
	switch p {
	case "Claude":
		return "CLAUDE_CONFIG_DIR"
	case "Codex":
		return "CODEX_HOME"
	case "Grok":
		return "GROK_HOME"
	}
	return ""
}

// resolveConfigDir：帳戶實際使用的設定資料夾。系統預設帳戶沿用繼承的 CLAUDE_CONFIG_DIR／CODEX_HOME／GROK_HOME（使用者自己設的），
// 沒設時是 CLI 自己的預設位置；Antigravity 的隔離帳戶是它自己的家裡的那一個
func (a *Account) resolveConfigDir() string {
	if a.Provider == "Antigravity" {
		if a.agyShadowed() {
			return filepath.Join(a.ConfigDir, ".gemini", "antigravity-cli")
		}
		return builtInConfigDir("Antigravity", userHome())
	}
	if !a.IsSystemDefault && a.ConfigDir != "" {
		return a.ConfigDir
	}
	if v := configHomeVariable(a.Provider); v != "" {
		if inherited := strings.TrimSpace(os.Getenv(v)); inherited != "" {
			return inherited
		}
	}
	return builtInConfigDir(a.Provider, userHome())
}

// agyShadowed：這個帳戶的 agy 用自己的家（macOS／Linux 的隔離帳戶）
func (a *Account) agyShadowed() bool { return agyShadowed(a) && !a.ExistingFolder }

// credentialFile：登入憑證檔（Claude 的 .credentials.json、Codex 與 Grok 的 auth.json；監看它的變化以重新檢查狀態與用量）。
// Antigravity 的登入在鑰匙圈，沒有檔案可監看
func (a *Account) credentialFile() string {
	switch a.Provider {
	case "Claude":
		return filepath.Join(a.resolveConfigDir(), ".credentials.json")
	case "Codex", "Grok":
		return filepath.Join(a.resolveConfigDir(), "auth.json")
	}
	return ""
}

// claudeGlobalConfigPaths：Claude CLI 的全域設定檔（CLI 2.1.296：設定資料夾裡舊的 .config.json 存在就用它，否則是
// CLAUDE_CONFIG_DIR（沒設時是使用者資料夾）底下的 .claude.json）
func (a *Account) claudeGlobalConfigPaths() (legacy, config string) {
	dir := a.resolveConfigDir()
	home := userHome()
	if !a.IsSystemDefault && a.ConfigDir != "" {
		home = a.ConfigDir
	} else if inherited := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); inherited != "" {
		home = inherited
	}
	return filepath.Join(dir, ".config.json"), filepath.Join(home, ".claude.json")
}

// ---- 子程序環境 ----

// secretVariables：永遠不傳給子程序的變數（API Key／Token 會改走 API 計費而不是訂閱）
var secretVariables = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"}

// antigravitySecretVariables：agy 程序另外剝除的（Gemini API Key 改走 API 計費、GOOGLE_GEMINI_BASE_URL 轉到別的端點）
var antigravitySecretVariables = []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GEMINI_BASE_URL"}

// grokSecretVariables：grok 程序另外剝除的（xAI API Key、指向別處登入與設定的、別人的 leader）
var grokSecretVariables = []string{"XAI_API_KEY", "GROK_AUTH_PATH", "GROK_CONFIG", "GROK_CONFIG_PATH", "GROK_LEADER_SOCKET"}

// credentialVariables：開終端機登入時一律拿掉的（終端機的 shell 有使用者自己 export 的，不在 app 的環境裡）
func credentialVariables() []string {
	out := append([]string{}, secretVariables...)
	out = append(out, antigravitySecretVariables...)
	return append(out, grokSecretVariables...)
}

// envMap 把 KEY=VALUE 清單轉成 map（後面的蓋前面的）
func envMap(list []string) map[string]string {
	m := map[string]string{}
	for _, kv := range list {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// applyToolEnv：不分帳戶的 CLI 呼叫（查版本、更新）：剝除 API Key、父 Claude session 帶來的 CLAUDE*（保留 CLAUDE_CONFIG_DIR、
// CLAUDE_CODE_GIT_BASH_PATH）、父 agy 工作階段的 ANTIGRAVITY_*、背景程式的標記
func applyToolEnv(env map[string]string) {
	for k := range env {
		u := strings.ToUpper(k)
		if containsFold(secretVariables, k) ||
			(strings.HasPrefix(u, "CLAUDE") && u != "CLAUDE_CONFIG_DIR" && u != "CLAUDE_CODE_GIT_BASH_PATH") ||
			strings.HasPrefix(u, "ANTIGRAVITY_") || u == "MYRIAGENT_AGENT_CONVERSATION" {
			delete(env, k)
		}
	}
}

// applyAccountEnv：帳戶的子程序環境：剝除 API Key 與父工作階段的變數，再依帳戶指定設定資料夾。
// 系統預設帳戶保留繼承的 CLAUDE_CONFIG_DIR／CODEX_HOME／GROK_HOME（使用者自己設的）。
// Antigravity 另剝除 Gemini API Key 類變數並關掉背景自動更新，隔離帳戶以它自己的家當 HOME；
// Grok 另剝除 xAI API Key 與指向別處設定的變數，一律關掉背景自動更新（換版由 app 協調），隔離帳戶以 GROK_HOME 指定資料夾
func applyAccountEnv(a *Account, env map[string]string) {
	applyToolEnv(env)
	switch a.Provider {
	case "Antigravity":
		for k := range env {
			if containsFold(antigravitySecretVariables, k) {
				delete(env, k)
			}
		}
		env["AGY_CLI_DISABLE_AUTO_UPDATE"] = "true"
	case "Grok":
		for k := range env {
			if containsFold(grokSecretVariables, k) {
				delete(env, k)
			}
		}
		env["GROK_DISABLE_AUTOUPDATER"] = "1"
	}
	if a.IsSystemDefault || a.ConfigDir == "" {
		return
	}
	switch a.Provider {
	case "Antigravity":
		if a.agyShadowed() {
			set, unset := agyShadowEnv(a.ConfigDir, func(k string) string { return env[k] })
			for k, v := range set {
				env[k] = v
			}
			for _, k := range unset {
				delete(env, k)
			}
		}
	default:
		if v := configHomeVariable(a.Provider); v != "" {
			env[v] = a.ConfigDir
		}
	}
}

// applyCommandEnv：app 自己跑的短命 CLI 子指令（--version、update、auth status、登出）：帳戶環境（a 為 nil＝不分帳戶）之外
// 另帶 DISABLE_AUTOUPDATER=1（app 啟動的 claude 一律不在背景自動更新）
func applyCommandEnv(a *Account, env map[string]string) {
	if a == nil {
		applyToolEnv(env)
	} else {
		applyAccountEnv(a, env)
	}
	env["DISABLE_AUTOUPDATER"] = "1"
}

// commandEnv：以這個程序的環境為底，套上 applyCommandEnv
func commandEnv(a *Account) []string {
	env := envMap(os.Environ())
	applyCommandEnv(a, env)
	return envList(env)
}

// ---- Claude 登入狀態 ----

// claudeTokenState：憑證檔裡 OAuth 權杖的狀況
const (
	tokenNone          = "none"          // 沒有 OAuth 權杖：沒有東西可換發
	tokenFresh         = "fresh"         // 距到期還有 10 分鐘以上
	tokenExpiring      = "expiring"      // 已過期或 10 分鐘內到期：短命的 claude 會開始換發卻不等結果
	tokenMissingExpiry = "missingExpiry" // 有權杖卻沒有 expiresAt
	tokenEmpty         = "empty"         // accessToken 是空字串：CLI 換發被拒後清空的登入（登入已失效）
	tokenUnreadable    = "unreadable"    // 內容讀不懂（CLI 可能正在寫入）
)

// claudeAuthStatusExpiryMargin：權杖剩下不到這麼久就不跑短命的 claude 子指令（實測 CLI 在剩不到約 5 分鐘時就會開始換發權杖）
const claudeAuthStatusExpiryMargin = 10 * time.Minute

func claudeOAuth(credentialsJSON string) (map[string]any, bool) {
	var doc map[string]any
	if json.Unmarshal([]byte(credentialsJSON), &doc) != nil {
		return nil, false
	}
	oauth, _ := doc["claudeAiOauth"].(map[string]any)
	return oauth, true
}

// inspectClaudeToken：只讀 accessToken 有沒有值與 expiresAt，不看權杖內容
func inspectClaudeToken(credentialsJSON string, now time.Time) string {
	if strings.TrimSpace(credentialsJSON) == "" {
		return tokenNone
	}
	oauth, ok := claudeOAuth(credentialsJSON)
	if !ok {
		return tokenUnreadable
	}
	if oauth == nil {
		return tokenNone
	}
	token, has := oauth["accessToken"]
	if !has || token == nil {
		return tokenNone
	}
	if s, ok := token.(string); !ok || strings.TrimSpace(s) == "" {
		return tokenEmpty
	}
	exp, ok := oauth["expiresAt"].(float64)
	if !ok {
		return tokenMissingExpiry
	}
	if time.UnixMilli(int64(exp)).Add(-claudeAuthStatusExpiryMargin).After(now) {
		return tokenFresh
	}
	return tokenExpiring
}

// readClaudeExpiresAt：憑證檔 claudeAiOauth.expiresAt（毫秒）；沒有回 0
func readClaudeExpiresAt(credentialsJSON string) int64 {
	if oauth, ok := claudeOAuth(credentialsJSON); ok && oauth != nil {
		if v, ok := oauth["expiresAt"].(float64); ok {
			return int64(v)
		}
	}
	return 0
}

// readClaudeLoginExpiresAt：憑證檔 claudeAiOauth.refreshTokenExpiresAt（毫秒；登入的到期，只是參考值）；沒有回 0
func readClaudeLoginExpiresAt(credentialsJSON string) int64 {
	if oauth, ok := claudeOAuth(credentialsJSON); ok && oauth != nil {
		if v, ok := oauth["refreshTokenExpiresAt"].(float64); ok && v > 0 {
			return int64(v)
		}
	}
	return 0
}

// readClaudePlanFromCredentials：憑證檔的方案資訊（不讀權杖）
func readClaudePlanFromCredentials(credentialsJSON string) string {
	oauth, ok := claudeOAuth(credentialsJSON)
	if !ok || oauth == nil {
		return ""
	}
	sub, _ := oauth["subscriptionType"].(string)
	tier, _ := oauth["rateLimitTier"].(string)
	return describeClaudePlan(sub, tier)
}

var tierPattern = regexp.MustCompile(`(\d+)x$`)

// describeClaudePlan：subscriptionType（max／pro／team…）＋ rateLimitTier（default_claude_max_20x → 20x）
func describeClaudePlan(subscriptionType, rateLimitTier string) string {
	key := strings.TrimSpace(subscriptionType)
	if len(key) >= 7 && strings.EqualFold(key[:7], "Claude ") {
		key = strings.TrimSpace(key[7:])
	}
	if key == "" {
		return ""
	}
	var name string
	switch strings.ToLower(key) {
	case "max":
		name = "Max"
	case "pro":
		name = "Pro"
	case "team":
		name = "Team"
	case "enterprise":
		name = "Enterprise"
	case "free":
		name = "Free"
	default:
		name = strings.ToUpper(key[:1]) + key[1:]
	}
	if m := tierPattern.FindStringSubmatch(rateLimitTier); m != nil {
		return name + " " + m[1] + "x"
	}
	return name
}

// claudeCredentials：憑證檔的樣子（只讀大小、修改時間與內容）。exists 但 json 為空＝讀不到（被占用）
type claudeCredentials struct {
	exists  bool
	size    int64
	modTime int64 // UnixNano；鑰匙圈來的是 0
	json    string
}

// claudeGlobalConfig：CLI 全域設定檔裡和登入有關的部分
type claudeGlobalConfig struct {
	readable    bool
	credMan     bool // cachedGrowthBookFeatures.tengu_windows_credman（Windows 才有，macOS／Linux 一律 false）
	hasOAuth    bool
	oauthEmail  string
	oauthBilled string
}

// parseClaudeGlobalConfig：只看最上層的 cachedGrowthBookFeatures 與 oauthAccount。空白＝讀不到內容；格式不對＝讀不懂
func parseClaudeGlobalConfig(text string) claudeGlobalConfig {
	if strings.TrimSpace(text) == "" {
		return claudeGlobalConfig{}
	}
	var doc struct {
		Features map[string]any `json:"cachedGrowthBookFeatures"`
		OAuth    *struct {
			Email string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal([]byte(text), &doc) != nil {
		return claudeGlobalConfig{}
	}
	c := claudeGlobalConfig{readable: true}
	if b, ok := doc.Features["tengu_windows_credman"].(bool); ok && b {
		c.credMan = true
	}
	if doc.OAuth != nil {
		c.hasOAuth = true
		c.oauthEmail = strings.TrimSpace(doc.OAuth.Email)
	}
	return c
}

// claudeStatusReason：為什麼要確認登入狀態（決定能不能用身分快取）
const (
	reasonStartup     = "startup"
	reasonPeriodic    = "periodic"
	reasonAfterTurn   = "afterTurn"
	reasonCredentials = "credentialsChanged"
	reasonManual      = "manual"
)

func strongerReason(a, b string) string {
	rank := map[string]int{reasonManual: 4, reasonCredentials: 3, reasonAfterTurn: 2, reasonStartup: 1}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// claudeStatusDecision：要不要跑 claude auth status
const (
	decideRunAuthStatus  = "runAuthStatus"
	decideUseCache       = "useCache"
	decideTokenPending   = "tokenPending"
	decideLoginExpired   = "loginExpired"
	decideConservative   = "conservative"
	decideCredUnreadable = "credentialsUnreadable"
	decideConfigUnread   = "configUnreadable"
)

// identityCache：claude auth status 成功時的身分與憑證檔的樣子（存進 accounts.json，不含權杖）
type identityCache struct {
	Status    string `json:"status"`
	Email     string `json:"email,omitempty"`
	Plan      string `json:"plan,omitempty"`
	CredSize  int64  `json:"credentialSize"`
	CredMod   int64  `json:"credentialModified"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	CheckedAt int64  `json:"checkedAt"`
}

// identityCacheValid：曾經以 auth status 確認登入（ready 或 apiKey），而且憑證檔的大小、修改時間、expiresAt 都和那時一樣
func identityCacheValid(c *identityCache, cred claudeCredentials) bool {
	return c != nil && (c.Status == "ready" || c.Status == "apiKey") && cred.exists &&
		c.CredSize == cred.size && c.CredMod == cred.modTime && c.ExpiresAt == readClaudeExpiresAt(cred.json)
}

// decideClaudeStatusCheck：要不要跑 claude auth status 來確認登入狀態（C# 的設計 6.5）。順序：
//  1. 憑證檔不存在：看 CLI 全域設定。讀不到＝無法判斷，不跑；啟用了認證管理員而且還有 OAuth 登入＝保守模式；其他照常跑
//  2. 權杖是空字串＝登入已失效，不跑 CLI
//  3. 憑證檔讀不懂＝稍後再查；快到期、沒有 expiresAt＝等權杖更新，不跑（auth status 會開始換發卻不等結果，可能把 refresh token 用掉）
//  4. 沒有 OAuth 權杖（API Key 等）＝照常跑
//  5. 權杖新鮮：啟動、定期、對話結束後而且快取有效＝用快取；其他＝跑
func decideClaudeStatusCheck(cred claudeCredentials, cache *identityCache, reason string, now time.Time, global *claudeGlobalConfig) string {
	if !cred.exists {
		if global == nil || !global.readable {
			return decideConfigUnread
		}
		if global.credMan && global.hasOAuth {
			return decideConservative
		}
		return decideRunAuthStatus
	}
	if cred.json == "" {
		return decideCredUnreadable
	}
	switch inspectClaudeToken(cred.json, now) {
	case tokenEmpty:
		return decideLoginExpired
	case tokenUnreadable:
		return decideCredUnreadable
	case tokenExpiring, tokenMissingExpiry:
		return decideTokenPending
	case tokenNone:
		return decideRunAuthStatus
	}
	if (reason == reasonStartup || reason == reasonPeriodic || reason == reasonAfterTurn) && identityCacheValid(cache, cred) {
		return decideUseCache
	}
	return decideRunAuthStatus
}

// findJSONObject：從輸出裡找第一個含指定欄位的完整 JSON 物件（前後夾雜的警告、stderr 都略過，即使它們也有大括號）
func findJSONObject(text, required string) map[string]any {
	for start := strings.IndexByte(text, '{'); start >= 0; {
		dec := json.NewDecoder(strings.NewReader(text[start:]))
		var m map[string]any
		if dec.Decode(&m) == nil {
			if _, ok := m[required]; ok {
				return m
			}
		}
		next := strings.IndexByte(text[start+1:], '{')
		if next < 0 {
			break
		}
		start += next + 1
	}
	return nil
}

// evaluateClaudeAuthStatus：claude auth status --json 的輸出 → 狀態、email、方案。authMethod＝claude.ai 才是訂閱；
// 只有讀到含 loggedIn 的 JSON 物件才下判斷，讀不到回 error（這是這次檢查失敗，不能當成未登入）
func evaluateClaudeAuthStatus(output string) (status, email, plan string) {
	m := findJSONObject(output, "loggedIn")
	if m == nil {
		return "error", "", ""
	}
	if b, _ := m["loggedIn"].(bool); !b {
		return "notLoggedIn", "", ""
	}
	email, _ = m["email"].(string)
	sub, _ := m["subscriptionType"].(string)
	method, _ := m["authMethod"].(string)
	plan = describeClaudePlan(sub, "")
	if plan == "" {
		plan = method
	}
	if strings.EqualFold(method, "claude.ai") {
		return "ready", email, plan
	}
	return "apiKey", email, plan
}

// describeClaudeAuthStatusFailure：claude auth status 沒有讀到結果時給人看的原因
func describeClaudeAuthStatusFailure(stderr string, timedOut bool) string {
	if timedOut {
		return "claude auth status 執行逾時"
	}
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return truncateEllipsis(l, 160)
		}
	}
	return "讀不到 claude auth status 的結果"
}

func truncateEllipsis(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ---- Codex ----

// evaluateCodexAccount：Codex account/read 的結果 → 狀態、email、方案
func evaluateCodexAccount(result map[string]any) (status, email, plan string) {
	acc, _ := result["account"].(map[string]any)
	if acc == nil {
		return "notLoggedIn", "", ""
	}
	switch t, _ := acc["type"].(string); t {
	case "chatgpt":
		email, _ = acc["email"].(string)
		pt, _ := acc["planType"].(string)
		return "ready", email, describeCodexPlan(pt)
	case "apiKey":
		return "apiKey", "", "API Key"
	default:
		return "apiKey", "", t
	}
}

func describeCodexPlan(p string) string {
	switch p {
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
	return p
}

// ---- 輪詢 ----

// 輪詢的間隔：對話在跑的帳戶比較密
func activePoll(p string) time.Duration {
	if p == "Claude" {
		return 2 * time.Minute
	}
	return 5 * time.Minute
}

func idlePoll(p string) time.Duration {
	if p == "Claude" {
		return 10 * time.Minute
	}
	return 15 * time.Minute
}

// shouldRecheckStatusOnTick：定期輪詢時要不要一併重查登入狀態：未登入、未安裝（在外部登入或安裝 CLI 後跟得上），以及上次檢查失敗
func shouldRecheckStatusOnTick(status string) bool {
	return status == "notLoggedIn" || status == "notInstalled" || status == "error"
}

// initialErrorRetryDelay：檢查失敗後第一次自動重試的間隔，之後每次加倍，不超過帳戶原本的輪詢間隔
const initialErrorRetryDelay = time.Minute

// nextPollDelay：查詢結束後到下一次輪詢的間隔：正常照原本的間隔；檢查失敗時依連續失敗次數退避（1 → 2 → 4 分鐘…，上限為原本的間隔）
func nextPollDelay(status string, consecutiveErrors int, normal time.Duration) time.Duration {
	if status != "error" {
		return normal
	}
	shift := consecutiveErrors - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 10 {
		shift = 10
	}
	backoff := initialErrorRetryDelay * time.Duration(1<<shift)
	if backoff < normal {
		return backoff
	}
	return normal
}

// deletableAccountRoot：可以刪的帳戶資料夾：只限 app 建立的 accounts/{帳戶 Id}（上一層必須正好是 accounts 資料夾、名稱必須是帳戶 Id）；
// 系統預設帳戶、使用者原有的資料夾、位置不符一律不刪（回空字串）
func deletableAccountRoot(a *Account, accountsDir string) string {
	if a.IsSystemDefault || a.ExistingFolder || a.ConfigDir == "" {
		return ""
	}
	config, err := filepath.Abs(strings.TrimRight(a.ConfigDir, "/"))
	if err != nil {
		return ""
	}
	root := filepath.Dir(config)
	dir, err := filepath.Abs(accountsDir)
	if err != nil {
		return ""
	}
	if filepath.Clean(filepath.Dir(root)) != filepath.Clean(dir) || filepath.Base(root) != a.ID || !isAccountFolderName(a.ID) {
		return ""
	}
	return root
}
