package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func getsid(pid int) (int, error) { return unix.Getsid(pid) }

// 執行列的流程測試：真的開程序（PATH 最前面放假的 npm、dotnet 殼層腳本），看建置、執行、停止、暫停、脫鉤與接回。

// fakeLaunchTools：假的 npm 與 dotnet。專案資料夾裡有 sleep 檔就一直跑（把 PID 寫進 pid 檔），有 fail 檔時建置失敗
func fakeLaunchTools(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	npm := `#!/bin/sh
echo "npm $1 $2"
echo "到 stderr" >&2
echo $$ > "$PWD/pid"
if [ -f "$PWD/sleep" ]; then sleep 60 & echo $! > "$PWD/child"; wait; fi
exit 3
`
	dotnet := `#!/bin/sh
case "$1" in
build)
  echo "  Determining projects to restore..."
  if [ -f "$PWD/fail" ]; then echo "$PWD/Program.cs(3,5): error CS1002: ; expected [$2]"; exit 1; fi
  echo "建置成功 $4"
  exit 0;;
run)
  echo "執行 $6"
  echo $$ > "$PWD/pid"
  if [ -f "$PWD/sleep" ]; then sleep 60 & echo $! > "$PWD/child"; wait; fi
  exit 0;;
esac
exit 9
`
	_ = os.WriteFile(filepath.Join(bin, "npm"), []byte(npm), 0o755)
	_ = os.WriteFile(filepath.Join(bin, "dotnet"), []byte(dotnet), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func electronProject(t *testing.T) string {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","dependencies":{"electron":"30"},"scripts":{"start":"electron ."}}`), 0o644)
	return dir
}

func dotnetProject(t *testing.T) string {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "App.csproj"), []byte(`<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>WinExe</OutputType><TargetFramework>net9.0</TargetFramework></PropertyGroup></Project>`), 0o644)
	return dir
}

// launchHarness：測試中樞掛上執行列，開一個資料夾是 dir 的對話，回對話 id 與目標 id
func launchHarness(t *testing.T, dir string) (*Hub, *testView, string, string, string) {
	t.Helper()
	fakeLaunchTools(t)
	h, tv := newTestHub(t)
	state := t.TempDir()
	h.mu.Lock()
	h.attachLaunch(fsLaunchScanner{}, unixLaunchStarter{}, state)
	h.mu.Unlock()
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "setFolder", "conversationId": id, "path": dir})
	m := tv.wait("執行列", func(m map[string]any) bool { return m["type"] == "launchStatus" && m["launch"] != nil })
	l := m["launch"].(map[string]any)
	return h, tv, id, l["selected"].(string), state
}

func launchOf(m map[string]any) map[string]any {
	if m["type"] != "launchStatus" {
		return nil
	}
	l, _ := m["launch"].(map[string]any)
	return l
}

func (tv *testView) waitLaunch(what string, ok func(l map[string]any) bool) map[string]any {
	tv.t.Helper()
	return launchOf(tv.wait(what, func(m map[string]any) bool { l := launchOf(m); return l != nil && ok(l) }))
}

func (tv *testView) clear() {
	tv.mu.Lock()
	tv.msgs = nil
	tv.mu.Unlock()
}

func lastKind(l map[string]any) string {
	last, _ := l["last"].(map[string]any)
	if last == nil {
		return ""
	}
	return fmt.Sprint(last["kind"], ":", last["text"])
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到 %s", path)
	return 0
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !launchProcExists(pid) || isZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("程序 %d 還在", pid)
}

// isZombie：已經結束、還沒被收屍（測試裡 sleep 的父程序可能晚一點才收）
func isZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func procState(pid int) string {
	out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

func TestLaunchElectronDebugRunExits(t *testing.T) {
	dir := electronProject(t)
	_, tv, id, target, _ := launchHarness(t, dir)
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "start"})
	l := tv.waitLaunch("結束", func(l map[string]any) bool { return l["phase"] == "idle" && l["last"] != nil })
	if lastKind(l) != "exited:已結束（結束碼 3）" {
		t.Fatalf("結束的狀態：%v", l["last"])
	}
	tv.send(map[string]any{"cmd": "launchLog", "conversationId": id, "targetId": target, "epoch": -1, "after": 0})
	m := tv.wait("輸出", func(m map[string]any) bool { return m["type"] == "launchLog" })
	lines := fmt.Sprint(m["lines"])
	if !strings.Contains(lines, "> npm run start") || !strings.Contains(lines, "npm run start") || !strings.Contains(lines, "到 stderr") || m["reset"] != true {
		t.Fatalf("輸出：%v", m)
	}
}

func TestLaunchDotNetBuildFailsWithSummary(t *testing.T) {
	dir := dotnetProject(t)
	_ = os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644)
	_, tv, id, target, _ := launchHarness(t, dir)
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "start"})
	tv.waitLaunch("建置中", func(l map[string]any) bool { return l["phase"] == "building" })
	l := tv.waitLaunch("建置失敗", func(l map[string]any) bool { return l["last"] != nil })
	if lastKind(l) != "buildFailed:Program.cs(3,5): CS1002 ; expected" {
		t.Fatalf("建置失敗的說明：%v", l["last"])
	}
}

