package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 隔離帳戶共用系統預設帳戶設定的純函式規則（照 C# 的 Accounts/SharedCustomizationRules.cs 與 Agents/CodexConfigRules.cs 的 MCP 部分）：
// 哪些項目、全域指示的雙向同步、記憶的合併、CLI 自動記憶資料夾用的專案根目錄、要轉給帳戶的 Claude／Codex MCP 伺服器。
// 登入憑證（.credentials.json、auth.json、.claude.json 的身分）一律不共用。

const (
	// shareStateFolder：帳戶設定資料夾裡放共用狀態、MCP 設定檔與備份的資料夾（CLI 不認得的名稱，不會讀它）
	shareStateFolder = ".myriagent-shared"
	// memoryIndexFile：CLI 自動記憶的索引檔（每個專案的 memory/ 底下；只有它會整份載入）
	memoryIndexFile = "MEMORY.md"
	// shareSkillsFolder：skills 資料夾（逐一連結：資料夾裡有帳戶自己的東西，不能整個連）
	shareSkillsFolder = "skills"
)

// 全域指示這次啟動前要怎麼同步
const (
	syncNone                = "none"                // 兩邊一樣（或兩邊都沒有）
	syncCopyToAccount       = "copyToAccount"       // 來源改過（或第一次、帳戶沒有自己的內容）：來源 → 帳戶
	syncCopyToSource        = "copyToSource"        // 只有帳戶這份改過（或來源沒有、帳戶有）：帳戶 → 來源
	syncMergeIntoSource     = "mergeIntoSource"     // 第一次共用、兩邊都有不同的內容：帳戶的內容附加到來源最後，再整份複製回帳戶
	syncKeepSourceBackupAcc = "keepSourceBackupAcc" // 上次同步之後兩邊都改過：保留來源，帳戶那份另存備份，再以來源取代
	syncDeleteAccount       = "deleteAccount"       // 來源被刪掉、帳戶這份沒改過：帳戶的也刪掉
)

// 帳戶原有的一個記憶檔要怎麼併進來源
const (
	memCopy        = "copy"        // 來源沒有：照原檔名複製過去
	memSkip        = "skip"        // 來源有、內容相同
	memAppendIndex = "appendIndex" // 索引（MEMORY.md）兩邊不同：把來源沒有的行附加上去
	memCopyRenamed = "copyRenamed" // 來源有同名但內容不同：以帶帳戶代碼的檔名另存一份，並在索引加一行
)

// shareActive：這個帳戶現在要不要共用：隔離帳戶、這種 CLI 支援、使用者沒關掉、不在精簡模式（精簡模式本來就不載入這些）。
// 使用者原有的資料夾一律不共用：那是他在終端機也在用的資料夾，不能把裡面的 skills 搬走換成連結
func shareActive(a *Account) bool {
	return !a.IsSystemDefault && !a.ExistingFolder && a.ConfigDir != "" && supportsSharedCustomizations(a.Provider) && a.shares() && !a.SafeMode
}

// shareInstructionFile：全域指示檔的檔名（在設定資料夾的最上層）
func shareInstructionFile(p string) string {
	if p == "Claude" {
		return "CLAUDE.md"
	}
	return "AGENTS.md"
}

// shareLinkedDirs：整個資料夾連到來源的項目：Claude 的自訂子代理、斜線指令、輸出風格；Codex 的自訂提示；Grok 的規則資料夾
func shareLinkedDirs(p string) []string {
	switch p {
	case "Claude":
		return []string{"agents", "commands", "output-styles"}
	case "Codex":
		return []string{"prompts"}
	case "Grok":
		return []string{"rules"}
	}
	return nil
}

// agyReservedSkillNames：Antigravity 內建的保留工作流與技能名稱
var agyReservedSkillNames = map[string]bool{
	"goal": true, "compact": true, "schedule": true, "browser": true, "plan": true, "grill-me": true, "teamwork": true,
	"learn": true, "boost": true, "swarm": true, "agy-customizations": true, "antigravity-guide": true, "antigravity_guide": true,
}

