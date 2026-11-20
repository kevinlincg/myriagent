package agent

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEstimateTokensAndMeasure(t *testing.T) {
	if n := estimateTokens("abcdefgh"); n != 2 {
		t.Fatal(n)
	}
	if n := estimateTokens("中文字abc"); n != 4 {
		t.Fatal(n)
	}
	m := measureText("a\n<!-- 註解 -->\nb\n", 20)
	if m.Lines != 2 || m.Bytes != 20 {
		t.Fatalf("%+v", m)
	}
	// 程式碼區塊裡的註解保留
	if s := stripHTMLComments("```\n<!-- x -->\n```"); !strings.Contains(s, "<!-- x -->") {
		t.Fatal(s)
	}
	if formatHealthTokens(850) != "約 850 token" || formatHealthTokens(13_200) != "約 1.3 萬 token" || formatBytes(25190) != "24.6 KB" {
		t.Fatal(formatHealthTokens(13_200), formatBytes(25190))
	}
}

func TestFindImportsAndFrontMatter(t *testing.T) {
	text := "看 @docs/a.md 與 @~/x.md、email me@example.com，`@code/x.md` 不算\n```\n@in/fence.md\n```\n(@b.md)"
	got := findImports(text)
	want := []string{"docs/a.md", "~/x.md", "b.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%v", got)
	}
	fm := parseFrontMatter("---\nname: x\ndescription: >\n  一行\n  兩行\npaths:\n  - src/**\n---\nbody")
	if frontMatterText(fm, "description") != "一行 兩行" || !hasPathScope(fm) {
		t.Fatalf("%v", fm)
	}
	if len(parseFrontMatter("---\nname: x\n")) != 0 {
		t.Fatal("沒有結尾的 --- 不是前置資料")
	}
	if p := frontMatterProblem("---\ndescription: Use when: x\n---"); p == "" {
		t.Fatal("「: 」應該被抓到")
	}
	if p := frontMatterProblem("---\ndescription: \"Use when: x\"\n---"); p != "" {
		t.Fatal(p)
	}
	links := relativeLinks("[a](ref/a.md) [b](https://x) [c](#x) [d](ref/b%20c.md#s)\n`[e](no.md)`")
	if strings.Join(links, ",") != "ref/a.md,ref/b c.md" {
		t.Fatal(links)
	}
}

func TestTomlRules(t *testing.T) {
	toml := "model = \"x\"\n[mcp_servers.github]\ncommand = \"x\"\n[mcp_servers.github.env]\nA = \"1\"\n[mcp_servers.\"a.b\"] # 註解\n[claude_compat]\nimported = true\n"
	if n := tomlTableNames(toml, "mcp_servers"); strings.Join(n, ",") != "github,a.b" {
		t.Fatal(n)
	}
	if b := tomlBool(toml, "claude_compat", "imported"); b == nil || !*b {
		t.Fatal("imported")
	}
	if b := tomlBool("claude_compat.imported = false", "claude_compat", "imported"); b == nil || *b {
		t.Fatal("最上層的 表.鍵")
	}
}

func scanOf(provider string, files ...instructionFileStat) *accountScan {
	return &accountScan{Account: healthAccountInput{ID: "a", Label: "A", Provider: provider}, Instructions: files}
}

func instr(path string, lines, tokens, chars int) instructionFileStat {
	return instructionFileStat{Path: path, Kind: "main", Measure: textMeasure{Bytes: int64(chars), Chars: chars, Lines: lines, Tokens: tokens}}
}

