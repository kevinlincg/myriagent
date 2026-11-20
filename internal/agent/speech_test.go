package agent

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVoiceRulesSentences(t *testing.T) {
	cases := []struct{ got, want string }{
		{voiceCompleted("整理報表", "月報已經匯出成 Excel。另外修正了日期格式。"), "「整理報表」已完成：月報已經匯出成 Excel"},
		{voiceCompleted("整理報表", ""), "「整理報表」已完成"},
		{voiceCompleted("   ", "  "), "對話已完成"},
		{voiceCompleted("語音通知", "語音通知已經加進**桌面版**，並推送到 main（`12087fa`）。"), "「語音通知」已完成：語音通知已經加進桌面版，並推送到 main"},
		{voiceCompleted("文件", "```bash\ndotnet build\n```\n看 [部署手冊](https://example.test/ops) 照做就好。"), "「文件」已完成：看 部署手冊 照做就好"},
		{voiceCompleted("登入", "## 摘要\n\n完成了，改動如下：\n\n- 修好登入頁的錯誤訊息\n- 加上測試"), "「登入」已完成：修好登入頁的錯誤訊息"},
		{voiceCompleted("登入", "好的。已把登入頁的錯誤訊息改成中文。"), "「登入」已完成：已把登入頁的錯誤訊息改成中文"},
		{voiceCompleted("登入", "完成登入頁改版。"), "「登入」已完成：完成登入頁改版"},
		{voiceCompleted("報表", "改好了。欄位順序照你給的範本排。"), "「報表」已完成：改好了。欄位順序照你給的範本排"},
		{voiceCompleted("報表", strings.Repeat("字", 300)), "「報表」已完成：" + strings.Repeat("字", 300)},
		{voiceQuestion("整理報表", "要用 **哪個** 格式？\n第二行說明"), "「整理報表」有問題想問你：要用 哪個 格式？"},
		{voiceQuestion("整理報表", "  "), "「整理報表」有問題想問你"},
		{voiceApproval("部署", "執行指令", "推送到 main\n\ngit push origin HEAD:main", ""), "「部署」要執行指令，等你核准：推送到 main"},
		{voiceApproval("部署", "執行指令", "Push to main", ""), "「部署」要執行指令，等你核准"},
		{voiceApproval("部署", "修改檔案", "", "/repo/src/AgentHub.cs"), "「部署」要修改檔案，等你核准：AgentHub.cs"},
		{voiceApproval("部署", "mcp__playwright__browser_click", "", ""), "「部署」要執行操作，等你核准"},
		{voicePlan("同步", "# 重構同步模組\n\n1. 拆出規則"), "「同步」的計畫等你確認：重構同步模組"},
		{voiceFailed("備份", "資料庫連不上"), "「備份」執行失敗：資料庫連不上"},
		{voiceFailed("備份", "API Error: 529 overloaded_error"), "「備份」執行失敗"},
		{voiceClean("請看 https://example.test/a?b=1 的\t說明", 60), "請看 連結 的 說明"},
		{voiceClean("`npm test` 要跑嗎", 60), "npm test 要跑嗎"},
		{voiceClean(strings.Repeat("長", 80), 20), strings.Repeat("長", 20)},
		{voiceFit("很長的說明，很長的說明，很長的說明", 12), "很長的說明，很長的說明"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("第 %d 個：\n得到 %q\n應為 %q", i, c.got, c.want)
		}
	}
}

func TestVoiceShouldQueue(t *testing.T) {
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	s := "「甲」已完成"
	if !voiceShouldQueue(s, 0, "", time.Time{}, now) || voiceShouldQueue(s, voiceMaxPending, "", time.Time{}, now) {
		t.Fatal("佇列已滿不排")
	}
	if voiceShouldQueue(s, 1, s, now.Add(-3*time.Second), now) {
		t.Fatal("剛念過同一句")
	}
	if !voiceShouldQueue(s, 0, s, now.Add(-voiceRepeatWindow), now) || !voiceShouldQueue("「乙」已完成", 1, s, now.Add(-time.Second), now) {
		t.Fatal("過了間隔、不同句子都要排")
	}
	if !voiceShouldQueue(s, 0, s, now.Add(5*time.Minute), now) || voiceShouldQueue("  ", 0, "", time.Time{}, now) {
		t.Fatal("時鐘倒退照排；空白不排")
	}
	if normalizeVoiceSpeed(9) != 2.5 || normalizeVoiceSpeed(1.26) != 1.3 || normalizeVoicePitch(-9) != -6 || normalizeVoicePitch(2.5) != 3 {
		t.Fatal("語速音調正規化")
	}
}

