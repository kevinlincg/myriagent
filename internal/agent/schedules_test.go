package agent

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kevinlincg/myriagent/internal/schedulecli"
	"github.com/kevinlincg/myriagent/internal/schedules"
)

// fakeScheduleReader 假的讀取器：回測試給的排程，記下快速操作（不碰這台真的 launchd、crontab、systemd）
type fakeScheduleReader struct {
	mu      sync.Mutex
	tasks   []schedules.TaskInfo
	actions []string
}

func (f *fakeScheduleReader) Scan(ctx context.Context) schedules.Scan {
	f.mu.Lock()
	defer f.mu.Unlock()
	env := schedules.Environment{UserName: "me", Variables: map[string]string{"HOME": "/Users/me"}}
	return schedules.Scan{ScannedAt: time.Now(), Tasks: append([]schedules.TaskInfo{}, f.tasks...), Env: env}
}

func (f *fakeScheduleReader) set(tasks ...schedules.TaskInfo) {
	f.mu.Lock()
	f.tasks = tasks
	f.mu.Unlock()
}

func (f *fakeScheduleReader) record(a string) error {
	f.mu.Lock()
	f.actions = append(f.actions, a)
	f.mu.Unlock()
	return nil
}

func (f *fakeScheduleReader) Run(p string) error  { return f.record("run " + p) }
func (f *fakeScheduleReader) Stop(p string) error { return f.record("stop " + p) }
func (f *fakeScheduleReader) SetEnabled(p string, on bool) error {
	if on {
		return f.record("enable " + p)
	}
	return f.record("disable " + p)
}

// fakeClock 可以往前撥的時鐘
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func job(path, label string, lastRun *time.Time, code int) schedules.TaskInfo {
	start := schedules.BaseDate().Add(9 * time.Hour)
	tr := schedules.NewTrigger(schedules.TriggerDaily)
	tr.Start = &start
	return schedules.TaskInfo{Path: path, DisplayName: label, XML: "<plist>" + label + "</plist>", State: schedules.StateReady, Enabled: true,
		LastRun: lastRun, LastResult: code, Definition: &schedules.Definition{Enabled: true, Triggers: []schedules.Trigger{tr}, LogonType: schedules.AgentLogonType, RunAs: "me",
			Actions: []schedules.Action{{Kind: "Exec", Command: "/Users/me/repo/" + label + ".sh"}}}}
}

func attachFake(t *testing.T, h *Hub) (*fakeScheduleReader, *fakeClock) {
	t.Helper()
	r := &fakeScheduleReader{}
	clock := &fakeClock{now: time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)}
	h.mu.Lock()
	h.attachSchedules(r, filepath.Join(h.root, "schedules"), clock.Now)
	h.mu.Unlock()
	t.Cleanup(h.stopSchedules)
	return r, clock
}

func scheduleEntries(m map[string]any) []map[string]any {
	sv, _ := m["schedules"].(map[string]any)
	list, _ := sv["entries"].([]any)
	var out []map[string]any
	for _, x := range list {
		out = append(out, x.(map[string]any))
	}
	return out
}

func entryOf(m map[string]any, path string) map[string]any {
	if m["type"] != "schedules" {
		return nil
	}
	for _, e := range scheduleEntries(m) {
		if e["path"] == path {
			return e
		}
	}
	return nil
}

// waitState 等到中樞的狀態符合條件（最多 10 秒；條件在鎖裡看）
func waitState(t *testing.T, h *Hub, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		done := ok()
		h.mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

// conversationWithReply：開一個正式對話（送過一則、這一輪結束了）
func conversationWithReply(t *testing.T, tv *testView) string {
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "你好"})
	tv.wait("一輪結束", func(m map[string]any) bool {
		hd, _ := m["header"].(map[string]any)
		return m["type"] == "header" && m["conversationId"] == id && hd["running"] == false && hd["lastTurn"] != nil
	})
	return id
}

func TestSchedulesNotAttachedIsNull(t *testing.T) {
	_, tv := newTestHub(t)
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	m := tv.wait("init", func(m map[string]any) bool { return m["type"] == "init" })
	if m["schedules"] != nil {
		t.Fatalf("沒掛讀取器時 schedules 要是 null：%v", m["schedules"])
	}
}

