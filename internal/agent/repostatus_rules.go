package agent

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 對話工作資料夾的 git 狀態的純規則（照 C# 的 src/Myriagent/Git/GitStatusRules.cs）：解析 git 的輸出、決定要和哪個遠端分支比、
// 產生給網頁的 GitStatusView（web-protocol 的 gitStatus）、決定什麼時候重讀。讀狀態只讀：git 指令一律以 GIT_OPTIONAL_LOCKS=0 執行
// （不寫索引、不取鎖），遠端只用 ls-remote 問；唯一會改 repo 的是有未拉取的提交時的自動快轉拉取（repostatus_pull.go）。

// gitPorcelain：git status --porcelain=v2 --branch --show-stash 整理出來的數字
type gitPorcelain struct {
	Oid       string // HEAD 的提交；還沒有任何提交是空字串
	Branch    string // 目前的分支；分離的 HEAD 是空字串
	Upstream  string // 上游分支的簡稱（origin/main）；沒設是空字串
	Ahead     *int   // 比上游多的提交；沒有上游、或上游已刪除是 nil
	Behind    *int
	Changed   int // 有變更的已追蹤檔案（一般與更名的項目）
	Staged    int
	Unstaged  int
	Untracked int
	Conflicts int
	Stashes   int
}

// gitRef：git for-each-ref 的一筆（格式見 gitRefFormat）
type gitRef struct {
	Name           string // refs/remotes/origin/main
	Object         string
	SymRef         string // refs/remotes/origin/HEAD 才有
	UpstreamRemote string // 本機分支的上游所在的遠端（上游是本機分支時是「.」）
	UpstreamRef    string // 上游在遠端的完整名稱（refs/heads/main）
}

// gitRemoteTarget：要問遠端的分支與這台記著的那個分支的提交
type gitRemoteTarget struct {
	Remote   string
	Ref      string
	LocalSha string
}

// gitRemoteSnapshot：問遠端的結果（Sha 空＝遠端沒有這個分支）
type gitRemoteSnapshot struct {
	Target    gitRemoteTarget
	Sha       string
	CheckedAt time.Time
}

// gitBaseRef：比較基準（遠端的預設分支）
type gitBaseRef struct {
	Name string // origin/main
	Ref  string // refs/remotes/origin/main
}

// gitRepoStatus：一個工作資料夾的 git 狀態（只讀本機；遠端的結果另外合併）
type gitRepoStatus struct {
	Root       string
	Porcelain  gitPorcelain
	Operation  string
	Base       *gitBaseRef
	BaseAhead  *int
	BaseBehind *int
	Remote     *gitRemoteTarget
	CommonDir  string // git 的共用資料夾（同一個 repo 的各個 worktree 都一樣）；不明是空字串
	CheckedAt  time.Time
}

type gitTrigger int

const (
	// gitOpen：開分頁、切分頁、視窗回到前景
	gitOpen gitTrigger = iota
	// gitPeriodic：維護計時器（畫面上看得到的對話）
	gitPeriodic
	// gitChanged：換了工作資料夾、這一輪結束
	gitChanged
	// gitUser：使用者按「重新檢查」
	gitUser
)

const (
	gitOpenMaxAge      = 5 * time.Second
	gitPeriodicMaxAge  = 25 * time.Second
	gitNotRepoMaxAge   = 2 * time.Minute
	gitRemoteInterval  = 5 * time.Minute
	gitFailureBackoff  = 2 * time.Minute
	gitRefFormat       = "%(refname)%09%(objectname)%09%(symref)%09%(upstream:remotename)%09%(upstream:remoteref)"
	gitRemotesPrefix   = "refs/remotes/"
	gitHeadsPrefix     = "refs/heads/"
	gitMaxConcurrentRd = 2
)

func ptrInt(i int) *int { return &i }

// gitRepoKey：同一個 repository 的鍵：git 的共用資料夾，不明時用最上層資料夾
func gitRepoKey(s *gitRepoStatus) string {
	p := s.CommonDir
	if p == "" {
		p = s.Root
	}
	t := strings.TrimRight(p, "/")
	if t == "" {
		return "/"
	}
	return t
}

// gitRemoteKey：問遠端的快取鍵：同一個 repository 的同一個遠端分支只問一次；沒有可問的分支是空字串
func gitRemoteKey(s *gitRepoStatus) string {
	if s.Remote == nil {
		return ""
	}
	return gitRepoKey(s) + "|" + s.Remote.Remote + "|" + s.Remote.Ref
}