func TestSayAndSpdArgs(t *testing.T) {
	if got := sayArgs("Meijia", voiceTuning{1.5, 2}); !reflect.DeepEqual(got, []string{"-v", "Meijia", "-r", "270", "-f", "-"}) {
		t.Fatalf("say 參數：%v", got)
	}
	if got := sayInput("看 [[slnc 1000]] 這裡", voiceTuning{1, -2}); got != "[[pbas -2]] 看 [ [slnc 1000]] 這裡" {
		t.Fatalf("say 文字：%q", got)
	}
	if sayInput("嗨", voiceTuning{1, 0}) != "嗨" {
		t.Fatal("音調 0 不加指令")
	}
	if got := spdArgs(voiceTuning{1.5, 3}); !reflect.DeepEqual(got, []string{"-w", "-e", "-r", "50", "-p", "50"}) {
		t.Fatalf("spd-say 參數：%v", got)
	}
	listing := "Alex                en_US    # Most people recognize me by my voice.\nTingting            zh_CN    # 你好\nMeijia              zh_TW    # 您好\n"
	if pickSayVoice(listing) != "Meijia" {
		t.Fatal("偏好美佳")
	}
	if pickSayVoice("Alex                en_US    # x\nSinji               zh_TW    # y\n") != "Sinji" {
		t.Fatal("沒有偏好的就挑台灣中文")
	}
	if pickSayVoice("Alex                en_US    # x\n") != "" {
		t.Fatal("都沒有就不指定")
	}
}

// fakeSpeech：假的聲音引擎：開始念時記下，念到 release 或被要求停下才返回
type fakeSpeech struct {
	mu       sync.Mutex
	started  []string
	tunings  []voiceTuning
	stopped  []string
	release  chan struct{}
	finished chan string
}

func newFakeSpeech() *fakeSpeech {
	return &fakeSpeech{release: make(chan struct{}, 100), finished: make(chan string, 100)}
}

func (f *fakeSpeech) Speak(text string, t voiceTuning, shouldStop func() bool, onStart func()) error {
	f.mu.Lock()
	f.started = append(f.started, text)
	f.tunings = append(f.tunings, t)
	f.mu.Unlock()
	onStart()
	for {
		select {
		case <-f.release:
			f.finished <- text
			return nil
		case <-time.After(5 * time.Millisecond):
			if shouldStop() {
				f.mu.Lock()
				f.stopped = append(f.stopped, text)
				f.mu.Unlock()
				f.finished <- text
				return nil
			}
		}
	}
}

func (f *fakeSpeech) startedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.started...)
}

func waitForSpeech(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestVoiceAnnouncerQueueStopAndUnavailable(t *testing.T) {
	f := newFakeSpeech()
	a := newVoiceAnnouncer(func() (speechEngine, error) { return f, nil })
	defer a.dispose()
	var startedCB sync.WaitGroup
	startedCB.Add(1)
	a.speak("一", voiceTuning{1, 0}, startedCB.Done)
	startedCB.Wait() // 開始念的回呼
	a.speak("二", voiceTuning{2, 1}, nil)
	a.speak("二", voiceTuning{2, 1}, nil) // 剛排過同一句：略過
	a.speak("三", voiceTuning{1, 0}, nil)
	a.speak("四", voiceTuning{1, 0}, nil)
	a.speak("五", voiceTuning{1, 0}, nil) // 佇列已有 3 句：略過
	for i := 0; i < 4; i++ {
		f.release <- struct{}{}
	}
	waitForSpeech(t, "念完", a.idle)
	if got := f.startedList(); !reflect.DeepEqual(got, []string{"一", "二", "三", "四"}) {
		t.Fatalf("念的順序：%v", got)
	}
	if f.tunings[1] != (voiceTuning{2, 1}) {
		t.Fatal("每句帶自己的語速音調")
	}
	// stop：停下正在念的、清掉還沒念的
	a.speak("六", voiceTuning{1, 0}, nil)
	a.speak("七", voiceTuning{1, 0}, nil)
	waitForSpeech(t, "開始念六", func() bool { return len(f.startedList()) == 5 })
	a.stop()
	waitForSpeech(t, "停下", a.idle)
	f.mu.Lock()
	stopped := append([]string{}, f.stopped...)
	f.mu.Unlock()
	if !reflect.DeepEqual(stopped, []string{"六"}) || len(f.startedList()) != 5 {
		t.Fatalf("停下的：%v、念過：%v", stopped, f.startedList())
	}

	// 引擎建不起來：之後都不念
	calls := 0
	b := newVoiceAnnouncer(func() (speechEngine, error) { calls++; return nil, errors.New("沒有語音") })
	b.speak("甲", voiceTuning{1, 0}, nil)
	waitForSpeech(t, "不能用", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.unavailable })
	b.speak("乙", voiceTuning{1, 0}, nil)
	time.Sleep(20 * time.Millisecond)
	if calls != 1 {
		t.Fatalf("建立引擎 %d 次", calls)
	}
}

