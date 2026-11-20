package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kevinlincg/myriagent/internal/acp"
)

// ACP 規則的單元測試（工具卡、權限、模型清單、帳戶的家）

func TestDescribeAcpToolGrokMeta(t *testing.T) {
	d := describeAcpTool(map[string]any{
		"toolCallId": "t1", "title": "run_terminal_command", "kind": "execute",
		"rawInput": map[string]any{"command": "go test ./...", "description": "跑測試"},
		"_meta":    map[string]any{"x.ai/tool": map[string]any{"name": "run_terminal_command"}},
	})
	if d.Name != "執行指令" || d.Kind != "bash" || d.Title != "go test ./..." || d.Detail != "跑測試\n\ngo test ./..." {
		t.Fatalf("grok 指令卡不對：%+v", d)
	}
	search := describeAcpTool(map[string]any{"_meta": map[string]any{"x.ai/tool": map[string]any{"name": "grep"}}, "rawInput": map[string]any{"pattern": "TODO", "path": "/src"}})
	if search.Kind != "search" || search.Title != "TODO  /src" {
		t.Fatalf("搜尋卡要同時寫樣式與資料夾：%+v", search)
	}
}

func TestDescribeAcpToolAgyKindAndDiff(t *testing.T) {
	d := describeAcpTool(map[string]any{"title": "Run command: git status", "kind": "execute", "rawInput": map[string]any{"CommandLine": "git status"}})
	if d.Name != "執行指令" || d.Kind != "bash" || d.Title != "git status" {
		t.Fatalf("agy 指令卡不對：%+v", d)
	}
	e := describeAcpTool(map[string]any{"title": "Edit a.go", "kind": "edit", "locations": []any{map[string]any{"path": "/w/a.go"}},
		"content": []any{map[string]any{"type": "diff", "path": "/w/a.go", "oldText": "a\nb\n", "newText": "a\nc\n"}}})
	if e.Kind != "edit" || e.File != "/w/a.go" || e.Stats == nil || e.Stats.Add != 1 || e.Stats.Del != 1 {
		t.Fatalf("編輯卡不對：%+v", e)
	}
	n := describeAcpTool(map[string]any{"kind": "edit", "content": []any{map[string]any{"type": "diff", "path": "/w/new.go", "newText": "x\ny\n"}}})
	if n.Stats == nil || n.Stats.Add != 2 || n.Stats.Del != 0 {
		t.Fatalf("新檔案的差異不對：%+v", n.Stats)
	}
	mcp := describeAcpTool(map[string]any{"_meta": map[string]any{"x.ai/tool": map[string]any{"name": "mcp__github__search"}}})
	if mcp.Kind != "mcp" {
		t.Fatalf("MCP 工具：%+v", mcp)
	}
}

func TestAcpToolOutput(t *testing.T) {
	if got := acpToolOutput(map[string]any{"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "a\r\nb"}}}}); got != "a\nb" {
		t.Fatalf("content 文字：%q", got)
	}
	// 指令的 content 有但是空的：不退回 rawOutput（output_for_prompt 是給模型看的）
	if got := acpToolOutput(map[string]any{"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": ""}}}, "rawOutput": map[string]any{"output_for_prompt": "exit: 0"}}); got != "" {
		t.Fatalf("不該退回 rawOutput：%q", got)
	}
	if got := acpToolOutput(map[string]any{"rawOutput": map[string]any{"error": "沒有權限"}}); got != "沒有權限" {
		t.Fatalf("錯誤訊息：%q", got)
	}
}

func TestAcpMergeKeepsMeta(t *testing.T) {
	call := map[string]any{"title": "a", "_meta": map[string]any{"x.ai/tool": map[string]any{"name": "edit"}}}
	acpMerge(call, map[string]any{"sessionUpdate": "tool_call_update", "title": nil, "kind": "edit", "_meta": map[string]any{"x.ai/tool": map[string]any{"input": "x"}}})
	tool := call["_meta"].(map[string]any)["x.ai/tool"].(map[string]any)
	if call["title"] != "a" || call["kind"] != "edit" || tool["name"] != "edit" || tool["input"] != "x" || call["sessionUpdate"] != nil {
		t.Fatalf("合併不對：%v", call)
	}
}

