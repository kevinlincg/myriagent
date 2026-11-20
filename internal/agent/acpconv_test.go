package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newACPTestHub：只裝了一種 ACP 代理（假 CLI）的中樞；回傳那一家預設帳戶的 id 與假 CLI 的請求記錄檔
func newACPTestHub(t *testing.T, fake, key string) (*Hub, *testView, string, string) {
	t.Helper()
	t.Setenv("MYRIAGENT_FAKE_CLI", fake)
	home := t.TempDir()
	t.Setenv("HOME", home)
	logFile := filepath.Join(t.TempDir(), "fake.log")
	t.Setenv("MYRIAGENT_FAKE_LOG", logFile)
	root := t.TempDir()
	h := &Hub{root: root, convs: map[string]*Conv{}, views: map[*View]struct{}{}, clis: map[string]*cliInfo{}, flushKick: make(chan struct{}, 1)}
	h.settings = loadSettings(root)
	h.clis[key] = &cliInfo{path: os.Args[0], version: "9.9.9"}
	h.loadAccounts()
	h.loadConversations()
	go h.flushLoop()
	tv := &testView{t: t, h: h}
	tv.v = h.addView(func(b []byte) error {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		tv.mu.Lock()
		tv.msgs = append(tv.msgs, m)
		tv.mu.Unlock()
		return nil
	})
	t.Cleanup(h.shutdown)
	accountID := ""
	h.mu.Lock()
	for _, a := range h.accounts {
		if a.Provider == key {
			accountID = a.ID
		}
	}
	h.mu.Unlock()
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	tv.wait("init", func(m map[string]any) bool { return m["type"] == "init" })
	return h, tv, accountID, logFile
}

func (tv *testView) draftFor(accountID, pane string) string {
	tv.send(map[string]any{"cmd": "newConversation", "accountId": accountID, "paneId": pane})
	m := tv.wait("草稿", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == pane })
	return m["conversationId"].(string)
}

func hasItem(kind, contains string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		it := itemOf(m)
		if it == nil || it["kind"] != kind {
			return false
		}
		html, _ := it["html"].(string)
		return strings.Contains(html, contains)
	}
}

func turnEnded(m map[string]any) bool {
	hd, _ := m["header"].(map[string]any)
	return m["type"] == "header" && hd["running"] == false && hd["lastTurn"] != nil
}

// waitLog 等假 CLI 記下一行含有全部片段的請求
func waitLog(t *testing.T, path string, parts ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range fakeLog(path) {
			ok := true
			for _, p := range parts {
				if !strings.Contains(l, p) {
					ok = false
				}
			}
			if ok {
				return l
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("假 CLI 沒收到 %v；記錄：\n%s", parts, strings.Join(fakeLog(path), "\n"))
	return ""
}

func countLog(path string, parts ...string) int {
	n := 0
	for _, l := range fakeLog(path) {
		ok := true
		for _, p := range parts {
			if !strings.Contains(l, p) {
				ok = false
			}
		}
		if ok {
			n++
		}
	}
	return n
}

var acpFlavors = []struct{ fake, key string }{{"grok", "Grok"}, {"agy", "Antigravity"}}

func TestACPAccountStatus(t *testing.T) {
	want := map[string][2]string{"Grok": {"grok@example.com", "SuperGrok"}, "Antigravity": {"agy@example.com", ""}}
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			h, tv, id, _ := newACPTestHub(t, f.fake, f.key)
			tv.wait("帳戶已登入", func(m map[string]any) bool {
				if m["type"] != "accounts" {
					return false
				}
				for _, x := range m["accounts"].([]any) {
					a := x.(map[string]any)
					if a["id"] == id && a["status"] == "ready" && a["email"] == want[f.key][0] && (want[f.key][1] == "" || a["plan"] == want[f.key][1]) {
						return true
					}
				}
				return false
			})
			h.mu.Lock()
			models := h.account(id).modelOptions()
			h.mu.Unlock()
			if len(models) < 2 || models[0].Value != "default" {
				t.Fatalf("模型清單不對：%+v", models)
			}
			if f.key == "Antigravity" {
				// agy models 的「id-強度」合併成一項、強度由低到高
				if models[1].Value != "gemini-3-flash" || strings.Join(models[1].Efforts, ",") != "low,high" {
					t.Fatalf("agy 模型沒有合併強度：%+v", models[1])
				}
			}
		})
	}
}

