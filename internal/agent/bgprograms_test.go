package agent

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kevinlincg/myriagent/internal/procs"
)

// 測試執行檔的兩種角色（在 TestMain 之前的 init 就處理掉）：
//   - MYRIAGENT_TEST_DETACH=<秒>：開一個睡 <秒> 的孫程序（新的工作階段）就結束，孫程序被 launchd／init 收養＝代理以 nohup 開、脫離 CLI 的程式
//   - MYRIAGENT_TEST_SLEEP=<秒>：睡著（孫程序本身；自己的執行檔，macOS 讀得到它的環境變數）
func init() {
	if s := os.Getenv("MYRIAGENT_TEST_SLEEP"); s != "" {
		n, _ := strconv.ParseFloat(s, 64)
		time.Sleep(time.Duration(n * float64(time.Second)))
		os.Exit(0)
	}
	if s := os.Getenv("MYRIAGENT_TEST_DETACH"); s != "" {
		c := exec.Command(os.Args[0], "worker", "--port", "5173", "--for", s)
		c.Env = append(os.Environ(), "MYRIAGENT_TEST_DETACH=", "MYRIAGENT_TEST_SLEEP="+s)
		c.SysProcAttr = detachAttr()
		if err := c.Start(); err != nil {
			os.Exit(1)
		}
		os.Stdout.WriteString(strconv.Itoa(c.Process.Pid) + "\n")
		os.Exit(0)
	}
}

// startDetached 開一個脫離這個程序的背景程式（帶對話標記），回它的 PID
func startDetached(t *testing.T, convID string, seconds float64) int {
	t.Helper()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), "MYRIAGENT_TEST_DETACH="+strconv.FormatFloat(seconds, 'f', -1, 64), envConversation+"="+convID)
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s, ok := procs.StartTime(pid); ok {
			procs.KillTree(pid, s)
		}
	})
	return pid
}

func TestClassifyPrograms(t *testing.T) {
	yes, no := true, false
	rows := []processRow{
		{Pid: 1, Ppid: 0, Start: 1, Name: "launchd"},
		{Pid: 100, Ppid: 1, Start: 10, Name: "myriagent"},
		{Pid: 101, Ppid: 100, Start: 20, Name: "claude", Marker: "c1"},                                                        // CLI：agent 底下，不算
		{Pid: 102, Ppid: 101, Start: 30, Name: "node", Marker: "c1", CommandLine: "node tool.js"},                             // 工具：還在 CLI 底下，不算
		{Pid: 200, Ppid: 1, Start: 40, Name: "node", Marker: "c1", CommandLine: "node server.js"},                             // 脫離了：算
		{Pid: 201, Ppid: 200, Start: 50, Name: "node", Marker: "c1", CommandLine: "node child.js"},                            // 同一棵樹：只算根
		{Pid: 300, Ppid: 1, Start: 60, Name: "Safari", Marker: "c2", IsConsole: &no},                                          // 視窗程式：不算
		{Pid: 301, Ppid: 1, Start: 61, Name: "Google Chrome", Marker: "c2", IsConsole: &no, CommandLine: "chrome --headless"}, // 自動化瀏覽器：算
		{Pid: 302, Ppid: 1, Start: 62, Name: "java", Marker: "c2", IsConsole: &yes, CommandLine: "java GradleDaemon"},         // 建置常駐：不算
		{Pid: 303, Ppid: 302, Start: 63, Name: "javac", Marker: "c2"},                                                         // 建置常駐開的：不算
		{Pid: 400, Ppid: 1, Start: 70, Name: "sleep"},                                                                         // 沒有標記
		{Pid: 500, Ppid: 999, Start: 80, Name: "py", Marker: "c3"},                                                            // 父程序不在：算
		{Pid: 501, Ppid: 100, Start: 5, Name: "py", Marker: "c3"},                                                             // 父 PID 是 agent 但比 agent 早建立（PID 重複使用）：算
	}
	got := classifyPrograms(rows, map[int]bool{100: true})
	ids := func(list []bgProgram) []int {
		var out []int
		for _, p := range list {
			out = append(out, p.Pid)
		}
		return out
	}
	if g := ids(got["c1"]); len(g) != 1 || g[0] != 200 {
		t.Fatalf("c1：%v", g)
	}
	if g := ids(got["c2"]); len(g) != 1 || g[0] != 301 {
		t.Fatalf("c2：%v", g)
	}
	if g := ids(got["c3"]); len(g) != 2 || g[0] != 501 || g[1] != 500 {
		t.Fatalf("c3（依建立時間）：%v", g)
	}
}