// isSharedSkill：skills/ 底下這個名稱要不要共用：點開頭的一律不要（Codex 內建的 .system、CLI 的暫存）；Claude 的 synced/ 是各帳戶從
// claude.ai 同步下來的；Antigravity 排除內建工作流名稱
func isSharedSkill(p, name string) bool {
	if strings.TrimSpace(name) == "" || name[0] == '.' {
		return false
	}
	switch p {
	case "Claude":
		return !strings.EqualFold(name, "synced")
	case "Antigravity":
		return !agyReservedSkillNames[strings.ToLower(name)]
	}
	return true
}

// shareHash：內容的雜湊（比對同步狀態用；換行一律當 \n）
func shareHash(content string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(content, "\r\n", "\n")))
	return hex.EncodeToString(sum[:])
}

// shareBlankHash：同步完沒有內容時記下的雜湊
var shareBlankHash = shareHash("")

// isBlankText：沒有內容（檔案不存在或只有空白；Codex 預設會留一個 0 位元組的 AGENTS.md）
func isBlankText(s *string) bool { return s == nil || strings.TrimSpace(*s) == "" }

// shareSyncedHash：同步完記下的雜湊：沒有內容記 shareBlankHash
func shareSyncedHash(s *string) string {
	if isBlankText(s) {
		return shareBlankHash
	}
	return shareHash(*s)
}

// decideInstructionSync：全域指示的雙向同步：以上次同步完的雜湊判斷是哪一邊改的。lastHash 為 nil＝第一次共用：
// 帳戶沒有自己的內容就拿來源的，來源沒有就拿帳戶的，兩邊都有不同的內容就合併進來源。之後只有一邊改過就以那一邊為準；
// 兩邊都改過以來源為準、帳戶那份另存備份。帳戶的檔被刪掉不會刪來源（改從來源補回）
func decideInstructionSync(source, account, lastHash *string) string {
	sb, ab := isBlankText(source), isBlankText(account)
	if sb && ab {
		return syncNone
	}
	if !sb && !ab && shareHash(*source) == shareHash(*account) {
		return syncNone
	}
	if lastHash == nil {
		switch {
		case ab:
			return syncCopyToAccount
		case sb:
			return syncCopyToSource
		}
		return syncMergeIntoSource
	}
	accountChanged := (ab && *lastHash != shareBlankHash) || (!ab && shareHash(*account) != *lastHash)
	sourceChanged := (sb && *lastHash != shareBlankHash) || (!sb && shareHash(*source) != *lastHash)
	if !accountChanged {
		if sb {
			return syncDeleteAccount
		}
		return syncCopyToAccount
	}
	if !sourceChanged {
		if ab {
			return syncCopyToAccount
		}
		return syncCopyToSource
	}
	if ab {
		return syncCopyToAccount
	}
	return syncKeepSourceBackupAcc
}

func normalizeNewlines(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// mergeInstructions：第一次共用時把帳戶原有的全域指示附加到來源最後（標明來自哪個帳戶；換行一律 \n）
func mergeInstructions(source, account, accountLabel string) string {
	return strings.TrimRight(normalizeNewlines(source), " \t\r\n") + "\n\n" +
		"<!-- 以下來自帳戶「" + accountLabel + "」：Clode Myriagent 開始共用設定時合併進來 -->\n" +
		strings.TrimSpace(normalizeNewlines(account)) + "\n"
}

// planMemoryFile：帳戶原有的記憶檔要怎麼併進來源（rel 是 memory/ 底下的相對路徑）
func planMemoryFile(rel string, targetExists, sameContent bool) string {
	switch {
	case !targetExists:
		return memCopy
	case sameContent:
		return memSkip
	case strings.EqualFold(rel, memoryIndexFile):
		return memAppendIndex
	}
	return memCopyRenamed
}

// renamedForAccount：同名衝突時另存的名稱：副檔名前加帳戶 id 的前 8 碼（notes.md → notes.1a2b3c4d.md）
func renamedForAccount(rel, accountID string) string {
	suffix := accountID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	dir, base := filepath.Split(rel)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext) + "." + suffix + ext
	if dir == "" {
		return name
	}
	return filepath.Join(dir, name)
}

