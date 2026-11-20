package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 隔離帳戶共用這台系統預設設定資料夾（~/.claude、~/.codex、~/.grok；繼承的 CLAUDE_CONFIG_DIR／CODEX_HOME／GROK_HOME）的設定，
// 登入憑證各自分開（照 C# 的 Accounts/SharedCustomizations.cs）：
//   - 全域指示（CLAUDE.md／AGENTS.md）：每次啟動 CLI 前雙向同步（decideInstructionSync）。用複製：來源的 CLAUDE.md 可能是指到 dotfiles 的
//     符號連結，寫入時原地寫（不寫暫存檔再改名，免得把連結換成一般檔）
//   - skills（逐一）與 agents、commands、output-styles、Codex 的 prompts、Grok 的 rules（整個資料夾）：符號連結到來源。
//     帳戶原有的先合併進來源、原件移到 .myriagent-shared/backup/，關掉共用時移回來
//   - Claude 的自動記憶：啟動時以 --settings 的 autoMemoryDirectory 指到來源同一個專案的 memory/（不搬檔、不建連結）；第一次共用時把帳戶原有的記憶合併過去
//   - MCP：Claude 以 --mcp-config 帶來源 .claude.json 的 user 層與這個專案的 local 層，Codex 以 -c 帶來源 config.toml 的 mcp_servers；帳戶自己有的同名伺服器優先
// 所有帳戶共用一把鎖（同時寫來源的只有這裡）；任何一步失敗只記錄、不擋對話（少共用一項總比開不了對話好）。
// 拿掉連結時只刪連結本身，絕不順著連結刪到來源。
//
// 各家的對話程序怎麼用 prepareAccountLaunch 回傳的 sharedLaunch（照 C# 的 ClaudeStreamRules.BuildArguments、CodexAppServerRules.BuildServerArguments）：
//   - Claude：ClaudeMemoryDir 不是空的、而且不是精簡模式 → 放進 --settings 的 JSON：{"autoMemoryDirectory": ClaudeMemoryDir}（和 effort 等其他
//     --settings 欄位合併成同一個 --settings 參數）；ClaudeMcpConfig 不是空的、而且不是精簡模式 → 「--mcp-config <檔案路徑>」
//     （--mcp-config 可以接好幾個值，帳戶委派的 JSON 字串接在同一個 --mcp-config 後面；不加 --strict-mcp-config，帳戶自己的 MCP 照常載入）
//   - Codex：CodexOverrides 的每一項 → 「-c <那一項>」接在 app-server 的參數後面（例：-c mcp_servers.github.command="gh"）
//   - Grok：只有指示與連結，沒有要帶的參數

// 跨四家 CLI 共用（設定 shareCrossCliCustomizations：打通四家系統預設資料夾的全域指示與 skills）尚未移植：它會改寫四個真正的預設資料夾，
// 等 Antigravity／Grok 的對話接上、健康度能顯示結果後再做。

const (
	shareStateFile    = "state.json"
	shareBackupFolder = "backup"
	shareMcpPrefix    = "mcp-"
)

// shareState：帳戶設定資料夾裡的共用狀態（.myriagent-shared/state.json）
type shareState struct {
	// Active：目前是共用中（關掉共用時改成 false；再打開時把這段期間帳戶自己寫的記憶合併過去）
	Active bool `json:"active"`
	// InstructionHash：上次同步完的全域指示雜湊；nil＝還沒同步過
	InstructionHash *string `json:"instructionHash,omitempty"`
	// MergedAt：上次把帳戶的記憶合併進來源的時間（Unix 毫秒）：再打開共用時只合併之後改過的檔
	MergedAt int64 `json:"mergedAt,omitempty"`
}

// shareAccount：共用設定要用的帳戶快照
type shareAccount struct {
	id, label, provider, configDir string
}

var shareMu sync.Mutex