func TestProgramExitsAndNotice(t *testing.T) {
	prev := map[string][]bgProgram{"c1": {{Pid: 10, Start: 100, Name: "bash", CommandLine: "bash run.sh"}, {Pid: 20, Start: 200, Name: "node"}}}
	// 10 結束了但子程序 11 接著當根：不算結束；20 消失：結束
	cur := map[string][]bgProgram{"c1": {{Pid: 11, Start: 150, Ppid: 10}}}
	exits := programExits(prev, cur)
	if len(exits) != 1 || exits[0].Program.Pid != 20 {
		t.Fatalf("exits：%+v", exits)
	}
	code, end := 1, int64(100+12*60*1_000_000)
	e := bgExit{Program: bgProgram{Start: 100, Name: "python", CommandLine: "/usr/bin/python3 worker6.py"}, ExitCode: &code, ExitedAt: &end}
	if s := programExitNotice(e); s != "代理另開的程式「python3 worker6.py」已結束（結束碼 1，執行了 12 分鐘）" {
		t.Fatal(s)
	}
	if exitCodeText(-1) != "被強制結束" || exitCodeText(int(int32(-1073741510))) != "被中斷（Ctrl+C）" || exitCodeText(0x10000) != "結束碼 0x00010000" {
		t.Fatal("exitCodeText")
	}
	if durationText(400*time.Millisecond) != "1 秒" || durationText(90*time.Minute) != "1 小時 30 分" || durationText(2*time.Hour) != "2 小時" {
		t.Fatal("durationText")
	}
}

func TestShortCommandAndCliTask(t *testing.T) {
	if s := shortCommand(`"C:\Program Files\node.exe"  server.js   --port 1`, "node.exe"); s != "node server.js --port 1" {
		t.Fatal(s)
	}
	if s := shortCommand("", "node"); s != "node" {
		t.Fatal(s)
	}
	if s := shortCommandN("/bin/echo "+strings.Repeat("字", 100), "echo", 10); s != "echo 字字字字…" {
		t.Fatal(s)
	}
	p := bgProgram{Name: "python", CommandLine: "/usr/bin/python worker.py --fast"}
	if !isCliTask(p, []string{`cd /c/proj && "python" worker.py   --fast > log`}) {
		t.Fatal("應該對得上 CLI 的背景工作")
	}
	if isCliTask(bgProgram{Name: "node", CommandLine: "node"}, []string{"node x"}) {
		t.Fatal("沒有參數的不比")
	}
	if markerOf([]string{"A=1", envConversation + "=c0123456789abcdef"}) != "c0123456789abcdef" || markerOf([]string{envConversation + "=a b"}) != "" {
		t.Fatal("markerOf")
	}
}

