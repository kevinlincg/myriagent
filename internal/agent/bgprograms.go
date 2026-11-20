package agent

import (
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"
)

// 對話開出的背景程式（C# Hub/AgentHub.BackgroundPrograms.cs；規則在 bgprograms_rules.go）：這一輪結束了、但代理開的程式脫離 CLI 還在跑的對話，
// 帳戶卡另外標出來（AccountView 的 sessions.programs 與 programs）；對話標頭的 programs 列出這個對話另開、還在跑的程式（可以停止，programStop），
// 程式自己結束時在對話裡加一則提示（noticeKind backgroundProgram；使用者按停止的不加、不叫回代理）。
// 掃描找到的只是候選：只有撐過一輪結束的才算（confirmed），對得上 CLI 背景工作的不算（cliTask）。
// 中樞預設的掃描器由啟動工作接上（registerStartHook）；測試的中樞沒有，要自己 attachPrograms

// bgState：一個中樞的背景程式狀態（都在 h.mu 底下）
type bgState struct {
	source    programSource
	scan      bgScan
	confirmed map[string]bool
	cliTask   map[string]bool
	quiet     map[string]bool
	conv      map[string]*bgConvState
	accSig    string
	started   time.Time
}

// bgConvState：一個對話的輪次狀態
type bgConvState struct {
	// idleSince：最近一輪結束的時間（一輪開始時清成 nil；沒執行過是 agent 啟動的時間）：在它之後才開始的掃描裡、對話不在執行中時還在的程式才算撐過了一輪
	idleSince *time.Time
	// idleSinceWall：同一個時間點（Unix 微秒，和程序的建立時間比）：之後才建立的程式是閒置時開出來的（別的背景工作的步驟）
	idleSinceWall int64
	// turnByCli：這一輪是 CLI 自己開的（背景工作跑完、叫回代理）：這時程式結束不加提示
	turnByCli bool
}

var bgStates sync.Map // *Hub → *bgState

// bg：這個中樞的背景程式狀態（第一次用到時建立）
func (h *Hub) bg() *bgState {
	if s, ok := bgStates.Load(h); ok {
		return s.(*bgState)
	}
	now := time.Now()
	s, _ := bgStates.LoadOrStore(h, &bgState{confirmed: map[string]bool{}, cliTask: map[string]bool{}, quiet: map[string]bool{}, conv: map[string]*bgConvState{}, started: now})
	return s.(*bgState)
}

func (st *bgState) convState(id string) *bgConvState {
	cs := st.conv[id]
	if cs == nil {
		t := st.started
		cs = &bgConvState{idleSince: &t, idleSinceWall: st.started.UnixMicro()}
		st.conv[id] = cs
	}
	return cs
}

func init() {
	registerStartHook(func(h *Hub) { h.attachPrograms(newProgramMonitor(newNativeSnapshot())) })
	registerTurnHook(onProgramsTurnStarted, onProgramsTurnCompleted)
	registerCommand("programStop", func(h *Hub, v *View, c *Conv, msg map[string]any) { h.stopProgram(v, c, str(msg, "programId")) })
}

// attachPrograms 接上背景程式的來源（不持有 h.mu）
func (h *Hub) attachPrograms(src programSource) {
	h.mu.Lock()
	h.bg().source = src
	h.mu.Unlock()
	src.start(func(scan bgScan) {
		h.mu.Lock()
		h.onProgramScan(scan)
		h.mu.Unlock()
		h.kick()
	})
}

// onProgramsTurnStarted：一輪開始：之後的掃描找到的新程式要等這一輪結束才算
func onProgramsTurnStarted(h *Hub, c *Conv) {
	cs := h.bg().convState(c.Meta.ID)
	cs.idleSince = nil
	cs.turnByCli = false
}

// onProgramsTurnCompleted：一輪結束：記下時間，立刻重掃（代理可能剛開了程式）
func onProgramsTurnCompleted(h *Hub, c *Conv) {
	st := h.bg()
	cs := st.convState(c.Meta.ID)
	now := time.Now()
	cs.idleSince, cs.idleSinceWall = &now, now.UnixMicro()
	if st.source != nil {
		st.source.requestScan()
	}
}

