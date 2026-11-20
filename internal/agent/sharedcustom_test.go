package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sp(s string) *string { return &s }

// 全域指示的雙向同步：哪一邊改過以哪一邊為準，兩邊都改過以來源為準、帳戶那份另存
func TestDecideInstructionSync(t *testing.T) {
	a, b := "甲", "乙"
	ha, hb := shareHash(a), shareHash(b)
	blank := shareBlankHash
	cases := []struct {
		name            string
		source, account *string
		last            *string
		want            string
	}{
		{"兩邊都沒有", nil, sp("  "), nil, syncNone},
		{"兩邊一樣", sp(a), sp(a + "\r\n"[:0]), nil, syncNone},
		{"第一次：帳戶沒有", sp(a), nil, nil, syncCopyToAccount},
		{"第一次：來源沒有", nil, sp(b), nil, syncCopyToSource},
		{"第一次：兩邊不同", sp(a), sp(b), nil, syncMergeIntoSource},
		{"來源改過", sp(b), sp(a), &ha, syncCopyToAccount},
		{"帳戶改過", sp(a), sp(b), &ha, syncCopyToSource},
		{"兩邊都改過", sp(a), sp(b), sp(shareHash("丙")), syncKeepSourceBackupAcc},
		{"來源被刪、帳戶沒改", nil, sp(a), &ha, syncDeleteAccount},
		{"帳戶被刪、來源沒改：補回", sp(a), nil, &ha, syncCopyToAccount},
		{"帳戶被刪、來源也改了：補回", sp(b), nil, &ha, syncCopyToAccount},
		{"來源從空白變成有內容", sp(b), nil, &blank, syncCopyToAccount},
		{"帳戶從空白變成有內容", nil, sp(b), &blank, syncCopyToSource},
		{"上次是乙、帳戶是乙、來源改成甲", sp(a), sp(b), &hb, syncCopyToAccount},
	}
	for _, c := range cases {
		if got := decideInstructionSync(c.source, c.account, c.last); got != c.want {
			t.Errorf("%s：%s，應該是 %s", c.name, got, c.want)
		}
	}
	// 換行不同不算改過
	if shareHash("x\r\ny") != shareHash("x\ny") {
		t.Fatal("CRLF 應該和 LF 同一個雜湊")
	}
}

func TestMemoryMergeRules(t *testing.T) {
	if planMemoryFile("a.md", false, false) != memCopy || planMemoryFile("a.md", true, true) != memSkip ||
		planMemoryFile("memory.md", true, false) != memAppendIndex || planMemoryFile("a.md", true, false) != memCopyRenamed {
		t.Fatal("planMemoryFile")
	}
	if r := renamedForAccount(filepath.Join("sub", "notes.md"), "1a2b3c4d5e6f"); r != filepath.Join("sub", "notes.1a2b3c4d.md") {
		t.Fatal(r)
	}
	got := appendIndexLines("- a\n- b\n", "- b\n\n- c  \n- a\n- d")
	if got != "- a\n- b\n- c\n- d\n" {
		t.Fatalf("%q", got)
	}
	if appendIndexLines("x\n", "x") != "x\n" {
		t.Fatal("沒有要加的應該回原文")
	}
	if !strings.Contains(indexLineForRenamed("notes.abc.md", "notes.md", "工作"), "](notes.abc.md)") {
		t.Fatal("索引行")
	}
}

func TestSharedSkillAndDirs(t *testing.T) {
	for _, c := range []struct {
		p, name string
		want    bool
	}{
		{"Claude", "review", true}, {"Claude", ".system", false}, {"Claude", "synced", false}, {"Codex", "synced", true},
		{"Antigravity", "plan", false}, {"Antigravity", "mine", true}, {"Grok", "", false},
	} {
		if isSharedSkill(c.p, c.name) != c.want {
			t.Errorf("%s %q 應該是 %v", c.p, c.name, c.want)
		}
	}
	if strings.Join(shareLinkedDirs("Claude"), ",") != "agents,commands,output-styles" || shareLinkedDirs("Codex")[0] != "prompts" || shareLinkedDirs("Grok")[0] != "rules" {
		t.Fatal("shareLinkedDirs")
	}
	if shareInstructionFile("Claude") != "CLAUDE.md" || shareInstructionFile("Codex") != "AGENTS.md" {
		t.Fatal("shareInstructionFile")
	}
	acc := &Account{Provider: "Claude", ConfigDir: "/x"}
	if !shareActive(acc) {
		t.Fatal("隔離帳戶預設共用")
	}
	for name, a := range map[string]*Account{
		"系統預設": {Provider: "Claude", IsSystemDefault: true}, "現有資料夾": {Provider: "Claude", ConfigDir: "/x", ExistingFolder: true},
		"精簡模式": {Provider: "Claude", ConfigDir: "/x", SafeMode: true}, "agy": {Provider: "Antigravity", ConfigDir: "/x"},
		"關掉": {Provider: "Claude", ConfigDir: "/x", ShareCustom: boolPtr(false)},
	} {
		if shareActive(a) {
			t.Errorf("%s 不應該共用", name)
		}
	}
}

