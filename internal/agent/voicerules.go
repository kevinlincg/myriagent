package agent

import (
	"math"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// 語音通知念的句子（Protocol 的 VoiceNotifyRules）：對話完成、失敗、代理提問、要求核准、計畫等確認時，
// 用一句話說明是哪個對話、發生什麼事，再接一段簡要說明（完成了什麼、問什麼、要核准什麼）。
// 句子沒有字數上限（使用者 2026-10-24：標題整個念、說明照取的那一句或那一行整個念）。
// 念的是純文字：Markdown 符號、網址、程式碼、括號裡的補充念出來只是雜音，先拿掉。

const (
	voiceMaxPending     = 3                // 佇列裡最多等幾句：同時完成很多個對話時，後面的不念
	voiceRepeatWindow   = 10 * time.Second // 同一句話在這段時間內不重念（同一件事從兩條路徑通知時）
	voiceEnabledSample  = "語音通知已開啟"
	voiceTuningSample   = "「整理報表」已完成：月報已匯出成 Excel"
	voiceMinSpeed       = 0.5
	voiceMaxSpeed       = 2.5
	voiceDefaultSpeed   = 1.5
	voiceMinPitch       = -6.0
	voiceMaxPitch       = 6.0
	voiceDefaultPitch   = 2.0
	firstLineMaxRunes   = 200
	voiceSentenceMinLen = 6
)

// voiceTuning：語速（倍速）與音調（半音）
type voiceTuning struct {
	Speed, Pitch float64
}

func roundAway(x float64) float64 { return math.Round(x) } // Go 的 Round 就是遠離零的四捨五入

// normalizeVoiceSpeed：限制在範圍內、取到 0.1；不是有限的數字回預設
func normalizeVoiceSpeed(s float64) float64 {
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return voiceDefaultSpeed
	}
	return roundAway(math.Max(voiceMinSpeed, math.Min(voiceMaxSpeed, s))*10) / 10
}

// normalizeVoicePitch：限制在範圍內、取到整數半音；不是有限的數字回預設
func normalizeVoicePitch(p float64) float64 {
	if math.IsNaN(p) || math.IsInf(p, 0) {
		return voiceDefaultPitch
	}
	return roundAway(math.Max(voiceMinPitch, math.Min(voiceMaxPitch, p)))
}

// textFirstLine：第一個有內容的行（去掉頭尾空白；超過 200 字截斷加「…」）
func textFirstLine(text string) string {
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			r := []rune(t)
			if len(r) <= firstLineMaxRunes {
				return t
			}
			return string(r[:firstLineMaxRunes]) + "…"
		}
	}
	return ""
}

func voiceCompleted(title, reply string) string {
	return voiceCompose(voiceName(title)+"已完成", voiceSummary(reply))
}

// voiceFailed：錯誤訊息是中文時接它的第一行（英文的技術錯誤念出來聽不懂，只說失敗）
func voiceFailed(title, errText string) string {
	line := textFirstLine(errText)
	if !hasCJK(line) {
		line = ""
	}
	return voiceCompose(voiceName(title)+"執行失敗", line)
}

func voiceQuestion(title, question string) string {
	return voiceCompose(voiceName(title)+"有問題想問你", textFirstLine(question))
}

// voiceApproval：工具名稱是中文的顯示名稱時念出來，英文的（MCP 工具的原始名稱）改說「執行操作」；
// 說明的第一行是中文時念它，否則念檔名
func voiceApproval(title, toolName, detail, filePath string) string {
	tool := voiceClean(toolName, math.MaxInt)
	if !hasCJK(tool) {
		tool = "執行操作"
	}
	line := textFirstLine(detail)
	brief := line
	if !hasCJK(line) {
		brief = voiceFileName(filePath)
	}
	return voiceCompose(voiceName(title)+"要"+tool+"，等你核准", brief)
}

func voicePlan(title, plan string) string {
	return voiceCompose(voiceName(title)+"的計畫等你確認", textFirstLine(plan))
}

// voiceName：「對話標題」；沒有標題時說「對話」
func voiceName(title string) string {
	n := voiceClean(title, math.MaxInt)
	if n == "" {
		return "對話"
	}
	return "「" + n + "」"
}

// voiceCompose：「誰、發生什麼事」接「：說明」；沒有說明就只念前半
func voiceCompose(head, brief string) string {
	head = voiceFit(head, math.MaxInt)
	text := voiceFit(brief, math.MaxInt)
	if text == "" {
		return head
	}
	return head + "：" + text
}

var (
	reCodeFence     = regexp.MustCompile("```[\\s\\S]*?(```|$)")
	reImage         = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	reLink          = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	reParenthetical = regexp.MustCompile(`[（(][^（）()\n]*[）)]`)
	reListMarker    = regexp.MustCompile(`^(>+|[-*+]|\d+[.)、])\s*`)
	// 開頭的客套（「好的，」「完成了。」）：後面要接標點才算，「完成登入頁」的「完成」不拿掉
	reFiller        = regexp.MustCompile(`^((好的|好|OK|Ok|ok|沒問題|完成了|完成|已完成|全部完成|都完成了|做完了|搞定了|搞定)\s*[，。！!,.、]\s*)+`)
	reURL           = regexp.MustCompile(`(?i)(?:https?|file)://\S+`)
	reInlineSymbols = regexp.MustCompile("[*`~]+")
	reSeparators    = regexp.MustCompile(`[_#>|]+`)
)

