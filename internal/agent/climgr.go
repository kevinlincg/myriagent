package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CLI 管理（照 C# 的 Cli/CliManager.cs、CliVersionRules.cs、CliUpdateRules.cs 與 Myriagent.Mac 的 MacCliLocator.cs）：
// 找執行檔、查版本、向官方查最新版（npm registry、agy 的 manifest、grok 的 stable 頻道）、安裝與更新。
// 自動更新（設定 autoUpdateCli）預設關；開了才在查到新版時更新，有這種 CLI 的對話在跑就延後。
// 尚未移植：熱更新的全體屏障（npm 換檔前讓執行中的對話停在安全點、換版後接續）——手動更新 npm 版時有對話在執行就請使用者等它們結束。

const (
	claudeNpmPackage  = "@anthropic-ai/claude-code"
	codexNpmPackage   = "@openai/codex"
	grokStableURL     = "https://x.ai/cli/stable"
	agyManifestURL    = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/windows_amd64.json"
	cliLatestInterval = 6 * time.Hour
	cliFirstCheck     = time.Minute
)

// cliState：一種 CLI 的管理狀態（cliInfo.cli；都在 h.mu 底下改）
type cliState struct {
	checking      bool   // 找到執行檔但還沒查過版本
	installKind   string // npm／native／other（""＝不知道）
	latest        string
	latestError   string
	lastCheckedAt int64
	checkedAt     int64
	busy          bool
	progress      string
	actionError   string
	versionError  string // 執行檔跑不起來的原因
	lastChange    *CliChangeView
}

// ---- 版本 ----

var versionPattern = regexp.MustCompile(`(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)`)

// parseCLIVersion：從 --version 輸出取版本號（第一個 x.y.z，可帶 -prerelease）；沒有回空字串
func parseCLIVersion(output string) string {
	if m := versionPattern.FindStringSubmatch(output); m != nil {
		return m[1]
	}
	return ""
}

func splitVersion(v string) ([]int, string, bool) {
	v = parseCLIVersion(v)
	if v == "" {
		return nil, "", false
	}
	core, pre := v, ""
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core, pre = v[:i], v[i+1:]
	}
	var nums []int
	for _, p := range strings.Split(core, ".") {
		n, _ := strconv.Atoi(p)
		nums = append(nums, n)
	}
	return nums, pre, true
}