// gitParseRevParse：rev-parse --show-toplevel --absolute-git-dir --git-common-dir 的三行（只有兩行時共用資料夾就是 .git 資料夾）；
// 共用資料夾可能是相對於執行時資料夾（cwd）的路徑。行數不對回 false
func gitParseRevParse(output, cwd string) (root, gitDir, common string, ok bool) {
	var lines []string
	for _, l := range strings.Split(output, "\n") {
		if l = strings.TrimSpace(strings.TrimRight(l, "\r")); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 && len(lines) != 3 {
		return "", "", "", false
	}
	abs := func(p string) string {
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	gitDir = abs(lines[1])
	common = gitDir
	if len(lines) == 3 {
		common = abs(lines[2])
	}
	return abs(lines[0]), gitDir, common, true
}

// gitParseStatus：解析 git status --porcelain=v2 --branch --show-stash（不加 -z：路徑裡的特殊字元會被加上引號，一筆一定是一行）
func gitParseStatus(output string) gitPorcelain {
	var p gitPorcelain
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.HasPrefix(line, "# ") {
			header := line[2:]
			if v, ok := gitHeaderValue(header, "branch.oid "); ok {
				if v != "(initial)" {
					p.Oid = v
				}
			} else if v, ok := gitHeaderValue(header, "branch.head "); ok {
				if v != "(detached)" {
					p.Branch = v
				}
			} else if v, ok := gitHeaderValue(header, "branch.upstream "); ok {
				p.Upstream = v
			} else if v, ok := gitHeaderValue(header, "branch.ab "); ok {
				p.Ahead, p.Behind = gitParseAheadBehind(v)
			} else if v, ok := gitHeaderValue(header, "stash "); ok {
				if n, err := strconv.Atoi(v); err == nil && n >= 0 {
					p.Stashes = n
				}
			}
			continue
		}
		if len(line) < 2 || line[1] != ' ' {
			continue
		}
		switch line[0] {
		case '1', '2':
			p.Changed++
			if len(line) >= 4 {
				if line[2] != '.' {
					p.Staged++
				}
				if line[3] != '.' {
					p.Unstaged++
				}
			}
		case 'u':
			p.Conflicts++
		case '?':
			p.Untracked++
		}
	}
	return p
}

func gitHeaderValue(header, key string) (string, bool) {
	if strings.HasPrefix(header, key) {
		return strings.TrimSpace(header[len(key):]), true
	}
	return "", false
}

// gitParseAheadBehind：「+3 -1」→ (3, 1)；格式不對回 nil
func gitParseAheadBehind(v string) (*int, *int) {
	parts := strings.Fields(v)
	if len(parts) == 2 && strings.HasPrefix(parts[0], "+") && strings.HasPrefix(parts[1], "-") {
		a, err1 := strconv.Atoi(parts[0][1:])
		b, err2 := strconv.Atoi(parts[1][1:])
		if err1 == nil && err2 == nil && a >= 0 && b >= 0 {
			return &a, &b
		}
	}
	return nil, nil
}

// gitRefPatterns：for-each-ref 要列的：目前的分支、上游、各遠端的 HEAD、main、master（不列全部的遠端分支：很多的 repo 會很慢）
func gitRefPatterns(p gitPorcelain) []string {
	var pats []string
	if p.Branch != "" {
		pats = append(pats, gitHeadsPrefix+p.Branch)
	}
	if p.Upstream != "" {
		pats = append(pats, gitRemotesPrefix+p.Upstream)
	}
	return append(pats, gitRemotesPrefix+"*/HEAD", gitRemotesPrefix+"*/main", gitRemotesPrefix+"*/master")
}

// gitParseRefs：解析 for-each-ref（gitRefFormat）的輸出；後面的空欄位可能被去掉
func gitParseRefs(output string) []gitRef {
	var refs []gitRef
	for _, raw := range strings.Split(output, "\n") {
		parts := strings.Split(strings.TrimRight(raw, "\r"), "\t")
		if len(parts) < 2 || len(parts) > 5 || parts[0] == "" || parts[1] == "" {
			continue
		}
		at := func(i int) string {
			if i < len(parts) {
				return parts[i]
			}
			return ""
		}
		refs = append(refs, gitRef{Name: parts[0], Object: parts[1], SymRef: at(2), UpstreamRemote: at(3), UpstreamRef: at(4)})
	}
	return refs
}

// gitPlanCompare：決定比較基準與要問遠端的分支：
//   - 遠端：上游所在的遠端，沒有上游就用 origin，再沒有就用唯一的那一個。
//   - 基準：那個遠端的預設分支（遠端/HEAD 指向的，沒有就 main、master）；就是上游時不另外比。
//   - 問遠端：有上游（而且是遠端分支）就問上游，否則問基準。
func gitPlanCompare(p gitPorcelain, refs []gitRef) (*gitBaseRef, *gitRemoteTarget) {
	byName := map[string]gitRef{}
	for _, r := range refs {
		if _, ok := byName[r.Name]; !ok {
			byName[r.Name] = r
		}
	}
	// 遠端/HEAD 指向的分支不一定在列出的範圍（例如指向 develop）：for-each-ref 給的 HEAD 提交就是它指向的那一個
	for _, r := range refs {
		if strings.HasPrefix(r.SymRef, gitRemotesPrefix) {
			if _, ok := byName[r.SymRef]; !ok {
				byName[r.SymRef] = gitRef{Name: r.SymRef, Object: r.Object}
			}
		}
	}
	var branchRef *gitRef
	if p.Branch != "" {
		if b, ok := byName[gitHeadsPrefix+p.Branch]; ok {
			branchRef = &b
		}
	}
	upstreamRemote := ""
	if branchRef != nil && branchRef.UpstreamRemote != "" && branchRef.UpstreamRemote != "." {
		upstreamRemote = branchRef.UpstreamRemote
	}
	var remotes []string
	seen := map[string]bool{}
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, gitRemotesPrefix) {
			continue
		}
		rest := r.Name[len(gitRemotesPrefix):]
		if i := strings.IndexByte(rest, '/'); i > 0 && !seen[rest[:i]] {
			seen[rest[:i]] = true
			remotes = append(remotes, rest[:i])
		}
	}
	remote := upstreamRemote
	if remote == "" {
		if seen["origin"] {
			remote = "origin"
		} else if len(remotes) == 1 {
			remote = remotes[0]
		}
	}
	var base *gitBaseRef
	if remote != "" {
		head := ""
		if h, ok := byName[gitRemotesPrefix+remote+"/HEAD"]; ok {
			head = h.SymRef
		}
		for _, c := range []string{head, gitRemotesPrefix + remote + "/main", gitRemotesPrefix + remote + "/master"} {
			if c == "" || !strings.HasPrefix(c, gitRemotesPrefix+remote+"/") {
				continue
			}
			if _, ok := byName[c]; ok {
				base = &gitBaseRef{Name: c[len(gitRemotesPrefix):], Ref: c}
				break
			}
		}
	}
	var target *gitRemoteTarget
	if tracking, ok := byName[gitRemotesPrefix+p.Upstream]; p.Upstream != "" && upstreamRemote != "" && branchRef != nil && branchRef.UpstreamRef != "" && ok {
		target = &gitRemoteTarget{Remote: upstreamRemote, Ref: branchRef.UpstreamRef, LocalSha: tracking.Object}
	} else if p.Upstream == "" && base != nil && remote != "" {
		if info, ok := byName[base.Ref]; ok {
			target = &gitRemoteTarget{Remote: remote, Ref: gitHeadsPrefix + base.Name[len(remote)+1:], LocalSha: info.Object}
		}
	}
	if base != nil && base.Name == p.Upstream {
		base = nil
	}
	return base, target
}

