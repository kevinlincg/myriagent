package agent

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 電腦層級的全域指示（各 CLI 使用者層的 CLAUDE.md、AGENTS.md、GEMINI.md）的比較（照 C# 的 Health/InstructionCompareRules.cs；純函式）：
// 把每份切成一條條指示，以正規化文字與字元雙連字的相似度歸成群，算出每份缺哪些、哪些只有一份有，判斷是否需要合併與同步；
// 另產生「用代理合併」「用代理精簡」「用代理整理」（自動記憶索引）的對話開頭訊息。比對只看文字。

const (
	fuzzyThreshold       = 0.8 // 相似度到這個值以上算同一條（字元雙連字的 Dice 係數）
	minUnitChars         = 4   // 正規化後比這短的不算一條
	maxListedUnits       = 12
	mostlySameRatio      = 0.9
	memoryTargetLines    = 150
	memoryTargetBytes    = 20_000
	verdictNone          = "none"
	verdictSingle        = "single"
	verdictSame          = "same"
	verdictNeedsSync     = "needsSync"
	verdictDiverged      = "diverged"
	instructionUnitClipN = 120
)

// instructionUnit：指示裡的一條（一個清單項目、表格的一列、一段文字或一個程式碼區塊）
type instructionUnit struct {
	Section, Text, Norm string
}

// instructionDoc：電腦層級的一份全域指示
type instructionDoc struct {
	Key, Provider, Label, Path, LinkTarget string
	Exists                                 bool
	Text                                   string
	Measure                                textMeasure
	AccountIDs                             []string
	AlsoLoadsKey                           string // Grok 的 Claude 相容會一併讀 ~/.claude/CLAUDE.md：那一份的 Key
	MaxBytes                               int64  // 0＝沒有上限
}

type docCoverage struct {
	Doc                   *instructionDoc
	Units, Covered, Union int
	Missing, Unique       []instructionUnit
	Duplicates            int
}

func (c docCoverage) coverage() float64 {
	if c.Union == 0 {
		return 1
	}
	return float64(c.Covered) / float64(c.Union)
}

type instructionsReport struct {
	Verdict string
	Level   healthLevel
	Summary string
	Docs    []docCoverage
}

func (r instructionsReport) mergeable() bool {
	return r.Verdict == verdictNeedsSync || r.Verdict == verdictDiverged
}

var (
	listMarkerPattern   = regexp.MustCompile(`^\s{0,3}(?:[-*+]|\d{1,3}[.)])\s+`)
	headingPattern      = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	tableSeparatorPat   = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	thematicBreakPat    = regexp.MustCompile(`^\s{0,3}(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	normalizeTrimSuffix = ".。;；:：,， "
)

// instructionUnits：把一份指示切成一條條（HTML 註解先去掉）
func instructionUnits(text string) []instructionUnit {
	var units []instructionUnit
	if strings.TrimSpace(text) == "" {
		return units
	}
	section := ""
	var cur strings.Builder
	fence := ""
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		raw := strings.TrimSpace(cur.String())
		cur.Reset()
		norm := normalizeInstruction(raw)
		if countChars(norm) >= minUnitChars {
			units = append(units, instructionUnit{Section: section, Text: spacesPattern.ReplaceAllString(raw, " "), Norm: norm})
		}
	}
	for _, line := range splitTextLines(stripHTMLComments(text)) {
		if fence != "" {
			cur.WriteString("\n" + line)
			if strings.HasPrefix(trimLeftSpace(line), fence) {
				fence = ""
				flush()
			}
			continue
		}
		if m := fenceMarker(line); m != "" {
			flush()
			fence = m
			cur.WriteString(line)
			continue
		}
		if strings.TrimSpace(line) == "" || thematicBreakPat.MatchString(line) {
			flush()
			continue
		}
		if m := headingPattern.FindStringSubmatch(line); m != nil {
			flush()
			section = strings.TrimSpace(m[1])
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") {
			flush()
			if !tableSeparatorPat.MatchString(line) {
				cur.WriteString(trimmed)
				flush()
			}
			continue
		}
		if loc := listMarkerPattern.FindStringIndex(line); loc != nil {
			flush()
			cur.WriteString(strings.TrimSpace(line[loc[1]:]))
			continue
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(strings.TrimSpace(strings.TrimLeft(trimmed, ">")))
	}
	flush()
	return units
}

// normalizeInstruction：比對用的正規化：小寫、去掉 Markdown 符號與多餘空白、去掉結尾標點
func normalizeInstruction(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if strings.ContainsRune("*_`#>|\t\r\n", r) {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(strings.TrimSpace(spacesPattern.ReplaceAllString(b.String(), " ")), normalizeTrimSuffix)
}

func bigrams(s string) map[[2]rune]bool {
	set := map[[2]rune]bool{}
	rs := []rune(s)
	for i := 0; i+1 < len(rs); i++ {
		if rs[i] != ' ' || rs[i+1] != ' ' {
			set[[2]rune{rs[i], rs[i+1]}] = true
		}
	}
	return set
}

// instructionSimilarity：字元雙連字的 Dice 係數（0～1）
func instructionSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	x, y := bigrams(a), bigrams(b)
	if len(x) == 0 || len(y) == 0 {
		return 0
	}
	common := 0
	for g := range x {
		if y[g] {
			common++
		}
	}
	return 2 * float64(common) / float64(len(x)+len(y))
}

