package agent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 帳戶健康度的門檻與評估（照 C# 的 Health/HealthRules.cs、HealthModels.cs、SkillHealthRules.cs；純函式）：每次對話都會載入的全域指示、
// Claude 自動記憶的索引、skill 清單、MCP 伺服器、外掛、設定檔。門檻取自各家文件：Claude Code 建議每份 CLAUDE.md 200 行以內、
// 單檔 40,000 字元以上啟動時警告、MEMORY.md 只載入前 200 行或 25KB、skill 清單的預算約為脈絡的 1%；Antigravity 的規則檔單檔
// 24,000 位元組（超過截斷）。token 數一律是粗估。
// 尚未移植：Claude CLI 自己回報的清單（get_skills_dialog、hooks、自動記憶開關）——Go 版只看掃描到的檔案。

// healthLevel：健康度的等級（網頁的 level：ok／info／warn／bad）；info 只是說明，不拉低整體
type healthLevel int

const (
	healthOK healthLevel = iota
	healthInfo
	healthWarn
	healthBad
)

func (l healthLevel) name() string {
	switch l {
	case healthInfo:
		return "info"
	case healthWarn:
		return "warn"
	case healthBad:
		return "bad"
	}
	return "ok"
}

func healthRank(l healthLevel) int {
	switch l {
	case healthWarn:
		return 1
	case healthBad:
		return 2
	}
	return 0
}

// worstHealth：整體等級：info 不拉低，取最差的
func worstHealth(levels ...healthLevel) healthLevel {
	w := healthOK
	for _, l := range levels {
		if healthRank(l) > healthRank(w) {
			w = l
		}
	}
	return w
}

const (
	instructionWarnTokens     = 5_000  // 全域指示合計超過這個 token 數算偏大
	instructionBadTokens      = 15_000 // 全域指示合計超過這個 token 數算過大
	instructionWarnLines      = 200    // Claude 官方建議每份 CLAUDE.md 的行數上限
	claudeFileWarnChars       = 40_000 // Claude Code 對單一指示檔的字元數警告
	antigravityFileMaxBytes   = 24_000 // Antigravity 規則檔單檔上限（位元組，超過截斷）
	memoryIndexMaxLines       = 200    // Claude 自動記憶索引只載入的行數
	memoryIndexMaxBytes       = 25_000 // Claude 自動記憶索引只載入的大小（位元組）
	memoryNearRatio           = 0.9    // 到上限的這個比例以上算接近上限
	skillDescriptionCap       = 1_536  // skill 清單的描述單筆上限（字元）
	skillListingBudgetChars   = 8_000  // 200K 脈絡的 skill 清單預算（約 1%）
	skillListingBudgetLarge   = 40_000 // 1M 脈絡的 skill 清單預算
	mcpWarnServers            = 10     // MCP 伺服器到這個數量算偏多
	claudeStateWarnBytes      = 5 * 1024 * 1024
	healthContextTokens       = 200_000
	healthMaxListedFiles      = 12
	skillShortDescriptionToks = 8   // 描述粗估不到這個 token 數算太短
	skillBodyWarnLines        = 500 // SKILL.md 本文建議的行數上限
	skillPurposeMaxChars      = 160
)

// ---- 掃描的資料 ----

// healthAccountInput：要分析的一個本機帳戶（在鎖內從 Account 取的快照；資料夾都已解析好）
type healthAccountInput struct {
	ID, Label, Provider string
	IsSystemDefault     bool
	ConfigDir           string
	SafeMode            bool
	SharedSource        string // 共用系統預設帳戶的設定時，來源的設定資料夾；不共用是空字串
	ClaudeGlobalConfig  string // Claude 的 .claude.json（使用者層 MCP 伺服器在這裡）；其他種類是空字串
}

// instructionFileStat：一個每次對話都會載入的指示檔。Kind：main、import、rule、compat
type instructionFileStat struct {
	Path, Kind string
	Measure    textMeasure
	LinkTarget string
}

type memoryIndexStat struct {
	Project, Path string
	Lines         int
	Bytes         int64
}

type pluginStat struct {
	ID                      string
	Skills, Commands, Agent int
	Mcp, Hooks, Missing     bool
}

// skillStat：掃描到的一個 skill 或斜線指令。Kind：skill／command；Source：user／plugin／synced
type skillStat struct {
	Name, Kind, Source, Plugin, File string
	Readable, HasFrontMatter         bool
	DeclaredName                     string
	Description, WhenToUse           string
	BodyLines                        int
	MissingRefs                      []string
	FrontMatterProblem               string
}