func TestSchedulesScanRecordsAgentChangeAndAutoDiagnoses(t *testing.T) {
	h, tv := newTestHub(t)
	r, clock := attachFake(t, h)
	convID := conversationWithReply(t, tv)
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	m := tv.wait("init 有排程頁", func(m map[string]any) bool { return m["type"] == "init" && m["schedules"] != nil })
	if sv := m["schedules"].(map[string]any); sv["loaded"] != false || sv["autoDiagnose"] != true {
		t.Fatalf("還沒讀過：%v", sv)
	}

	// 第一次讀：只建基準
	r.set(job("/Users/me/Library/LaunchAgents/com.other.plist", "com.other", nil, schedules.ResultHasNotRun))
	tv.send(map[string]any{"cmd": "scheduleWatch", "on": true})
	tv.wait("讀到第一次", func(m map[string]any) bool {
		return entryOf(m, "/Users/me/Library/LaunchAgents/com.other.plist") != nil
	})

	// 對話的工具指令碰到排程：之後讀到的新排程記成它建立的
	path := "/Users/me/Library/LaunchAgents/com.me.job.plist"
	h.mu.Lock()
	c := h.convs[convID]
	it := c.addItem("Tool")
	it.Title = "launchctl bootstrap gui/501 ~/Library/LaunchAgents/com.me.job.plist"
	h.runToolStartHooks(c, it)
	// 代理建好了排程，這一輪結束：立刻重讀
	r.set(job("/Users/me/Library/LaunchAgents/com.other.plist", "com.other", nil, schedules.ResultHasNotRun), job(path, "com.me.job", nil, schedules.ResultHasNotRun))
	h.runTurnEndHooks(c, turnCompleted)
	h.mu.Unlock()
	msg := tv.wait("代理建立的排程", func(m map[string]any) bool {
		e := entryOf(m, path)
		return e != nil && e["origin"] == "agent"
	})
	e := entryOf(msg, path)
	ch := e["changes"].([]any)[0].(map[string]any)
	prov := e["provenance"].(map[string]any)
	if ch["kind"] != "created" || ch["conversationId"] != convID || prov["conversationId"] != convID || e["managed"] != true {
		t.Fatalf("變更與出處要記成這個對話：%v / %v", ch, prov)
	}
	// 第一次讀只建基準：原本就在的不記成「建立」
	if other := entryOf(msg, "/Users/me/Library/LaunchAgents/com.other.plist"); len(other["changes"].([]any)) != 0 || other["origin"] == "agent" {
		t.Fatalf("基準：%v", other)
	}

	// 列入管理的排程失敗：自動請代理診斷（開管理對話、規劃模式、送出）
	failedAt := clock.Now().Add(-10 * time.Minute)
	r.set(job(path, "com.me.job", &failedAt, 1))
	tv.send(map[string]any{"cmd": "scheduleRefresh"})
	waitState(t, h, "診斷的回覆", func() bool {
		c := h.managementConversationOf(path)
		if c == nil || c.running {
			return false
		}
		for _, x := range c.Items {
			if x.Kind == "Assistant" && strings.Contains(x.Text, "收到：這是 Myriagent 排程頁交給你的工作") {
				return true
			}
		}
		return false
	})
	h.mu.Lock()
	mgmt := h.managementConversationOf(path)
	var user *Item
	for _, x := range mgmt.Items {
		if x.Kind == "User" {
			user = x
		}
	}
	diag := h.sched.store.State.Diagnoses
	h.mu.Unlock()
	if mgmt == nil || mgmt.Meta.Permission != "Plan" || mgmt.Meta.Title != "排程 com.me.job 的管理" || mgmt.Meta.IsDraft {
		t.Fatalf("管理對話：%+v", mgmt.Meta)
	}
	if !strings.Contains(user.HTML, "（自動診斷）排程 "+path) || !strings.Contains(user.Text, "**只診斷、不要修改任何檔案或排程**") {
		t.Fatalf("畫面顯示短句、代理收到完整說明：%q / %q", user.HTML, user.Text)
	}
	if len(diag) != 1 || diag[0].ConversationID != mgmt.Meta.ID {
		t.Fatalf("診斷紀錄：%+v", diag)
	}
	// 同一串失敗不再通知與診斷；24 小時內也不再自動診斷
	failedAgain := clock.Now().Add(-5 * time.Minute)
	r.set(job(path, "com.me.job", &failedAgain, 1))
	h.mu.Lock()
	h.requestScheduleScan()
	h.mu.Unlock()
	tv.wait("連續失敗 2 次", func(m map[string]any) bool {
		e := entryOf(m, path)
		return e != nil && e["streak"] == float64(2)
	})
	h.mu.Lock()
	n := len(h.sched.store.State.Diagnoses)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("同一串失敗只診斷一次：%d", n)
	}
}