type unitCluster struct {
	first instructionUnit
	grams map[[2]rune]bool
	docs  map[string]bool
}

// compareInstructions：比較電腦層級的指示檔
func compareInstructions(docs []*instructionDoc) instructionsReport {
	if len(docs) == 0 {
		return instructionsReport{Verdict: verdictNone, Level: healthOK, Summary: "這台沒有可以比較的指示檔。"}
	}
	unitsBy := map[string][]instructionUnit{}
	for _, d := range docs {
		unitsBy[d.Key] = instructionUnits(d.Text)
	}
	// 內容多的先歸群（群的代表原文取自它）
	order := append([]*instructionDoc{}, docs...)
	sort.SliceStable(order, func(i, j int) bool { return len(unitsBy[order[i].Key]) > len(unitsBy[order[j].Key]) })
	var clusters []*unitCluster
	exact := map[string]*unitCluster{}
	for _, d := range order {
		for _, u := range unitsBy[d.Key] {
			c := exact[u.Norm]
			if c == nil {
				g := bigrams(u.Norm)
				c = findSimilarCluster(clusters, u.Norm, g)
				if c == nil {
					c = &unitCluster{first: u, grams: g, docs: map[string]bool{}}
					clusters = append(clusters, c)
				}
				exact[u.Norm] = c
			}
			c.docs[d.Key] = true
		}
	}
	byKey := map[string]*instructionDoc{}
	for _, d := range docs {
		byKey[d.Key] = d
	}
	var covs []docCoverage
	for _, d := range docs {
		also := ""
		if d.AlsoLoadsKey != "" && byKey[d.AlsoLoadsKey] != nil {
			also = d.AlsoLoadsKey
		}
		has := func(c *unitCluster) bool { return c.docs[d.Key] || (also != "" && c.docs[also]) }
		cov := docCoverage{Doc: d, Units: len(unitsBy[d.Key]), Union: len(clusters)}
		for _, c := range clusters {
			if !has(c) {
				cov.Missing = append(cov.Missing, c.first)
			}
			if len(c.docs) == 1 && c.docs[d.Key] {
				cov.Unique = append(cov.Unique, c.first)
			}
			if also != "" && c.docs[d.Key] && c.docs[also] {
				cov.Duplicates++
			}
		}
		cov.Covered = len(clusters) - len(cov.Missing)
		covs = append(covs, cov)
	}
	withContent := 0
	for _, c := range covs {
		if c.Units > 0 {
			withContent++
		}
	}
	switch {
	case withContent == 0:
		return instructionsReport{Verdict: verdictNone, Level: healthOK, Summary: "這台的 CLI 都還沒有電腦層級的指示。", Docs: covs}
	case len(docs) == 1:
		return instructionsReport{Verdict: verdictSingle, Level: healthOK, Summary: "這台只有一種 CLI 的指示檔，不需要合併。", Docs: covs}
	}
	allSame := true
	for _, c := range covs {
		allSame = allSame && len(c.Missing) == 0
	}
	if allSame {
		return instructionsReport{Verdict: verdictSame, Level: healthOK, Summary: "各 CLI 的全域指示一致。", Docs: covs}
	}
	// 什麼都拿不到的（Grok 自己沒有、但經 Claude 相容讀得到的不算）
	var empty []docCoverage
	maxMissing := 0
	for _, c := range covs {
		if c.Covered == 0 && len(c.Missing) > 0 {
			empty = append(empty, c)
			if len(c.Missing) > maxMissing {
				maxMissing = len(c.Missing)
			}
		}
	}
	if len(empty) > 0 {
		var names []string
		for _, c := range empty {
			names = append(names, c.Doc.Label+" 的 "+filepath.Base(c.Doc.Path))
		}
		word := "是空的"
		if len(empty) > 1 {
			word = "都是空的"
		}
		return instructionsReport{Verdict: verdictNeedsSync, Level: healthWarn, Docs: covs,
			Summary: fmt.Sprintf("%s %s，拿不到其他 CLI 的 %d 條指示；建議合併並同步。", strings.Join(names, "、"), word, maxMissing)}
	}
	var gaps []string
	mostly := true
	for _, c := range covs {
		if len(c.Missing) > 0 {
			gaps = append(gaps, fmt.Sprintf("%s 缺 %d 條", c.Doc.Label, len(c.Missing)))
		}
		mostly = mostly && c.coverage() >= mostlySameRatio
	}
	level, lead := healthWarn, "各份有分歧："
	if mostly {
		level, lead = healthInfo, "大致一致，但"
	}
	return instructionsReport{Verdict: verdictDiverged, Level: level, Docs: covs,
		Summary: lead + strings.Join(gaps, "、") + "（有些可能是某個 CLI 專屬的）；建議合併。"}
}

