package agent

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 對話開出的背景程式（C# Background/BackgroundProgramRules.cs）的純規則。
// agent 啟動每個對話的 CLI 時在環境變數 MYRIAGENT_AGENT_CONVERSATION 放對話 id；代理以 nohup、setsid 等開出、脫離 CLI 的程序都繼承它
// （父程序結束、agent 重開之後都還在）。分類（classifyPrograms）：帶標記、主控台程式、不在 agent 底下（CLI 與它還連著的子孫）的才算——
// 回合中的工具、MCP 伺服器、run_in_background 多半還在 CLI 底下。Unix 的「脫離」就是父子關係斷了：程序被 launchd／init（或 subreaper）收養。
// 但「不在 CLI 底下」只是候選：中樞只把撐過一輪結束的當成背景程式、對得上 CLI 背景工作的（isCliTask）不算（bgprograms.go）

const (
	// 平常多久列一次程序；一輪結束時另外在 programRescanDelay 後立刻掃一次
	programScanInterval = 15 * time.Second
	programRescanDelay  = 500 * time.Millisecond
	// 帳戶卡的說明每個對話最多列幾個程式、命令列最多幾個字
	maxListedPrograms   = 4
	maxProgramCmdLength = 80
	// 對話標頭的清單：命令列最多幾個字、最多幾個程式
	maxHeaderCmdLength = 240
	maxHeaderPrograms  = 20
	// 往上找父程序最多幾層（防環）
	maxProgramDepth = 64
)

// processRow：程序快照的一列。Marker 是標記（對話 id；讀不到或沒有是空的）；IsConsole：nil＝不知道；CommandLine 只有帶標記的才讀
type processRow struct {
	Pid, Ppid   int
	Start       int64 // 建立時間（Unix 微秒）
	Name        string
	Marker      string
	IsConsole   *bool
	CommandLine string
}

// bgProgram：一個背景程式（一棵脫離 CLI 的程序樹的根）。Ppid 是快照裡的父程序 PID（根結束了、子程序接著當根的情形靠它認出來）
type bgProgram struct {
	Pid         int
	Name        string
	CommandLine string
	Start       int64
	Ppid        int
}

// id：給網頁的 id（PID 會被重複使用，連建立時間一起）
func (p bgProgram) id() string { return fmt.Sprintf("%d-%d", p.Pid, p.Start) }

// bgExit：一個結束了的背景程式；ExitCode、ExitedAt（Unix 微秒）讀不到是 nil（macOS／Linux 讀不到別人的子程序的結束碼）
type bgExit struct {
	ConvID        string
	Program       bgProgram
	ExitCode      *int
	ExitedAt      *int64
	StoppedByUser bool
}

// 主控台的宿主程序與建置工具留著的常駐程序（不是代理開的程式）
var ignoredProgramNames = map[string]bool{"conhost": true, "openconsole": true, "vbcscompiler": true, "mspdbsrv": true, "vctip": true}

// isMarker：標記的值合格（對話 id 的格式：英數、底線、減號，1～64 字）
func isMarker(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for _, ch := range v {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

// markerOf：環境變數清單（NAME=VALUE）裡的標記；沒有、或值不合格回空字串
func markerOf(env []string) string {
	prefix := envConversation + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			if v := kv[len(prefix):]; isMarker(v) {
				return v
			}
			return ""
		}
	}
	return ""
}