// gitParseLeftRight：rev-list --left-right --count A...B 的輸出（「3\t1」）
func gitParseLeftRight(output string) (int, int, bool) {
	parts := strings.Fields(output)
	if len(parts) != 2 {
		return 0, 0, false
	}
	l, err1 := strconv.Atoi(parts[0])
	r, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || l < 0 || r < 0 {
		return 0, 0, false
	}
	return l, r, true
}

// gitParseLsRemote：ls-remote 的輸出裡這個分支的提交；沒有這個分支回空字串
func gitParseLsRemote(output, ref string) string {
	for _, raw := range strings.Split(output, "\n") {
		parts := strings.Split(strings.TrimRight(raw, "\r"), "\t")
		if len(parts) == 2 && parts[1] == ref && len(parts[0]) >= 7 {
			return parts[0]
		}
	}
	return ""
}

// gitOperationOf：進行中的操作（看 .git 資料夾裡的標記檔；worktree 的是它自己的那一個）
func gitOperationOf(gitDir string, fileExists, dirExists func(string) bool) string {
	switch {
	case dirExists(filepath.Join(gitDir, "rebase-merge")) || dirExists(filepath.Join(gitDir, "rebase-apply")):
		return "rebase"
	case fileExists(filepath.Join(gitDir, "MERGE_HEAD")):
		return "merge"
	case fileExists(filepath.Join(gitDir, "CHERRY_PICK_HEAD")):
		return "cherryPick"
	case fileExists(filepath.Join(gitDir, "REVERT_HEAD")):
		return "revert"
	case fileExists(filepath.Join(gitDir, "BISECT_LOG")):
		return "bisect"
	}
	return ""
}

