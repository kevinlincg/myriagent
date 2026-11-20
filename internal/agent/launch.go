package agent

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 執行列（web-protocol 的 launchStatus／launchAction／launchSelect／launchLog／launchRescan；照 C# 的 Hub/AgentHub.Launch.cs）：
// 對話的工作資料夾是可以執行的視窗程式專案（launchrules.go）時，網頁在輸入區上方顯示啟動（建置並執行）、暫停、停止、重新建置並執行。
// 偵測以資料夾為單位快取（同一個資料夾的對話共用），開分頁、換資料夾、這一輪結束（代理可能剛建立專案）、畫面上看得到的每分鐘重新偵測；
// 執行狀態以目標（專案檔）為單位：不同資料夾偵測到同一個專案時是同一份。
// 建置是 agent 的子程序（agent 結束時結束它）。執行分兩種模式（每個專案各自記得）：Debug 模式的程式也是 agent 的子程序，agent 結束時一起結束；
// Release 模式的程式和 agent 脫鉤——開著的記在 launch/runs.json（PID＋開始時間、輸出檔），下次啟動接回，停止與重新建置並執行時先結束它。
// 每個資料夾選的專案與每個專案的模式記在 launch/prefs.json（使用者明確選過的才記）。
// 沒掛（attachLaunch；測試預設）時什麼都不做、也不送 launchStatus。狀態都在 h.mu 底下改，輸出的緩衝自己加鎖。

type launchHub struct {
	scanner  launchScanner
	starter  launchStarter
	stateDir string
	folders  map[string]*launchFolderState
	runs     map[string]*launchRun
	prefs    launchPrefsFile
	// written：上次寫進 runs.json 的內容（沒變就不重寫；nil＝檔案不存在）
	written *string
	ctx     context.Context
	cancel  context.CancelFunc
	clock   func() time.Time
	closed  bool
}

type launchFolderState struct {
	targets   []launchTarget
	known     bool
	scannedAt *time.Time
	scanning  bool
	again     bool
	selected  string
}

type launchRun struct {
	target           launchTarget
	phase            launchPhase
	since            *time.Time
	last             *launchEnd
	log              *launchLog
	process          launchProcess
	mode             launchMode
	stopRequested    bool
	restartRequested bool
	outputPending    bool
	detached         bool
	logFile          string
}

func init() {
	registerStartHook(func(h *Hub) {
		h.mu.Lock()
		h.attachLaunch(fsLaunchScanner{}, unixLaunchStarter{}, filepath.Join(h.root, "launch"))
		h.mu.Unlock()
		launchMaintenance(h)
	})
	registerConvEvent(func(h *Hub, kind convEventKind, v *View, c *Conv) {
		if h.launch == nil {
			return
		}
		switch kind {
		case convOpened:
			// 先送快取的，太舊就重新偵測
			h.postLaunch(c, v)
			h.requestLaunchScan(c, launchScanOpen)
		case convFolderChanged:
			// 先送新資料夾快取的（沒有就是 null，藏起執行列），再偵測
			h.postLaunch(c, nil)
			h.requestLaunchScan(c, launchScanUser)
		case convTurnEnded:
			// 代理可能剛建立或改了專案
			if h.convOpenAnywhere(c.Meta.ID) {
				h.requestLaunchScan(c, launchScanChanged)
			}
		}
	})
	registerShutdownHook(func(h *Hub) { h.stopLaunchAll() })
	for _, cmd := range []string{"launchAction", "launchSelect", "launchLog", "launchRescan"} {
		name := cmd
		registerCommand(name, func(h *Hub, v *View, c *Conv, msg map[string]any) { h.handleLaunchCommand(v, name, c, msg) })
	}
}

// attachLaunch：掛上專案偵測與程序（只掛一次）。stateDir 放 runs.json、prefs.json 與輸出檔；掛上時讀回記得的選擇、接回上次開著、還在執行的程式
func (h *Hub) attachLaunch(scanner launchScanner, starter launchStarter, stateDir string) {
	ctx, cancel := context.WithCancel(context.Background())
	h.launch = &launchHub{scanner: scanner, starter: starter, stateDir: stateDir, folders: map[string]*launchFolderState{}, runs: map[string]*launchRun{},
		ctx: ctx, cancel: cancel, clock: time.Now}
	h.loadLaunchPrefs()
	h.restoreLaunchRuns()
}

