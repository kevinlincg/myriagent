package agent

import (
	"context"
	"log"
	"time"
)

// 對話工作資料夾的 git 狀態（web-protocol 的 gitStatus／gitRefresh；照 C# 的 Hub/AgentHub.Git.cs）：本機的狀態以資料夾為單位快取
// （同一個資料夾的多個對話共用一份），讀完送給開著那些對話的 view。什麼時候讀（gitShouldReadLocal）：開分頁與切到前景（5 秒內讀過就不再讀）、
// 畫面上看得到的每 30 秒、換了資料夾與這一輪結束、使用者按「重新檢查」。
// 問遠端（ls-remote）以「repository＋遠端分支」為單位：不論有幾個對話、幾個資料夾，同一個遠端分支最多每 5 分鐘問一次，結果大家共用。
// 讀完本機或問完遠端時，有未拉取的提交就自動快轉拉取（repostatus_pull.go）；同一個 repository 同時只拉一次，拉完重讀這個 repo 的每個資料夾。
// 沒掛讀取器（attachGitStatus；測試預設）時什麼都不做、也不送 gitStatus。狀態都在 h.mu 底下改，git 指令在 goroutine、同時最多兩個

type repoHub struct {
	reader  gitStatusReader
	folders map[string]*gitFolderState
	remotes map[string]*gitRemoteState
	pulling map[string]bool
	slots   chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	clock   func() time.Time
	closed  bool
}

type gitFolderState struct {
	status     *gitRepoStatus // 最近一次讀到的；不是 repo 是 nil
	known      bool
	checkedAt  *time.Time
	retryAfter *time.Time
	reading    bool
	// again：讀的途中又被要求讀：讀完再讀一次；值是最強的那個原因
	again       *gitTrigger
	autoPull    *gitAutoPullView
	pullBusy    bool
	pullAt      *time.Time
	pullFailed  bool
	pullAskedAt *time.Time // 使用者按「重新檢查」的時間：之後的第一次拉取不管冷卻
}

type gitRemoteState struct {
	snapshot  *gitRemoteSnapshot
	checkedAt *time.Time
	reading   bool
}

func init() {
	registerStartHook(func(h *Hub) {
		h.mu.Lock()
		h.attachGitStatus(&cliGitReader{})
		h.mu.Unlock()
		gitMaintenance(h)
	})
	registerConvEvent(func(h *Hub, kind convEventKind, v *View, c *Conv) {
		if h.repo == nil {
			return
		}
		switch kind {
		case convOpened:
			// 先送快取的，太舊就重讀
			h.postGit(c, v)
			h.requestGit(c, gitOpen)
		case convVisible:
			// 切到這個分頁、視窗回到前景：太舊就重讀（使用者可能在別處提交、拉取了）
			h.requestGit(c, gitOpen)
		case convFolderChanged:
			h.postGit(c, nil)
			h.requestGit(c, gitChanged)
		case convTurnEnded:
			if h.convOpenAnywhere(c.Meta.ID) {
				h.requestGit(c, gitChanged)
			}
		}
	})
	registerShutdownHook(func(h *Hub) {
		if h.repo != nil && !h.repo.closed {
			h.repo.closed = true
			h.repo.cancel()
		}
	})
	registerCommand("gitRefresh", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		if c != nil {
			h.requestGit(c, gitUser)
		}
	})
}

// attachGitStatus：掛上讀取器（只掛一次）
func (h *Hub) attachGitStatus(r gitStatusReader) {
	ctx, cancel := context.WithCancel(context.Background())
	h.repo = &repoHub{reader: r, folders: map[string]*gitFolderState{}, remotes: map[string]*gitRemoteState{}, pulling: map[string]bool{},
		slots: make(chan struct{}, gitMaxConcurrentRd), ctx: ctx, cancel: cancel, clock: time.Now}
}

func gitMaintenance(h *Hub) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		h.mu.Lock()
		if h.repo == nil || h.repo.closed {
			h.mu.Unlock()
			return
		}
		for _, c := range h.convs {
			if h.convVisibleAnywhere(c.Meta.ID) {
				h.requestGit(c, gitPeriodic)
			}
		}
		h.mu.Unlock()
		h.kick()
	}
}

func (h *Hub) gitFolderOf(c *Conv) string {
	if h.repo == nil {
		return ""
	}
	return folderKey(c.workDir(h))
}