// gitShouldReadLocal：這次要不要讀本機的狀態。retryAfter：上次讀失敗時，這個時間之前只有使用者要求才讀
func gitShouldReadLocal(trigger gitTrigger, checkedAt *time.Time, isRepo bool, retryAfter *time.Time, now time.Time) bool {
	if trigger == gitUser {
		return true
	}
	if retryAfter != nil && now.Before(*retryAfter) {
		return false
	}
	if trigger == gitChanged || checkedAt == nil {
		return true
	}
	age := now.Sub(*checkedAt)
	if trigger == gitOpen {
		return age >= gitOpenMaxAge
	}
	if isRepo {
		return age >= gitPeriodicMaxAge
	}
	return age >= gitNotRepoMaxAge
}

// gitShouldCheckRemote：讀完本機之後要不要問遠端：使用者要求就問，其他每 5 分鐘
func gitShouldCheckRemote(trigger gitTrigger, remoteCheckedAt *time.Time, now time.Time) bool {
	return trigger == gitUser || remoteCheckedAt == nil || now.Sub(*remoteCheckedAt) >= gitRemoteInterval
}

// gitRemoteHasNew：遠端有沒有這台還沒下載的提交：問過同一個分支、遠端現在的提交和這台記著的不同就是 true；
// 沒問過、問的不是同一個分支、遠端沒有這個分支是 nil
func gitRemoteHasNew(target *gitRemoteTarget, snap *gitRemoteSnapshot) *bool {
	if target == nil || snap == nil || snap.Sha == "" || snap.Target.Remote != target.Remote || snap.Target.Ref != target.Ref {
		return nil
	}
	v := !strings.EqualFold(snap.Sha, target.LocalSha)
	return &v
}

// renderGitStatus：GitStatusView（web-protocol 第三節）
func renderGitStatus(s *gitRepoStatus, remote *gitRemoteSnapshot, autoPull *gitAutoPullView) map[string]any {
	p := s.Porcelain
	opt := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	optInt := func(v *int) any {
		if v == nil {
			return nil
		}
		return *v
	}
	var head any
	if len(p.Oid) >= 7 {
		head = p.Oid[:7]
	}
	var baseName any
	if s.Base != nil {
		baseName = s.Base.Name
	}
	var remoteName, remoteNew, remoteChecked any
	if s.Remote != nil {
		remoteName = s.Remote.Remote
		if remote != nil && remote.Target.Remote == s.Remote.Remote && remote.Target.Ref == s.Remote.Ref {
			remoteChecked = remote.CheckedAt.UnixMilli()
		}
	}
	if b := gitRemoteHasNew(s.Remote, remote); b != nil {
		remoteNew = *b
	}
	return map[string]any{
		"root": s.Root, "branch": opt(p.Branch), "head": head, "upstream": opt(p.Upstream),
		"upstreamGone": p.Upstream != "" && p.Ahead == nil,
		"ahead":        optInt(p.Ahead), "behind": optInt(p.Behind),
		"base": baseName, "baseAhead": optInt(s.BaseAhead), "baseBehind": optInt(s.BaseBehind),
		"changed": p.Changed, "staged": p.Staged, "unstaged": p.Unstaged, "untracked": p.Untracked,
		"conflicts": p.Conflicts, "stashes": p.Stashes, "operation": opt(s.Operation),
		"remote": remoteName, "remoteNew": remoteNew, "remoteCheckedAt": remoteChecked,
		"checkedAt": s.CheckedAt.UnixMilli(), "autoPull": renderGitAutoPull(autoPull),
	}
}