// 讀不到環境變數的程序（macOS 的 /bin/sleep）：負責的程序是 agent 才推斷，父程序鏈或對話資料夾
func TestSnapshotAttributesHiddenEnv(t *testing.T) {
	now := time.Unix(1000, 0)
	table := []procs.Proc{
		{Pid: 50, Ppid: 1, Start: now.UnixMicro() - 60e6, Name: "node"},   // 帶標記
		{Pid: 51, Ppid: 50, Start: now.UnixMicro() - 50e6, Name: "sleep"}, // 讀不到：父程序有標記
		{Pid: 52, Ppid: 1, Start: now.UnixMicro() - 50e6, Name: "bash"},   // 讀不到：在對話資料夾
		{Pid: 53, Ppid: 1, Start: now.UnixMicro() - 50e6, Name: "bash"},   // 讀不到、不是 agent 開的
	}
	s := &nativeSnapshot{cache: map[[2]int64]*snapEntry{}, own: 7, now: func() time.Time { return now },
		list: func() ([]procs.Proc, error) { return table, nil },
		inspect: func(pid int) (procs.Info, bool) {
			if pid == 50 {
				return procs.Info{Args: []string{"node", "a b.js"}, Env: []string{envConversation + "=c1"}}, true
			}
			return procs.Info{Args: []string{"x"}, EnvHidden: true}, true
		},
		exePath: func(pid int) string { return "/usr/local/bin/node" },
		cwd: func(pid int) string {
			return map[int]string{52: "/data/conversations/c9/workspace/sub", 53: "/data/conversations/c8"}[pid]
		},
		responsible: func(pid int) int { return map[int]int{51: 7, 52: 7, 53: 99}[pid] },
	}
	rows, _ := s.snapshot()
	m := map[int]processRow{}
	for _, r := range rows {
		m[r.Pid] = r
	}
	if m[50].Marker != "c1" || m[50].CommandLine != `node "a b.js"` || m[51].Marker != "c1" || m[52].Marker != "c9" || m[53].Marker != "" {
		t.Fatalf("推斷不對：%+v", m)
	}
}

// 中樞：真的開一個脫離的程式 → 一輪結束之後列在標頭；programStop 停掉（不加提示）；自己結束的加提示
func TestBackgroundProgramsInHeader(t *testing.T) {
	h, tv := newTestHub(t)
	id := tv.newDraft()
	mon := newProgramMonitor(newNativeSnapshot())
	mon.interval = 150 * time.Millisecond
	t.Cleanup(mon.close)
	h.attachPrograms(mon)

	// 一輪裡代理以 nohup 開了兩個程式（一個一直跑、一個 3 秒後自己結束），然後這一輪結束
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "慢一點"})
	tv.wait("執行中", func(m map[string]any) bool {
		hd, _ := m["header"].(map[string]any)
		return m["type"] == "header" && hd["running"] == true
	})
	pid := startDetached(t, id, 60)
	pid2 := startDetached(t, id, 3)
	hasPid := func(m map[string]any, pid int) bool {
		hd, _ := m["header"].(map[string]any)
		list, _ := hd["programs"].([]any)
		for _, x := range list {
			if p, _ := x.(map[string]any); p != nil && int(p["pid"].(float64)) == pid {
				return strings.Contains(p["command"].(string), "worker --port 5173")
			}
		}
		return false
	}
	time.Sleep(400 * time.Millisecond) // 執行中的掃描找到了也不列
	tv.mu.Lock()
	for _, m := range tv.msgs {
		if m["type"] == "header" && hasPid(m, pid) {
			t.Fatal("執行中就列出了")
		}
	}
	tv.mu.Unlock()
	tv.send(map[string]any{"cmd": "stop", "conversationId": id})

	var progID string
	m := tv.wait("標頭列出背景程式", func(m map[string]any) bool { return m["type"] == "header" && hasPid(m, pid) && hasPid(m, pid2) })
	for _, x := range m["header"].(map[string]any)["programs"].([]any) {
		if p := x.(map[string]any); int(p["pid"].(float64)) == pid {
			progID = p["id"].(string)
		}
	}
	tv.wait("清單的 programCount", func(m map[string]any) bool {
		if m["type"] != "conversations" {
			return false
		}
		for _, x := range m["items"].([]any) {
			if c := x.(map[string]any); c["id"] == id && c["programCount"] == float64(2) {
				return true
			}
		}
		return false
	})
	tv.wait("帳戶卡的 sessions.programs", func(m map[string]any) bool {
		if m["type"] != "accounts" {
			return false
		}
		a := m["accounts"].([]any)[0].(map[string]any)
		ps, _ := a["programs"].([]any)
		return a["sessions"].(map[string]any)["programs"] == float64(1) && len(ps) == 1
	})

	tv.send(map[string]any{"cmd": "programStop", "conversationId": id, "programId": progID})
	tv.wait("停止後不再列出", func(m map[string]any) bool { return m["type"] == "header" && !hasPid(m, pid) && hasPid(m, pid2) })
	stillAlive := func() bool { s, ok := procs.StartTime(pid); return ok && procs.Alive(pid, s) }
	for deadline := time.Now().Add(5 * time.Second); stillAlive() && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if stillAlive() {
		t.Fatal("程式還在跑")
	}

	// 自己結束的：加一則提示；使用者停止的那個沒有
	tv.wait("結束提示", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["noticeKind"] == "backgroundProgram" && strings.Contains(x["html"].(string), "--for 3」已結束")
	})
	tv.wait("都結束了", func(m map[string]any) bool {
		hd, _ := m["header"].(map[string]any)
		return m["type"] == "header" && hd["programs"] == nil
	})
	tv.mu.Lock()
	n := 0
	for _, m := range tv.msgs {
		if x := itemOf(m); x != nil && x["noticeKind"] == "backgroundProgram" {
			n++
		}
	}
	tv.mu.Unlock()
	if n != 1 {
		t.Fatalf("結束提示應該只有 1 則，有 %d 則", n)
	}
	// 不在清單的 id：提示已經結束
	tv.send(map[string]any{"cmd": "programStop", "conversationId": id, "programId": progID})
	tv.wait("已經結束的提示", func(m map[string]any) bool { return m["type"] == "toast" && m["text"] == "這個程式已經結束了" })
}