func TestParseAcpPermissionOptions(t *testing.T) {
	p := parseAcpPermission(map[string]any{"toolCall": map[string]any{"toolCallId": "c", "kind": "execute"}, "options": []any{
		map[string]any{"optionId": "a1", "kind": "allow_once"}, map[string]any{"optionId": "r1", "kind": "reject_once"}}})
	if p.outcome("allowSession")["outcome"].(map[string]any)["optionId"] != "a1" {
		t.Fatal("沒有 allow_always 時「本對話都允許」退回允許一次")
	}
	if p.outcome("deny")["outcome"].(map[string]any)["optionId"] != "r1" {
		t.Fatal("拒絕")
	}
	none := parseAcpPermission(map[string]any{"toolCall": map[string]any{}, "options": []any{}})
	if none.outcome("allow")["outcome"].(map[string]any)["outcome"] != "cancelled" {
		t.Fatal("沒有選項時以 cancelled 回覆")
	}
	// agy 沒附 options：用 ACP 的預設代碼
	def := parseAcpPermission(map[string]any{"toolCall": map[string]any{}})
	if def.allowOnce != "allow_once" || def.allowAlways != "allow_always" || def.rejectOnce != "reject_once" {
		t.Fatalf("預設代碼：%+v", def)
	}
}

func TestAcpCanAutoApproveEdit(t *testing.T) {
	wd := t.TempDir()
	conv := t.TempDir()
	perm := func(kind string, paths ...string) acpPermission {
		return acpPermission{kind: kind, allowOnce: "ok", paths: paths}
	}
	if !acpCanAutoApproveEdit(perm("edit", filepath.Join(wd, "src", "a.go")), wd, conv) {
		t.Fatal("工作資料夾內的編輯要放行")
	}
	if !acpCanAutoApproveEdit(perm("edit", filepath.Join(conv, "note.md")), wd, conv) {
		t.Fatal("對話資料夾內的編輯要放行")
	}
	cases := map[string]acpPermission{
		"指令不放行":    perm("execute", filepath.Join(wd, "a")),
		"沒有路徑":     perm("edit"),
		"../ 跳出":   perm("edit", filepath.Join(wd, "..", "x")),
		".git 不放行": perm("edit", filepath.Join(wd, ".git", "hooks", "pre-commit")),
		"相對路徑":     perm("edit", "a.go"),
		"一個在外面":    perm("edit", filepath.Join(wd, "a"), "/etc/passwd"),
		"資料夾本身":    perm("edit", wd),
	}
	for name, p := range cases {
		if acpCanAutoApproveEdit(p, wd, conv) {
			t.Fatalf("%s：不該放行", name)
		}
	}
	// 工作資料夾裡指到外面的符號連結
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(wd, "link")); err != nil {
		t.Fatal(err)
	}
	if acpCanAutoApproveEdit(perm("edit", filepath.Join(wd, "link", "a")), wd, conv) {
		t.Fatal("經過符號連結的不放行")
	}
	if acpCanAutoApproveEdit(acpPermission{kind: "edit", paths: []string{filepath.Join(wd, "a")}}, wd, conv) {
		t.Fatal("沒有允許一次的選項就不能代為核准")
	}
}

func TestParseAgyModelsGroupsEfforts(t *testing.T) {
	out := parseAgyModels("gemini-3.8-flash-high\tGemini 3.8 Flash (High)\nFetching models\ngemini-3.8-flash-low\tGemini 3.8 Flash (Low)\nclaude-opus-4-6-thinking Claude Opus 4.6 (Thinking)\ngemini-3.8-flash-low\tdup\n")
	if len(out) != 2 {
		t.Fatalf("模型 %d 個：%+v", len(out), out)
	}
	if out[0].Value != "gemini-3.8-flash" || out[0].Label != "Gemini 3.8 Flash" || strings.Join(out[0].Efforts, ",") != "low,high" {
		t.Fatalf("合併強度不對：%+v", out[0])
	}
	if out[1].Value != "claude-opus-4-6-thinking" || out[1].Label != "Claude Opus 4.6 (Thinking)" || len(out[1].Efforts) != 0 {
		t.Fatalf("空白分隔的行：%+v", out[1])
	}
}

