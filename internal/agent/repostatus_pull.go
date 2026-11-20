package agent

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// 工作資料夾有未拉取的提交時自動拉取（照 C# 的 src/Myriagent/Git/GitAutoPullRules.cs；web-protocol GitStatusView 的 autoPull）。
// 只做快轉：git fetch 那一個遠端分支到它的追蹤分支，再 git merge --ff-only；不產生合併提交、不 rebase。有上游的分支和上游比，
// 分離的 HEAD 和遠端的預設分支比；沒有上游的分支不拉。有進行中的操作、衝突、這裡有不在目標上的提交、已追蹤的檔案有未提交的變更、
// 這個 repo 有對話正在執行時不拉，面板寫出原因。純函式

// gitPullPlan：先 fetch（遠端有還沒下載的提交時）、再快轉到 TargetRef
type gitPullPlan struct {
	TargetRef    string           // refs/remotes/origin/main
	TargetName   string           // origin/main（顯示用）
	Fetch        *gitRemoteTarget // 要先 fetch 的遠端分支；本機記著的已經比較新時是 nil
	ExpectedHead string           // 決定時的 HEAD：執行前 HEAD 變了就不拉
	Behind       int
}

type gitAutoPullState int

const (
	gitPulling gitAutoPullState = iota
	gitPulled
	gitPullSkipped
	gitPullFailed
)

// gitPullResult：讀取器回報的結果（Pulled 是快轉過來的提交數）
type gitPullResult struct {
	State  gitAutoPullState
	Pulled int
	Detail string
}

func gitPullDone(n int) gitPullResult { return gitPullResult{State: gitPulled, Pulled: n} }
func gitPullSkip(reason string) gitPullResult {
	return gitPullResult{State: gitPullSkipped, Detail: reason}
}
func gitPullFail(message string) gitPullResult {
	return gitPullResult{State: gitPullFailed, Detail: message}
}

// gitAutoPullView：面板上「自動拉取」那一列
type gitAutoPullView struct {
	State gitAutoPullState
	Text  string
	At    time.Time
}

const (
	gitPullFailureBackoff = 15 * time.Minute
	gitPullMinInterval    = time.Minute
	gitUserRequestWindow  = 2 * time.Minute
)

var gitOperationNames = map[string]string{"merge": "合併", "rebase": "rebase", "cherryPick": "揀選（cherry-pick）", "revert": "還原（revert）", "bisect": "bisect"}

// gitDecidePull：這次要不要自動拉取。plan 不是 nil 就拉；reason 不空是有提交可拉、但這次不拉的原因；兩個都沒有＝沒有要拉的
func gitDecidePull(s *gitRepoStatus, remote *gitRemoteSnapshot, agentRunning bool) (*gitPullPlan, string) {
	p := s.Porcelain
	if p.Oid == "" {
		return nil, ""
	}
	var targetRef, targetName string
	var ahead, behind int
	tracking := gitTrackingRefOf(s.Remote)
	if p.Branch != "" {
		// 有上游、上游還在而且是遠端分支：和上游比。沒有上游的分支不拉
		if p.Upstream == "" || p.Ahead == nil || p.Behind == nil || tracking != gitRemotesPrefix+p.Upstream {
			return nil, ""
		}
		targetRef, targetName, ahead, behind = gitRemotesPrefix+p.Upstream, p.Upstream, *p.Ahead, *p.Behind
	} else {
		// 分離的 HEAD：和遠端的預設分支比
		if s.Base == nil || s.BaseAhead == nil || s.BaseBehind == nil || tracking != s.Base.Ref {
			return nil, ""
		}
		targetRef, targetName, ahead, behind = s.Base.Ref, s.Base.Name, *s.BaseAhead, *s.BaseBehind
	}
	newer := gitRemoteHasNew(s.Remote, remote)
	remoteNew := newer != nil && *newer
	if behind == 0 && !remoteNew {
		return nil, ""
	}
	if s.Operation != "" {
		name := gitOperationNames[s.Operation]
		if name == "" {
			name = s.Operation
		}
		return nil, "有進行中的" + name + "還沒完成"
	}
	if p.Conflicts > 0 {
		return nil, fmt.Sprintf("有 %d 個檔案的衝突還沒解決", p.Conflicts)
	}
	if ahead > 0 {
		return nil, fmt.Sprintf("這裡有 %d 個提交不在 %s，不能快轉", ahead, targetName)
	}
	if p.Changed > 0 {
		return nil, fmt.Sprintf("有 %d 個已追蹤的檔案還沒提交", p.Changed)
	}
	if agentRunning {
		return nil, "這個資料夾有對話正在執行，結束後再拉"
	}
	plan := &gitPullPlan{TargetRef: targetRef, TargetName: targetName, ExpectedHead: p.Oid, Behind: behind}
	if remoteNew {
		t := *s.Remote
		plan.Fetch = &t
	}
	return plan, ""
}

