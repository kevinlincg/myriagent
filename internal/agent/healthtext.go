package agent

import (
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// 健康度分析用的純文字規則（照 C# 的 Health/HealthTextRules.cs 與 HealthTomlRules.cs）：粗估 token、去掉 CLI 不送的 HTML 註解、
// 找出 CLAUDE.md 的 @路徑 匯入、讀 YAML 前置資料（skill 的描述、規則的 paths）、讀 TOML 設定的表頭與布林值。
// 只做近似：不跑各家的 tokenizer，CJK 字元一字算一個 token、其他字元四個算一個。

// textMeasure：指示檔的量測（CLI 實際送進脈絡的內容：HTML 註解已去掉）。Bytes 是原檔大小
type textMeasure struct {
	Bytes  int64
	Chars  int
	Lines  int
	Tokens int
}

// healthWideStart：CJK 與全形字元的起點（CJK 部首補充 U+2E80；以下的拉丁、符號、標點四字元一個 token）
const healthWideStart = 0x2E80

// estimateTokens：CJK（U+2E80 以上）每字 1 個，其他每 4 字元 1 個（無條件進位）
func estimateTokens(text string) int {
	wide, narrow := 0, 0
	for _, r := range text {
		if r >= healthWideStart {
			wide++
		} else {
			narrow++
		}
	}
	return wide + (narrow+3)/4
}

// countChars：字元數（以 Unicode 字元計）
func countChars(text string) int { return len([]rune(text)) }

// countLines：空字串 0 行；最後一行沒有換行也算一行
func countLines(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if text[len(text)-1] == '\n' {
		return n
	}
	return n + 1
}

// measureText：量測一份指示：原檔位元組數另外給，其他以去掉 HTML 註解之後的內容計
func measureText(text string, bytes int64) textMeasure {
	sent := stripHTMLComments(text)
	return textMeasure{Bytes: bytes, Chars: countChars(sent), Lines: countLines(sent), Tokens: estimateTokens(sent)}
}

func splitTextLines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

// fenceMarker：程式碼區塊的圍欄（``` 或 ~~~，可縮排三格以內）；不是圍欄回空字串
func fenceMarker(line string) string {
	t := strings.TrimLeftFunc(line, unicode.IsSpace)
	if len(line)-len(t) > 3 {
		return ""
	}
	switch {
	case strings.HasPrefix(t, "```"):
		return "```"
	case strings.HasPrefix(t, "~~~"):
		return "~~~"
	}
	return ""
}

func trimLeftSpace(s string) string { return strings.TrimLeftFunc(s, unicode.IsSpace) }

// stripHTMLComments：去掉程式碼區塊以外的 HTML 註解（Claude Code 送進脈絡之前會去掉）。沒有結尾的註解視為到檔尾
func stripHTMLComments(text string) string {
	if !strings.Contains(text, "<!--") {
		return text
	}
	var lines []string
	fence := ""
	inComment := false
	for _, line := range splitTextLines(text) {
		if !inComment {
			if m := fenceMarker(line); m != "" {
				if fence == "" {
					fence = m
				} else if strings.HasPrefix(trimLeftSpace(line), fence) {
					fence = ""
				}
				lines = append(lines, line)
				continue
			}
		}
		if fence != "" {
			lines = append(lines, line)
			continue
		}
		var kept strings.Builder
		i := 0
		for i < len(line) {
			if inComment {
				end := strings.Index(line[i:], "-->")
				if end < 0 {
					i = len(line)
					break
				}
				inComment = false
				i += end + 3
			} else {
				start := strings.Index(line[i:], "<!--")
				if start < 0 {
					kept.WriteString(line[i:])
					break
				}
				kept.WriteString(line[i : i+start])
				inComment = true
				i += start + 4
			}
		}
		// 整行都是註解（或註解跨行的中間）就整行拿掉，不留空行
		if kept.Len() > 0 || (!inComment && line == "") {
			lines = append(lines, kept.String())
		}
	}
	return strings.Join(lines, "\n")
}

// removeInlineCode：行內程式碼（反引號包起來的）換成空白
func removeInlineCode(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	var b strings.Builder
	in := false
	for _, r := range line {
		if r == '`' {
			in = !in
			b.WriteByte(' ')
			continue
		}
		if in {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// importStop：@路徑 遇到這些字元就停
const importStop = "`'\"<>()（）、，。；：！？「」@"

// findImports：CLAUDE.md 的 @路徑 匯入（程式碼區塊與行內程式碼裡的不算；@ 要在行首、空白或括號之後）：回傳原樣的路徑字串，去掉結尾的標點。
// 只挑「看起來是路徑」的（含 / \ . 或 ~ 開頭）
func findImports(text string) []string {
	var result []string
	if !strings.Contains(text, "@") {
		return result
	}
	fence := ""
	for _, line := range splitTextLines(stripHTMLComments(text)) {
		if m := fenceMarker(line); m != "" {
			if fence == "" {
				fence = m
			} else if strings.HasPrefix(trimLeftSpace(line), fence) {
				fence = ""
			}
			continue
		}
		if fence != "" || !strings.Contains(line, "@") {
			continue
		}
		rs := []rune(removeInlineCode(line))
		for i := 0; i < len(rs); i++ {
			if rs[i] != '@' {
				continue
			}
			if i > 0 && !unicode.IsSpace(rs[i-1]) && rs[i-1] != '(' && rs[i-1] != '（' {
				continue
			}
			j := i + 1
			for j < len(rs) && !unicode.IsSpace(rs[j]) && !strings.ContainsRune(importStop, rs[j]) {
				j++
			}
			path := strings.TrimRight(string(rs[i+1:j]), ".,;:!?。，；：、）)]」")
			i = j - 1
			if path == "" || strings.Contains(path, "://") {
				continue
			}
			if path[0] == '~' || strings.ContainsAny(path, `/\.`) {
				if !containsString(result, path) {
					result = append(result, path)
				}
			}
		}
	}
	return result
}

// resolveImport：匯入路徑的實際位置：~ 開頭以使用者資料夾、相對路徑以匯入它的檔案所在資料夾為基準；格式不對回空字串
func resolveImport(importPath, containingFile, home string) string {
	switch {
	case importPath == "~":
		return ""
	case strings.HasPrefix(importPath, "~/"):
		return filepath.Clean(filepath.Join(home, importPath[2:]))
	case filepath.IsAbs(importPath):
		return filepath.Clean(importPath)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(containingFile), importPath))
}

// frontMatter：YAML 前置資料（檔頭 --- 到下一個 ---）的最上層鍵與值（鍵小寫）：縮排的續行與清單併進前一個鍵（以 \n 連接）。
// 沒有前置資料（含沒有結尾的 ---）回空的
func parseFrontMatter(text string) map[string]string {
	result := map[string]string{}
	lines := splitTextLines(strings.TrimPrefix(text, "\uFEFF"))
	if text == "" || len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return result
	}
	key := ""
	for _, line := range lines[1:] {
		if strings.TrimRight(line, " \t") == "---" {
			return result
		}
		if line != "" && !unicode.IsSpace(rune(line[0])) && line[0] != '-' && line[0] != '#' {
			if colon := strings.IndexByte(line, ':'); colon > 0 {
				key = strings.ToLower(strings.TrimSpace(line[:colon]))
				result[key] = strings.TrimSpace(line[colon+1:])
				continue
			}
		}
		if key != "" && strings.TrimSpace(line) != "" {
			if result[key] == "" {
				result[key] = strings.TrimSpace(line)
			} else {
				result[key] += "\n" + strings.TrimSpace(line)
			}
		}
	}
	return map[string]string{}
}

// frontMatterProblem：skill 前置資料裡 YAML 會出錯或讀錯的寫法（name、description、when_to_use 的單行值）：
// 沒加引號的值裡有「: 」（整份前置資料讀不懂）、「 #」（後面被當成註解截掉）。沒有問題回空字串
func frontMatterProblem(text string) string {
	lines := splitTextLines(strings.TrimPrefix(text, "\uFEFF"))
	if text == "" || strings.TrimRight(lines[0], " \t") != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		if strings.TrimRight(line, " \t") == "---" {
			break
		}
		if line == "" || unicode.IsSpace(rune(line[0])) {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		if lk := strings.ToLower(key); lk != "name" && lk != "description" && lk != "when_to_use" {
			continue
		}
		value := strings.TrimSpace(line[colon+1:])
		if value == "" || strings.ContainsRune(`"'>|[{`, rune(value[0])) {
			continue
		}
		if strings.Contains(value, ": ") || strings.HasSuffix(value, ":") {
			return key + " 的值沒加引號、裡面有「: 」"
		}
		if strings.Contains(value, " #") {
			return key + " 的值沒加引號、裡面有「 #」（後面會被當成註解）"
		}
	}
	return ""
}

// bodyAfterFrontMatter：前置資料之後的本文（沒有前置資料就是整份）；第二個回傳值是有完整的前置資料
func bodyAfterFrontMatter(text string) (string, bool) {
	lines := splitTextLines(strings.TrimPrefix(text, "\uFEFF"))
	if text == "" || strings.TrimRight(lines[0], " \t") != "---" {
		return text, false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			return strings.Join(lines[i+1:], "\n"), true
		}
	}
	return text, false
}

var markdownLinkPattern = regexp.MustCompile(`\]\((<[^>]+>|[^)\s]+)(?:\s+"[^"]*")?\)`)

// relativeLinks：skill 本文以相對路徑連結的檔（程式碼區塊與行內程式碼裡的不算）：去掉 #錨點與 ?查詢、解開 %20 這類編碼。
// 網址、錨點、絕對路徑、~ 開頭、含 { } 範本的都不算
func relativeLinks(body string) []string {
	var result []string
	if !strings.Contains(body, "](") {
		return result
	}
	fence := ""
	for _, line := range splitTextLines(stripHTMLComments(body)) {
		if m := fenceMarker(line); m != "" {
			if fence == "" {
				fence = m
			} else if strings.HasPrefix(trimLeftSpace(line), fence) {
				fence = ""
			}
			continue
		}
		if fence != "" || !strings.Contains(line, "](") {
			continue
		}
		for _, m := range markdownLinkPattern.FindAllStringSubmatch(removeInlineCode(line), -1) {
			target := strings.Trim(m[1], "<>")
			if cut := strings.IndexAny(target, "#?"); cut >= 0 {
				target = target[:cut]
			}
			if target == "" || strings.Contains(target, ":") || strings.ContainsRune(`/\~`, rune(target[0])) || strings.ContainsAny(target, "{}") {
				continue
			}
			if u, err := url.PathUnescape(target); err == nil {
				target = u
			}
			if !containsString(result, target) {
				result = append(result, target)
			}
		}
	}
	return result
}

// frontMatterText：前置資料的文字值：去掉區塊標記（> |）與外層引號，續行以空白連接
func frontMatterText(fm map[string]string, key string) string {
	v := fm[key]
	if strings.TrimSpace(v) == "" {
		return ""
	}
	var parts []string
	for _, p := range strings.Split(v, "\n") {
		parts = append(parts, strings.TrimSpace(p))
	}
	switch parts[0] {
	case ">", "|", ">-", "|-", ">+", "|+":
		parts = parts[1:]
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	joined := strings.TrimSpace(strings.Join(kept, " "))
	if len(joined) >= 2 && ((joined[0] == '"' && joined[len(joined)-1] == '"') || (joined[0] == '\'' && joined[len(joined)-1] == '\'')) {
		joined = joined[1 : len(joined)-1]
	}
	return strings.TrimSpace(joined)
}

// hasPathScope：規則檔有沒有 paths（只在碰到相符的檔案時才載入）
func hasPathScope(fm map[string]string) bool {
	p, ok := fm["paths"]
	return ok && strings.TrimSpace(p) != "" && strings.TrimSpace(p) != "[]"
}

// isAntigravityScopedRule：Antigravity 規則檔是否為按需觸發（trigger 不是 always_on，或指定了 globs／paths）
func isAntigravityScopedRule(fm map[string]string) bool {
	if hasPathScope(fm) {
		return true
	}
	for _, k := range []string{"globs", "glob"} {
		if v, ok := fm[k]; ok && strings.TrimSpace(v) != "" && strings.TrimSpace(v) != "[]" {
			return true
		}
	}
	t := frontMatterText(fm, "trigger")
	return t != "" && !strings.EqualFold(t, "always_on")
}

// skillListingChars：skill 在清單裡的一筆的字元數：名稱＋描述與 when_to_use（描述部分最多 cap 字元）
func skillListingChars(name string, fm map[string]string, cap int) int {
	desc, when := frontMatterText(fm, "description"), frontMatterText(fm, "when_to_use")
	text := desc
	if when != "" {
		text = desc + " " + when
	}
	n := countChars(text)
	if n > cap {
		n = cap
	}
	return countChars(name) + n
}

// truncateChars：最多 n 個字元（不加省略號）
func truncateChars(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---- 顯示 ----

// formatCount：數字加千分位
func formatCount(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// oneDecimal：最多一位小數（同 C# 的 0.#）
func oneDecimal(f float64) string {
	return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64)
}

// formatHealthTokens：「約 850 token」「約 3,200 token」「約 1.3 萬 token」
func formatHealthTokens(tokens int) string {
	if tokens < 10_000 {
		return "約 " + formatCount(int64(tokens)) + " token"
	}
	return "約 " + oneDecimal(float64(tokens)/10_000) + " 萬 token"
}

// formatBytes：「820 B」「24.6 KB」「3.1 MB」
func formatBytes(b int64) string {
	switch {
	case b < 1024:
		return strconv.FormatInt(b, 10) + " B"
	case b < 1024*1024:
		return oneDecimal(float64(b)/1024) + " KB"
	}
	return oneDecimal(float64(b)/1024/1024) + " MB"
}

// ---- TOML（Codex、Grok 的 config.toml；只認行首的表頭與「鍵 = 值」，不是完整的 TOML） ----

// stripTomlComment：去掉不在字串裡的 # 註解
func stripTomlComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
		} else if c == '#' {
			return line[:i]
		}
	}
	return line
}