// accountScan：一個帳戶的量測。nil 的切片＝這種代理不分析這一項
type accountScan struct {
	Account            healthAccountInput
	Instructions       []instructionFileStat
	MainInstruction    string
	ScopedRules        int
	MissingImports     []string
	MemoryIndexes      []memoryIndexStat // nil＝這種代理沒有
	Skills             *int
	SkillList          []skillStat
	PluginSkills       int
	SkillListingChars  int
	SkillListingTokens int
	Agents             int
	McpServers         []string     // nil＝不分析
	Plugins            []pluginStat // nil＝沒有
	SettingsError      string
	SettingsPath       string
	StateFileBytes     int64
	StateFilePath      string
}

// ---- 評估的結果 ----

type healthFileView struct {
	Path   string  `json:"path"`
	Label  string  `json:"label"`
	Note   string  `json:"note"`
	Level  string  `json:"level"`
	Target *string `json:"target"`
}

type healthItem struct {
	Key, Title string
	Level      healthLevel
	Value      string
	Detail     string
	Files      []healthFileView
	Headline   string
	Skills     []skillView // 只有 skills 那一項；nil＝不帶這個鍵
}

type accountHealth struct {
	AccountID     string
	Level         healthLevel
	Summary       string
	Load          string
	Items         []healthItem
	CanTrim       bool
	CanTrimMemory bool
}

func fileView(path, label, note string, level healthLevel, target string) healthFileView {
	return healthFileView{Path: path, Label: label, Note: note, Level: level.name(), Target: strPtr(target)}
}

// healthInstructionFileName：各代理的全域指示主檔名
func healthInstructionFileName(p string) string {
	switch p {
	case "Claude":
		return "CLAUDE.md"
	case "Antigravity":
		return "GEMINI.md"
	}
	return "AGENTS.md"
}

// instructionsLoaded：精簡模式下 Claude、Codex 不載入全域指示；Antigravity 的 --disable-slash-commands 仍載入 GEMINI.md
func instructionsLoaded(s *accountScan) bool {
	return !(s.Account.SafeMode && (s.Account.Provider == "Claude" || s.Account.Provider == "Codex"))
}

// skillsLoaded：Antigravity 精簡模式不載入斜線指令與 skill 清單
func skillsLoaded(s *accountScan) bool {
	return !(s.Account.SafeMode && s.Account.Provider == "Antigravity")
}

// evaluateHealth：評估一個帳戶
func evaluateHealth(s *accountScan) accountHealth {
	items := []healthItem{evaluateInstructions(s)}
	if s.MemoryIndexes != nil {
		items = append(items, evaluateMemory(s))
	}
	if s.Skills != nil {
		it := evaluateSkills(s)
		if skillsLoaded(s) {
			it = withSkillList(it, evaluateSkillViews(s))
		}
		items = append(items, it)
	}
	if s.McpServers != nil {
		items = append(items, evaluateMcp(s))
	}
	if s.Plugins != nil {
		items = append(items, evaluatePlugins(s))
	}
	if s.SettingsPath != "" || s.StateFilePath != "" {
		items = append(items, evaluateSettings(s))
	}
	levels := make([]healthLevel, len(items))
	for i, it := range items {
		levels[i] = it.Level
	}
	level := worstHealth(levels...)
	summary := "良好"
	if level != healthOK {
		var hs []string
		for _, it := range items {
			if it.Level == level && it.Headline != "" && len(hs) < 2 {
				hs = append(hs, it.Headline)
			}
		}
		summary = strings.Join(hs, "、")
	}
	var parts []string
	if instructionsLoaded(s) {
		t := 0
		for _, f := range s.Instructions {
			t += f.Measure.Tokens
		}
		if t > 0 {
			parts = append(parts, formatHealthTokens(t)+" 指示")
		}
	}
	if skillsLoaded(s) && s.SkillListingTokens > 0 {
		parts = append(parts, formatHealthTokens(s.SkillListingTokens)+" skill 清單")
	}
	canTrimMemory := false
	for _, m := range s.MemoryIndexes {
		canTrimMemory = canTrimMemory || memoryNeedsTrim(m)
	}
	return accountHealth{AccountID: s.Account.ID, Level: level, Summary: summary, Load: strings.Join(parts, "＋"), Items: items,
		CanTrim: items[0].Level == healthWarn || items[0].Level == healthBad, CanTrimMemory: canTrimMemory}
}

func healthPercent(tokens int) string {
	p := float64(tokens) * 100 / healthContextTokens
	if p < 1 {
		return oneDecimal(p) + "%"
	}
	return strconv.FormatFloat(float64(int64(p+0.5)), 'f', 0, 64) + "%"
}

func instructionKindLabel(k string) string {
	switch k {
	case "main":
		return "主檔"
	case "import":
		return "匯入"
	case "rule":
		return "規則"
	case "compat":
		return "Claude 相容"
	}
	return k
}