func TestMemoryProjectRoot(t *testing.T) {
	files := map[string]string{
		"/main/.git":                        "",
		"/wt/.git":                          "gitdir: /main/.git/worktrees/wt\n",
		"/main/.git/worktrees/wt/commondir": "../..\n",
		"/main/.git/worktrees/wt/gitdir":    "/wt/.git\n",
		"/bad/.git":                         "gitdir: /elsewhere/x\n",
	}
	isDir := func(p string) bool { return p == "/main/.git" }
	read := func(p string) (string, bool) {
		if p == "/main/.git" {
			return "", false
		}
		s, ok := files[p]
		return s, ok
	}
	if r := memoryProjectRoot("/main/src/pkg", isDir, read); r != "/main" {
		t.Fatal(r)
	}
	if r := memoryProjectRoot("/wt/sub", isDir, read); r != "/main" {
		t.Fatalf("worktree 應該用主 repo：%s", r)
	}
	if r := memoryProjectRoot("/bad/x", isDir, read); r != "/bad" {
		t.Fatal(r)
	}
	if r := memoryProjectRoot("/plain/dir", isDir, read); r != "/plain/dir" {
		t.Fatal(r)
	}
	if n := claudeProjectFolderName("/Users/u/my repo/"); n != "-Users-u-my-repo" {
		t.Fatal(n)
	}
	if k := claudeProjectKeys("/a", "/a/"); len(k) != 1 {
		t.Fatal(k)
	}
}

func TestClaudeMcpRules(t *testing.T) {
	doc := `{"mcpServers":{"gh":{"command":"gh"},"both":{"command":"src"}},"x":[1,2],
		"projects":{"/repo":{"mcpServers":{"local":{"command":"l"}}},"/other":{"mcpServers":{"no":{"command":"n"}}}}}`
	u, p, ok := readClaudeMcpServers([]byte(doc), []string{"/repo"})
	if !ok || len(u) != 2 || len(p) != 1 || p["local"] == nil {
		t.Fatalf("%v %v %v", u, p, ok)
	}
	out := buildClaudeMcpConfig(u, p, []string{"both"})
	var parsed struct {
		McpServers map[string]any `json:"mcpServers"`
	}
	if json.Unmarshal(out, &parsed) != nil || len(parsed.McpServers) != 2 || parsed.McpServers["both"] != nil {
		t.Fatalf("帳戶自己有的不給：%s", out)
	}
	if buildClaudeMcpConfig(nil, nil, nil) != nil {
		t.Fatal("沒有要給的回 nil")
	}
}

func TestCodexMcpRules(t *testing.T) {
	toml := `model = "x"
[mcp_servers.github]
command = "gh"
args = [
  "mcp",
]
[mcp_servers.github.env]
TOKEN = "t" # 註解
[mcp_servers."bad.name"]
command = "x"
[[mcp_servers.arr.items]]
a = 1
[mcp_servers.arr]
command = "y"
[other]
note = """
[mcp_servers.fake]
"""
mcp_servers.inline.command = "i"
`
	names := codexMcpServerNames(toml)
	if strings.Join(names, ",") != "github,bad.name,arr" {
		t.Fatalf("名稱：%v", names)
	}
	over := codexSharedMcpOverrides(toml, []string{"mine"})
	want := []string{`mcp_servers.github.command="gh"`, "mcp_servers.github.args=[\n  \"mcp\",\n]", `mcp_servers.github.env.TOKEN="t" # 註解`}
	if strings.Join(over, "|") != strings.Join(want, "|") {
		t.Fatalf("覆寫：%q", over)
	}
	if len(codexSharedMcpOverrides(toml, []string{"github"})) != 0 {
		t.Fatal("帳戶自己有的不給")
	}
}