func TestScheduleActionsAndAgentDraft(t *testing.T) {
	h, tv := newTestHub(t)
	r, _ := attachFake(t, h)
	path := "/Users/me/Library/LaunchAgents/com.me.x.plist"
	r.set(job(path, "com.me.x", nil, schedules.ResultHasNotRun))
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	tv.send(map[string]any{"cmd": "scheduleRefresh"})
	tv.wait("讀到", func(m map[string]any) bool { return entryOf(m, path) != nil })

	tv.send(map[string]any{"cmd": "scheduleAction", "path": strings.ToUpper(path[:1]) + path[1:], "action": "run"})
	tv.wait("已開始執行", func(m map[string]any) bool { return m["type"] == "toast" && m["text"] == "已開始執行 com.me.x" })
	r.mu.Lock()
	acts := strings.Join(r.actions, ",")
	r.mu.Unlock()
	if acts != "run "+path {
		t.Fatalf("快速操作經讀取器：%q", acts)
	}
	tv.send(map[string]any{"cmd": "scheduleAction", "path": path, "action": "adopt"})
	tv.wait("納入管理", func(m map[string]any) bool { e := entryOf(m, path); return e != nil && e["origin"] == "adopted" })
	tv.send(map[string]any{"cmd": "scheduleAction", "path": path, "action": "autoDiagnose", "on": false})
	tv.wait("個別關掉自動診斷", func(m map[string]any) bool { e := entryOf(m, path); return e != nil && e["autoDiagnose"] == false })
	tv.send(map[string]any{"cmd": "scheduleAction", "path": "/nope", "action": "run"})
	tv.wait("找不到", func(m map[string]any) bool {
		return m["type"] == "toast" && m["text"] == "找不到這個排程，請重新掃描"
	})

	// 交給代理：開草稿、預先填好說明、記成管理對話
	tv.send(map[string]any{"cmd": "scheduleAgent", "path": path, "task": "explain", "paneId": "p9"})
	conv := tv.wait("草稿", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "p9" })
	draft, _ := conv["draft"].(string)
	if !strings.Contains(draft, "請說明這個排程在做什麼") || !strings.Contains(draft, "`"+path+"`") || !strings.Contains(draft, "launchctl kickstart") {
		t.Fatalf("說明：%q", draft)
	}
	tv.wait("管理對話", func(m map[string]any) bool {
		e := entryOf(m, path)
		mg, _ := e["management"].(map[string]any)
		return mg != nil && mg["conversationId"] == conv["conversationId"] && mg["isDraft"] == true
	})
	// 第二次沿用同一個
	tv.send(map[string]any{"cmd": "scheduleAgent", "path": path, "task": "retime", "paneId": "p10"})
	again := tv.wait("沿用", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "p10" })
	if again["conversationId"] != conv["conversationId"] {
		t.Fatal("每個排程一個管理對話")
	}
	tv.send(map[string]any{"cmd": "scheduleAgent", "task": "create", "paneId": "p11"})
	created := tv.wait("新增排程", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "p11" })
	if d, _ := created["draft"].(string); !strings.Contains(d, "建立一個排程") {
		t.Fatalf("新增排程的說明：%q", d)
	}
}

