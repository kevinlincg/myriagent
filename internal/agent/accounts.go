package agent

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var providerKeys = []string{"Claude", "Codex", "Antigravity", "Grok"}

var providerNames = map[string]string{"Claude": "Claude Code", "Codex": "Codex", "Antigravity": "Antigravity", "Grok": "Grok Build"}

var providerExecutables = map[string]string{"Claude": "claude", "Codex": "codex", "Antigravity": "agy", "Grok": "grok"}

// 目前接上的代理種類（其餘的之後移植；清單照 C# 的 AgentProviders.All 順序）。登記了 Provider（registerProvider）的種類也算
var implementedProviders = map[string]bool{"Claude": true, "Codex": true, "Antigravity": true, "Grok": true}

var systemDefaultAppearance = map[string][2]string{
	"Claude": {"Claude（本機）", "#D97757"}, "Codex": {"Codex（本機）", "#19C3A6"},
	"Antigravity": {"Antigravity（本機）", "#7C8CF8"}, "Grok": {"Grok（本機）", "#A3B83A"},
}

// Account 是 accounts.json 的一筆（C# 的 AgentAccount）。ConfigDir 空＝這台的預設資料夾（~/.claude、~/.codex…，或繼承的
// CLAUDE_CONFIG_DIR／CODEX_HOME／GROK_HOME）；否則是 app 建立的隔離資料夾（accounts/{id}/claude…）或使用者原有的資料夾（ExistingFolder）
type Account struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	Label           string `json:"label"`
	Color           string `json:"color"`
	IsSystemDefault bool   `json:"isSystemDefault"`
	ConfigDir       string `json:"configDir,omitempty"` // 空＝這台的預設資料夾
	// ExistingFolder：使用者原有的設定資料夾（accountAdd 帶 directory）：不共用預設帳戶的設定、移除時絕不刪
	ExistingFolder bool `json:"existingFolder,omitempty"`
	// SafeMode：精簡模式（不載入外掛、hooks、MCP、CLAUDE.md）
	SafeMode bool `json:"safeMode,omitempty"`
	// ShareCustom：隔離帳戶共用系統預設帳戶的設定；沒有這個欄位＝預設 true（shares）
	ShareCustom *bool `json:"shareCustomizations,omitempty"`
	// 上次查到的 email 與方案（狀態還在檢查時先顯示）
	SavedEmail string `json:"email,omitempty"`
	SavedPlan  string `json:"plan,omitempty"`
	// LastIdentity：Claude 最近一次 claude auth status 成功時的結果與當時憑證檔的樣子（不含權杖）
	LastIdentity *identityCache `json:"lastIdentity,omitempty"`
	CreatedAt    int64          `json:"createdAt,omitempty"`

	// 執行期間才有
	email    string
	plan     string
	status   string
	models   []Option
	commands []Command
	rt       accountRuntime
}

// accountRuntime：帳戶的執行期狀態（不存檔；都在 h.mu 底下改）
type accountRuntime struct {
	statusText      string
	loginBusy       bool
	resetCreditBusy bool
	loginCancel     context.CancelFunc
	usage           *usageSnapshot
	nextPoll        time.Time
	soonPoll        time.Time
	errorCount      int
	refreshing      bool
	again           bool   // 查詢期間又有人要求重查
	againStatus     bool   // 那次要連登入狀態一起查
	againReason     string // 那次的理由
	refreshGen      int    // 每做完一次查詢加一
	credSig         string // 上次看到的憑證檔大小與修改時間（變了就重查）
	accessExpiresAt int64
	loginExpiresAt  int64
	lifetime        context.Context
	cancel          context.CancelFunc
	removed         bool
	waiters         []chan struct{} // 等下一次查詢做完的
}

// shares：隔離帳戶要不要共用系統預設帳戶的設定（沒有這個欄位時是 true）
func (a *Account) shares() bool { return a.ShareCustom == nil || *a.ShareCustom }

type cliInfo struct {
	path    string
	version string
	cli     cliState
}

// ---- CLI ----

// searchPaths：launchd／systemd 啟動時 PATH 很短，另外找常見的安裝位置（Claude 官方安裝在 ~/.local/bin、Grok 在 ~/.grok/bin）
func searchPaths() []string {
	home, _ := os.UserHomeDir()
	dirs := filepath.SplitList(os.Getenv("PATH"))
	dirs = append(dirs,
		filepath.Join(home, ".local", "bin"), filepath.Join(home, ".claude", "local"), filepath.Join(home, ".grok", "bin"),
		filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, "bin"),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin")
	return dirs
}

