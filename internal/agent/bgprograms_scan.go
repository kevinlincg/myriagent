package agent

import (
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kevinlincg/myriagent/internal/procs"
)

// 背景程式的掃描（C# Background/BackgroundProgramMonitor.cs 與 Myriagent.Mac 的 NativeProcessSnapshot.Mac.cs）：
// 每 programScanInterval（中樞要求時 programRescanDelay 後）列一次程序、以 agent 自己為根分類（classifyPrograms），把結果交給中樞。
// 在自己的 goroutine 上跑，同一時間只有一次掃描；掃描失敗只記錄、下一次照常。消失的程式放進下一次的 exits（programExits：換了根的不算結束）

// bgScan：一次掃描的結果。Programs：對話 id → 還在跑的背景程式；Exits：上一次掃描之後結束的（第一次掃描沒有）；
// TakenAt：開始列程序的時間（中樞只把一輪結束之後才開始的掃描裡還在的程式當成撐過了那一輪）
type bgScan struct {
	Programs map[string][]bgProgram
	Exits    []bgExit
	TakenAt  time.Time
}

// programSource：背景程式的來源（中樞以 attachPrograms 接上；測試換成假的）
type programSource interface {
	// start 開始定期掃描；每次掃描完在自己的 goroutine 呼叫 onScan（不持有 h.mu）。只呼叫一次
	start(onScan func(bgScan))
	// requestScan 儘快再掃一次（一輪結束時：代理可能剛開了程式）
	requestScan()
	// stop 結束這個背景程式與它的子孫（programStop），完成後儘快再掃一次；那個程序已經不是它、已經結束或結束不了回 false。
	// 它在下一次掃描的 exits 裡帶 StoppedByUser
	stop(p bgProgram) bool
	close()
}

// processSnapshotter：列出目前的程序（測試換成假的）
type processSnapshotter interface {
	snapshot() ([]processRow, error)
}

// programMonitor：programSource 的正式實作
type programMonitor struct {
	snap     processSnapshotter
	managed  func() map[int]bool
	interval time.Duration
	alive    func(pid int, start int64) bool
	kill     func(pid int, start int64) bool

	mu       sync.Mutex
	onScan   func(bgScan)
	kick     chan struct{}
	done     chan struct{}
	closed   bool
	started  bool
	stopped  map[[2]int64]bool
	previous map[string][]bgProgram
	scanned  bool
}

func newProgramMonitor(snap processSnapshotter) *programMonitor {
	return &programMonitor{
		snap: snap, managed: func() map[int]bool { return map[int]bool{procs.Self(): true} }, interval: programScanInterval,
		alive: procs.Alive, kill: procs.KillTree,
		kick: make(chan struct{}, 1), done: make(chan struct{}), stopped: map[[2]int64]bool{},
	}
}

func (m *programMonitor) start(onScan func(bgScan)) {
	m.mu.Lock()
	if m.started || m.closed {
		m.mu.Unlock()
		return
	}
	m.started, m.onScan = true, onScan
	m.mu.Unlock()
	go m.loop()
}

func (m *programMonitor) loop() {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-timer.C:
		case <-m.kick:
			// 連續好幾個對話同時結束時併成一次
			select {
			case <-m.done:
				return
			case <-time.After(programRescanDelay):
			}
		}
		m.tick()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(m.interval)
	}
}

func (m *programMonitor) requestScan() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *programMonitor) stop(p bgProgram) bool {
	key := [2]int64{int64(p.Pid), p.Start}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	m.stopped[key] = true
	m.mu.Unlock()
	ok := m.kill(p.Pid, p.Start)
	if !ok {
		m.mu.Lock()
		delete(m.stopped, key)
		m.mu.Unlock()
	}
	m.requestScan()
	return ok
}

func (m *programMonitor) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.done)
	}
}

