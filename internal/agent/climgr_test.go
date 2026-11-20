package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCLIVersionRules(t *testing.T) {
	if v := parseCLIVersion("2.1.295 (Claude Code)"); v != "2.1.295" {
		t.Fatal(v)
	}
	if v := parseCLIVersion("codex-cli 0.163.0-alpha.3"); v != "0.163.0-alpha.3" {
		t.Fatal(v)
	}
	for _, c := range []struct {
		a, b string
		want int
	}{{"1.2.3", "1.2.10", -1}, {"1.0.0", "1.0.0-beta", 1}, {"2.0.0", "2.0.0", 0}, {"x", "1.0.0", -1}} {
		if got := compareVersions(c.a, c.b); (got > 0) != (c.want > 0) || (got < 0) != (c.want < 0) {
			t.Errorf("%s vs %s：%d", c.a, c.b, got)
		}
	}
	if !isNewerVersion("1.0.1", "1.0.0") || isNewerVersion("1.0.0", "1.0.0") || isNewerVersion("", "1.0.0") {
		t.Fatal("isNewerVersion")
	}
	if v := parseLatestJSON(`{"name":"@openai/codex","version":"0.170.0"}`); v != "0.170.0" {
		t.Fatal(v)
	}
	home := "/Users/u"
	for _, c := range []struct{ p, path, real, want string }{
		{"Codex", "/opt/homebrew/bin/codex", "/opt/homebrew/lib/node_modules/@openai/codex/bin/codex.js", "npm"},
		{"Claude", "/Users/u/.local/bin/claude", "/Users/u/.local/share/claude/versions/2.1.300", "native"},
		{"Claude", "/opt/homebrew/bin/claude", "/opt/homebrew/Caskroom/claude-code/2.1/claude", "other"},
		{"Grok", "/Users/u/.grok/bin/grok", "/Users/u/.grok/bin/grok", "other"},
	} {
		if got := installKindOf(c.p, c.path, c.real, home); got != c.want {
			t.Errorf("%s %s：%s", c.p, c.real, got)
		}
	}
}

func TestClaudeUpdateJudgement(t *testing.T) {
	r := parseClaudeUpdate(0, "Current version: 2.1.295\nChecking for updates\n\x1b[32mSuccessfully updated from 2.1.295 to version 2.1.297\x1b[0m", "")
	if r.kind != updateUpdated || r.from != "2.1.295" || r.to != "2.1.297" {
		t.Fatalf("%+v", r)
	}
	// 回報成功卻沒換檔：算失敗
	if j := judgeSelfUpdate(r, "2.1.295", "2.1.295"); j.switched || !strings.Contains(j.err, "仍是 2.1.295") {
		t.Fatalf("%+v", j)
	}
	if j := judgeSelfUpdate(r, "2.1.295", "2.1.297"); !j.switched || j.err != "" {
		t.Fatalf("%+v", j)
	}
	busy := parseClaudeUpdate(0, "Another Claude process (PID 4242) is currently running. Please try again in a moment.", "")
	if j := judgeSelfUpdate(busy, "1.0.0", "1.0.0"); !j.retryLater || !strings.Contains(j.note, "PID 4242") {
		t.Fatalf("%+v", j)
	}
	fail := parseClaudeUpdate(1, "Current version: 1.0.0", "Error: Failed to install native update")
	if j := judgeSelfUpdate(fail, "1.0.0", "1.0.0"); j.err != "更新失敗：Error: Failed to install native update" {
		t.Fatalf("%+v", j)
	}
	if j := judgeSelfUpdate(fail, "1.0.0", "1.0.1"); !j.switched {
		t.Fatal("失敗但版本其實換了就算換版")
	}
	if r := parseClaudeUpdate(0, "Claude Code is up to date (2.1.300)", ""); r.kind != updateUpToDate || r.to != "2.1.300" {
		t.Fatalf("%+v", r)
	}
	if d := describeCLIFailure("", "npm warn deprecated x\nnpm error code EBUSY\nnpm error EBUSY: resource busy or locked, rename\nnpm error A complete log"); d != "npm error EBUSY: resource busy or locked, rename" {
		t.Fatal(d)
	}
}