// 中樞：開著語音通知時，一輪完成閃對話並念「…已完成：回覆的重點」，聲音出來時再送帶 speaking 的 flash；核准卡念「要…等你核准」
func TestVoiceNotificationsHub(t *testing.T) {
	f := newFakeSpeech()
	old := newSpeechEngine
	newSpeechEngine = func() (speechEngine, error) { return f, nil }
	t.Cleanup(func() { newSpeechEngine = old })
	for i := 0; i < 20; i++ {
		f.release <- struct{}{}
	}
	h, tv := newTestHub(t)
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "setting", "key": "voiceNotifications", "value": true})
	waitForSpeech(t, "試聽句", func() bool { s := f.startedList(); return len(s) == 1 && s[0] == voiceEnabledSample })
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "你好"})
	tv.wait("閃", func(m map[string]any) bool {
		return m["type"] == "flash" && m["conversationId"] == id && m["speaking"] == nil
	})
	tv.wait("念的時候再閃", func(m map[string]any) bool { return m["type"] == "flash" && m["speaking"] == true })
	waitForSpeech(t, "念完成", func() bool {
		s := f.startedList()
		return len(s) == 2 && s[1] == "「你好」已完成：收到：你好"
	})
	f.mu.Lock()
	tn := f.tunings[1]
	f.mu.Unlock()
	if tn != (voiceTuning{1.5, 2}) {
		t.Fatalf("預設語速音調：%v", tn)
	}
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個工具"})
	waitForSpeech(t, "念核准", func() bool {
		s := f.startedList()
		return len(s) == 3 && s[2] == "「你好」要執行指令，等你核准"
	})
	// 調語速：存正規化的值、念試聽句
	tv.send(map[string]any{"cmd": "setting", "key": "voiceSpeed", "value": 9.0})
	waitForSpeech(t, "試聽語速", func() bool { s := f.startedList(); return len(s) == 4 && s[3] == voiceTuningSample })
	h.mu.Lock()
	sp := h.setting("voiceSpeed")
	h.mu.Unlock()
	if sp != 2.5 {
		t.Fatalf("語速要限制在範圍內：%v", sp)
	}
	// 使用者按停止的那一輪不念
	tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": pendingItemID(h, id), "choice": "deny", "message": "不要"})
	waitForSpeech(t, "核准那一輪結束", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return !h.convs[id].running })
	waitForSpeech(t, "念完成", func() bool { return len(f.startedList()) == 5 })
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "慢一點"})
	waitForSpeech(t, "執行中", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.convs[id].running })
	tv.send(map[string]any{"cmd": "stop", "conversationId": id})
	waitForSpeech(t, "停止", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return !h.convs[id].running })
	time.Sleep(150 * time.Millisecond)
	if n := len(f.startedList()); n != 5 {
		t.Fatalf("按停止的那一輪不該念：%v", f.startedList())
	}
	// 關掉：之後只閃不念
	tv.send(map[string]any{"cmd": "setting", "key": "voiceNotifications", "value": false})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "再一次"})
	tv.wait("回覆", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Assistant" && strings.Contains(x["html"].(string), "收到：再一次")
	})
	time.Sleep(150 * time.Millisecond)
	if n := len(f.startedList()); n != 5 {
		t.Fatalf("關掉之後還在念：%v", f.startedList())
	}
}

func pendingItemID(h *Hub, convID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.convs[convID].pending {
		return id
	}
	return ""
}