func TestAgyAccountSignedOut(t *testing.T) {
	t.Setenv("MYRIAGENT_FAKE_AGY_SIGNED_OUT", "1")
	_, tv, id, _ := newACPTestHub(t, "agy", "Antigravity")
	tv.wait("尚未登入", func(m map[string]any) bool {
		if m["type"] != "accounts" {
			return false
		}
		for _, x := range m["accounts"].([]any) {
			if a := x.(map[string]any); a["id"] == id && a["status"] == "notLoggedIn" {
				return true
			}
		}
		return false
	})
}

func TestACPSendStreamsReply(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			h, tv, acc, logFile := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "你好"})
			tv.wait("思考", hasItem("Thinking", "想一下"))
			tv.wait("回覆", func(m map[string]any) bool {
				return hasItem("Assistant", "收到：你好")(m) && itemOf(m)["status"] == "Done"
			})
			tv.wait("一輪結束", func(m map[string]any) bool {
				hd, _ := m["header"].(map[string]any)
				return turnEnded(m) && hd["lastTurn"] == "輸入 10・輸出 5 tokens"
			})
			h.mu.Lock()
			c := h.convs[id]
			sid, started := c.Meta.SessionID, c.Meta.SessionStarted
			h.mu.Unlock()
			if !started || sid == "" {
				t.Fatalf("對話沒有記下 session：%q %v", sid, started)
			}
			if f.key == "Grok" {
				// grok 的 session id 由 app 預先產生、在 session/new 的 _meta 帶過去
				waitLog(t, logFile, "session/new", `"sessionId":"`+sid+`"`, `"askUserQuestion":true`)
			} else if !strings.HasPrefix(sid, "agy-") {
				t.Fatalf("agy 的 session id 要用 agy 回的：%q", sid)
			}
			// available_commands_update 的指令進帳戶（always-approve 不列）
			h.mu.Lock()
			cmds := h.account(acc).commands
			h.mu.Unlock()
			if len(cmds) != 1 || cmds[0].Name != "compact" {
				t.Fatalf("指令不對：%+v", cmds)
			}
		})
	}
}

func TestACPApprovalAllow(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			_, tv, acc, logFile := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個工具"})
			card := tv.wait("核准卡", func(m map[string]any) bool {
				it := itemOf(m)
				return it != nil && it["kind"] == "Approval" && it["status"] == "Waiting"
			})
			it := itemOf(card)
			if it["title"] != "echo hello" || it["toolKind"] != "bash" || it["canAllowForSession"] != true {
				t.Fatalf("核准卡內容不對：%v", it)
			}
			if f.key == "Grok" && it["allowSessionLabel"] != "本對話的指令都不再詢問" {
				t.Fatalf("grok 的「本對話都允許」說明不對：%v", it["allowSessionLabel"])
			}
			tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": it["id"], "choice": "allow"})
			tv.wait("工具結果", func(m map[string]any) bool {
				x := itemOf(m)
				return x != nil && x["kind"] == "Tool" && x["status"] == "Done" && x["output"] == "hello\n"
			})
			tv.wait("代理接著回覆", hasItem("Assistant", "指令跑完了（allow-once）"))
			tv.wait("卡片已允許", func(m map[string]any) bool {
				x := itemOf(m)
				return x != nil && x["id"] == it["id"] && x["status"] == "Done" && x["decision"] == "allow"
			})
			tv.wait("一輪結束", turnEnded)
			waitLog(t, logFile, "session/prompt")
		})
	}
}

func TestACPApprovalDenyFeedbackBecomesNextMessage(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			_, tv, acc, _ := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個工具"})
			card := tv.wait("核准卡", func(m map[string]any) bool { x := itemOf(m); return x != nil && x["kind"] == "Approval" })
			tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": itemOf(card)["id"], "choice": "deny", "message": "不要"})
			tv.wait("工具已拒絕", func(m map[string]any) bool {
				x := itemOf(m)
				return x != nil && x["kind"] == "Tool" && x["status"] == "Declined"
			})
			tv.wait("代理知道被拒絕", hasItem("Assistant", "好，不跑了"))
			// ACP 的拒絕沒有欄位能帶說明：這一輪結束後當下一則訊息送出
			tv.wait("說明當下一則送出", hasItem("Assistant", "收到：（我剛才拒絕了那個操作）不要"))
		})
	}
}

