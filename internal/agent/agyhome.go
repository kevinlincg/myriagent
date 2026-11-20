package agent

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Antigravity 的隔離帳戶（照 C# 版的 AntigravityHomeRules 與 Myriagent.Mac 的 AntigravityHome）：agy 的登入存在系統鑰匙圈的固定名稱，
// 設定在 ~/.gemini/antigravity-cli——兩者都跟著 HOME 走（鑰匙圈預設是 $HOME/Library/Keychains/login.keychain-db）。
// 所以每個隔離帳戶給 agy 一個自己的「家」（帳戶的設定資料夾 agy-home）：裡面有自己的鑰匙圈與 .gemini/antigravity-cli，
// 其他東西全部連結回真正的家（.gitconfig、.ssh、~/.agents 的 skills、~/.gemini 其他設定都照舊）。
// agy 開的 shell 由 ZDOTDIR／BASH_ENV 指到的小腳本把 HOME 換回真正的家，代理跑的指令（gh、git 走鑰匙圈的登入…）和平常一樣。

const (
	agyHomeFolder        = "agy-home"         // 帳戶資料夾裡 agy 的「家」
	agyShellFolder       = ".myriagent-shell" // 家裡放 shell 小腳本的資料夾（ZDOTDIR 指到這裡）
	agyBashEnvFile       = "bashenv"
	agyOriginalZdotdir   = "MYRIAGENT_ORIGINAL_ZDOTDIR"
	agyOriginalBashEnv   = "MYRIAGENT_ORIGINAL_BASH_ENV"
	agyHomeRefreshPeriod = 30 * time.Second
)

// agyMirrored：要連結回真正的家的資料夾層級與那一層自己保留的項目
var agyMirrored = []struct {
	folder string
	own    []string
}{
	{"", []string{"Library", ".gemini", agyShellFolder}},
	{"Library", []string{"Keychains"}},
	{".gemini", []string{"antigravity-cli"}},
}

// agySharedConfig：antigravity-cli 裡和真正的家共用的項目（有才連結）。登入、對話、記憶各自一份
var agySharedConfig = []string{"settings.json", "skills", "plugins", "workflows", "agents", "GEMINI.md", "mcp_config.json"}

// agyShadowed：這個帳戶的 agy 用自己的家（隔離帳戶；Windows 沒有，這一版也不在 Windows 跑）
func agyShadowed(a *Account) bool {
	return a.Provider == "Antigravity" && !a.IsSystemDefault && a.ConfigDir != "" && runtime.GOOS != "windows"
}

// agyConfigDir：家裡 agy 的設定資料夾（.gemini/antigravity-cli）
func agyConfigDir(home string) string { return filepath.Join(home, ".gemini", "antigravity-cli") }

func agyKeychainPath(home string) string {
	return filepath.Join(home, "Library", "Keychains", "login.keychain-db")
}