// 全域指示的門檻：合計 5,000 token 以上偏大、15,000 以上過大、單檔 200 行以上偏大、Claude 單檔 40,000 字元以上過大
func TestInstructionThresholds(t *testing.T) {
	cases := []struct {
		name     string
		scan     *accountScan
		level    healthLevel
		headline string
	}{
		{"小", scanOf("Claude", instr("/c/CLAUDE.md", 50, 1000, 4000)), healthOK, ""},
		{"剛好 5000", scanOf("Claude", instr("/c/CLAUDE.md", 50, 5000, 4000)), healthOK, ""},
		{"超過 5000", scanOf("Claude", instr("/c/CLAUDE.md", 50, 5001, 4000)), healthWarn, "CLAUDE.md 偏大"},
		{"超過 15000", scanOf("Codex", instr("/c/AGENTS.md", 50, 15001, 4000)), healthBad, "AGENTS.md 過大"},
		{"201 行", scanOf("Claude", instr("/c/CLAUDE.md", 201, 100, 4000)), healthWarn, "CLAUDE.md 偏大"},
		{"40001 字", scanOf("Claude", instr("/c/CLAUDE.md", 10, 100, 40001)), healthBad, "CLAUDE.md 過大"},
		{"Codex 不看字元", scanOf("Codex", instr("/c/AGENTS.md", 10, 100, 40001)), healthOK, ""},
	}
	for _, c := range cases {
		h := evaluateHealth(c.scan)
		if h.Items[0].Level != c.level || h.Items[0].Headline != c.headline {
			t.Errorf("%s：%v %q", c.name, h.Items[0].Level, h.Items[0].Headline)
		}
		if h.CanTrim != (c.level == healthWarn || c.level == healthBad) {
			t.Errorf("%s：canTrim %v", c.name, h.CanTrim)
		}
	}
	s := scanOf("Claude", instr("/c/CLAUDE.md", 10, 100, 100))
	s.MissingImports = []string{"docs/x.md"}
	if h := evaluateHealth(s); h.Items[0].Headline != "匯入的檔案找不到" || h.Summary != "匯入的檔案找不到" {
		t.Fatal(h.Items[0].Headline, h.Summary)
	}
	// 精簡模式不載入：info，不拉低整體
	s = scanOf("Claude", instr("/c/CLAUDE.md", 10, 99999, 100))
	s.Account.SafeMode = true
	if h := evaluateHealth(s); h.Level != healthOK || h.Summary != "良好" || h.Load != "" {
		t.Fatalf("%+v", h)
	}
}

func TestMemoryLimits(t *testing.T) {
	over := memoryIndexStat{Project: "p", Lines: 201, Bytes: 10}
	near := memoryIndexStat{Project: "q", Lines: 180, Bytes: 10}
	ok := memoryIndexStat{Project: "r", Lines: 179, Bytes: 22_499}
	if !memoryOverLimit(over) || memoryOverLimit(near) || !memoryNearLimit(near) || memoryNearLimit(over) || memoryNeedsTrim(ok) {
		t.Fatal("門檻不對")
	}
	if !memoryOverLimit(memoryIndexStat{Bytes: 25_001}) || !memoryNearLimit(memoryIndexStat{Bytes: 22_500}) {
		t.Fatal("位元組的門檻不對")
	}
	s := scanOf("Claude")
	s.MemoryIndexes = []memoryIndexStat{ok, near, over}
	h := evaluateHealth(s)
	if h.Items[1].Level != healthBad || h.Items[1].Headline != "記憶索引超過上限" || !h.CanTrimMemory {
		t.Fatalf("%+v", h.Items[1])
	}
	if len(h.Items[1].Files) != 2 || h.Items[1].Files[0].Level != "bad" {
		t.Fatalf("%+v", h.Items[1].Files)
	}
}

func TestSkillIssues(t *testing.T) {
	s := scanOf("Claude")
	n := 4
	s.Skills = &n
	s.SkillList = []skillStat{
		{Name: "good", Kind: "skill", Source: "user", Readable: true, HasFrontMatter: true, DeclaredName: "good", Description: "Plan tests and keep baselines updated when the suite changes."},
		{Name: "nofm", Kind: "skill", Source: "user", Readable: true},
		{Name: "short", Kind: "skill", Source: "user", Readable: true, HasFrontMatter: true, Description: "Test Plan"},
		{Name: "good", Kind: "skill", Source: "plugin", Plugin: "pl", Readable: true, HasFrontMatter: true, DeclaredName: "good", Description: "Another long enough description for the plugin."},
	}
	views := evaluateSkillViews(s)
	by := map[string]skillView{}
	for _, v := range views {
		by[v.Name] = v
	}
	if by["nofm"].level != healthBad || by["short"].level != healthWarn || by["good"].level != healthWarn || by["pl:good"].level != healthWarn {
		t.Fatalf("%+v", views)
	}
	if views[0].level != healthBad {
		t.Fatal("有問題的要排在前面")
	}
	// 使用者自己的 skill 有 bad：skills 這一項拉到 bad
	h := evaluateHealth(s)
	var item healthItem
	for _, it := range h.Items {
		if it.Key == "skills" {
			item = it
		}
	}
	if item.Level != healthBad || !strings.Contains(item.Headline, "有問題") {
		t.Fatalf("%v %q", item.Level, item.Headline)
	}
	if p := skillPurpose("Use this e.g. for tests. More text."); p != "Use this e.g. for tests." {
		t.Fatal(p)
	}
	if p := skillPurpose("做測試計畫。其他"); p != "做測試計畫。" {
		t.Fatal(p)
	}
}