// compareVersions：a<b 回負數；無法解析的視為最小；正式版大於同版號的預發布版
func compareVersions(a, b string) int {
	na, pa, oka := splitVersion(a)
	nb, pb, okb := splitVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < len(na) || i < len(nb); i++ {
		x, y := 0, 0
		if i < len(na) {
			x = na[i]
		}
		if i < len(nb) {
			y = nb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa == "" && pb != "":
		return 1
	case pa != "" && pb == "":
		return -1
	}
	return strings.Compare(pa, pb)
}

// isNewerVersion：latest 比 current 新（任一無法解析回 false）
func isNewerVersion(latest, current string) bool {
	return parseCLIVersion(latest) != "" && parseCLIVersion(current) != "" && compareVersions(latest, current) > 0
}

// parseLatestJSON：npm registry 的 /{套件}/latest、agy 的 manifest：取 version 欄位
func parseLatestJSON(text string) string {
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal([]byte(text), &m) != nil {
		return ""
	}
	return parseCLIVersion(m.Version)
}

// latestSource：這種 CLI 的最新版從哪裡查、怎麼解讀
func latestSource(p string) (string, func(string) string) {
	switch p {
	case "Claude":
		return "https://registry.npmjs.org/" + claudeNpmPackage + "/latest", parseLatestJSON
	case "Codex":
		return "https://registry.npmjs.org/" + codexNpmPackage + "/latest", parseLatestJSON
	case "Antigravity":
		return agyManifestURL, parseLatestJSON
	}
	return grokStableURL, parseCLIVersion
}

func npmPackageOf(p string) string {
	switch p {
	case "Claude":
		return claudeNpmPackage
	case "Codex":
		return codexNpmPackage
	}
	return ""
}

// installKindOf：安裝方式看執行檔實際指向哪裡：npm 全域安裝（…/node_modules/〈套件〉/…）、Claude 官方安裝（~/.local/bin 或
// ~/.local/share/claude/versions）、其他（Homebrew、各家的安裝程式）
func installKindOf(p, path, real, home string) string {
	if pkg := npmPackageOf(p); pkg != "" && strings.Contains(real, "/node_modules/"+pkg+"/") {
		return "npm"
	}
	if p == "Claude" && (filepath.Dir(path) == filepath.Join(home, ".local", "bin") ||
		strings.HasPrefix(real, filepath.Join(home, ".local", "share", "claude", "versions")+"/")) {
		return "native"
	}
	return "other"
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// probeVersion：跑 --version（最多 15 秒）；回版本與錯誤（跑不起來的原因）
func probeVersion(path string) (string, string) {
	res := runShort(context.Background(), path, []string{"--version"}, commandEnv(nil), userHome(), 15*time.Second)
	if v := parseCLIVersion(res.stdout); v != "" {
		return v, ""
	}
	if v := parseCLIVersion(res.stderr); v != "" && res.exitCode == 0 {
		return v, ""
	}
	switch {
	case res.timedOut:
		return "", "--version 執行逾時"
	case strings.TrimSpace(res.stderr) != "":
		return "", truncateEllipsis(strings.TrimSpace(strings.SplitN(res.stderr, "\n", 2)[0]), 200)
	}
	return "", "讀不到版本"
}

// detectCLIs：啟動時找四種 CLI 與它們的版本（平行跑 --version）
func (h *Hub) detectCLIs() {
	type found struct {
		p, path, version, verr, kind string
	}
	results := make([]found, len(providerKeys))
	var wg sync.WaitGroup
	home := userHome()
	for i, p := range providerKeys {
		path := findExecutable(providerExecutables[p])
		results[i] = found{p: p, path: path}
		if path == "" {
			continue
		}
		wg.Add(1)
		go func(i int, p, path string) {
			defer wg.Done()
			v, e := probeVersion(path)
			results[i].version, results[i].verr, results[i].kind = v, e, installKindOf(p, path, realPath(path), home)
		}(i, p, path)
	}
	wg.Wait()
	for _, r := range results {
		info := h.clis[r.p]
		if info == nil {
			info = &cliInfo{}
			h.clis[r.p] = info
		}
		info.path, info.version = r.path, r.version
		info.cli.installKind, info.cli.versionError = r.kind, r.verr
	}
}

// redetectCLI：重新找一種 CLI 與版本（不持有 h.mu）；版本變了記下換版（origin：app 自己更新的是 app，其他是 external）
func (h *Hub) redetectCLI(p, origin string) {
	path := h.locateCLI(p)
	version, verr, kind := "", "", ""
	if path != "" {
		version, verr = probeVersion(path)
		kind = installKindOf(p, path, realPath(path), userHome())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	info := h.clis[p]
	if info == nil {
		info = &cliInfo{}
		h.clis[p] = info
	}
	before := info.version
	info.path, info.version = path, version
	info.cli.installKind, info.cli.versionError, info.cli.checking = kind, verr, false
	if version != "" && before != version {
		ch := &CliChangeView{To: version, Origin: origin, At: nowMs()}
		if before != "" {
			b := before
			ch.From = &b
		}
		if before != "" || origin == "app" {
			info.cli.lastChange = ch
		}
		// 閒置的對話關掉程序：下一則訊息以新版重開（執行中的照舊跑完）
		for _, c := range h.convs {
			if c.Meta.Provider == p && !c.running {
				h.closeSession(c)
			}
		}
	}
	for _, a := range h.accounts {
		if a.Provider == p && (a.status == "notInstalled") != (path == "") {
			h.refreshAccount(a)
			h.requestRefresh(a, true, reasonManual, false)
		}
	}
}

// locateCLI：找這種 CLI；測試以 h.ar().cliPaths 指定（有設定就只看它，不會找到這台真的 CLI）。不持有 h.mu
func (h *Hub) locateCLI(p string) string {
	h.mu.Lock()
	paths := h.ar().cliPaths
	h.mu.Unlock()
	if paths != nil {
		return paths[p]
	}
	return findExecutable(providerExecutables[p])
}

// ---- 畫面 ----

func (h *Hub) cliViews() map[string]CliView {
	out := map[string]CliView{}
	auto := h.settingBool("autoUpdateCli")
	policy, _ := h.setting("cliRestartPolicy").(string)
	for _, p := range providerKeys {
		c := h.clis[p]
		v := CliView{Provider: p, Status: "notInstalled", AutoUpdate: auto, RestartPolicy: policy}
		if c == nil {
			out[p] = v
			continue
		}
		s := c.cli
		if c.path != "" {
			path := c.path
			v.Path = &path
			if c.version != "" {
				ver := c.version
				v.Installed, v.Version, v.Status = true, &ver, "ready"
			} else {
				v.Checking = s.checking
				v.Status = "error"
			}
			if s.installKind != "" {
				k := s.installKind
				v.InstallKind = &k
			}
		}
		if s.latest != "" {
			l := s.latest
			v.Latest = &l
		}
		v.UpdateAvailable = v.Installed && isNewerVersion(s.latest, c.version)
		v.LatestError = strPtr(s.latestError)
		v.Busy = s.busy
		v.Progress = strPtr(s.progress)
		v.Changing = s.busy && s.progress != "" && strings.Contains(s.progress, "安裝")
		v.Error = strPtr(firstNonBlank(s.actionError, s.versionError))
		if s.lastCheckedAt > 0 {
			t := s.lastCheckedAt
			v.LastCheckedAt = &t
		}
		if s.checkedAt > 0 {
			t := s.checkedAt
			v.CheckedAt = &t
		}
		v.LastChange = s.lastChange
		out[p] = v
	}
	return out
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// pushCLI：CLI 狀態改變時送給所有視窗（持有 h.mu）
func (h *Hub) pushCLI() {
	h.broadcast(map[string]any{"type": "cli", "cli": h.cliViews(), "devicesCli": nil})
}

// ---- 最新版與自動更新 ----

// fetchLatest：向官方查最新版；失敗回（"", 給人看的原因）
func (r *acctRuntime) fetchLatest(p string) (string, string) {
	if r.fetchText == nil {
		return "", "無法查詢最新版本：這一版沒有連網"
	}
	url, parse := latestSource(p)
	text, err := r.fetchText(url)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", "無法查詢最新版本：連線逾時"
		}
		return "", "無法查詢最新版本：" + err.Error()
	}
	if v := parse(text); v != "" {
		return v, ""
	}
	return "", "無法查詢最新版本：回應看不懂"
}

func httpFetchText(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

// checkCLI：重新找安裝、查版本、向官方查最新版；開了自動更新、有新版就更新（不持有 h.mu）
func (h *Hub) checkCLI(p string, allowAuto bool) {
	h.redetectCLI(p, "external")
	h.mu.Lock()
	r := h.ar()
	h.mu.Unlock()
	latest, latestErr := r.fetchLatest(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	info := h.clis[p]
	if info == nil {
		return
	}
	s := &info.cli
	now := nowMs()
	if latest != "" {
		s.latest, s.lastCheckedAt = latest, now
	}
	s.latestError, s.checkedAt = latestErr, now
	// 已是最新版、或版本變了，上次的失敗原因就不再顯示
	if info.version != "" && !isNewerVersion(s.latest, info.version) {
		s.actionError = ""
	}
	h.pushCLI()
	if allowAuto && h.settingBool("autoUpdateCli") && info.path != "" && isNewerVersion(s.latest, info.version) && !s.busy {
		h.startCLIUpdate(nil, p, true)
	}
}

// checkAllCLIs：立即檢查每種 CLI（cliCheck；定期的 6 小時檢查也是）
func (h *Hub) checkAllCLIs(allowAuto bool) {
	var wg sync.WaitGroup
	for _, p := range providerKeys {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			h.checkCLI(p, allowAuto)
		}(p)
	}
	wg.Wait()
}

// ---- 安裝與更新 ----

// runningSessions：這種 CLI 執行中的對話數與活著的程序數（持有 h.mu）
func (h *Hub) runningSessions(p string) (running, alive int) {
	for _, c := range h.convs {
		if c.Meta.Provider != p {
			continue
		}
		if c.running {
			running++
		}
		if h.alive(c) {
			alive++
		}
	}
	return
}

// startCLIUpdate：更新到最新版（npm 版跑 npm install -g、其他跑 CLI 自己的 update）。automatic＝背景自動更新：有對話在跑就安靜地延後。
// 沒安裝就改成安裝。呼叫時持有 h.mu；v 是送指令的視窗（自動更新是 nil）
func (h *Hub) startCLIUpdate(v *View, p string, automatic bool) {
	info := h.clis[p]
	if info == nil || info.path == "" {
		h.startCLIInstall(v, p)
		return
	}
	s := &info.cli
	if s.busy {
		return
	}
	running, _ := h.runningSessions(p)
	route := "self"
	if s.installKind == "npm" {
		route = "npm"
	}
	if running > 0 && (automatic || route == "npm") {
		if !automatic {
			// npm 換檔會刪掉執行中的檔案：等對話結束（熱更新的屏障尚未移植）
			s.actionError = fmt.Sprintf("有 %d 個 %s 的對話正在執行，請等它們結束後再更新", running, providerNames[p])
			h.pushCLI()
		}
		return
	}
	npm := ""
	if route == "npm" {
		if npm = h.ar().npmPath(); npm == "" {
			s.actionError = "找不到 npm，無法更新 npm 安裝的 CLI"
			h.pushCLI()
			return
		}
		// npm 換檔前關掉這種 CLI 閒置的程序（它們開著舊檔）
		for _, c := range h.convs {
			if c.Meta.Provider == p && !c.running {
				h.closeSession(c)
			}
		}
	}
	s.busy, s.progress, s.actionError = true, "更新中…", ""
	h.pushCLI()
	path, before := info.path, info.version
	go func() {
		var res cliResult
		var errText, note string
		if route == "npm" {
			res = runShort(context.Background(), npm, []string{"install", "-g", npmPackageOf(p) + "@latest"}, commandEnv(nil), userHome(), 30*time.Minute)
			if res.exitCode != 0 {
				errText = "npm 更新失敗：" + describeCLIFailure(res.stdout, res.stderr)
			}
		} else {
			res = runShort(context.Background(), path, []string{"update"}, commandEnv(nil), userHome(), 30*time.Minute)
		}
		after, _ := probeVersion(path)
		if route != "npm" {
			var parsed claudeUpdateResult
			if p == "Claude" {
				parsed = parseClaudeUpdate(res.exitCode, res.stdout, res.stderr)
			} else if res.exitCode == 0 {
				parsed = claudeUpdateResult{kind: updateDeclined, message: summarizeCLIOutput(res.stdout, res.stderr)}
			} else {
				parsed = claudeUpdateResult{kind: updateFailed, message: describeCLIFailure(res.stdout, res.stderr)}
			}
			j := judgeSelfUpdate(parsed, before, after)
			errText, note = j.err, j.note
		}
		h.redetectCLI(p, "app")
		h.mu.Lock()
		defer h.mu.Unlock()
		defer h.kick()
		s := &h.clis[p].cli
		s.busy, s.progress = false, ""
		switch {
		case errText != "":
			s.actionError = errText
			log.Printf("%s CLI %s更新失敗：%s", p, map[bool]string{true: "自動", false: ""}[automatic], errText)
		case h.clis[p].version == before && isNewerVersion(s.latest, before):
			s.actionError = firstNonBlank(note, "更新指令已結束，但版本沒有變")
		}
		h.pushCLI()
	}()
}

// installScript：官方安裝指令（在終端機視窗跑，看得到進度與錯誤）
func installScript(p string) string {
	switch p {
	case "Claude":
		return "curl -fsSL https://claude.ai/install.sh | bash"
	case "Antigravity":
		return "curl -fsSL https://antigravity.google/cli/install.sh | bash"
	case "Grok":
		return "curl -fsSL https://x.ai/cli/install.sh | bash"
	}
	return "npm install -g " + codexNpmPackage
}

// startCLIInstall：安裝 CLI。Claude、Antigravity、Grok 開終端機跑官方安裝指令（等視窗關閉）；Codex 有 npm 就 npm install -g，
// 沒有就請使用者先裝 Node.js。裝好後重新找安裝與版本。呼叫時持有 h.mu
func (h *Hub) startCLIInstall(v *View, p string) {
	info := h.clis[p]
	if info == nil {
		info = &cliInfo{}
		h.clis[p] = info
	}
	s := &info.cli
	if s.busy {
		return
	}
	r := h.ar()
	if p == "Codex" {
		npm := r.npmPath()
		if npm == "" {
			s.actionError = "找不到 npm：請先安裝 Node.js（或在終端機執行 brew install codex），再按重新檢查"
			h.pushCLI()
			return
		}
		s.busy, s.progress, s.actionError = true, "npm 正在安裝…", ""
		h.pushCLI()
		go func() {
			res := runShort(context.Background(), npm, []string{"install", "-g", codexNpmPackage + "@latest"}, commandEnv(nil), userHome(), 30*time.Minute)
			errText := ""
			if res.exitCode != 0 {
				errText = "npm 安裝失敗：" + describeCLIFailure(res.stdout, res.stderr)
			}
			h.finishInstall(p, errText)
		}()
		return
	}
	script := installScript(p)
	proc, _, err := r.console.Start("/bin/sh", []string{"-c", script}, commandEnv(nil), providerNames[p]+" 安裝")
	if errors.Is(err, errNoTerminal) {
		s.actionError = "找不到可以開啟的終端機，請在終端機執行：" + script + "，完成後按重新檢查"
		h.pushCLI()
		return
	}
	if err != nil {
		s.actionError = "無法開啟安裝視窗：" + err.Error()
		h.pushCLI()
		return
	}
	s.busy, s.progress, s.actionError = true, "已開啟安裝視窗，完成後關閉視窗即可", ""
	h.pushCLI()
	go func() {
		code, _ := proc.Wait(context.Background())
		errText := ""
		if code != 0 {
			errText = fmt.Sprintf("%s 安裝失敗（結束碼 %d），原因請看安裝視窗裡的訊息", providerNames[p], code)
		}
		h.finishInstall(p, errText)
	}()
}

func (h *Hub) finishInstall(p, errText string) {
	h.redetectCLI(p, "app")
	h.mu.Lock()
	r := h.ar()
	h.mu.Unlock()
	latest, latestErr := r.fetchLatest(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	info := h.clis[p]
	s := &info.cli
	s.busy, s.progress = false, ""
	if latest != "" {
		s.latest, s.lastCheckedAt = latest, nowMs()
	}
	s.latestError, s.checkedAt = latestErr, nowMs()
	switch {
	case errText != "":
		s.actionError = errText
	case info.path == "":
		// 安裝指令沒報錯卻還是找不到執行檔（例如 npm 的全域資料夾不在 PATH 上）：也要告訴使用者
		s.actionError = providerNames[p] + " 安裝指令已結束，但找不到 " + providerExecutables[p] + "（安裝位置可能不在 PATH 上）"
	}
	h.pushCLI()
}

// npmPath：npm 的位置（測試預設沒有：不會碰到真的 npm）
func (r *acctRuntime) npmPath() string {
	if r.findNpm == nil {
		return ""
	}
	return r.findNpm()
}

// ---- claude update 的判讀 ----

const (
	updateUpdated  = "updated"
	updateUpToDate = "upToDate"
	updateLockBusy = "lockBusy" // 版本鎖被別的程序占住：結束碼 0 但沒有更新
	updateDeclined = "declined" // 結束碼 0、沒換版的其他說明（由套件管理員管理、停用更新…）
	updateFailed   = "failed"
)

type claudeUpdateResult struct {
	kind     string
	from, to string
	pid      int
	message  string
}

var (
	ansiPattern     = regexp.MustCompile(`\x1B\[[0-9;]*[A-Za-z]`)
	lockBusyPattern = regexp.MustCompile(`(?i)Another Claude process(?: \(PID (\d+)\))? is currently running`)
	updatedPattern  = regexp.MustCompile(`(?i)Successfully (?:updated|downgraded) from (\S+) to version (\S+)`)
	upToDatePattern = regexp.MustCompile(`(?i)Claude(?: Code)? is up to date(?: \(([^)]+)\))?`)
	errorWord       = regexp.MustCompile(`(?i)\b(error|err|failed|fatal)\b`)
	npmDetail       = regexp.MustCompile(`(?i)^npm (error|ERR!) (code|errno|syscall|path|dest|A complete log)\b`)
	npmWarn         = regexp.MustCompile(`(?i)^npm warn`)
	progressLine    = regexp.MustCompile(`(?i)^(Current version|Checking for updates)`)
)

func outputLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(ansiPattern.ReplaceAllString(s, ""), "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func firstMatchingLine(text string, re *regexp.Regexp) string {
	for _, l := range outputLines(text) {
		if re.MatchString(l) {
			return l
		}
	}
	return ""
}

// parseClaudeUpdate：claude update 的輸出（stdout 與 stderr 一起看；色碼先去掉）。順序：版本鎖被占住 → 結束碼不是 0＝失敗 →
// 「Successfully updated／downgraded from X to version Y」→「is up to date」→ 其他＝沒有換版的說明
func parseClaudeUpdate(exitCode int, stdout, stderr string) claudeUpdateResult {
	text := ansiPattern.ReplaceAllString(stdout+"\n"+stderr, "")
	if m := lockBusyPattern.FindStringSubmatch(text); m != nil {
		pid, _ := strconv.Atoi(m[1])
		return claudeUpdateResult{kind: updateLockBusy, pid: pid, message: firstMatchingLine(text, lockBusyPattern)}
	}
	if exitCode != 0 {
		return claudeUpdateResult{kind: updateFailed, message: describeCLIFailure(stdout, stderr)}
	}
	if m := updatedPattern.FindStringSubmatch(text); m != nil {
		return claudeUpdateResult{kind: updateUpdated, from: parseCLIVersion(m[1]), to: parseCLIVersion(m[2]), message: firstMatchingLine(text, updatedPattern)}
	}
	if m := upToDatePattern.FindStringSubmatch(text); m != nil {
		v := parseCLIVersion(m[1])
		return claudeUpdateResult{kind: updateUpToDate, from: v, to: v, message: firstMatchingLine(text, upToDatePattern)}
	}
	return claudeUpdateResult{kind: updateDeclined, message: summarizeCLIOutput(stdout, stderr)}
}

type updateJudgement struct {
	switched   bool
	err, note  string
	retryLater bool
}

// coreVersion：版本的 x.y.z 部分
func coreVersion(v string) string {
	v = parseCLIVersion(v)
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i]
	}
	return v
}

// judgeSelfUpdate：一律以執行前後的版本為準——回報成功卻沒換檔算失敗；版本鎖被占住不算成功、30 分鐘後再試；失敗但版本其實換了就算換版
func judgeSelfUpdate(r claudeUpdateResult, before, after string) updateJudgement {
	switched := after != "" && (before == "" || coreVersion(before) != coreVersion(after))
	switch r.kind {
	case updateLockBusy:
		if switched {
			return updateJudgement{switched: true}
		}
		holder := ""
		if r.pid > 0 {
			holder = fmt.Sprintf("（PID %d）", r.pid)
		}
		return updateJudgement{note: "另一個 Claude 程序" + holder + "正在更新 Claude Code，30 分鐘後再試", retryLater: true}
	case updateFailed:
		if switched {
			return updateJudgement{switched: true}
		}
		return updateJudgement{err: "更新失敗：" + orDefault(r.message, "原因不明")}
	case updateUpdated:
		if !switched {
			return updateJudgement{err: "claude update 回報已更新到 " + orDefault(r.to, "新版") + "，但執行檔仍是 " + firstNonBlank(after, before, "原本的版本")}
		}
		return updateJudgement{switched: true}
	}
	if switched {
		return updateJudgement{switched: true}
	}
	return updateJudgement{note: r.message}
}

// describeCLIFailure：安裝／更新失敗時給使用者看的原因：先找 stderr、再找 stdout 裡含 error／failed 的行（npm 的細節行與 warn 行跳過）
func describeCLIFailure(stdout, stderr string) string {
	var cands []string
	for _, l := range append(outputLines(stderr), outputLines(stdout)...) {
		if !npmWarn.MatchString(l) {
			cands = append(cands, l)
		}
	}
	for _, l := range cands {
		if errorWord.MatchString(l) && !npmDetail.MatchString(l) {
			return l
		}
	}
	for _, l := range cands {
		if errorWord.MatchString(l) {
			return l
		}
	}
	if l := outputLines(stderr); len(l) > 0 {
		return l[len(l)-1]
	}
	if l := outputLines(stdout); len(l) > 0 {
		return l[len(l)-1]
	}
	return "原因不明"
}

// summarizeCLIOutput：更新指令成功結束、版本卻沒變時，CLI 自己的說明（去掉進度行後合併成一行，最多 300 字）
func summarizeCLIOutput(stdout, stderr string) string {
	var lines []string
	for _, l := range append(outputLines(stdout), outputLines(stderr)...) {
		if !progressLine.MatchString(l) {
			lines = append(lines, l)
		}
	}
	return truncateEllipsis(strings.Join(lines, " "), 300)
}

// ---- 指令與排程 ----

func init() {
	registerInitField("cli", func(h *Hub, v *View) any { return h.cliViews() })
	registerCommand("cliCheck", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		go h.checkAllCLIs(true)
	})
	registerCommand("cliInstall", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		if p := str(msg, "provider"); providerNames[p] != "" {
			h.startCLIInstall(v, p)
		}
	})
	registerCommand("cliUpdate", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		if p := str(msg, "provider"); providerNames[p] != "" {
			h.startCLIUpdate(v, p, false)
		}
	})
	registerStartHook(func(h *Hub) {
		h.mu.Lock()
		r := h.ar()
		r.fetchText = httpFetchText
		r.findNpm = func() string { return findExecutable("npm") }
		h.mu.Unlock()
		time.Sleep(cliFirstCheck)
		for {
			h.checkAllCLIs(true)
			time.Sleep(cliLatestInterval)
		}
	})
	_ = os.Getenv
}