// classifyPrograms 從程序快照找出各對話的背景程式（每棵脫離 CLI 的程序樹只算根）。managed 是 agent 自己與 CLI 程序的 PID：
// 它們與活著的子孫都不算。父程序以 PID 對上、而且建立得比子程序早才算活著的父程序（PID 被重複使用時不會誤接）。
// 子系統不是主控台的（視窗程式）不算，例外見 isBackgroundGuiProgram；不知道的照算
func classifyPrograms(rows []processRow, managed map[int]bool) map[string][]bgProgram {
	byPid := map[int]*processRow{}
	for i := range rows {
		if _, ok := byPid[rows[i].Pid]; !ok {
			byPid[rows[i].Pid] = &rows[i]
		}
	}
	liveParent := func(r *processRow) *processRow {
		if r.Ppid == r.Pid {
			return nil
		}
		if p := byPid[r.Ppid]; p != nil && p.Start <= r.Start {
			return p
		}
		return nil
	}
	isManaged := func(r *processRow) bool {
		cur := r
		for d := 0; d < maxProgramDepth && cur != nil; d++ {
			if managed[cur.Pid] {
				return true
			}
			cur = liveParent(cur)
		}
		return false
	}
	isBuildDaemon := func(r *processRow) bool {
		return ignoredProgramNames[programKey(r.Name)] || isToolDaemon(r.Name, r.CommandLine)
	}
	underBuildDaemon := func(r *processRow) bool {
		cur := r
		for d := 0; d < maxProgramDepth; d++ {
			p := liveParent(cur)
			if p == nil {
				return false
			}
			if isBuildDaemon(p) {
				return true
			}
			cur = p
		}
		return false
	}
	isCandidate := func(r *processRow) bool {
		return r.Marker != "" && (r.IsConsole == nil || *r.IsConsole || isBackgroundGuiProgram(r.Name, r.CommandLine)) &&
			!isBuildDaemon(r) && !isManaged(r) && !underBuildDaemon(r)
	}
	out := map[string][]bgProgram{}
	for i := range rows {
		r := &rows[i]
		if !isCandidate(r) {
			continue
		}
		// 父程序也是同一個對話的背景程式：它是同一棵樹，只算根
		if p := liveParent(r); p != nil && p.Marker == r.Marker && isCandidate(p) {
			continue
		}
		out[r.Marker] = append(out[r.Marker], bgProgram{Pid: r.Pid, Name: r.Name, CommandLine: r.CommandLine, Start: r.Start, Ppid: r.Ppid})
	}
	for _, list := range out {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Start != list[j].Start {
				return list[i].Start < list[j].Start
			}
			return list[i].Pid < list[j].Pid
		})
	}
	return out
}

// programKey：比對用的程式名稱：小寫、去掉 Windows 的「.exe」
func programKey(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".exe")
}

// isToolDaemon：建置與版本控制工具自己留著的常駐程序（MSBuild 節點重用、VBCSCompiler／Razor 編譯伺服器、Gradle 與 Kotlin 的 daemon、
// git fsmonitor、adb 伺服器、LocalDB）。命令列讀不到的照算
func isToolDaemon(name, cmd string) bool {
	if cmd == "" {
		return false
	}
	lc := strings.ToLower(cmd)
	has := func(s string) bool { return strings.Contains(lc, strings.ToLower(s)) }
	switch programKey(name) {
	case "msbuild":
		return has("nodemode:")
	case "dotnet":
		return has("VBCSCompiler.dll") || has("rzc.dll") || (has("MSBuild.dll") && has("nodemode:"))
	case "java", "javaw":
		return has("GradleDaemon") || has("KotlinCompileDaemon")
	case "git":
		return has("fsmonitor--daemon")
	case "adb":
		return has("fork-server")
	case "sqlservr":
		return has("LocalDB")
	}
	return false
}

// isBackgroundGuiProgram：不是主控台程式、但也不是開給人看的視窗程式：沒有視窗的背景執行器（pythonw、javaw、wscript），
// 與自動化用的瀏覽器（帶 --remote-debugging-port／--remote-debugging-pipe／--headless 的 Chromium 系列、帶 --marionette／-headless 的 Firefox）
func isBackgroundGuiProgram(name, cmd string) bool {
	lc := strings.ToLower(cmd)
	switch programKey(name) {
	case "pythonw", "javaw", "wscript":
		return true
	case "chrome", "msedge", "brave", "chromium", "vivaldi", "google chrome", "google chrome for testing", "microsoft edge", "brave browser":
		return strings.Contains(lc, "--remote-debugging-port") || strings.Contains(lc, "--remote-debugging-pipe") || strings.Contains(lc, "--headless")
	case "firefox":
		return strings.Contains(lc, "--marionette") || strings.Contains(lc, "-headless")
	}
	return false
}

// isCliTask：這個程式是 CLI 自己追蹤的背景工作（taskTexts 是會話回報的背景工作的工具說明與完整指令）：
// 程式的命令列（執行檔去掉路徑、加上參數）正規化後出現在其中一個裡面。沒有參數的不比（只有執行檔名太容易誤配）
func isCliTask(p bgProgram, taskTexts []string) bool {
	command := normalizeCommand(shortCommandN(p.CommandLine, p.Name, math.MaxInt32))
	if !strings.Contains(command, " ") {
		return false
	}
	for _, t := range taskTexts {
		if strings.TrimSpace(t) != "" && strings.Contains(normalizeCommand(t), command) {
			return true
		}
	}
	return false
}

var msysDrive = regexp.MustCompile(`(^|[\s=(])/([a-z])/`)