func doc(key, provider, text string) *instructionDoc {
	return &instructionDoc{Key: key, Provider: provider, Label: provider, Path: "/" + key, Exists: text != "", Text: text}
}

func TestCompareInstructionVerdicts(t *testing.T) {
	a := "# 習慣\n- 一律用繁體中文回覆\n- 先寫測試再實作\n- 提交前跑 go vet"
	if r := compareInstructions(nil); r.Verdict != verdictNone {
		t.Fatal(r.Verdict)
	}
	if r := compareInstructions([]*instructionDoc{doc("c", "Claude", ""), doc("x", "Codex", "")}); r.Verdict != verdictNone {
		t.Fatal(r.Verdict)
	}
	if r := compareInstructions([]*instructionDoc{doc("c", "Claude", a)}); r.Verdict != verdictSingle {
		t.Fatal(r.Verdict)
	}
	if r := compareInstructions([]*instructionDoc{doc("c", "Claude", a), doc("x", "Codex", "* 一律用繁體中文回覆。\n* 先寫測試再實作\n\n提交前跑 go vet")}); r.Verdict != verdictSame {
		t.Fatalf("%s %s", r.Verdict, r.Summary)
	}
	r := compareInstructions([]*instructionDoc{doc("c", "Claude", a), doc("x", "Codex", "")})
	if r.Verdict != verdictNeedsSync || r.Level != healthWarn || !r.mergeable() || !strings.Contains(r.Summary, "3 條") {
		t.Fatalf("%s %s", r.Verdict, r.Summary)
	}
	r = compareInstructions([]*instructionDoc{doc("c", "Claude", a), doc("x", "Codex", "- 一律用繁體中文回覆\n- 部署前先備份資料庫")})
	if r.Verdict != verdictDiverged || r.Level != healthWarn {
		t.Fatalf("%s %s", r.Verdict, r.Summary)
	}
	// Grok 自己沒有、經 Claude 相容讀得到：不算缺
	g := doc("g", "Grok", "")
	g.AlsoLoadsKey = "c"
	if r := compareInstructions([]*instructionDoc{doc("c", "Claude", a), g}); r.Verdict != verdictSame {
		t.Fatalf("%s %s", r.Verdict, r.Summary)
	}
	if s := instructionSimilarity("一律用繁體中文回覆", "一律用繁體中文回覆喔"); s < fuzzyThreshold {
		t.Fatal(s)
	}
}

// ---- 中樞 ----

func newHealthHub(t *testing.T) (*Hub, *testView, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := implementedProviders["Codex"]
	implementedProviders["Codex"] = true
	t.Cleanup(func() { implementedProviders["Codex"] = old })
	root := t.TempDir()
	h := &Hub{root: root, convs: map[string]*Conv{}, views: map[*View]struct{}{}, clis: map[string]*cliInfo{}, flushKick: make(chan struct{}, 1)}
	h.settings = loadSettings(root)
	h.loadAccounts()
	tv := &testView{t: t, h: h}
	tv.v = h.addView(func(b []byte) error {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		tv.mu.Lock()
		tv.msgs = append(tv.msgs, m)
		tv.mu.Unlock()
		return nil
	})
	return h, tv, home
}