// gitTrackingRefOf：遠端分支在這台的追蹤分支（refs/heads/main → refs/remotes/origin/main）；不是分支回空字串
func gitTrackingRefOf(t *gitRemoteTarget) string {
	if t == nil || !strings.HasPrefix(t.Ref, gitHeadsPrefix) || len(t.Ref) <= len(gitHeadsPrefix) {
		return ""
	}
	return gitRemotesPrefix + t.Remote + "/" + t.Ref[len(gitHeadsPrefix):]
}

// gitRefspec：fetch 用的 refspec：只更新要快轉的那一個追蹤分支（遠端被強制推送時照樣跟上）
func gitRefspec(p *gitPullPlan) string { return "+" + p.Fetch.Ref + ":" + p.TargetRef }

// gitUserRequested：使用者剛按過「重新檢查」（2 分鐘內）、還沒用掉
func gitUserRequested(requestedAt *time.Time, now time.Time) bool {
	return requestedAt != nil && now.Sub(*requestedAt) < gitUserRequestWindow && !now.Before(*requestedAt)
}

// gitCanAttempt：使用者要求一律可以；否則離上次自動拉取至少 1 分鐘，上次失敗的要等 15 分鐘
func gitCanAttempt(lastAttempt *time.Time, lastFailed, userRequested bool, now time.Time) bool {
	if userRequested || lastAttempt == nil {
		return true
	}
	wait := gitPullMinInterval
	if lastFailed {
		wait = gitPullFailureBackoff
	}
	return now.Sub(*lastAttempt) >= wait
}

// gitIsInside：工作資料夾在這個 repo 裡（同一個資料夾或子資料夾）
func gitIsInside(folder, root string) bool {
	f := strings.TrimRight(folder, "/")
	r := strings.TrimRight(root, "/")
	return f == r || (len(f) > len(r)+1 && strings.HasPrefix(f, r) && f[len(r)] == '/')
}

func gitPullingView(p *gitPullPlan, now time.Time) *gitAutoPullView {
	return &gitAutoPullView{State: gitPulling, Text: "正在從 " + p.TargetName + " 拉取…", At: now}
}

func gitSkippedView(reason string, now time.Time) *gitAutoPullView {
	return &gitAutoPullView{State: gitPullSkipped, Text: "這次不拉：" + reason, At: now}
}

// gitPullView：拉取的結果要顯示什麼；快轉了 0 個（fetch 之後發現已經是最新）不顯示
func gitPullView(r gitPullResult, p *gitPullPlan, now time.Time) *gitAutoPullView {
	switch r.State {
	case gitPulled:
		if r.Pulled > 0 {
			return &gitAutoPullView{State: gitPulled, Text: fmt.Sprintf("已從 %s 快轉拉取 %d 個提交", p.TargetName, r.Pulled), At: now}
		}
		return nil
	case gitPullSkipped:
		d := r.Detail
		if d == "" {
			d = "狀態在檢查之後變了"
		}
		return gitSkippedView(d, now)
	case gitPullFailed:
		d := r.Detail
		if d == "" {
			d = "原因不明"
		}
		return &gitAutoPullView{State: gitPullFailed, Text: "失敗：" + d, At: now}
	}
	return gitPullingView(p, now)
}

// gitSameView：兩個說明是不是同一件事（不看時間）
func gitSameView(a, b *gitAutoPullView) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.State == b.State && a.Text == b.Text
}

var gitURLCredentials = regexp.MustCompile(`://[^/\s@]+@`)

// gitErrorLine：git 的錯誤輸出整理成一行：優先取 fatal:／error: 開頭的那一行，否則第一個非空行；網址裡的帳號密碼遮掉、最多 200 字
func gitErrorLine(stderr, fallback string) string {
	var lines []string
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(strings.TrimRight(l, "\r")); l != "" {
			lines = append(lines, l)
		}
	}
	line := ""
	for _, l := range lines {
		ll := strings.ToLower(l)
		if strings.HasPrefix(ll, "fatal:") || strings.HasPrefix(ll, "error:") {
			line = l
			break
		}
	}
	if line == "" && len(lines) > 0 {
		line = lines[0]
	}
	if line == "" {
		return fallback
	}
	line = gitURLCredentials.ReplaceAllString(line, "://***@")
	if utf8.RuneCountInString(line) > 200 {
		line = string([]rune(line)[:200]) + "…"
	}
	return line
}

func renderGitAutoPull(v *gitAutoPullView) any {
	if v == nil {
		return nil
	}
	return map[string]any{"state": [...]string{"pulling", "pulled", "skipped", "failed"}[v.State], "text": v.Text, "at": v.At.UnixMilli()}
}
