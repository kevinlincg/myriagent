package procs

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 測試執行檔在 PROCS_TEST_SLEEP 有值時當成一個睡著的子程序（自己的執行檔，macOS 讀得到它的環境變數；系統內建的 /bin/sleep 讀不到）
func TestMain(m *testing.M) {
	if os.Getenv("PROCS_TEST_SLEEP") != "" {
		if os.Getenv("PROCS_TEST_SPAWN") != "" {
			// 再開一個孫程序（KillTree 要連它一起結束）
			c := exec.Command(os.Args[0])
			c.Env = append(os.Environ(), "PROCS_TEST_SPAWN=")
			_ = c.Start()
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startChild(t *testing.T, extra ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^$", "marker-arg")
	c.Env = append(os.Environ(), append([]string{"PROCS_TEST_SLEEP=1", "MYRIAGENT_AGENT_CONVERSATION=cabc"}, extra...)...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
	return c
}

func find(t *testing.T, pid int) Proc {
	t.Helper()
	table, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range table {
		if p.Pid == pid {
			return p
		}
	}
	t.Fatalf("程序表裡沒有 %d", pid)
	return Proc{}
}

func TestListInspectAndUsage(t *testing.T) {
	c := startChild(t)
	time.Sleep(200 * time.Millisecond)
	p := find(t, c.Process.Pid)
	if p.Ppid != os.Getpid() || p.Start == 0 {
		t.Fatalf("父程序或建立時間不對：%+v", p)
	}
	if s, ok := StartTime(p.Pid); !ok || s != p.Start {
		t.Fatalf("StartTime %d 和程序表的 %d 不同", s, p.Start)
	}
	info, ok := Inspect(p.Pid)
	if !ok || EnvValue(info.Env, "MYRIAGENT_AGENT_CONVERSATION") != "cabc" {
		t.Fatalf("讀不到標記：%+v", info)
	}
	if !strings.Contains(strings.Join(info.Args, " "), "marker-arg") {
		t.Fatalf("命令列不對：%v", info.Args)
	}
	if ExePath(p.Pid) == "" || Cwd(p.Pid) == "" {
		t.Fatal("讀不到執行檔路徑或工作目錄")
	}
	if u, ok := ReadUsage(os.Getpid()); !ok || u.CPUNs == 0 || u.Mem == 0 {
		t.Fatalf("用量不對：%+v", u)
	}
	if !Alive(p.Pid, p.Start) || Alive(p.Pid, p.Start+1) {
		t.Fatal("Alive 沒有比對建立時間")
	}
}

func TestKillTree(t *testing.T) {
	c := startChild(t, "PROCS_TEST_SPAWN=1")
	var grand Proc
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		table, _ := List()
		for _, d := range Descendants(table, find(t, c.Process.Pid)) {
			grand = d
		}
		if grand.Pid != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if grand.Pid == 0 {
		t.Fatal("等不到孫程序")
	}
	root := find(t, c.Process.Pid)
	if KillTree(root.Pid, root.Start+1) {
		t.Fatal("建立時間不對還結束了")
	}
	if !KillTree(root.Pid, root.Start) {
		t.Fatal("結束不了")
	}
	_, _ = c.Process.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for Alive(grand.Pid, grand.Start) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if Alive(grand.Pid, grand.Start) {
		t.Fatal("孫程序沒有跟著結束")
	}
}

func TestDescendantsIgnoresReusedPid(t *testing.T) {
	table := []Proc{{Pid: 10, Ppid: 1, Start: 100}, {Pid: 11, Ppid: 10, Start: 200}, {Pid: 12, Ppid: 10, Start: 50}, {Pid: 13, Ppid: 11, Start: 300}}
	got := Descendants(table, Proc{Pid: 10, Start: 100})
	if len(got) != 2 || got[0].Pid != 11 || got[1].Pid != 13 {
		t.Fatalf("子孫不對：%+v", got)
	}
}
