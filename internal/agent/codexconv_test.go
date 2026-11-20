package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newCodexTestHub：中樞照常啟動，Codex CLI 換成測試執行檔扮演的假 app-server（fakeCodex）；預設帳戶的 ~/.codex 有 auth.json
func newCodexTestHub(t *testing.T) (*Hub, *testView, string) {
	t.Helper()
	h, tv := newTestHub(t)
	t.Setenv("MYRIAGENT_FAKE_CLI", "codex")
	t.Setenv("CODEX_HOME", "")
	logPath := filepath.Join(t.TempDir(), "codex.log")
	t.Setenv("MYRIAGENT_FAKE_CODEX_LOG", logPath)
	codexHome := filepath.Join(os.Getenv("HOME"), ".codex")
	_ = os.MkdirAll(codexHome, 0o755)
	_ = os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{}`), 0o600)
	h.mu.Lock()
	h.clis["Codex"] = &cliInfo{path: os.Args[0], version: "0.170.0"}
	for _, a := range h.accounts {
		if a.Provider == "Codex" {
			h.refreshAccount(a)
		}
	}
	h.mu.Unlock()
	return h, tv, logPath
}

func codexAccountID(h *Hub) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.accounts {
		if a.Provider == "Codex" {
			return a.ID
		}
	}
	return ""
}

func (tv *testView) newCodexDraft() string {
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	tv.wait("init", func(m map[string]any) bool { return m["type"] == "init" })
	tv.send(map[string]any{"cmd": "newConversation", "accountId": codexAccountID(tv.h), "paneId": "pc"})
	m := tv.wait("草稿", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "pc" })
	hd := m["header"].(map[string]any)
	if hd["provider"] != "Codex" {
		tv.t.Fatalf("草稿不是 Codex 對話：%v", hd["provider"])
	}
	return m["conversationId"].(string)
}

func (tv *testView) waitItem(what string, ok func(it map[string]any) bool) map[string]any {
	tv.t.Helper()
	return itemOf(tv.wait(what, func(m map[string]any) bool {
		it := itemOf(m)
		return it != nil && ok(it)
	}))
}

// waitIdle：等這個對話不在執行中、而且送給網頁的最後一個標頭也是閒置。
// 不能只找「有一則 running:false 的標頭」：送出之前（例如背景查帳戶時）就可能有一則，等待會立刻通過。
// 送出（send）在 h.handle 裡同步把對話標成執行中，所以送出之後看中樞的狀態不會看到舊的閒置
func (tv *testView) waitIdle(id string) {
	tv.t.Helper()
	waitFor(tv.t, "這一輪結束", func() bool {
		tv.h.mu.Lock()
		c := tv.h.convs[id]
		running := c != nil && c.running
		tv.h.mu.Unlock()
		if running {
			return false
		}
		tv.mu.Lock()
		defer tv.mu.Unlock()
		for i := len(tv.msgs) - 1; i >= 0; i-- {
			m := tv.msgs[i]
			if m["type"] == "header" && m["conversationId"] == id {
				hd, _ := m["header"].(map[string]any)
				return hd["running"] == false
			}
		}
		return false
	})
}

func htmlHas(it map[string]any, s string) bool {
	h, _ := it["html"].(string)
	return strings.Contains(h, s)
}

// codexLog：假 app-server 收到的每一則（method → params 依序）
func codexLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(path)
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func codexCalls(t *testing.T, path, method string) []map[string]any {
	var out []map[string]any
	for _, m := range codexLog(t, path) {
		if m["method"] == method {
			p, _ := m["params"].(map[string]any)
			out = append(out, p)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

func TestCodexAccountReady(t *testing.T) {
	h, tv, _ := newCodexTestHub(t)
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	tv.wait("Codex 帳戶已登入", func(m map[string]any) bool {
		if m["type"] != "accounts" {
			return false
		}
		for _, x := range m["accounts"].([]any) {
			a := x.(map[string]any)
			if a["provider"] == "Codex" && a["status"] == "ready" && a["email"] == "codex@example.com" && a["plan"] == "Pro" &&
				strings.HasSuffix(a["configDirectory"].(string), ".codex") {
				return true
			}
		}
		return false
	})
	// 查詢時一併拿到模型清單：「預設」、預設模型、其他；隱藏的不列
	waitFor(t, "帳戶的模型清單", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, a := range h.accounts {
			if a.Provider == "Codex" && len(a.models) == 3 && a.models[1].Value == "gpt-a" && a.models[1].Effort != nil && *a.models[1].Effort == "medium" {
				return true
			}
		}
		return false
	})
}

func TestCodexAccountNotLoggedInWithoutAuth(t *testing.T) {
	h, _, _ := newCodexTestHub(t)
	_ = os.Remove(filepath.Join(os.Getenv("HOME"), ".codex", "auth.json"))
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.accounts {
		if a.Provider == "Codex" {
			h.refreshAccount(a)
			if a.status != "notLoggedIn" {
				t.Fatalf("沒有 auth.json 應該是尚未登入：%s", a.status)
			}
		}
	}
}

func TestCodexSendStreamsReplyAndUsesThread(t *testing.T) {
	h, tv, logPath := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "你好"})
	tv.waitItem("回覆", func(it map[string]any) bool {
		return it["kind"] == "Assistant" && it["status"] == "Done" && htmlHas(it, "收到：你好")
	})
	tv.wait("用量", func(m map[string]any) bool {
		hd, _ := m["header"].(map[string]any)
		return m["type"] == "header" && hd["running"] == false && hd["lastTurn"] == "輸入 1,000・輸出 100 tokens"
	})
	starts := codexCalls(t, logPath, "thread/start")
	if len(starts) != 1 || starts[0]["approvalPolicy"] != "untrusted" || starts[0]["sandbox"] != "workspace-write" {
		t.Fatalf("thread/start 的參數不對：%v", starts)
	}
	turns := codexCalls(t, logPath, "turn/start")
	sp, _ := turns[0]["sandboxPolicy"].(map[string]any)
	if len(turns) != 1 || turns[0]["approvalPolicy"] != "untrusted" || sp["type"] != "workspaceWrite" || turns[0]["model"] != nil {
		t.Fatalf("turn/start 的參數不對：%v", turns)
	}
	h.mu.Lock()
	sid, started := h.convs[id].Meta.SessionID, h.convs[id].Meta.SessionStarted
	h.mu.Unlock()
	if !strings.HasPrefix(sid, "th-") || !started {
		t.Fatalf("對話沒記下 Codex 的 thread：%q %v", sid, started)
	}
	// 模型清單：預設、預設模型、其他；隱藏的不列
	tv.wait("模型清單", func(m map[string]any) bool {
		hd, _ := m["header"].(map[string]any)
		ms, _ := hd["models"].([]any)
		if m["type"] != "header" || len(ms) != 3 {
			return false
		}
		return ms[0].(map[string]any)["value"] == "default" && ms[1].(map[string]any)["value"] == "gpt-a" && ms[2].(map[string]any)["value"] == "gpt-b"
	})
	// 換模型與強度：下一輪的 turn/start 帶上，不必重開程序
	tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "model", "value": "gpt-b"})
	tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "effort", "value": "low"})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第二則"})
	tv.waitItem("第二則回覆", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "收到：第二則") })
	tv.waitIdle(id)
	// 第二輪的用量是 total 的差（累計 2000／200 減掉上一輪的 1000／100），不是累計、也不是最後一次呼叫的 600／60
	h.mu.Lock()
	lastTurn := h.convs[id].Meta.LastTurn
	h.mu.Unlock()
	if lastTurn != "輸入 1,000・輸出 100 tokens" {
		t.Fatalf("第二輪的用量：%q", lastTurn)
	}
	turns = codexCalls(t, logPath, "turn/start")
	if len(turns) != 2 || turns[1]["model"] != "gpt-b" || turns[1]["effort"] != "low" || turns[1]["threadId"] != sid {
		t.Fatalf("換模型後的 turn/start 不對：%v", turns)
	}
	if n := len(codexCalls(t, logPath, "thread/resume")) + len(codexCalls(t, logPath, "thread/start")); n != 1 {
		t.Fatalf("換模型不該重開程序（重開會再 thread/resume），開 thread 了 %d 次", n)
	}
}

func TestCodexApprovalAllowRunsCommand(t *testing.T) {
	_, tv, logPath := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個指令"})
	card := tv.waitItem("核准卡", func(it map[string]any) bool { return it["kind"] == "Approval" && it["status"] == "Waiting" })
	if card["title"] != "echo hello" || card["canAllowForSession"] != true || !strings.Contains(card["detail"].(string), "要跑測試") {
		t.Fatalf("核准卡內容不對：%v", card)
	}
	tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": card["id"], "choice": "allow"})
	tv.waitItem("指令結果", func(it map[string]any) bool {
		return it["kind"] == "Tool" && it["toolKind"] == "bash" && it["status"] == "Done" && it["output"] == "hello\n"
	})
	tv.waitItem("代理接著回覆", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "指令跑完了") })
	tv.waitItem("卡片已允許", func(it map[string]any) bool {
		return it["id"] == card["id"] && it["decision"] == "allow" && it["status"] == "Done"
	})
	found := false
	for _, m := range codexLog(t, logPath) {
		if r, ok := m["result"].(map[string]any); ok && r["decision"] == "accept" {
			found = true
		}
	}
	if !found {
		t.Fatal("沒有回 accept 給 Codex")
	}
}

func TestCodexApprovalDenySendsFeedbackNext(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個指令"})
	card := tv.waitItem("核准卡", func(it map[string]any) bool { return it["kind"] == "Approval" && it["status"] == "Waiting" })
	tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": card["id"], "choice": "deny", "message": "不要"})
	tv.waitItem("工具被拒", func(it map[string]any) bool { return it["kind"] == "Tool" && it["status"] == "Declined" })
	tv.waitItem("只拒這一步（decline）", func(it map[string]any) bool {
		return it["kind"] == "Assistant" && htmlHas(it, "好，不跑了（decline）")
	})
	// Codex 沒有欄位放拒絕的說明：這一輪結束後當下一則訊息送出
	tv.waitItem("說明當下一則訊息", func(it map[string]any) bool {
		return it["kind"] == "Assistant" && htmlHas(it, "收到：（我剛才拒絕了那個操作）不要")
	})
}

func TestCodexFileChangeAllowForSession(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "改檔"})
	card := tv.waitItem("改檔核准卡", func(it map[string]any) bool { return it["kind"] == "Approval" && it["status"] == "Waiting" })
	diff, _ := json.Marshal(card["diff"])
	if card["title"] != "/tmp/a.txt" || card["toolKind"] != "edit" || !strings.Contains(string(diff), `{"s":"舊","t":"del"}`) || !strings.Contains(string(diff), `{"s":"新","t":"add"}`) {
		t.Fatalf("改檔核准卡要有檔名與差異：%v", card)
	}
	tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": card["id"], "choice": "allowSession"})
	tv.waitItem("第一次改檔", func(it map[string]any) bool {
		return it["kind"] == "Assistant" && htmlHas(it, "改檔（turn-1）：acceptForSession")
	})
	tv.waitIdle(id)
	// 本對話的檔案修改都不再詢問：第二次直接放行，不再出現核准卡
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "再改檔"})
	tv.waitItem("第二次改檔直接放行", func(it map[string]any) bool {
		return it["kind"] == "Assistant" && htmlHas(it, "改檔（turn-2）：acceptForSession")
	})
	tv.mu.Lock()
	defer tv.mu.Unlock()
	cards := map[any]bool{}
	for _, m := range tv.msgs {
		if it := itemOf(m); it != nil && it["kind"] == "Approval" {
			cards[it["id"]] = true
		}
	}
	if len(cards) != 1 {
		t.Fatalf("選了本對話都允許之後又出現核准卡：%d 張", len(cards))
	}
}

func TestCodexStopInterruptsAndSendsQueue(t *testing.T) {
	_, tv, logPath := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "慢一點"})
	tv.waitItem("開始回覆", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "慢慢來") })
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第二則"})
	tv.wait("排進佇列", func(m map[string]any) bool {
		c, _ := m["composer"].(map[string]any)
		q, _ := c["queue"].([]any)
		return m["type"] == "composer" && len(q) == 1
	})
	tv.send(map[string]any{"cmd": "stop", "conversationId": id})
	tv.waitItem("已停止", func(it map[string]any) bool { return it["kind"] == "Notice" && htmlHas(it, "已停止這一輪") })
	tv.waitItem("佇列送出", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "收到：第二則") })
	ints := codexCalls(t, logPath, "turn/interrupt")
	if len(ints) != 1 || ints[0]["turnId"] != "turn-1" || !strings.HasPrefix(ints[0]["threadId"].(string), "th-") {
		t.Fatalf("turn/interrupt 不對：%v", ints)
	}
}

func TestCodexResumeAfterAgentRestart(t *testing.T) {
	h, tv, logPath := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "記住我"})
	tv.waitItem("回覆", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "收到：記住我") })
	tv.waitIdle(id)
	h.mu.Lock()
	thread := h.convs[id].Meta.SessionID
	root := h.root
	h.mu.Unlock()
	h.shutdown()

	// 新的中樞（app 重開）：同一個資料夾，對話以 thread/resume 續接
	h2 := &Hub{root: root, convs: map[string]*Conv{}, views: map[*View]struct{}{}, clis: map[string]*cliInfo{}, flushKick: make(chan struct{}, 1)}
	h2.settings = loadSettings(root)
	h2.clis["Codex"] = &cliInfo{path: os.Args[0]}
	h2.loadAccounts()
	h2.loadConversations()
	go h2.flushLoop()
	t.Cleanup(h2.shutdown)
	tv2 := &testView{t: t, h: h2}
	tv2.v = h2.addView(func(b []byte) error {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		tv2.mu.Lock()
		tv2.msgs = append(tv2.msgs, m)
		tv2.mu.Unlock()
		return nil
	})
	tv2.send(map[string]any{"cmd": "ready", "mode": "main"})
	tv2.send(map[string]any{"cmd": "open", "conversationId": id})
	tv2.send(map[string]any{"cmd": "send", "conversationId": id, "text": "還記得嗎"})
	tv2.waitItem("續接後的回覆", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "收到：還記得嗎") })
	res := codexCalls(t, logPath, "thread/resume")
	if len(res) != 1 || res[0]["threadId"] != thread || res[0]["excludeTurns"] != true {
		t.Fatalf("沒有以 thread/resume 續接 %s：%v", thread, res)
	}
	if n := len(codexCalls(t, logPath, "thread/start")); n != 1 {
		t.Fatalf("續接成功不該另開新的 thread（thread/start %d 次）", n)
	}
}

func TestCodexResumeMissingThreadStartsNew(t *testing.T) {
	h, tv, logPath := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第一則"})
	tv.waitIdle(id)
	h.mu.Lock()
	c := h.convs[id]
	codexProvider{}.Close(c)
	c.Meta.SessionID = "missing-thread"
	h.mu.Unlock()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第二則"})
	tv.waitItem("說明改開新對話", func(it map[string]any) bool {
		return it["kind"] == "Notice" && htmlHas(it, "找不到原本的 Codex 對話紀錄")
	})
	tv.waitItem("照常回覆", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "收到：第二則") })
	if n := len(codexCalls(t, logPath, "thread/start")); n != 2 {
		t.Fatalf("續接不了要改開新的 thread（thread/start %d 次）", n)
	}
	h.mu.Lock()
	sid := h.convs[id].Meta.SessionID
	h.mu.Unlock()
	if sid == "missing-thread" {
		t.Fatal("改開新的 thread 之後沒有記下新的 id")
	}
}

func TestCodexResumeActiveWriterRefuses(t *testing.T) {
	h, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第一則"})
	tv.waitIdle(id)
	h.mu.Lock()
	c := h.convs[id]
	codexProvider{}.Close(c)
	c.Meta.SessionID = "busy-thread"
	h.mu.Unlock()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第二則"})
	tv.waitItem("請使用者先關掉另一邊", func(it map[string]any) bool { return it["kind"] == "Error" && htmlHas(it, "正在其他程式") })
	tv.waitIdle(id)
}

func TestCodexAsyncQuestionPausesAndAnswersAsMessage(t *testing.T) {
	_, tv, logPath := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "提問"})
	card := tv.waitItem("非同步提問", func(it map[string]any) bool {
		return it["kind"] == "Question" && it["status"] == "Waiting" && it["answerAsMessage"] == true
	})
	// 停下這一輪等回答（不算「已停止」）
	tv.waitIdle(id)
	if len(codexCalls(t, logPath, "turn/interrupt")) != 1 {
		t.Fatal("非同步提問要停下這一輪等回答")
	}
	tv.send(map[string]any{"cmd": "answer", "conversationId": id, "itemId": card["id"], "answers": map[string]any{card["questions"].([]any)[0].(map[string]any)["id"].(string): []any{"乙"}}})
	tv.waitItem("答案當下一則訊息", func(it map[string]any) bool {
		return it["kind"] == "Assistant" && htmlHas(it, "收到：我的回答：") && htmlHas(it, "要哪一個？：乙")
	})
	tv.mu.Lock()
	defer tv.mu.Unlock()
	for _, m := range tv.msgs {
		if it := itemOf(m); it != nil && it["kind"] == "Notice" && htmlHas(it, "已停止") {
			t.Fatal("為了等回答停下的一輪不該顯示「已停止」")
		}
	}
}

func TestCodexUserInputQuestion(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "表單"})
	card := tv.waitItem("問卷", func(it map[string]any) bool { return it["kind"] == "Question" && it["status"] == "Waiting" })
	if card["answerAsMessage"] == true {
		t.Fatal("requestUserInput 是同步的問卷，不是當下一則訊息")
	}
	tv.send(map[string]any{"cmd": "answer", "conversationId": id, "itemId": card["id"], "answers": map[string]any{"q1": []any{"乙"}}})
	tv.waitItem("代理收到答案", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "你選了 [乙]") })
}

func TestCodexReasoningAndPlan(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "計畫"})
	tv.waitItem("推理分段", func(it map[string]any) bool {
		return it["kind"] == "Thinking" && it["status"] == "Done" && htmlHas(it, "想一想") && htmlHas(it, "再想")
	})
	plan := tv.waitItem("待辦清單", func(it map[string]any) bool { return it["kind"] == "Plan" && htmlHas(it, "分兩步") })
	steps := plan["plan"].([]any)
	if len(steps) != 2 || steps[1].(map[string]any)["status"] != "inProgress" {
		t.Fatalf("計畫步驟不對：%v", steps)
	}
	tv.waitIdle(id)
}

func TestCodexFailedTurnShowsError(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "失敗"})
	tv.waitItem("錯誤", func(it map[string]any) bool { return it["kind"] == "Error" && htmlHas(it, "伺服器壞了") })
	tv.waitIdle(id)
	tv.mu.Lock()
	defer tv.mu.Unlock()
	n := 0
	for _, m := range tv.msgs {
		if it := itemOf(m); it != nil && it["kind"] == "Notice" && htmlHas(it, "正在重試") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("連線重試的提示同一輪只要一次：%d", n)
	}
}

func TestCodexProcessCrashThenRecovers(t *testing.T) {
	_, tv, logPath := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "先聊聊"})
	tv.waitIdle(id)
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "當掉"})
	tv.waitItem("程序意外結束", func(it map[string]any) bool {
		return it["kind"] == "Error" && htmlHas(it, "結束碼 3") && htmlHas(it, "假的當掉")
	})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "還在嗎"})
	tv.waitItem("重開程序後續接", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "收到：還在嗎") })
	if len(codexCalls(t, logPath, "thread/resume")) != 1 {
		t.Fatal("程序重開後要以 thread/resume 續接原本的 thread")
	}
}

func TestCodexFullAccessAutoApproves(t *testing.T) {
	_, tv, logPath := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "permission", "value": "FullAccess"})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個指令"})
	tv.waitItem("指令直接執行", func(it map[string]any) bool {
		return it["kind"] == "Tool" && it["status"] == "Done" && it["output"] == "hello\n"
	})
	tv.waitIdle(id)
	tv.mu.Lock()
	for _, m := range tv.msgs {
		if it := itemOf(m); it != nil && it["kind"] == "Approval" {
			tv.mu.Unlock()
			t.Fatal("完全自動不該出現核准卡")
		}
	}
	tv.mu.Unlock()
	turns := codexCalls(t, logPath, "turn/start")
	sp, _ := turns[0]["sandboxPolicy"].(map[string]any)
	if turns[0]["approvalPolicy"] != "never" || sp["type"] != "dangerFullAccess" {
		t.Fatalf("完全自動的政策不對：%v", turns[0])
	}
}

func TestCodexSwitchToFullAccessApprovesWaitingCard(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個指令"})
	card := tv.waitItem("核准卡", func(it map[string]any) bool { return it["kind"] == "Approval" && it["status"] == "Waiting" })
	tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "permission", "value": "FullAccess"})
	tv.waitItem("卡片自動允許", func(it map[string]any) bool {
		return it["id"] == card["id"] && it["status"] == "Done" && it["decision"] == "allow"
	})
	tv.waitItem("指令執行", func(it map[string]any) bool { return it["kind"] == "Tool" && it["output"] == "hello\n" })
}

func TestCodexSubagentCard(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "子代理"})
	tv.waitItem("子代理卡", func(it map[string]any) bool {
		out, _ := it["output"].(string)
		return it["kind"] == "Tool" && it["toolKind"] == "agent" && it["title"] == "探路者 · gpt-b" && it["status"] == "Done" &&
			strings.Contains(out, "執行：ls") && strings.Contains(out, "找到了")
	})
	tv.waitItem("主對話回覆", func(it map[string]any) bool { return it["kind"] == "Assistant" && htmlHas(it, "子代理做完了") })
}

func TestCodexDoctor(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	tv.send(map[string]any{"cmd": "codexDoctor", "accountId": codexAccountID(tv.h), "requestId": "r1", "networkOnly": false})
	m := tv.wait("診斷結果", func(m map[string]any) bool { return m["type"] == "codexDoctor" && m["requestId"] == "r1" })
	res, _ := m["result"].(map[string]any)
	checks, _ := res["checks"].([]any)
	if res["status"] != "ok" || res["version"] != "0.170.0" || len(checks) != 2 || checks[0].(map[string]any)["id"] != "auth" {
		t.Fatalf("診斷結果不對：%v", m)
	}
	tv.send(map[string]any{"cmd": "codexDoctor", "accountId": "nope", "requestId": "r2"})
	m = tv.wait("錯誤的帳戶", func(m map[string]any) bool { return m["type"] == "codexDoctor" && m["requestId"] == "r2" })
	if m["error"] == nil {
		t.Fatal("不是 Codex 帳戶要回錯誤")
	}
}

func TestCodexCrashDuringPreparationReportsExitCode(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	t.Setenv("MYRIAGENT_FAKE_CODEX_CRASH_ON", "thread/start")
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第一則"})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "排隊的"})
	// 準備這一輪時程序掛掉：錯誤要有結束碼與錯誤輸出（不是只有「程序已結束」）
	tv.waitItem("結束碼與錯誤輸出", func(it map[string]any) bool {
		return it["kind"] == "Error" && htmlHas(it, "結束碼 3") && htmlHas(it, "在 thread/start 當掉")
	})
	// 排隊的那則以新的程序送出（不能送進已經死掉的舊程序）；這次也當掉，同樣回報結束碼
	waitFor(t, "排隊的那則也回報", func() bool {
		tv.mu.Lock()
		defer tv.mu.Unlock()
		n := map[any]bool{}
		for _, m := range tv.msgs {
			if it := itemOf(m); it != nil && it["kind"] == "Error" && htmlHas(it, "結束碼 3") {
				n[it["id"]] = true
			}
		}
		return len(n) == 2
	})
	tv.waitIdle(id)
}

func TestCodexSubagentCardCancelledWhenProcessDies(t *testing.T) {
	_, tv, _ := newCodexTestHub(t)
	id := tv.newCodexDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "子代理掛了"})
	tv.waitItem("子代理卡收成取消", func(it map[string]any) bool {
		return it["kind"] == "Tool" && it["toolKind"] == "agent" && it["status"] == "Cancelled"
	})
	tv.waitIdle(id)
}