func TestTimedMessageFiresIntoConversation(t *testing.T) {
	h, tv := newTestHub(t)
	_, clock := attachFake(t, h)
	convID := conversationWithReply(t, tv)
	tv.send(map[string]any{"cmd": "timedSave", "conversationId": convID, "when": map[string]any{"kind": "every", "value": "30m"}, "text": "檢查部署", "label": "部署"})
	tv.wait("已儲存", func(m map[string]any) bool { return m["type"] == "toast" && m["text"] == "已儲存定時訊息" })
	tv.send(map[string]any{"cmd": "timedSave", "conversationId": convID, "when": map[string]any{"kind": "every", "value": "7m"}, "text": "x"})
	tv.wait("時間不對", func(m map[string]any) bool {
		return m["type"] == "toast" && strings.Contains(m["text"].(string), "整除 60")
	})

	// 撥到下一個整點半之後：送進對話
	clock.Add(31 * time.Minute)
	h.mu.Lock()
	h.tickTimed()
	h.mu.Unlock()
	h.kick()
	tv.wait("代理收到定時訊息", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Assistant" && strings.Contains(x["html"].(string), "收到：（這是 Myriagent 的定時訊息「部署」，每 30 分鐘")
	})
	tv.wait("畫面上的那一則", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "User" && strings.Contains(x["html"].(string), "（定時訊息：部署）")
	})
	tv.wait("記下完成", func(m map[string]any) bool {
		sv, _ := m["schedules"].(map[string]any)
		list, _ := sv["timed"].([]any)
		if m["type"] != "schedules" || len(list) != 1 {
			return false
		}
		last, _ := list[0].(map[string]any)["last"].(map[string]any)
		return last != nil && last["level"] == "ok" && last["text"] == "完成"
	})
	// 暫停之後不送
	h.mu.Lock()
	id := h.sched.timed[0].ID
	h.mu.Unlock()
	tv.send(map[string]any{"cmd": "timedAction", "id": id, "action": "pause"})
	clock.Add(2 * time.Hour)
	h.mu.Lock()
	h.tickTimed()
	runs := len(h.sched.timed[0].Runs)
	h.mu.Unlock()
	if runs != 1 {
		t.Fatalf("暫停的不送：%d", runs)
	}
	// 存檔、重新載入
	b, err := os.ReadFile(filepath.Join(h.root, "schedules", "timed.json"))
	if err != nil || !bytes.Contains(b, []byte(`"Cron": "*/30 * * * *"`)) || !bytes.Contains(b, []byte(`"Outcome": "completed"`)) {
		t.Fatalf("timed.json：%v %s", err, b)
	}
}

func TestTimedMissedOneShot(t *testing.T) {
	h, tv := newTestHub(t)
	_, clock := attachFake(t, h)
	convID := conversationWithReply(t, tv)
	at := clock.Now().Add(2 * time.Hour).Format("2006-01-02 15:04")
	tv.send(map[string]any{"cmd": "timedSave", "conversationId": convID, "when": map[string]any{"kind": "at", "value": at}, "text": "一次"})
	tv.wait("已儲存", func(m map[string]any) bool { return m["type"] == "toast" && m["text"] == "已儲存定時訊息" })
	clock.Add(15 * time.Hour)
	h.mu.Lock()
	h.tickTimed()
	m := h.sched.timed[0]
	done, outcome := m.Done, m.Runs[0].Outcome
	h.mu.Unlock()
	if !done || outcome != "missed" {
		t.Fatalf("錯過超過 12 小時不補：%v %q", done, outcome)
	}
	tv.wait("錯過的提示", func(m map[string]any) bool {
		return m["type"] == "toast" && strings.Contains(m["text"].(string), "錯過太久沒有補送")
	})
}

