package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// gitStatusReader：讀 git 狀態（中樞在 goroutine 呼叫；測試換成假的）
type gitStatusReader interface {
	// Read 讀本機的狀態（不連網路）。不是 git repo、沒有 git、在網路磁碟上回 nil, nil；git 出錯或逾時回錯誤
	Read(ctx context.Context, folder string) (*gitRepoStatus, error)
	// CheckRemote 問遠端這個分支現在的提交（ls-remote，要連網路）。遠端沒有這個分支時 Sha 是空的；連不上、要登入、逾時回錯誤
	CheckRemote(ctx context.Context, folder string, t gitRemoteTarget) (gitRemoteSnapshot, error)
	// FastForward 自動拉取：需要時先 fetch 那一個遠端分支，重讀狀態再判斷一次（HEAD 變了、有了未提交的變更就不拉），再 merge --ff-only。
	// git 失敗回 Failed（不回錯誤）；找不到 git、逾時回錯誤
	FastForward(ctx context.Context, folder string, plan *gitPullPlan) (gitPullResult, error)
}

// cliGitReader：以本機的 git 讀狀態。讀的部分只讀：GIT_OPTIONAL_LOCKS=0（status 不重寫索引、不取 index.lock，不會和代理的 git 撞在一起）、
// core.fsmonitor=false（repo 設定的監看程式不跟著執行）；問遠端用 ls-remote（不更新任何參照），而且不跳任何登入提示。
// 自動拉取是唯一會改 repo 的：fetch 一個追蹤分支、快轉，同樣不跳登入提示
type cliGitReader struct {
	mu         sync.Mutex
	path       string
	nextLocate time.Time
}

const (
	gitLocalTimeout  = 15 * time.Second
	gitRemoteTimeout = 20 * time.Second
	gitPullTimeout   = 60 * time.Second
	gitLocateRetry   = 10 * time.Minute
)

type gitRun struct {
	code     int
	stdout   string
	stderr   string
	timedOut bool
}

func (r *cliGitReader) Read(ctx context.Context, folder string) (*gitRepoStatus, error) {
	if !filepath.IsAbs(folder) || folderOnNetworkFS(folder) {
		return nil, nil
	}
	if fi, err := os.Stat(folder); err != nil || !fi.IsDir() {
		return nil, nil
	}
	git := r.locate()
	if git == "" {
		return nil, nil
	}
	top, err := r.run(ctx, git, folder, gitLocalTimeout, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	// 不是 repo（128）、在 .git 裡面、被 safe.directory 擋下：都當成不是 repo
	root, gitDir, common, ok := gitParseRevParse(top.stdout, folder)
	if top.code != 0 || !ok {
		return nil, nil
	}
	st, err := r.run(ctx, git, folder, gitLocalTimeout, "status", "--porcelain=v2", "--branch", "--show-stash")
	if err != nil {
		return nil, err
	}
	if st.code != 0 {
		return nil, fmt.Errorf("git status 失敗（%d）：%s", st.code, gitTail(st.stderr))
	}
	porcelain := gitParseStatus(st.stdout)
	refsRun, err := r.run(ctx, git, folder, gitLocalTimeout, append([]string{"for-each-ref", "--format=" + gitRefFormat}, gitRefPatterns(porcelain)...)...)
	if err != nil {
		return nil, err
	}
	var refs []gitRef
	if refsRun.code == 0 {
		refs = gitParseRefs(refsRun.stdout)
	}
	base, target := gitPlanCompare(porcelain, refs)
	s := &gitRepoStatus{Root: root, Porcelain: porcelain, Base: base, Remote: target, CommonDir: common,
		Operation: gitOperationOf(gitDir, launchFileExists, dirExists)}
	if base != nil && porcelain.Oid != "" {
		counts, err := r.run(ctx, git, folder, gitLocalTimeout, "rev-list", "--left-right", "--count", "HEAD..."+base.Ref)
		if err != nil {
			return nil, err
		}
		if counts.code == 0 {
			if l, rr, ok := gitParseLeftRight(counts.stdout); ok {
				s.BaseAhead, s.BaseBehind = ptrInt(l), ptrInt(rr)
			}
		}
	}
	s.CheckedAt = time.Now()
	return s, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (r *cliGitReader) CheckRemote(ctx context.Context, folder string, t gitRemoteTarget) (gitRemoteSnapshot, error) {
	git := r.locate()
	if git == "" {
		return gitRemoteSnapshot{}, errors.New("找不到 git")
	}
	res, err := r.run(ctx, git, folder, gitRemoteTimeout, "ls-remote", "--refs", t.Remote, t.Ref)
	if err != nil {
		return gitRemoteSnapshot{}, err
	}
	if res.code != 0 {
		return gitRemoteSnapshot{}, fmt.Errorf("git ls-remote 失敗（%d）：%s", res.code, gitTail(res.stderr))
	}
	return gitRemoteSnapshot{Target: t, Sha: gitParseLsRemote(res.stdout, t.Ref), CheckedAt: time.Now()}, nil
}

func (r *cliGitReader) FastForward(ctx context.Context, folder string, plan *gitPullPlan) (gitPullResult, error) {
	git := r.locate()
	if git == "" {
		return gitPullResult{}, errors.New("找不到 git")
	}
	if plan.Fetch != nil {
		f, err := r.run(ctx, git, folder, gitPullTimeout, "fetch", "--no-tags", "--no-recurse-submodules", "--quiet", plan.Fetch.Remote, gitRefspec(plan))
		if err != nil {
			return gitPullResult{}, err
		}
		if f.code != 0 {
			return gitPullFail(gitErrorLine(f.stderr, fmt.Sprintf("git fetch 失敗（%d）", f.code))), nil
		}
	}
	// 執行前再看一次（決定到現在，代理、使用者或其他程式可能提交了、改了檔、換了分支）
	now, err := r.Read(ctx, folder)
	if err != nil {
		return gitPullResult{}, err
	}
	if now == nil {
		return gitPullFail("讀不到這個 repo"), nil
	}
	if !strings.EqualFold(now.Porcelain.Oid, plan.ExpectedHead) {
		return gitPullSkip("HEAD 在檢查之後變了"), nil
	}
	fresh, reason := gitDecidePull(now, nil, false)
	if fresh == nil {
		if reason != "" {
			return gitPullSkip(reason), nil
		}
		return gitPullDone(0), nil
	}
	if fresh.TargetRef != plan.TargetRef {
		return gitPullSkip("要比較的分支在檢查之後變了"), nil
	}
	m, err := r.run(ctx, git, folder, gitPullTimeout, "merge", "--ff-only", "--no-stat", "--quiet", fresh.TargetRef)
	if err != nil {
		return gitPullResult{}, err
	}
	if m.code != 0 {
		return gitPullFail(gitErrorLine(m.stderr, fmt.Sprintf("git merge --ff-only 失敗（%d）", m.code))), nil
	}
	return gitPullDone(fresh.Behind), nil
}

// run：在 folder 執行 git（-c core.fsmonitor=false）；逾時回錯誤，其他結束碼照實回
func (r *cliGitReader) run(ctx context.Context, git, folder string, timeout time.Duration, args ...string) (gitRun, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = folder
	cmd.Env = gitEnv(os.Environ())
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return gitRun{timedOut: true}, fmt.Errorf("git %s 逾時", args[0])
	}
	if ctx.Err() != nil {
		return gitRun{}, ctx.Err()
	}
	res := gitRun{stdout: out.String(), stderr: errb.String()}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return res, fmt.Errorf("執行 git 失敗：%v", err)
		}
		res.code = ee.ExitCode()
	}
	return res, nil
}