// cliEnv：CLI 管理的測試環境：四種 CLI 都是假的（版本記在檔案裡、update 會改它）、最新版由假的 fetchText 回
func newCLIEnv(t *testing.T, latest string) (*acctEnv, string) {
	vf := filepath.Join(t.TempDir(), "version")
	_ = os.WriteFile(vf, []byte("1.0.0"), 0o600)
	e := newAcctEnv(t, fakeSpec{VersionFile: vf, UpdateTo: "1.1.0", UpdateOutput: "Successfully updated from 1.0.0 to version 1.1.0"}, nil)
	e.h.mu.Lock()
	e.h.ar().fetchText = func(url string) (string, error) {
		if strings.Contains(url, "x.ai") {
			return latest, nil
		}
		return `{"version":"` + latest + `"}`, nil
	}
	e.h.mu.Unlock()
	return e, vf
}

func (e *acctEnv) cliView(p string) CliView {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	return e.h.cliViews()[p]
}

// 立即檢查：查到最新版、updateAvailable；自動更新預設關，不會跑任何更新
func TestCLICheckWithoutAutoUpdate(t *testing.T) {
	e, _ := newCLIEnv(t, "1.1.0")
	e.tv.send(map[string]any{"cmd": "cliCheck"})
	e.tv.wait("檢查完", func(m map[string]any) bool {
		if m["type"] != "cli" {
			return false
		}
		for _, p := range providerKeys {
			if m["cli"].(map[string]any)[p].(map[string]any)["checkedAt"] == nil {
				return false
			}
		}
		return true
	})
	v := e.cliView("Claude")
	if !v.Installed || *v.Version != "1.0.0" || v.Latest == nil || *v.Latest != "1.1.0" || !v.UpdateAvailable || v.AutoUpdate || v.LastCheckedAt == nil {
		t.Fatalf("%+v", v)
	}
	if g := e.cliView("Grok"); g.Latest == nil || *g.Latest != "1.1.0" {
		t.Fatalf("Grok 的 stable 頻道是純文字：%+v", g)
	}
	if calls := e.called("update"); len(calls) != 0 {
		t.Fatalf("自動更新預設關，不該更新：%v", calls)
	}
	// 查不到最新版：記下原因，網頁不顯示「已是最新版」
	e.h.mu.Lock()
	e.h.ar().fetchText = func(string) (string, error) { return "", errors.New("離線") }
	e.h.mu.Unlock()
	e.h.checkCLI("Codex", true)
	if c := e.cliView("Codex"); c.LatestError == nil || *c.LatestError != "無法查詢最新版本：離線" || *c.Latest != "1.1.0" {
		t.Fatalf("%+v", c)
	}
}

// 開了自動更新：查到新版就更新（跑 CLI 自己的 update），版本換了記成 app 的換版
func TestCLIAutoUpdateWhenEnabled(t *testing.T) {
	e, _ := newCLIEnv(t, "1.1.0")
	e.tv.send(map[string]any{"cmd": "setting", "key": "autoUpdateCli", "value": true})
	e.h.checkCLI("Claude", true)
	e.tv.wait("更新完", func(m map[string]any) bool {
		if m["type"] != "cli" {
			return false
		}
		c := m["cli"].(map[string]any)["Claude"].(map[string]any)
		return c["busy"] == false && c["version"] == "1.1.0"
	})
	v := e.cliView("Claude")
	if v.LastChange == nil || v.LastChange.Origin != "app" || *v.LastChange.From != "1.0.0" || v.LastChange.To != "1.1.0" || v.UpdateAvailable || v.Error != nil {
		t.Fatalf("%+v %+v", v, v.LastChange)
	}
	if calls := e.called("update"); len(calls) != 1 || !strings.Contains(calls[0], "DISABLE_AUTOUPDATER=1") || strings.Contains(calls[0], "ANTHROPIC_API_KEY") {
		t.Fatalf("%v", calls)
	}
}

// 更新沒換到版本（CLI 說已更新、執行檔沒變）：顯示原因
func TestCLIUpdateNoEffect(t *testing.T) {
	e, vf := newCLIEnv(t, "1.1.0")
	e.spec(fakeSpec{VersionFile: vf, UpdateOutput: "Successfully updated from 1.0.0 to version 1.1.0"})
	e.h.checkCLI("Claude", false)
	e.tv.send(map[string]any{"cmd": "cliUpdate", "provider": "Claude"})
	e.tv.wait("更新失敗", func(m map[string]any) bool {
		if m["type"] != "cli" {
			return false
		}
		c := m["cli"].(map[string]any)["Claude"].(map[string]any)
		return c["busy"] == false && c["error"] != nil
	})
	if v := e.cliView("Claude"); !strings.Contains(*v.Error, "但執行檔仍是 1.0.0") {
		t.Fatal(*v.Error)
	}
}