func (h *Hub) gitRemoteOf(e *gitFolderState) *gitRemoteState {
	if e.status == nil {
		return nil
	}
	if k := gitRemoteKey(e.status); k != "" {
		return h.repo.remotes[k]
	}
	return nil
}

// postGit：送這個對話目前知道的 git 狀態（還沒讀完或不是 repo 時 git 是 null）；only 是 nil＝開著它的所有 view
func (h *Hub) postGit(c *Conv, only *View) {
	if h.repo == nil {
		return
	}
	key := h.gitFolderOf(c)
	var g any
	if e := h.repo.folders[key]; key != "" && e != nil && e.known && e.status != nil {
		var snap *gitRemoteSnapshot
		if r := h.gitRemoteOf(e); r != nil {
			snap = r.snapshot
		}
		g = renderGitStatus(e.status, snap, e.autoPull)
	}
	msg := map[string]any{"type": "gitStatus", "conversationId": c.Meta.ID, "git": g}
	if only != nil {
		h.sendTo(only, msg)
	} else {
		h.sendToOpen(c.Meta.ID, msg)
	}
}

func (h *Hub) requestGit(c *Conv, trigger gitTrigger) {
	if key := h.gitFolderOf(c); key != "" {
		h.requestGitFolder(key, trigger)
	}
}

func (h *Hub) requestGitFolder(key string, trigger gitTrigger) {
	rh := h.repo
	if rh == nil || rh.closed {
		return
	}
	e := rh.folders[key]
	if e == nil {
		e = &gitFolderState{}
		rh.folders[key] = e
	}
	now := rh.clock()
	if trigger == gitUser {
		e.pullAskedAt = &now
	}
	if e.reading {
		// 讀的途中：要求的原因比定時強（資料可能已經變了）才在讀完後再讀一次
		if trigger == gitChanged || trigger == gitUser {
			t := trigger
			if e.again != nil && *e.again == gitUser {
				t = gitUser
			}
			e.again = &t
		}
		return
	}
	if gitShouldReadLocal(trigger, e.checkedAt, e.status != nil, e.retryAfter, now) {
		e.reading = true
		go h.readGit(key, e, trigger)
	} else {
		h.maybeCheckGitRemote(key, e, trigger, now)
	}
}

// maybeCheckGitRemote：這個資料夾所在 repository 的遠端分支該問了（使用者要求，或超過間隔沒問）就問
func (h *Hub) maybeCheckGitRemote(folderKey string, e *gitFolderState, trigger gitTrigger, now time.Time) {
	if e.status == nil || e.status.Remote == nil {
		return
	}
	rk := gitRemoteKey(e.status)
	r := h.repo.remotes[rk]
	if r == nil {
		r = &gitRemoteState{}
		h.repo.remotes[rk] = r
	}
	if !r.reading && gitShouldCheckRemote(trigger, r.checkedAt, now) {
		r.reading = true
		// 先記時間：失敗（離線、要登入）也要等下一個間隔才再問
		r.checkedAt = &now
		go h.checkGitRemote(folderKey, rk, r, *e.status.Remote)
	}
}

// withSlot：同時最多兩個 git 指令
func (rh *repoHub) withSlot(f func()) bool {
	select {
	case rh.slots <- struct{}{}:
	case <-rh.ctx.Done():
		return false
	}
	defer func() { <-rh.slots }()
	f()
	return true
}

func (h *Hub) readGit(key string, e *gitFolderState, trigger gitTrigger) {
	rh := h.repo
	for {
		var status *gitRepoStatus
		var err error
		if !rh.withSlot(func() { status, err = rh.reader.Read(rh.ctx, key) }) {
			h.mu.Lock()
			e.reading = false
			h.mu.Unlock()
			return
		}
		h.mu.Lock()
		if rh.closed {
			e.reading = false
			h.mu.Unlock()
			return
		}
		now := rh.clock()
		if err != nil {
			// 留著上次的狀態；一陣子之內只有使用者要求才再讀
			log.Printf("讀 git 狀態失敗（%s）：%v", key, err)
			retry := now.Add(gitFailureBackoff)
			e.retryAfter = &retry
			e.checkedAt = &now
			e.known = true
		} else {
			e.status = status
			e.known = true
			e.checkedAt = &now
			e.retryAfter = nil
			h.updateAutoPull(key, e)
		}
		h.postGitFolder(key)
		if err == nil {
			h.maybeCheckGitRemote(key, e, trigger, now)
		}
		again := e.again
		e.again = nil
		if again == nil {
			e.reading = false
		}
		h.mu.Unlock()
		h.kick()
		if again == nil {
			return
		}
		trigger = *again
	}
}