func TestAgyLogEmailAndStatus(t *testing.T) {
	log := "You are not logged into Antigravity.\nI1 server_oauth.go:229] OAuth: authenticated successfully as a@x.com\nI2 server_oauth.go:229] OAuth: authenticated successfully as b@y.org\n"
	if e := parseAgySignedInEmail(log); e != "b@y.org" {
		t.Fatalf("取最後一行：%q", e)
	}
	if e := parseAgySignedInEmail("nothing"); e != "" {
		t.Fatal("沒有就回空")
	}
	if s, _ := evaluateAgyModels(0, "", "Please sign in to continue", false); s != "notLoggedIn" {
		t.Fatal(s)
	}
	if s, _ := evaluateAgyModels(1, "", "panic", false); s != "error" {
		t.Fatal(s)
	}
	if s, _ := evaluateAgyModels(0, "", "", true); s != "error" {
		t.Fatal(s)
	}
}

func TestGrokModelsAndAuth(t *testing.T) {
	models, current := parseGrokModels(map[string]any{"currentModelId": "b", "availableModels": []any{
		map[string]any{"modelId": "a", "name": "A"},
		map[string]any{"modelId": "b", "name": "B", "_meta": map[string]any{"reasoningEfforts": []any{map[string]any{"value": "high"}, map[string]any{"value": "low"}}}},
	}})
	if current != "b" || len(models) != 3 || models[0].Value != "default" || strings.Join(models[0].Efforts, ",") != "low,high" || !strings.Contains(models[0].Description, "B") {
		t.Fatalf("grok 模型：%+v", models)
	}
	if s, e := grokAuthStatus(map[string]any{"email": "x@y.z"}); s != "ready" || e != "x@y.z" {
		t.Fatal(s)
	}
	if s, _ := grokAuthStatus(map[string]any{"methodId": "xai-api-key"}); s != "apiKey" {
		t.Fatal(s)
	}
	if s, _ := grokAuthStatus(map[string]any{}); s != "notLoggedIn" {
		t.Fatal(s)
	}
	g := grokFlavor{}
	if !g.isSessionNotFound(&acp.Error{DataCode: "FS_NOT_FOUND", Text: "Path not found"}) || !g.isSessionNotFound(&acp.Error{Text: "Unknown session id"}) || g.isSessionNotFound(&acp.Error{Text: "rate limited"}) {
		t.Fatal("找不到對話的判斷")
	}
	if !supportsEffort(models, "", "low") || supportsEffort(models, "b", "max") || !supportsEffort(models, "a", "max") {
		t.Fatal("思考強度支援的判斷")
	}
}

func TestQuestionAnswerShapes(t *testing.T) {
	qs := []Question{{ID: "q", Options: []QuestionOption{{Label: "甲"}}}}
	g := grokQuestionAnswer(qs, map[string][]string{"q": {"甲", "自己寫的"}})
	labels := g["answers"].(map[string]any)["q"].([]any)
	if len(labels) != 2 || labels[1] != "Other" || g["annotations"].(map[string]any)["q"].(map[string]any)["notes"] != "自己寫的" {
		t.Fatalf("grok 答案：%v", g)
	}
	el := parseElicitation(map[string]any{"message": "選", "requestedSchema": map[string]any{"required": []any{"env"}, "properties": map[string]any{
		"addons": map[string]any{"type": "array", "title": "元件", "items": map[string]any{"enum": []any{"redis", "cdn"}}},
		"env":    map[string]any{"type": "string", "description": "環境？", "oneOf": []any{map[string]any{"const": "prod", "title": "正式"}}},
	}}})
	if len(el) != 2 || el[0].ID != "env" || el[0].Text != "環境？" || el[0].Options[0].Label != "prod" || el[0].Options[0].Description != "正式" || !el[1].Multi || len(el[1].Options) != 2 {
		t.Fatalf("elicitation 題目：%+v", el)
	}
	acc := elicitationAccept(el, map[string][]string{"env": {"prod"}, "addons": {"redis", "cdn"}})
	content := acc["content"].(map[string]any)
	if acc["action"] != "accept" || content["env"] != "prod" || len(content["addons"].([]any)) != 2 {
		t.Fatalf("elicitation 答案：%v", acc)
	}
	if one := parseElicitation(map[string]any{"message": "說說看"}); len(one) != 1 || one[0].Text != "說說看" {
		t.Fatalf("沒有 schema 的提問：%+v", one)
	}
}