// onProgramScan：掃描結果（持有 h.mu）：記下來、判斷哪些撐過了一輪，帳戶卡的數字或說明變了就重送；列出的程式變了的對話重送標頭；
// 撐過一輪的程式自己結束時在它的對話裡加一則提示
func (h *Hub) onProgramScan(scan bgScan) {
	st := h.bg()
	before := st.scan
	ids := map[string]bool{}
	for id := range before.Programs {
		ids[id] = true
	}
	for id := range scan.Programs {
		ids[id] = true
	}
	shownBefore := map[string]string{}
	for id := range ids {
		shownBefore[id] = h.shownSignature(id)
	}
	st.scan = scan
	h.trackPrograms(before, scan)
	for _, e := range scan.Exits {
		id := e.Program.id()
		c := h.convs[e.ConvID]
		if e.StoppedByUser || !st.confirmed[id] || st.quiet[id] || c == nil || c.Meta.IsDraft {
			continue
		}
		// 這一輪是 CLI 自己開的：CLI 會告訴代理，不另外提示
		if c.running && st.convState(c.Meta.ID).turnByCli {
			continue
		}
		c.notice(h, "backgroundProgram", programExitNotice(e))
	}
	alive := map[string]bool{}
	for _, list := range scan.Programs {
		for _, p := range list {
			alive[p.id()] = true
		}
	}
	for _, set := range []map[string]bool{st.confirmed, st.cliTask, st.quiet} {
		for id := range set {
			if !alive[id] {
				delete(set, id)
			}
		}
	}
	for id := range ids {
		if c := h.convs[id]; c != nil && h.shownSignature(id) != shownBefore[id] {
			c.dirtyHeader = true
			c.dirtyList = true // 清單的 programCount（刪除前問要不要一併停止）
		}
	}
	if sig := h.programsSignature(); sig != st.accSig {
		st.accSig = sig
		h.pushAccounts()
	}
}

// trackPrograms：新出現的候選：這個對話不在執行中、而且掃描是在最近一輪結束之後才開始的，就撐過了一輪；
// 父程序是上一次已經算數的程式的（根結束了、子程序接著當根）跟著它。閒置中才開出來的（不是接續）是別的背景工作的步驟：列出、但結束時不提示
func (h *Hub) trackPrograms(before, scan bgScan) {
	st := h.bg()
	for id, programs := range scan.Programs {
		c := h.convs[id]
		if c == nil {
			continue
		}
		cs := st.convState(id)
		if !c.running && cs.idleSince == nil {
			// 沒經過結束掛鉤就停下的：從現在算起
			now := time.Now()
			cs.idleSince, cs.idleSinceWall = &now, now.UnixMicro()
		}
		settled := !c.running && cs.idleSince != nil && !scan.TakenAt.Before(*cs.idleSince)
		tasks := h.cliTaskTexts(c)
		earlier := before.Programs[id]
		for _, p := range programs {
			pid := p.id()
			if st.confirmed[pid] || st.cliTask[pid] {
				continue
			}
			var parent *bgProgram
			for i := range earlier {
				if earlier[i].Pid == p.Ppid && earlier[i].Start <= p.Start {
					parent = &earlier[i]
					break
				}
			}
			continues := parent != nil && st.confirmed[parent.id()]
			idleBorn := settled && !continues && p.Start > cs.idleSinceWall
			switch {
			case (parent != nil && st.cliTask[parent.id()]) || (tasks != nil && (idleBorn || isCliTask(p, tasks))):
				st.cliTask[pid] = true
			case settled || continues:
				st.confirmed[pid] = true
				if idleBorn || (continues && st.quiet[parent.id()]) {
					st.quiet[pid] = true
				}
			}
		}
	}
}

// cliTaskTexts：這個對話的 CLI 回報、還在跑的背景工作的說明與完整指令（沒有背景工作是 nil）。
// CLI 的背景工作（Claude 的 run_in_background、Codex 讓出的指令）由各家的 Provider 追蹤；還沒接上前一律 nil
var cliTaskTexts = func(h *Hub, c *Conv) []string { return nil }

func (h *Hub) cliTaskTexts(c *Conv) []string { return cliTaskTexts(h, c) }

// shownPrograms：這個對話列出的程式（撐過一輪、不是 CLI 的背景工作），依建立時間舊到新
func (h *Hub) shownPrograms(convID string) []bgProgram {
	st := h.bg()
	var out []bgProgram
	for _, p := range st.scan.Programs[convID] {
		if st.confirmed[p.id()] {
			out = append(out, p)
		}
	}
	return out
}

func (h *Hub) shownSignature(convID string) string {
	b, _ := json.Marshal(h.shownPrograms(convID))
	return string(b)
}

// programView：標頭 programs 的一筆
type programView struct {
	ID        string `json:"id"`
	Pid       int    `json:"pid"`
	Name      string `json:"name"`
	Command   string `json:"command"`
	StartedAt *int64 `json:"startedAt"`
}