func TestGrokAllowSessionRemembersKind(t *testing.T) {
	_, tv, acc, logFile := newACPTestHub(t, "grok", "Grok")
	id := tv.draftFor(acc, "p1")
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個工具"})
	card := tv.wait("核准卡", func(m map[string]any) bool { x := itemOf(m); return x != nil && x["kind"] == "Approval" })
	tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": itemOf(card)["id"], "choice": "allowSession"})
	tv.wait("第一次用 allow_always", hasItem("Assistant", "指令跑完了（always-allow）"))
	tv.wait("一輪結束", turnEnded)
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "再跑一次工具"})
	// 第二次 app 代為核准（allow_once），不再出核准卡
	tv.wait("第二次自動放行", hasItem("Assistant", "指令跑完了（allow-once）"))
	tv.mu.Lock()
	cards := 0
	for _, m := range tv.msgs {
		if x := itemOf(m); x != nil && x["kind"] == "Approval" && x["status"] == "Waiting" {
			cards++
		}
	}
	tv.mu.Unlock()
	if cards != 1 {
		t.Fatalf("核准卡出現 %d 次，應該只有第一次", cards)
	}
	_ = logFile
}

func TestGrokAcceptEditsAutoApprovesInsideWorkdir(t *testing.T) {
	_, tv, acc, _ := newACPTestHub(t, "grok", "Grok")
	id := tv.draftFor(acc, "p1")
	tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "permission", "value": "AcceptEdits"})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "編輯一下"})
	tv.wait("自動放行的編輯", hasItem("Assistant", "改好了（allow-once）"))
	edit := tv.wait("編輯卡有差異", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Tool" && x["toolKind"] == "edit" && x["status"] == "Done"
	})
	if d, _ := itemOf(edit)["diff"].([]any); len(d) == 0 {
		t.Fatalf("編輯卡沒有差異：%v", itemOf(edit))
	}
	tv.mu.Lock()
	defer tv.mu.Unlock()
	for _, m := range tv.msgs {
		if x := itemOf(m); x != nil && x["kind"] == "Approval" {
			t.Fatalf("工作資料夾內的編輯不該出核准卡：%v", x)
		}
	}
}

func TestGrokAskModeStillAsksForEdits(t *testing.T) {
	_, tv, acc, _ := newACPTestHub(t, "grok", "Grok")
	id := tv.draftFor(acc, "p1")
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "編輯一下"})
	tv.wait("每步詢問會出核准卡", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Approval" && x["toolKind"] == "edit" && x["allowSessionLabel"] == "本對話的編輯都允許"
	})
}

func TestACPQuestionAnswer(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			_, tv, acc, _ := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "提問"})
			card := tv.wait("問卷", func(m map[string]any) bool {
				x := itemOf(m)
				return x != nil && x["kind"] == "Question" && x["status"] == "Waiting"
			})
			q := itemOf(card)["questions"].([]any)[0].(map[string]any)
			if q["text"] != "要哪一個？" || len(q["options"].([]any)) != 2 {
				t.Fatalf("題目不對：%v", q)
			}
			tv.send(map[string]any{"cmd": "answer", "conversationId": id, "itemId": itemOf(card)["id"], "answers": map[string]any{q["id"].(string): []any{"乙"}}})
			tv.wait("代理收到答案", hasItem("Assistant", "你選了 乙"))
		})
	}
}

func TestGrokQuestionOtherGoesToNotes(t *testing.T) {
	_, tv, acc, _ := newACPTestHub(t, "grok", "Grok")
	id := tv.draftFor(acc, "p1")
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "提問"})
	card := tv.wait("問卷", func(m map[string]any) bool { x := itemOf(m); return x != nil && x["kind"] == "Question" })
	tv.send(map[string]any{"cmd": "answer", "conversationId": id, "itemId": itemOf(card)["id"], "answers": map[string]any{"要哪一個？": []any{"丙"}}})
	tv.wait("自填的答案寫成 Other＋notes", hasItem("Assistant", "你選了 Other／丙"))
}

func TestGrokPlanApprovalSwitchesMode(t *testing.T) {
	h, tv, acc, logFile := newACPTestHub(t, "grok", "Grok")
	id := tv.draftFor(acc, "p1")
	tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "permission", "value": "Plan"})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "先做計畫"})
	tv.wait("待辦清單", func(m map[string]any) bool {
		x := itemOf(m)
		p, _ := x["plan"].([]any)
		return x != nil && x["kind"] == "Plan" && len(p) == 2
	})
	card := tv.wait("計畫確認卡", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "PlanApproval" && x["status"] == "Waiting" && strings.Contains(x["detailHtml"].(string), "讀程式")
	})
	waitLog(t, logFile, "session/set_mode", `"modeId":"plan"`)
	tv.send(map[string]any{"cmd": "approvePlan", "conversationId": id, "itemId": itemOf(card)["id"], "choice": "acceptEdits"})
	tv.wait("照計畫執行", hasItem("Assistant", "開始執行"))
	tv.wait("一輪結束", turnEnded)
	// grok 同意計畫後自己回到 default（current_mode_update）：app 記下來，不必再送 set_mode default（送了也無妨，看先後）
	h.mu.Lock()
	perm := h.convs[id].Meta.Permission
	plan := h.convs[id].acp().applied.val("plan")
	h.mu.Unlock()
	if perm != "AcceptEdits" || plan != "default" {
		t.Fatalf("同意計畫後權限模式是 %q、程序上的模式是 %q", perm, plan)
	}
}