func evaluateInstructions(s *accountScan) healthItem {
	p := s.Account.Provider
	name := healthInstructionFileName(p)
	const title = "全域指示"
	if !instructionsLoaded(s) {
		return healthItem{Key: "instructions", Title: title, Level: healthInfo, Value: "精簡模式不載入",
			Detail: "這個帳戶開著精簡模式，對話不載入 " + name + "、外掛、hooks 與 MCP。", Files: []healthFileView{}}
	}
	files := s.Instructions
	if len(files) == 0 {
		fv := []healthFileView{}
		if s.MainInstruction != "" {
			fv = append(fv, fileView(s.MainInstruction, name, "不存在", healthInfo, ""))
		}
		return healthItem{Key: "instructions", Title: title, Level: healthInfo, Value: "沒有",
			Detail: "這個帳戶沒有使用者層的 " + name + "，對話不會帶任何電腦層級的指示。要讓這台的各個 CLI 遵守同一套習慣，可以在下方「電腦層級的指示檔」合併。",
			Files:  fv}
	}
	tokens, lines := 0, 0
	views := []healthFileView{}
	var reasons []string
	level := healthOK
	for _, f := range files {
		tokens += f.Measure.Tokens
		lines += f.Measure.Lines
		fl := healthOK
		fn := filepath.Base(f.Path)
		switch {
		case p == "Claude" && f.Measure.Chars > claudeFileWarnChars:
			fl = healthBad
			reasons = append(reasons, fmt.Sprintf("%s 有 %s 字元，超過 Claude Code 的 %s 字元門檻（啟動時會警告「Large CLAUDE.md」）。", fn, formatCount(int64(f.Measure.Chars)), formatCount(claudeFileWarnChars)))
		case p == "Antigravity" && f.Measure.Bytes > antigravityFileMaxBytes:
			fl = healthBad
			reasons = append(reasons, fmt.Sprintf("%s 有 %s，超過 Antigravity 單檔 %s 位元組的上限，後面的內容會被截掉。", fn, formatBytes(f.Measure.Bytes), formatCount(antigravityFileMaxBytes)))
		case f.Measure.Lines > instructionWarnLines:
			fl = healthWarn
			reasons = append(reasons, fmt.Sprintf("%s 有 %s 行，Claude 官方建議每份在 %d 行以內。", fn, formatCount(int64(f.Measure.Lines)), instructionWarnLines))
		}
		level = worstHealth(level, fl)
		views = append(views, fileView(f.Path, instructionKindLabel(f.Kind)+"　"+fn,
			formatCount(int64(f.Measure.Lines))+" 行・"+formatHealthTokens(f.Measure.Tokens), fl, f.LinkTarget))
	}
	if tokens > instructionBadTokens {
		level = healthBad
	} else if tokens > instructionWarnTokens {
		level = worstHealth(level, healthWarn)
	}
	if len(s.MissingImports) > 0 {
		level = worstHealth(level, healthWarn)
		shown := s.MissingImports
		more := ""
		if len(shown) > 5 {
			shown, more = shown[:5], " 等"
		}
		reasons = append(reasons, "找不到這些 @ 匯入的檔案："+strings.Join(shown, "、")+more+"。")
	}
	detail := []string{fmt.Sprintf("每次對話開始都會載入這些指示，合計%s，約占 200K 脈絡的 %s。", formatHealthTokens(tokens), healthPercent(tokens))}
	detail = append(detail, reasons...)
	if level == healthWarn || level == healthBad {
		if tokens > instructionWarnTokens {
			detail = append(detail, fmt.Sprintf("合計超過 %s token：指示越長越佔脈絡，也越容易有些規則沒被遵守。", formatCount(instructionWarnTokens)))
		} else {
			detail = append(detail, "指示越長越佔脈絡，也越容易有些規則沒被遵守。")
		}
		if p == "Claude" {
			detail = append(detail, "建議：只在特定情境需要的細節搬到另外的文件，指示檔只留一行「情境 → 檔案」的索引；或改成 ~/.claude/rules/ 裡有 paths 的規則，碰到相符的檔案才載入。@ 匯入不會減少載入量。")
		} else {
			detail = append(detail, "建議：只在特定情境需要的細節搬到另外的文件，指示檔只留一行「情境 → 檔案」的索引。")
		}
	}
	if s.ScopedRules > 0 {
		if p == "Antigravity" {
			detail = append(detail, fmt.Sprintf("另有 %d 個按需觸發的規則（model_decision／glob），預設不佔固定脈絡（不計入）。", s.ScopedRules))
		} else {
			detail = append(detail, fmt.Sprintf("另有 %d 個有 paths 的規則，只在碰到相符的檔案時才載入（不計入）。", s.ScopedRules))
		}
	}
	value := formatCount(int64(lines)) + " 行・" + formatHealthTokens(tokens)
	if len(files) > 1 {
		value += "・" + strconv.Itoa(len(files)) + " 個檔"
	}
	// 摘要以最大的那個檔命名（Grok 的大小多半來自 Claude 相容讀的 CLAUDE.md）
	largest := files[0]
	for _, f := range files[1:] {
		if f.Measure.Tokens > largest.Measure.Tokens {
			largest = f
		}
	}
	headline := ""
	switch level {
	case healthBad:
		headline = filepath.Base(largest.Path) + " 過大"
	case healthWarn:
		if len(s.MissingImports) > 0 && tokens <= instructionWarnTokens {
			headline = "匯入的檔案找不到"
		} else {
			headline = filepath.Base(largest.Path) + " 偏大"
		}
	}
	return healthItem{Key: "instructions", Title: title, Level: level, Value: value, Detail: strings.Join(detail, "\n"), Files: views, Headline: headline}
}