// launchMaintenance：維護計時器（每 30 秒）：畫面上看得到的對話重新偵測、補記 runs.json
func launchMaintenance(h *Hub) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		h.mu.Lock()
		lh := h.launch
		if lh == nil || lh.closed {
			h.mu.Unlock()
			return
		}
		h.tickLaunch()
		h.mu.Unlock()
		h.kick()
	}
}

func (h *Hub) tickLaunch() {
	for _, c := range h.convs {
		if h.convVisibleAnywhere(c.Meta.ID) {
			h.requestLaunchScan(c, launchScanPeriodic)
		}
	}
	h.saveLaunchRuns()
}

func (h *Hub) convOpenAnywhere(id string) bool {
	for v := range h.views {
		if v.open[id] {
			return true
		}
	}
	return false
}

func (h *Hub) convVisibleAnywhere(id string) bool {
	for v := range h.views {
		if v.open[id] && v.visible[id] {
			return true
		}
	}
	return false
}

// launchBuilding：建置中（或停止中）：agent 結束會中斷它
func (h *Hub) launchBuilding() bool {
	if h.launch == nil {
		return false
	}
	for _, r := range h.launch.runs {
		if launchBlocksAppRestart(r.phase) {
			return true
		}
	}
	return false
}

// launchDebugging：有 Debug 模式執行中（或暫停中）的程式（agent 重新啟動會一起結束它）
func (h *Hub) launchDebugging() bool {
	if h.launch == nil {
		return false
	}
	for _, r := range h.launch.runs {
		if launchDiesWithApp(r.phase, r.mode) {
			return true
		}
	}
	return false
}

// launchFolderOf：這個對話要偵測的資料夾（快取的鍵）
func (h *Hub) launchFolderOf(c *Conv) string {
	if h.launch == nil {
		return ""
	}
	return folderKey(c.workDir(h))
}

// folderKey：快取的鍵：完整路徑、去掉結尾的分隔符號（根目錄是「/」）；不是完整路徑回空字串
func folderKey(folder string) string {
	if folder == "" || !filepath.IsAbs(folder) {
		return ""
	}
	return filepath.Clean(folder)
}

// launchTargetsOf：偵測到的，加上執行中、但這次沒偵測到的（專案檔暫時讀不到、被改壞）——還開著的程式要停得掉
func (h *Hub) launchTargetsOf(key string, e *launchFolderState) []launchTarget {
	var extra []launchTarget
	for _, r := range h.launch.runs {
		if r.phase == launchIdle || !launchIsInside(r.target.ProjectPath, key) {
			continue
		}
		found := false
		for _, t := range e.targets {
			if t.ID == r.target.ID {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, r.target)
		}
	}
	if len(extra) == 0 {
		return e.targets
	}
	return launchOrder(append(append([]launchTarget{}, e.targets...), extra...))
}

func (h *Hub) launchSnapshotOf(id string) *launchSnapshot {
	r := h.launch.runs[id]
	if r == nil {
		return nil
	}
	epoch, seq := r.log.state()
	s := &launchSnapshot{Phase: r.phase, Since: r.since, Last: r.last, LogSeq: seq, LogEpoch: epoch}
	if r.phase != launchIdle {
		m := r.mode
		s.RunMode = &m
	}
	return s
}

func (h *Hub) launchModeOf(id string) launchMode { return launchModeFromPrefs(&h.launch.prefs, id) }

// postLaunch：送這個對話的執行列（還沒偵測完、沒有目標時 launch 是 null）；only 是 nil＝開著它的所有 view
func (h *Hub) postLaunch(c *Conv, only *View) {
	if h.launch == nil {
		return
	}
	key := h.launchFolderOf(c)
	var launch any
	if e := h.launch.folders[key]; key != "" && e != nil && e.known {
		if r := renderLaunch(key, h.launchTargetsOf(key, e), e.selected, h.launchSnapshotOf, h.launchModeOf); r != nil {
			launch = r
		}
	}
	msg := map[string]any{"type": "launchStatus", "conversationId": c.Meta.ID, "launch": launch}
	if only != nil {
		h.sendTo(only, msg)
	} else {
		h.sendToOpen(c.Meta.ID, msg)
	}
}

