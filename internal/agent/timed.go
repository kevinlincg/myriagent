package agent

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kevinlincg/myriagent/internal/schedules"
)

// Myriagent 的定時訊息（排程頁的 S4；C# 的 AgentHub.TimedMessages.cs）：在指定的時間對某個對話送一則訊息（或每次開新對話），
// 取代 Claude 的 CronCreate（程序關掉就失效），四種代理都能用、agent 重開也不會丟。
// 定義在 <資料根目錄>/schedules/timed.json；計時器排到最近的一則（最多一小時，維護每 30 秒也補看一次，睡眠或改時間之後照樣接上），
// 到點時照一般的送出（對話執行中就排進佇列，不插進這一輪）；錯過的重複訊息只補一次、一次性的錯過超過 12 小時不補。
// 建立：代理以 myriagent schedule（本機介面的 op:"timed"，對話權杖驗過）、使用者在排程頁新增（timedSave）。只在 agent 開著時送

// timedStartupDelay 計時器第一次可以送的時間（agent 剛啟動時先等帳戶與畫面就緒）
const timedStartupDelay = 20 * time.Second

// timedPending 送出之後在等這一輪結束的
type timedPending struct {
	msg *schedules.TimedMessage
	run *schedules.TimedRun
	at  time.Time
}

func init() {
	registerCommand("timedSave", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		when, _ := msg["when"].(map[string]any)
		newConv, _ := msg["newConversation"].(bool)
		_, problem := h.saveTimed(str(msg, "id"), str(msg, "text"), str(msg, "label"), str(when, "kind"), str(when, "value"), str(msg, "conversationId"), newConv, nil)
		if problem != "" {
			h.toast(v, problem)
		} else {
			h.toast(v, "已儲存定時訊息")
		}
	})
	registerCommand("timedAction", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		if p := h.timedAction(str(msg, "id"), str(msg, "action")); p != "" {
			h.toast(v, p)
		}
	})
	registerLocalOp("timed", func(ctx context.Context, h *Hub, req map[string]any, emit func(any) error) {
		args, _ := req["timed"].(map[string]any)
		if args == nil {
			_ = emit(map[string]any{"t": "error", "code": localErrInvalid, "message": "少了定時訊息的內容"})
			return
		}
		h.mu.Lock()
		res := h.handleTimedCLI(str(req, "conversation"), args)
		h.mu.Unlock()
		h.kick()
		res["t"] = "timed"
		_ = emit(res)
	})
	registerInstructions(func(h *Hub, c *Conv) string { return h.timedInstructions() })
}

func (s *scheduleHub) timedPath() string { return filepath.Join(s.dir, "timed.json") }

// loadTimedMessages 讀定時訊息、排計時器（agent 剛啟動，先等一下才送）
func (h *Hub) loadTimedMessages() {
	s := h.sched
	s.timed = []*schedules.TimedMessage{}
	if b, err := os.ReadFile(s.timedPath()); err == nil {
		if json.Unmarshal(b, &s.timed) != nil {
			log.Printf("定時訊息：timed.json 讀不懂，改名留著")
			_ = os.Rename(s.timedPath(), s.timedPath()+".bad-"+time.Now().Format("20060102-150405"))
			s.timed = []*schedules.TimedMessage{}
		}
	}
	s.timedReadyAt = s.clock().Add(timedStartupDelay)
	h.armTimedTimer()
}

func (h *Hub) saveTimedMessages() {
	s := h.sched
	if s == nil || s.timed == nil {
		return
	}
	writeJSON(s.timedPath(), s.timed)
}

// armTimedTimer 計時器排到最近一則（最多一小時；還在啟動的等待期就排到等待結束）
func (h *Hub) armTimedTimer() {
	s := h.sched
	if s == nil || s.ctx.Err() != nil {
		return
	}
	now := s.clock()
	var next *time.Time
	for _, m := range s.timed {
		if d := schedules.NextDue(m); d != nil && (next == nil || d.Before(*next)) {
			next = d
		}
	}
	wait := time.Hour
	if next != nil {
		at := *next
		if at.Before(s.timedReadyAt) {
			at = s.timedReadyAt
		}
		wait = at.Sub(now)
	}
	if wait < 0 {
		wait = 0
	}
	if wait > time.Hour {
		wait = time.Hour
	}
	wait += 200 * time.Millisecond
	if s.timedTimer != nil {
		s.timedTimer.Stop()
	}
	s.timedTimer = time.AfterFunc(wait, func() {
		h.mu.Lock()
		h.tickTimed()
		h.mu.Unlock()
		h.kick()
	})
}

