package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// 執行列的純規則（照 C# 的 src/Myriagent/Launch/LaunchRules*.cs、LaunchLog.cs）：在對話的工作資料夾找可以「建置並執行」的視窗程式專案、
// 決定按鈕做什麼、組出建置與執行的指令、整理輸出、產生給網頁的 LaunchView（web-protocol 的 launchStatus）。
// 只認得視窗程式（主控台程式、網站、測試專案不列）：SDK 式 .NET 專案的 WinExe（WinForms、WPF 只能在 Windows 執行，這裡不列；
// Avalonia、net*-macos 可以），與 package.json 有 electron 而且有 start（其次 dev）指令碼的 Electron 專案。

type launchKind int

const (
	// launchDotNet：.NET 的視窗程式：dotnet build 再 dotnet run --no-build
	launchDotNet launchKind = iota
	// launchElectron：npm run start（或 dev）
	launchElectron
)

// launchMode：Debug＝執行的程式是 agent 的子程序，agent 結束時一起結束；Release＝和 agent 脫鉤，agent 重新啟動後接回。每個專案各自記得
type launchMode int

const (
	launchDebug launchMode = iota
	launchRelease
)

type launchPhase int

const (
	launchIdle launchPhase = iota
	launchBuilding
	launchRunning
	launchPaused
	launchStopping
)

type launchEndKind int

const (
	// launchExited：程式自己結束（關掉視窗、當掉）
	launchExited launchEndKind = iota
	// launchStopped：使用者按了停止
	launchStopped
	launchBuildFailed
	// launchStartFailed：程序開不起來（找不到 dotnet、npm…）
	launchStartFailed
)

type launchStep int

const (
	launchStepNone launchStep = iota
	launchStepStart
	launchStepStop
	launchStepPause
	launchStepResume
	// launchStepRestart：停掉、等它結束，再重新建置與執行
	launchStepRestart
)

type launchScanTrigger int

const (
	launchScanOpen launchScanTrigger = iota
	launchScanChanged
	launchScanUser
	launchScanPeriodic
)

const (
	launchMaxDepth            = 3
	launchMaxDirectories      = 2000
	launchMaxTargets          = 20
	launchMaxProjectFileBytes = 1024 * 1024
	launchLogCapacity         = 2000
	launchMaxLineLength       = 2000
	launchOutputPostDelay     = 300 * time.Millisecond
	launchOutputDrainTimeout  = 2 * time.Second
	launchOpenRescanAge       = 30 * time.Second
	launchChangedRescanAge    = 3 * time.Second
	launchPeriodicRescanAge   = 60 * time.Second
	launchPrefsFileName       = "prefs.json"
	launchStateFileName       = "runs.json"
	launchMaxPrefs            = 200
	launchDefaultMode         = launchDebug
	launchMaxLogFileBytes     = 16 * 1024 * 1024
	launchLogPollInterval     = 200 * time.Millisecond
	launchReattachedNote      = "（Myriagent 重新啟動前開的程式，已接回）"
)

// launchTarget：一個可以執行的專案。ID 是專案檔完整路徑的雜湊（同一個專案不論從哪個資料夾偵測到都是同一個，執行狀態共用）
type launchTarget struct {
	ID           string
	Kind         launchKind
	Name         string
	ProjectPath  string
	RelativePath string
	Detail       string
	Framework    string // 要以 -f 指定的目標架構（多目標的專案）
	Script       string // npm 的指令碼名稱（Electron）
}

func (t launchTarget) dir() string { return filepath.Dir(t.ProjectPath) }

// launchCommand：執行檔（PATH 上的名稱或完整路徑）、參數（逐一傳，不經殼層）、工作資料夾
type launchCommand struct {
	File string
	Args []string
	Dir  string
}

type launchEnd struct {
	Kind     launchEndKind
	ExitCode *int
	Text     string
	At       time.Time
}

// launchSnapshot：一個目標的執行狀態（給 renderLaunch）；RunMode 只在不是閒置時有意義
type launchSnapshot struct {
	Phase    launchPhase
	Since    *time.Time
	Last     *launchEnd
	LogSeq   int64
	LogEpoch int64
	RunMode  *launchMode
}

var launchSkippedDirs = map[string]bool{
	"bin": true, "obj": true, "node_modules": true, "packages": true, "dist": true, "out": true, "build": true, "target": true,
	"artifacts": true, "testresults": true, "__pycache__": true, "venv": true, "library": true, "temp": true, "logs": true,
}