// 真的在暫存的家裡共用、再關掉：連結、合併、備份、移回，來源的東西一樣都不能少
func TestShareAndUnshareOnDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := filepath.Join(home, ".claude")
	write := func(p, s string) {
		t.Helper()
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	write(filepath.Join(src, "CLAUDE.md"), "來源的指示\n")
	write(filepath.Join(src, "skills", "common", "SKILL.md"), "共用")
	write(filepath.Join(src, "agents", "a.md"), "來源的子代理")
	write(filepath.Join(home, ".claude.json"), `{"mcpServers":{"gh":{"command":"gh"}}}`)
	write(filepath.Join(src, "projects", "-repo", "memory", "MEMORY.md"), "- [x](x.md)\n")

	id := strings.Repeat("ab", 16)
	acc := filepath.Join(t.TempDir(), "accounts", id, "claude")
	write(filepath.Join(acc, "CLAUDE.md"), "帳戶的指示\n")
	write(filepath.Join(acc, "skills", "mine", "SKILL.md"), "帳戶自己的")
	write(filepath.Join(acc, "skills", "common", "SKILL.md"), "帳戶版本")
	write(filepath.Join(acc, "agents", "b.md"), "帳戶的子代理")
	write(filepath.Join(acc, "projects", "-repo", "memory", "MEMORY.md"), "- [y](y.md)\n")
	write(filepath.Join(acc, "projects", "-repo", "memory", "y.md"), "帳戶的記憶")
	write(filepath.Join(acc, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@b.c"}}`)

	h := &Hub{}
	a := &Account{ID: id, Provider: "Claude", Label: "工作", ConfigDir: acc}
	work := t.TempDir()
	launch := h.prepareAccountLaunch(a, work)

	// 指示：第一次、兩邊不同 → 合併進來源，帳戶拿到同一份
	merged := read(filepath.Join(src, "CLAUDE.md"))
	if !strings.Contains(merged, "來源的指示") || !strings.Contains(merged, "帳戶的指示") || read(filepath.Join(acc, "CLAUDE.md")) != merged {
		t.Fatalf("指示沒有合併：%q", merged)
	}
	// skills：帳戶獨有的搬進來源再連結；同名的留著帳戶自己的
	if tgt, ok := linkTarget(filepath.Join(acc, "skills", "mine")); !ok || tgt != filepath.Join(src, "skills", "mine") {
		t.Fatal("mine 應該連到來源")
	}
	if read(filepath.Join(src, "skills", "mine", "SKILL.md")) != "帳戶自己的" {
		t.Fatal("mine 沒有複製到來源")
	}
	if isSymlink(filepath.Join(acc, "skills", "common")) || read(filepath.Join(acc, "skills", "common", "SKILL.md")) != "帳戶版本" {
		t.Fatal("同名的應該留帳戶自己的")
	}
	// agents：帳戶的合併進來源、原件移到備份、換成連結
	if !isSymlink(filepath.Join(acc, "agents")) || read(filepath.Join(src, "agents", "b.md")) != "帳戶的子代理" ||
		read(filepath.Join(acc, shareStateFolder, "backup", "agents", "b.md")) != "帳戶的子代理" {
		t.Fatal("agents 沒有合併、備份、連結")
	}
	// 記憶：索引合併、新的檔複製
	if !strings.Contains(read(filepath.Join(src, "projects", "-repo", "memory", "MEMORY.md")), "- [y](y.md)") ||
		read(filepath.Join(src, "projects", "-repo", "memory", "y.md")) != "帳戶的記憶" {
		t.Fatal("記憶沒有合併")
	}
	// 啟動參數：記憶資料夾在來源、MCP 檔帶來源的 gh
	if launch.ClaudeMemoryDir != filepath.Join(src, "projects", claudeProjectFolderName(work), "memory") {
		t.Fatalf("記憶資料夾：%s", launch.ClaudeMemoryDir)
	}
	if !strings.Contains(read(launch.ClaudeMcpConfig), `"gh"`) || !strings.HasPrefix(launch.ClaudeMcpConfig, filepath.Join(acc, shareStateFolder)) {
		t.Fatalf("MCP 設定檔：%s", launch.ClaudeMcpConfig)
	}

	// 第二次啟動：只有帳戶改了指示 → 寫回來源
	write(filepath.Join(acc, "CLAUDE.md"), "帳戶改過\n")
	h.prepareAccountLaunch(a, work)
	if read(filepath.Join(src, "CLAUDE.md")) != "帳戶改過\n" {
		t.Fatal("帳戶改的指示沒有寫回來源")
	}
	// 兩邊都改 → 以來源為準，帳戶那份另存
	write(filepath.Join(acc, "CLAUDE.md"), "帳戶又改\n")
	write(filepath.Join(src, "CLAUDE.md"), "來源也改\n")
	h.prepareAccountLaunch(a, work)
	if read(filepath.Join(acc, "CLAUDE.md")) != "來源也改\n" {
		t.Fatal("兩邊都改應該以來源為準")
	}
	backups, _ := filepath.Glob(filepath.Join(acc, shareStateFolder, "backup", "CLAUDE.md.*"))
	if len(backups) != 1 || read(backups[0]) != "帳戶又改\n" {
		t.Fatalf("帳戶那份沒有另存：%v", backups)
	}

	// 關掉共用：連結拿掉、原件移回，來源一樣都不少
	unshareCustomizations("Claude", acc)
	if pathExists(filepath.Join(acc, "skills", "mine")) {
		// mine 的原件在備份：移回來的是真資料夾
		if isSymlink(filepath.Join(acc, "skills", "mine")) || read(filepath.Join(acc, "skills", "mine", "SKILL.md")) != "帳戶自己的" {
			t.Fatal("mine 沒有移回原件")
		}
	} else {
		t.Fatal("mine 不見了")
	}
	if isSymlink(filepath.Join(acc, "agents")) || read(filepath.Join(acc, "agents", "b.md")) != "帳戶的子代理" {
		t.Fatal("agents 沒有移回原件")
	}
	if read(filepath.Join(acc, "skills", "common", "SKILL.md")) != "帳戶版本" {
		t.Fatal("帳戶自己的同名 skill 被動到了")
	}
	if pathExists(launch.ClaudeMcpConfig) {
		t.Fatal("MCP 設定檔應該刪掉")
	}
	for _, p := range []string{"skills/common/SKILL.md", "skills/mine/SKILL.md", "agents/a.md", "agents/b.md", "CLAUDE.md", "projects/-repo/memory/MEMORY.md"} {
		if !pathExists(filepath.Join(src, p)) {
			t.Errorf("來源的 %s 被刪掉了", p)
		}
	}
	if st := readShareState(filepath.Join(acc, shareStateFolder, shareStateFile)); st.Active {
		t.Fatal("狀態應該是沒有共用")
	}
}

// 帳戶沒有、來源有的 skill 與資料夾直接連過去；Codex 帶來源 config.toml 的 MCP 覆寫
func TestShareCodexOnDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := filepath.Join(home, ".codex")
	_ = os.MkdirAll(filepath.Join(src, "skills", "s1"), 0o700)
	_ = os.MkdirAll(filepath.Join(src, "prompts"), 0o700)
	_ = os.WriteFile(filepath.Join(src, "AGENTS.md"), []byte("指示"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "config.toml"), []byte("[mcp_servers.gh]\ncommand = \"gh\"\n[mcp_servers.mine]\ncommand = \"m\"\n"), 0o600)
	acc := filepath.Join(t.TempDir(), "codex")
	_ = os.MkdirAll(acc, 0o700)
	_ = os.WriteFile(filepath.Join(acc, "config.toml"), []byte("[mcp_servers.mine]\ncommand = \"own\"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(acc, "AGENTS.md"), nil, 0o600) // Codex 預設的空檔
	launch := (&Hub{}).prepareAccountLaunch(&Account{ID: "x", Provider: "Codex", ConfigDir: acc}, t.TempDir())
	if strings.Join(launch.CodexOverrides, "|") != `mcp_servers.gh.command="gh"` {
		t.Fatalf("%q", launch.CodexOverrides)
	}
	if !isSymlink(filepath.Join(acc, "skills", "s1")) || !isSymlink(filepath.Join(acc, "prompts")) {
		t.Fatal("沒有連結")
	}
	if b, _ := os.ReadFile(filepath.Join(acc, "AGENTS.md")); string(b) != "指示" {
		t.Fatal("空的 AGENTS.md 應該拿來源的")
	}
	if b, _ := os.ReadFile(filepath.Join(src, "AGENTS.md")); string(b) != "指示" {
		t.Fatal("來源被改了")
	}
}
