package schedules

import (
	"regexp"
	"strconv"
	"strings"
)

// 從代理執行的指令文字（工具卡的指令）找出它對這台的排程做了什麼（C# 的 ScheduleOriginRules＋LaunchdOriginRules；
// Windows 的 schtasks／ScheduledTask 在 macOS、Linux 上用不到，不移植）：
//   - launchctl 的 load／bootstrap（載入）、unload／bootout／remove（卸載）、enable、disable、kickstart／start（執行）、kill／stop（停止）；
//     提到 LaunchAgents、LaunchDaemons 底下的 .plist（寫入、複製、刪除）
//   - systemctl --user 的 enable／disable／start／stop／daemon-reload，以及 systemd/user 底下的 .timer、.service（Linux）
//   - 改 crontab（crontab -e、crontab 檔案、| crontab -、crontab -r）
//
// 名稱＝工作的標籤（或檔名去掉副檔名）；crontab 看不出是哪一行，名稱是空。只是找線索：判斷錯了頂多多掃一次、少一個連結

// Verb 指令對排程做了什麼（Unknown＝看得出在改排程、看不出是哪一個）
type Verb int

const (
	VerbCreate Verb = iota
	VerbChange
	VerbDelete
	VerbRun
	VerbEnd
	VerbEnable
	VerbDisable
	VerbUnknown
)

// CommandHit 一筆：動作＋名稱（看不出名稱是空字串）
type CommandHit struct {
	Verb Verb
	Name string
}

// Mutates 會改變排程的動作（不含執行、停止）
func Mutates(v Verb) bool {
	return v == VerbCreate || v == VerbChange || v == VerbDelete || v == VerbEnable || v == VerbDisable || v == VerbUnknown
}

var (
	reLaunchctl   = regexp.MustCompile(`(^|[^\w.-])launchctl\s+([a-z-]+)([^|;&\r\n]*)`)
	rePlistPath   = regexp.MustCompile(`(?:~|\$HOME|\$\{HOME\}|/)[^\s"'|;&<>]*?/Launch(?:Agents|Daemons)/([^/\s"'|;&<>]+)\.plist`)
	reCrontab     = regexp.MustCompile(`(^|[^\w.-])crontab([^|;&\r\n]*)`)
	reDelete      = regexp.MustCompile(`(^|[^\w.-])(?:rm|unlink|trash)\s`)
	reService     = regexp.MustCompile(`^(?:(?:gui|user)/\d+/(.+)|system/(.+))$`)
	reSystemctl   = regexp.MustCompile(`(^|[^\w.-])systemctl\s+([^|;&\r\n]*)`)
	reUnitPath    = regexp.MustCompile(`(?:~|\$HOME|\$\{HOME\}|\$XDG_CONFIG_HOME|/)[^\s"'|;&<>]*?/systemd/user/([^/\s"'|;&<>]+)\.(?:timer|service)`)
	reLineCont    = regexp.MustCompile("[`^\\\\][ \t]*\r?\n")
	reBracedVar   = regexp.MustCompile(`\$\{(\w+)\}`)
	reShellAssign = regexp.MustCompile(`(?m)(?:^|[\s;])([A-Za-z_]\w*)=(['"]?)([^'"\s;]+)['"]?`)
)

// MayTouch 快速篩選：文字裡有沒有可能碰到排程的字
func MayTouch(text string) bool {
	return strings.Contains(text, "launchctl") || strings.Contains(text, "LaunchAgents") || strings.Contains(text, "LaunchDaemons") ||
		strings.Contains(text, "crontab") || strings.Contains(text, "systemctl") || strings.Contains(text, "systemd/user")
}

// ParseCommandHits 找出指令文字碰到的排程
func ParseCommandHits(text string) []CommandHit {
	var hits []CommandHit
	if text == "" || !MayTouch(text) {
		return hits
	}
	// 接續到下一行的指令（反斜線）先接成一行
	text = reLineCont.ReplaceAllString(text, " ")
	vars := shellVariables(text)
	add := func(h CommandHit) {
		h.Name = resolveVar(h.Name, vars)
		for _, x := range hits {
			if x == h {
				return
			}
		}
		hits = append(hits, h)
	}
	for _, m := range reLaunchctl.FindAllStringSubmatch(text, -1) {
		var verb Verb
		switch m[2] {
		case "load", "bootstrap":
			verb = VerbCreate
		case "unload", "bootout", "remove":
			verb = VerbDelete
		case "enable":
			verb = VerbEnable
		case "disable":
			verb = VerbDisable
		case "kickstart", "start":
			verb = VerbRun
		case "kill", "stop":
			verb = VerbEnd
		default:
			continue
		}
		add(CommandHit{verb, launchctlTarget(m[3])})
	}
	// 直接碰 plist 檔（寫入、複製、刪除）。launchctl load 〈plist〉 已經算過的不重複
	for _, loc := range rePlistPath.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		lineStart := strings.LastIndexAny(text[:loc[0]], "\n;&|") + 1
		before := text[lineStart:loc[0]]
		if strings.Contains(before, "launchctl") {
			continue
		}
		v := VerbChange
		if reDelete.MatchString(before) {
			v = VerbDelete
		}
		add(CommandHit{v, name})
	}
	for _, m := range reSystemctl.FindAllStringSubmatch(text, -1) {
		parseSystemctl(m[2], add)
	}
	for _, loc := range reUnitPath.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		lineStart := strings.LastIndexAny(text[:loc[0]], "\n;&|") + 1
		before := text[lineStart:loc[0]]
		if strings.Contains(before, "systemctl") {
			continue
		}
		v := VerbChange
		if reDelete.MatchString(before) {
			v = VerbDelete
		}
		add(CommandHit{v, name})
	}
	for _, m := range reCrontab.FindAllStringSubmatch(text, -1) {
		rest := strings.TrimSpace(m[2])
		// crontab -l 只是列出
		if rest == "-l" || strings.HasPrefix(rest, "-l ") {
			continue
		}
		v := VerbUnknown
		if strings.HasPrefix(rest, "-r") {
			v = VerbDelete
		}
		add(CommandHit{v, ""})
	}
	return hits
}