// checkGitRemote：問遠端（在發起的資料夾執行 ls-remote），結果給這個 repo 這個分支的每個資料夾
func (h *Hub) checkGitRemote(folderKey, remoteKey string, r *gitRemoteState, target gitRemoteTarget) {
	rh := h.repo
	snap, err := rh.reader.CheckRemote(rh.ctx, folderKey, target)
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	r.reading = false
	if rh.closed {
		return
	}
	if err != nil {
		log.Printf("問遠端的 git 分支失敗（%s）：%v", folderKey, err)
		return
	}
	r.snapshot = &snap
	for key, e := range rh.folders {
		if e.status != nil && gitRemoteKey(e.status) == remoteKey {
			h.updateAutoPull(key, e)
			h.postGitFolder(key)
		}
	}
}

// updateAutoPull：讀完本機或問完遠端之後：照規則決定要不要自動拉取。不拉的更新面板的說明；要拉的在背景執行
func (h *Hub) updateAutoPull(key string, e *gitFolderState) {
	rh := h.repo
	if rh.closed || e.pullBusy || e.status == nil {
		return
	}
	now := rh.clock()
	var snap *gitRemoteSnapshot
	if r := h.gitRemoteOf(e); r != nil {
		snap = r.snapshot
	}
	plan, reason := gitDecidePull(e.status, snap, h.gitAgentRunningIn(e.status.Root))
	if plan == nil {
		// 沒有要拉的：留著剛拉完的說明（讓使用者看到自動拉過了），其他的清掉；有提交可拉但不拉的寫出原因
		var view *gitAutoPullView
		if reason != "" {
			view = gitSkippedView(reason, now)
		} else if e.autoPull != nil && e.autoPull.State == gitPulled {
			view = e.autoPull
		}
		if !gitSameView(view, e.autoPull) {
			e.autoPull = view
		}
		return
	}
	repo := gitRepoKey(e.status)
	if rh.pulling[repo] {
		return
	}
	if !gitCanAttempt(e.pullAt, e.pullFailed, gitUserRequested(e.pullAskedAt, now), now) {
		return
	}
	e.pullAskedAt = nil
	e.pullBusy = true
	rh.pulling[repo] = true
	e.pullAt = &now
	e.autoPull = gitPullingView(plan, now)
	h.postGitFolder(key)
	go h.autoPull(key, e, plan, repo)
}

// gitAgentRunningIn：這個 repo 裡有沒有對話正在執行（代理可能正在改檔、提交：這時不拉）
func (h *Hub) gitAgentRunningIn(root string) bool {
	for _, c := range h.convs {
		if c.running {
			if f := h.gitFolderOf(c); f != "" && gitIsInside(f, root) {
				return true
			}
		}
	}
	return false
}

func (h *Hub) autoPull(key string, e *gitFolderState, plan *gitPullPlan, repo string) {
	rh := h.repo
	var result gitPullResult
	var err error
	ran := rh.withSlot(func() { result, err = rh.reader.FastForward(rh.ctx, key, plan) })
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	e.pullBusy = false
	delete(rh.pulling, repo)
	if !ran || rh.closed {
		return
	}
	if err != nil {
		result = gitPullFail(err.Error())
	}
	e.pullFailed = result.State == gitPullFailed
	e.autoPull = gitPullView(result, plan, rh.clock())
	if result.State == gitPulled {
		log.Printf("自動拉取 %s：從 %s 快轉 %d 個提交", key, plan.TargetName, result.Pulled)
	} else {
		log.Printf("自動拉取 %s 沒有拉：%s", key, result.Detail)
	}
	h.postGitFolder(key)
	// 拉完重讀這個 repository 的每個資料夾：未拉取歸零、HEAD 換成新的；fetch 更新了共用的追蹤分支，其他 worktree 的未拉取也變了
	for other, st := range rh.folders {
		if other == key || (st.status != nil && gitRepoKey(st.status) == repo) {
			h.requestGitFolder(other, gitChanged)
		}
	}
}

// postGitFolder：送給開著工作資料夾是這個的對話的 view
func (h *Hub) postGitFolder(key string) {
	for _, c := range h.convs {
		if h.convOpenAnywhere(c.Meta.ID) && h.gitFolderOf(c) == key {
			h.postGit(c, nil)
		}
	}
}