func (h *Hub) requestLaunchScan(c *Conv, trigger launchScanTrigger) {
	lh := h.launch
	if lh == nil || lh.closed {
		return
	}
	key := h.launchFolderOf(c)
	if key == "" {
		return
	}
	e := lh.folders[key]
	if e == nil {
		e = &launchFolderState{selected: launchSelectedFromPrefs(&lh.prefs, key)}
		lh.folders[key] = e
	}
	if e.scanning {
		if trigger == launchScanChanged || trigger == launchScanUser {
			e.again = true
		}
		return
	}
	if launchShouldScan(trigger, e.scannedAt, lh.clock()) {
		e.scanning = true
		go h.scanLaunch(key, e)
	}
}

// scanLaunch：在 goroutine 偵測（不持有 h.mu），結果回到中樞
func (h *Hub) scanLaunch(key string, e *launchFolderState) {
	h.mu.Lock()
	lh := h.launch
	h.mu.Unlock()
	for {
		found, err := lh.scanner.Scan(lh.ctx, key)
		h.mu.Lock()
		if lh.closed {
			e.scanning = false
			h.mu.Unlock()
			return
		}
		if err != nil {
			log.Printf("偵測可以執行的專案失敗（%s）：%v", key, err)
		}
		now := lh.clock()
		e.scannedAt = &now
		// 失敗時留著上次的；第一次就失敗當成沒有目標（藏起執行列）
		targets := e.targets
		if err == nil {
			targets = found
		}
		changed := !e.known || !launchTargetsEqual(e.targets, targets)
		e.targets = targets
		e.known = true
		selected := launchSelect(h.launchTargetsOf(key, e), e.selected)
		if selected != e.selected {
			changed = true
		}
		e.selected = selected
		// 閒置的目標換成最新偵測到的內容（多目標的架構、npm 指令碼可能改了）
		for _, t := range targets {
			if r := lh.runs[t.ID]; r != nil && r.phase == launchIdle {
				r.target = t
			}
		}
		if changed {
			h.postLaunchFolder(key)
		}
		again := e.again
		e.again = false
		if !again {
			e.scanning = false
		}
		h.mu.Unlock()
		h.kick()
		if !again {
			return
		}
	}
}

func launchTargetsEqual(a, b []launchTarget) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// postLaunchFolder：送給開著工作資料夾是這個的對話的 view
func (h *Hub) postLaunchFolder(key string) {
	for _, c := range h.convs {
		if h.convOpenAnywhere(c.Meta.ID) && h.launchFolderOf(c) == key {
			h.postLaunch(c, nil)
		}
	}
}

// postLaunchTarget：這個目標的狀態變了：送給列出它的每個對話（不同資料夾可能列出同一個專案）
func (h *Hub) postLaunchTarget(targetID string) {
	lh := h.launch
	if lh == nil || lh.closed {
		return
	}
	for _, c := range h.convs {
		if !h.convOpenAnywhere(c.Meta.ID) {
			continue
		}
		key := h.launchFolderOf(c)
		e := lh.folders[key]
		if key == "" || e == nil || !e.known {
			continue
		}
		for _, t := range h.launchTargetsOf(key, e) {
			if t.ID == targetID {
				h.postLaunch(c, nil)
				break
			}
		}
	}
}

// ───── 網頁指令 ─────

// handleLaunchCommand：targetId 只認這個對話的資料夾列出的（網頁不能指定任意路徑）。選目標（launchSelect、launchAction）記進 prefs.json
func (h *Hub) handleLaunchCommand(v *View, cmd string, c *Conv, msg map[string]any) {
	lh := h.launch
	if lh == nil || c == nil {
		return
	}
	key := h.launchFolderOf(c)
	if key == "" {
		return
	}
	if cmd == "launchRescan" {
		h.requestLaunchScan(c, launchScanUser)
		return
	}
	e := lh.folders[key]
	if e == nil || !e.known {
		return
	}
	targetID := str(msg, "targetId")
	var target *launchTarget
	for _, t := range h.launchTargetsOf(key, e) {
		if t.ID == targetID {
			tt := t
			target = &tt
			break
		}
	}
	if target == nil {
		return
	}
	switch cmd {
	case "launchSelect":
		selectedChanged := e.selected != target.ID
		e.selected = target.ID
		prefsChanged := launchRememberSelection(&lh.prefs, key, target.ID)
		modeChanged := false
		if m, ok := launchParseMode(str(msg, "mode")); ok && h.launchModeOf(target.ID) != m {
			modeChanged = launchRememberMode(&lh.prefs, target.ID, m)
			log.Printf("執行列：%s 改成 %s 模式", target.ProjectPath, launchConfiguration(m))
		}
		if prefsChanged || modeChanged {
			h.saveLaunchPrefs()
		}
		// 模式是專案的：列出這個專案的對話都要更新
		if modeChanged {
			h.postLaunchTarget(target.ID)
		} else if selectedChanged {
			h.postLaunchFolder(key)
		}
	case "launchLog":
		epoch, after := int64(-1), int64(0)
		if f, ok := msg["epoch"].(float64); ok {
			epoch = int64(f)
		}
		if f, ok := msg["after"].(float64); ok {
			after = int64(f)
		}
		slice := launchLogSlice{Lines: []string{}, Reset: true}
		if r := lh.runs[target.ID]; r != nil {
			slice = r.log.read(epoch, after)
		}
		h.sendTo(v, renderLaunchLog(c.Meta.ID, target.ID, slice))
	case "launchAction":
		if e.selected != target.ID {
			e.selected = target.ID
			h.postLaunchFolder(key)
		}
		if launchRememberSelection(&lh.prefs, key, target.ID) {
			h.saveLaunchPrefs()
		}
		h.launchAct(v, *target, str(msg, "action"))
	}
}