// memoryOverLimit：記憶索引超過載入上限（超過的部分不會進入對話）
func memoryOverLimit(m memoryIndexStat) bool {
	return m.Lines > memoryIndexMaxLines || m.Bytes > memoryIndexMaxBytes
}

// memoryNearLimit：到上限的九成以上（還沒超過）
func memoryNearLimit(m memoryIndexStat) bool {
	return !memoryOverLimit(m) && (float64(m.Lines) >= memoryIndexMaxLines*memoryNearRatio || float64(m.Bytes) >= memoryIndexMaxBytes*memoryNearRatio)
}

// memoryNeedsTrim：超過或接近上限：該整理了
func memoryNeedsTrim(m memoryIndexStat) bool { return memoryOverLimit(m) || memoryNearLimit(m) }

func memoryNote(m memoryIndexStat) string {
	return formatCount(int64(m.Lines)) + " 行・" + formatBytes(m.Bytes)
}

func evaluateMemory(s *accountScan) healthItem {
	const title = "自動記憶索引"
	idx := s.MemoryIndexes
	shared := ""
	if s.Account.SharedSource != "" {
		shared = "這個帳戶共用系統預設帳戶的記憶。"
	}
	if len(idx) == 0 {
		return healthItem{Key: "memory", Title: title, Level: healthOK, Value: "還沒有", Detail: "還沒有任何專案的自動記憶（MEMORY.md）。" + shared, Files: []healthFileView{}}
	}
	var over, near []memoryIndexStat
	for _, m := range idx {
		if memoryOverLimit(m) {
			over = append(over, m)
		} else if memoryNearLimit(m) {
			near = append(near, m)
		}
	}
	byBytes := func(l []memoryIndexStat) {
		sort.SliceStable(l, func(i, j int) bool { return l[i].Bytes > l[j].Bytes })
	}
	byBytes(over)
	byBytes(near)
	level := healthOK
	value := strconv.Itoa(len(idx)) + " 個專案"
	switch {
	case len(over) > 0:
		level = healthBad
		value += "・" + strconv.Itoa(len(over)) + " 個超過上限"
	case len(near) > 0:
		level = healthWarn
		value += "・" + strconv.Itoa(len(near)) + " 個接近上限"
	}
	files := []healthFileView{}
	for _, m := range over {
		files = append(files, fileView(m.Path, m.Project, memoryNote(m), healthBad, ""))
	}
	for _, m := range near {
		files = append(files, fileView(m.Path, m.Project, memoryNote(m), healthWarn, ""))
	}
	if len(files) > healthMaxListedFiles {
		files = files[:healthMaxListedFiles]
	}
	largest := idx[0]
	for _, m := range idx[1:] {
		if m.Bytes > largest.Bytes {
			largest = m
		}
	}
	detail := []string{fmt.Sprintf("每個專案的 MEMORY.md 只載入前 %d 行或 25KB（先到者為準），超過的部分不會進入對話。", memoryIndexMaxLines) + shared}
	headline := ""
	switch {
	case len(over) > 0:
		detail = append(detail, "超過上限的索引後面幾條記憶等於沒記：可以用代理整理——把索引改成一行一條、細節移到主題檔，合併或刪掉過時的條目。")
		headline = "記憶索引超過上限"
	case len(near) > 0:
		detail = append(detail, "接近上限：再記幾條就會開始被截掉，建議先整理。")
		headline = "記憶索引接近上限"
	default:
		detail = append(detail, "最大的是 "+largest.Project+"（"+memoryNote(largest)+"）。")
	}
	return healthItem{Key: "memory", Title: title, Level: level, Value: value, Detail: strings.Join(detail, "\n"), Files: files, Headline: headline}
}

