package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitParseStatus(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid 1a2b3c4d5e6f",
		"# branch.head main",
		"# branch.upstream origin/main",
		"# branch.ab +2 -1",
		"# stash 3",
		"1 .M N... 100644 100644 100644 a b src/a.go",
		"1 M. N... 100644 100644 100644 a b src/b.go",
		"1 MM N... 100644 100644 100644 a b src/c.go",
		"2 R. N... 100644 100644 100644 a b R100 new\told",
		"u UU N... 1 2 3 4 a b c x.go",
		"? new.txt",
		"! ignored.txt",
	}, "\r\n")
	p := gitParseStatus(out)
	if p.Oid != "1a2b3c4d5e6f" || p.Branch != "main" || p.Upstream != "origin/main" || *p.Ahead != 2 || *p.Behind != 1 || p.Stashes != 3 {
		t.Fatalf("標頭：%+v", p)
	}
	if p.Changed != 4 || p.Staged != 3 || p.Unstaged != 2 || p.Conflicts != 1 || p.Untracked != 1 {
		t.Fatalf("檔案數：%+v", p)
	}
	q := gitParseStatus("# branch.oid (initial)\n# branch.head (detached)\n# branch.upstream origin/x\n")
	if q.Oid != "" || q.Branch != "" || q.Ahead != nil {
		t.Fatalf("還沒提交、分離的 HEAD、上游已刪除：%+v", q)
	}
}

func TestGitPlanCompare(t *testing.T) {
	refs := gitParseRefs(strings.Join([]string{
		"refs/heads/feature\tf1\t\torigin\trefs/heads/feature",
		"refs/remotes/origin/feature\tf0",
		"refs/remotes/origin/HEAD\tm1\trefs/remotes/origin/develop",
		"refs/remotes/origin/main\tm2",
		"refs/remotes/up/main\tu1",
	}, "\n"))
	if len(refs) != 5 || refs[0].UpstreamRemote != "origin" || refs[1].SymRef != "" {
		t.Fatalf("for-each-ref：%+v", refs)
	}
	base, target := gitPlanCompare(gitPorcelain{Branch: "feature", Upstream: "origin/feature"}, refs)
	// 遠端/HEAD 指向 develop（不在列出的範圍）：照樣當基準
	if base == nil || base.Name != "origin/develop" || target == nil || target.Ref != "refs/heads/feature" || target.LocalSha != "f0" {
		t.Fatalf("有上游：base=%+v target=%+v", base, target)
	}
	// 分離的 HEAD：和預設分支比、問預設分支
	base, target = gitPlanCompare(gitPorcelain{}, refs)
	if base == nil || base.Name != "origin/develop" || target == nil || target.Ref != "refs/heads/develop" || target.LocalSha != "m1" {
		t.Fatalf("分離的 HEAD：base=%+v target=%+v", base, target)
	}
	// 上游就是預設分支：不另外比
	refs2 := gitParseRefs("refs/heads/main\tx\t\torigin\trefs/heads/main\nrefs/remotes/origin/main\ty")
	base, target = gitPlanCompare(gitPorcelain{Branch: "main", Upstream: "origin/main"}, refs2)
	if base != nil || target == nil || target.LocalSha != "y" {
		t.Fatalf("上游就是預設分支：base=%+v target=%+v", base, target)
	}
	// 沒有 origin、只有一個遠端：用它
	base, _ = gitPlanCompare(gitPorcelain{}, gitParseRefs("refs/remotes/up/master\tz"))
	if base == nil || base.Name != "up/master" {
		t.Fatalf("唯一的遠端：%+v", base)
	}
}

