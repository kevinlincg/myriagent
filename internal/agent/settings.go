package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings：settings.json 的內容（docs/web-protocol.md 的 Settings）。以 map 存，送給網頁時補上預設值。
// Layout 另外放（網頁擁有、宿主只負責保存）。
type Settings struct {
	Values map[string]any `json:"values"`
	Layout any            `json:"layout"`
	// 每個帳戶上次選的模型（setOption model 記下的）
	LastModel map[string]string `json:"lastModel"`
	// 新對話預設用的帳戶（上次用的）
	LastAccount string `json:"lastAccount"`
	// 最近用過的資料夾（新的在前）
	RecentFolders []string `json:"recentFolders"`
}

var settingDefaults = map[string]any{
	"theme":                       "dark",
	"fontSize":                    14,
	"visualEffects":               true,
	"closeToTray":                 true,
	"autoStart":                   nil,
	"autoStartLock":               nil,
	"autoLogon":                   nil,
	"notifications":               true,
	"voiceNotifications":          false,
	"voiceSpeed":                  1.5,
	"voicePitch":                  2,
	"scheduleAutoDiagnose":        nil,
	"autoUpdateCli":               false,
	"autoUpdateApp":               false,
	"cliRestartPolicy":            "SafePoint",
	"enterToSend":                 true,
	"claudeRemoteControl":         false,
	"claudeFallbackModel":         nil,
	"claudeIdleTokenRefresh":      false,
	"claudeIdleResumeWarning":     true,
	"autoResumeAfterQuota":        false,
	"autoSwitchAccount":           false,
	"autoSwitchCrossProvider":     false,
	"shareCrossCliCustomizations": false,
	"instructions":                "",
	"continueText":                "進度如何呢如果還沒做完就繼續",
}

// 網頁可以改的鍵與型別（不在這裡的回 invalid）
var settingKinds = map[string]string{
	"theme": "theme", "fontSize": "number", "visualEffects": "bool", "closeToTray": "bool", "notifications": "bool",
	"voiceNotifications": "bool", "voiceSpeed": "number", "voicePitch": "number", "autoUpdateCli": "bool", "autoUpdateApp": "bool",
	"cliRestartPolicy": "string", "enterToSend": "bool", "claudeRemoteControl": "bool", "claudeFallbackModel": "string",
	"claudeIdleTokenRefresh": "bool", "claudeIdleResumeWarning": "bool", "autoResumeAfterQuota": "bool", "autoSwitchAccount": "bool",
	"autoSwitchCrossProvider": "bool", "shareCrossCliCustomizations": "bool", "instructions": "string", "continueText": "string",
}

func settingsFile(root string) string { return filepath.Join(root, "settings.json") }

func loadSettings(root string) Settings {
	s := Settings{Values: map[string]any{}, LastModel: map[string]string{}}
	if b, err := os.ReadFile(settingsFile(root)); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Values == nil {
		s.Values = map[string]any{}
	}
	if s.LastModel == nil {
		s.LastModel = map[string]string{}
	}
	return s
}

func (h *Hub) saveSettings() {
	writeJSON(settingsFile(h.root), h.settings)
}

func (h *Hub) setting(key string) any {
	if v, ok := h.settings.Values[key]; ok {
		return v
	}
	return settingDefaults[key]
}

func (h *Hub) settingBool(key string) bool {
	b, _ := h.setting(key).(bool)
	return b
}

func (h *Hub) themeAttr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, _ := h.setting("theme").(string); t == "light" {
		return "light"
	}
	return "dark"
}

// settingsView：送給網頁的 Settings（預設值＋使用者改過的＋新對話的預設）
func (h *Hub) settingsView() map[string]any {
	out := map[string]any{}
	for k, v := range settingDefaults {
		out[k] = v
	}
	for k, v := range h.settings.Values {
		out[k] = v
	}
	defaults := map[string]any{}
	for _, p := range providerKeys {
		if !h.providerAvailable(p) {
			continue
		}
		d := h.conversationDefault(p)
		defaults[p] = map[string]any{
			"model":                   d.Model,
			"permission":              d.Permission,
			"permissionModes":         permissionModes(p),
			"models":                  h.providerModels(p),
			"autoCompact":             nil,
			"autoCompactRange":        nil,
			"autoCompactPercent":      nil,
			"autoCompactPercentRange": nil,
		}
	}
	out["conversationDefaults"] = defaults
	return out
}

type convDefault struct {
	Model      *string           `json:"model"`
	Permission string            `json:"permission"`
	Effort     map[string]string `json:"effort"`
}

func (h *Hub) conversationDefault(provider string) convDefault {
	d := convDefault{Permission: "Ask"}
	if raw, ok := h.settings.Values["conversationDefaults"].(map[string]any); ok {
		if pm, ok := raw[provider].(map[string]any); ok {
			if m, ok := pm["model"].(string); ok {
				d.Model = &m
			}
			if p, ok := pm["permission"].(string); ok && p != "" {
				d.Permission = p
			}
		}
	}
	return d
}

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}