func evaluateSkills(s *accountScan) healthItem {
	const title = "Skills"
	if !skillsLoaded(s) {
		return healthItem{Key: "skills", Title: title, Level: healthInfo, Value: "精簡模式不載入",
			Detail: "這個帳戶開著精簡模式（--disable-slash-commands），對話不載入斜線指令與 skill 清單。", Files: []healthFileView{}}
	}
	count := *s.Skills
	value := "沒有"
	if count > 0 {
		value = strconv.Itoa(count) + " 個・清單 " + formatCount(int64(s.SkillListingChars)) + " 字元"
	}
	if s.Agents > 0 {
		value += "・" + strconv.Itoa(s.Agents) + " 個子代理"
	}
	if count == 0 {
		return healthItem{Key: "skills", Title: title, Level: healthOK, Value: value, Detail: "沒有使用者層或外掛的 skill。", Files: []healthFileView{}}
	}
	plugin := ""
	if s.PluginSkills > 0 {
		plugin = fmt.Sprintf("其中 %d 個是外掛帶來的。", s.PluginSkills)
	}
	head := fmt.Sprintf("skill 的名稱與描述每一輪都在脈絡裡（%s）。", formatHealthTokens(s.SkillListingTokens)) + plugin
	if s.Account.Provider != "Claude" {
		return healthItem{Key: "skills", Title: title, Level: healthInfo, Value: value, Detail: head, Files: []healthFileView{}}
	}
	over := s.SkillListingChars > skillListingBudgetChars
	overLarge := s.SkillListingChars > skillListingBudgetLarge
	detail := []string{head}
	switch {
	case overLarge:
		detail = append(detail, fmt.Sprintf("Claude Code 的清單預算約為脈絡的 1%%，連 1M 脈絡的預算（約 %s 字元）都超過了：較少用的 skill 會被拿掉描述，Claude 就比較認不出什麼時候該用它。", formatCount(skillListingBudgetLarge)))
	case over:
		detail = append(detail, fmt.Sprintf("Claude Code 的清單預算約為脈絡的 1%%：200K 脈絡的模型（約 %s 字元）已經超過，較少用的 skill 會被拿掉描述，Claude 就比較認不出什麼時候該用它；1M 脈絡（約 %s 字元）還在範圍內。", formatCount(skillListingBudgetChars), formatCount(skillListingBudgetLarge)))
	default:
		detail = append(detail, fmt.Sprintf("Claude Code 的清單預算約為脈絡的 1%%（200K 約 %s 字元、1M 約 %s 字元），目前在範圍內。", formatCount(skillListingBudgetChars), formatCount(skillListingBudgetLarge)))
	}
	level, headline := healthOK, ""
	if over {
		detail = append(detail, "建議：停用用不到的外掛，或在 settings.json 的 skillOverrides 把少用的設成 name-only。")
		level, headline = healthWarn, "skill 清單偏大"
		if overLarge {
			headline = "skill 清單超過預算"
		}
	}
	return healthItem{Key: "skills", Title: title, Level: level, Value: value, Detail: strings.Join(detail, "\n"), Files: []healthFileView{}, Headline: headline}
}

// withSkillList：skills 那一項附上逐一的清單；使用者自己的 skill 有問題時拉高這一項的等級（外掛與 claude.ai 的只列出、不拉低）
func withSkillList(it healthItem, skills []skillView) healthItem {
	it.Skills = skills
	if len(skills) == 0 {
		return it
	}
	flagged := 0
	var owned []skillView
	for _, s := range skills {
		if s.level == healthWarn || s.level == healthBad {
			flagged++
			if s.Source == "user" {
				owned = append(owned, s)
			}
		}
	}
	line := fmt.Sprintf("下方逐一列出 %d 個使用中的 skill 與用途", len(skills))
	if flagged > 0 {
		line += fmt.Sprintf("，%d 個需要注意", flagged)
	} else {
		line += "，都沒有發現問題"
	}
	line += "。"
	if flagged > len(owned) {
		line += "外掛與 claude.ai 的 skill 不是你能直接改的，只列出、不計入等級。"
	}
	if len(owned) > 0 {
		var ls []healthLevel
		for _, s := range owned {
			ls = append(ls, s.level)
		}
		w := worstHealth(ls...)
		if healthRank(w) > healthRank(it.Level) {
			it.Level = w
			word := "需要注意"
			if w == healthBad {
				word = "有問題"
			}
			if len(owned) == 1 {
				it.Headline = "skill「" + owned[0].Label + "」" + word
			} else {
				it.Headline = fmt.Sprintf("%d 個 skill %s", len(owned), word)
			}
		}
	}
	it.Detail += "\n" + line
	return it
}

