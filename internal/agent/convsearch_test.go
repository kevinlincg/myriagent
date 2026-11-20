package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSearchTerms(t *testing.T) {
	if len(searchTerms("")) != 0 || len(searchTerms("  \t\n　")) != 0 {
		t.Fatal("全是空白（含全形）沒有關鍵字")
	}
	if got := searchTerms(" 部署　失敗\nHUB-PC "); !reflect.DeepEqual(got, []string{"部署", "失敗", "HUB-PC"}) {
		t.Fatalf("切法：%v", got)
	}
	if got := searchTerms("API api Api key"); !reflect.DeepEqual(got, []string{"API", "key"}) {
		t.Fatalf("留第一次出現的寫法：%v", got)
	}
	var many []string
	for i := 0; i < 12; i++ {
		many = append(many, "t"+itoa(i))
	}
	if len(searchTerms(strings.Join(many, " "))) != searchMaxTerms {
		t.Fatal("最多 8 個")
	}
	long := strings.Repeat("a", searchMaxQuery) + " 後面"
	if got := searchTerms(long); len(got) != 1 || got[0] != strings.Repeat("a", searchMaxQuery) {
		t.Fatal("整串先截到上限")
	}
}

func TestSearchMatchRules(t *testing.T) {
	sn, ok := searchMatch("部署 MyriagentHub", []string{"你好", "沒有關鍵字"}, searchTerms("myriagenthub"))
	if !ok || sn != nil {
		t.Fatal("只有標題符合：片段是 nil")
	}
	sn, ok = searchMatch("別的標題", []string{"第一則沒有", "這裡有 B 也有 A", "後面也有 A"}, searchTerms("a b"))
	if !ok || *sn != "這裡有 B 也有 A" {
		t.Fatalf("片段取依序第一則：%v", sn)
	}
	terms := searchTerms("部署 失敗")
	if sn, ok = searchMatch("部署到 HUB-PC", []string{"結果失敗了"}, terms); !ok || *sn != "結果失敗了" {
		t.Fatal("關鍵字可以分散在標題與內容")
	}
	if _, ok = searchMatch("部署到 HUB-PC", []string{"成功"}, terms); ok {
		t.Fatal("失敗哪裡都沒有")
	}
	if sn, ok = searchMatch("", []string{"先部署", "再看失敗"}, terms); !ok || *sn != "先部署" {
		t.Fatal("片段取第一則")
	}
	if _, ok = searchMatch("標題", []string{"內容"}, nil); ok {
		t.Fatal("沒有關鍵字不算符合")
	}
	if sn, ok = searchMatch("", []string{"Run DOTNET build"}, searchTerms("dotnet")); !ok || *sn != "Run DOTNET build" {
		t.Fatal("不分大小寫")
	}
}

func TestSearchSnippet(t *testing.T) {
	before := strings.Repeat("前", 40)
	after := strings.Repeat("後", 120)
	text := []rune(before + "\r\n\r\n  關鍵字\n\n" + after)
	idx := indexOfFold(text, []rune("關鍵字"))
	s := searchSnippet(text, idx, 3)
	if !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || !strings.Contains(s, " 關鍵字 後") {
		t.Fatalf("片段：%q", s)
	}
	body := []rune(strings.Trim(s, "…"))
	k := indexOfFold(body, []rune("關鍵字"))
	if k != searchSnippetBefore || len(body)-k != searchSnippetAfter {
		t.Fatalf("前 %d 字、後 %d 字", k, len(body)-k)
	}
	if got := searchSnippet([]rune("短短的關鍵字句子"), 3, 3); got != "短短的關鍵字句子" {
		t.Fatal(got)
	}
	if got := searchSnippet([]rune("   \n關鍵字\n\n   "), 4, 3); got != "關鍵字" {
		t.Fatal(got)
	}
	sp := strings.Repeat(" ", 1000)
	if got := searchSnippet([]rune(sp+"關鍵字"+sp), 1000, 3); got != "關鍵字" {
		t.Fatalf("視窗外只有空白：%q", got)
	}
	if got := searchSnippet([]rune("前"+sp+"關鍵字"), 1001, 3); got != "…關鍵字" {
		t.Fatalf("視窗外有字：%q", got)
	}
	term := strings.Repeat("字", 150)
	if got := searchSnippet([]rune("前面 "+term+" 後面"), 3, 150); got != "前面 "+term+"…" {
		t.Fatal("關鍵字比上限長時整個留著")
	}
}