func TestACPStopCancelsAndSendsQueue(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			_, tv, acc, logFile := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "慢一點"})
			tv.wait("執行中的工具", func(m map[string]any) bool {
				x := itemOf(m)
				return x != nil && x["kind"] == "Tool" && x["status"] == "Running"
			})
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第二則"})
			tv.wait("排進佇列（不能插進這一輪）", func(m map[string]any) bool {
				c, _ := m["composer"].(map[string]any)
				q, _ := c["queue"].([]any)
				return m["type"] == "composer" && len(q) == 1
			})
			tv.send(map[string]any{"cmd": "stop", "conversationId": id})
			waitLog(t, logFile, "session/cancel")
			tv.wait("工具標成已停止", func(m map[string]any) bool {
				x := itemOf(m)
				return x != nil && x["kind"] == "Tool" && x["status"] == "Cancelled" && x["output"] == "已停止"
			})
			tv.wait("佇列接著送出", hasItem("Assistant", "收到：第二則"))
		})
	}
}

func TestACPStopKillsStuckAgentAndResumes(t *testing.T) {
	old := acpCancelTimeout
	acpCancelTimeout = 300 * time.Millisecond
	t.Cleanup(func() { acpCancelTimeout = old })
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			h, tv, acc, logFile := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "卡住了"})
			tv.wait("回覆中", hasItem("Assistant", "慢慢來"))
			tv.send(map[string]any{"cmd": "stop", "conversationId": id})
			tv.wait("結束程序後這一輪結束", func(m map[string]any) bool {
				hd, _ := m["header"].(map[string]any)
				return m["type"] == "header" && hd["running"] == false
			})
			h.mu.Lock()
			alive := h.alive(h.convs[id])
			sid := h.convs[id].Meta.SessionID
			h.mu.Unlock()
			if alive {
				t.Fatal("卡住的程序應該已經結束")
			}
			// 下一則訊息重開程序、以 session/resume 續接同一個對話
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "還在嗎"})
			tv.wait("續接", hasItem("Assistant", "收到：還在嗎（續接）"))
			waitLog(t, logFile, "session/resume", `"sessionId":"`+sid+`"`)
		})
	}
}

func TestGrokResumeLostStartsNewWithRecap(t *testing.T) {
	h, tv, acc, logFile := newACPTestHub(t, "grok", "Grok")
	id := tv.draftFor(acc, "p1")
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "記住紅色"})
	tv.wait("一輪結束", turnEnded)
	h.mu.Lock()
	c := h.convs[id]
	h.closeSession(c)
	c.Meta.SessionID = "gone-123"
	h.mu.Unlock()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "還記得嗎"})
	tv.wait("改開新對話的提示", hasItem("Notice", "找不到這個對話先前的紀錄"))
	tv.wait("回覆", hasItem("Assistant", "收到：還記得嗎"))
	line := waitLog(t, logFile, "session/prompt", "previous_conversation", "記住紅色")
	if strings.Contains(line, "（續接）") {
		t.Fatal("不該是續接")
	}
	h.mu.Lock()
	sid := h.convs[id].Meta.SessionID
	h.mu.Unlock()
	if sid == "gone-123" || sid == "" {
		t.Fatalf("session id 沒換成新的：%q", sid)
	}
}

func TestACPUnexpectedExitShowsError(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			_, tv, acc, _ := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "崩潰吧"})
			tv.wait("錯誤", func(m map[string]any) bool {
				x := itemOf(m)
				return x != nil && x["kind"] == "Error" && strings.Contains(x["html"].(string), "程序意外結束") && strings.Contains(x["html"].(string), "boom")
			})
			tv.wait("不再執行中", func(m map[string]any) bool {
				hd, _ := m["header"].(map[string]any)
				return m["type"] == "header" && hd["running"] == false
			})
		})
	}
}