func (h *Hub) launchAct(v *View, target launchTarget, action string) {
	lh := h.launch
	run := lh.runs[target.ID]
	if run == nil {
		run = &launchRun{target: target, log: newLaunchLog(launchLogCapacity)}
		lh.runs[target.ID] = run
	} else if run.phase == launchIdle {
		run.target = target
	}
	switch launchPlan(run.phase, action) {
	case launchStepNone:
		return
	case launchStepStart:
		h.startLaunch(run)
	case launchStepStop:
		h.stopLaunch(run, false)
	case launchStepRestart:
		h.stopLaunch(run, true)
	case launchStepPause:
		if run.process != nil && run.process.Suspend() {
			run.phase = launchPaused
			h.saveLaunchRuns()
		} else {
			h.toast(v, "無法暫停這個程式")
		}
	case launchStepResume:
		if run.process != nil && run.process.Resume() {
			run.phase = launchRunning
			h.saveLaunchRuns()
		} else {
			h.toast(v, "無法繼續這個程式")
		}
	}
	h.postLaunchTarget(run.target.ID)
}

// startLaunch：新的一次：照這個專案現在選的模式；清空輸出，有建置步驟就先建置
func (h *Hub) startLaunch(run *launchRun) {
	lh := h.launch
	run.stopRequested, run.restartRequested = false, false
	run.last = nil
	run.mode = h.launchModeOf(run.target.ID)
	run.log.restart()
	now := lh.clock()
	run.since = &now
	log.Printf("執行列：開始 %s（%s 模式）", run.target.ProjectPath, launchConfiguration(run.mode))
	if build := launchBuildCommand(run.target, run.mode); build != nil {
		run.phase = launchBuilding
		if p := h.startLaunchProcess(run, *build, lh.starter.Build); p != nil {
			go h.watchLaunch(run, p, true)
		}
		return
	}
	h.startLaunchRun(run)
}

// startLaunchRun：執行（建置成功之後）：Debug 模式是 agent 的子程序；Release 模式和 agent 脫鉤，輸出寫進這次的輸出檔，開起來就記進 runs.json
func (h *Hub) startLaunchRun(run *launchRun) {
	lh := h.launch
	run.phase = launchRunning
	now := lh.clock()
	run.since = &now
	cmd := launchRunCommand(run.target, run.mode)
	if run.mode == launchDebug {
		if p := h.startLaunchProcess(run, cmd, lh.starter.RunAttached); p != nil {
			go h.watchLaunch(run, p, false)
		}
		return
	}
	logFile := launchLogFileName(run.target.ID, lh.clock())
	logPath := filepath.Join(lh.stateDir, logFile)
	p := h.startLaunchProcess(run, cmd, func(c launchCommand, onLine func(string)) (launchProcess, error) {
		return lh.starter.Run(c, logPath, onLine)
	})
	if p != nil {
		run.detached = true
		run.logFile = logFile
		h.saveLaunchRuns()
		go h.watchLaunch(run, p, false)
	}
}