// shareSourceDir：各 CLI 的來源設定資料夾（這台的系統預設帳戶用的那個）
func shareSourceDir(p string) string {
	return (&Account{Provider: p, IsSystemDefault: true}).resolveConfigDir()
}

// shareClaudeSourceGlobalConfig：來源的 .claude.json（Claude 的 user 層 MCP 在這裡，不在設定資料夾裡）
func shareClaudeSourceGlobalConfig() string {
	legacy, config := (&Account{Provider: "Claude", IsSystemDefault: true}).claudeGlobalConfigPaths()
	if pathExists(legacy) {
		return legacy
	}
	return config
}

func init() {
	sharedLaunchHook = func(h *Hub, a *Account, workDir string) sharedLaunch {
		if !shareActive(a) {
			return sharedLaunch{}
		}
		return prepareShared(shareAccount{a.ID, a.Label, a.Provider, a.ConfigDir}, workDir)
	}
	shareChangedHook = func(h *Hub, a *Account) {
		if a.shares() || a.IsSystemDefault || a.ExistingFolder || a.ConfigDir == "" {
			return
		}
		provider, dir := a.Provider, a.ConfigDir
		go unshareCustomizations(provider, dir)
	}
}

// prepareShared：啟動 CLI 之前：同步全域指示、補上連結、第一次共用時合併帳戶原有的內容，回傳要多帶的啟動參數。
// 來源就是帳戶自己的資料夾（不該發生）時什麼都不做
func prepareShared(acc shareAccount, workDir string) sharedLaunch {
	shareMu.Lock()
	defer shareMu.Unlock()
	source := shareSourceDir(acc.provider)
	if sameFolder(source, acc.configDir) {
		return sharedLaunch{}
	}
	statePath := filepath.Join(acc.configDir, shareStateFolder, shareStateFile)
	st := readShareState(statePath)
	// 合併失敗（來源暫時寫不進去）不標成共用中：下次啟動再試
	if !st.Active && shareTry(acc, "合併原有的記憶", func() error { return mergeMemory(acc, source, st.MergedAt) }) {
		st.Active = true
		st.MergedAt = time.Now().UnixMilli()
	}
	shareTry(acc, "同步全域指示", func() error { return syncInstructions(acc, source, &st) })
	shareTry(acc, "連結 skills", func() error { return linkSkills(acc, source) })
	for _, name := range shareLinkedDirs(acc.provider) {
		name := name
		shareTry(acc, "連結 "+name, func() error { return linkDirectory(acc, source, name) })
	}
	shareTry(acc, "存共用狀態", func() error { return writeShareState(statePath, st) })

	var out sharedLaunch
	switch acc.provider {
	case "Claude":
		shareTry(acc, "找記憶資料夾與 MCP", func() error {
			root := memoryProjectRoot(workDir, dirExists, readSmallFile)
			out.ClaudeMemoryDir = sharedMemoryDirectory(source, root)
			path, err := writeClaudeMcpConfig(acc, claudeProjectKeys(root, workDir))
			out.ClaudeMcpConfig = path
			return err
		})
	case "Codex":
		shareTry(acc, "讀 MCP 設定", func() error {
			own := codexMcpServerNames(readTextOr(filepath.Join(acc.configDir, "config.toml")))
			out.CodexOverrides = codexSharedMcpOverrides(readTextOr(filepath.Join(source, "config.toml")), own)
			return nil
		})
	}
	return out
}

