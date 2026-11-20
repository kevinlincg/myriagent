package agent

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 健康度的掃描（照 C# 的 Health/HealthScanner.cs；在 goroutine 上跑、不持有 h.mu）：只讀，不寫任何檔。每個帳戶讀它每次對話都會載入的指示
// （主檔、@ 匯入、使用者層規則、Grok 的 Claude 相容）、Claude 的自動記憶索引、skill 與外掛、MCP 伺服器、設定檔；再整理出電腦層級的
// 指示檔（各 CLI 使用者層的那一份）給 compareInstructions 比較。讀不到的檔一律當不存在，不讓一個檔案的錯誤拖垮整份分析。

const (
	healthMaxReadBytes   = 4 << 20 // 超過這個大小的指示檔不讀內容（Claude Code 也會略過 4 MiB 以上的檔）
	healthMaxImportDepth = 4       // @ 匯入最多幾層
	healthMaxImportFiles = 50      // 一個帳戶最多追幾個匯入的檔
)

var (
	grokFileNames        = []string{"Agents.md", "Claude.md", "CLAUDE.md", "CLAUDE.local.md", "AGENT.md", "AGENTS.md"}
	grokClaudeCompatName = []string{"CLAUDE.md", "CLAUDE.local.md"}
)

// healthScanResult：一次掃描的結果
type healthScanResult struct {
	CheckedAt time.Time
	Accounts  []*accountScan
	Docs      []*instructionDoc
}

// healthScanner：掃描用的位置（測試換成暫存資料夾）
type healthScanner struct{ home string }

func (sc healthScanner) scan(accounts []healthAccountInput) *healthScanResult {
	res := &healthScanResult{CheckedAt: time.Now()}
	for _, a := range accounts {
		res.Accounts = append(res.Accounts, sc.scanAccount(a))
	}
	res.Docs = sc.buildDocs(accounts)
	return res
}

func (sc healthScanner) scanAccount(a healthAccountInput) *accountScan {
	s := &accountScan{Account: a}
	switch a.Provider {
	case "Claude":
		sc.scanClaude(s)
	case "Codex":
		sc.scanCodex(s)
	case "Grok":
		sc.scanGrok(s)
	case "Antigravity":
		sc.scanAntigravity(s)
	}
	return s
}

// ---- 檔案 ----

// healthReadText：讀文字；不存在、太大、讀不到回 ok=false
func healthReadText(path string, max int64) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() || fi.Size() > max {
		return "", false
	}
	b, err := io.ReadAll(f)
	if err != nil {
		log.Printf("健康度分析讀不到 %s：%v", path, err)
		return "", false
	}
	return strings.TrimPrefix(string(b), "\uFEFF"), true
}

func readHealthText(path string) (string, bool) { return healthReadText(path, healthMaxReadBytes) }

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// fileLength：檔案大小（符號連結取它指向的實際檔案）
func fileLength(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// linkTarget：符號連結（檔案或使用者資料夾底下的上層資料夾是連結都算）指向的實際檔案；不是連結回空字串。
// 使用者資料夾本身與更上層的連結（例如 macOS 的 /var → /private/var）不算
func (sc healthScanner) linkTarget(p string) string {
	if isSymlink(p) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return ""
	}
	home := filepath.Clean(sc.home)
	dir, tail := filepath.Dir(p), filepath.Base(p)
	for strings.HasPrefix(dir, home+string(filepath.Separator)) {
		if isSymlink(dir) {
			if r, err := filepath.EvalSymlinks(dir); err == nil {
				return filepath.Join(r, tail)
			}
			return ""
		}
		tail = filepath.Join(filepath.Base(dir), tail)
		dir = filepath.Dir(dir)
	}
	return ""
}

func subDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if dirExists(p) {
			out = append(out, p)
		}
	}
	return out
}

func markdownFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") || !strings.EqualFold(filepath.Ext(n), ".md") {
			continue
		}
		if p := filepath.Join(dir, n); fileExists(p) {
			out = append(out, p)
		}
	}
	return out
}