func evaluateMcp(s *accountScan) healthItem {
	const title = "MCP 伺服器"
	servers := s.McpServers
	if s.Account.SafeMode && s.Account.Provider == "Claude" {
		return healthItem{Key: "mcp", Title: title, Level: healthInfo, Value: "精簡模式不載入", Detail: "精簡模式下不啟動 MCP 伺服器。", Files: []healthFileView{}}
	}
	if len(servers) == 0 {
		return healthItem{Key: "mcp", Title: title, Level: healthOK, Value: "沒有", Detail: "沒有使用者層或外掛的 MCP 伺服器。", Files: []healthFileView{}}
	}
	shown, more := servers, ""
	if len(shown) > 20 {
		shown, more = shown[:20], " 等"
	}
	many := len(servers) >= mcpWarnServers
	detail := []string{"使用者層與外掛的 MCP 伺服器：" + strings.Join(shown, "、") + more + "。",
		"每個對話開始時都會啟動這些伺服器；工具定義大多延後載入，但伺服器多了會拖慢啟動、也多佔記憶體。"}
	level, headline := healthOK, ""
	if many {
		detail = append(detail, "建議：停用不常用的伺服器，或改成只在需要的專案（.mcp.json）啟用。")
		level, headline = healthWarn, "MCP 伺服器偏多"
	}
	return healthItem{Key: "mcp", Title: title, Level: level, Value: strconv.Itoa(len(servers)) + " 個", Detail: strings.Join(detail, "\n"), Files: []healthFileView{}, Headline: headline}
}

func evaluatePlugins(s *accountScan) healthItem {
	const title = "外掛"
	plugins := s.Plugins
	if s.Account.SafeMode && s.Account.Provider != "Antigravity" {
		return healthItem{Key: "plugins", Title: title, Level: healthInfo, Value: "精簡模式不載入", Detail: "精簡模式下不載入外掛。", Files: []healthFileView{}}
	}
	if len(plugins) == 0 {
		return healthItem{Key: "plugins", Title: title, Level: healthOK, Value: "沒有", Detail: "沒有啟用的外掛。", Files: []healthFileView{}}
	}
	var installed []pluginStat
	var missing []string
	sk, cm, ag, mc, hk := 0, 0, 0, 0, 0
	for _, p := range plugins {
		if p.Missing {
			missing = append(missing, p.ID)
			continue
		}
		installed = append(installed, p)
		sk, cm, ag = sk+p.Skills, cm+p.Commands, ag+p.Agent
		if p.Mcp {
			mc++
		}
		if p.Hooks {
			hk++
		}
	}
	detail := []string{fmt.Sprintf("啟用 %d 個，帶來 %d 個 skill、%d 個斜線指令、%d 個子代理、%d 個附 MCP、%d 個附 hooks。", len(plugins), sk, cm, ag, mc, hk)}
	sort.SliceStable(installed, func(i, j int) bool {
		return installed[i].Skills+installed[i].Commands+installed[i].Agent > installed[j].Skills+installed[j].Commands+installed[j].Agent
	})
	var heavy []string
	for _, p := range installed {
		if n := p.Skills + p.Commands + p.Agent; n > 0 && len(heavy) < 5 {
			heavy = append(heavy, fmt.Sprintf("%s（%d）", p.ID, n))
		}
	}
	if len(heavy) > 0 {
		detail = append(detail, "帶最多 skill、指令與子代理的："+strings.Join(heavy, "、")+"。")
	}
	level, headline := healthInfo, ""
	if len(missing) > 0 {
		detail = append(detail, "settings.json 啟用了但這台沒有安裝："+strings.Join(missing, "、")+"。")
		level, headline = healthWarn, "有啟用卻沒安裝的外掛"
	}
	return healthItem{Key: "plugins", Title: title, Level: level, Value: strconv.Itoa(len(plugins)) + " 個啟用", Detail: strings.Join(detail, "\n"), Files: []healthFileView{}, Headline: headline}
}

func evaluateSettings(s *accountScan) healthItem {
	const title = "設定檔"
	files := []healthFileView{}
	var detail []string
	level, headline := healthOK, ""
	name := "settings.json"
	if s.SettingsPath != "" {
		name = filepath.Base(s.SettingsPath)
	}
	if s.SettingsError != "" {
		level, headline = healthBad, name+" 讀不懂"
		detail = append(detail, name+" 讀不懂："+s.SettingsError+"。CLI 可能忽略整份設定（權限、外掛、環境變數都不生效）。")
		if s.SettingsPath != "" {
			files = append(files, fileView(s.SettingsPath, name, "讀不懂", healthBad, ""))
		}
	}
	if s.StateFilePath != "" {
		big := s.StateFileBytes > claudeStateWarnBytes
		fl := healthOK
		if big {
			level, fl = worstHealth(level, healthWarn), healthWarn
			if headline == "" {
				headline = ".claude.json 過大"
			}
			detail = append(detail, ".claude.json 有 "+formatBytes(s.StateFileBytes)+"（各專案的紀錄累積下來的），CLI 每次啟動與結束都要讀寫它，太大會拖慢啟動。可以刪掉 projects 裡已經不用的專案。")
		}
		files = append(files, fileView(s.StateFilePath, ".claude.json", formatBytes(s.StateFileBytes), fl, ""))
	}
	if len(detail) == 0 {
		detail = append(detail, "設定檔讀得懂，大小正常。")
	}
	value := "正常"
	switch level {
	case healthBad:
		value = "讀不懂"
	case healthWarn:
		value = "偏大"
	}
	return healthItem{Key: "settings", Title: title, Level: level, Value: value, Detail: strings.Join(detail, "\n"), Files: files, Headline: headline}
}