// unshareCustomizations：關掉共用：拿掉指向來源的連結、把共用前移走的原件移回來、刪掉 MCP 設定檔。全域指示留著（帳戶照最後一次同步的內容
// 繼續用），共用期間寫在來源的記憶也留在來源（帳戶回頭讀自己原本的記憶）
func unshareCustomizations(provider, configDir string) {
	if !supportsSharedCustomizations(provider) {
		return
	}
	source := shareSourceDir(provider)
	if sameFolder(source, configDir) {
		return
	}
	acc := shareAccount{provider: provider, configDir: configDir}
	shareMu.Lock()
	defer shareMu.Unlock()
	statePath := filepath.Join(configDir, shareStateFolder, shareStateFile)
	_, stateErr := os.Stat(statePath)
	st := readShareState(statePath)
	shareTry(acc, "拿掉 skills 的連結", func() error {
		skills := filepath.Join(configDir, shareSkillsFolder)
		sourceSkills := filepath.Join(source, shareSkillsFolder)
		for _, n := range listNames(skills) {
			entry := filepath.Join(skills, n)
			if t, ok := linkTarget(entry); ok && isUnderPath(t, sourceSkills) {
				if err := os.Remove(entry); err != nil {
					return err
				}
			}
		}
		return restoreBackups(configDir, shareSkillsFolder)
	})
	for _, name := range shareLinkedDirs(provider) {
		name := name
		shareTry(acc, "拿掉 "+name+" 的連結", func() error {
			link := filepath.Join(configDir, name)
			if t, ok := linkTarget(link); ok && sameFolder(t, filepath.Join(source, name)) {
				if err := os.Remove(link); err != nil {
					return err
				}
			}
			backup := filepath.Join(configDir, shareStateFolder, shareBackupFolder, name)
			if dirExists(backup) && !pathExists(link) {
				return os.Rename(backup, link)
			}
			return nil
		})
	}
	shareTry(acc, "刪掉 MCP 設定檔", func() error { return deleteMcpFiles(configDir, "") })
	if stateErr == nil {
		st.Active = false
		shareTry(acc, "存共用狀態", func() error { return writeShareState(statePath, st) })
	}
}

// ---- 全域指示 ----