// agyPlanLinks：家裡該有的連結（連結路徑 → 真正的家裡的目標）。每一層自己保留的項目不連結；antigravity-cli 只連結 agySharedConfig 裡真的存在的
func agyPlanLinks(home, realHome string, list func(string) []string) [][2]string {
	var links [][2]string
	for _, m := range agyMirrored {
		src, dst := filepath.Join(realHome, m.folder), filepath.Join(home, m.folder)
		names := list(src)
		sort.Strings(names)
		for _, n := range names {
			if contains(m.own, n) {
				continue
			}
			links = append(links, [2]string{filepath.Join(dst, n), filepath.Join(src, n)})
		}
	}
	realCfg := agyConfigDir(realHome)
	present := list(realCfg)
	for _, n := range agySharedConfig {
		if contains(present, n) {
			links = append(links, [2]string{filepath.Join(agyConfigDir(home), n), filepath.Join(realCfg, n)})
		}
	}
	return links
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// agyShadowEnv：agy 程序的環境：HOME 換成它的家，shell 的小腳本（zsh 讀 ZDOTDIR 的 .zshenv、非互動的 bash 讀 BASH_ENV）把 HOME 換回來；
// 使用者原本設的 ZDOTDIR／BASH_ENV 另外帶著，小腳本換回去。get 讀目前的環境變數
func agyShadowEnv(home string, get func(string) string) (set map[string]string, unset []string) {
	shell := filepath.Join(home, agyShellFolder)
	set = map[string]string{}
	keep := func(name, original, ours string) {
		v := get(name)
		if v == ours {
			// 已經是自己的小腳本（套過一次）：之前記下的原值留著
			if o := get(original); o != "" {
				set[original] = o
			}
			return
		}
		if v != "" {
			set[original] = v
		} else {
			unset = append(unset, original)
		}
	}
	keep("ZDOTDIR", agyOriginalZdotdir, shell)
	keep("BASH_ENV", agyOriginalBashEnv, filepath.Join(shell, agyBashEnvFile))
	set["HOME"] = home
	set["ZDOTDIR"] = shell
	set["BASH_ENV"] = filepath.Join(shell, agyBashEnvFile)
	return set, unset
}

func agyShellHeader(realHome string) string {
	restore := func(name, original string) string {
		return "if [ -n \"${" + original + ":-}\" ]; then export " + name + "=\"$" + original + "\"; else unset " + name + "; fi\n"
	}
	return "# Myriagent：這個 agy 帳戶的 HOME 是它自己的家（登入與設定各自一份）；agy 開的 shell 換回真正的家，指令照平常執行\n" +
		"export HOME=" + shellQuote(realHome) + "\n" +
		restore("ZDOTDIR", agyOriginalZdotdir) +
		restore("BASH_ENV", agyOriginalBashEnv) +
		"unset " + agyOriginalZdotdir + " " + agyOriginalBashEnv + "\n"
}

// agyZshEnvScript：家/.myriagent-shell/.zshenv：換回真正的家、還原 ZDOTDIR／BASH_ENV，再讀使用者自己的 .zshenv
func agyZshEnvScript(realHome string) string {
	return agyShellHeader(realHome) + "if [ -f \"${ZDOTDIR:-$HOME}/.zshenv\" ]; then . \"${ZDOTDIR:-$HOME}/.zshenv\"; fi\n"
}

// agyBashEnvScript：家/.myriagent-shell/bashenv：換回真正的家、還原 ZDOTDIR／BASH_ENV，再讀使用者自己的 BASH_ENV
func agyBashEnvScript(realHome string) string {
	return agyShellHeader(realHome) + "if [ -n \"${BASH_ENV:-}\" ] && [ -f \"$BASH_ENV\" ]; then . \"$BASH_ENV\"; fi\n"
}

// shellQuote：單引號包起來給 sh（裡面的單引號寫成 '\”）
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var (
	agyHomeMu   sync.Mutex
	agyHomeLast = map[string]time.Time{}
)

// prepareAgyHome：每次要跑這個帳戶的 agy 之前整理好它的家——自己的鑰匙圈（macOS：沒有就建立、空密碼、不自動上鎖，每次解鎖）、
// shell 小腳本、連回真正的家的連結（真正的家多了東西就補、少了就拿掉）。最多每 30 秒整理一次。失敗只記錄：agy 照樣啟動，看起來就是沒登入
func prepareAgyHome(home string) {
	agyHomeMu.Lock()
	defer agyHomeMu.Unlock()
	if t, ok := agyHomeLast[home]; ok && time.Since(t) < agyHomeRefreshPeriod {
		return
	}
	// 家或要整理的那幾層是連結（不是 app 建的）就整個不動，免得列到真正的家去刪連結
	for _, f := range agyOwnFolders(home) {
		if isSymlink(f) {
			log.Printf("agy 帳戶的資料夾被換成連結，不整理：%s", home)
			return
		}
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if err := os.MkdirAll(agyConfigDir(home), 0o700); err != nil {
		log.Printf("整理 agy 帳戶的資料夾失敗（%s）：%v", home, err)
		return
	}
	_ = os.MkdirAll(filepath.Dir(agyKeychainPath(home)), 0o700)
	shell := filepath.Join(home, agyShellFolder)
	_ = os.MkdirAll(shell, 0o700)
	writeIfChanged(filepath.Join(shell, ".zshenv"), agyZshEnvScript(realHome))
	writeIfChanged(filepath.Join(shell, agyBashEnvFile), agyBashEnvScript(realHome))
	syncAgyLinks(home, realHome)
	if runtime.GOOS == "darwin" {
		if err := agyKeychain(home); err != nil {
			log.Printf("準備 agy 帳戶的鑰匙圈失敗（%s）：%v", home, err)
		}
	}
	agyHomeLast[home] = time.Now()
}

func agyOwnFolders(home string) []string {
	out := []string{}
	for _, m := range agyMirrored {
		out = append(out, filepath.Join(home, m.folder))
	}
	return append(out, agyConfigDir(home))
}

func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func writeIfChanged(path, content string) {
	if b, err := os.ReadFile(path); err == nil && string(b) == content {
		return
	}
	_ = os.WriteFile(path, []byte(content), 0o600)
}

func listNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// syncAgyLinks：補上該有的連結、換掉指錯的、拿掉多出來的連結（只動連結；同名的真檔案或資料夾是 agy 自己建的，不動）
func syncAgyLinks(home, realHome string) {
	planned := agyPlanLinks(home, realHome, listNames)
	wanted := map[string]bool{}
	for _, l := range planned {
		link, target := l[0], l[1]
		wanted[link] = true
		if cur, err := os.Readlink(link); err == nil {
			if cur == target {
				continue
			}
			_ = os.Remove(link)
		} else if _, err := os.Lstat(link); err == nil {
			continue
		}
		_ = os.MkdirAll(filepath.Dir(link), 0o700)
		if err := os.Symlink(target, link); err != nil {
			log.Printf("建立 agy 帳戶的連結失敗（%s）：%v", link, err)
		}
	}
	for _, folder := range agyOwnFolders(home) {
		for _, n := range listNames(folder) {
			p := filepath.Join(folder, n)
			if isSymlink(p) && !wanted[p] {
				_ = os.Remove(p)
			}
		}
	}
}

// removeAgyHomeLinks：刪除帳戶資料夾之前先拿掉家裡所有連結（只拿掉連結本身）：連結指向真正的家，刪資料夾絕不能順著連結刪到那邊
func removeAgyHomeLinks(home string) {
	if isSymlink(home) {
		return
	}
	for _, folder := range agyOwnFolders(home) {
		if isSymlink(folder) {
			continue
		}
		for _, n := range listNames(folder) {
			if p := filepath.Join(folder, n); isSymlink(p) {
				_ = os.Remove(p)
			}
		}
	}
	agyHomeMu.Lock()
	delete(agyHomeLast, home)
	agyHomeMu.Unlock()
}

// agyKeychain：準備鑰匙圈（測試換成不動系統的版本）
var agyKeychain = ensureAgyKeychain

// ensureAgyKeychain：自己的鑰匙圈：沒有就以 HOME＝家 建立（空密碼；不加進使用者的鑰匙圈清單），設成不自動上鎖，每次解鎖（重開機後是鎖著的）
func ensureAgyKeychain(home string) error {
	kc := agyKeychainPath(home)
	if _, err := os.Stat(kc); err != nil {
		if err := agySecurity(home, "create-keychain", "-p", "", kc); err != nil {
			return err
		}
		if err := agySecurity(home, "set-keychain-settings", kc); err != nil {
			return err
		}
	}
	return agySecurity(home, "unlock-keychain", "-p", "", kc)
}

func agySecurity(home string, args ...string) error {
	cmd := exec.Command("/usr/bin/security", args...)
	cmd.Env = childEnv(map[string]string{"HOME": home})
	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("security %s 失敗：%s", args[0], strings.TrimSpace(string(out)))
		}
		return nil
	case <-time.After(15 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return fmt.Errorf("security %s 逾時", args[0])
	}
}

// childEnv：子程序的環境：繼承目前的環境，set 的覆蓋、unset 的拿掉
func childEnv(set map[string]string, unset ...string) []string {
	out := []string{}
	for _, kv := range os.Environ() {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if _, ok := set[k]; ok || contains(unset, k) {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out
}