// tickTimed 到點的送出（計時器、維護；持有 h.mu）
func (h *Hub) tickTimed() {
	s := h.sched
	if s == nil || s.ctx.Err() != nil {
		return
	}
	now := s.clock()
	if now.Before(s.timedReadyAt) {
		h.armTimedTimer()
		return
	}
	changed := false
	for _, m := range append([]*schedules.TimedMessage{}, s.timed...) {
		due, scheduled, late := schedules.Decide(m, now)
		switch due {
		case schedules.DueFire:
			h.fireTimed(m, scheduled, late, now)
			changed = true
		case schedules.DueMissed:
			sc := scheduled
			m.LastScheduledAt = &sc
			m.Done = true
			schedules.AddRun(m, schedules.TimedRun{ScheduledAt: scheduled, Outcome: "missed"})
			h.broadcast(map[string]any{"type": "toast", "text": "定時訊息「" + schedules.LabelOf(m) + "」原定 " + scheduled.Format("1/2 15:04") + "，錯過太久沒有補送"})
			changed = true
		}
	}
	// 等一輪結束的記號太久沒結束（被拒、對話被刪）就放掉
	for id, p := range s.timedPending {
		if now.Sub(p.at) > 6*time.Hour {
			delete(s.timedPending, id)
		}
	}
	if changed {
		h.saveTimedMessages()
		h.postSchedules(true)
	}
	h.armTimedTimer()
}

// fireTimed 送出一則：對話在就照一般的送出（執行中排進佇列）；每次開新對話的先開一個
func (h *Hub) fireTimed(m *schedules.TimedMessage, scheduled time.Time, late bool, now time.Time) {
	s := h.sched
	sc, fired := scheduled, now
	m.LastScheduledAt = &sc
	if m.Cron == "" {
		m.Done = true
	}
	run := schedules.AddRun(m, schedules.TimedRun{ScheduledAt: scheduled, FiredAt: &fired})
	label := schedules.LabelOf(m)
	var c *Conv
	if m.NewConversation {
		var problem string
		c, problem = h.createTimedConversation(m, now)
		if c == nil {
			run.Outcome, run.Error = "error", problem
			h.broadcast(map[string]any{"type": "toast", "text": "定時訊息「" + label + "」沒有送出：" + problem})
			return
		}
	} else {
		c = h.convs[m.ConversationID]
		if m.ConversationID == "" || c == nil || c.Meta.IsDraft {
			run.Outcome, run.Error = "error", "對話已經刪除"
			m.Enabled = false
			h.broadcast(map[string]any{"type": "toast", "text": "定時訊息「" + label + "」的對話已經刪除，已停用"})
			return
		}
	}
	run.ConversationID = c.Meta.ID
	text := schedules.AgentText(m, scheduled, late)
	display := schedules.DisplayText(m, late)
	if h.sendHeadless(c, text, display) {
		run.Outcome = "queued"
	} else if !c.running {
		// 沒辦法開始這一輪（CLI 不在、帳戶移除）：錯誤已經記在對話裡
		run.Outcome = "failed"
	} else {
		run.Outcome = "sent"
		s.timedPending[c.Meta.ID] = &timedPending{msg: m, run: run, at: now}
	}
	extra := ""
	if late {
		extra = "，補送"
	}
	log.Printf("定時訊息 %s 送進對話 %s（原定 %s%s）", m.ID, c.Meta.ID, scheduled.Format("2006-01-02 15:04"), extra)
}