// npm 安裝的 CLI：有對話在執行時不換檔（npm 會刪掉執行中的檔案）；沒有 npm 時說明
func TestCLINpmUpdateGuards(t *testing.T) {
	e, _ := newCLIEnv(t, "1.1.0")
	e.h.mu.Lock()
	e.h.clis["Claude"].cli.installKind = "npm"
	c := &Conv{Meta: ConvMeta{ID: "c1", AccountID: "local-claude", Provider: "Claude"}}
	c.init()
	c.running = true
	e.h.convs["c1"] = c
	e.h.startCLIUpdate(e.tv.v, "Claude", false)
	msg := e.h.clis["Claude"].cli.actionError
	c.running = false
	e.h.startCLIUpdate(e.tv.v, "Claude", false)
	msg2 := e.h.clis["Claude"].cli.actionError
	busy := e.h.clis["Claude"].cli.busy
	e.h.mu.Unlock()
	if !strings.Contains(msg, "1 個") || !strings.Contains(msg2, "找不到 npm") || busy {
		t.Fatalf("%q %q %v", msg, msg2, busy)
	}
	if len(e.called("update")) != 0 || len(e.called("install")) != 0 {
		t.Fatal("不該跑任何更新")
	}
	// 有 npm（假的）：npm install -g 套件@latest
	e.h.mu.Lock()
	e.h.ar().findNpm = func() string { return os.Args[0] }
	e.h.startCLIUpdate(e.tv.v, "Claude", false)
	e.h.mu.Unlock()
	e.tv.wait("npm 更新完", func(m map[string]any) bool {
		if m["type"] != "cli" {
			return false
		}
		x := m["cli"].(map[string]any)["Claude"].(map[string]any)
		return x["busy"] == false && x["version"] == "1.1.0"
	})
	if calls := e.called("install -g @anthropic-ai/claude-code@latest"); len(calls) != 1 {
		t.Fatal(e.calls())
	}
}

// 安裝：Claude 開終端機跑官方安裝指令；沒有終端機時告訴使用者要執行什麼
func TestCLIInstall(t *testing.T) {
	e, _ := newCLIEnv(t, "1.1.0")
	e.h.mu.Lock()
	e.h.ar().console = noConsole{}
	e.h.clis["Claude"].path = ""
	e.h.startCLIInstall(e.tv.v, "Claude")
	msg := e.h.clis["Claude"].cli.actionError
	e.h.startCLIInstall(e.tv.v, "Codex")
	msg2 := e.h.clis["Codex"].cli.actionError
	e.h.mu.Unlock()
	if !strings.Contains(msg, "curl -fsSL https://claude.ai/install.sh | bash") || !strings.Contains(msg2, "找不到 npm") {
		t.Fatalf("%q %q", msg, msg2)
	}
}