// markdownFilesDeep：資料夾（含子資料夾）裡所有 .md，依路徑排序
func markdownFilesDeep(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".md") && !strings.HasPrefix(d.Name(), ".") {
			out = append(out, p)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

type skillFile struct {
	Name, File string
	Synced     bool
}

// skillFiles：skills/ 底下的 skill（每個資料夾的 SKILL.md）；點開頭的略過，synced/ 再往下一層
func skillFiles(skillsDir string) []skillFile {
	var out []skillFile
	for _, sub := range subDirs(skillsDir) {
		name := filepath.Base(sub)
		if strings.HasPrefix(name, ".") {
			continue
		}
		if f := filepath.Join(sub, "SKILL.md"); fileExists(f) {
			out = append(out, skillFile{name, f, false})
			continue
		}
		if strings.EqualFold(name, "synced") {
			for _, s := range subDirs(sub) {
				if f := filepath.Join(s, "SKILL.md"); !strings.HasPrefix(filepath.Base(s), ".") && fileExists(f) {
					out = append(out, skillFile{filepath.Base(s), f, true})
				}
			}
		}
	}
	return out
}

func readJSONObject(path string, max int64) (map[string]any, bool, string) {
	text, ok := healthReadText(path, max)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, false, ""
	}
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return nil, true, truncateEllipsis(err.Error(), 160)
	}
	m, isObj := v.(map[string]any)
	if !isObj {
		return nil, true, "最上層不是物件"
	}
	return m, true, ""
}

// ---- skill 清單 ----

func skillStatOf(name, file, kind, source, plugin, text string, readable bool, fm map[string]string) skillStat {
	body, hasFM := "", false
	if readable {
		body, hasFM = bodyAfterFrontMatter(text)
	}
	var missing []string
	base := filepath.Dir(file)
	for _, link := range relativeLinks(body) {
		if len(missing) >= 10 {
			break
		}
		if full := filepath.Join(base, link); !pathExists(full) {
			missing = append(missing, link)
		}
	}
	st := skillStat{Name: name, Kind: kind, Source: source, Plugin: plugin, File: file, Readable: readable, HasFrontMatter: hasFM,
		DeclaredName: frontMatterText(fm, "name"), Description: frontMatterText(fm, "description"), WhenToUse: frontMatterText(fm, "when_to_use"),
		MissingRefs: missing}
	if readable {
		st.BodyLines = countLines(strings.Trim(body, "\r\n"))
		st.FrontMatterProblem = frontMatterProblem(text)
	}
	return st
}

// addListing：skill 清單的一筆：名稱＋描述（單筆描述最多 1,536 字元）；同時記下這一筆的明細
func addListing(listing *strings.Builder, s *accountScan, name, file, kind, source, plugin string) {
	text, ok := healthReadText(file, 256*1024)
	fm := map[string]string{}
	if ok {
		fm = parseFrontMatter(text)
	}
	s.SkillList = append(s.SkillList, skillStatOf(name, file, kind, source, plugin, text, ok, fm))
	skillName := firstNonBlank(frontMatterText(fm, "name"), name)
	s.SkillListingChars += skillListingChars(skillName, fm, skillDescriptionCap)
	desc, when := frontMatterText(fm, "description"), frontMatterText(fm, "when_to_use")
	if when != "" {
		desc += " " + when
	}
	listing.WriteString(skillName + ": " + truncateChars(desc, skillDescriptionCap) + "\n")
}

// ---- Claude ----