func findSimilarCluster(clusters []*unitCluster, norm string, grams map[[2]rune]bool) *unitCluster {
	if len(grams) == 0 {
		return nil
	}
	var best *unitCluster
	bestScore := fuzzyThreshold
	n := float64(len([]rune(norm)))
	for _, c := range clusters {
		l := float64(len([]rune(c.first.Norm)))
		if l < n*0.6 || l*0.6 > n || len(c.grams) == 0 {
			continue
		}
		common := 0
		for g := range grams {
			if c.grams[g] {
				common++
			}
		}
		if score := 2 * float64(common) / float64(len(grams)+len(c.grams)); score >= bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

func coverageStatus(c docCoverage) string {
	switch {
	case c.Covered == 0 && c.Units == 0 && c.Doc.Exists:
		return "empty"
	case c.Covered == 0 && c.Units == 0:
		return "missing"
	case len(c.Missing) == 0:
		return "complete"
	}
	return "partial"
}

func clipChars(text string, max int) string {
	t := strings.ReplaceAll(text, "\n", " ")
	if r := []rune(t); len(r) > max {
		return string(r[:max]) + "…"
	}
	return t
}

func renderUnits(us []instructionUnit) []any {
	out := []any{}
	for i, u := range us {
		if i >= maxListedUnits {
			break
		}
		out = append(out, map[string]any{"section": u.Section, "text": clipChars(u.Text, instructionUnitClipN)})
	}
	return out
}

// renderInstructions：InstructionsView
func renderInstructions(r instructionsReport) map[string]any {
	byKey := map[string]docCoverage{}
	for _, c := range r.Docs {
		byKey[c.Doc.Key] = c
	}
	docs := []any{}
	for _, c := range r.Docs {
		d := c.Doc
		note := "不存在"
		switch {
		case d.Exists && c.Units == 0:
			note = "空的"
		case d.Exists:
			note = formatCount(int64(d.Measure.Lines)) + " 行・" + formatHealthTokens(d.Measure.Tokens)
		}
		var also any
		if o, ok := byKey[d.AlsoLoadsKey]; ok && d.AlsoLoadsKey != "" {
			also = o.Doc.Label
		}
		accounts := d.AccountIDs
		if accounts == nil {
			accounts = []string{}
		}
		docs = append(docs, map[string]any{
			"key": d.Key, "provider": d.Provider, "label": d.Label, "path": d.Path, "target": nilIfEmpty(d.LinkTarget),
			"exists": d.Exists, "status": coverageStatus(c), "note": note, "overLimit": d.MaxBytes > 0 && d.Measure.Bytes > d.MaxBytes,
			"units": c.Units, "covered": c.Covered, "union": c.Union, "coverage": math.Round(c.coverage()*1000) / 1000,
			"missingCount": len(c.Missing), "missing": renderUnits(c.Missing), "uniqueCount": len(c.Unique), "unique": renderUnits(c.Unique),
			"alsoLoads": also, "duplicates": c.Duplicates, "accounts": accounts,
		})
	}
	return map[string]any{"verdict": r.Verdict, "level": r.Level.name(), "summary": r.Summary, "mergeable": r.mergeable(), "docs": docs}
}

// ---- 用代理合併、精簡、整理 ----

func docLine(d *instructionDoc) string {
	var b strings.Builder
	b.WriteString(d.Label + "：" + d.Path)
	if d.LinkTarget != "" {
		b.WriteString("（符號連結，實際檔案是 " + d.LinkTarget + "）")
	}
	b.WriteString(" — ")
	switch {
	case !d.Exists:
		b.WriteString("不存在")
	case strings.TrimSpace(d.Text) == "":
		b.WriteString("空的")
	default:
		b.WriteString(formatCount(int64(d.Measure.Lines)) + " 行、" + formatBytes(d.Measure.Bytes) + "、" + formatHealthTokens(d.Measure.Tokens))
	}
	return b.String()
}

func backupStamp(now time.Time) string { return now.Format("20060102-1504") }

// buildMergePrompt：「用代理合併」的對話開頭訊息：列出各份的位置與分析結果，約定合併的做法（先給計畫、確認後才寫、寫前備份）
func buildMergePrompt(r instructionsReport, now time.Time) string {
	var b strings.Builder
	b.WriteString("請幫我合併並同步這台電腦各個 AI CLI 的全域指示檔（使用者層、每次對話都會載入的指示）。\n\n檔案：\n")
	for i, c := range r.Docs {
		b.WriteString(strconv.Itoa(i+1) + ". " + docLine(c.Doc) + "\n")
	}
	b.WriteString("\nClode Myriagent 的分析：" + r.Summary + "\n")
	for _, c := range r.Docs {
		if len(c.Missing) == 0 {
			continue
		}
		var secs []string
		for _, u := range c.Missing {
			if u.Section != "" && !contains(secs, u.Section) && len(secs) < 8 {
				secs = append(secs, u.Section)
			}
		}
		b.WriteString(fmt.Sprintf("- %s 缺 %d 條", c.Doc.Label, len(c.Missing)))
		if len(secs) > 0 {
			b.WriteString("（在這些段落：" + strings.Join(secs, "、") + "）")
		}
		b.WriteString("\n")
	}
	for _, c := range r.Docs {
		if c.Doc.AlsoLoadsKey == "" {
			continue
		}
		for _, o := range r.Docs {
			if o.Doc.Key == c.Doc.AlsoLoadsKey {
				b.WriteString("- " + c.Doc.Label + " 會一併讀取 " + o.Doc.Path + "（Claude 相容）：共通的指示不必再寫進它自己的檔，寫了會重複載入")
				if c.Duplicates > 0 {
					b.WriteString(fmt.Sprintf("（目前重複 %d 條）", c.Duplicates))
				}
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("\n請照這個流程做：\n")
	b.WriteString("1. 先完整讀過每一份檔案。符號連結要改它指向的實際檔案，不要把連結換成一般檔案。\n")
	b.WriteString("2. 把內容分成「共通的指示」（語言、溝通方式、開發與部署習慣等，每個 CLI 都適用）與「某個 CLI 專屬的」（例如 Claude Code 的工具、settings.json、hooks、skills；Codex 的 config.toml、沙箱；Grok、Antigravity 各自的設定）。\n")
	b.WriteString("3. 共通的指示在每一份都要有、文字一致；專屬的只留在對應的檔。不要改寫原意，也不要刪掉仍然有效的指示——只有重複的才併成一條。\n")
	b.WriteString("4. 合併後每份都盡量精簡：Claude 建議每份 200 行以內，Antigravity 的 GEMINI.md 單檔上限 24,000 位元組；只在特定情境需要的細節可以搬到另外的文件，指示檔只留一行索引。\n")
	b.WriteString("5. 有互相矛盾的指示時先列出來問我，不要自己選。\n")
	b.WriteString("6. 先把合併計畫（每份檔案會新增、刪除、改寫哪些段落，合併後的行數與大小）給我看，等我確認才寫入；寫入前把每份原檔備份成同一個資料夾的「<檔名>.bak-" + backupStamp(now) + "」。\n")
	b.WriteString("7. 寫完之後列出每份的行數與大小，並確認共通的段落已經一致。\n")
	b.WriteString("\n之後改共通的指示時，同一則修改要套用到每一份檔案，讓它們保持同步。")
	return b.String()
}

// buildTrimPrompt：「用代理精簡」的對話開頭訊息：一個帳戶每次都載入的指示檔與分析結果
func buildTrimPrompt(s *accountScan, h accountHealth, now time.Time) string {
	item := h.Items[0]
	p := s.Account.Provider
	var b strings.Builder
	b.WriteString("請幫我精簡「" + s.Account.Label + "」（" + providerNames[p] + "）每次對話都會載入的全域指示。\n\n檔案：\n")
	for i, f := range s.Instructions {
		b.WriteString(strconv.Itoa(i+1) + ". " + f.Path)
		if f.LinkTarget != "" {
			b.WriteString("（符號連結，實際檔案是 " + f.LinkTarget + "）")
		}
		b.WriteString(" — " + formatCount(int64(f.Measure.Lines)) + " 行、" + formatBytes(f.Measure.Bytes) + "、" + formatHealthTokens(f.Measure.Tokens) + "\n")
	}
	b.WriteString("\nClode Myriagent 的分析：" + item.Value + "。\n" + item.Detail + "\n")
	b.WriteString("\n請照這個流程做：\n")
	b.WriteString("1. 讀完上面的檔案（符號連結要改它指向的實際檔案），標出：可以從程式碼或其他文件推得的、過時的、重複的、只在特定專案或特定情境才需要的。\n")
	if p == "Claude" {
		b.WriteString("2. 只在特定情境需要的細節搬到另外的文件（例如 ~/.claude/docs/<主題>.md），指示檔只留一行「情境 → 檔案」的索引；或改成 ~/.claude/rules/ 裡有 paths 的規則，碰到相符的檔案才載入。@ 匯入不會減少載入量。\n")
	} else {
		b.WriteString("2. 只在特定情境需要的細節搬到另外的文件，指示檔只留一行「情境 → 檔案」的索引。\n")
	}
	b.WriteString("3. 仍然有效的規則一條都不要刪；拿不準的先問我。\n")
	b.WriteString("4. 先給我精簡計畫（刪、搬、改寫哪些段落，精簡後的行數與大小），等我確認才寫入；寫入前把原檔備份成同一個資料夾的「<檔名>.bak-" + backupStamp(now) + "」。\n")
	b.WriteString("\n目標：每份 200 行以內、每次載入的合計約 " + formatCount(instructionWarnTokens) + " token 以下。")
	return b.String()
}

// buildMemoryPrompt：「用代理整理」的對話開頭訊息：一個 Claude 帳戶超過或接近載入上限的自動記憶索引；沒有要整理的回空字串
func buildMemoryPrompt(s *accountScan, now time.Time) string {
	var targets []memoryIndexStat
	for _, m := range s.MemoryIndexes {
		if memoryNeedsTrim(m) {
			targets = append(targets, m)
		}
	}
	if len(targets) == 0 {
		return ""
	}
	sort.SliceStable(targets, func(i, j int) bool {
		oi, oj := memoryOverLimit(targets[i]), memoryOverLimit(targets[j])
		if oi != oj {
			return oi
		}
		return targets[i].Bytes > targets[j].Bytes
	})
	var b strings.Builder
	which := "這些專案"
	if len(targets) == 1 {
		which = "這個專案"
	}
	b.WriteString("請幫我整理「" + s.Account.Label + "」（" + providerNames[s.Account.Provider] + "）的自動記憶索引：下面" + which + "的 MEMORY.md 超過或接近載入上限。\n\n")
	b.WriteString(fmt.Sprintf("Claude Code 每次對話開始只載入每個專案 MEMORY.md 的前 %d 行或 %dKB（先到者為準），超過的部分不會進入對話——後面的記憶等於沒記。", memoryIndexMaxLines, memoryIndexMaxBytes/1000))
	if s.Account.SharedSource != "" {
		b.WriteString("這個帳戶共用系統預設帳戶的記憶，下面是那邊的檔案。")
	}
	b.WriteString("\n\n檔案：\n")
	for i, m := range targets {
		state := "（接近上限）"
		if memoryOverLimit(m) {
			state = "（超過上限）"
		}
		b.WriteString(fmt.Sprintf("%d. %s：%s — %s 行、%s%s\n", i+1, m.Project, m.Path, formatCount(int64(m.Lines)), formatBytes(m.Bytes), state))
	}
	b.WriteString("\n請照這個流程做（每個專案分開整理）：\n")
	b.WriteString("1. 讀完 MEMORY.md 與同一個 memory 資料夾裡的主題檔。索引的每一條只該是一行指標：「- [標題](主題檔.md) — 一句話說明什麼情境用得到」，內容寫在主題檔裡。\n")
	b.WriteString("2. 索引裡直接寫了細節的條目：細節搬進主題檔（已有相關的主題檔就併進去，沒有就新建，照既有主題檔的格式，例如開頭的 frontmatter），索引只留一行。\n")
	b.WriteString("3. 重複或講同一件事的條目合併成一條。已經過時、做完了、或和現況不符的才刪（主題檔一併處理）：專案資料夾的名稱是工作資料夾的路徑把英數字以外的字元都換成「-」（例如 /Users/me/my.app 是 -Users-me-my-app），拿不準時到那個資料夾查證。仍然有效的記憶一條都不要刪，拿不準的先問我。\n")
	b.WriteString("4. 索引連到不存在的檔、主題檔沒有被索引連到的，列出來讓我決定。\n")
	b.WriteString("5. 先給我整理計畫（每個專案：搬、併、刪哪些條目，整理後索引的行數與大小），等我確認才寫入；寫入前把整個 memory 資料夾複製成同一層的「memory.bak-" + backupStamp(now) + "」（不要放在 memory 資料夾裡）。\n")
	b.WriteString("6. 寫完之後列出每個索引的行數與大小。\n")
	b.WriteString(fmt.Sprintf("\n目標：每個 MEMORY.md %d 行、%dKB 以內（留空間給之後的記憶）。", memoryTargetLines, memoryTargetBytes/1000))
	return b.String()
}