// tomlTableNames：[前綴.名稱]（含 [前綴."有點號的名稱"]）的名稱，依出現順序、不重複；[前綴.名稱.env] 這類子表算同一個
func tomlTableNames(toml, prefix string) []string {
	names := []string{}
	for _, raw := range splitTextLines(toml) {
		line := strings.TrimSpace(stripTomlComment(raw))
		if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") || !strings.HasSuffix(line, "]") {
			continue
		}
		path := strings.TrimSpace(line[1 : len(line)-1])
		if !strings.HasPrefix(path, prefix+".") {
			continue
		}
		rest := trimLeftSpace(path[len(prefix)+1:])
		var name string
		if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
			end := strings.IndexByte(rest[1:], rest[0])
			if end < 0 {
				continue
			}
			name = rest[1 : end+1]
		} else {
			if dot := strings.IndexByte(rest, '.'); dot >= 0 {
				rest = rest[:dot]
			}
			name = strings.TrimSpace(rest)
		}
		if name != "" && !containsString(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// tomlBool：[表] 底下的「鍵 = true／false」，或最上層的「表.鍵 = …」；沒有或不是布林回 nil
func tomlBool(toml, table, key string) *bool {
	current := ""
	inTable := false
	var found *bool
	for _, raw := range splitTextLines(toml) {
		line := strings.TrimSpace(stripTomlComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(strings.TrimRight(strings.TrimLeft(line, "["), "]"))
			inTable = true
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		name, value := strings.TrimSpace(line[:eq]), strings.TrimSpace(line[eq+1:])
		if !((inTable && current == table && name == key) || (!inTable && name == table+"."+key)) {
			continue
		}
		switch value {
		case "true":
			found = boolPtr(true)
		case "false":
			found = boolPtr(false)
		default:
			found = nil
		}
	}
	return found
}