func syncInstructions(acc shareAccount, source string, st *shareState) error {
	name := shareInstructionFile(acc.provider)
	sourcePath := filepath.Join(source, name)
	accountPath := filepath.Join(acc.configDir, name)
	sourceText, accountText := readText(sourcePath), readText(accountPath)
	action := decideInstructionSync(sourceText, accountText, st.InstructionHash)
	setHash := func(s *string) { h := shareSyncedHash(s); st.InstructionHash = &h }
	switch action {
	case syncNone:
		if sourceText != nil {
			setHash(sourceText)
		} else {
			setHash(accountText)
		}
		return nil
	case syncCopyToAccount:
		if err := writeTextInPlace(accountPath, deref(sourceText)); err != nil {
			return err
		}
		setHash(sourceText)
	case syncCopyToSource:
		if err := writeTextInPlace(sourcePath, *accountText); err != nil {
			return err
		}
		setHash(accountText)
	case syncMergeIntoSource:
		merged := mergeInstructions(*sourceText, *accountText, acc.label)
		if err := writeTextInPlace(sourcePath, merged); err != nil {
			return err
		}
		if err := writeTextInPlace(accountPath, merged); err != nil {
			return err
		}
		setHash(&merged)
	case syncKeepSourceBackupAcc:
		backup := filepath.Join(acc.configDir, shareStateFolder, shareBackupFolder, name+"."+time.Now().Format("20060102-150405"))
		if err := writeTextInPlace(backup, *accountText); err != nil {
			return err
		}
		var err error
		if isBlankText(sourceText) {
			err = os.Remove(accountPath)
		} else {
			err = writeTextInPlace(accountPath, *sourceText)
		}
		if err != nil {
			return err
		}
		setHash(sourceText)
		log.Printf("共用設定：帳戶 %s 的 %s 和來源都改過，以來源為準，帳戶那份另存在 %s", acc.id, name, backup)
	case syncDeleteAccount:
		if err := os.Remove(accountPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		h := shareBlankHash
		st.InstructionHash = &h
	}
	log.Printf("共用設定：帳戶 %s 的 %s %s", acc.id, name, action)
	return nil
}

// ---- 連結 ----

// linkSkills：skills/ 逐一連結：來源有、帳戶沒有的建連結；帳戶自己有、來源沒有的先複製到來源、原件移到備份再連結；
// 兩邊都有同名的真資料夾就留著帳戶自己的（它的版本優先）；指向來源但來源已刪掉的連結拿掉
func linkSkills(acc shareAccount, source string) error {
	skills := filepath.Join(acc.configDir, shareSkillsFolder)
	sourceSkills := filepath.Join(source, shareSkillsFolder)
	for _, name := range listNames(skills) {
		entry := filepath.Join(skills, name)
		if !isSharedSkill(acc.provider, name) {
			continue
		}
		if isSymlink(entry) {
			if t, ok := linkTarget(entry); ok && isUnderPath(t, sourceSkills) && !dirExists(t) {
				if err := os.Remove(entry); err != nil {
					return err
				}
			}
			continue
		}
		if !dirExists(entry) {
			continue
		}
		sourceEntry := filepath.Join(sourceSkills, name)
		if dirExists(sourceEntry) {
			continue
		}
		if err := copyTree(entry, sourceEntry, acc.id); err != nil {
			return err
		}
		if err := moveToBackup(acc.configDir, filepath.Join(shareSkillsFolder, name)); err != nil {
			return err
		}
		if err := os.Symlink(sourceEntry, entry); err != nil {
			return err
		}
		log.Printf("共用設定：帳戶 %s 的 skill「%s」合併進來源", acc.id, name)
	}
	for _, name := range listNames(sourceSkills) {
		sourceEntry := filepath.Join(sourceSkills, name)
		entry := filepath.Join(skills, name)
		if !isSharedSkill(acc.provider, name) || pathExists(entry) || !dirExists(sourceEntry) {
			continue
		}
		if err := os.MkdirAll(skills, 0o700); err != nil {
			return err
		}
		if err := os.Symlink(sourceEntry, entry); err != nil {
			return err
		}
	}
	return nil
}

// linkDirectory：整個資料夾連結到來源：帳戶沒有就建連結（來源也沒有就等來源出現）；帳戶有真資料夾就先把裡面的檔合併進來源
// （同名不同內容以帶帳戶代碼的檔名另存）、原件移到備份再連結；空的直接換成連結。指向來源但來源已刪掉的連結拿掉，指向別處的（使用者自己建的）不動
func linkDirectory(acc shareAccount, source, name string) error {
	link := filepath.Join(acc.configDir, name)
	target := filepath.Join(source, name)
	if isSymlink(link) {
		if t, ok := linkTarget(link); ok && sameFolder(t, target) && !dirExists(target) {
			return os.Remove(link)
		}
		return nil
	}
	if dirExists(link) {
		if len(listNames(link)) > 0 {
			if err := copyTree(link, target, acc.id); err != nil {
				return err
			}
			if err := moveToBackup(acc.configDir, name); err != nil {
				return err
			}
			log.Printf("共用設定：帳戶 %s 的 %s 合併進來源", acc.id, name)
		} else if err := os.Remove(link); err != nil {
			return err
		}
	}
	if dirExists(target) && !pathExists(link) {
		return os.Symlink(target, link)
	}
	return nil
}

func moveToBackup(configDir, rel string) error {
	backup := filepath.Join(configDir, shareStateFolder, shareBackupFolder, rel)
	if pathExists(backup) {
		backup += "." + time.Now().Format("20060102-150405")
	}
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		return err
	}
	return os.Rename(filepath.Join(configDir, rel), backup)
}

// restoreBackups：把 backup/<子資料夾>/ 底下的項目移回帳戶的 <子資料夾>/（帳戶那裡已有同名的不動）
func restoreBackups(configDir, folder string) error {
	backups := filepath.Join(configDir, shareStateFolder, shareBackupFolder, folder)
	for _, n := range listNames(backups) {
		entry := filepath.Join(backups, n)
		if !dirExists(entry) || isSymlink(entry) {
			continue
		}
		dest := filepath.Join(configDir, folder, n)
		if pathExists(dest) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		if err := os.Rename(entry, dest); err != nil {
			return err
		}
	}
	return nil
}

