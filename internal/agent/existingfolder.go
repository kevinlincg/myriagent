package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 「使用現有的設定資料夾」的規則（照 C# 的 Accounts/ExistingFolderRules.cs）：使用者在終端機以 CLAUDE_CONFIG_DIR=~/.claude-work 之類的方式
// 分開的帳戶，直接把那個資料夾加成帳戶——登入、Skills、外掛、記憶、CLI 對話紀錄都是同一份。app 不建立、不搬動，移除帳戶時也絕不刪。

const (
	folderOK              = ""
	folderUnsupported     = "unsupported"
	folderEmpty           = "empty"
	folderNotAbsolute     = "notAbsolute"
	folderMissing         = "missing"
	folderForbidden       = "forbidden"
	folderNotConfigFolder = "notConfigFolder"
	folderIsDefault       = "isDefault"
	folderInUse           = "inUse"
)

// existingFolderSupported：能建隔離帳戶的種類，但 Antigravity 不行（agy 的登入跟著整個 HOME 走，不是一個設定資料夾）
func existingFolderSupported(p string) bool { return p != "Antigravity" && supportsIsolatedAccounts(p) }

// cleanFolderInput：去掉頭尾的空白與引號、開頭的 ~ 換成使用者資料夾、完整路徑寫成正規的樣子、去掉結尾的分隔符號。
// 寫成正規的樣子是為了讓 app 啟動的 CLI 與 app 自己讀登入（macOS 的鑰匙圈項目名稱由 CLAUDE_CONFIG_DIR 的字面算出）看到同一個字串
func cleanFolderInput(input, home string) string {
	text := strings.TrimSpace(strings.Trim(strings.TrimSpace(input), `"'`))
	if text == "" {
		return ""
	}
	if text == "~" {
		text = home
	} else if strings.HasPrefix(text, "~/") {
		rest := strings.TrimLeft(text[2:], "/")
		if rest == "" {
			text = home
		} else {
			text = filepath.Join(home, rest)
		}
	}
	if filepath.IsAbs(text) {
		text = filepath.Clean(text)
	}
	if trimmed := strings.TrimRight(text, "/"); trimmed != "" {
		return trimmed
	}
	return text
}

// sameFolder：兩個路徑是不是同一個資料夾（不分大小寫、不管結尾的分隔符號；macOS 預設的檔案系統不分大小寫）
func sameFolder(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// checkExistingFolder：這個資料夾（已經過 cleanFolderInput）能不能加成帳戶
func checkExistingFolder(p, dir string, dirExists func(string) bool, defaults, used, forbidden []string, looksLikeConfig func(string) bool) string {
	if !existingFolderSupported(p) {
		return folderUnsupported
	}
	if strings.TrimSpace(dir) == "" {
		return folderEmpty
	}
	if !filepath.IsAbs(dir) {
		return folderNotAbsolute
	}
	if !dirExists(dir) {
		return folderMissing
	}
	for _, d := range defaults {
		if sameFolder(d, dir) {
			return folderIsDefault
		}
	}
	for _, d := range used {
		if sameFolder(d, dir) {
			return folderInUse
		}
	}
	if filepath.Clean(dir) == "/" {
		return folderForbidden
	}
	for _, d := range forbidden {
		if sameFolder(d, dir) {
			return folderForbidden
		}
	}
	if !looksLikeConfig(dir) {
		return folderNotConfigFolder
	}
	return folderOK
}

// describeFolderCheck：檢查不過時給使用者看的說明
func describeFolderCheck(check, p, dir string) string {
	name := providerShortName(p)
	switch check {
	case folderUnsupported:
		return name + " 一台電腦只有一個登入，不能另外指定設定資料夾"
	case folderEmpty:
		return "請輸入設定資料夾的路徑"
	case folderNotAbsolute:
		return "請輸入完整的路徑：" + dir
	case folderMissing:
		return "找不到這個資料夾：" + dir
	case folderForbidden:
		return "這個資料夾不能當成 " + name + " 的設定資料夾：" + dir
	case folderNotConfigFolder:
		return "這個資料夾看起來不是 " + name + " 的設定資料夾（裡面有別的東西，卻沒有 " + name + " 用過的痕跡）：" + dir
	case folderIsDefault:
		return "這是 " + name + " 預設的設定資料夾，「本機預設」帳戶用的就是它：" + dir
	case folderInUse:
		return "已經有帳戶在用這個資料夾：" + dir
	}
	return ""
}

// configFolderPrefix：設定資料夾名稱的開頭（.claude、.codex、.grok）；沒有隔離帳戶的 CLI 回空字串
func configFolderPrefix(p string) string {
	switch p {
	case "Claude":
		return ".claude"
	case "Codex":
		return ".codex"
	case "Grok":
		return ".grok"
	}
	return ""
}

// isCandidateFolderName：.claude 本身，或 .claude 後面接 -、_、. 的（.claude-work）；.claudex 之類只是開頭相同的不算
func isCandidateFolderName(p, name string) bool {
	prefix := configFolderPrefix(p)
	if prefix == "" || len(name) < len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
		return false
	}
	return len(name) == len(prefix) || strings.ContainsRune("-_.", rune(name[len(prefix)]))
}