// 終端機的兩個小腳本：環境只寫差異、API Key 一律拿掉、結束碼傳回來；等待的程序被結束時視窗裡的指令跟著結束
func TestConsoleScripts(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("沒有 sh")
	}
	set, unset := consoleChanges(map[string]string{"A": "1", "B": "2", "OPENAI_API_KEY": "k", "bad-name": "x"},
		map[string]string{"A": "1", "B": "3", "C": "it's", "bad-name": "y"}, []string{"ANTHROPIC_API_KEY"})
	if len(set) != 2 || set[0] != [2]string{"B", "3"} || set[1] != [2]string{"C", "it's"} {
		t.Fatalf("set：%v", set)
	}
	if strings.Join(unset, ",") != "ANTHROPIC_API_KEY,OPENAI_API_KEY" {
		t.Fatalf("unset：%v", unset)
	}
	run := func(cmd string, cancelAfter time.Duration) (int, string) {
		dir := t.TempDir()
		folder := filepath.Join(dir, "c")
		_ = os.MkdirAll(folder, 0o700)
		out := filepath.Join(dir, "out")
		_ = os.WriteFile(filepath.Join(folder, consoleRunFile), []byte(consoleRunScript(folder, "測試", "/bin/sh",
			[]string{"-c", "{ " + cmd + "; } > " + shellQuote(out)}, [][2]string{{"C", "it's"}}, []string{"OPENAI_API_KEY"}, dir)), 0o700)
		// 假的終端機：在背景直接跑 run.command
		_ = os.WriteFile(filepath.Join(folder, consoleWatchFile), []byte(consoleWatchScript(folder, `( /bin/sh "$D/`+consoleRunFile+`" >/dev/null 2>&1 & )`)), 0o700)
		c := exec.Command("/bin/sh", filepath.Join(folder, consoleWatchFile))
		c.Env = append(os.Environ(), "OPENAI_API_KEY=k")
		// 自己一個程序群組：run.command 取消時的 kill -TERM 0 不能打到測試程序（真的終端機裡它在終端機自己的群組）
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		p := newProcWaiter(c)
		ctx := context.Background()
		if cancelAfter > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, cancelAfter)
			defer cancel()
		}
		code, _ := p.Wait(ctx)
		time.Sleep(300 * time.Millisecond)
		b, _ := os.ReadFile(out)
		if _, err := os.Stat(folder); cancelAfter == 0 && !os.IsNotExist(err) {
			t.Error("暫存資料夾要清掉")
		}
		return code, string(b)
	}
	code, out := run(`echo "$C|${OPENAI_API_KEY:-無}"; exit 3`, 0)
	if code != 3 || strings.TrimSpace(out) != "it's|無" {
		t.Fatalf("結束碼 %d、輸出 %q", code, out)
	}
	// 取消：等待的程序被結束，視窗裡的指令也結束（不會寫出 done 之後的東西）
	code, out = run(`sleep 3; echo 還在`, 500*time.Millisecond)
	time.Sleep(3 * time.Second)
	if b := out; strings.Contains(b, "還在") {
		t.Fatalf("取消後指令還在跑：%q（%d）", b, code)
	}
	if line := terminalLaunchLine("linux", func(n string) (string, error) {
		if n == "konsole" {
			return "/usr/bin/konsole", nil
		}
		return "", errors.New("沒有")
	}); !strings.Contains(line, "'/usr/bin/konsole' -e /bin/sh") {
		t.Fatal(line)
	}
	if terminalLaunchLine("linux", func(string) (string, error) { return "", errors.New("沒有") }) != "" {
		t.Fatal("沒有終端機")
	}
	if !strings.Contains(terminalLaunchLine("darwin", nil), "open -a Terminal") {
		t.Fatal("macOS")
	}
	if m := manualCommand("/x/claude", []string{"auth", "login"}, [][2]string{{"CLAUDE_CONFIG_DIR", "/a b"}, {"FOO", "1"}}); m != "CLAUDE_CONFIG_DIR='/a b' '/x/claude' 'auth' 'login'" {
		t.Fatal(m)
	}
}