// createTimedConversation 每次開新對話：照記下的帳戶、模型、推理強度、權限模式、資料夾開一個正式對話
func (h *Hub) createTimedConversation(m *schedules.TimedMessage, now time.Time) (*Conv, string) {
	a := h.account(m.AccountID)
	if a == nil {
		return nil, "帳戶已經移除"
	}
	if a.status == "notLoggedIn" || a.status == "notInstalled" {
		return nil, "「" + a.Label + "」" + statusText[a.status]
	}
	c := h.createConversation(a, schedules.LabelOf(m)+"（"+now.Format("1/2 15:04")+"）", m.Folder)
	if m.Model != "" {
		c.Meta.Model = m.Model
	}
	if m.Effort != "" {
		c.Meta.Effort = m.Effort
	}
	if m.PermissionMode != "" {
		for _, o := range permissionModes(a.Provider) {
			if o.Value == m.PermissionMode {
				c.Meta.Permission = m.PermissionMode
			}
		}
	}
	c.save(h)
	return c, ""
}

// onTimedTurnCompleted 一輪結束：定時訊息送出的那一輪記下結果
func (h *Hub) onTimedTurnCompleted(c *Conv, outcome string) {
	s := h.sched
	if s == nil {
		return
	}
	p := s.timedPending[c.Meta.ID]
	if p == nil {
		return
	}
	// 額度用完換帳戶、API 過載重試、等額度重置：中樞會自動接著做，結果等接著做的那一輪（這些還沒移植，目前一律直接記）
	if schedules.WaitsForContinuation(outcome == turnCompleted, false, false) {
		p.at = s.clock()
		return
	}
	delete(s.timedPending, c.Meta.ID)
	p.run.Outcome = schedules.OutcomeName(outcome == turnFailed, outcome == turnInterrupted)
	h.saveTimedMessages()
	h.postSchedules(true)
}

// saveTimed 新增或修改（排程頁的 timedSave、myriagent schedule add）：檢查時間、文字、對話；成功回那一則，失敗回原因。
// from＝建立它的對話（代理）；每次開新對話時帳戶、模型、權限模式、資料夾照送進的對話
func (h *Hub) saveTimed(id, text, label, whenKind, whenValue, convID string, newConversation bool, from *Conv) (*schedules.TimedMessage, string) {
	s := h.sched
	if s == nil {
		return nil, "這台的 Myriagent 沒有開定時訊息"
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "少了要送的文字"
	}
	if len([]rune(text)) > schedules.MaxTimedText {
		return nil, "文字超過 20000 字"
	}
	now := s.clock()
	cron, at, problem := schedules.ParseWhen(whenKind, whenValue, now)
	if problem != "" {
		return nil, problem
	}
	target := h.convs[convID]
	if convID == "" || target == nil {
		return nil, "找不到要送進的對話"
	}
	if target.Meta.IsDraft && !newConversation {
		return nil, "這個對話還是草稿：先送出第一則再設定時訊息"
	}
	var m *schedules.TimedMessage
	if id != "" {
		for _, x := range s.timed {
			if x.ID == id {
				m = x
			}
		}
		if m == nil {
			return nil, "找不到這則定時訊息"
		}
	}
	if m == nil {
		if len(s.timed) >= schedules.MaxTimedMessages {
			return nil, "定時訊息最多 200 則"
		}
		m = &schedules.TimedMessage{ID: "tm_" + newID("")[:8], CreatedAt: now, Runs: []*schedules.TimedRun{}}
		if from != nil {
			m.CreatedBy, m.CreatedByTitle, m.CreatedByProvider = from.Meta.ID, from.Meta.Title, from.Meta.Provider
		}
		s.timed = append(s.timed, m)
	}
	m.Text = text
	m.Label = strings.TrimSpace(label)
	m.Cron, m.At = cron, at
	m.Enabled, m.Done = true, false
	m.ConversationID = target.Meta.ID
	m.NewConversation = newConversation
	// 時間改了：下一次從現在之後算（不補建立以前或改之前的）
	n := now
	m.LastScheduledAt = &n
	if newConversation {
		m.AccountID, m.Model, m.Effort, m.PermissionMode = target.Meta.AccountID, target.Meta.Model, target.Meta.Effort, target.Meta.Permission
		m.Folder = ""
		if target.Meta.CustomFolder {
			m.Folder = target.Meta.WorkingDirectory
		}
	}
	h.saveTimedMessages()
	h.armTimedTimer()
	h.postSchedules(true)
	return m, ""
}