var launchElectronScripts = []string{"start", "dev"}

// ───── 偵測 ─────

// launchShouldDescend：這個資料夾要不要往下找（工作資料夾本身一律找）。名稱以點開頭、建置輸出與套件資料夾、符號連結不找
func launchShouldDescend(name string, symlink bool) bool {
	return name != "" && name[0] != '.' && !launchSkippedDirs[strings.ToLower(name)] && !symlink
}

func launchIsDotNetProject(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".csproj") || strings.HasSuffix(l, ".fsproj") || strings.HasSuffix(l, ".vbproj")
}

func launchIsCandidateFile(name string) bool {
	return launchIsDotNetProject(name) || strings.EqualFold(name, "package.json")
}

// xmlNode：讀專案檔用的最小 XML 樹
type xmlNode struct {
	name     string
	attrs    map[string]string
	children []*xmlNode
	text     strings.Builder
}

func (n *xmlNode) attr(name string) (string, bool) {
	v, ok := n.attrs[name]
	return v, ok
}

// value：所有子孫的文字（同 XElement.Value）
func (n *xmlNode) value() string {
	var b strings.Builder
	var walk func(x *xmlNode)
	walk = func(x *xmlNode) {
		b.WriteString(x.text.String())
		for _, c := range x.children {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// parseProjectXML：不處理 DTD（有 DOCTYPE 一律當成壞掉）、不解析外部實體；壞掉回 nil
func parseProjectXML(text string) *xmlNode {
	d := xml.NewDecoder(strings.NewReader(text))
	d.Strict = true
	var root *xmlNode
	var stack []*xmlNode
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil
		case xml.StartElement:
			n := &xmlNode{name: t.Name.Local, attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.attrs[a.Name.Local] = a.Value
			}
			if len(stack) == 0 {
				if root != nil {
					return nil
				}
				root = n
			} else {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		}
	}
	return root
}

// launchOtherPlatforms：這台不能執行的目標架構（-windows、-android…）；Linux 另外不能執行 -macos
func launchIsOtherPlatformFramework(framework, goos string) bool {
	others := []string{"-windows", "-android", "-ios", "-maccatalyst", "-tvos", "-tizen", "-browser"}
	if goos != "darwin" {
		others = append(others, "-macos")
	}
	l := strings.ToLower(framework)
	for _, p := range others {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// launchFromDotNetProject：.NET 專案檔是不是可以在這台（goos）執行的視窗程式；不是（主控台、程式庫、網站、測試、MAUI、舊式非 SDK 專案、
// XML 壞掉、WinForms／WPF、只有其他平台的目標架構）回 nil。只看沒有 Condition 的屬性。多目標的 macOS 優先選 -macos、其次不分平台的
func launchFromDotNetProject(projectPath, relativePath, text, goos string) *launchTarget {
	root := parseProjectXML(text)
	if root == nil || root.name != "Project" {
		return nil
	}
	_, sdkAttr := root.attr("Sdk")
	sdkStyle := sdkAttr
	for _, e := range root.children {
		if e.name == "Sdk" {
			sdkStyle = true
		}
		if _, ok := e.attr("Sdk"); ok && e.name == "Import" {
			sdkStyle = true
		}
	}
	if !sdkStyle {
		return nil
	}
	props := map[string]string{}
	for _, g := range root.children {
		if _, cond := g.attr("Condition"); g.name != "PropertyGroup" || cond {
			continue
		}
		for _, p := range g.children {
			if _, cond := p.attr("Condition"); !cond {
				props[strings.ToLower(p.name)] = strings.TrimSpace(p.value())
			}
		}
	}
	packages := map[string]bool{}
	var walk func(n *xmlNode)
	walk = func(n *xmlNode) {
		for _, c := range n.children {
			if c.name == "PackageReference" {
				if inc := strings.TrimSpace(c.attrs["Include"]); inc != "" {
					packages[strings.ToLower(inc)] = true
				}
			}
			walk(c)
		}
	}
	walk(root)
	prop := func(name string) string { return props[strings.ToLower(name)] }
	flag := func(name string) bool { return strings.EqualFold(prop(name), "true") }

	if flag("IsTestProject") || packages["microsoft.net.test.sdk"] || flag("UseMaui") {
		return nil
	}
	winForms, wpf := flag("UseWindowsForms"), flag("UseWPF")
	outputType := prop("OutputType")
	windowed := strings.EqualFold(outputType, "WinExe") || (strings.EqualFold(outputType, "Exe") && (winForms || wpf))
	if !windowed {
		return nil
	}
	// WinForms、WPF 只能在 Windows 執行
	if winForms || wpf {
		return nil
	}
	framework := ""
	shown := prop("TargetFramework")
	if list := prop("TargetFrameworks"); list != "" && !strings.Contains(list, "$(") {
		var all []string
		for _, f := range strings.Split(list, ";") {
			if f = strings.TrimSpace(f); f != "" {
				all = append(all, f)
			}
		}
		if goos == "darwin" {
			for _, f := range all {
				if strings.Contains(strings.ToLower(f), "-macos") {
					framework = f
					break
				}
			}
		}
		if framework == "" {
			for _, f := range all {
				if !strings.HasPrefix(strings.ToLower(f), "netstandard") && !launchIsOtherPlatformFramework(f, goos) {
					framework = f
					break
				}
			}
		}
		// 這台一個都不能執行（例如只有 net10.0-windows 與 netstandard2.0）
		if framework == "" {
			return nil
		}
		shown = framework
	} else if shown != "" && launchIsOtherPlatformFramework(shown, goos) {
		return nil
	}
	if strings.Contains(shown, "$(") {
		shown = ""
	}
	ui := "WinExe"
	avalonia := false
	for p := range packages {
		if strings.HasPrefix(p, "avalonia") {
			avalonia = true
		}
	}
	if avalonia {
		ui = "Avalonia"
	} else if strings.Contains(strings.ToLower(shown), "-macos") {
		ui = "macOS"
	}
	name := strings.TrimSuffix(filepath.Base(projectPath), filepath.Ext(projectPath))
	if a := prop("AssemblyName"); a != "" && !strings.Contains(a, "$(") && utf8.RuneCountInString(a) <= 80 {
		name = a
	}
	detail := ui
	if shown != "" {
		detail = ui + " · " + shown
	}
	return &launchTarget{ID: launchTargetID(projectPath), Kind: launchDotNet, Name: name, ProjectPath: projectPath, RelativePath: relativePath, Detail: detail, Framework: framework}
}

// launchFromPackageJSON：dependencies 或 devDependencies 有 electron，而且有 start 或 dev 指令碼；不是回 nil
func launchFromPackageJSON(path, relativePath, text string) *launchTarget {
	var pkg map[string]any
	if json.Unmarshal([]byte(text), &pkg) != nil || pkg == nil {
		return nil
	}
	has := func(group string) bool {
		deps, ok := pkg[group].(map[string]any)
		if !ok {
			return false
		}
		_, ok = deps["electron"]
		return ok
	}
	if !has("dependencies") && !has("devDependencies") {
		return nil
	}
	scripts, _ := pkg["scripts"].(map[string]any)
	script := ""
	for _, s := range launchElectronScripts {
		if v, ok := scripts[s].(string); ok && strings.TrimSpace(v) != "" {
			script = s
			break
		}
	}
	if script == "" {
		return nil
	}
	name := filepath.Base(filepath.Dir(path))
	if n, ok := pkg["name"].(string); ok && n != "" && utf8.RuneCountInString(n) <= 80 {
		name = n
	}
	return &launchTarget{ID: launchTargetID(path), Kind: launchElectron, Name: name, ProjectPath: path, RelativePath: relativePath, Detail: "Electron · npm run " + script, Script: script}
}

// launchTargetID：專案檔完整路徑（不分大小寫）的 SHA-256 前 12 碼
func launchTargetID(projectPath string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(projectPath)))
	return "t" + hex.EncodeToString(sum[:6])
}

var launchTargetIDPattern = regexp.MustCompile(`^t[0-9a-f]{12}$`)

func launchIsTargetID(id string) bool { return launchTargetIDPattern.MatchString(id) }

// launchOrder：去掉重複、淺的在前（工作資料夾本身的專案最前面）、同一層依相對路徑（不分大小寫），最多 20 個
func launchOrder(targets []launchTarget) []launchTarget {
	seen := map[string]bool{}
	out := []launchTarget{}
	for _, t := range targets {
		if !seen[t.ID] {
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	depth := func(p string) int { return strings.Count(p, "/") + strings.Count(p, `\`) }
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := depth(out[i].RelativePath), depth(out[j].RelativePath)
		if di != dj {
			return di < dj
		}
		return strings.ToLower(out[i].RelativePath) < strings.ToLower(out[j].RelativePath)
	})
	if len(out) > launchMaxTargets {
		out = out[:launchMaxTargets]
	}
	return out
}

// launchSelect：之前選的還在就留著，否則第一個；沒有目標是空字串
func launchSelect(targets []launchTarget, previous string) string {
	for _, t := range targets {
		if previous != "" && t.ID == previous {
			return previous
		}
	}
	if len(targets) > 0 {
		return targets[0].ID
	}
	return ""
}

func launchShouldScan(trigger launchScanTrigger, scannedAt *time.Time, now time.Time) bool {
	if scannedAt == nil {
		return true
	}
	age := now.Sub(*scannedAt)
	switch trigger {
	case launchScanUser:
		return true
	case launchScanOpen:
		return age >= launchOpenRescanAge
	case launchScanChanged:
		return age >= launchChangedRescanAge
	default:
		return age >= launchPeriodicRescanAge
	}
}

// ───── 按鈕 ─────

// launchPlan：網頁的 action 在目前的階段要做什麼；不合時宜（執行中按啟動）是 None
func launchPlan(phase launchPhase, action string) launchStep {
	switch action {
	case "start":
		switch phase {
		case launchIdle:
			return launchStepStart
		case launchPaused:
			// 暫停中按啟動（F5）＝繼續，和 VS Code 一樣
			return launchStepResume
		}
	case "stop":
		if phase == launchBuilding || phase == launchRunning || phase == launchPaused {
			return launchStepStop
		}
	case "pause":
		if phase == launchRunning {
			return launchStepPause
		}
	case "resume":
		if phase == launchPaused {
			return launchStepResume
		}
	case "restart":
		if phase == launchIdle {
			return launchStepStart
		}
		// 停止中按重啟：等它結束後照樣重新建置與執行
		return launchStepRestart
	}
	return launchStepNone
}

// ───── 指令 ─────

func launchConfiguration(m launchMode) string {
	if m == launchRelease {
		return "Release"
	}
	return "Debug"
}

func launchFrameworkArgs(t launchTarget) []string {
	if t.Framework != "" {
		return []string{"-f", t.Framework}
	}
	return nil
}

// launchBuildCommand：建置的指令；不必建置（Electron 由 npm 指令碼自己處理）回 nil
func launchBuildCommand(t launchTarget, m launchMode) *launchCommand {
	if t.Kind != launchDotNet {
		return nil
	}
	// -tl:off：不用終端機記錄器（輸出是一行一行的文字）；-nodeReuse:false：建置完 MSBuild 節點跟著結束，不留著鎖檔
	args := append([]string{"build", t.ProjectPath, "-c", launchConfiguration(m), "-nologo", "-tl:off", "-nodeReuse:false"}, launchFrameworkArgs(t)...)
	return &launchCommand{File: "dotnet", Args: args, Dir: t.dir()}
}

// launchRunCommand：.NET 用 dotnet run --no-build（照 launchSettings.json，工作資料夾是專案資料夾）；Electron 直接執行 PATH 上的 npm
func launchRunCommand(t launchTarget, m launchMode) launchCommand {
	if t.Kind == launchDotNet {
		args := append([]string{"run", "--project", t.ProjectPath, "-c", launchConfiguration(m), "--no-build"}, launchFrameworkArgs(t)...)
		return launchCommand{File: "dotnet", Args: args, Dir: t.dir()}
	}
	// 指令碼名稱只會是固定的字串，不會帶進專案檔裡的任何文字
	script := launchElectronScripts[0]
	for _, s := range launchElectronScripts {
		if s == t.Script {
			script = s
		}
	}
	return launchCommand{File: "npm", Args: []string{"run", script}, Dir: t.dir()}
}

// launchDisplayCommand：輸出開頭的那一行（「> dotnet build /…/App.csproj -c Debug …」）：含空白的參數加引號，只給人看
func launchDisplayCommand(c launchCommand) string {
	parts := []string{c.File}
	parts = append(parts, c.Args...)
	for i, a := range parts {
		if a == "" || strings.IndexFunc(a, unicode.IsSpace) >= 0 {
			parts[i] = `"` + a + `"`
		}
	}
	return "> " + strings.Join(parts, " ")
}

// launchIsInside：path 是不是 folder 本身或在它底下
func launchIsInside(path, folder string) bool {
	if path == folder {
		return true
	}
	prefix := folder
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(path, prefix)
}

// launchResolveExecutable：有路徑的照用（存在才回），只有名稱的在 PATH（只認完整路徑的項目）上找；找不到回空字串
func launchResolveExecutable(name, pathVar string, exists func(string) bool) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	if strings.HasPrefix(name, "/") {
		if exists(name) {
			return name
		}
		return ""
	}
	if strings.Contains(name, "/") {
		return ""
	}
	for _, raw := range strings.Split(pathVar, ":") {
		dir := strings.TrimSpace(raw)
		if dir == "" || !strings.HasPrefix(dir, "/") {
			continue
		}
		found := strings.TrimRight(dir, "/") + "/" + name
		if exists(found) {
			return found
		}
	}
	return ""
}

// ───── 輸出 ─────

// launchDecodeLine：先試 UTF-8；不是的（macOS、Linux 的終端機輸出都是 UTF-8，壞掉的位元組）以 Latin-1 一對一顯示
func launchDecodeLine(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

var launchAnsiPattern = regexp.MustCompile(`\x1B(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1B]*(?:\x07|\x1B\\)|[@-_])`)

// launchCleanLine：去掉 ANSI 色碼、開頭的 BOM、控制字元（tab 換成四個空白），超過 2000 字截掉加「…」
func launchCleanLine(line string) string {
	text := launchAnsiPattern.ReplaceAllString(line, "")
	text = strings.TrimPrefix(text, "\uFEFF")
	var b strings.Builder
	n := 0
	for _, ch := range text {
		if n >= launchMaxLineLength {
			b.WriteRune('…')
			break
		}
		if ch == '\t' {
			b.WriteString("    ")
			n += 4
		} else if !unicode.IsControl(ch) {
			b.WriteRune(ch)
			n++
		}
	}
	return b.String()
}

// MSBuild 的錯誤行：「檔案(行,欄): error 代碼: 訊息 [專案]」
var launchMsBuildError = regexp.MustCompile(`^\s*(?P<file>[^\s].*?)\((?P<pos>\d+(?:,\d+)*)\)\s*:\s*error\s+(?P<code>[A-Za-z]+\d+)\s*:\s*(?P<msg>.*?)(?:\s+\[[^\]]+\])?\s*$`)

// 其他錯誤行（沒有檔案位置的 MSBuild 錯誤、NETSDK、NU 開頭的還原錯誤）
var launchGenericError = regexp.MustCompile(`(?:^|:\s*)error\s+(?P<code>[A-Za-z]+\d+)\s*:\s*(?P<msg>.*?)(?:\s+\[[^\]]+\])?\s*$`)

// baseName：檔名（Windows 與 Unix 的分隔符號都認）
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// launchBuildErrorSummary：建置失敗時的說明：第一個錯誤（檔名只留名稱）；找不到錯誤行是空字串
func launchBuildErrorSummary(lines []string) string {
	for _, l := range lines {
		if m := launchMsBuildError.FindStringSubmatch(l); m != nil {
			return strings.TrimRight(fmt.Sprintf("%s(%s): %s %s", baseName(strings.TrimSpace(m[1])), m[2], m[3], m[4]), " \t")
		}
	}
	for _, l := range lines {
		if m := launchGenericError.FindStringSubmatch(l); m != nil {
			return strings.TrimRight(m[1]+" "+m[2], " \t")
		}
	}
	return ""
}

// launchFormatExitCode：一般的用十進位；負數或很大的用十六進位
func launchFormatExitCode(code int) string {
	if code >= 0 && code <= 0xFFFF {
		return fmt.Sprint(code)
	}
	return fmt.Sprintf("0x%08X", uint32(code))
}

func launchExitedEnd(code int, at time.Time) *launchEnd {
	text := "已結束"
	if code != 0 {
		text = fmt.Sprintf("已結束（結束碼 %s）", launchFormatExitCode(code))
	}
	return &launchEnd{Kind: launchExited, ExitCode: &code, Text: text, At: at}
}

func launchStoppedEnd(at time.Time) *launchEnd {
	return &launchEnd{Kind: launchStopped, Text: "已停止", At: at}
}

func launchBuildFailedEnd(code int, lines []string, at time.Time) *launchEnd {
	text := launchBuildErrorSummary(lines)
	if text == "" {
		text = fmt.Sprintf("建置失敗（結束碼 %s）", launchFormatExitCode(code))
	}
	return &launchEnd{Kind: launchBuildFailed, ExitCode: &code, Text: text, At: at}
}

func launchStartFailedEnd(message string, at time.Time) *launchEnd {
	return &launchEnd{Kind: launchStartFailed, Text: "無法啟動：" + message, At: at}
}

// ───── 給網頁的 LaunchView ─────

func launchKindName(k launchKind) string {
	if k == launchElectron {
		return "electron"
	}
	return "dotnet"
}

func launchModeName(m launchMode) string {
	if m == launchRelease {
		return "release"
	}
	return "debug"
}

// launchParseMode：網頁與 prefs.json 的模式名稱；不認得回 false
func launchParseMode(name string) (launchMode, bool) {
	switch name {
	case "debug":
		return launchDebug, true
	case "release":
		return launchRelease, true
	}
	return launchDebug, false
}

func launchPhaseName(p launchPhase) string {
	return [...]string{"idle", "building", "running", "paused", "stopping"}[p]
}

func launchEndKindName(k launchEndKind) string {
	return [...]string{"exited", "stopped", "buildFailed", "startFailed"}[k]
}

// renderLaunch：launchStatus 的 launch。沒有目標回 nil（網頁藏起執行列）
func renderLaunch(folder string, targets []launchTarget, selected string, runOf func(string) *launchSnapshot, modeOf func(string) launchMode) map[string]any {
	if len(targets) == 0 {
		return nil
	}
	chosen := targets[0]
	for _, t := range targets {
		if t.ID == selected {
			chosen = t
			break
		}
	}
	run := runOf(chosen.ID)
	list := make([]any, 0, len(targets))
	for _, t := range targets {
		phase := launchIdle
		if r := runOf(t.ID); r != nil {
			phase = r.Phase
		}
		list = append(list, map[string]any{"id": t.ID, "name": t.Name, "kind": launchKindName(t.Kind), "path": t.RelativePath, "detail": t.Detail, "phase": launchPhaseName(phase)})
	}
	out := map[string]any{
		"folder": folder, "selected": chosen.ID, "targets": list, "phase": "idle", "mode": launchModeName(modeOf(chosen.ID)),
		"runMode": nil, "since": nil, "last": nil, "logSeq": int64(0), "logEpoch": int64(0),
	}
	if run != nil {
		out["phase"] = launchPhaseName(run.Phase)
		if run.Phase != launchIdle && run.RunMode != nil {
			out["runMode"] = launchModeName(*run.RunMode)
		}
		if run.Since != nil {
			out["since"] = run.Since.UnixMilli()
		}
		if l := run.Last; l != nil {
			var code any
			if l.ExitCode != nil {
				code = *l.ExitCode
			}
			out["last"] = map[string]any{"kind": launchEndKindName(l.Kind), "code": code, "text": l.Text, "at": l.At.UnixMilli()}
		}
		out["logSeq"], out["logEpoch"] = run.LogSeq, run.LogEpoch
	}
	return out
}

// ───── 輸出的緩衝（LaunchLog）─────

// launchLog：一個目標的輸出，只留最後 capacity 行。讀輸出的 goroutine 寫入、中樞讀，所以自己加鎖。
// 每次啟動 restart 清空、epoch 加一，網頁看到 epoch 變了就整份重拿
type launchLog struct {
	mu       sync.Mutex
	capacity int
	lines    []string
	epoch    int64
	seq      int64
}

type launchLogSlice struct {
	Lines []string
	Epoch int64
	Seq   int64
	Reset bool
}

func newLaunchLog(capacity int) *launchLog { return &launchLog{capacity: capacity} }

func (l *launchLog) state() (epoch, seq int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.epoch, l.seq
}

func (l *launchLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.lines...)
}

func (l *launchLog) appendLocked(line string) {
	l.lines = append(l.lines, line)
	l.seq++
	if over := len(l.lines) - l.capacity; over > 0 {
		l.lines = append([]string{}, l.lines[over:]...)
	}
}

func (l *launchLog) append(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendLocked(line)
}

// appendIf：還是這個 epoch 才加（停掉的上一次執行晚到的輸出不混進新的一次）
func (l *launchLog) appendIf(epoch int64, line string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.epoch != epoch {
		return false
	}
	l.appendLocked(line)
	return true
}

func (l *launchLog) restart() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = nil
	l.seq = 0
	l.epoch++
}

// read：序號 after 之後的行；knownEpoch 不是目前的、after 之後的第一行已被丟掉、after 比目前的還大時給整份（Reset）
func (l *launchLog) read(knownEpoch, after int64) launchLogSlice {
	l.mu.Lock()
	defer l.mu.Unlock()
	first := l.seq - int64(len(l.lines)) + 1
	if knownEpoch != l.epoch || after < first-1 || after > l.seq {
		return launchLogSlice{Lines: append([]string{}, l.lines...), Epoch: l.epoch, Seq: l.seq, Reset: true}
	}
	return launchLogSlice{Lines: append([]string{}, l.lines[after-(first-1):]...), Epoch: l.epoch, Seq: l.seq}
}

func renderLaunchLog(convID, targetID string, s launchLogSlice) map[string]any {
	lines := s.Lines
	if lines == nil {
		lines = []string{}
	}
	return map[string]any{"type": "launchLog", "conversationId": convID, "targetId": targetID, "epoch": s.Epoch, "reset": s.Reset, "seq": s.Seq, "lines": lines}
}

// launchLineSplitter：把輸出的位元組以換行切開，每行各自解碼、清理；一直沒有換行的（進度列）滿了先送出
type launchLineSplitter struct {
	pending []byte
	onLine  func(string)
}

func (s *launchLineSplitter) feed(data []byte) {
	for _, b := range data {
		if b == '\n' {
			s.emit()
			continue
		}
		s.pending = append(s.pending, b)
		if len(s.pending) >= launchMaxLineLength*4 {
			s.emit()
		}
	}
}

// flush：最後一行沒有換行也送出
func (s *launchLineSplitter) flush() {
	if len(s.pending) > 0 {
		s.emit()
	}
}

func (s *launchLineSplitter) emit() {
	b := s.pending
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	s.onLine(launchCleanLine(launchDecodeLine(b)))
	s.pending = s.pending[:0]
}

// ───── 記得的選擇（launch/prefs.json）─────

type launchPrefsFile struct {
	Folders []launchFolderPref `json:"folders"`
	Targets []launchTargetPref `json:"targets"`
}

type launchFolderPref struct {
	Folder   string `json:"folder"`
	Selected string `json:"selected"`
}

type launchTargetPref struct {
	TargetID string `json:"targetId"`
	Mode     string `json:"mode"`
}

// launchSelectedFromPrefs：這個資料夾上次選的目標；沒記過（或記的不是目標的鍵）回空字串
func launchSelectedFromPrefs(p *launchPrefsFile, folder string) string {
	for _, f := range p.Folders {
		if f.Folder == folder {
			if launchIsTargetID(f.Selected) {
				return f.Selected
			}
			return ""
		}
	}
	return ""
}

// launchModeFromPrefs：這個目標選的執行模式；沒記過或不認得的是 Debug
func launchModeFromPrefs(p *launchPrefsFile, targetID string) launchMode {
	for _, t := range p.Targets {
		if t.TargetID == targetID {
			if m, ok := launchParseMode(t.Mode); ok {
				return m
			}
			break
		}
	}
	return launchDefaultMode
}

// launchRememberSelection：記下這個資料夾選的目標（移到最前面）；內容沒變回 false
func launchRememberSelection(p *launchPrefsFile, folder, targetID string) bool {
	if len(p.Folders) > 0 && p.Folders[0].Folder == folder && p.Folders[0].Selected == targetID {
		return false
	}
	list := []launchFolderPref{{Folder: folder, Selected: targetID}}
	for _, f := range p.Folders {
		if f.Folder != folder && len(list) < launchMaxPrefs {
			list = append(list, f)
		}
	}
	p.Folders = list
	return true
}

// launchRememberMode：記下這個目標的執行模式（移到最前面）；內容沒變回 false
func launchRememberMode(p *launchPrefsFile, targetID string, m launchMode) bool {
	name := launchModeName(m)
	if len(p.Targets) > 0 && p.Targets[0].TargetID == targetID && p.Targets[0].Mode == name {
		return false
	}
	list := []launchTargetPref{{TargetID: targetID, Mode: name}}
	for _, t := range p.Targets {
		if t.TargetID != targetID && len(list) < launchMaxPrefs {
			list = append(list, t)
		}
	}
	p.Targets = list
	return true
}

// launchDiesWithApp：這個階段的程式會跟著 agent 結束（Debug 模式執行中、暫停中）
func launchDiesWithApp(phase launchPhase, m launchMode) bool {
	return (phase == launchRunning || phase == launchPaused) && m == launchDebug
}

// launchBlocksAppRestart：建置與停止中（重啟會中斷它）
func launchBlocksAppRestart(phase launchPhase) bool {
	return phase == launchBuilding || phase == launchStopping
}

// ───── 和 agent 脫鉤的執行（launch/runs.json）─────

// launchProcRef：PID 加上開始時間（PID 會被重複使用，接回時兩個都對得上才算同一個程序）
type launchProcRef struct {
	Pid     int   `json:"pid"`
	Created int64 `json:"created"`
}

type launchRunRecord struct {
	TargetID     string          `json:"targetId"`
	Kind         string          `json:"kind"`
	Name         string          `json:"name"`
	ProjectPath  string          `json:"projectPath"`
	RelativePath string          `json:"relativePath"`
	Detail       string          `json:"detail"`
	Framework    string          `json:"framework,omitempty"`
	Script       string          `json:"script,omitempty"`
	SinceMs      int64           `json:"sinceMs"`
	Paused       bool            `json:"paused"`
	LogFile      string          `json:"logFile,omitempty"`
	Processes    []launchProcRef `json:"processes"`
}

type launchRunsFile struct {
	Runs []launchRunRecord `json:"runs"`
}

// launchLogFileName：這次執行的輸出檔名（目標的鍵＋開始時間，每次不同）
func launchLogFileName(targetID string, now time.Time) string {
	u := now.UTC()
	return fmt.Sprintf("%s-%s%03d.log", targetID, u.Format("20060102150405"), u.Nanosecond()/1e6)
}

var launchLogFilePattern = regexp.MustCompile(`^t[0-9a-f]{12}-\d{17}\.log$`)

// launchIsLogFileName：是 launchLogFileName 產生的檔名（清理只刪這種、狀態檔裡的檔名不是這種就不用：不會指到狀態資料夾以外）
func launchIsLogFileName(name string) bool { return launchLogFilePattern.MatchString(name) }

func launchToRecord(t launchTarget, since *time.Time, paused bool, logFile string, procs []launchProcRef) launchRunRecord {
	r := launchRunRecord{TargetID: t.ID, Kind: launchKindName(t.Kind), Name: t.Name, ProjectPath: t.ProjectPath, RelativePath: t.RelativePath,
		Detail: t.Detail, Framework: t.Framework, Script: t.Script, Paused: paused, LogFile: logFile, Processes: append([]launchProcRef{}, procs...)}
	if since != nil {
		r.SinceMs = since.UnixMilli()
	}
	return r
}

// launchTargetFromRecord：狀態檔的一筆還原成目標；不完整、種類不認得、鍵和專案檔對不上回 nil
func launchTargetFromRecord(r launchRunRecord) *launchTarget {
	if r.ProjectPath == "" || !filepath.IsAbs(r.ProjectPath) {
		return nil
	}
	id := launchTargetID(r.ProjectPath)
	if r.TargetID != id {
		return nil
	}
	var kind launchKind
	switch r.Kind {
	case "dotnet":
		kind = launchDotNet
	case "electron":
		kind = launchElectron
	default:
		return nil
	}
	name := r.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(r.ProjectPath), filepath.Ext(r.ProjectPath))
	}
	rel := r.RelativePath
	if rel == "" {
		rel = filepath.Base(r.ProjectPath)
	}
	return &launchTarget{ID: id, Kind: kind, Name: name, ProjectPath: r.ProjectPath, RelativePath: rel, Detail: r.Detail, Framework: r.Framework, Script: r.Script}
}

func launchSinceFromRecord(r launchRunRecord) *time.Time {
	if r.SinceMs <= 0 {
		return nil
	}
	t := time.UnixMilli(r.SinceMs)
	return &t
}