// startLaunchProcess：開程序；開不起來記成「無法啟動」並回 nil
func (h *Hub) startLaunchProcess(run *launchRun, cmd launchCommand, start func(launchCommand, func(string)) (launchProcess, error)) launchProcess {
	epoch, _ := run.log.state()
	h.appendLaunchLine(run, epoch, launchDisplayCommand(cmd))
	p, err := start(cmd, func(line string) { h.appendLaunchLine(run, epoch, line) })
	if err != nil {
		log.Printf("執行列：無法啟動 %s（%s）：%v", cmd.File, run.target.ProjectPath, err)
		h.appendLaunchLine(run, epoch, err.Error())
		run.process = nil
		h.finishLaunch(run, launchStartFailedEnd(err.Error(), h.launch.clock()))
		return nil
	}
	run.process = p
	return p
}

// watchLaunch：等程序結束（不持有 h.mu），回到中樞處理。build＝這是建置：成功就接著執行
func (h *Hub) watchLaunch(run *launchRun, p launchProcess, build bool) {
	lh := h.launch
	select {
	case <-p.Done():
	case <-lh.ctx.Done():
		return
	}
	code := p.Code()
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	p.Close()
	if lh.closed || run.process != p {
		return
	}
	run.process = nil
	now := lh.clock()
	switch {
	case run.stopRequested:
		h.finishLaunch(run, launchStoppedEnd(now))
	case build && code != 0:
		h.finishLaunch(run, launchBuildFailedEnd(code, run.log.snapshot(), now))
	case build:
		h.startLaunchRun(run)
		h.postLaunchTarget(run.target.ID)
	default:
		h.finishLaunch(run, launchExitedEnd(code, now))
	}
}

func (h *Hub) finishLaunch(run *launchRun, end *launchEnd) {
	run.phase = launchIdle
	run.since = nil
	run.last = end
	run.stopRequested = false
	log.Printf("執行列：%s %s", run.target.ProjectPath, end.Text)
	if run.detached {
		// 程式結束了：不必再記得它；輸出已經在記憶體裡（畫面上照樣看得到），輸出檔刪掉
		run.detached = false
		h.deleteLaunchLog(run.logFile)
		run.logFile = ""
		h.saveLaunchRuns()
	}
	if run.restartRequested {
		run.restartRequested = false
		h.startLaunch(run)
	}
	h.postLaunchTarget(run.target.ID)
}

// stopLaunch：停止（建置中或執行中，暫停中的也一樣；接回的程式也是）；restart＝結束後重新建置並執行——
// 一律等這個程式真的結束了才開始建置（否則它還開著輸出的檔，建置會失敗）
func (h *Hub) stopLaunch(run *launchRun, restart bool) {
	if restart {
		run.restartRequested = true
	}
	if run.phase == launchStopping {
		return
	}
	run.stopRequested = true
	run.phase = launchStopping
	if run.process != nil {
		run.process.Kill()
	}
}

// appendLaunchLine：加一行輸出（任何 goroutine，可能持有或不持有 h.mu）：停掉的上一次晚到的不加；狀態（logSeq）稍後合併送一次
func (h *Hub) appendLaunchLine(run *launchRun, epoch int64, line string) {
	if !run.log.appendIf(epoch, line) {
		return
	}
	run.log.mu.Lock()
	pending := run.outputPending
	run.outputPending = true
	run.log.mu.Unlock()
	if pending {
		return
	}
	time.AfterFunc(launchOutputPostDelay, func() {
		run.log.mu.Lock()
		run.outputPending = false
		run.log.mu.Unlock()
		h.mu.Lock()
		h.postLaunchTarget(run.target.ID)
		h.mu.Unlock()
		h.kick()
	})
}

// stopLaunchAll：中樞關閉：停掉偵測，結束建置（被打斷之後也不會執行）與 Debug 模式執行的程式；Release 模式執行的程式不結束——
// 最後一次記下它的程序、放開，下次啟動接回
func (h *Hub) stopLaunchAll() {
	lh := h.launch
	if lh == nil || lh.closed {
		return
	}
	h.saveLaunchRuns()
	lh.closed = true
	lh.cancel()
	for _, run := range lh.runs {
		p := run.process
		if p == nil {
			continue
		}
		run.process = nil
		if !run.detached {
			p.Kill()
		}
		p.Close()
	}
}

// ───── 和 agent 脫鉤的程式：記下來、接回 ─────