// renderAccountHealth：AccountHealthView
func renderAccountHealth(a accountHealth) map[string]any {
	items := make([]any, 0, len(a.Items))
	for _, it := range a.Items {
		m := map[string]any{"key": it.Key, "title": it.Title, "level": it.Level.name(), "value": it.Value, "detail": it.Detail, "files": it.Files}
		if it.Skills != nil {
			m["skills"] = it.Skills
		}
		items = append(items, m)
	}
	return map[string]any{"level": a.Level.name(), "summary": a.Summary, "load": nilIfEmpty(a.Load), "canTrim": a.CanTrim,
		"canTrimMemory": a.CanTrimMemory, "items": items}
}

// ---- 逐一的 skill（SkillHealthRules） ----

type skillIssue struct {
	Level string `json:"level"`
	Text  string `json:"text"`
	level healthLevel
}

// skillView：SkillHealthView
type skillView struct {
	Name        string       `json:"name"`
	Label       string       `json:"label"`
	Kind        string       `json:"kind"`
	Source      string       `json:"source"`
	Plugin      *string      `json:"plugin"`
	Path        *string      `json:"path"`
	Purpose     string       `json:"purpose"`
	Description string       `json:"description"`
	Tokens      int          `json:"tokens"`
	Listed      bool         `json:"listed"`
	Level       string       `json:"level"`
	Issues      []skillIssue `json:"issues"`
	level       healthLevel
}

var (
	skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	spacesPattern    = regexp.MustCompile(`\s+`)
)

func validSkillName(name string) bool { return len(name) <= 64 && skillNamePattern.MatchString(name) }

func fullSkillDescription(s skillStat) string {
	if s.WhenToUse != "" {
		return strings.TrimSpace(s.Description + " " + s.WhenToUse)
	}
	return s.Description
}