func TestDenyFeedbackMessage(t *testing.T) {
	if got := buildDenyFeedback([]string{"不要"}); got != "（我剛才拒絕了那個操作）不要" {
		t.Fatal(got)
	}
	if got := buildDenyFeedback([]string{"甲", "乙"}); got != "（我剛才拒絕了這幾個操作）\n1. 甲\n2. 乙" {
		t.Fatal(got)
	}
}

func TestEnsureGrokIsolatedHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "grok")
	ensureGrokIsolatedHome(home)
	b, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(b) != grokIsolatedConfig {
		t.Fatalf("新的 config.toml：%q", b)
	}
	_ = os.WriteFile(filepath.Join(home, "config.toml"), []byte("[plugins]\nx = 1"), 0o600)
	ensureGrokIsolatedHome(home)
	b, _ = os.ReadFile(filepath.Join(home, "config.toml"))
	if string(b) != "[plugins]\nx = 1\n\n"+grokIsolatedConfig {
		t.Fatalf("補上 [claude_compat]：%q", b)
	}
	ensureGrokIsolatedHome(home)
	b2, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(b2) != string(b) {
		t.Fatal("已經有就不再補")
	}
	env := grokEnv(&Account{Provider: "Grok", ConfigDir: home}, nil)
	if !contains(env, "GROK_HOME="+home) || !contains(env, "GROK_DISABLE_AUTOUPDATER=1") {
		t.Fatal("隔離帳戶的環境")
	}
}

func TestAgyShadowHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	old := agyKeychain
	var keychainCalls []string
	agyKeychain = func(home string) error { keychainCalls = append(keychainCalls, home); return nil }
	t.Cleanup(func() { agyKeychain = old })

	real := t.TempDir()
	t.Setenv("HOME", real)
	t.Setenv("ZDOTDIR", "/my/zdot")
	t.Setenv("BASH_ENV", "")
	for _, d := range []string{".ssh", "Library/Preferences", "Library/Keychains", ".gemini/config", ".gemini/antigravity-cli/skills", ".gemini/antigravity-cli/conversations"} {
		_ = os.MkdirAll(filepath.Join(real, d), 0o700)
	}
	_ = os.WriteFile(filepath.Join(real, ".gitconfig"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(real, ".gemini/antigravity-cli/settings.json"), []byte("{}"), 0o600)

	home := filepath.Join(t.TempDir(), agyHomeFolder)
	a := &Account{ID: "a1", Provider: "Antigravity", ConfigDir: home}
	if !agyShadowed(a) || agyShadowed(&Account{Provider: "Antigravity", IsSystemDefault: true}) || agyShadowed(&Account{Provider: "Grok", ConfigDir: home}) {
		t.Fatal("只有 agy 的隔離帳戶用自己的家")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := agyEnv(a, nil)
	link := func(p string) string { s, _ := os.Readlink(filepath.Join(home, p)); return s }
	if link(".ssh") != filepath.Join(real, ".ssh") || link(".gitconfig") != filepath.Join(real, ".gitconfig") ||
		link("Library/Preferences") != filepath.Join(real, "Library/Preferences") || link(".gemini/config") != filepath.Join(real, ".gemini/config") ||
		link(".gemini/antigravity-cli/skills") != filepath.Join(real, ".gemini/antigravity-cli/skills") ||
		link(".gemini/antigravity-cli/settings.json") != filepath.Join(real, ".gemini/antigravity-cli/settings.json") {
		t.Fatal("該連結回真正的家的沒連好")
	}
	// 計畫的連結不含每一層自己保留的項目
	for _, l := range agyPlanLinks(home, real, listNames) {
		for _, own := range []string{"Library", ".gemini", "Keychains", "antigravity-cli"} {
			if filepath.Base(l[0]) == own {
				t.Fatalf("不該連結 %s", l[0])
			}
		}
	}
	// 自己保留的：鑰匙圈、antigravity-cli、對話
	for _, own := range []string{"Library/Keychains", ".gemini/antigravity-cli", ".gemini/antigravity-cli/conversations", "Library", ".gemini"} {
		if isSymlink(filepath.Join(home, own)) {
			t.Fatalf("%s 不該是連結", own)
		}
	}
	shell := filepath.Join(home, agyShellFolder)
	if !contains(env, "HOME="+home) || !contains(env, "ZDOTDIR="+shell) || !contains(env, "BASH_ENV="+filepath.Join(shell, agyBashEnvFile)) ||
		!contains(env, agyOriginalZdotdir+"=/my/zdot") || !contains(env, "AGY_CLI_DISABLE_AUTO_UPDATE=true") {
		t.Fatalf("環境不對：%v", env)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, agyOriginalBashEnv+"=") {
			t.Fatal("原本沒有 BASH_ENV 就不帶")
		}
	}
	z, _ := os.ReadFile(filepath.Join(shell, ".zshenv"))
	if !strings.Contains(string(z), "export HOME="+shellQuote(real)) || !strings.Contains(string(z), agyOriginalZdotdir) {
		t.Fatalf("zsh 小腳本：%s", z)
	}
	if runtime.GOOS == "darwin" && len(keychainCalls) != 1 {
		t.Fatalf("鑰匙圈準備了 %d 次", len(keychainCalls))
	}
	// 真正的家少了東西：下次整理時拿掉多的連結
	_ = os.Remove(filepath.Join(real, ".gitconfig"))
	agyHomeMu.Lock()
	delete(agyHomeLast, home)
	agyHomeMu.Unlock()
	prepareAgyHome(home)
	if isSymlink(filepath.Join(home, ".gitconfig")) {
		t.Fatal("多出來的連結要拿掉")
	}
	// 移除帳戶前拿掉所有連結，不會順著連結刪到真正的家
	removeAgyHomeLinks(home)
	if isSymlink(filepath.Join(home, ".ssh")) {
		t.Fatal("連結要拿掉")
	}
	if _, err := os.Stat(filepath.Join(real, ".ssh")); err != nil {
		t.Fatal("真正的家不能被動到")
	}
	if shellQuote("a'b") != `'a'\''b'` {
		t.Fatal("單引號")
	}
}

func TestAcpParse(t *testing.T) {
	m := acp.Parse([]byte(`{"jsonrpc":"2.0","id":3,"method":"_x.ai/ask_user_question","params":{"a":1}}`))
	if m == nil || !m.IsRequest() || m.Method != "x.ai/ask_user_question" || m.Key() != "3" {
		t.Fatalf("請求：%+v", m)
	}
	n := acp.Parse([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{}}`))
	if n == nil || n.IsRequest() || n.IsResponse() {
		t.Fatalf("通知：%+v", n)
	}
	r := acp.Parse([]byte(`{"jsonrpc":"2.0","id":"7","error":{"code":-32603,"message":"Path not found","data":{"code":"FS_NOT_FOUND","detail":"x"}}}`))
	if r == nil || !r.IsResponse() || r.Err == nil || r.Err.DataCode != "FS_NOT_FOUND" || r.Err.Text != "Path not found（x）" {
		t.Fatalf("錯誤回應：%+v", r)
	}
	if acp.Parse([]byte("starting...")) != nil || acp.Parse([]byte(`{"jsonrpc":"2.0"}`)) != nil {
		t.Fatal("不是 JSON-RPC 的行")
	}
}

func TestHistoryRecap(t *testing.T) {
	c := &Conv{}
	c.init()
	for _, x := range [][2]string{{"User", "第一句"}, {"Assistant", "回答"}, {"Tool", "x"}, {"User", "這一次"}} {
		it := c.addItem(x[0])
		it.Text = x[1]
	}
	r := historyRecap(c)
	if !strings.Contains(r, "使用者：第一句") || !strings.Contains(r, "你：回答") || strings.Contains(r, "這一次") {
		t.Fatalf("前文節錄：%s", r)
	}
}