func findExecutable(name string) string {
	for _, d := range searchPaths() {
		if d == "" {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func (h *Hub) providerAvailable(p string) bool { return implementedProviders[p] || providers[p] != nil }

func (h *Hub) providerViews() []ProviderView {
	out := []ProviderView{}
	for _, p := range providerKeys {
		if !h.providerAvailable(p) {
			continue
		}
		out = append(out, ProviderView{
			Key: p, Name: providerNames[p], IsolatedAccounts: supportsIsolatedAccounts(p), SafeMode: supportsSafeMode(p),
			SharedCustomizations: supportsSharedCustomizations(p), ExistingFolders: h.existingFolderViews(p),
		})
	}
	return out
}

// ---- 帳戶清單 ----

func accountsFile(root string) string { return filepath.Join(root, "accounts.json") }

// accountsDir：app 建立的隔離帳戶的資料夾（accounts/{id}/…）
func (h *Hub) accountsDir() string { return filepath.Join(h.root, "accounts") }

// loadAccounts：讀 accounts.json。不存在或損毀時以本機預設帳戶＋從資料夾找回 app 建立過的隔離帳戶重建（損毀的原檔另存備份）；
// 每種接上的代理都補上本機預設帳戶（固定 Id local-claude 等）
func (h *Hub) loadAccounts() {
	var list []*Account
	readOnly := false
	b, err := os.ReadFile(accountsFile(h.root))
	switch {
	case err == nil && json.Unmarshal(b, &list) == nil:
	case err == nil:
		backup := accountsFile(h.root) + ".bad-" + time.Now().Format("20060102-150405")
		_ = os.WriteFile(backup, b, 0o600)
		log.Printf("帳戶清單已損毀（原檔備份為 %s），從資料夾還原", backup)
		list = h.recoverIsolatedAccounts()
	case os.IsNotExist(err):
		list = h.recoverIsolatedAccounts()
	default:
		// 讀不到（權限、被占用）：不能用預設清單蓋掉原檔；這次先用預設帳戶執行、不存檔
		log.Printf("讀不到帳戶清單（這次不存檔）：%v", err)
		readOnly = true
		list = h.recoverIsolatedAccounts()
	}
	clean := list[:0]
	for _, a := range list {
		if a != nil && a.ID != "" && a.Provider != "" {
			clean = append(clean, a)
		}
	}
	list = clean
	for _, p := range providerKeys {
		if !h.providerAvailable(p) {
			continue
		}
		found := false
		for _, a := range list {
			if a.Provider == p && a.IsSystemDefault {
				found = true
			}
		}
		if !found {
			list = append(list, newSystemDefault(p))
		}
	}
	h.accounts = list
	if !readOnly {
		h.saveAccounts()
	}
	for _, a := range h.accounts {
		a.email, a.plan = a.SavedEmail, a.SavedPlan
		if a.status == "" {
			a.status = "checking"
		}
		h.prepareIsolatedFolder(a)
		h.refreshAccount(a)
	}
}

func newSystemDefault(p string) *Account {
	ap := systemDefaultAppearance[p]
	return &Account{ID: systemDefaultID(p), Provider: p, Label: ap[0], Color: ap[1], IsSystemDefault: true, CreatedAt: nowMs()}
}

// recoverIsolatedAccounts：帳戶清單遺失或損毀時，從 accounts/{id}/claude、codex… 資料夾找回 app 建立過的隔離帳戶
// （登入憑證還在資料夾裡，只是名稱與顏色要重設）；留有待刪標記的資料夾不算
func (h *Hub) recoverIsolatedAccounts() []*Account {
	var out []*Account
	for _, id := range listNames(h.accountsDir()) {
		dir := filepath.Join(h.accountsDir(), id)
		if !isAccountFolderName(id) || !dirExists(dir) || pathExists(dir+pendingDeleteSuffix) {
			continue
		}
		for _, p := range providerKeys {
			if !supportsIsolatedAccounts(p) {
				continue
			}
			config := filepath.Join(dir, isolatedFolderName(p))
			if !dirExists(config) {
				continue
			}
			out = append(out, &Account{ID: id, Provider: p, Label: providerShortName(p) + " 帳戶（還原 " + id[:6] + "）",
				Color: recoveredColor(p), ConfigDir: config, CreatedAt: nowMs()})
			break
		}
	}
	return out
}

func (h *Hub) saveAccounts() {
	for _, a := range h.accounts {
		if a.email != "" {
			a.SavedEmail = a.email
		}
		if a.plan != "" {
			a.SavedPlan = a.plan
		}
	}
	writeJSON(accountsFile(h.root), h.accounts)
}

func (h *Hub) account(id string) *Account {
	for _, a := range h.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// configDirectory：畫面上顯示的設定資料夾（各家的預設位置不同）
func (a *Account) configDirectory() string { return a.resolveConfigDir() }

// claudeConfigDir：帳戶的 Claude 設定資料夾（預設帳戶是 ~/.claude 或繼承的 CLAUDE_CONFIG_DIR）
func (a *Account) claudeConfigDir() string { return a.resolveConfigDir() }

// prepareIsolatedFolder：隔離帳戶的資料夾要先放好的設定：Grok 的 config.toml 標記「Claude 設定已匯入過」（grok 才不會讀使用者的 ~/.claude）；
// agy 隔離帳戶的家（鑰匙圈、連結、shell 小腳本）。使用者原有的資料夾不動
func (h *Hub) prepareIsolatedFolder(a *Account) {
	if a.IsSystemDefault || a.ExistingFolder || a.ConfigDir == "" {
		return
	}
	switch a.Provider {
	case "Grok":
		ensureGrokIsolatedHome(a.ConfigDir)
	case "Antigravity":
		if a.agyShadowed() {
			prepareAgyHome(a.ConfigDir)
		}
	}
}

// refreshAccount：先以讀檔快速判斷狀態（各家 Provider 的 RefreshAccount），完整的檢查與用量由帳戶的查詢排程（accountmgr.go）做
func (h *Hub) refreshAccount(a *Account) {
	if p := providers[a.Provider]; p != nil {
		p.RefreshAccount(h, a)
	} else if a.status == "" {
		a.status = "checking"
	}
}

// refreshClaudeAccount：Claude 的快速判斷（不跑 CLI）：沒安裝；CLI 設定檔有 oauthAccount＝已登入；有身分快取就沿用；否則未登入。
// 完整的檢查做過之後就不再以它覆蓋
func refreshClaudeAccount(h *Hub, a *Account) {
	if a.Provider != "Claude" {
		a.status = "notInstalled"
		return
	}
	if h.clis["Claude"] == nil || h.clis["Claude"].path == "" {
		a.status = "notInstalled"
		return
	}
	if a.rt.refreshGen > 0 {
		return
	}
	legacy, config := a.claudeGlobalConfigPaths()
	path := config
	if pathExists(legacy) {
		path = legacy
	}
	b, _ := os.ReadFile(path)
	g := parseClaudeGlobalConfig(string(b))
	switch {
	case g.hasOAuth && g.oauthEmail != "":
		a.email = g.oauthEmail
		a.status = "ready"
	case a.LastIdentity != nil && (a.LastIdentity.Status == "ready" || a.LastIdentity.Status == "apiKey"):
		a.status = a.LastIdentity.Status
		if a.LastIdentity.Email != "" {
			a.email = a.LastIdentity.Email
		}
	default:
		a.status = "notLoggedIn"
	}
}

// statusText：帳戶卡的狀態說明（查詢給的說明優先，沒有就照狀態）
func (a *Account) statusText() string {
	if a.rt.statusText != "" {
		return a.rt.statusText
	}
	return describeAccountStatus(a.Provider, a.status)
}

func (h *Hub) accountViews() []AccountView {
	out := []AccountView{}
	for _, a := range h.accounts {
		v := AccountView{
			ID: a.ID, Provider: a.Provider, Label: a.Label, Color: a.Color, IsSystemDefault: a.IsSystemDefault,
			ExistingFolder: a.ExistingFolder, ConfigDirectory: a.resolveConfigDir(), Status: a.status, StatusText: a.statusText(),
			SafeMode:            a.SafeMode && supportsSafeMode(a.Provider),
			ShareCustomizations: a.shares() && !a.IsSystemDefault && !a.ExistingFolder && supportsSharedCustomizations(a.Provider),
			LoginBusy:           a.rt.loginBusy,
			ResetCreditBusy:     a.rt.resetCreditBusy,
			Usage:               h.usageView(a),
		}
		if a.email != "" {
			e := a.email
			v.Email = &e
		}
		if a.plan != "" {
			p := a.plan
			v.Plan = &p
		}
		if a.Provider == "Claude" && a.status == "ready" && a.rt.loginExpiresAt > 0 {
			e := a.rt.loginExpiresAt
			v.LoginExpiresAt = &e
		}
		for _, c := range h.convs {
			if c.Meta.AccountID != a.ID || c.Meta.IsDraft {
				continue
			}
			if h.alive(c) {
				v.Sessions.Open++
			}
			if c.running {
				v.Sessions.Running++
			}
			if c.waiting() {
				v.Sessions.Waiting++
			}
			if h.hasPrograms(c) {
				v.Sessions.Programs++
			}
		}
		if v.Sessions.Programs > 0 {
			v.Programs = h.accountPrograms(a.ID)
		}
		out = append(out, v)
	}
	return out
}

// ---- 模型與權限模式 ----

func permissionModes(provider string) []Option {
	if modes, ok := extraPermissionModes[provider]; ok {
		return modes
	}
	switch provider {
	case "Codex":
		return []Option{
			{Value: "Ask", Label: "每步詢問", Description: "沙箱內執行，每個指令先問你"},
			{Value: "Auto", Label: "自動", Description: "沙箱內自動執行，需要更多權限時才問"},
			{Value: "FullAccess", Label: "完全自動", Description: "完全存取、一律放行"},
		}
	default:
		return []Option{
			{Value: "Ask", Label: "每步詢問", Description: "執行指令、修改檔案前先問你"},
			{Value: "AcceptEdits", Label: "自動接受編輯", Description: "修改檔案與 mkdir、rm、mv 等檔案指令不再詢問，其他指令仍會問"},
			{Value: "Plan", Label: "規劃模式", Description: "只讀取與分析，先提出計畫給你確認"},
			{Value: "Auto", Label: "自動", Description: "由代理判斷，高風險操作才問"},
			{Value: "FullAccess", Label: "完全自動", Description: "一律放行，不再詢問"},
		}
	}
}

// extraPermissionModes：各家自己登記的權限模式選單（registerPermissionModes；沒登記的照上面的 switch）
var extraPermissionModes = map[string][]Option{}

func registerPermissionModes(provider string, modes []Option) { extraPermissionModes[provider] = modes }

func claudePermission(mode string) string {
	switch mode {
	case "AcceptEdits":
		return "acceptEdits"
	case "Plan":
		return "plan"
	case "Auto":
		return "auto"
	case "FullAccess":
		return "bypassPermissions"
	default:
		return "default"
	}
}

func fromClaudePermission(mode string) string {
	switch mode {
	case "acceptEdits":
		return "AcceptEdits"
	case "plan":
		return "Plan"
	case "auto":
		return "Auto"
	case "bypassPermissions":
		return "FullAccess"
	case "default":
		return "Ask"
	}
	return ""
}

var defaultModelOption = Option{Value: "default", Label: "預設（建議）", Description: "帳戶預設模型"}

// accountModels：帳戶的 CLI 回報過的模型（沒回報過只有「預設」）
func (a *Account) modelOptions() []Option {
	if len(a.models) > 0 {
		return a.models
	}
	return []Option{defaultModelOption}
}

// providerModels：這一家各帳戶回報的聯集（最前面一定有 default）
func (h *Hub) providerModels(provider string) []Option {
	seen := map[string]bool{}
	out := []Option{}
	for _, a := range h.accounts {
		if a.Provider != provider {
			continue
		}
		for _, m := range a.modelOptions() {
			if !seen[m.Value] {
				seen[m.Value] = true
				out = append(out, m)
			}
		}
	}
	if !seen["default"] {
		out = append([]Option{defaultModelOption}, out...)
	}
	return out
}

// parseInitialize：initialize 回應的 commands、models、account
func (a *Account) applyInitialize(resp map[string]any) {
	if cmds, ok := resp["commands"].([]any); ok {
		list := []Command{}
		for _, c := range cmds {
			m, _ := c.(map[string]any)
			name := str(m, "name")
			if name == "" {
				continue
			}
			list = append(list, Command{Name: name, Description: str(m, "description"), ArgumentHint: str(m, "argumentHint")})
		}
		a.commands = list
	}
	if models, ok := resp["models"].([]any); ok {
		list := []Option{}
		for _, x := range models {
			m, _ := x.(map[string]any)
			value := str(m, "value")
			if value == "" {
				continue
			}
			o := Option{Value: value, Label: firstNonBlank(str(m, "displayName"), value), Description: str(m, "description")}
			if efforts, ok := m["supportedEffortLevels"].([]any); ok {
				for _, e := range efforts {
					if s, ok := e.(string); ok {
						o.Efforts = append(o.Efforts, s)
					}
				}
			}
			list = append(list, o)
		}
		if len(list) > 0 {
			hasDefault := false
			for _, o := range list {
				if o.Value == "default" {
					hasDefault = true
				}
			}
			if !hasDefault {
				list = append([]Option{defaultModelOption}, list...)
			}
			a.models = list
		}
	}
	if acc, ok := resp["account"].(map[string]any); ok {
		if e := str(acc, "email"); e != "" {
			a.email = e
			a.status = "ready"
		}
		if p := str(acc, "subscriptionType"); p != "" {
			a.plan = planName(p)
		}
	}
}

func planName(s string) string {
	switch strings.ToLower(s) {
	case "max":
		return "Claude Max"
	case "pro":
		return "Claude Pro"
	case "team":
		return "Claude Team"
	case "enterprise":
		return "Claude Enterprise"
	}
	return s
}

var _ = time.Now
