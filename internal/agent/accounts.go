package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var providerKeys = []string{"Claude", "Codex", "Antigravity", "Grok"}

var providerNames = map[string]string{"Claude": "Claude Code", "Codex": "Codex", "Antigravity": "Antigravity", "Grok": "Grok Build"}

var providerExecutables = map[string]string{"Claude": "claude", "Codex": "codex", "Antigravity": "agy", "Grok": "grok"}

// 目前接上的代理種類（其餘的之後移植；清單照 C# 的 AgentProviders.All 順序）
var implementedProviders = map[string]bool{"Claude": true}

var systemDefaultAppearance = map[string][2]string{
	"Claude": {"Claude（本機）", "#D97757"}, "Codex": {"Codex（本機）", "#19C3A6"},
	"Antigravity": {"Antigravity（本機）", "#7C8CF8"}, "Grok": {"Grok（本機）", "#A3B83A"},
}

// Account 是 accounts.json 的一筆
type Account struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	Label           string `json:"label"`
	Color           string `json:"color"`
	IsSystemDefault bool   `json:"isSystemDefault"`
	ConfigDir       string `json:"configDir,omitempty"` // 空＝這台的預設資料夾

	// 執行期間才有
	email    string
	plan     string
	status   string
	models   []Option
	commands []Command
}

type cliInfo struct {
	path    string
	version string
}

// ---- CLI ----

// searchPaths：launchd／systemd 啟動時 PATH 很短，另外找常見的安裝位置
func searchPaths() []string {
	home, _ := os.UserHomeDir()
	dirs := filepath.SplitList(os.Getenv("PATH"))
	dirs = append(dirs,
		filepath.Join(home, ".local", "bin"), filepath.Join(home, ".claude", "local"),
		filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, "bin"),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin")
	return dirs
}

func findExecutable(name string) string {
	for _, d := range searchPaths() {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func (h *Hub) detectCLIs() {
	for _, p := range providerKeys {
		path := findExecutable(providerExecutables[p])
		info := &cliInfo{path: path}
		if path != "" {
			if out, err := exec.Command(path, "--version").Output(); err == nil {
				info.version = strings.TrimSpace(strings.Fields(string(out) + " ")[0])
			}
		}
		h.clis[p] = info
	}
}

func (h *Hub) providerAvailable(p string) bool { return implementedProviders[p] }

func (h *Hub) cliViews() map[string]CliView {
	out := map[string]CliView{}
	for _, p := range providerKeys {
		c := h.clis[p]
		v := CliView{Status: "notInstalled"}
		if c != nil && c.path != "" {
			path := c.path
			v.Installed, v.Path, v.Status = true, &path, "ready"
			if c.version != "" {
				ver := c.version
				v.Version = &ver
			}
		}
		out[p] = v
	}
	return out
}

func (h *Hub) providerViews() []ProviderView {
	out := []ProviderView{}
	for _, p := range providerKeys {
		if !h.providerAvailable(p) {
			continue
		}
		out = append(out, ProviderView{Key: p, Name: providerNames[p], IsolatedAccounts: p != "Antigravity", SafeMode: false, SharedCustomizations: false, ExistingFolders: []any{}})
	}
	return out
}

// ---- 帳戶 ----

func accountsFile(root string) string { return filepath.Join(root, "accounts.json") }

func (h *Hub) loadAccounts() {
	var list []*Account
	if b, err := os.ReadFile(accountsFile(h.root)); err == nil {
		_ = json.Unmarshal(b, &list)
	}
	// 每種接上的代理都有一個這台的預設帳戶
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
			ap := systemDefaultAppearance[p]
			list = append(list, &Account{ID: newID("a"), Provider: p, Label: ap[0], Color: ap[1], IsSystemDefault: true})
		}
	}
	h.accounts = list
	h.saveAccounts()
	for _, a := range h.accounts {
		h.refreshAccount(a)
	}
}

func (h *Hub) saveAccounts() { writeJSON(accountsFile(h.root), h.accounts) }

func (h *Hub) account(id string) *Account {
	for _, a := range h.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// claudeConfigDir：帳戶的 Claude 設定資料夾（預設帳戶是 ~/.claude）
func (a *Account) claudeConfigDir() string {
	if a.ConfigDir != "" {
		return a.ConfigDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// refreshAccount 讀帳戶的登入狀態（Claude：設定檔裡的 oauthAccount）
func (h *Hub) refreshAccount(a *Account) {
	if p := providers[a.Provider]; p != nil {
		p.RefreshAccount(h, a)
		return
	}
	a.status = "notInstalled"
}

func refreshClaudeAccount(h *Hub, a *Account) {
	switch a.Provider {
	case "Claude":
		if h.clis["Claude"] == nil || h.clis["Claude"].path == "" {
			a.status = "notInstalled"
			return
		}
		home, _ := os.UserHomeDir()
		cfg := filepath.Join(home, ".claude.json")
		if a.ConfigDir != "" {
			cfg = filepath.Join(a.ConfigDir, ".claude.json")
		}
		var doc struct {
			OauthAccount struct {
				EmailAddress     string `json:"emailAddress"`
				OrganizationType string `json:"organizationType"`
				BillingType      string `json:"billingType"`
			} `json:"oauthAccount"`
		}
		if b, err := os.ReadFile(cfg); err == nil {
			_ = json.Unmarshal(b, &doc)
		}
		if doc.OauthAccount.EmailAddress != "" {
			a.email = doc.OauthAccount.EmailAddress
			a.status = "ready"
		} else {
			a.status = "notLoggedIn"
		}
	default:
		a.status = "notInstalled"
	}
}

var statusText = map[string]string{"checking": "檢查中…", "ready": "已登入", "notLoggedIn": "尚未登入", "apiKey": "使用 API 金鑰", "notInstalled": "沒有安裝", "error": "發生錯誤"}

func (h *Hub) accountViews() []AccountView {
	out := []AccountView{}
	for _, a := range h.accounts {
		v := AccountView{
			ID: a.ID, Provider: a.Provider, Label: a.Label, Color: a.Color, IsSystemDefault: a.IsSystemDefault,
			ConfigDirectory: a.claudeConfigDir(), Status: a.status, StatusText: statusText[a.status],
		}
		if a.email != "" {
			e := a.email
			v.Email = &e
		}
		if a.plan != "" {
			p := a.plan
			v.Plan = &p
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
		}
		out = append(out, v)
	}
	return out
}

// ---- 模型與權限模式 ----

func permissionModes(provider string) []Option {
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