func TestLaunchDotNetBuildThenRunStopKillsTree(t *testing.T) {
	dir := dotnetProject(t)
	_ = os.WriteFile(filepath.Join(dir, "sleep"), nil, 0o644)
	_, tv, id, target, _ := launchHarness(t, dir)
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "start"})
	tv.waitLaunch("執行中", func(l map[string]any) bool { return l["phase"] == "running" && l["runMode"] == "debug" })
	child := readPid(t, filepath.Join(dir, "child"))
	// 暫停：整個群組停住
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "pause"})
	tv.waitLaunch("暫停中", func(l map[string]any) bool { return l["phase"] == "paused" })
	if st := procState(child); !strings.HasPrefix(st, "T") {
		t.Fatalf("暫停後子程序的狀態：%q", st)
	}
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "start"}) // 暫停中的 start＝繼續
	tv.waitLaunch("繼續", func(l map[string]any) bool { return l["phase"] == "running" })
	if st := procState(child); strings.HasPrefix(st, "T") {
		t.Fatalf("繼續後子程序還停著：%q", st)
	}
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "stop"})
	l := tv.waitLaunch("已停止", func(l map[string]any) bool { return l["phase"] == "idle" && l["last"] != nil })
	if lastKind(l) != "stopped:已停止" {
		t.Fatalf("停止的狀態：%v", l["last"])
	}
	// 停止＝結束整個程序樹（腳本開的 sleep 也結束）
	waitGone(t, child)
}

func TestLaunchRestartRebuilds(t *testing.T) {
	dir := dotnetProject(t)
	_ = os.WriteFile(filepath.Join(dir, "sleep"), nil, 0o644)
	_, tv, id, target, _ := launchHarness(t, dir)
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "start"})
	first := tv.waitLaunch("執行中", func(l map[string]any) bool { return l["phase"] == "running" })
	child := readPid(t, filepath.Join(dir, "child"))
	_ = os.Remove(filepath.Join(dir, "child"))
	tv.clear()
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "restart"})
	tv.waitLaunch("重新建置", func(l map[string]any) bool { return l["phase"] == "building" })
	again := tv.waitLaunch("再次執行", func(l map[string]any) bool { return l["phase"] == "running" })
	if again["logEpoch"].(float64) <= first["logEpoch"].(float64) {
		t.Fatalf("重新執行要換 epoch：%v → %v", first["logEpoch"], again["logEpoch"])
	}
	waitGone(t, child)
	child2 := readPid(t, filepath.Join(dir, "child"))
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "stop"})
	waitGone(t, child2)
}

func TestLaunchReleaseDetachedReattach(t *testing.T) {
	dir := electronProject(t)
	_ = os.WriteFile(filepath.Join(dir, "sleep"), nil, 0o644)
	h, tv, id, target, state := launchHarness(t, dir)
	tv.send(map[string]any{"cmd": "launchSelect", "conversationId": id, "targetId": target, "mode": "release"})
	tv.waitLaunch("Release 模式", func(l map[string]any) bool { return l["mode"] == "release" })
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "start"})
	tv.waitLaunch("執行中", func(l map[string]any) bool { return l["phase"] == "running" && l["runMode"] == "release" })
	pid := readPid(t, filepath.Join(dir, "pid"))
	child := readPid(t, filepath.Join(dir, "child"))
	t.Cleanup(func() { _ = exec.Command("kill", "-9", "-"+strconv.Itoa(pid)).Run() })
	// 記進 runs.json（PID＋開始時間）
	var runs launchRunsFile
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(state, launchStateFileName)); err == nil && jsonUnmarshal(b, &runs) == nil && len(runs.Runs) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].Processes[0].Pid != pid || runs.Runs[0].Processes[0].Created == 0 || !launchIsLogFileName(runs.Runs[0].LogFile) {
		t.Fatalf("runs.json：%+v", runs)
	}
	// 輸出寫進輸出檔，這裡跟著讀
	tv.send(map[string]any{"cmd": "launchLog", "conversationId": id, "targetId": target, "epoch": -1, "after": 0})
	deadline = time.Now().Add(5 * time.Second)
	for {
		m := tv.wait("輸出", func(m map[string]any) bool { return m["type"] == "launchLog" })
		if strings.Contains(fmt.Sprint(m["lines"]), "到 stderr") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("輸出檔的內容沒讀到：%v", m)
		}
		tv.clear()
		time.Sleep(100 * time.Millisecond)
		tv.send(map[string]any{"cmd": "launchLog", "conversationId": id, "targetId": target, "epoch": -1, "after": 0})
	}

	// agent 關閉：Release 模式的程式不結束
	h.mu.Lock()
	h.stopLaunchAll()
	h.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if !launchProcExists(pid) || !launchProcExists(child) {
		t.Fatal("agent 關閉後脫鉤的程式不該結束")
	}

	// 新的中樞接回：照樣執行中、第一行是接回的說明
	h2, tv2 := newTestHub(t)
	h2.mu.Lock()
	h2.attachLaunch(fsLaunchScanner{}, unixLaunchStarter{}, state)
	h2.mu.Unlock()
	id2 := tv2.newDraft()
	tv2.send(map[string]any{"cmd": "setFolder", "conversationId": id2, "path": dir})
	l := tv2.waitLaunch("接回", func(l map[string]any) bool { return l["phase"] == "running" && l["runMode"] == "release" })
	tv2.send(map[string]any{"cmd": "launchLog", "conversationId": id2, "targetId": target, "epoch": -1, "after": 0})
	m := tv2.wait("接回的輸出", func(m map[string]any) bool { return m["type"] == "launchLog" })
	lines, _ := m["lines"].([]any)
	if len(lines) == 0 || lines[0] != launchReattachedNote {
		t.Fatalf("接回的輸出：%v", m)
	}
	_ = l
	tv2.send(map[string]any{"cmd": "launchAction", "conversationId": id2, "targetId": target, "action": "stop"})
	l = tv2.waitLaunch("已停止", func(l map[string]any) bool { return l["phase"] == "idle" && l["last"] != nil })
	if lastKind(l) != "stopped:已停止" {
		t.Fatalf("停止接回的程式：%v", l["last"])
	}
	waitGone(t, pid)
	waitGone(t, child)
	// 結束了就忘掉：runs.json 刪掉
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(state, launchStateFileName)); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("程式結束後 runs.json 應該刪掉")
}