// ---- 記憶 ----

// mergeMemory：帳戶原有的自動記憶（projects/<專案>/memory/）合併進來源同名的專案資料夾；sinceMs 不是 0 時只合併之後改過的檔
// （關掉共用期間帳戶自己寫的）。帳戶的原件不動（共用期間 CLI 不會讀它）
func mergeMemory(acc shareAccount, source string, sinceMs int64) error {
	if acc.provider != "Claude" {
		return nil
	}
	projects := filepath.Join(acc.configDir, "projects")
	merged := 0
	for _, project := range listNames(projects) {
		memory := filepath.Join(projects, project, "memory")
		if !dirExists(memory) || isSymlink(memory) {
			continue
		}
		targetMemory := filepath.Join(source, "projects", project, "memory")
		files := regularFiles(memory)
		// 索引先合併：帳戶索引裡的行排在另存檔的說明行前面
		sort.SliceStable(files, func(i, j int) bool {
			ri, rj := strings.EqualFold(files[i], memoryIndexFile), strings.EqualFold(files[j], memoryIndexFile)
			if ri != rj {
				return ri
			}
			return strings.ToLower(files[i]) < strings.ToLower(files[j])
		})
		for _, rel := range files {
			file := filepath.Join(memory, rel)
			if sinceMs > 0 {
				if fi, err := os.Stat(file); err == nil && fi.ModTime().UnixMilli() <= sinceMs {
					continue
				}
			}
			content, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			target := filepath.Join(targetMemory, rel)
			existing, err := os.ReadFile(target)
			exists := err == nil
			switch planMemoryFile(rel, exists, exists && bytes.Equal(existing, content)) {
			case memSkip:
				continue
			case memCopy:
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(target, content, 0o600); err != nil {
					return err
				}
			case memAppendIndex:
				if err := writeIfDifferent(target, appendIndexLines(string(existing), string(content))); err != nil {
					return err
				}
			case memCopyRenamed:
				renamed := renamedForAccount(rel, acc.id)
				renamedPath := filepath.Join(targetMemory, renamed)
				if b, err := os.ReadFile(renamedPath); err == nil && bytes.Equal(b, content) {
					continue
				}
				if err := os.WriteFile(renamedPath, content, 0o600); err != nil {
					return err
				}
				index := filepath.Join(targetMemory, memoryIndexFile)
				if err := writeIfDifferent(index, appendIndexLines(readTextOr(index), indexLineForRenamed(renamed, rel, acc.label))); err != nil {
					return err
				}
			}
			merged++
		}
	}
	if merged > 0 {
		log.Printf("共用設定：帳戶 %s 有 %d 個記憶檔合併進來源", acc.id, merged)
	}
	return nil
}

// ---- MCP ----

// writeClaudeMcpConfig：寫 Claude 的 --mcp-config 檔，回路徑（沒有要共用的回空字串）。檔名照內容的雜湊：內容一樣共用一個檔、寫好就不再改。
// 裡面可能有 MCP 的環境變數（金鑰），所以放在帳戶的設定資料夾裡（和登入憑證同一層保護）
func writeClaudeMcpConfig(acc shareAccount, keys []string) (string, error) {
	srcUser, srcProject := readClaudeMcpFile(shareClaudeSourceGlobalConfig(), keys)
	legacy, config := (&Account{Provider: "Claude", ConfigDir: acc.configDir}).claudeGlobalConfigPaths()
	ownPath := config
	if pathExists(legacy) {
		ownPath = legacy
	}
	ownUser, ownProject := readClaudeMcpFile(ownPath, keys)
	data := buildClaudeMcpConfig(srcUser, srcProject, mcpServerNames(ownUser, ownProject))
	if data == nil {
		return "", deleteMcpFiles(acc.configDir, "")
	}
	path := filepath.Join(acc.configDir, shareStateFolder, shareMcpPrefix+shareHash(string(data))[:16]+".json")
	if !pathExists(path) {
		if err := writeTextInPlace(path, string(data)); err != nil {
			return "", err
		}
	}
	return path, deleteMcpFiles(acc.configDir, path)
}