func TestGitSmallParsers(t *testing.T) {
	root, gd, common, ok := gitParseRevParse("/r\n/r/.git\n../.git\n", "/r/sub")
	if !ok || root != "/r" || gd != "/r/.git" || common != "/r/.git" {
		t.Fatalf("rev-parse：%s %s %s", root, gd, common)
	}
	if _, _, _, ok := gitParseRevParse("/r\n", ""); ok {
		t.Fatal("一行不對")
	}
	if l, r, ok := gitParseLeftRight("3\t1\n"); !ok || l != 3 || r != 1 {
		t.Fatal("left-right")
	}
	if gitParseLsRemote("abcdef1234\trefs/heads/main\n0000000\trefs/heads/x", "refs/heads/main") != "abcdef1234" || gitParseLsRemote("", "refs/heads/main") != "" {
		t.Fatal("ls-remote")
	}
	files := map[string]bool{"/g/MERGE_HEAD": true}
	if gitOperationOf("/g", func(p string) bool { return files[p] }, func(string) bool { return false }) != "merge" {
		t.Fatal("進行中的合併")
	}
	if gitOperationOf("/g", func(string) bool { return true }, func(p string) bool { return p == "/g/rebase-apply" }) != "rebase" {
		t.Fatal("rebase 優先")
	}
	if got := gitErrorLine("remote: x\nfatal: unable to access 'https://user:pw@host/r/': 403\n", "x"); got != "fatal: unable to access 'https://***@host/r/': 403" {
		t.Fatalf("錯誤行：%q", got)
	}
	if gitErrorLine("  \n", "後備") != "後備" {
		t.Fatal("沒有內容用後備")
	}
	if !gitIsInside("/r/sub", "/r") || gitIsInside("/rx", "/r") || !gitIsInside("/r/", "/r") {
		t.Fatal("在 repo 裡")
	}
	if gitFind("rel:/a:/b", []string{"/c/git"}, func(p string) bool { return p == "/b/git" || p == "/c/git" }, func(string) bool { return true }) != "/b/git" {
		t.Fatal("PATH 上找 git")
	}
	if gitFind("/b", []string{"/c/git"}, func(p string) bool { return p == "/b/git" || p == "/c/git" }, func(p string) bool { return p != "/b/git" }) != "/c/git" {
		t.Fatal("不能用的跳過")
	}
	env := strings.Join(gitEnv([]string{"GIT_DIR=/x", "HOME=/h", "GIT_OPTIONAL_LOCKS=1"}), " ")
	if strings.Contains(env, "GIT_DIR") || !strings.Contains(env, "HOME=/h") || !strings.Contains(env, "GIT_OPTIONAL_LOCKS=0") || strings.Contains(env, "GIT_OPTIONAL_LOCKS=1") {
		t.Fatalf("環境變數：%s", env)
	}
}

func TestGitShouldRead(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	later := now.Add(time.Minute)
	if !gitShouldReadLocal(gitUser, ago(time.Second), true, &later, now) {
		t.Fatal("使用者要求一律讀")
	}
	if gitShouldReadLocal(gitChanged, ago(time.Hour), true, &later, now) {
		t.Fatal("失敗後的冷卻期只有使用者要求才讀")
	}
	if !gitShouldReadLocal(gitChanged, ago(time.Second), true, nil, now) {
		t.Fatal("變了一律讀")
	}
	if gitShouldReadLocal(gitOpen, ago(4*time.Second), true, nil, now) || !gitShouldReadLocal(gitOpen, ago(5*time.Second), true, nil, now) {
		t.Fatal("開分頁 5 秒")
	}
	if !gitShouldReadLocal(gitPeriodic, ago(30*time.Second), true, nil, now) || gitShouldReadLocal(gitPeriodic, ago(30*time.Second), false, nil, now) {
		t.Fatal("定時：repo 25 秒、不是 repo 2 分鐘")
	}
	if gitShouldCheckRemote(gitOpen, ago(time.Minute), now) || !gitShouldCheckRemote(gitOpen, ago(5*time.Minute), now) || !gitShouldCheckRemote(gitUser, ago(time.Second), now) {
		t.Fatal("問遠端的間隔")
	}
}

func cleanRepo(behind int) *gitRepoStatus {
	return &gitRepoStatus{Root: "/r", Porcelain: gitPorcelain{Oid: "aaaa", Branch: "main", Upstream: "origin/main", Ahead: ptrInt(0), Behind: ptrInt(behind)},
		Remote: &gitRemoteTarget{Remote: "origin", Ref: "refs/heads/main", LocalSha: "bbbb"}}
}