// restoreLaunchRuns：讀 runs.json，還在執行的（PID 與開始時間都對得上）接回成執行中或暫停中、從輸出檔接著讀，都不在了的忘掉；
// 沒被記得的輸出檔（上次結束前沒刪到的）刪掉
func (h *Hub) restoreLaunchRuns() {
	lh := h.launch
	dir := lh.stateDir
	var file launchRunsFile
	b, err := os.ReadFile(filepath.Join(dir, launchStateFileName))
	missing := os.IsNotExist(err)
	if err == nil && json.Unmarshal(b, &file) != nil {
		log.Printf("執行列：讀不到 runs.json，上次開著的程式這次接不回來")
	}
	keep := map[string]bool{}
	for _, rec := range file.Runs {
		target := launchTargetFromRecord(rec)
		if target == nil || lh.runs[target.ID] != nil {
			continue
		}
		logFile := ""
		if launchIsLogFileName(rec.LogFile) {
			logFile = rec.LogFile
		}
		run := &launchRun{target: *target, log: newLaunchLog(launchLogCapacity)}
		run.log.restart()
		epoch, _ := run.log.state()
		run.log.append(launchReattachedNote)
		logPath := ""
		if logFile != "" {
			logPath = filepath.Join(dir, logFile)
		}
		p := lh.starter.Reattach(rec.Processes, logPath, func(line string) { h.appendLaunchLine(run, epoch, line) })
		if p == nil {
			continue
		}
		run.process = p
		run.detached = true
		run.mode = launchRelease
		run.logFile = logFile
		run.phase = launchRunning
		if rec.Paused {
			run.phase = launchPaused
		}
		run.since = launchSinceFromRecord(rec)
		if run.since == nil {
			now := lh.clock()
			run.since = &now
		}
		lh.runs[target.ID] = run
		if logFile != "" {
			keep[logFile] = true
		}
		log.Printf("執行列：接回 %s", target.ProjectPath)
		go h.watchLaunch(run, p, false)
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if launchIsLogFileName(e.Name()) && !keep[e.Name()] {
				h.deleteLaunchLog(e.Name())
			}
		}
	}
	// 檔案在：一定重寫一次（忘掉不在了的）
	if !missing {
		empty := ""
		lh.written = &empty
	}
	h.saveLaunchRuns()
}

// saveLaunchRuns：寫 runs.json：開著的執行的程式各自現在的程序、輸出檔、暫停中與否；沒有就刪檔。內容沒變不重寫
func (h *Hub) saveLaunchRuns() {
	lh := h.launch
	if lh == nil || lh.stateDir == "" {
		return
	}
	ids := make([]string, 0, len(lh.runs))
	for id := range lh.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var records []launchRunRecord
	for _, id := range ids {
		run := lh.runs[id]
		if !run.detached || run.process == nil {
			continue
		}
		// 讀不到（剛好結束、還沒收到結束通知）：不記這一筆，結束時本來也要忘掉它
		if procs := run.process.Processes(); len(procs) > 0 {
			records = append(records, launchToRecord(run.target, run.since, run.phase == launchPaused, run.logFile, procs))
		}
	}
	path := filepath.Join(lh.stateDir, launchStateFileName)
	if len(records) == 0 {
		if lh.written != nil {
			_ = os.Remove(path)
			lh.written = nil
		}
		return
	}
	b, err := json.MarshalIndent(launchRunsFile{Runs: records}, "", "  ")
	if err != nil {
		return
	}
	text := string(b)
	if lh.written != nil && *lh.written == text {
		return
	}
	writeJSON(path, launchRunsFile{Runs: records})
	lh.written = &text
}

func (h *Hub) loadLaunchPrefs() {
	lh := h.launch
	b, err := os.ReadFile(filepath.Join(lh.stateDir, launchPrefsFileName))
	if err == nil && json.Unmarshal(b, &lh.prefs) != nil {
		log.Printf("執行列：讀不到 prefs.json，選的專案與執行模式回到預設")
		lh.prefs = launchPrefsFile{}
	}
}

func (h *Hub) saveLaunchPrefs() {
	lh := h.launch
	if lh.stateDir != "" {
		writeJSON(filepath.Join(lh.stateDir, launchPrefsFileName), lh.prefs)
	}
}

func (h *Hub) deleteLaunchLog(logFile string) {
	lh := h.launch
	if lh == nil || logFile == "" || !launchIsLogFileName(logFile) || strings.ContainsAny(logFile, "/\\") {
		return
	}
	if err := os.Remove(filepath.Join(lh.stateDir, logFile)); err != nil && !os.IsNotExist(err) {
		log.Printf("執行列：刪除輸出檔 %s 失敗：%v", logFile, err)
	}
}