func TestTokenTallyAndSegments(t *testing.T) {
	dir := t.TempDir()
	tl := &tokenTally{path: filepath.Join(dir, "usage-tokens.json")}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	tl.add("a1", now.AddDate(0, 0, -40), tokenDelta{Input: 999})
	tl.add("a1", now.AddDate(0, 0, -3), tokenDelta{Input: 100, Cached: 50, Output: 10})
	tl.add("a1", now, tokenDelta{Input: 1, Output: 2})
	if d := tl.sum("a1", now, 1); d.Input != 1 || d.Output != 2 {
		t.Fatalf("%+v", d)
	}
	if d := tl.sum("a1", now, 7); d.Input != 101 || d.Cached != 50 {
		t.Fatalf("%+v", d)
	}
	if _, ok := tl.Days[dayKey(now.AddDate(0, 0, -40))]; ok {
		t.Fatal("超過 30 天的要刪")
	}
	tl.flush()
	tl2 := &tokenTally{path: tl.path}
	if d := tl2.sum("a1", now, 30); d.Input != 101 {
		t.Fatalf("重新載入：%+v", d)
	}
	if d := tokenIncrement(tokenDelta{Input: 10, Output: 5}, tokenDelta{Input: 8, Output: 9}); d.Input != 0 || d.Output != 4 {
		t.Fatal(d)
	}

	// 每週 5%：開始追蹤時在一段中間＝不完整；跨過邊界結一筆；一次跳過兩個邊界結成一筆；換週丟掉進行中的那段
	w := func(p float64, resets int64) usageWindow {
		return usageWindow{Key: "weekly", Label: "每週", Percent: p, ResetsAt: resets}
	}
	o, rec := observeSegment(nil, "a1", w(3, 1000), 1)
	if rec != nil || o.Complete || o.Bucket != 0 {
		t.Fatalf("%+v", o)
	}
	addSegmentTokens(o, "opus", tokenDelta{Input: 1000, Output: 100})
	o, rec = observeSegment(o, "a1", w(6, 1000), 2)
	if rec == nil || rec.Complete || rec.FromBucket != 0 || rec.ToBucket != 5 || rec.Models["opus"].Input != 1000 || !o.Complete || o.Bucket != 5 {
		t.Fatalf("%+v %+v", rec, o)
	}
	addSegmentTokens(o, "opus", tokenDelta{Input: 3000, Output: 300})
	addSegmentTokens(o, "haiku", tokenDelta{Input: 10})
	o, rec = observeSegment(o, "a1", w(16.2, 1000), 3)
	if rec == nil || !rec.Complete || rec.ToBucket != 15 || dominantModel(rec.Models) != "opus" {
		t.Fatalf("%+v", rec)
	}
	o2, rec := observeSegment(o, "a1", w(1, 1000+segmentResetShift+1), 4)
	if rec != nil || o2 == o || o2.Bucket != 0 || o2.Complete {
		t.Fatalf("換週：%+v", o2)
	}
	if !isWeeklyWindow(usageWindow{Key: "codex:10080"}) || isWeeklyWindow(usageWindow{Key: "session", Label: "5 小時"}) {
		t.Fatal("isWeeklyWindow")
	}
	sum := summarizeSegments([]*segmentRecord{
		{AccountID: "a1", WindowKey: "weekly", StartPercent: 5, EndPercent: 10, Complete: true, EndedAt: 1, Models: map[string]*segmentTokens{"opus": {Input: 900, Output: 100}}},
		{AccountID: "a1", WindowKey: "weekly", StartPercent: 10, EndPercent: 20, Complete: true, EndedAt: 2, Models: map[string]*segmentTokens{"opus": {Input: 3000}}},
		{AccountID: "a1", WindowKey: "weekly", StartPercent: 0, EndPercent: 5, Complete: false, Models: map[string]*segmentTokens{"opus": {Input: 1}}},
	})
	if len(sum) != 1 || sum[0].Records != 2 || sum[0].Percent != 15 || per5(sum[0].Tokens.Input+sum[0].Tokens.Output, sum[0].Percent) != 1333 ||
		sum[0].MinPer5 != 1000 || sum[0].MaxPer5 != 1500 {
		t.Fatalf("%+v", sum[0])
	}
}

// 中樞：用量快照經觀察者結成分段紀錄、usageSegments 指令回給要過的視窗；token 回報記進側欄計數
func TestUsageSegmentsHub(t *testing.T) {
	e := newAcctEnv(t, fakeSpec{}, nil)
	e.tv.send(map[string]any{"cmd": "usageSegments"})
	e.tv.wait("分段", func(m map[string]any) bool { return m["type"] == "usageSegments" })
	e.h.mu.Lock()
	a := e.h.account("local-claude")
	e.h.storeUsage(a, &usageSnapshot{FetchedAt: nowMs(), Source: "api", Windows: []usageWindow{{Key: "weekly", Label: "每週", Percent: 0}}})
	e.h.recordTokenUsage("local-claude", "claude-opus", tokenDelta{Input: 500, Output: 50})
	e.h.storeUsage(a, &usageSnapshot{FetchedAt: nowMs(), Source: "api", Windows: []usageWindow{{Key: "weekly", Label: "每週", Percent: 5.5}}})
	e.h.mu.Unlock()
	m := e.tv.wait("結了一筆", func(m map[string]any) bool {
		if m["type"] != "usageSegments" {
			return false
		}
		return m["usageSegments"].(map[string]any)["total"] == 1.0
	})
	us := m["usageSegments"].(map[string]any)
	rec := us["records"].([]any)[0].(map[string]any)
	if rec["model"] != "claude-opus" || rec["complete"] != true || us["summary"].([]any)[0].(map[string]any)["per5"].(map[string]any)["input"] != 455.0 {
		t.Fatalf("%v", us)
	}
	tok := e.tv.wait("token", func(m map[string]any) bool { return m["type"] == "tokens" })
	today := tok["tokens"].(map[string]any)["accounts"].(map[string]any)["local-claude"].(map[string]any)["today"].(map[string]any)
	if today["input"] != 500.0 || today["output"] != 50.0 {
		t.Fatal(today)
	}
	b, _ := os.ReadFile(filepath.Join(e.h.root, "usage-segments", "records.jsonl"))
	if strings.Count(string(b), "\n") != 1 || !strings.Contains(string(b), `"windowKey":"weekly"`) {
		t.Fatal(string(b))
	}
}