func TestGitDecidePull(t *testing.T) {
	plan, reason := gitDecidePull(cleanRepo(2), nil, false)
	if plan == nil || plan.TargetRef != "refs/remotes/origin/main" || plan.Fetch != nil || plan.Behind != 2 || reason != "" {
		t.Fatalf("可以快轉：%+v %q", plan, reason)
	}
	if plan, reason := gitDecidePull(cleanRepo(0), nil, false); plan != nil || reason != "" {
		t.Fatal("沒有要拉的")
	}
	// 遠端有新的：先 fetch
	snap := &gitRemoteSnapshot{Target: gitRemoteTarget{Remote: "origin", Ref: "refs/heads/main"}, Sha: "cccc"}
	if plan, _ := gitDecidePull(cleanRepo(0), snap, false); plan == nil || plan.Fetch == nil || gitRefspec(plan) != "+refs/heads/main:refs/remotes/origin/main" {
		t.Fatalf("遠端有新的：%+v", plan)
	}
	snap.Sha = "BBBB" // 和這台記著的一樣（不分大小寫）
	if plan, _ := gitDecidePull(cleanRepo(0), snap, false); plan != nil {
		t.Fatal("遠端沒有新的")
	}
	for _, c := range []struct {
		mut  func(s *gitRepoStatus)
		want string
	}{
		{func(s *gitRepoStatus) { s.Operation = "cherryPick" }, "有進行中的揀選（cherry-pick）還沒完成"},
		{func(s *gitRepoStatus) { s.Porcelain.Conflicts = 1 }, "有 1 個檔案的衝突還沒解決"},
		{func(s *gitRepoStatus) { s.Porcelain.Ahead = ptrInt(3) }, "這裡有 3 個提交不在 origin/main，不能快轉"},
		{func(s *gitRepoStatus) { s.Porcelain.Changed = 2 }, "有 2 個已追蹤的檔案還沒提交"},
	} {
		s := cleanRepo(1)
		c.mut(s)
		if _, reason := gitDecidePull(s, nil, false); reason != c.want {
			t.Errorf("不拉的原因：%q，要 %q", reason, c.want)
		}
	}
	if _, reason := gitDecidePull(cleanRepo(1), nil, true); reason != "這個資料夾有對話正在執行，結束後再拉" {
		t.Fatal("代理執行中不拉")
	}
	// 沒有上游的分支不拉
	s := cleanRepo(1)
	s.Porcelain.Upstream = ""
	if plan, reason := gitDecidePull(s, nil, false); plan != nil || reason != "" {
		t.Fatal("沒有上游不拉")
	}
	// 分離的 HEAD：和預設分支比
	d := &gitRepoStatus{Porcelain: gitPorcelain{Oid: "a"}, Base: &gitBaseRef{Name: "origin/main", Ref: "refs/remotes/origin/main"}, BaseAhead: ptrInt(0), BaseBehind: ptrInt(4),
		Remote: &gitRemoteTarget{Remote: "origin", Ref: "refs/heads/main"}}
	if plan, _ := gitDecidePull(d, nil, false); plan == nil || plan.TargetName != "origin/main" || plan.Behind != 4 {
		t.Fatalf("分離的 HEAD：%+v", plan)
	}
	now := time.Now()
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	if gitCanAttempt(ago(30*time.Second), false, false, now) || !gitCanAttempt(ago(time.Minute), false, false, now) {
		t.Fatal("兩次至少隔 1 分鐘")
	}
	if gitCanAttempt(ago(10*time.Minute), true, false, now) || !gitCanAttempt(ago(10*time.Minute), true, true, now) {
		t.Fatal("失敗後 15 分鐘，使用者要求照樣試")
	}
	if !gitUserRequested(ago(time.Minute), now) || gitUserRequested(ago(3*time.Minute), now) {
		t.Fatal("使用者要求的有效期")
	}
	p := &gitPullPlan{TargetName: "origin/main"}
	if v := gitPullView(gitPullDone(0), p, now); v != nil {
		t.Fatal("快轉 0 個不顯示")
	}
	if v := gitPullView(gitPullDone(2), p, now); v.Text != "已從 origin/main 快轉拉取 2 個提交" {
		t.Fatal(v.Text)
	}
	if v := gitPullView(gitPullFail(""), p, now); v.Text != "失敗：原因不明" {
		t.Fatal(v.Text)
	}
}

func TestGitRender(t *testing.T) {
	s := cleanRepo(1)
	s.Porcelain.Oid = "1234567890"
	s.CheckedAt = time.UnixMilli(5000)
	snap := &gitRemoteSnapshot{Target: *s.Remote, Sha: "zzzz", CheckedAt: time.UnixMilli(4000)}
	v := renderGitStatus(s, snap, &gitAutoPullView{State: gitPullSkipped, Text: "x", At: time.UnixMilli(1)})
	if v["head"] != "1234567" || v["remoteNew"] != true || v["remoteCheckedAt"] != int64(4000) || v["upstreamGone"] != false || v["base"] != nil || v["checkedAt"] != int64(5000) {
		t.Fatalf("GitStatusView：%v", v)
	}
	if ap := v["autoPull"].(map[string]any); ap["state"] != "skipped" {
		t.Fatalf("autoPull：%v", ap)
	}
	s.Porcelain.Ahead = nil
	if renderGitStatus(s, nil, nil)["upstreamGone"] != true {
		t.Fatal("上游已刪除")
	}
}