// 執行中開出的程式要等這一輪結束才列（前景指令不算）
func TestProgramsNotConfirmedWhileRunning(t *testing.T) {
	h, _ := newTestHub(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	c := &Conv{Meta: ConvMeta{ID: "c1", AccountID: "x"}}
	c.init()
	h.convs["c1"] = c
	c.running = true
	onProgramsTurnStarted(h, c)
	p := bgProgram{Pid: 9, Start: time.Now().UnixMicro(), Name: "node"}
	h.onProgramScan(bgScan{Programs: map[string][]bgProgram{"c1": {p}}, TakenAt: time.Now()})
	if h.programCount(c) != 0 {
		t.Fatal("執行中不該列出")
	}
	c.running = false
	onProgramsTurnCompleted(h, c)
	h.onProgramScan(bgScan{Programs: map[string][]bgProgram{"c1": {p}}, TakenAt: time.Now()})
	if h.programCount(c) != 1 || h.headerPrograms(c) == nil {
		t.Fatal("一輪結束之後應該列出")
	}
	// 結束之前就開始的掃描（晚一點才送到）不算
	h.onProgramScan(bgScan{Programs: map[string][]bgProgram{}, TakenAt: time.Now()})
	q := bgProgram{Pid: 10, Start: time.Now().UnixMicro(), Name: "x"}
	h.onProgramScan(bgScan{Programs: map[string][]bgProgram{"c1": {q}}, TakenAt: time.Now().Add(-time.Hour)})
	if h.programCount(c) != 0 {
		t.Fatal("一輪結束前開始的掃描不算撐過了一輪")
	}
	// 沒經過開始掛鉤、但正在執行的對話（例如別家的 Provider 自己開的一輪）：照樣不列
	c2 := &Conv{Meta: ConvMeta{ID: "c2", AccountID: "x"}}
	c2.init()
	c2.running = true
	h.convs["c2"] = c2
	h.onProgramScan(bgScan{Programs: map[string][]bgProgram{"c2": {{Pid: 11, Start: 1, Name: "y"}}}, TakenAt: time.Now()})
	if h.programCount(c2) != 0 {
		t.Fatal("執行中的對話不該列出")
	}
}