// configFolderMarkers：CLI 用過的設定資料夾才會有的東西
func configFolderMarkers(p string) []string {
	switch p {
	case "Claude":
		return []string{".claude.json", ".credentials.json", "projects"}
	case "Codex", "Grok":
		return []string{"auth.json", "config.toml", "sessions"}
	}
	return nil
}

func hasConfigMarker(p, dir string, exists func(string) bool) bool {
	for _, m := range configFolderMarkers(p) {
		if exists(filepath.Join(dir, m)) {
			return true
		}
	}
	return false
}

// findExistingFolders：使用者資料夾底下可以直接加成帳戶的設定資料夾：名稱像、有 CLI 用過的痕跡、不是預設帳戶的、還沒有帳戶在用
func findExistingFolders(p string, homeDirs []string, exists func(string) bool, defaults, used []string) []string {
	if !existingFolderSupported(p) {
		return nil
	}
	excluded := append(append([]string{}, defaults...), used...)
	var out []string
outer:
	for _, d := range homeDirs {
		if !isCandidateFolderName(p, filepath.Base(d)) {
			continue
		}
		for _, x := range excluded {
			if sameFolder(x, d) {
				continue outer
			}
		}
		if hasConfigMarker(p, d, exists) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// existingFolderLabel：~/.claude-work → 「Claude work」；沒有、或同名的帳戶已經有了就照一般的編號
func existingFolderLabel(p, dir string, existing []*Account) string {
	name := filepath.Base(dir)
	if prefix := configFolderPrefix(p); prefix != "" && isCandidateFolderName(p, name) {
		rest := strings.Trim(name[len(prefix):], "-_. ")
		if rest != "" {
			label := truncateRunes(providerShortName(p)+" "+rest, 40)
			dup := false
			for _, a := range existing {
				if strings.EqualFold(a.Label, label) {
					dup = true
				}
			}
			if !dup {
				return label
			}
		}
	}
	return defaultAccountLabel(p, existing)
}

// displayFolder：使用者資料夾底下的寫成 ~/…
func displayFolder(dir, home string) string {
	home = strings.TrimRight(home, "/")
	if home != "" && len(dir) > len(home) && strings.EqualFold(dir[:len(home)], home) && dir[len(home)] == '/' {
		return "~" + dir[len(home):]
	}
	return dir
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// looksLikeConfigFolder：資料夾裡有這種 CLI 用過的痕跡，或是空的（空的可以當成新的設定資料夾）。讀不到內容當成不是
func looksLikeConfigFolder(p, dir string) bool {
	if hasConfigMarker(p, dir, pathExists) {
		return true
	}
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

// defaultConfigDirs：「本機預設」帳戶用的資料夾：CLI 自己的預設位置，以及這次啟動實際用的（繼承的環境變數）
func defaultConfigDirs(p, home string) []string {
	return []string{builtInConfigDir(p, home), (&Account{Provider: p, IsSystemDefault: true}).resolveConfigDir()}
}

// forbiddenConfigDirs：使用者資料夾本身、app 自己的資料夾、別種 CLI 的設定資料夾
func forbiddenConfigDirs(p, home, appRoot string) []string {
	out := []string{home, appRoot}
	for _, other := range providerKeys {
		if other != p {
			out = append(out, defaultConfigDirs(other, home)...)
		}
	}
	return out
}

// usedConfigDirs：清單裡這種 CLI 的帳戶實際用的資料夾
func (h *Hub) usedConfigDirs(p string) []string {
	var out []string
	for _, a := range h.accounts {
		if a.Provider == p {
			out = append(out, a.resolveConfigDir())
		}
	}
	return out
}

// existingFolderViews：ProviderView.existingFolders（只在 init 送）
func (h *Hub) existingFolderViews(p string) []any {
	out := []any{}
	home := userHome()
	entries, err := os.ReadDir(home)
	if err != nil {
		return out
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(home, e.Name()))
		}
	}
	for _, d := range findExistingFolders(p, dirs, pathExists, defaultConfigDirs(p, home), h.usedConfigDirs(p)) {
		out = append(out, map[string]any{"path": d, "display": displayFolder(d, home)})
	}
	return out
}