func TestACPSetOptionsLive(t *testing.T) {
	cases := map[string][5]string{
		"Grok":        {"session/set_config_option", `"configId":"model"`, `"value":"grok-code"`, `"configId":"reasoning_effort"`, `"value":"low"`},
		"Antigravity": {"session/set_model", `"modelId":"claude-sonnet-4-6"`, `"sessionId"`, `"configId":"thought_level"`, `"value":"low"`},
	}
	models := map[string]string{"Grok": "grok-code", "Antigravity": "claude-sonnet-4-6"}
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			_, tv, acc, logFile := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "你好"})
			tv.wait("一輪結束", turnEnded)
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "model", "value": models[f.key]})
			waitLog(t, logFile, cases[f.key][0], cases[f.key][1], cases[f.key][2])
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "effort", "value": "low"})
			waitLog(t, logFile, "session/set_config_option", cases[f.key][3], cases[f.key][4])
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "permission", "value": "FullAccess"})
			if f.key == "Grok" {
				waitLog(t, logFile, "_x.ai/yolo_mode_changed", `"permission_mode":"always-approve"`, `"yolo_mode":true`)
			} else {
				waitLog(t, logFile, "session/set_mode", `"modeId":"auto"`)
			}
			// 程序沒有重開：initialize 只有對話一次（grok 另有帳戶查詢的一次）
			want := map[string]int{"Grok": 2, "Antigravity": 1}[f.key]
			if n := countLog(logFile, f.fake+" initialize"); n != want {
				t.Fatalf("initialize 送了 %d 次", n)
			}
		})
	}
}

func TestACPFullAccessAutoApproves(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			_, tv, acc, _ := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "permission", "value": "FullAccess"})
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個工具"})
			tv.wait("自動放行", hasItem("Assistant", "指令跑完了（allow-once）"))
		})
	}
}

func TestACPHeaderSteerFalseAndModes(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			_, tv, acc, _ := newACPTestHub(t, f.fake, f.key)
			tv.send(map[string]any{"cmd": "newConversation", "accountId": acc, "paneId": "p9"})
			m := tv.wait("草稿", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "p9" })
			hd := m["header"].(map[string]any)
			if hd["steer"] != false {
				t.Fatal("ACP 代理不能插進執行中的一輪")
			}
			if modes := hd["permissionModes"].([]any); len(modes) != 5 {
				t.Fatalf("權限模式 %d 個", len(modes))
			}
		})
	}
}

// 代理自己進入規劃模式後，使用者換回每步詢問要真的送出（不能被「已套用」的記錄擋掉）
func TestACPAgentModeChangeThenUserSwitchBack(t *testing.T) {
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			h, tv, acc, logFile := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "你自己規劃"})
			tv.wait("一輪結束", turnEnded)
			h.mu.Lock()
			perm := h.convs[id].Meta.Permission
			h.mu.Unlock()
			if perm != "Plan" {
				t.Fatalf("代理進入規劃模式後畫面是 %q", perm)
			}
			before := countLog(logFile, "session/set_mode", `"modeId":"default"`)
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "permission", "value": "Ask"})
			deadline := time.Now().Add(10 * time.Second)
			for countLog(logFile, "session/set_mode", `"modeId":"default"`) == before {
				if time.Now().After(deadline) {
					t.Fatalf("換回每步詢問沒有送出 set_mode default；記錄：\n%s", strings.Join(fakeLog(logFile), "\n"))
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// 對話一開始就指定模型與推理強度，之後換回「預設」要切回代理的預設模型與強度
func TestACPSwitchBackToDefaultModelAndEffort(t *testing.T) {
	want := map[string][4]string{
		// 指定的模型、換回預設時送的方法、預設模型、推理強度的設定 id
		"Grok":        {"grok-code", "session/set_config_option", `"value":"grok-4"`, "reasoning_effort"},
		"Antigravity": {"claude-sonnet-4-6", "session/set_model", `"modelId":"gemini-3-flash"`, "thought_level"},
	}
	for _, f := range acpFlavors {
		t.Run(f.key, func(t *testing.T) {
			w := want[f.key]
			_, tv, acc, logFile := newACPTestHub(t, f.fake, f.key)
			id := tv.draftFor(acc, "p1")
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "model", "value": w[0]})
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "effort", "value": "low"})
			tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "你好"})
			tv.wait("一輪結束", turnEnded)
			waitLog(t, logFile, "session/set_config_option", `"configId":"`+w[3]+`"`, `"value":"low"`)
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "model", "value": "default"})
			waitLog(t, logFile, w[1], w[2])
			tv.send(map[string]any{"cmd": "setOption", "conversationId": id, "key": "effort", "value": "default"})
			waitLog(t, logFile, "session/set_config_option", `"configId":"`+w[3]+`"`, `"value":"high"`)
		})
	}
}