// ───── 流程：真的 git repo ─────

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main", "-c", "user.name=測試", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitOf(m map[string]any) map[string]any {
	if m["type"] != "gitStatus" {
		return nil
	}
	g, _ := m["git"].(map[string]any)
	return g
}

func (tv *testView) waitGit(what string, ok func(g map[string]any) bool) map[string]any {
	tv.t.Helper()
	return gitOf(tv.wait(what, func(m map[string]any) bool { g := gitOf(m); return g != nil && ok(g) }))
}

// 兩個 clone 共用一個 bare 遠端：另一個 clone 推了新的提交，這邊「重新檢查」→ ls-remote 發現遠端有新的 → 自動 fetch＋快轉
func TestGitStatusFlowAndAutoPull(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("沒有 git")
	}
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	runGit(t, base, "init", "--bare", remote)
	a := filepath.Join(base, "a")
	runGit(t, base, "clone", remote, a)
	_ = os.WriteFile(filepath.Join(a, "f.txt"), []byte("1"), 0o644)
	runGit(t, a, "add", ".")
	runGit(t, a, "commit", "-m", "一")
	runGit(t, a, "push", "-u", "origin", "main")
	b := filepath.Join(base, "b")
	runGit(t, base, "clone", remote, b)

	h, tv := newTestHub(t)
	h.mu.Lock()
	h.attachGitStatus(&cliGitReader{})
	h.mu.Unlock()
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "setFolder", "conversationId": id, "path": a})
	g := tv.waitGit("git 狀態", func(g map[string]any) bool { return g["branch"] == "main" })
	if g["upstream"] != "origin/main" || g["ahead"] != float64(0) || g["behind"] != float64(0) || g["changed"] != float64(0) {
		t.Fatalf("乾淨的 repo：%v", g)
	}
	resolved, _ := filepath.EvalSymlinks(a)
	if g["root"] != a && g["root"] != resolved {
		t.Fatalf("root：%v", g["root"])
	}

	// 未提交的變更與未追蹤的檔案（這一輪結束的觸發換成 gitRefresh）
	_ = os.WriteFile(filepath.Join(a, "f.txt"), []byte("2"), 0o644)
	_ = os.WriteFile(filepath.Join(a, "new.txt"), []byte("x"), 0o644)
	tv.send(map[string]any{"cmd": "gitRefresh", "conversationId": id})
	tv.waitGit("有變更", func(g map[string]any) bool {
		return g["changed"] == float64(1) && g["untracked"] == float64(1) && g["unstaged"] == float64(1)
	})
	runGit(t, a, "checkout", "f.txt")
	_ = os.Remove(filepath.Join(a, "new.txt"))

	// 另一邊推了新的提交
	_ = os.WriteFile(filepath.Join(b, "g.txt"), []byte("1"), 0o644)
	runGit(t, b, "add", ".")
	runGit(t, b, "commit", "-m", "二")
	runGit(t, b, "push")
	newHead := runGit(t, b, "rev-parse", "HEAD")

	tv.clear()
	tv.send(map[string]any{"cmd": "gitRefresh", "conversationId": id})
	tv.waitGit("自動拉取", func(g map[string]any) bool {
		ap, _ := g["autoPull"].(map[string]any)
		return ap != nil && ap["state"] == "pulled" && ap["text"] == "已從 origin/main 快轉拉取 1 個提交"
	})
	tv.waitGit("拉完重讀", func(g map[string]any) bool { return g["head"] == newHead[:7] && g["behind"] == float64(0) })
	if got := runGit(t, a, "rev-parse", "HEAD"); got != newHead {
		t.Fatalf("快轉之後的 HEAD：%s，要 %s", got, newHead)
	}

	// 有未提交的變更時不拉
	_ = os.WriteFile(filepath.Join(b, "g.txt"), []byte("2"), 0o644)
	runGit(t, b, "commit", "-am", "三")
	runGit(t, b, "push")
	_ = os.WriteFile(filepath.Join(a, "f.txt"), []byte("dirty"), 0o644)
	h.mu.Lock()
	for _, e := range h.repo.folders {
		e.pullAt = nil // 不等兩次拉取之間的間隔
	}
	h.mu.Unlock()
	tv.clear()
	tv.send(map[string]any{"cmd": "gitRefresh", "conversationId": id})
	tv.waitGit("這次不拉", func(g map[string]any) bool {
		ap, _ := g["autoPull"].(map[string]any)
		return ap != nil && ap["state"] == "skipped" && ap["text"] == "這次不拉：有 1 個已追蹤的檔案還沒提交"
	})
}