// voiceSummary：回覆的重點：拿掉程式碼區塊、圖片、連結網址與括號裡的補充，從第一個不是標題、表格、分隔線的段落取第一句；
// 開頭的客套拿掉，太短就接下一句；以「如下」「以下」加冒號帶出清單的那一句跳過、改看下一段。都沒有回空字串
func voiceSummary(reply string) string {
	if strings.TrimSpace(reply) == "" {
		return ""
	}
	text := reCodeFence.ReplaceAllString(strings.ReplaceAll(reply, "\r", ""), "\n")
	text = reImage.ReplaceAllString(text, "")
	text = reLink.ReplaceAllString(text, "$1")
	text = reParenthetical.ReplaceAllString(text, "")
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "|") || strings.HasPrefix(line, "---") {
			continue
		}
		line = reFiller.ReplaceAllString(reListMarker.ReplaceAllString(line, ""), "")
		sentence := firstSentences(line)
		trimmed := strings.TrimRightFunc(sentence, unicode.IsSpace)
		if strings.HasSuffix(trimmed, "：") || strings.HasSuffix(trimmed, ":") {
			if strings.Contains(trimmed, "如下") || strings.Contains(trimmed, "以下") {
				continue
			}
			r := []rune(trimmed)
			sentence = string(r[:len(r)-1])
		}
		if len([]rune(voiceClean(sentence, math.MaxInt))) >= 2 {
			return sentence
		}
	}
	return ""
}

// firstSentences：第一句（到。！？或英文句點加空白為止）；第一句不到 6 個字就再接下一句
func firstSentences(line string) string {
	r := []rune(line)
	var sb strings.Builder
	start := 0
	for start < len(r) {
		end := sentenceEnd(r, start)
		sb.WriteString(string(r[start:end]))
		start = end
		if len([]rune(voiceClean(sb.String(), math.MaxInt))) >= voiceSentenceMinLen {
			break
		}
	}
	return sb.String()
}

func sentenceEnd(r []rune, start int) int {
	for i := start; i < len(r); i++ {
		switch r[i] {
		case '。', '！', '？', '!', '?':
			return i + 1
		case '.':
			if i+1 == len(r) || unicode.IsSpace(r[i+1]) {
				return i + 1
			}
		}
	}
	return len(r)
}

func isClauseBreak(r rune) bool { return strings.ContainsRune("，、；：,;:。！？", r) }

// trimTrailingPunct：尾端念出來沒有意義的標點（問號留著，語調不同）
func trimTrailingPunct(s string) string { return strings.TrimRight(s, " ，、；：。,;:.…") }

// voiceFit：放進 budget 個字：先 voiceClean，太長時在後半段最後一個子句邊界斷，沒有邊界就直接截；尾端的標點拿掉
func voiceFit(text string, budget int) string {
	line := []rune(trimTrailingPunct(voiceClean(text, math.MaxInt)))
	if len(line) <= budget {
		return string(line)
	}
	cut := -1
	for i := budget; i >= budget/2 && i > 0; i-- {
		if i < len(line) && isClauseBreak(line[i]) {
			cut = i
			break
		}
	}
	if cut < 0 {
		cut = budget
	}
	return trimTrailingPunct(string(line[:cut]))
}

// voiceClean：念出來的純文字：網址換成「連結」、拿掉 Markdown 符號與控制字元、空白合併成一個，超過 max 字截斷
func voiceClean(text string, max int) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	plain := reSeparators.ReplaceAllString(reInlineSymbols.ReplaceAllString(reURL.ReplaceAllString(text, " 連結 "), ""), " ")
	var out []rune
	space := false
	for _, r := range plain {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = true
			continue
		}
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		if space && len(out) > 0 {
			out = append(out, ' ')
		}
		space = false
		out = append(out, r)
	}
	if len(out) <= max {
		return string(out)
	}
	return strings.TrimRightFunc(string(out[:max]), unicode.IsSpace)
}

func voiceFileName(p string) string {
	t := strings.TrimRight(strings.TrimSpace(p), "/\\")
	if t == "" {
		return ""
	}
	if i := strings.LastIndexAny(t, "/\\"); i >= 0 {
		return t[i+1:]
	}
	return path.Base(t)
}

func hasCJK(s string) bool {
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) {
			return true
		}
	}
	return false
}

// voiceShouldQueue：佇列已滿不排；和上一句相同而且還在 10 秒內不排（時鐘倒退時照排）
func voiceShouldQueue(text string, pending int, lastText string, lastAt time.Time, now time.Time) bool {
	if strings.TrimSpace(text) == "" || pending >= voiceMaxPending {
		return false
	}
	return lastAt.IsZero() || lastText != text || now.Sub(lastAt) >= voiceRepeatWindow || now.Before(lastAt)
}