// gitEnv：不取鎖、不跳提示；拿掉會把 git 指到別的 repo 的變數
func gitEnv(environ []string) []string {
	drop := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true, "GIT_COMMON_DIR": true, "GIT_NAMESPACE": true,
		"GIT_OPTIONAL_LOCKS": true, "GIT_TERMINAL_PROMPT": true, "GCM_INTERACTIVE": true}
	env := []string{}
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[kv[:i]] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
}

func (r *cliGitReader) locate() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path != "" && launchFileExists(r.path) {
		return r.path
	}
	if time.Now().Before(r.nextLocate) {
		return ""
	}
	r.path = gitFind(os.Getenv("PATH"), []string{"/opt/homebrew/bin/git", "/usr/local/bin/git", "/usr/bin/git"}, launchFileExists, gitUsable)
	if r.path == "" {
		r.nextLocate = time.Now().Add(gitLocateRetry)
		log.Printf("找不到 git：對話的工作資料夾不顯示 git 狀態")
	}
	return r.path
}

// gitFind：在 PATH 找 git（只認完整路徑的項目：不從目前資料夾找），找不到再試常見的安裝位置；usable 回 false 的跳過
func gitFind(pathVar string, fallbacks []string, exists, usable func(string) bool) string {
	for _, dir := range strings.Split(pathVar, ":") {
		dir = strings.TrimSpace(dir)
		if dir == "" || !strings.HasPrefix(dir, "/") {
			continue
		}
		c := filepath.Join(dir, "git")
		if exists(c) && usable(c) {
			return c
		}
	}
	for _, c := range fallbacks {
		if exists(c) && usable(c) {
			return c
		}
	}
	return ""
}

// gitUsable：macOS 的 /usr/bin/git 是個殼：沒裝命令列工具時一執行就跳出安裝視窗，先以 xcode-select -p 確認裝了
func gitUsable(path string) bool {
	if runtime.GOOS != "darwin" || path != "/usr/bin/git" {
		return true
	}
	return exec.Command("/usr/bin/xcode-select", "-p").Run() == nil
}

func gitTail(s string) string {
	t := strings.TrimSpace(s)
	r := []rune(t)
	if len(r) <= 300 {
		return t
	}
	return string(r[len(r)-300:])
}