// 中樞：搜尋內容只看使用者訊息與回覆、依清單順序、只回給發出的視窗
func TestSearchConversationsHub(t *testing.T) {
	h, tv := newTestHub(t)
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	tv.wait("init", func(m map[string]any) bool { return m["type"] == "init" })
	h.mu.Lock()
	mk := func(id, title string, updated int64, items ...*Item) {
		c := &Conv{Meta: ConvMeta{ID: id, Title: title, Provider: "Claude", UpdatedAt: updated}, Items: items}
		c.init()
		h.convs[id] = c
	}
	mk("c1", "部署", 100, &Item{Kind: "User", Text: "上次部署失敗的原因"})
	mk("c2", "閒聊", 200, &Item{Kind: "Tool", Text: "部署 失敗"}, &Item{Kind: "Assistant", Text: "沒事"})
	mk("c3", "部署失敗", 300)
	h.mu.Unlock()
	tv.send(map[string]any{"cmd": "searchConversations", "requestId": "cs1", "query": "部署 失敗"})
	m := tv.wait("搜尋結果", func(m map[string]any) bool { return m["type"] == "conversationSearch" && m["requestId"] == "cs1" })
	hits := m["hits"].([]any)
	if len(hits) != 2 {
		t.Fatalf("結果：%v", hits)
	}
	first, second := hits[0].(map[string]any), hits[1].(map[string]any)
	if first["conversationId"] != "c3" || first["snippet"] != nil || second["conversationId"] != "c1" || second["snippet"] != "上次部署失敗的原因" {
		t.Fatalf("依清單順序（新的在前）、只有標題符合時片段是 null：%v", hits)
	}
	tv.send(map[string]any{"cmd": "searchConversations", "requestId": "cs2", "query": "  "})
	m = tv.wait("空查詢", func(m map[string]any) bool { return m["requestId"] == "cs2" })
	if len(m["hits"].([]any)) != 0 {
		t.Fatal("沒有關鍵字回空")
	}
}

func TestListFilesAndFileSearchHub(t *testing.T) {
	h, tv := newTestHub(t)
	id := tv.newDraft()
	dir := t.TempDir()
	writeTestFile(t, dir, "src/app.js", "x")
	writeTestFile(t, dir, "pic.png", "png")
	writeTestFile(t, dir, "node_modules/app.js", "")
	tv.send(map[string]any{"cmd": "setFolder", "conversationId": id, "path": dir})
	tv.send(map[string]any{"cmd": "listFiles", "conversationId": id, "requestId": "r1", "path": nil})
	m := tv.wait("files", func(m map[string]any) bool { return m["type"] == "files" && m["requestId"] == "r1" })
	if m["path"] != dir || m["root"] != dir || m["error"] != nil {
		t.Fatalf("files：%v", m)
	}
	entries := m["entries"].([]any)
	var pic map[string]any
	for _, e := range entries {
		if e.(map[string]any)["name"] == "pic.png" {
			pic = e.(map[string]any)
		}
	}
	if pic == nil || !strings.HasPrefix(pic["url"].(string), "/media?path=") || pic["thumbUrl"] == nil {
		t.Fatalf("圖片要有網址：%v", pic)
	}
	tv.send(map[string]any{"cmd": "listFiles", "conversationId": id, "requestId": "r2", "path": filepath.Join(dir, "沒有")})
	m = tv.wait("錯誤", func(m map[string]any) bool { return m["type"] == "files" && m["requestId"] == "r2" })
	if m["error"] == nil || len(m["entries"].([]any)) != 0 {
		t.Fatal("讀不到時回帶 error 的 files")
	}
	tv.send(map[string]any{"cmd": "fileSearch", "conversationId": id, "requestId": "r3", "query": "app"})
	m = tv.wait("fileSearch", func(m map[string]any) bool { return m["type"] == "fileSearch" && m["requestId"] == "r3" })
	res := m["results"].([]any)
	if len(res) != 1 || res[0].(map[string]any)["relativePath"] != "src/app.js" || res[0].(map[string]any)["fullPath"] != filepath.Join(dir, "src/app.js") {
		t.Fatalf("搜尋結果：%v", res)
	}
	// 預設 workspace 不存在時建立
	tv.send(map[string]any{"cmd": "newConversation", "accountId": nil, "paneId": "p2"})
	id2 := tv.wait("第二個草稿", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "p2" })["conversationId"].(string)
	h.mu.Lock()
	ws := h.convs[id2].workDir(h)
	h.mu.Unlock()
	tv.send(map[string]any{"cmd": "listFiles", "conversationId": id2, "requestId": "r4"})
	tv.wait("workspace", func(m map[string]any) bool { return m["requestId"] == "r4" && m["error"] == nil })
	if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
		t.Fatal("對話自己的 workspace 要建立")
	}
}

func TestOpenPathSafety(t *testing.T) {
	h, tv := newTestHub(t)
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	var mu sync.Mutex
	var opened, revealed []string
	oldOpen, oldReveal := shellOpen, shellReveal
	shellOpen = func(p string) { mu.Lock(); opened = append(opened, p); mu.Unlock() }
	shellReveal = func(p string) { mu.Lock(); revealed = append(revealed, p); mu.Unlock() }
	t.Cleanup(func() { shellOpen, shellReveal = oldOpen, oldReveal })
	dir := t.TempDir()
	pdf := writeTestFile(t, dir, "a.pdf", "")
	sh := writeTestFile(t, dir, "run.sh", "")
	h.openLocalPath(tv.v, pdf)
	h.openLocalPath(tv.v, sh)
	h.openLocalPath(tv.v, "relative.pdf")
	h.openLink(tv.v, "javascript:alert(1)")
	h.openLink(tv.v, "https://example.com/x")
	mu.Lock()
	o, r := append([]string{}, opened...), append([]string{}, revealed...)
	mu.Unlock()
	if !reflect.DeepEqual(o, []string{pdf, "https://example.com/x"}) || !reflect.DeepEqual(r, []string{sh}) {
		t.Fatalf("開：%v、選取：%v", o, r)
	}
	tv.wait("不直接開的提示", func(m map[string]any) bool {
		return m["type"] == "toast" && strings.Contains(m["text"].(string), "不直接開啟")
	})
	tv.wait("相對路徑", func(m map[string]any) bool {
		return m["type"] == "toast" && strings.Contains(m["text"].(string), "只能開啟")
	})
}
