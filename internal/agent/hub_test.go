package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testView 收集中樞送給網頁的訊息
type testView struct {
	t    *testing.T
	h    *Hub
	v    *View
	mu   sync.Mutex
	msgs []map[string]any
}

func newTestHub(t *testing.T) (*Hub, *testView) {
	t.Helper()
	t.Setenv("MYRIAGENT_FAKE_CLI", "claude")
	home := t.TempDir()
	t.Setenv("HOME", home)
	// 讓預設帳戶看起來已登入
	_ = os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"me@example.com"}}`), 0o600)
	root := t.TempDir()
	h := &Hub{root: root, convs: map[string]*Conv{}, views: map[*View]struct{}{}, clis: map[string]*cliInfo{}, flushKick: make(chan struct{}, 1)}
	h.settings = loadSettings(root)
	h.clis["Claude"] = &cliInfo{path: os.Args[0], version: "9.9.9"}
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
	return h, tv
}

func (tv *testView) send(m map[string]any) { tv.h.handle(tv.v, m) }

// wait 等到有一則訊息符合條件（最多 10 秒）
func (tv *testView) wait(what string, ok func(map[string]any) bool) map[string]any {
	tv.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tv.mu.Lock()
		for _, m := range tv.msgs {
			if ok(m) {
				tv.mu.Unlock()
				return m
			}
		}
		tv.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	tv.t.Fatalf("等不到：%s", what)
	return nil
}

func itemOf(m map[string]any) map[string]any {
	if m["type"] != "item" {
		return nil
	}
	it, _ := m["item"].(map[string]any)
	return it
}

func (tv *testView) newDraft() string {
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	tv.wait("init", func(m map[string]any) bool { return m["type"] == "init" })
	tv.send(map[string]any{"cmd": "newConversation", "accountId": nil, "paneId": "p1"})
	m := tv.wait("草稿", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "p1" })
	return m["conversationId"].(string)
}

func TestInitHasAccountsAndProviders(t *testing.T) {
	_, tv := newTestHub(t)
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	m := tv.wait("init", func(m map[string]any) bool { return m["type"] == "init" })
	accounts, _ := m["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("帳戶數 %d", len(accounts))
	}
	a := accounts[0].(map[string]any)
	if a["provider"] != "Claude" || a["status"] != "ready" || a["email"] != "me@example.com" {
		t.Fatalf("預設 Claude 帳戶不對：%v", a)
	}
}

func TestSendStreamsReplyAndEndsTurn(t *testing.T) {
	h, tv := newTestHub(t)
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "你好"})
	tv.wait("回覆", func(m map[string]any) bool {
		it := itemOf(m)
		return it != nil && it["kind"] == "Assistant" && it["status"] == "Done" && strings.Contains(it["html"].(string), "收到：你好")
	})
	tv.wait("一輪結束", func(m map[string]any) bool {
		hd, _ := m["header"].(map[string]any)
		return m["type"] == "header" && hd["running"] == false && hd["lastTurn"] == "輸入 10・輸出 5 tokens"
	})
	// 草稿送出後成為正式對話、標題取第一句、有存檔
	h.mu.Lock()
	c := h.convs[id]
	draft, title := c.Meta.IsDraft, c.Meta.Title
	c.save(h)
	h.mu.Unlock()
	if draft || title != "你好" {
		t.Fatalf("草稿狀態 %v、標題 %q", draft, title)
	}
	if _, err := os.Stat(filepath.Join(h.root, "conversations", id, "items.json")); err != nil {
		t.Fatal("沒有存檔")
	}
	// initialize 的帳戶資訊與模型清單
	tv.wait("帳戶方案", func(m map[string]any) bool {
		if m["type"] != "accounts" {
			return false
		}
		a := m["accounts"].([]any)[0].(map[string]any)
		return a["plan"] == "Claude Max"
	})
}

func TestApprovalAllowRunsTool(t *testing.T) {
	_, tv := newTestHub(t)
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個工具"})
	card := tv.wait("核准卡", func(m map[string]any) bool {
		it := itemOf(m)
		return it != nil && it["kind"] == "Approval" && it["status"] == "Waiting"
	})
	it := itemOf(card)
	if it["title"] != "echo hello" || it["canAllowForSession"] != true {
		t.Fatalf("核准卡內容不對：%v", it)
	}
	tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": it["id"], "choice": "allow"})
	tv.wait("工具結果", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Tool" && x["status"] == "Done" && x["output"] == "hello\n"
	})
	tv.wait("代理接著回覆", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Assistant" && strings.Contains(x["html"].(string), "指令跑完了")
	})
	tv.wait("卡片已允許", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["id"] == it["id"] && x["status"] == "Done" && x["decision"] == "allow"
	})
}

func TestApprovalDenyTellsAgent(t *testing.T) {
	_, tv := newTestHub(t)
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "跑個工具"})
	card := tv.wait("核准卡", func(m map[string]any) bool { x := itemOf(m); return x != nil && x["kind"] == "Approval" })
	tv.send(map[string]any{"cmd": "approve", "conversationId": id, "itemId": itemOf(card)["id"], "choice": "deny", "message": "不要"})
	tv.wait("工具失敗", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Tool" && x["status"] == "Failed" && strings.Contains(x["output"].(string), "不要")
	})
}

func TestQuestionAnswer(t *testing.T) {
	_, tv := newTestHub(t)
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "提問"})
	card := tv.wait("問卷", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Question" && x["status"] == "Waiting"
	})
	tv.send(map[string]any{"cmd": "answer", "conversationId": id, "itemId": itemOf(card)["id"], "answers": map[string]any{"要哪一個？": []any{"乙"}}})
	tv.wait("代理收到答案", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Assistant" && strings.Contains(x["html"].(string), "你選了 乙")
	})
}

func TestQueueWhileRunningAndStop(t *testing.T) {
	_, tv := newTestHub(t)
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "慢一點"})
	tv.wait("執行中", func(m map[string]any) bool {
		hd, _ := m["header"].(map[string]any)
		return m["type"] == "header" && hd["running"] == true
	})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "第二則"})
	tv.wait("排進佇列", func(m map[string]any) bool {
		c, _ := m["composer"].(map[string]any)
		q, _ := c["queue"].([]any)
		return m["type"] == "composer" && len(q) == 1
	})
	tv.send(map[string]any{"cmd": "stop", "conversationId": id})
	// 停止後這一輪結束，佇列接著送出
	tv.wait("佇列送出", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Assistant" && strings.Contains(x["html"].(string), "收到：第二則")
	})
}

func TestReloadKeepsConversation(t *testing.T) {
	h, tv := newTestHub(t)
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "記住我"})
	tv.wait("回覆", func(m map[string]any) bool {
		x := itemOf(m)
		return x != nil && x["kind"] == "Assistant" && x["status"] == "Done"
	})
	h.mu.Lock()
	h.convs[id].save(h)
	root := h.root
	h.mu.Unlock()
	h2 := &Hub{root: root, convs: map[string]*Conv{}}
	h2.loadConversations()
	c := h2.convs[id]
	if c == nil || len(c.Items) != 2 || c.Items[0].Text != "記住我" {
		t.Fatalf("重新載入後的對話不對：%+v", c)
	}
}