func TestScheduleCLIThroughLocalAPI(t *testing.T) {
	h, tv := newTestHub(t)
	attachFake(t, h)
	convID := conversationWithReply(t, tv)
	h.mu.Lock()
	other := h.createConversation(h.accounts[0], "另一個", "").Meta.ID
	h.mu.Unlock()
	srv := httptest.NewServer(&server{hub: h, token: "x"})
	defer srv.Close()
	h.SetLocalURL(srv.URL)
	run := func(conv string, args ...string) (int, string, string) {
		t.Setenv("MYRIAGENT_AGENT_URL", srv.URL)
		t.Setenv("MYRIAGENT_REMOTE_TOKEN", h.localToken(conv))
		t.Setenv("MYRIAGENT_AGENT_CONVERSATION", conv)
		var out, errb bytes.Buffer
		code := schedulecli.Main(args, &out, &errb)
		return code, out.String(), errb.String()
	}
	code, out, errText := run(convID, "add", "--daily", "09:00", "--label", "報表", "--", "整理", "昨天的報表")
	if code != 0 || !strings.Contains(out, "已建立定時訊息 tm_") || !strings.Contains(out, "每天 09:00") {
		t.Fatalf("add：%d %q %q", code, out, errText)
	}
	code, out, _ = run(convID, "list")
	if code != 0 || !strings.HasPrefix(out, "* tm_") || !strings.Contains(out, "報表｜每天 09:00，下一次 ") || !strings.Contains(out, "送進「你好」") {
		t.Fatalf("list：%d %q", code, out)
	}
	h.mu.Lock()
	id := h.sched.timed[0].ID
	text, createdBy := h.sched.timed[0].Text, h.sched.timed[0].CreatedBy
	h.mu.Unlock()
	if text != "整理 昨天的報表" || createdBy != convID {
		t.Fatalf("文字、建立的對話：%q %q", text, createdBy)
	}
	// 別的對話不能刪
	if code, _, e := run(other, "remove", id); code != 1 || !strings.Contains(e, "不是這個對話建立的") {
		t.Fatalf("別的對話：%d %q", code, e)
	}
	if code, out, _ := run(other, "list"); code != 0 || !strings.HasPrefix(out, "  tm_") {
		t.Fatalf("別的對話看得到但不是它的：%q", out)
	}
	if code, out, _ := run(convID, "remove", id); code != 0 || !strings.Contains(out, "已刪除 "+id) {
		t.Fatalf("remove：%d %q", code, out)
	}
	if code, _, e := run(convID, "add", "--every", "3m", "--", "x"); code != 1 || !strings.Contains(e, "至少 5 分鐘") {
		t.Fatalf("被拒絕：%d %q", code, e)
	}
	if code, _, _ := run(convID, "add", "--", "x"); code != 2 {
		t.Fatal("少了時間是參數錯誤")
	}
	// 權杖不對
	t.Setenv("MYRIAGENT_REMOTE_TOKEN", h.localToken("someone-else"))
	var out2, err2 bytes.Buffer
	if code := schedulecli.Main([]string{"list"}, &out2, &err2); code != 3 {
		t.Fatalf("權杖不對是 3：%d %q", code, err2.String())
	}
	// 說明：本機介面開著才給
	h.mu.Lock()
	hint := h.extraInstructions(h.convs[convID])
	h.mu.Unlock()
	if !strings.Contains(hint, "myriagent schedule add --daily 09:00") {
		t.Fatalf("給代理的說明：%q", hint)
	}
}

func TestScheduleFingerprintIgnoresEnabled(t *testing.T) {
	a := scheduleFingerprint("<plist><dict><key>Disabled</key><true/><key>Label</key><string>x</string></dict></plist>")
	b := scheduleFingerprint("<plist><dict><key>Disabled</key><false/><key>Label</key><string>x</string></dict></plist>")
	c := scheduleFingerprint("<plist><dict><key>Label</key><string>y</string></dict></plist>")
	if a != b || a == c || len(a) != 16 {
		t.Fatalf("指紋：%s %s %s", a, b, c)
	}
}

func TestTurnOutcome(t *testing.T) {
	c := &Conv{}
	c.init()
	c.addItem("User")
	c.addItem("Assistant")
	if turnOutcome(c) != turnCompleted {
		t.Fatal("完成")
	}
	c.addItem("Error")
	if turnOutcome(c) != turnFailed {
		t.Fatal("失敗")
	}
	c.addItem("User")
	if turnOutcome(c) != turnCompleted {
		t.Fatal("上一輪的錯誤不算")
	}
	c.stopping = true
	if turnOutcome(c) != turnInterrupted {
		t.Fatal("停止")
	}
}