// headerPrograms：標頭的 programs（Header.programs）：這個對話另開、撐過一輪結束還在跑的程式，依建立時間舊到新、最多 maxHeaderPrograms 個；
// 沒有（或草稿）是 nil。執行中也列之前的輪次留下的
func (h *Hub) headerPrograms(c *Conv) any {
	if c.Meta.IsDraft {
		return nil
	}
	list := h.shownPrograms(c.Meta.ID)
	if len(list) == 0 {
		return nil
	}
	if len(list) > maxHeaderPrograms {
		list = list[:maxHeaderPrograms]
	}
	out := make([]programView, 0, len(list))
	for _, p := range list {
		v := programView{ID: p.id(), Pid: p.Pid, Name: p.Name, Command: shortCommandN(p.CommandLine, p.Name, maxHeaderCmdLength)}
		if p.Start > 0 {
			ms := p.Start / 1000
			v.StartedAt = &ms
		}
		out = append(out, v)
	}
	return out
}

// programCount：清單的 programCount（同 Header.programs 的個數）
func (h *Hub) programCount(c *Conv) int {
	if c.Meta.IsDraft {
		return 0
	}
	return len(h.shownPrograms(c.Meta.ID))
}

// hasPrograms：這一輪結束了、但這個對話另開的程式還在跑（帳戶卡的 sessions.programs；草稿、執行中的不算）
func (h *Hub) hasPrograms(c *Conv) bool {
	return !c.Meta.IsDraft && !c.running && len(h.shownPrograms(c.Meta.ID)) > 0
}

// accountProgramsView：帳戶卡 programs 的一筆
type accountProgramsView struct {
	ConversationID string   `json:"conversationId"`
	Title          string   `json:"title"`
	Programs       []string `json:"programs"`
	More           int      `json:"more"`
}

// accountPrograms：帳戶卡的背景程式說明：有背景程式的對話依更新時間新到舊，每個對話列標題與程式的命令列（最多 maxListedPrograms 個）。沒有回 nil
func (h *Hub) accountPrograms(accountID string) []accountProgramsView {
	var convs []*Conv
	for _, c := range h.convs {
		if c.Meta.AccountID == accountID && h.hasPrograms(c) {
			convs = append(convs, c)
		}
	}
	sort.Slice(convs, func(i, j int) bool { return convs[i].Meta.UpdatedAt > convs[j].Meta.UpdatedAt })
	var out []accountProgramsView
	for _, c := range convs {
		found := h.shownPrograms(c.Meta.ID)
		v := accountProgramsView{ConversationID: c.Meta.ID, Title: c.Meta.Title, Programs: []string{}}
		for i, p := range found {
			if i == maxListedPrograms {
				break
			}
			v.Programs = append(v.Programs, shortCommand(p.CommandLine, p.Name))
		}
		if len(found) > maxListedPrograms {
			v.More = len(found) - maxListedPrograms
		}
		out = append(out, v)
	}
	return out
}

// programsSignature：所有帳戶的背景程式說明的摘要（帳戶卡是不是要重送）
func (h *Hub) programsSignature() string {
	all := map[string]any{}
	for _, a := range h.accounts {
		all[a.ID] = h.accountPrograms(a.ID)
	}
	b, _ := json.Marshal(all)
	return string(b)
}

// stopProgram：programStop：結束那個程式與它的子孫。只認這個對話最近一次掃描列出的 id（網頁不能指定 PID）；
// 已經不在清單、或結束不了時只對送指令的視窗提示
func (h *Hub) stopProgram(v *View, c *Conv, programID string) {
	src := h.bg().source
	if c == nil || src == nil || programID == "" {
		return
	}
	var target *bgProgram
	for _, p := range h.shownPrograms(c.Meta.ID) {
		if p.id() == programID {
			p := p
			target = &p
			break
		}
	}
	if target == nil {
		h.toast(v, "這個程式已經結束了")
		return
	}
	p := *target
	go func() {
		if src.stop(p) {
			return
		}
		h.mu.Lock()
		h.toast(v, "沒有停止「"+shortCommand(p.CommandLine, p.Name)+"」：它可能已經結束了，或沒有權限結束它")
		h.mu.Unlock()
	}()
}

// stopConversationPrograms：刪除對話時一併停止它另開的程式（delete 的 stopPrograms）。只停列出的
func (h *Hub) stopConversationPrograms(c *Conv) {
	src := h.bg().source
	if src == nil {
		return
	}
	for _, p := range h.shownPrograms(c.Meta.ID) {
		p := p
		go func() {
			if !src.stop(p) {
				log.Printf("刪除對話時停止背景程式 %d 失敗", p.Pid)
			}
		}()
	}
}