func TestGitNotRepoSendsNull(t *testing.T) {
	h, tv := newTestHub(t)
	h.mu.Lock()
	h.attachGitStatus(&cliGitReader{})
	h.mu.Unlock()
	id := tv.newDraft()
	dir := t.TempDir()
	tv.send(map[string]any{"cmd": "setFolder", "conversationId": id, "path": dir})
	time.Sleep(500 * time.Millisecond)
	h.mu.Lock()
	e := h.repo.folders[folderKey(dir)]
	known := e != nil && e.known && e.status == nil
	h.mu.Unlock()
	if !known {
		t.Fatal("不是 repo 的資料夾應該讀完、狀態是 nil")
	}
	tv.mu.Lock()
	defer tv.mu.Unlock()
	for _, m := range tv.msgs {
		if gitOf(m) != nil {
			t.Fatalf("不是 repo 不該送狀態：%v", m)
		}
	}
}

// 讀的途中又被要求（換資料夾、使用者）：讀完再讀一次；失敗之後 2 分鐘內只有使用者要求才讀
type fakeGitReader struct {
	calls  chan string
	block  chan struct{}
	fail   bool
	status *gitRepoStatus
}

func (f *fakeGitReader) Read(ctx context.Context, folder string) (*gitRepoStatus, error) {
	f.calls <- folder
	if f.block != nil {
		<-f.block
	}
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	return f.status, nil
}

func (f *fakeGitReader) CheckRemote(ctx context.Context, folder string, t gitRemoteTarget) (gitRemoteSnapshot, error) {
	return gitRemoteSnapshot{Target: t, CheckedAt: time.Now()}, nil
}

func (f *fakeGitReader) FastForward(ctx context.Context, folder string, plan *gitPullPlan) (gitPullResult, error) {
	return gitPullDone(0), nil
}

func TestGitReadCoalescingAndBackoff(t *testing.T) {
	h, tv := newTestHub(t)
	f := &fakeGitReader{calls: make(chan string, 10), block: make(chan struct{}), status: &gitRepoStatus{Root: "/r", CheckedAt: time.Now()}}
	id := tv.newDraft()
	h.mu.Lock()
	h.attachGitStatus(f)
	h.mu.Unlock()
	dir := t.TempDir()
	tv.send(map[string]any{"cmd": "setFolder", "conversationId": id, "path": dir})
	recvCall(t, f.calls)
	// 讀的途中：使用者要求兩次 → 讀完只再讀一次
	tv.send(map[string]any{"cmd": "gitRefresh", "conversationId": id})
	tv.send(map[string]any{"cmd": "gitRefresh", "conversationId": id})
	sendBlock(t, f.block)
	recvCall(t, f.calls)
	sendBlock(t, f.block)
	select {
	case <-f.calls:
		t.Fatal("讀的途中的要求只該再讀一次")
	case <-time.After(300 * time.Millisecond):
	}
	// 失敗：冷卻期內換資料夾不讀、使用者要求照讀
	close(f.block)
	f.block = nil
	h.mu.Lock()
	f.fail = true
	h.mu.Unlock()
	tv.send(map[string]any{"cmd": "gitRefresh", "conversationId": id})
	recvCall(t, f.calls)
	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	h.requestGit(h.convs[id], gitChanged)
	h.mu.Unlock()
	select {
	case <-f.calls:
		t.Fatal("失敗後的冷卻期內不該重讀")
	case <-time.After(300 * time.Millisecond):
	}
	tv.send(map[string]any{"cmd": "gitRefresh", "conversationId": id})
	select {
	case <-f.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("使用者要求要照讀")
	}
}

func recvCall(t *testing.T, ch chan string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("等不到讀取")
	}
}

func sendBlock(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case ch <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("讀取沒有在等")
	}
}