// timedAction run（立即送一次，不動下一次的時間）、pause、resume（下一次從現在之後算）、delete
func (h *Hub) timedAction(id, action string) string {
	s := h.sched
	if s == nil {
		return "這台的 Myriagent 沒有開定時訊息"
	}
	idx := -1
	for i, x := range s.timed {
		if x.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		return "找不到這則定時訊息"
	}
	m := s.timed[idx]
	now := s.clock()
	switch action {
	case "run":
		keepLast, keepDone := m.LastScheduledAt, m.Done
		h.fireTimed(m, now, false, now)
		m.LastScheduledAt, m.Done = keepLast, keepDone
	case "pause":
		m.Enabled = false
	case "resume":
		m.Enabled, m.Done = true, false
		n := now
		m.LastScheduledAt = &n
	case "delete":
		s.timed = append(s.timed[:idx:idx], s.timed[idx+1:]...)
	default:
		return "不認得的操作"
	}
	h.saveTimedMessages()
	h.armTimedTimer()
	h.postSchedules(true)
	return ""
}

// handleTimedCLI myriagent schedule 的要求（本機介面；convID 是權杖驗過的呼叫者）。
// op：add（when {kind, value}、text、label、new）、list、remove／pause／resume（id；只能動這個對話建立或送進這個對話的）
func (h *Hub) handleTimedCLI(convID string, args map[string]any) map[string]any {
	fail := func(msg string) map[string]any { return map[string]any{"ok": false, "message": msg} }
	s := h.sched
	if s == nil {
		return fail("這台的 Myriagent 沒有開定時訊息")
	}
	caller := h.convs[convID]
	op := str(args, "op")
	switch op {
	case "add":
		when, _ := args["when"].(map[string]any)
		newConv, _ := args["new"].(bool)
		m, problem := h.saveTimed("", str(args, "text"), str(args, "label"), str(when, "kind"), str(when, "value"), convID, newConv, caller)
		if m == nil {
			return fail(problem)
		}
		next := ""
		if n := schedules.NextDue(m); n != nil {
			next = n.Format("2006/1/2 15:04")
		}
		return map[string]any{"ok": true, "message": "已建立定時訊息 " + m.ID + "：" + schedules.DescribeTimed(m) + "，下一次 " + next, "item": h.renderTimedCLI(m, convID)}
	case "list":
		items := []any{}
		for _, m := range s.timed {
			items = append(items, h.renderTimedCLI(m, convID))
		}
		return map[string]any{"ok": true, "items": items}
	case "remove", "pause", "resume":
		var m *schedules.TimedMessage
		for _, x := range s.timed {
			if x.ID == str(args, "id") {
				m = x
			}
		}
		if m == nil {
			return fail("找不到這則定時訊息（用 myriagent schedule list 看 id）")
		}
		if m.ConversationID != convID && m.CreatedBy != convID {
			return fail("這則不是這個對話建立的，請使用者在排程頁管理")
		}
		action := op
		if op == "remove" {
			action = "delete"
		}
		if p := h.timedAction(m.ID, action); p != "" {
			return fail(p)
		}
		done := map[string]string{"remove": "已刪除", "pause": "已暫停", "resume": "已繼續"}[op]
		return map[string]any{"ok": true, "message": done + " " + m.ID}
	}
	return fail("不認得的操作：用 add、list、remove、pause、resume")
}

func (h *Hub) renderTimedCLI(m *schedules.TimedMessage, caller string) map[string]any {
	var next, conv any
	if n := schedules.NextDue(m); n != nil {
		next = n.Format("2006/1/2 15:04")
	}
	if c := h.convs[m.ConversationID]; c != nil {
		conv = c.Meta.Title
	}
	return map[string]any{
		"id": m.ID, "label": schedules.LabelOf(m), "when": schedules.DescribeTimed(m), "next": next, "enabled": m.Enabled && !m.Done,
		"newConversation": m.NewConversation, "mine": m.ConversationID == caller || m.CreatedBy == caller, "conversation": conv,
	}
}