// normalizeCommand：比對命令列用：小寫、反斜線換斜線、MSYS 的 /c/… 換成 c:/…、拿掉引號、空白壓成一個
func normalizeCommand(text string) string {
	s := strings.ToLower(strings.NewReplacer(`\`, "/", `"`, "", "'", "").Replace(text))
	s = msysDrive.ReplaceAllString(s, "$1$2:/")
	return strings.Join(strings.Fields(s), " ")
}

// programExits：兩次掃描之間自己結束的背景程式：上一次有、這一次沒有，而且不是根結束了、它的子程序接著當根
func programExits(previous, current map[string][]bgProgram) []bgExit {
	var exits []bgExit
	ids := make([]string, 0, len(previous))
	for id := range previous {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		now := current[id]
	next:
		for _, p := range previous[id] {
			for _, q := range now {
				if q.Pid == p.Pid && q.Start == p.Start {
					continue next
				}
				if q.Ppid == p.Pid && q.Start >= p.Start {
					continue next
				}
			}
			exits = append(exits, bgExit{ConvID: id, Program: p})
		}
	}
	return exits
}

// programExitNotice：程式自己結束時加在對話裡的提示：「代理另開的程式「python worker6.py」已結束（結束碼 1，執行了 12 分鐘）」
func programExitNotice(e bgExit) string {
	var parts []string
	if e.ExitCode != nil {
		parts = append(parts, exitCodeText(*e.ExitCode))
	}
	if e.ExitedAt != nil && *e.ExitedAt > e.Program.Start {
		parts = append(parts, "執行了 "+durationText(time.Duration(*e.ExitedAt-e.Program.Start)*time.Microsecond))
	}
	detail := ""
	if len(parts) > 0 {
		detail = "（" + strings.Join(parts, "，") + "）"
	}
	return fmt.Sprintf("代理另開的程式「%s」已結束%s", shortCommand(e.Program.CommandLine, e.Program.Name), detail)
}

// exitCodeText：結束碼給人看的說法。-1（被強制結束）寫「被強制結束」、0xC000013A「被中斷（Ctrl+C）」、其他大於 0xFFFF 的以十六進位
func exitCodeText(code int) string {
	u := uint32(code)
	switch {
	case u == 0xFFFFFFFF:
		return "被強制結束"
	case u == 0xC000013A:
		return "被中斷（Ctrl+C）"
	case u > 0xFFFF:
		return fmt.Sprintf("結束碼 0x%08X", u)
	}
	return fmt.Sprintf("結束碼 %d", code)
}

// durationText：不到 1 分鐘以秒、不到 1 小時以分鐘、之後以小時加分鐘
func durationText(d time.Duration) string {
	switch {
	case d < time.Minute:
		s := int(math.Round(d.Seconds()))
		if s < 1 {
			s = 1
		}
		return fmt.Sprintf("%d 秒", s)
	case d < time.Hour:
		return fmt.Sprintf("%d 分鐘", int(d.Minutes()))
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if m == 0 {
		return fmt.Sprintf("%d 小時", h)
	}
	return fmt.Sprintf("%d 小時 %d 分", h, m)
}

// shortCommand：給人看的命令列（最多 maxProgramCmdLength 字）
func shortCommand(cmd, name string) string { return shortCommandN(cmd, name, maxProgramCmdLength) }

// shortCommandN：第一個參數（執行檔，可能有引號）換成不含路徑與 .exe 的檔名，其他照舊，空白壓成一個，超過 max 字截斷加「…」。沒有命令列時用程序名稱
func shortCommandN(cmd, name string, max int) string {
	text := strings.TrimSpace(cmd)
	var exe, rest string
	switch {
	case text == "":
		exe = name
	case text[0] == '"':
		if end := strings.IndexByte(text[1:], '"'); end < 0 {
			exe = text[1:]
		} else {
			exe, rest = text[1:end+1], text[end+2:]
		}
	default:
		if sp := strings.IndexByte(text, ' '); sp < 0 {
			exe = text
		} else {
			exe, rest = text[:sp], text[sp:]
		}
	}
	file := strings.ReplaceAll(exe, "/", `\`)
	file = file[strings.LastIndex(file, `\`)+1:]
	if strings.HasSuffix(strings.ToLower(file), ".exe") {
		file = file[:len(file)-4]
	}
	joined := strings.Join(strings.Fields(file+" "+rest), " ")
	if joined == "" {
		joined = name
	}
	if utf8.RuneCountInString(joined) <= max {
		return joined
	}
	return string([]rune(joined)[:max-1]) + "…"
}

// quoteArg：組命令列時，有空白的參數加引號（同 C# 的 Quote）
func quoteArg(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\n") {
		return a
	}
	return `"` + a + `"`
}