// evaluateSkillViews：使用中的 skill（照掃描到的）。排序：有問題的在前，再來使用者自己的、指令、claude.ai、外掛，最後照名稱
func evaluateSkillViews(s *accountScan) []skillView {
	type draft struct {
		name string
		stat skillStat
	}
	drafts := make([]draft, 0, len(s.SkillList))
	for _, st := range s.SkillList {
		n := st.Name
		if st.Plugin != "" {
			n = st.Plugin + ":" + st.Name
		}
		drafts = append(drafts, draft{n, st})
	}
	unqualified := func(d draft) string {
		n := d.name
		if d.stat.DeclaredName != "" {
			n = d.stat.DeclaredName
		}
		if i := strings.LastIndexByte(n, ':'); i >= 0 {
			n = n[i+1:]
		}
		return strings.ToLower(n)
	}
	groups := map[string][]string{}
	for _, d := range drafts {
		k := unqualified(d)
		groups[k] = append(groups[k], d.name)
	}
	out := make([]skillView, 0, len(drafts))
	for _, d := range drafts {
		st := d.stat
		desc := fullSkillDescription(st)
		var issues []skillIssue
		add := func(l healthLevel, t string) { issues = append(issues, skillIssue{Level: l.name(), Text: t, level: l}) }
		if !st.Readable {
			add(healthWarn, "讀不到 "+filepath.Base(st.File)+"，無法檢查內容。")
		} else {
			if st.Kind == "skill" && !st.HasFrontMatter {
				add(healthBad, "SKILL.md 開頭沒有前置資料（--- 包起來的 name、description），CLI 拿不到描述。")
			}
			if desc == "" {
				if st.Kind == "skill" {
					add(healthBad, "沒有描述：模型只看得到名稱，不知道什麼時候該用它。")
				} else {
					add(healthInfo, "沒有描述：清單裡只有名稱。")
				}
			}
			if st.FrontMatterProblem != "" {
				add(healthWarn, "前置資料的 YAML 寫法容易讀錯（"+st.FrontMatterProblem+"），有些工具會讀不到描述；把值用引號包起來。")
			}
			if st.BodyLines > skillBodyWarnLines {
				add(healthWarn, fmt.Sprintf("本文 %s 行（建議 %d 行以內）：用到時整份載入脈絡，細節可以拆到參考檔、需要時再讀。", formatCount(int64(st.BodyLines)), skillBodyWarnLines))
			}
			if len(st.MissingRefs) > 0 {
				add(healthWarn, "連結的檔案不存在："+strings.Join(st.MissingRefs, "、")+"。")
			}
			if st.Kind == "skill" && st.Source == "user" && st.DeclaredName != "" && !validSkillName(st.DeclaredName) {
				add(healthInfo, "名稱「"+st.DeclaredName+"」不合 Agent Skills 的規範（小寫英數與連字號、64 字元內），其他工具可能不認得。")
			}
		}
		if desc != "" {
			if n := countChars(desc); n > skillDescriptionCap {
				add(healthWarn, fmt.Sprintf("描述 %s 字，超過 %s 字的部分會被截掉。", formatCount(int64(n)), formatCount(skillDescriptionCap)))
			} else if st.Kind == "skill" && estimateTokens(desc) < skillShortDescriptionToks {
				add(healthWarn, "描述太短（「"+desc+"」）：模型很難判斷什麼時候該用它，寫清楚做什麼、什麼情況下用。")
			}
		}
		if same := groups[unqualified(d)]; len(same) > 1 {
			var others []string
			for _, n := range same {
				if n != d.name {
					others = append(others, n)
				}
			}
			if len(others) > 0 {
				add(healthWarn, "和「"+strings.Join(others, "」「")+"」同名，兩份描述都佔 skill 清單；用不到的那一份可以停用。")
			}
		}
		levels := make([]healthLevel, len(issues))
		for i, is := range issues {
			levels[i] = is.level
		}
		level := worstHealth(levels...)
		own := st.Name
		if st.Kind == "skill" && st.DeclaredName != "" {
			own = st.DeclaredName
		}
		label := own
		if st.Plugin != "" {
			label = st.Plugin + ":" + own
		}
		if st.Kind == "command" {
			label = "/" + label
		}
		listDesc := truncateChars(desc, skillDescriptionCap)
		tokens := estimateTokens(firstNonBlank(st.DeclaredName, st.Name) + ": " + listDesc)
		shown := desc
		if countChars(desc) > skillDescriptionCap {
			shown = listDesc + "…"
		}
		if issues == nil {
			issues = []skillIssue{}
		}
		out = append(out, skillView{Name: d.name, Label: label, Kind: st.Kind, Source: st.Source, Plugin: strPtr(st.Plugin), Path: strPtr(st.File),
			Purpose: skillPurpose(shown), Description: shown, Tokens: tokens, Listed: true, Level: level.name(), Issues: issues, level: level})
	}
	srcOrder := func(v skillView) int {
		switch v.Source {
		case "user":
			if v.Kind == "command" {
				return 1
			}
			return 0
		case "synced":
			return 2
		case "plugin":
			return 3
		}
		return 4
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if healthRank(a.level) != healthRank(b.level) {
			return healthRank(a.level) > healthRank(b.level)
		}
		if srcOrder(a) != srcOrder(b) {
			return srcOrder(a) < srcOrder(b)
		}
		return strings.ToLower(a.Label) < strings.ToLower(b.Label)
	})
	return out
}

// skillPurpose：用途摘要：描述的第一句（中文句號、驚嘆號、問號，或英文的 . ! ? 後面接空白或結尾；太前面的句點與 e.g. 這類縮寫不算），
// 空白合併，超過 160 字截斷加「…」
func skillPurpose(description string) string {
	t := strings.TrimSpace(spacesPattern.ReplaceAllString(description, " "))
	rs := []rune(t)
	end := -1
	for i, c := range rs {
		if c == '。' || c == '！' || c == '？' {
			end = i + 1
			break
		}
		if (c == '.' || c == '!' || c == '?') && i >= 12 && (i+1 == len(rs) || rs[i+1] == ' ') && !endsWithAbbreviation(rs, i) {
			end = i + 1
			break
		}
	}
	if end > 0 {
		rs = rs[:end]
	}
	if len(rs) > skillPurposeMaxChars {
		return strings.TrimRight(string(rs[:skillPurposeMaxChars-1]), " ") + "…"
	}
	return string(rs)
}

func endsWithAbbreviation(rs []rune, dot int) bool {
	start := 0
	for i := dot; i >= 0; i-- {
		if rs[i] == ' ' {
			start = i + 1
			break
		}
	}
	word := strings.ToLower(strings.TrimLeft(string(rs[start:dot+1]), `("'`))
	switch word {
	case "e.g.", "i.e.", "etc.", "vs.", "cf.", "approx.":
		return true
	}
	return false
}