func (sc healthScanner) scanClaude(s *accountScan) {
	dir := s.Account.ConfigDir
	main := filepath.Join(dir, "CLAUDE.md")
	s.MainInstruction = main
	seen := map[string]bool{}
	sc.addWithImports(s, main, "main", 0, seen)
	sc.scanRules(s, filepath.Join(dir, "rules"), seen)

	// 自動記憶：共用設定時記憶寫在來源（系統預設帳戶）的 projects/ 底下
	mem := firstNonBlank(s.Account.SharedSource, dir)
	s.MemoryIndexes = scanMemory(filepath.Join(mem, "projects"))

	settingsPath := filepath.Join(dir, "settings.json")
	var settings map[string]any
	if fileExists(settingsPath) {
		s.SettingsPath = settingsPath
		var errText string
		settings, _, errText = readJSONObject(settingsPath, healthMaxReadBytes)
		s.SettingsError = errText
	}

	var listing strings.Builder
	skills := 0
	for _, sk := range skillFiles(filepath.Join(dir, "skills")) {
		skills++
		src := "user"
		if sk.Synced {
			src = "synced"
		}
		addListing(&listing, s, sk.Name, sk.File, "skill", src, "")
	}
	for _, c := range markdownFiles(filepath.Join(dir, "commands")) {
		skills++
		addListing(&listing, s, strings.TrimSuffix(filepath.Base(c), filepath.Ext(c)), c, "command", "user", "")
	}
	agents := len(markdownFiles(filepath.Join(dir, "agents")))

	// 外掛：enabledPlugins 為 true 的，照 installed_plugins.json 的安裝位置數它帶來的東西
	mcp := []string{}
	plugins := []pluginStat{}
	pluginSkills := 0
	installed := readInstalledPlugins(filepath.Join(dir, "plugins", "installed_plugins.json"))
	if enabled, ok := settings["enabledPlugins"].(map[string]any); ok {
		ids := make([]string, 0, len(enabled))
		for id := range enabled {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if b, ok := enabled[id].(bool); !ok || !b {
				continue
			}
			path := installed[id]
			name := strings.SplitN(id, "@", 2)[0]
			if path == "" || !dirExists(path) {
				plugins = append(plugins, pluginStat{ID: id, Missing: true})
				continue
			}
			psk := pluginSkillFiles(path)
			pcmd := markdownFiles(filepath.Join(path, "commands"))
			pag := len(markdownFiles(filepath.Join(path, "agents")))
			for _, sk := range psk {
				addListing(&listing, s, sk.Name, sk.File, "skill", "plugin", name)
			}
			for _, c := range pcmd {
				addListing(&listing, s, strings.TrimSuffix(filepath.Base(c), filepath.Ext(c)), c, "command", "plugin", name)
			}
			pluginSkills += len(psk) + len(pcmd)
			agents += pag
			pm := pluginMcpServers(path)
			for _, n := range pm {
				mcp = append(mcp, n+"（"+name+"）")
			}
			plugins = append(plugins, pluginStat{ID: id, Skills: len(psk), Commands: len(pcmd), Agent: pag, Mcp: len(pm) > 0,
				Hooks: fileExists(filepath.Join(path, "hooks", "hooks.json"))})
		}
	}
	total := skills + pluginSkills
	s.Skills = &total
	s.PluginSkills = pluginSkills
	s.Agents = agents
	s.Plugins = plugins
	s.SkillListingTokens = estimateTokens(listing.String())

	// 使用者層的 MCP 伺服器在 .claude.json 的 mcpServers
	if g := s.Account.ClaudeGlobalConfig; g != "" && fileExists(g) {
		s.StateFilePath = g
		s.StateFileBytes = fileLength(g)
		if cfg, ok, _ := readJSONObject(g, 64<<20); ok {
			if servers, ok := cfg["mcpServers"].(map[string]any); ok {
				names := sortedKeys(servers)
				mcp = append(names, mcp...)
			}
		}
	}
	s.McpServers = mcp
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readInstalledPlugins：installed_plugins.json（v2：plugins 的每個 id 是安裝紀錄的陣列）→ id 對安裝位置
func readInstalledPlugins(path string) map[string]string {
	out := map[string]string{}
	root, _, _ := readJSONObject(path, 8<<20)
	plugins, _ := root["plugins"].(map[string]any)
	for id, v := range plugins {
		var entry map[string]any
		switch x := v.(type) {
		case []any:
			if len(x) > 0 {
				entry, _ = x[0].(map[string]any)
			}
		case map[string]any:
			entry = x
		}
		if p, ok := entry["installPath"].(string); ok {
			out[id] = p
		}
	}
	return out
}

// pluginSkillFiles：外掛的 skill：預設的 skills/，加上 .claude-plugin/plugin.json 的 skills（字串或陣列）。同一個 SKILL.md 只算一次
func pluginSkillFiles(pluginDir string) []skillFile {
	dirs := []string{filepath.Join(pluginDir, "skills")}
	if root, ok, _ := readJSONObject(filepath.Join(pluginDir, ".claude-plugin", "plugin.json"), 1<<20); ok {
		var paths []string
		switch x := root["skills"].(type) {
		case string:
			paths = []string{x}
		case []any:
			for _, v := range x {
				if s, ok := v.(string); ok {
					paths = append(paths, s)
				}
			}
		}
		base := filepath.Clean(pluginDir)
		for _, rel := range paths {
			if strings.TrimSpace(rel) == "" || filepath.IsAbs(rel) {
				continue
			}
			full := filepath.Clean(filepath.Join(pluginDir, rel))
			if strings.HasPrefix(full, base) {
				dirs = append(dirs, full)
			}
		}
	}
	var out []skillFile
	seen := map[string]bool{}
	for _, d := range dirs {
		var found []skillFile
		if single := filepath.Join(d, "SKILL.md"); fileExists(single) {
			found = []skillFile{{filepath.Base(d), single, false}}
		} else {
			found = skillFiles(d)
		}
		for _, f := range found {
			if k := strings.ToLower(filepath.Clean(f.File)); !seen[k] {
				seen[k] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// pluginMcpServers：外掛的 MCP 伺服器：.mcp.json 或 .claude-plugin/plugin.json 的 mcpServers
func pluginMcpServers(pluginDir string) []string {
	var names []string
	for _, f := range []string{filepath.Join(pluginDir, ".mcp.json"), filepath.Join(pluginDir, ".claude-plugin", "plugin.json")} {
		root, ok, _ := readJSONObject(f, 1<<20)
		if !ok || root == nil {
			continue
		}
		servers, _ := root["mcpServers"].(map[string]any)
		if servers == nil && strings.HasSuffix(f, ".mcp.json") {
			servers = root
		}
		for _, n := range sortedKeys(servers) {
			if _, isObj := servers[n].(map[string]any); isObj && !containsString(names, n) {
				names = append(names, n)
			}
		}
	}
	return names
}

// scanMemory：每個專案的 memory/MEMORY.md：行數與大小
func scanMemory(projectsDir string) []memoryIndexStat {
	out := []memoryIndexStat{}
	for _, p := range subDirs(projectsDir) {
		f := filepath.Join(p, "memory", "MEMORY.md")
		if !fileExists(f) {
			continue
		}
		lines := 0
		if t, ok := readHealthText(f); ok {
			lines = countLines(t)
		}
		out = append(out, memoryIndexStat{Project: filepath.Base(p), Path: f, Lines: lines, Bytes: fileLength(f)})
	}
	return out
}

// scanRules：使用者層規則（rules/ 底下所有 .md，含子資料夾）：沒有 paths 的每次都載入
func (sc healthScanner) scanRules(s *accountScan, dir string, seen map[string]bool) {
	for _, f := range markdownFilesDeep(dir) {
		k := strings.ToLower(filepath.Clean(f))
		if seen[k] {
			continue
		}
		seen[k] = true
		t, ok := readHealthText(f)
		if !ok {
			continue
		}
		if hasPathScope(parseFrontMatter(t)) {
			s.ScopedRules++
			continue
		}
		s.Instructions = append(s.Instructions, instructionFileStat{Path: f, Kind: "rule", Measure: measureText(t, fileLength(f)), LinkTarget: sc.linkTarget(f)})
	}
}

// addWithImports：加一個指示檔與它的 @ 匯入（最多四跳、50 個檔）；不存在的主檔不加
func (sc healthScanner) addWithImports(s *accountScan, file, kind string, depth int, seen map[string]bool) {
	full := filepath.Clean(file)
	k := strings.ToLower(full)
	if seen[k] || !fileExists(full) {
		return
	}
	seen[k] = true
	t, ok := readHealthText(full)
	if !ok {
		return
	}
	target := sc.linkTarget(full)
	s.Instructions = append(s.Instructions, instructionFileStat{Path: full, Kind: kind, Measure: measureText(t, fileLength(full)), LinkTarget: target})
	if depth >= healthMaxImportDepth {
		return
	}
	// 匯入路徑以連結的實際位置為基準（CLI 讀的是連結指向的檔）
	base := firstNonBlank(target, full)
	for _, imp := range findImports(t) {
		if len(seen) >= healthMaxImportFiles {
			return
		}
		resolved := resolveImport(imp, base, sc.home)
		if resolved == "" {
			continue
		}
		if fileExists(resolved) {
			sc.addWithImports(s, resolved, "import", depth+1, seen)
		} else if !dirExists(resolved) && looksLikeImportFile(imp) && !containsString(s.MissingImports, imp) {
			s.MissingImports = append(s.MissingImports, imp)
		}
	}
}

// looksLikeImportFile：只把像檔案的（有副檔名）找不到時列為「匯入找不到」：@某人、@套件名稱這類誤認的不提
func looksLikeImportFile(imp string) bool {
	return filepath.Ext(imp) != "" && (strings.ContainsAny(imp, `/\`) || strings.HasPrefix(imp, "~") || strings.HasSuffix(strings.ToLower(imp), ".md"))
}

// ---- Codex ----

// codexInstructionFile：Codex 的全域指示：AGENTS.override.md 有內容就用它，否則 AGENTS.md
func codexInstructionFile(dir string) string {
	over := filepath.Join(dir, "AGENTS.override.md")
	if t, ok := readHealthText(over); ok && strings.TrimSpace(t) != "" {
		return over
	}
	return filepath.Join(dir, "AGENTS.md")
}

func (sc healthScanner) scanCodex(s *accountScan) {
	dir := s.Account.ConfigDir
	main := codexInstructionFile(dir)
	s.MainInstruction = main
	if t, ok := readHealthText(main); ok && strings.TrimSpace(t) != "" {
		s.Instructions = append(s.Instructions, instructionFileStat{Path: main, Kind: "main", Measure: measureText(t, fileLength(main)), LinkTarget: sc.linkTarget(main)})
	}
	var listing strings.Builder
	skills := 0
	for _, sk := range skillFiles(filepath.Join(dir, "skills")) {
		skills++
		t, ok := healthReadText(sk.File, 256*1024)
		fm := map[string]string{}
		if ok {
			fm = parseFrontMatter(t)
		}
		s.SkillList = append(s.SkillList, skillStatOf(sk.Name, sk.File, "skill", "user", "", t, ok, fm))
		s.SkillListingChars += skillListingChars(sk.Name, fm, skillDescriptionCap)
		listing.WriteString(sk.Name + ": " + frontMatterText(fm, "description") + "\n")
	}
	s.Skills = &skills
	s.SkillListingTokens = estimateTokens(listing.String())
	config := filepath.Join(dir, "config.toml")
	s.McpServers = []string{}
	if fileExists(config) {
		s.SettingsPath = config
		if t, ok := readHealthText(config); ok {
			s.McpServers = tomlTableNames(t, "mcp_servers")
		}
	}
}

// ---- Grok ----

// grokHomeFiles：GROK_HOME 那一層 grok 會讀的檔（不分大小寫去重，顯示磁碟上的寫法）
func grokHomeFiles(dir string) []string {
	actual := map[string]string{}
	for _, f := range markdownFiles(dir) {
		actual[strings.ToLower(filepath.Base(f))] = f
	}
	var out []string
	for _, n := range grokFileNames {
		if f, ok := actual[strings.ToLower(n)]; ok {
			out = append(out, f)
			delete(actual, strings.ToLower(n))
		}
	}
	return out
}

// grokClaudeCompat：grok 的 Claude 相容有沒有開：config.toml 的 [claude_compat] 沒有 imported = true 就會讀 ~/.claude
func grokClaudeCompat(grokHome string) bool {
	t, ok := readHealthText(filepath.Join(grokHome, "config.toml"))
	if !ok {
		return true
	}
	b := tomlBool(t, "claude_compat", "imported")
	return b == nil || !*b
}

func (sc healthScanner) scanGrok(s *accountScan) {
	dir := s.Account.ConfigDir
	s.MainInstruction = filepath.Join(dir, "AGENTS.md")
	add := func(f, kind string) {
		if t, ok := readHealthText(f); ok && strings.TrimSpace(t) != "" {
			s.Instructions = append(s.Instructions, instructionFileStat{Path: f, Kind: kind, Measure: measureText(t, fileLength(f)), LinkTarget: sc.linkTarget(f)})
		}
	}
	for _, f := range grokHomeFiles(dir) {
		add(f, "main")
	}
	rules := markdownFiles(filepath.Join(dir, "rules"))
	sort.Slice(rules, func(i, j int) bool { return strings.ToLower(rules[i]) < strings.ToLower(rules[j]) })
	for _, f := range rules {
		add(f, "rule")
	}
	if grokClaudeCompat(dir) {
		for _, n := range grokClaudeCompatName {
			if f := filepath.Join(sc.home, ".claude", n); fileExists(f) {
				add(f, "compat")
			}
		}
	}
}

// ---- Antigravity ----

func (sc healthScanner) scanAntigravity(s *accountScan) {
	// agy 的資料夾是 ~/.gemini/antigravity-cli；全域規則與自訂設定在上一層 ~/.gemini/GEMINI.md 與 ~/.gemini/config/
	dir := filepath.Clean(s.Account.ConfigDir)
	gemini := filepath.Dir(dir)
	global := filepath.Join(gemini, "config")
	main := filepath.Join(gemini, "GEMINI.md")
	s.MainInstruction = main
	seen := map[string]bool{}
	for _, c := range []string{main, filepath.Join(global, "GEMINI.md"), filepath.Join(global, "AGENTS.md")} {
		k := strings.ToLower(c)
		if seen[k] || !fileExists(c) {
			continue
		}
		seen[k] = true
		if t, ok := readHealthText(c); ok && strings.TrimSpace(t) != "" {
			s.Instructions = append(s.Instructions, instructionFileStat{Path: c, Kind: "main", Measure: measureText(t, fileLength(c)), LinkTarget: sc.linkTarget(c)})
		}
	}
	for _, r := range []string{filepath.Join(global, "rules"), filepath.Join(gemini, "rules"), filepath.Join(dir, "rules")} {
		sc.scanAgyRules(s, r, seen)
	}
	agyConfig := tryReadJSONConfig(s, filepath.Join(dir, "config.json"))
	for _, h := range []string{filepath.Join(global, "hooks.json"), filepath.Join(dir, "hooks.json")} {
		if fileExists(h) {
			_ = tryReadJSONConfig(s, h)
		}
	}
	var listing strings.Builder
	skills := 0
	for _, sk := range skillFiles(filepath.Join(global, "skills")) {
		skills++
		addListing(&listing, s, sk.Name, sk.File, "skill", "user", "")
	}
	agents := len(markdownFiles(filepath.Join(global, "agents"))) + len(markdownFiles(filepath.Join(dir, "agents")))
	mcp := []string{}
	plugins := []pluginStat{}
	pluginSkills := 0
	overrides, _ := agyConfig["plugins"].(map[string]any)
	for _, pd := range subDirs(filepath.Join(global, "plugins")) {
		dn := filepath.Base(pd)
		if strings.HasPrefix(dn, ".") {
			continue
		}
		manifestPath := ""
		for _, rel := range []string{"plugin.json", ".antigravity-plugin/plugin.json", ".gemini-plugin/plugin.json", ".claude-plugin/plugin.json"} {
			if c := filepath.Join(pd, rel); fileExists(c) {
				manifestPath = c
				break
			}
		}
		if manifestPath == "" {
			continue
		}
		manifest, _, _ := readJSONObject(manifestPath, 512*1024)
		if !agyPluginEnabled(dn, manifest, overrides) {
			continue
		}
		display := firstNonBlank(jstr(manifest, "name"), dn)
		psk := skillFiles(filepath.Join(pd, "skills"))
		for _, sk := range psk {
			addListing(&listing, s, sk.Name, sk.File, "skill", "plugin", display)
		}
		pluginSkills += len(psk)
		pcmd := append(markdownFiles(filepath.Join(pd, "workflows")), markdownFiles(filepath.Join(pd, "commands"))...)
		for _, c := range pcmd {
			addListing(&listing, s, display+":"+strings.TrimSuffix(filepath.Base(c), filepath.Ext(c)), c, "command", "plugin", display)
		}
		pag := len(markdownFiles(filepath.Join(pd, "agents")))
		agents += pag
		sc.scanAgyRules(s, filepath.Join(pd, "rules"), seen)
		pm := agyMcpServers(filepath.Join(pd, "mcp_config.json"))
		for _, n := range agyMcpServers(filepath.Join(pd, ".mcp.json")) {
			dup := false
			for _, x := range pm {
				dup = dup || strings.EqualFold(x, n)
			}
			if !dup {
				pm = append(pm, n)
			}
		}
		for _, n := range pm {
			mcp = append(mcp, n+"（"+display+"）")
		}
		hooks := fileExists(filepath.Join(pd, "hooks.json")) || fileExists(filepath.Join(pd, "hooks", "hooks.json"))
		plugins = append(plugins, pluginStat{ID: display, Skills: len(psk), Commands: len(pcmd), Agent: pag, Mcp: len(pm) > 0, Hooks: hooks})
	}
	if gm := filepath.Join(global, "mcp_config.json"); fileExists(gm) {
		if obj := tryReadJSONConfig(s, gm); obj != nil {
			if servers, ok := obj["mcpServers"].(map[string]any); ok {
				var names []string
				for _, n := range sortedKeys(servers) {
					if _, isObj := servers[n].(map[string]any); isObj {
						names = append(names, n)
					}
				}
				mcp = append(names, mcp...)
			}
		}
	}
	total := skills + pluginSkills
	s.Skills = &total
	s.PluginSkills = pluginSkills
	s.Agents = agents
	s.Plugins = plugins
	s.McpServers = mcp
	s.SkillListingTokens = estimateTokens(listing.String())
}

func (sc healthScanner) scanAgyRules(s *accountScan, dir string, seen map[string]bool) {
	for _, f := range markdownFilesDeep(dir) {
		k := strings.ToLower(filepath.Clean(f))
		if seen[k] {
			continue
		}
		seen[k] = true
		t, ok := readHealthText(f)
		if !ok || strings.TrimSpace(t) == "" {
			continue
		}
		if isAntigravityScopedRule(parseFrontMatter(t)) {
			s.ScopedRules++
			continue
		}
		s.Instructions = append(s.Instructions, instructionFileStat{Path: f, Kind: "rule", Measure: measureText(t, fileLength(f)), LinkTarget: sc.linkTarget(f)})
	}
}

func agyPluginEnabled(dirName string, manifest, overrides map[string]any) bool {
	if e, ok := overrides[dirName].(map[string]any); ok {
		if b, ok := e["enabled"].(bool); ok {
			return b
		}
	}
	if b, ok := manifest["disabled"].(bool); ok && b {
		return false
	}
	return true
}

func agyMcpServers(path string) []string {
	root, ok, _ := readJSONObject(path, 1<<20)
	if !ok {
		return nil
	}
	servers, _ := root["mcpServers"].(map[string]any)
	var out []string
	for _, n := range sortedKeys(servers) {
		if _, isObj := servers[n].(map[string]any); isObj {
			out = append(out, n)
		}
	}
	return out
}

// tryReadJSONConfig：讀一個 JSON 設定；讀不懂時記下第一個錯誤（設定檔那一項）
func tryReadJSONConfig(s *accountScan, path string) map[string]any {
	if !fileExists(path) {
		return nil
	}
	if s.SettingsPath == "" {
		s.SettingsPath = path
	}
	obj, _, errText := readJSONObject(path, healthMaxReadBytes)
	if errText != "" && s.SettingsError == "" {
		s.SettingsPath, s.SettingsError = path, errText
	}
	return obj
}

// ---- 電腦層級的指示檔 ----

func (sc healthScanner) buildDocs(accounts []healthAccountInput) []*instructionDoc {
	docs := map[string]*instructionDoc{}
	var order []string
	for _, a := range accounts {
		// 共用系統預設帳戶設定的隔離帳戶：指示檔每次啟動前與來源同步，不另外列
		if a.SharedSource != "" {
			continue
		}
		var path string
		var files []string
		var max int64
		switch a.Provider {
		case "Claude":
			path = filepath.Join(a.ConfigDir, "CLAUDE.md")
			files = []string{path}
		case "Codex":
			path = codexInstructionFile(a.ConfigDir)
			files = []string{path}
		case "Grok":
			path = filepath.Join(a.ConfigDir, "AGENTS.md")
			files = grokHomeFiles(a.ConfigDir)
		case "Antigravity":
			gemini := filepath.Dir(filepath.Clean(a.ConfigDir))
			path = filepath.Join(gemini, "GEMINI.md")
			files = []string{path, filepath.Join(gemini, "config", "GEMINI.md"), filepath.Join(gemini, "config", "AGENTS.md")}
			max = antigravityFileMaxBytes
		default:
			continue
		}
		key := strings.ToLower(filepath.Clean(path))
		if d := docs[key]; d != nil {
			d.AccountIDs = append(d.AccountIDs, a.ID)
			continue
		}
		var texts []string
		exists := false
		var bytes int64
		for _, f := range files {
			if !fileExists(f) {
				continue
			}
			exists = true
			bytes += fileLength(f)
			if t, ok := readHealthText(f); ok && strings.TrimSpace(t) != "" {
				texts = append(texts, t)
			}
		}
		text := strings.Join(texts, "\n\n")
		label := providerNames[a.Provider]
		if !a.IsSystemDefault {
			label += "・" + a.Label
		}
		d := &instructionDoc{Key: key, Provider: a.Provider, Label: label, Path: path, Exists: exists, Text: text,
			Measure: measureText(text, bytes), MaxBytes: max, AccountIDs: []string{a.ID}}
		if fileExists(path) {
			d.LinkTarget = sc.linkTarget(path)
		}
		if a.Provider == "Grok" && grokClaudeCompat(a.ConfigDir) {
			d.AlsoLoadsKey = strings.ToLower(filepath.Join(sc.home, ".claude", "CLAUDE.md"))
		}
		docs[key] = d
		order = append(order, key)
	}
	out := make([]*instructionDoc, 0, len(order))
	for _, k := range order {
		d := docs[k]
		// Grok 的 Claude 相容指向的檔不在清單裡就不算
		if d.AlsoLoadsKey != "" && docs[d.AlsoLoadsKey] == nil {
			d.AlsoLoadsKey = ""
		}
		out = append(out, d)
	}
	providerOrder := func(p string) int {
		for i, x := range providerKeys {
			if x == p {
				return i
			}
		}
		return len(providerKeys)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if pi, pj := providerOrder(out[i].Provider), providerOrder(out[j].Provider); pi != pj {
			return pi < pj
		}
		return out[i].Label < out[j].Label
	})
	return out
}