func readClaudeMcpFile(path string, keys []string) (map[string]json.RawMessage, map[string]json.RawMessage) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	u, p, _ := readClaudeMcpServers(b, keys)
	return u, p
}

// deleteMcpFiles：刪掉舊的 MCP 設定檔（留 keep；一天內寫的也留著：可能有剛啟動、還沒讀完的 CLI）
func deleteMcpFiles(configDir, keep string) error {
	folder := filepath.Join(configDir, shareStateFolder)
	for _, n := range listNames(folder) {
		if !strings.HasPrefix(n, shareMcpPrefix) || !strings.HasSuffix(n, ".json") {
			continue
		}
		p := filepath.Join(folder, n)
		if keep != "" {
			if p == keep {
				continue
			}
			if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) < 24*time.Hour {
				continue
			}
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ---- 檔案 ----

func readShareState(path string) shareState {
	var st shareState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func writeShareState(path string, st shareState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readText(path string) *string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s := strings.TrimPrefix(string(b), "\xef\xbb\xbf")
	return &s
}

func readTextOr(path string) string { return deref(readText(path)) }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// writeTextInPlace：原地寫入（UTF-8 無 BOM）：目標是符號連結時寫進它指向的檔（os.WriteFile 會跟著連結），不換成一般檔
func writeTextInPlace(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func writeIfDifferent(path, content string) error {
	if s := readText(path); s != nil && *s == content {
		return nil
	}
	return writeTextInPlace(path, content)
}

// readSmallFile：讀 .git 檔、commondir、gitdir 這類小檔；讀不到、是資料夾或太大回 ok=false
func readSmallFile(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Size() > 4096 {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// regularFiles：資料夾底下所有的一般檔（相對路徑；含子資料夾，不走進符號連結）
func regularFiles(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.Type().IsRegular() {
			if rel, err := filepath.Rel(root, p); err == nil {
				out = append(out, rel)
			}
		}
		return nil
	})
	return out
}

// copyTree：複製整個資料夾（不走進連結）；目的地已有同名不同內容的檔，以帶帳戶代碼的檔名另存
func copyTree(from, to, accountID string) error {
	if err := os.MkdirAll(to, 0o700); err != nil {
		return err
	}
	for _, rel := range regularFiles(from) {
		src := filepath.Join(from, rel)
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if existing, err := os.ReadFile(target); err == nil {
			if bytes.Equal(existing, data) {
				continue
			}
			target = filepath.Join(to, renamedForAccount(rel, accountID))
		}
		mode := os.FileMode(0o600)
		if fi, err := os.Stat(src); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return err
		}
	}
	return nil
}

// linkTarget：符號連結指向的完整路徑（相對的以連結所在的資料夾為準）；不是連結回 ok=false
func linkTarget(p string) (string, bool) {
	if !isSymlink(p) {
		return "", false
	}
	t, err := os.Readlink(p)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(p), t)
	}
	return filepath.Clean(t), true
}

// isUnderPath：path 在 folder 底下（不分大小寫；/a/b 不會誤認 /a/bc 底下的）
func isUnderPath(path, folder string) bool {
	p, f := filepath.Clean(path), filepath.Clean(folder)
	return len(p) > len(f) && strings.EqualFold(p[:len(f)], f) && p[len(f)] == filepath.Separator
}

// shareTry：執行一步；失敗只記錄（回 false），不擋對話
func shareTry(acc shareAccount, step string, f func() error) bool {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return f()
	}()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("共用設定：帳戶 %s（%s）%s失敗：%v", acc.id, acc.configDir, step, err)
		return false
	}
	return err == nil
}