func TestHealthCheckAndFix(t *testing.T) {
	h, tv, home := newHealthHub(t)
	big := strings.Repeat("- 一律用繁體中文回覆並說明理由\n", 250)
	writeFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), big+"看 @docs/missing.md\n")
	writeFile(t, filepath.Join(home, ".claude", "projects", "-Users-me-app", "memory", "MEMORY.md"), strings.Repeat("- x\n", 210))
	writeFile(t, filepath.Join(home, ".claude", "skills", "plan", "SKILL.md"), "---\nname: plan\ndescription: Plan the work step by step before editing code.\n---\n本文\n")
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), "{ 壞掉")
	writeFile(t, filepath.Join(home, ".codex", "AGENTS.md"), "")
	writeFile(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.a]\n[mcp_servers.b]\n")

	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	init := tv.wait("init", func(m map[string]any) bool { return m["type"] == "init" })
	hv, _ := init["health"].(map[string]any)
	if hv == nil || hv["running"] != false || hv["checkedAt"] != nil {
		t.Fatalf("init 的 health：%v", init["health"])
	}
	tv.send(map[string]any{"cmd": "healthCheck"})
	tv.wait("分析中", func(m map[string]any) bool {
		x, _ := m["health"].(map[string]any)
		return m["type"] == "health" && x["running"] == true
	})
	done := tv.wait("分析完", func(m map[string]any) bool {
		x, _ := m["health"].(map[string]any)
		return m["type"] == "health" && x["running"] == false && x["checkedAt"] != nil
	})
	x := done["health"].(map[string]any)
	accounts := x["accounts"].(map[string]any)
	claude, _ := accounts["local-claude"].(map[string]any)
	if claude == nil || claude["level"] != "bad" || claude["canTrim"] != true || claude["canTrimMemory"] != true {
		t.Fatalf("Claude 的健康度：%v", claude)
	}
	keys := map[string]map[string]any{}
	for _, it := range claude["items"].([]any) {
		m := it.(map[string]any)
		keys[m["key"].(string)] = m
	}
	if keys["instructions"]["level"] != "warn" || keys["memory"]["level"] != "bad" || keys["settings"]["level"] != "bad" {
		t.Fatalf("各項：%v", keys)
	}
	if !strings.Contains(keys["instructions"]["detail"].(string), "docs/missing.md") {
		t.Fatal("找不到的匯入沒有列出")
	}
	if sk, _ := keys["skills"]["skills"].([]any); len(sk) != 1 {
		t.Fatalf("skill 清單：%v", keys["skills"])
	}
	codex, _ := accounts["local-codex"].(map[string]any)
	if codex == nil {
		t.Fatal("沒有 Codex 帳戶")
	}
	ins := x["instructions"].(map[string]any)
	if ins["verdict"] != "needsSync" || ins["mergeable"] != true {
		t.Fatalf("指示檔：%v", ins)
	}

	// 30 秒內再要一次（不帶 force、帳戶沒變）：不重做，只重送目前的結果
	before := countMsgs(tv, "health")
	tv.send(map[string]any{"cmd": "healthCheck"})
	for i := 0; i < 200 && countMsgs(tv, "health") <= before; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	h.mu.Lock()
	running := h.health().running
	h.mu.Unlock()
	if running || countMsgs(tv, "health") != before+1 {
		t.Fatal("不帶 force 不應該重新分析")
	}

	// 用代理合併：開草稿、輸入框預先填好說明，工作資料夾是使用者資料夾
	tv.send(map[string]any{"cmd": "healthFix", "task": "merge", "accountId": "local-claude", "paneId": "p9"})
	conv := tv.wait("合併的草稿", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "p9" })
	if d, _ := conv["draft"].(string); !strings.Contains(d, "請幫我合併並同步") || !strings.Contains(d, filepath.Join(home, ".codex", "AGENTS.md")) {
		t.Fatalf("草稿：%v", conv["draft"])
	}
	if hd := conv["header"].(map[string]any); hd["workingDirectory"] != home || hd["isDraft"] != true {
		t.Fatalf("標頭：%v", hd)
	}
	tv.send(map[string]any{"cmd": "healthFix", "task": "memory", "accountId": "local-claude", "targetAccountId": "local-claude", "paneId": "p10"})
	mem := tv.wait("整理記憶的草稿", func(m map[string]any) bool { return m["type"] == "conversation" && m["paneId"] == "p10" })
	if d, _ := mem["draft"].(string); !strings.Contains(d, "-Users-me-app") {
		t.Fatalf("草稿：%v", mem["draft"])
	}
	// Codex 沒有可以精簡的指示：提示原因
	tv.send(map[string]any{"cmd": "healthFix", "task": "trim", "accountId": "local-claude", "targetAccountId": "local-codex"})
	tv.wait("提示", func(m map[string]any) bool {
		return m["type"] == "toast" && strings.Contains(m["text"].(string), "沒有可以精簡")
	})
}

func countMsgs(tv *testView, typ string) int {
	tv.mu.Lock()
	defer tv.mu.Unlock()
	n := 0
	for _, m := range tv.msgs {
		if m["type"] == typ {
			n++
		}
	}
	return n
}

var _ = sync.Mutex{}