// appendIndexLines：把帳戶索引裡來源沒有的行（去掉前後空白比對、略過空行）依序附加到來源索引最後；沒有要加的回原文
func appendIndexLines(sourceIndex, accountIndex string) string {
	existing := map[string]bool{}
	for _, l := range strings.Split(normalizeNewlines(sourceIndex), "\n") {
		existing[strings.TrimSpace(l)] = true
	}
	var added []string
	for _, l := range strings.Split(normalizeNewlines(accountIndex), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || existing[t] {
			continue
		}
		existing[t] = true
		added = append(added, strings.TrimRight(l, " \t"))
	}
	if len(added) == 0 {
		return sourceIndex
	}
	head := strings.TrimRight(normalizeNewlines(sourceIndex), "\n")
	if head != "" {
		head += "\n"
	}
	return head + strings.Join(added, "\n") + "\n"
}

// indexLineForRenamed：另存的記憶檔在索引的那一行（照 CLI 索引的格式：- [標題](檔名) — 說明）
func indexLineForRenamed(renamed, original, accountLabel string) string {
	base := filepath.Base(original)
	return "- [" + strings.TrimSuffix(base, filepath.Ext(base)) + "（帳戶「" + accountLabel + "」的版本）](" +
		filepath.ToSlash(renamed) + ") — 開始共用設定時兩邊內容不同，另存一份"
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeProjectFolderName：CLI 存放這個工作資料夾逐字稿與記憶的專案資料夾名稱：英數字以外的字元都換成「-」（/Users/u/repo → -Users-u-repo）
func claudeProjectFolderName(workDir string) string {
	p := workDir
	if len(p) > 3 {
		p = strings.TrimRight(p, "/")
	}
	return nonAlphanumeric.ReplaceAllString(p, "-")
}

// memoryProjectRoot：CLI 自動記憶（<設定資料夾>/projects/<名稱>/memory）用的專案根目錄，照 CLI 2.1.308 的 defaultKey：
// 在 git worktree 裡用主 repo 的根目錄、在 git 裡用 repo 根目錄、都不是就是工作資料夾。
// worktree 的判斷同 CLI：.git 檔的 gitdir 指到 <共同 .git>/worktrees/<名稱>、那裡的 commondir 指回共同 .git、gitdir 反指回這個 worktree，
// 任何一項對不上就當成一般的 repo 根目錄。readFile 讀不到回 ok=false
func memoryProjectRoot(workDir string, isDir func(string) bool, readFile func(string) (string, bool)) string {
	start, err := filepath.Abs(workDir)
	if err != nil {
		start = filepath.Clean(workDir)
	}
	for dir := start; ; {
		dotGit := filepath.Join(dir, ".git")
		if isDir(dotGit) {
			return dir
		}
		if text, ok := readFile(dotGit); ok {
			return canonicalWorktreeRoot(dir, text, readFile)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return start
}

func canonicalWorktreeRoot(root, gitFile string, readFile func(string) (string, bool)) string {
	line := strings.TrimSpace(gitFile)
	if !strings.HasPrefix(line, "gitdir:") {
		return root
	}
	gitDir := absJoin(root, strings.TrimSpace(line[7:]))
	common, ok := readFile(filepath.Join(gitDir, "commondir"))
	if !ok {
		return root
	}
	commonDir := absJoin(gitDir, strings.TrimSpace(common))
	if !samePathFold(filepath.Dir(gitDir), filepath.Join(commonDir, "worktrees")) {
		return root
	}
	backText, ok := readFile(filepath.Join(gitDir, "gitdir"))
	if !ok || !samePathFold(absJoin(gitDir, strings.TrimSpace(backText)), filepath.Join(root, ".git")) {
		return root
	}
	// 共同的 .git 在主 repo 的根目錄底下；bare repo 的 worktree 沒有主要的工作資料夾，照一般 repo 處理
	if !strings.EqualFold(filepath.Base(commonDir), ".git") {
		return root
	}
	return filepath.Dir(commonDir)
}

func absJoin(base, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

func samePathFold(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

// sharedMemoryDirectory：共用的自動記憶資料夾：來源設定資料夾的 projects/<專案根目錄的資料夾名稱>/memory（和系統預設帳戶在同一個專案寫的是同一個資料夾）
func sharedMemoryDirectory(sourceConfigDir, projectRoot string) string {
	return filepath.Join(sourceConfigDir, "projects", claudeProjectFolderName(projectRoot), "memory")
}

// claudeProjectKeys：.claude.json 的 projects 用的鍵：正斜線的完整路徑；專案根目錄與工作資料夾各一個，相同就只有一個
func claudeProjectKeys(projectRoot, workDir string) []string {
	key := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			p = a
		}
		return filepath.ToSlash(strings.TrimRight(filepath.Clean(p), "/"))
	}
	keys := []string{key(projectRoot)}
	if w := key(workDir); !strings.EqualFold(w, keys[0]) {
		keys = append(keys, w)
	}
	return keys
}

// readClaudeMcpServers：.claude.json 的 MCP 伺服器：最上層的 mcpServers（user 層）與 projects 裡這幾個鍵（不分大小寫、斜線方向不拘）的
// mcpServers（local 層，後面的鍵蓋過前面的同名）。讀不懂回 ok=false
func readClaudeMcpServers(data []byte, projectKeys []string) (user, project map[string]json.RawMessage, ok bool) {
	var doc struct {
		McpServers map[string]json.RawMessage `json:"mcpServers"`
		Projects   map[string]struct {
			McpServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil, nil, false
	}
	user = doc.McpServers
	found := map[int]map[string]json.RawMessage{}
	for k, v := range doc.Projects {
		key := strings.TrimRight(strings.ReplaceAll(k, `\`, "/"), "/")
		for i, want := range projectKeys {
			if strings.EqualFold(strings.TrimRight(want, "/"), key) && v.McpServers != nil {
				found[i] = v.McpServers
			}
		}
	}
	for i := range projectKeys {
		if s, ok := found[i]; ok {
			if project == nil {
				project = map[string]json.RawMessage{}
			}
			for n, d := range s {
				project[n] = d
			}
		}
	}
	return user, project, true
}

// buildClaudeMcpConfig：要以 --mcp-config 帶給隔離帳戶的 Claude MCP 設定（{"mcpServers":{…}}）：來源的 user 層加上這個專案的 local 層
// （同名以 local 為準），帳戶自己的設定已經有的名稱不給。沒有可給的回 nil
func buildClaudeMcpConfig(userServers, projectServers map[string]json.RawMessage, accountNames []string) []byte {
	own := map[string]bool{}
	for _, n := range accountNames {
		own[n] = true
	}
	servers := map[string]json.RawMessage{}
	for _, src := range []map[string]json.RawMessage{userServers, projectServers} {
		for n, d := range src {
			if own[n] || !bytes.HasPrefix(bytes.TrimSpace(d), []byte("{")) {
				continue
			}
			servers[n] = d
		}
	}
	if len(servers) == 0 {
		return nil
	}
	out, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return nil
	}
	return out
}

// mcpServerNames：帳戶自己的 Claude MCP 伺服器名稱（user 層與這個專案的 local 層）
func mcpServerNames(userServers, projectServers map[string]json.RawMessage) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]json.RawMessage{userServers, projectServers} {
		for n := range m {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---- Codex config.toml 的 MCP 伺服器（照 C# 的 CodexConfigRules；只認得共用需要的部分） ----

// tomlValueState：一個值還沒掃完的部分：多行字串的結尾記號、還沒關上的 [ 與 { 層數
type tomlValueState struct {
	multiline string
	depth     int
}

func (s *tomlValueState) pending() bool { return s.multiline != "" || s.depth > 0 }

func tomlSkipSpace(line string, i *int) {
	for *i < len(line) && (line[*i] == ' ' || line[*i] == '\t') {
		*i++
	}
}

// tomlReadQuoted：讀一段單行的引號字串，回傳引號內的原文；沒有收尾的引號回 ok=false（游標停在行尾）
func tomlReadQuoted(line string, i *int) (string, bool) {
	quote := line[*i]
	*i++
	start := *i
	for *i < len(line) {
		if quote == '"' && line[*i] == '\\' {
			*i += 2
			continue
		}
		if line[*i] == quote {
			s := line[start:*i]
			*i++
			return s, true
		}
		*i++
	}
	*i = len(line)
	return "", false
}

// tomlReadKey：讀一個點號分隔的鍵（裸鍵 A-Za-z0-9_-、"基本字串"、'字面字串'，點號前後可有空白）
func tomlReadKey(line string, i *int) ([]string, bool) {
	var segs []string
	for {
		tomlSkipSpace(line, i)
		if *i >= len(line) {
			return nil, false
		}
		var seg string
		if c := line[*i]; c == '"' || c == '\'' {
			s, ok := tomlReadQuoted(line, i)
			if !ok {
				return nil, false
			}
			seg = s
		} else {
			start := *i
			for *i < len(line) {
				c := line[*i]
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
					*i++
					continue
				}
				break
			}
			if *i == start {
				return nil, false
			}
			seg = line[start:*i]
		}
		segs = append(segs, seg)
		tomlSkipSpace(line, i)
		if *i < len(line) && line[*i] == '.' {
			*i++
			continue
		}
		return segs, true
	}
}

func tomlExpect(line string, i *int, c byte) bool {
	tomlSkipSpace(line, i)
	if *i >= len(line) || line[*i] != c {
		return false
	}
	*i++
	return true
}

func tomlMultilineEnd(line string, start int, delim string) int {
	quote := delim[0]
	for i := start; i < len(line); i++ {
		if quote == '"' && line[i] == '\\' {
			i++
			continue
		}
		if !strings.HasPrefix(line[i:], delim) {
			continue
		}
		end := i + 3
		for extra := 0; extra < 2 && end < len(line) && line[end] == quote; extra++ {
			end++
		}
		return end
	}
	return -1
}

// tomlScanValue：掃過一行裡屬於值的部分（字串、陣列、行內表、行尾註解），記下跨到下一行的狀態
func tomlScanValue(line string, i *int, st *tomlValueState) {
	for *i < len(line) {
		if st.multiline != "" {
			end := tomlMultilineEnd(line, *i, st.multiline)
			if end < 0 {
				*i = len(line)
				return
			}
			st.multiline = ""
			*i = end
			continue
		}
		c := line[*i]
		switch {
		case c == '#':
			*i = len(line)
			return
		case c == '"' || c == '\'':
			triple := strings.Repeat(string(c), 3)
			if strings.HasPrefix(line[*i:], triple) {
				st.multiline = triple
				*i += 3
			} else {
				tomlReadQuoted(line, i)
			}
			continue
		case c == '[' || c == '{':
			st.depth++
		case (c == ']' || c == '}') && st.depth > 0:
			st.depth--
		}
		*i++
	}
}

// codexMcpServerNames：config.toml 裡定義的 MCP 伺服器名稱（標頭 [mcp_servers.名稱]、[mcp_servers] 表內的鍵、根層的 mcp_servers.名稱.x）。
// 多行字串與跨行陣列裡的文字不當成設定
func codexMcpServerNames(toml string) []string {
	var out []string
	add := func(n string) {
		if n != "" && !containsString(out, n) {
			out = append(out, n)
		}
	}
	table := []string{}
	tableOK := true
	st := &tomlValueState{}
	for _, raw := range strings.Split(toml, "\n") {
		line := strings.TrimRight(raw, "\r")
		i := 0
		if st.pending() {
			tomlScanValue(line, &i, st)
			continue
		}
		tomlSkipSpace(line, &i)
		if i >= len(line) || line[i] == '#' {
			continue
		}
		if line[i] == '[' {
			arrayTable := i+1 < len(line) && line[i+1] == '['
			if arrayTable {
				i += 2
			} else {
				i++
			}
			header, ok := tomlReadKey(line, &i)
			ok = ok && tomlExpect(line, &i, ']')
			table, tableOK = header, !arrayTable && ok
			if tableOK && len(table) >= 2 && table[0] == "mcp_servers" {
				add(table[1])
			}
			continue
		}
		key, ok := tomlReadKey(line, &i)
		if !ok || !tomlExpect(line, &i, '=') {
			continue
		}
		if tableOK {
			path := append(append([]string{}, table...), key...)
			if len(path) >= 2 && path[0] == "mcp_servers" {
				add(path[1])
			}
		}
		tomlScanValue(line, &i, st)
	}
	return out
}

var sharedMcpServerName = regexp.MustCompile(`^[A-Za-z0-9_:@/-]+$`)

// codexSharedMcpOverrides：共用給隔離帳戶的 MCP 伺服器：來源 config.toml 裡每個 mcp_servers.名稱 的每個鍵，轉成 app-server 的
// 「-c mcp_servers.名稱.鍵=值」（-c 後面那個參數；值照原文）。名稱或鍵含點號、或用到陣列表的伺服器整個略過——只給一部分等於少了 command，
// Codex 會整個起不來。帳戶自己的 config.toml 已有的名稱不給：-c 會蓋過帳戶自己的設定
func codexSharedMcpOverrides(sourceToml string, exclude []string) []string {
	type entry struct {
		server string
		keys   []string
		value  string
	}
	var entries []entry
	unsupported := map[string]bool{}
	if sourceToml == "" {
		return nil
	}
	table := []string{}
	tableOK := true
	st := &tomlValueState{}
	var pend *entry
	var pendText strings.Builder
	for _, raw := range strings.Split(sourceToml, "\n") {
		line := strings.TrimRight(raw, "\r")
		i := 0
		if st.pending() {
			tomlScanValue(line, &i, st)
			if pend != nil {
				pendText.WriteString("\n" + line)
				if !st.pending() {
					pend.value = strings.TrimSpace(pendText.String())
					entries = append(entries, *pend)
					pend = nil
				}
			}
			continue
		}
		tomlSkipSpace(line, &i)
		if i >= len(line) || line[i] == '#' {
			continue
		}
		if line[i] == '[' {
			arrayTable := i+1 < len(line) && line[i+1] == '['
			if arrayTable {
				i += 2
			} else {
				i++
			}
			header, ok := tomlReadKey(line, &i)
			ok = ok && tomlExpect(line, &i, ']')
			if arrayTable && ok && len(header) >= 2 && header[0] == "mcp_servers" {
				unsupported[header[1]] = true
			}
			table, tableOK = header, !arrayTable && ok
			continue
		}
		key, ok := tomlReadKey(line, &i)
		if !ok || !tomlExpect(line, &i, '=') {
			continue
		}
		start := i
		tomlScanValue(line, &i, st)
		if !tableOK {
			continue
		}
		path := append(append([]string{}, table...), key...)
		if len(path) < 2 || path[0] != "mcp_servers" {
			continue
		}
		text := strings.TrimSpace(line[start:])
		e := entry{server: path[1], keys: append([]string{}, path[2:]...)}
		if st.pending() {
			pend = &e
			pendText.Reset()
			pendText.WriteString(text)
		} else {
			e.value = text
			entries = append(entries, e)
		}
	}
	if pend != nil {
		unsupported[pend.server] = true
	}
	for _, e := range entries {
		bad := !sharedMcpServerName.MatchString(e.server) || e.value == ""
		for _, k := range e.keys {
			if k == "" || strings.ContainsAny(k, ".=") || strings.TrimSpace(k) != k {
				bad = true
			}
		}
		if bad {
			unsupported[e.server] = true
		}
	}
	var out []string
	for _, e := range entries {
		if unsupported[e.server] || containsString(exclude, e.server) {
			continue
		}
		k := "mcp_servers." + e.server
		for _, x := range e.keys {
			k += "." + x
		}
		out = append(out, k+"="+e.value)
	}
	return out
}