// parseSystemctl systemctl 的參數：只看 --user 的（系統的服務不是使用者的排程）
func parseSystemctl(rest string, add func(CommandHit)) {
	tokens := Tokens(rest)
	user := false
	var verb *Verb
	var names []string
	for _, t := range tokens {
		switch {
		case t == "--user":
			user = true
		case strings.HasPrefix(t, "-"):
		case verb == nil:
			var v Verb
			switch t {
			case "enable", "link", "reenable", "daemon-reload":
				v = VerbCreate
				if t == "daemon-reload" {
					v = VerbUnknown
				}
			case "disable", "mask":
				v = VerbDisable
			case "start", "restart":
				v = VerbRun
			case "stop", "kill":
				v = VerbEnd
			case "edit", "revert", "set-property":
				v = VerbChange
			default:
				return
			}
			verb = &v
		default:
			names = append(names, t)
		}
	}
	if !user || verb == nil {
		return
	}
	if len(names) == 0 {
		add(CommandHit{*verb, ""})
		return
	}
	for _, n := range names {
		n = n[strings.LastIndex(n, "/")+1:]
		n = strings.TrimSuffix(strings.TrimSuffix(n, ".timer"), ".service")
		if n != "" && !strings.Contains(n, "$") {
			add(CommandHit{*verb, n})
		} else {
			add(CommandHit{*verb, ""})
		}
	}
}

func shellVariables(text string) map[string]string {
	vars := map[string]string{}
	text = reBracedVar.ReplaceAllString(text, "$$$1")
	for _, m := range reShellAssign.FindAllStringSubmatch(text, -1) {
		if _, ok := vars["$"+m[1]]; !ok {
			vars["$"+m[1]] = strings.TrimSpace(m[3])
		}
	}
	return vars
}

func resolveVar(name string, vars map[string]string) string {
	if !strings.Contains(name, "$") {
		return name
	}
	key := strings.ReplaceAll(strings.ReplaceAll(name, "${", "$"), "}", "")
	if v, ok := vars[key]; ok {
		return v
	}
	return ""
}

// launchctlTarget launchctl 子指令後面的對象：網域開頭的工作名稱、.plist 的檔名、或單獨一個標籤（remove、start、stop）；看不出來回空
func launchctlTarget(rest string) string {
	fallback := ""
	for _, token := range Tokens(rest) {
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		if m := reService.FindStringSubmatch(token); m != nil {
			if m[1] != "" {
				return m[1]
			}
			return m[2]
		}
		if strings.HasSuffix(strings.ToLower(token), ".plist") {
			return LaunchdStemOf(token)
		}
		// 網域本身（bootstrap gui/501 〈plist〉）、訊號（kill SIGTERM …）、變數不是名稱
		if _, err := strconv.Atoi(token); err == nil || strings.Contains(token, "/") || strings.HasPrefix(token, "SIG") || strings.Contains(token, "$") {
			continue
		}
		if fallback == "" {
			fallback = token
		}
	}
	// 對象是算出來的（gui/$(id -u)/標籤 會被切散）：寧可說看不出來，也不要把 id 之類的字當成名稱
	if strings.Contains(rest, "$(") || strings.Contains(rest, "`") {
		return ""
	}
	return fallback
}

// Tokens 以空白、逗號、方括號、圓括號切成記號（引號裡的不切、去掉外層引號）
func Tokens(text string) []string {
	var tokens []string
	var sb strings.Builder
	var quote rune
	any := false
	for _, ch := range text {
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				sb.WriteRune(ch)
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			any = true
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' || ch == '[' || ch == ']' || ch == '(' || ch == ')' || ch == '{' || ch == '}' {
			if sb.Len() > 0 || any {
				tokens = append(tokens, sb.String())
			}
			sb.Reset()
			any = false
			continue
		}
		if ch == '`' {
			continue
		}
		sb.WriteRune(ch)
		any = true
	}
	if sb.Len() > 0 || any {
		tokens = append(tokens, sb.String())
	}
	return tokens
}

// NameMatches 指令裡的名稱對不對得上排程（taskPath 是 .plist／.timer 的完整路徑）：名稱是路徑的比結尾（~ 開頭的去掉 ~），
// 否則比檔名去掉副檔名。crontab 的排程（taskPath 不是 / 開頭）沒有名稱可比
func NameMatches(hitName, taskPath string) bool {
	name := strings.TrimSpace(hitName)
	if name == "" || !strings.HasPrefix(taskPath, "/") {
		return false
	}
	if strings.Contains(name, "/") {
		tail := strings.TrimPrefix(name, "~")
		return strings.EqualFold(taskPath, tail) || (strings.HasPrefix(name, "~") && strings.HasSuffix(strings.ToLower(taskPath), strings.ToLower(tail)))
	}
	stem := LaunchdStemOf(taskPath)
	stem = strings.TrimSuffix(stem, ".timer")
	return strings.EqualFold(stem, name)
}