// 真的讓 agent 的程序結束：Release 模式開的程式活下來（setsid），Debug 模式開的整個群組跟著結束（看門的發現管線斷了）
func TestLaunchSurvivesAgentExit(t *testing.T) {
	fakeLaunchTools(t)
	relDir := electronProject(t)
	dbgDir := electronProject(t)
	for _, d := range []string{relDir, dbgDir} {
		_ = os.WriteFile(filepath.Join(d, "sleep"), nil, 0o644)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLaunchHelperAgent$")
	cmd.Env = append(os.Environ(), "MYRIAGENT_LAUNCH_HELPER=1", "HELPER_REL="+relDir, "HELPER_DBG="+dbgDir, "HELPER_LOG="+filepath.Join(t.TempDir(), "r.log"))
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if sc.Text() == "ready" {
			break
		}
	}
	relPid := readPid(t, filepath.Join(relDir, "pid"))
	dbgChild := readPid(t, filepath.Join(dbgDir, "child"))
	t.Cleanup(func() { _ = exec.Command("kill", "-9", "-"+strconv.Itoa(relPid)).Run() })
	// 模擬 agent 當掉
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	waitGone(t, dbgChild)
	time.Sleep(300 * time.Millisecond)
	if !launchProcExists(relPid) {
		t.Fatal("Release 模式的程式應該在 agent 結束後繼續執行")
	}
	if sid, err := getsid(relPid); err != nil || sid != relPid {
		t.Fatalf("脫鉤的程式要自己一個工作階段：sid=%d err=%v", sid, err)
	}
}

// TestLaunchHelperAgent：給 TestLaunchSurvivesAgentExit 當「agent」的子程序（平常略過）
func TestLaunchHelperAgent(t *testing.T) {
	if os.Getenv("MYRIAGENT_LAUNCH_HELPER") != "1" {
		t.Skip("只在 TestLaunchSurvivesAgentExit 裡當子程序")
	}
	var s unixLaunchStarter
	rel := launchRunCommand(*launchFromPackageJSON(filepath.Join(os.Getenv("HELPER_REL"), "package.json"), "package.json", `{"dependencies":{"electron":"1"},"scripts":{"start":"x"}}`), launchRelease)
	if _, err := s.Run(rel, os.Getenv("HELPER_LOG"), func(string) {}); err != nil {
		t.Fatal(err)
	}
	dbg := launchRunCommand(*launchFromPackageJSON(filepath.Join(os.Getenv("HELPER_DBG"), "package.json"), "package.json", `{"dependencies":{"electron":"1"},"scripts":{"start":"x"}}`), launchDebug)
	if _, err := s.RunAttached(dbg, func(string) {}); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

func TestLaunchStartFailedWhenToolMissing(t *testing.T) {
	dir := electronProject(t)
	_, tv, id, target, _ := launchHarness(t, dir)
	t.Setenv("PATH", t.TempDir())
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": target, "action": "start"})
	l := tv.waitLaunch("無法啟動", func(l map[string]any) bool { return l["last"] != nil })
	if lastKind(l) != "startFailed:無法啟動：找不到 npm（不在 PATH 上）" {
		t.Fatalf("找不到 npm：%v", l["last"])
	}
}

func TestLaunchIgnoresUnknownTarget(t *testing.T) {
	dir := electronProject(t)
	h, tv, id, _, _ := launchHarness(t, dir)
	tv.send(map[string]any{"cmd": "launchAction", "conversationId": id, "targetId": launchTargetID("/etc/passwd"), "action": "start"})
	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	n := len(h.launch.runs)
	h.mu.Unlock()
	if n != 0 {
		t.Fatal("不是這個資料夾列出的目標不該執行")
	}
}