// tick 掃描一次（掃描的 goroutine；測試直接呼叫）
func (m *programMonitor) tick() {
	taken := time.Now()
	rows, err := m.snap.snapshot()
	if err != nil {
		log.Printf("背景程式的掃描失敗：%v", err)
		return
	}
	programs := classifyPrograms(rows, m.managed())
	// 已經結束（殭屍、剛好結束）的拿掉
	for id, list := range programs {
		kept := list[:0]
		for _, p := range list {
			if m.alive(p.Pid, p.Start) {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			delete(programs, id)
		} else {
			programs[id] = kept
		}
	}
	var exits []bgExit
	m.mu.Lock()
	if m.scanned {
		exits = programExits(m.previous, programs)
		for i := range exits {
			key := [2]int64{int64(exits[i].Program.Pid), exits[i].Program.Start}
			exits[i].StoppedByUser = m.stopped[key]
			delete(m.stopped, key)
		}
	}
	m.previous, m.scanned = programs, true
	onScan := m.onScan
	closed := m.closed
	m.mu.Unlock()
	if onScan != nil && !closed {
		onScan(bgScan{Programs: programs, Exits: exits, TakenAt: taken})
	}
}

// ---- 列程序（正式的） ----

// 剛建立的程序可能還沒換成要執行的程式（fork 之後、exec 之前）：這段時間內認不出來就下一次再看
const inspectRetryWindow = 10 * time.Second

// 對話自己的資料夾：…/conversations/〈對話 id〉（後面可以再有子資料夾）
var conversationFolder = regexp.MustCompile(`/conversations/([^/]+)(?:/|$)`)

type snapEntry struct {
	name        string
	marker      string
	isConsole   *bool
	commandLine string
	cwd         string
	cwdRead     bool
	envHidden   bool
	done        bool
}

// nativeSnapshot：每個程序第一次出現時讀一次它的環境變數裡的對話標記，帶標記的再記下命令列與是不是視窗程式（執行檔在 .app 裡）。一律只讀。
// macOS 不讓人讀系統內建程式（/bin/sleep、/bin/bash…）的環境變數：代理用 nohup 開的 shell 迴圈正是這一類，
// 所以讀不到環境變數的程序改用推斷的（attribute）：先確認它是這個 agent 開出來的（系統記的「負責的程序」是 agent），
// 再依序看父程序鏈上有沒有帶標記的、工作目錄是不是某個對話自己的資料夾、有沒有哪個對話的 CLI 的工作目錄包含它的
type nativeSnapshot struct {
	cache map[[2]int64]*snapEntry
	own   int
	// 測試可以換（預設是 procs 的）
	list        func() ([]procs.Proc, error)
	inspect     func(pid int) (procs.Info, bool)
	exePath     func(pid int) string
	cwd         func(pid int) string
	responsible func(pid int) int
	now         func() time.Time
}

func newNativeSnapshot() *nativeSnapshot {
	return &nativeSnapshot{cache: map[[2]int64]*snapEntry{}, own: procs.Self(), list: procs.List, inspect: procs.Inspect,
		exePath: procs.ExePath, cwd: procs.Cwd, responsible: procs.Responsible, now: time.Now}
}

type liveProc struct {
	p procs.Proc
	e *snapEntry
}

func (s *nativeSnapshot) snapshot() ([]processRow, error) {
	table, err := s.list()
	if err != nil {
		return nil, err
	}
	nowUs := s.now().UnixMicro()
	live := make([]liveProc, 0, len(table))
	seen := map[[2]int64]bool{}
	for _, p := range table {
		key := [2]int64{int64(p.Pid), p.Start}
		e := s.cache[key]
		if e == nil {
			e = &snapEntry{name: p.Name}
			s.cache[key] = e
		}
		seen[key] = true
		if !e.done && !e.envHidden {
			s.inspectOne(p.Pid, e, nowUs-p.Start > inspectRetryWindow.Microseconds())
		}
		live = append(live, liveProc{p, e})
	}
	for k := range s.cache {
		if !seen[k] {
			delete(s.cache, k)
		}
	}
	// 讀不到環境變數的：等這一輪所有程序的標記都讀完再推斷
	byPid := map[int]liveProc{}
	pending := false
	for _, lp := range live {
		if _, ok := byPid[lp.p.Pid]; !ok {
			byPid[lp.p.Pid] = lp
		}
		if lp.e.envHidden && !lp.e.done {
			pending = true
		}
	}
	if pending {
		for _, lp := range live {
			if lp.e.envHidden && !lp.e.done {
				s.attribute(lp, byPid, nowUs-lp.p.Start > inspectRetryWindow.Microseconds())
			}
		}
	}
	rows := make([]processRow, 0, len(live))
	for _, lp := range live {
		rows = append(rows, processRow{Pid: lp.p.Pid, Ppid: lp.p.Ppid, Start: lp.p.Start, Name: lp.e.name, Marker: lp.e.marker, IsConsole: lp.e.isConsole, CommandLine: lp.e.commandLine})
	}
	return rows, nil
}

// inspectOne 讀標記；有標記再記命令列、完整的名稱與是不是視窗程式。沒有標記而且程序還很新時不記成完成（下一次再讀）
func (s *nativeSnapshot) inspectOne(pid int, e *snapEntry, settled bool) {
	info, ok := s.inspect(pid)
	if !ok {
		// 別的權限的程序、已經結束：不是這個 agent 的代理開的
		e.done = true
		return
	}
	quoted := make([]string, len(info.Args))
	for i, a := range info.Args {
		quoted[i] = quoteArg(a)
	}
	e.commandLine = strings.Join(quoted, " ")
	if info.EnvHidden {
		e.envHidden = true
		return
	}
	e.marker = markerOf(info.Env)
	if e.marker == "" {
		e.commandLine = ""
		e.done = settled
		return
	}
	s.describe(pid, e)
	e.done = true
}

// attribute：讀不到環境變數的程序屬於哪個對話；認不出來、而且程序已經不新了就不再看
func (s *nativeSnapshot) attribute(lp liveProc, byPid map[int]liveProc, settled bool) {
	e := lp.e
	if s.responsible(lp.p.Pid) != s.own {
		e.commandLine, e.done = "", true
		return
	}
	marker := markerOfAncestors(lp.p.Ppid, lp.p.Start, byPid)
	if marker == "" {
		marker = s.markerOfCwd(lp, byPid)
	}
	if marker == "" {
		if settled {
			e.commandLine, e.done = "", true
		}
		return
	}
	e.marker = marker
	s.describe(lp.p.Pid, e)
	e.done = true
}

// markerOfAncestors：活著的父程序鏈上第一個帶標記的（父程序要建立得比子程序早）
func markerOfAncestors(ppid int, start int64, byPid map[int]liveProc) string {
	child, cur := start, ppid
	for d := 0; d < maxProgramDepth && cur > 1; d++ {
		p, ok := byPid[cur]
		if !ok || p.p.Start > child {
			return ""
		}
		if p.e.marker != "" {
			return p.e.marker
		}
		child, cur = p.p.Start, p.p.Ppid
	}
	return ""
}

// markerOfCwd：依工作目錄：在某個對話自己的資料夾裡就是那個對話；否則找工作目錄包含它的 CLI（這個 agent 直接啟動、帶標記的程序），
// 不只一個對話時取比它早啟動的裡面最晚的那個
func (s *nativeSnapshot) markerOfCwd(lp liveProc, byPid map[int]liveProc) string {
	dir := s.cwdOf(lp.p.Pid, lp.e)
	if dir == "" {
		return ""
	}
	if m := conversationFolder.FindStringSubmatch(dir); m != nil && isMarker(m[1]) {
		return m[1]
	}
	best, bestStart := "", int64(-1<<63)
	for pid, cli := range byPid {
		if cli.p.Ppid != s.own || cli.e.marker == "" || cli.p.Start > lp.p.Start || cli.p.Start <= bestStart {
			continue
		}
		if root := s.cwdOf(pid, cli.e); root != "" && isSameOrUnder(dir, root) {
			best, bestStart = cli.e.marker, cli.p.Start
		}
	}
	return best
}

func isSameOrUnder(dir, root string) bool {
	root = strings.TrimRight(root, "/")
	return root != "" && strings.HasPrefix(dir, root) && (len(dir) == len(root) || dir[len(root)] == '/')
}

func (s *nativeSnapshot) cwdOf(pid int, e *snapEntry) string {
	if !e.cwdRead {
		e.cwdRead, e.cwd = true, s.cwd(pid)
	}
	return e.cwd
}

// describe：帶標記的程序：完整的名稱（sysctl 給的最多 16 個字）與是不是視窗程式
func (s *nativeSnapshot) describe(pid int, e *snapEntry) {
	path := s.exePath(pid)
	if path == "" {
		return
	}
	e.name = filepath.Base(path)
	console := !procs.IsGUIPath(path)
	e.isConsole = &console
}