// renderTimed 排程頁的 timed（ScheduleView.timed）：下一次最早的在前
func (h *Hub) renderTimed() []any {
	s := h.sched
	out := []any{}
	if s == nil {
		return out
	}
	list := append([]*schedules.TimedMessage{}, s.timed...)
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	key := func(m *schedules.TimedMessage) time.Time {
		if n := schedules.NextDue(m); n != nil {
			return *n
		}
		return far
	}
	sort.SliceStable(list, func(i, j int) bool { return key(list[i]).Before(key(list[j])) })
	now := s.clock()
	for _, m := range list {
		target := h.convs[m.ConversationID]
		var provider, folder, title any
		if m.NewConversation {
			if a := h.account(m.AccountID); a != nil {
				provider = a.Provider
			}
			folder = nilStr(m.Folder)
		} else if target != nil {
			provider = target.Meta.Provider
			folder = target.workDir(h)
		}
		if target != nil {
			title = target.Meta.Title
		}
		upcoming := []any{}
		for _, x := range schedules.Upcoming(m, now, now.Add(24*time.Hour)) {
			upcoming = append(upcoming, x.UnixMilli())
		}
		var createdBy, last any
		if m.CreatedBy != "" {
			createdBy = map[string]any{"conversationId": m.CreatedBy, "title": nilStr(m.CreatedByTitle), "provider": nilStr(m.CreatedByProvider), "exists": h.convs[m.CreatedBy] != nil}
		}
		runs := []any{}
		for i := len(m.Runs) - 1; i >= 0; i-- {
			r := m.Runs[i]
			at := r.ScheduledAt
			if r.FiredAt != nil {
				at = *r.FiredAt
			}
			if i == len(m.Runs)-1 {
				last = map[string]any{"level": schedules.LevelOfOutcome(r.Outcome), "text": schedules.OutcomeText(r), "at": at.UnixMilli()}
			}
			runs = append(runs, map[string]any{"at": at.UnixMilli(), "scheduledAt": r.ScheduledAt.UnixMilli(),
				"late": r.FiredAt != nil && r.FiredAt.Sub(r.ScheduledAt) > 2*time.Minute, "conversationId": nilStr(r.ConversationID),
				"exists": r.ConversationID != "" && h.convs[r.ConversationID] != nil, "level": schedules.LevelOfOutcome(r.Outcome), "text": schedules.OutcomeText(r)})
		}
		out = append(out, map[string]any{
			"id": m.ID, "label": schedules.LabelOf(m), "text": m.Text, "when": schedules.DescribeTimed(m), "cron": nilStr(m.Cron), "at": msOf(m.At),
			"enabled": m.Enabled, "done": m.Done, "newConversation": m.NewConversation, "conversationId": nilStr(m.ConversationID),
			"conversationTitle": title, "conversationExists": target != nil, "provider": provider, "folder": folder,
			"nextAt": msOf(schedules.NextDue(m)), "upcoming": upcoming, "createdAt": m.CreatedAt.UnixMilli(), "createdBy": createdBy,
			"last": last, "runs": runs,
		})
	}
	return out
}

// timedInstructions 給代理的說明多一段：myriagent schedule 的用法（掛上了定時訊息、本機介面開著才給）
func (h *Hub) timedInstructions() string {
	if h.sched == nil || h.localURL == "" {
		return ""
	}
	where := "launchd 的排程"
	if runtimeGOOS() == "linux" {
		where = "systemd 計時器或 crontab"
	}
	return "\n\n要在之後的某個時間或定期讓代理接著做事（例如每天 09:00 整理報表、明天 14:00 檢查部署有沒有成功），用 myriagent schedule（已在 PATH）建立 Myriagent 的定時訊息：" +
		"`myriagent schedule add --daily 09:00 -- 要做的事`（也可以 --weekdays HH:mm、--every 30m、--at \"2026-10-27 14:00\"、--cron \"0 9 * * 1-5\"；加 --new 每次開新對話，" +
		"前文才不會越堆越長）、`myriagent schedule list`、`myriagent schedule remove <id>`。時間到了 Myriagent 會把文字送進這個對話，app 沒開時錯過的重複訊息只補一次。" +
		"不要為這種事另裝" + where + "，除非使用者要的是 app 沒開也要跑的工作。"
}

func runtimeGOOS() string { return runtime.GOOS }
