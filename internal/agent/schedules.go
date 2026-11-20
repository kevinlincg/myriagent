package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kevinlincg/myriagent/internal/schedules"
)

// 排程頁（web-protocol 的 schedules／scheduleWatch／scheduleRefresh／scheduleAction／scheduleAgent；C# 的 AgentHub.Schedules.cs）：
// 這台的排程（macOS：launchd＋crontab；Linux：systemd 使用者計時器＋crontab）在背景讀（schedules.Reader），回到鎖裡比對上一次：
// 建立、修改、刪除、啟用、停用記成變更紀錄，這段期間有對話的工具指令碰到排程就記成那個對話做的、排程算「代理建立」；
// 上次執行的結果比對成 Myriagent 自己的執行紀錄（runs.jsonl），列入管理的排程新的一串失敗開始時念一句、送通知、自動請代理診斷
// （只診斷不修改；次數與額度有上限）。
// 什麼時候讀：排程頁開著的視窗（scheduleWatch）有就每 30 秒，沒有每 5 分鐘，一輪結束時這一輪的工具指令碰到排程就立刻讀，
// 使用者按「重新掃描」、快速操作之後也讀。「交給代理」開草稿、預先填好說明，不自動送出；每個排程一個管理對話，之後沿用。
// 沒掛讀取器（測試預設）時什麼都不做、init 的 schedules 是 null（網頁不顯示排程頁）。彈出視窗不送

const (
	scheduleWatchedInterval = 30 * time.Second
	scheduleIdleInterval    = 5 * time.Minute
	// scheduleTouchLifetime 工具指令碰到排程的記號留多久（一輪跑很久也夠；一輪結束後再讀一次就清掉）
	scheduleTouchLifetime = 3 * time.Hour
)

type scheduleHub struct {
	reader  schedules.Reader
	dir     string
	clock   func() time.Time
	machine string

	store      *schedules.Store
	scan       *schedules.Scan
	scanning   bool
	scanAgain  bool
	lastScanAt time.Time
	sentSig    string
	watchers   map[*View]bool
	touches    map[string]*scheduleTouch
	busy       map[string]bool
	ctx        context.Context
	cancel     context.CancelFunc

	// 定時訊息（timed.go）
	timed        []*schedules.TimedMessage
	timedTimer   *time.Timer
	timedReadyAt time.Time
	timedPending map[string]*timedPending
	toolReady    bool

	// 通知與念出來（中樞以外的；沒接上就不做）
	speak  func(text string)
	notify func(title, body string)
	// weeklyRemaining 帳戶每週額度剩下的百分比（不知道回 nil；帳戶用量接上之前一律不知道）
	weeklyRemaining func(a *Account) *float64
}

// scheduleTouch 一個對話這一輪的工具指令碰到了排程：讀到的變更記成它做的
type scheduleTouch struct {
	convID, title, provider string
	hits                    []schedules.CommandHit
	at                      time.Time
	turnEnded               bool
}

func init() {
	registerCommand("scheduleWatch", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		on, _ := msg["on"].(bool)
		h.setScheduleWatch(v, on)
	})
	registerCommand("scheduleRefresh", func(h *Hub, v *View, c *Conv, msg map[string]any) { h.requestScheduleScan() })
	registerCommand("scheduleAction", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		var on *bool
		if b, ok := msg["on"].(bool); ok {
			on = &b
		}
		h.scheduleAction(v, str(msg, "path"), str(msg, "action"), on)
	})
	registerCommand("scheduleAgent", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		h.scheduleAgent(v, str(msg, "path"), str(msg, "task"), str(msg, "accountId"), str(msg, "folder"), msg["paneId"])
	})
	registerInitField("schedules", func(h *Hub, v *View) any {
		if h.sched == nil || v.mode == "popout" {
			return nil
		}
		return h.renderSchedules()
	})
	registerToolStartHook(func(h *Hub, c *Conv, it *Item) { h.onScheduleToolStarted(c, it) })
	registerTurnEndHook(func(h *Hub, c *Conv, outcome string) {
		h.onSchedulesTurnCompleted(c)
		h.onTimedTurnCompleted(c, outcome)
	})
	// 設定頁的「排程失敗時自動診斷」：預設開；這台有讀取器才顯示（沒有是 null）
	settingKinds["scheduleAutoDiagnose"] = "bool"
	if schedules.NewReader(runtime.GOOS, nil) != nil {
		settingDefaults["scheduleAutoDiagnose"] = true
	}
	registerStartHook(func(h *Hub) {
		r := schedules.NewReader(runtime.GOOS, []string{h.root})
		if r == nil {
			return
		}
		h.mu.Lock()
		h.attachSchedules(r, filepath.Join(h.root, "schedules"), time.Now)
		h.mu.Unlock()
	})
}

// attachSchedules 掛上排程的讀取器與紀錄資料夾（只掛一次；呼叫時持有 h.mu）。之後背景每 30 秒看一次要不要讀、定時訊息有沒有到點
func (h *Hub) attachSchedules(r schedules.Reader, dir string, clock func() time.Time) {
	if h.sched != nil {
		return
	}
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	ctx, cancel := context.WithCancel(context.Background())
	h.sched = &scheduleHub{reader: r, dir: dir, clock: clock, machine: host, watchers: map[*View]bool{}, touches: map[string]*scheduleTouch{},
		busy: map[string]bool{}, ctx: ctx, cancel: cancel, timedPending: map[string]*timedPending{}}
	// Myriagent 的定時訊息跟著排程頁一起掛
	h.loadTimedMessages()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.mu.Lock()
				h.tickSchedules()
				h.tickTimed()
				h.mu.Unlock()
				h.kick()
			}
		}
	}()
}

// stopSchedules 停掉背景工作（測試收尾）
func (h *Hub) stopSchedules() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sched; s != nil {
		s.cancel()
		if s.timedTimer != nil {
			s.timedTimer.Stop()
		}
	}
}

// tickSchedules 維護：排程頁開著 30 秒、沒開 5 分鐘讀一次
func (h *Hub) tickSchedules() {
	s := h.sched
	if s == nil || s.scanning {
		return
	}
	for v := range s.watchers {
		if v.closed {
			delete(s.watchers, v)
		}
	}
	interval := scheduleIdleInterval
	if len(s.watchers) > 0 {
		interval = scheduleWatchedInterval
	}
	if s.clock().Sub(s.lastScanAt) >= interval-time.Second {
		h.requestScheduleScan()
	}
}

// setScheduleWatch 排程頁顯示中（on）或離開（off）。開啟時上次讀取超過 30 秒就馬上讀，否則重送目前的結果
func (h *Hub) setScheduleWatch(v *View, on bool) {
	s := h.sched
	if s == nil {
		return
	}
	if !on {
		delete(s.watchers, v)
		return
	}
	s.watchers[v] = true
	if s.scan == nil || s.clock().Sub(s.lastScanAt) >= scheduleWatchedInterval {
		h.requestScheduleScan()
	} else {
		h.sendTo(v, h.schedulesMsg())
	}
}

// requestScheduleScan 開始讀一次（讀取中就排一次）；第一次讀之前先在背景載入紀錄
func (h *Hub) requestScheduleScan() {
	s := h.sched
	if s == nil || s.ctx.Err() != nil {
		return
	}
	if s.scanning {
		s.scanAgain = true
		return
	}
	s.scanning, s.scanAgain = true, false
	needLoad := s.store == nil
	h.postSchedules(true)
	go func() {
		var store *schedules.Store
		if needLoad {
			store = schedules.LoadStore(s.dir)
		}
		scan := s.reader.Scan(s.ctx)
		h.mu.Lock()
		h.onSchedulesScanned(store, scan)
		h.mu.Unlock()
		h.kick()
	}()
}

func (h *Hub) onSchedulesScanned(store *schedules.Store, scan schedules.Scan) {
	s := h.sched
	s.scanning = false
	if s.ctx.Err() != nil {
		return
	}
	if s.store == nil {
		s.store = store
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("排程：處理掃描結果失敗：%v", r)
			}
		}()
		h.applyScheduleScan(scan)
	}()
	s.lastScanAt = s.clock()
	h.postSchedules(false)
	if s.scanAgain {
		h.requestScheduleScan()
	}
}

var (
	enabledElement = regexp.MustCompile(`(?i)<Enabled>\s*(true|false)\s*</Enabled>`)
	// plist 的停用（launchctl 另外記的啟用狀態改了不算修改定義；直接改 Disabled 鍵也只記成啟用、停用）
	disabledKey = regexp.MustCompile(`<key>Disabled</key>\s*<(?:true|false)\s*/>`)
)

// scheduleFingerprint 定義的指紋：拿掉啟用狀態（啟用、停用另外記）
func scheduleFingerprint(def string) string {
	if def == "" {
		return ""
	}
	n := disabledKey.ReplaceAllString(enabledElement.ReplaceAllString(def, ""), "")
	n = strings.ReplaceAll(n, "\r\n", "\n")
	sum := sha256.Sum256([]byte(n))
	return strings.ToUpper(hex.EncodeToString(sum[:]))[:16]
}

// applyScheduleScan 讀到一次結果：和上一次比出變更（第一次只建基準）、記成碰到排程的對話做的、觀察執行結果、通知新的一串失敗與自動診斷
func (h *Hub) applyScheduleScan(scan schedules.Scan) {
	s := h.sched
	store := s.store
	if store == nil {
		return
	}
	now := s.clock()
	st := store.State
	baseline := !st.Baseline
	dirty := false
	if scan.Error != "" && len(scan.Tasks) == 0 {
		s.scan = &scan
		return
	}
	seen := map[string]bool{}
	type alert struct {
		task schedules.TaskInfo
		run  schedules.RunRecord
	}
	var alerts []alert
	for _, t := range scan.Tasks {
		seen[strings.ToLower(t.Path)] = true
		if t.XML == "" {
			continue
		}
		fp := scheduleFingerprint(t.XML)
		before, had := schedules.Lookup(st.Fingerprints, t.Path)
		if !baseline {
			if !had {
				dirty = h.recordScheduleChange(t.Path, "created", now) || dirty
			} else if before != fp {
				dirty = h.recordScheduleChange(t.Path, "changed", now) || dirty
			}
			if was, ok := schedules.Lookup(st.EnabledStates, t.Path); ok && was != t.Enabled {
				kind := "disabled"
				if t.Enabled {
					kind = "enabled"
				}
				dirty = h.recordScheduleChange(t.Path, kind, now) || dirty
			}
		}
		if !had || before != fp {
			schedules.Put(st.Fingerprints, t.Path, fp)
			dirty = true
		}
		if e, ok := schedules.Lookup(st.EnabledStates, t.Path); !ok || e != t.Enabled {
			schedules.Put(st.EnabledStates, t.Path, t.Enabled)
			dirty = true
		}
		// 執行紀錄
		previous := store.LastRunOf(t.Path)
		var triggers []schedules.Trigger
		if t.Definition != nil {
			triggers = t.Definition.Triggers
		}
		if run := schedules.Observe(t.Path, previous, t.LastRun, t.LastResult, triggers, now); run != nil {
			store.AppendRun(*run)
			if schedules.ShouldAlert(*run, previous, now) && schedules.IsManaged(h.originOf(t, scan.Env)) {
				alerts = append(alerts, alert{t, *run})
			}
		}
	}
	// 不見了：刪除（讀取失敗的那次不算）
	if scan.Error == "" {
		var gone []string
		for p := range st.Fingerprints {
			if !seen[strings.ToLower(p)] {
				gone = append(gone, p)
			}
		}
		sort.Strings(gone)
		for _, p := range gone {
			if !baseline {
				h.recordScheduleChange(p, "deleted", now)
			}
			delete(st.Fingerprints, p)
			schedules.Remove(st.EnabledStates, p)
			dirty = true
		}
	}
	if baseline {
		st.Baseline = true
		dirty = true
	}
	// 碰過排程的對話：這一輪已經結束的，結束後這次讀完就清掉；太久的也清掉
	for id, t := range s.touches {
		if t.turnEnded || now.Sub(t.at) > scheduleTouchLifetime {
			delete(s.touches, id)
		}
	}
	s.scan = &scan
	if dirty {
		store.Save()
	}
	for _, a := range alerts {
		h.alertScheduleFailure(a.task, a.run, scan.Env)
	}
}

// recordScheduleChange 記一筆變更；這段期間有對話的工具指令碰到排程就記成它做的（名稱對得上的優先），建立與修改另外記成來源
func (h *Hub) recordScheduleChange(path, kind string, now time.Time) bool {
	s := h.sched
	st := s.store.State
	touch := h.findScheduleTouch(path)
	ch := schedules.ChangeEntry{Path: path, At: now, Kind: kind}
	if touch != nil {
		ch.ConversationID, ch.Title, ch.Provider = touch.convID, touch.title, touch.provider
	}
	s.store.AddChange(ch)
	if _, has := schedules.Lookup(st.Origins, path); touch != nil && (kind == "created" || kind == "changed") && !has {
		schedules.Put(st.Origins, path, schedules.OriginEntry{Source: "myriagent", Verb: kind, At: now, ConversationID: touch.convID, Title: touch.title, Provider: touch.provider})
	}
	if kind == "deleted" {
		schedules.Remove(st.Origins, path)
		schedules.Remove(st.Links, path)
	}
	return true
}

func (h *Hub) findScheduleTouch(path string) *scheduleTouch {
	s := h.sched
	if len(s.touches) == 0 {
		return nil
	}
	var named *scheduleTouch
	for _, t := range s.touches {
		for _, hit := range t.hits {
			if schedules.NameMatches(hit.Name, path) && (named == nil || t.at.After(named.at)) {
				named = t
			}
		}
	}
	if named != nil {
		return named
	}
	// 看不出名稱的（變數、crontab）：只有一個對話在碰排程時才算它的
	var unnamed []*scheduleTouch
	for _, t := range s.touches {
		for _, hit := range t.hits {
			if hit.Name == "" && schedules.Mutates(hit.Verb) {
				unnamed = append(unnamed, t)
				break
			}
		}
	}
	if len(unnamed) == 1 {
		return unnamed[0]
	}
	return nil
}

// onScheduleToolStarted 工具卡的指令出來時：碰到排程就記下這個對話
func (h *Hub) onScheduleToolStarted(c *Conv, it *Item) {
	s := h.sched
	if s == nil {
		return
	}
	text := it.Title + "\n" + it.Detail
	if !schedules.MayTouch(text) {
		return
	}
	hits := schedules.ParseCommandHits(text)
	if len(hits) == 0 {
		return
	}
	t := s.touches[c.Meta.ID]
	if t == nil || t.turnEnded {
		t = &scheduleTouch{convID: c.Meta.ID}
		s.touches[c.Meta.ID] = t
	}
	t.title, t.provider, t.at = c.Meta.Title, c.Meta.Provider, s.clock()
	for _, hit := range hits {
		dup := false
		for _, x := range t.hits {
			dup = dup || x == hit
		}
		if !dup {
			t.hits = append(t.hits, hit)
		}
	}
}

// onSchedulesTurnCompleted 一輪結束：這一輪碰過排程就立刻重讀，讀完清掉記號
func (h *Hub) onSchedulesTurnCompleted(c *Conv) {
	s := h.sched
	if s == nil {
		return
	}
	t := s.touches[c.Meta.ID]
	if t == nil {
		return
	}
	t.title = c.Meta.Title
	t.turnEnded = true
	h.requestScheduleScan()
}

// originOf 排程的分類：不再列出 → 有證據 → 納入管理 → 規則判斷
func (h *Hub) originOf(t schedules.TaskInfo, env schedules.Environment) schedules.Origin {
	if s := h.sched; s != nil && s.store != nil {
		st := s.store.State
		if schedules.ContainsFold(st.Ignored, t.Path) {
			return schedules.OriginIgnored
		}
		if _, ok := schedules.Lookup(st.Origins, t.Path); ok {
			return schedules.OriginAgent
		}
		if schedules.ContainsFold(st.Adopted, t.Path) {
			return schedules.OriginAdopted
		}
	}
	if t.Definition == nil {
		return schedules.OriginOther
	}
	return schedules.ClassifyOrigin(schedules.ClassifyInput{Path: t.Path, Author: t.Definition.Author, Actions: t.Definition.Actions, AnyPathInRepo: t.RepoRoot != "", Env: env})
}

// alertScheduleFailure 列入管理的排程新的一串失敗：念一句、通知、自動請代理診斷
func (h *Hub) alertScheduleFailure(t schedules.TaskInfo, run schedules.RunRecord, env schedules.Environment) {
	s := h.sched
	result := schedules.Classify(run.Code)
	log.Printf("排程 %s 失敗：%s（%s）", t.Path, result.Text, run.At.Format("2006-01-02 15:04:05"))
	if h.settingBool("voiceNotifications") {
		text := fmt.Sprintf("%s 的排程 %s 失敗，%s", s.machine, t.Name(), result.Text)
		if s.speak != nil {
			s.speak(text)
		} else {
			h.speak(text, nil) // 語音通知（speech.go）
		}
	}
	if h.settingBool("notifications") {
		title, body := "排程失敗："+t.Name(), run.At.Format("1/2 15:04")+" "+result.Text
		if s.notify != nil {
			s.notify(title, body)
		} else {
			// 沒有系統通知：告訴每個視窗
			h.broadcast(map[string]any{"type": "toast", "text": title + "（" + body + "）"})
		}
	}
	h.autoDiagnoseSchedule(t, run, env)
}

func (h *Hub) scheduleAutoDiagnoseOn() bool {
	b, ok := h.setting("scheduleAutoDiagnose").(bool)
	return !ok || b
}

// autoDiagnoseSchedule 自動請代理診斷（只診斷不修改）：管理對話在就送進去（執行中排進佇列），不在就開一個；條件不符時記下原因給明細顯示
func (h *Hub) autoDiagnoseSchedule(t schedules.TaskInfo, run schedules.RunRecord, env schedules.Environment) {
	s := h.sched
	st := s.store.State
	now := s.clock()
	st.Diagnoses = schedules.PruneDiagnoses(st.Diagnoses, now)
	existing := h.managementConversationOf(t.Path)
	var account *Account
	if existing != nil {
		account = h.account(existing.Meta.AccountID)
	} else {
		account = h.pickScheduleAccount("")
	}
	var weekly *float64
	if account != nil && s.weeklyRemaining != nil {
		weekly = s.weeklyRemaining(account)
	}
	block := schedules.DiagnoseBlock(t.Path, h.originOf(t, env), h.scheduleAutoDiagnoseOn(), schedules.ContainsFold(st.NoAutoDiagnose, t.Path),
		st.Diagnoses, now, account != nil && scheduleAccountUsable(account), weekly)
	if block != "" {
		schedules.Put(st.DiagnoseBlocks, t.Path, block)
		s.store.Save()
		log.Printf("排程 %s 不自動診斷：%s", t.Path, block)
		return
	}
	brief := h.briefOf(t, env)
	prompt := schedules.BuildPrompt("auto", brief)
	display := schedules.AutoDisplay(brief, run)
	c := existing
	if c == nil {
		c = h.createManagementConversation(account, t, brief.WorkFolder)
	}
	// 能規劃的代理用規劃模式（只讀取與分析、最後給計畫）；Codex 沒有，只靠說明約束
	for _, o := range permissionModes(c.Meta.Provider) {
		if o.Value == "Plan" {
			h.setPermissionQuiet(c, "Plan")
		}
	}
	h.sendHeadless(c, prompt, display)
	st.Diagnoses = append(st.Diagnoses, schedules.DiagnoseRecord{Path: t.Path, At: now, ConversationID: c.Meta.ID})
	schedules.Remove(st.DiagnoseBlocks, t.Path)
	s.store.Save()
	log.Printf("排程 %s 已自動請代理診斷（對話 %s）", t.Path, c.Meta.ID)
}

// setPermissionQuiet 換權限模式（不提示；程序在跑而這一家不能即時換就下一則訊息重開）
func (h *Hub) setPermissionQuiet(c *Conv, mode string) {
	if c.Meta.Permission == mode {
		return
	}
	c.Meta.Permission = mode
	if p := h.provider(c); p != nil && p.Alive(c) && !p.SetOption(h, c, "permission", mode) && !c.running {
		h.closeSession(c)
	}
	c.changed()
}

// sendHeadless 沒有視窗的送出（自動診斷、定時訊息）：執行中就排進佇列，否則開始一輪；對話裡顯示 display（代理收到 text）。
// 回傳是不是排進了佇列
func (h *Hub) sendHeadless(c *Conv, text, display string) bool {
	if c.running {
		c.queue = append(c.queue, QueuedMessage{ID: newID("q"), Text: text})
		c.dirtyComp = true
		return true
	}
	h.startTurn(c, text)
	if display != "" {
		for i := len(c.Items) - 1; i >= 0; i-- {
			if it := c.Items[i]; it.Kind == "User" {
				it.HTML = renderUserText(display)
				c.touch(it)
				break
			}
		}
	}
	return false
}

// createConversation 開一個正式的對話（不是草稿、存檔、出現在清單上；沒有視窗開著它）
func (h *Hub) createConversation(a *Account, title, folder string) *Conv {
	d := h.conversationDefault(a.Provider)
	model := "default"
	if d.Model != nil {
		model = *d.Model
	} else if m := h.settings.LastModel[a.ID]; m != "" {
		model = m
	}
	now := nowMs()
	c := &Conv{Meta: ConvMeta{ID: newID("c"), AccountID: a.ID, Provider: a.Provider, Title: title, Model: model, Effort: "default",
		Permission: d.Permission, CreatedAt: now, UpdatedAt: now}}
	c.init()
	if folder != "" {
		if fi, err := os.Stat(folder); err == nil && fi.IsDir() {
			c.Meta.CustomFolder, c.Meta.WorkingDirectory = true, folder
		}
	}
	h.convs[c.Meta.ID] = c
	c.save(h)
	c.changed()
	h.pushConversations()
	h.pushAccounts()
	return c
}

// createManagementConversation 開一個正式的管理對話（自動診斷用），記成這個排程的管理對話
func (h *Hub) createManagementConversation(a *Account, t schedules.TaskInfo, folder string) *Conv {
	c := h.createConversation(a, "排程 "+t.Name()+" 的管理", folder)
	schedules.Put(h.sched.store.State.Links, t.Path, c.Meta.ID)
	return c
}

// managementConversationOf 這個排程的管理對話（還在的；草稿也算：使用者剛開還沒送出）
func (h *Hub) managementConversationOf(path string) *Conv {
	s := h.sched
	if s == nil || s.store == nil {
		return nil
	}
	if id, ok := schedules.Lookup(s.store.State.Links, path); ok {
		return h.convs[id]
	}
	return nil
}

func scheduleAccountUsable(a *Account) bool { return a.status == "ready" || a.status == "apiKey" }

// pickScheduleAccount 交給代理用的帳戶：指定的、排程頁上次用的、第一個可用的 Claude、第一個可用的
func (h *Hub) pickScheduleAccount(requested string) *Account {
	var ready []*Account
	for _, a := range h.accounts {
		if scheduleAccountUsable(a) {
			ready = append(ready, a)
		}
	}
	last := ""
	if s := h.sched; s != nil && s.store != nil {
		last = s.store.State.AccountID
	}
	for _, want := range []func(a *Account) bool{
		func(a *Account) bool { return requested != "" && a.ID == requested },
		func(a *Account) bool { return last != "" && a.ID == last },
		func(a *Account) bool { return a.Provider == "Claude" },
		func(a *Account) bool { return true },
	} {
		for _, a := range ready {
			if want(a) {
				return a
			}
		}
	}
	return nil
}

func (h *Hub) briefOf(t schedules.TaskInfo, env schedules.Environment) schedules.Brief {
	st := h.sched.store.State
	var origin *schedules.OriginEntry
	if o, ok := schedules.Lookup(st.Origins, t.Path); ok {
		origin = &o
	}
	var changes []schedules.ChangeEntry
	for _, c := range st.Changes {
		if strings.EqualFold(c.Path, t.Path) {
			changes = append(changes, c)
		}
	}
	folder := ""
	if t.Definition != nil {
		folder = schedules.WorkFolder(t.Definition.Actions, env)
	}
	if folder == "" {
		folder = t.RepoRoot
	}
	return schedules.Brief{Task: t, Machine: h.sched.machine, Origin: h.originOf(t, env), OriginEntry: origin, Runs: h.sched.store.RunsOf(t.Path), Changes: changes, WorkFolder: folder}
}

func (h *Hub) findScheduleTask(path string) *schedules.TaskInfo {
	s := h.sched
	if s == nil || s.scan == nil || path == "" {
		return nil
	}
	for i := range s.scan.Tasks {
		if strings.EqualFold(s.scan.Tasks[i].Path, path) {
			return &s.scan.Tasks[i]
		}
	}
	return nil
}

// scheduleAction run、stop、enable、disable 在背景經讀取器做，做完重讀；ignore、unignore、adopt、unadopt、autoDiagnose 只改這台的選擇
func (h *Hub) scheduleAction(v *View, path, action string, on *bool) {
	s := h.sched
	if s == nil || s.store == nil || strings.TrimSpace(path) == "" || action == "" {
		return
	}
	t := h.findScheduleTask(path)
	if t == nil {
		h.toast(v, "找不到這個排程，請重新掃描")
		return
	}
	st := s.store.State
	switch action {
	case "run", "stop", "enable", "disable":
		key := strings.ToLower(t.Path)
		if s.busy[key] {
			return
		}
		s.busy[key] = true
		h.postSchedules(true)
		target, name := t.Path, t.Name()
		go func() {
			var err error
			switch action {
			case "run":
				err = s.reader.Run(target)
			case "stop":
				err = s.reader.Stop(target)
			default:
				err = s.reader.SetEnabled(target, action == "enable")
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			defer h.kick()
			delete(s.busy, key)
			if s.ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("排程：%s %s 失敗：%v", action, target, err)
				h.toast(v, "排程 "+name+"："+err.Error())
			} else {
				text := map[string]string{"run": "已開始執行 ", "stop": "已停止 ", "enable": "已啟用 ", "disable": "已停用 "}[action] + name
				h.toast(v, text)
			}
			h.requestScheduleScan()
		}()
		return
	case "ignore":
		st.Ignored = schedules.Toggle(st.Ignored, t.Path, true)
	case "unignore":
		st.Ignored = schedules.Toggle(st.Ignored, t.Path, false)
	case "adopt":
		st.Adopted = schedules.Toggle(st.Adopted, t.Path, true)
	case "unadopt":
		st.Adopted = schedules.Toggle(st.Adopted, t.Path, false)
	case "autoDiagnose":
		st.NoAutoDiagnose = schedules.Toggle(st.NoAutoDiagnose, t.Path, on != nil && !*on)
	case "importOrigin":
		// 出處是外部的工作階段：匯入成 Myriagent 對話（排程回溯與匯入外部對話還沒移植）
		h.toast(v, "這個排程沒有可以匯入的外部工作階段")
		return
	default:
		return
	}
	s.store.Save()
	h.postSchedules(true)
}

// scheduleAgent 交給代理。有 path 的以那個排程的管理對話（沒有就用選的帳戶開草稿，資料夾是排程的工作資料夾）、預先填好說明；
// 不自動送出。task 是 diagnose、explain、retime、retire、custom，或 create（新增排程：沒有 path，在選的資料夾開草稿）
func (h *Hub) scheduleAgent(v *View, path, task, accountID, folder string, paneID any) {
	s := h.sched
	if s == nil || s.store == nil || task == "" {
		return
	}
	if task == "create" {
		a := h.pickScheduleAccount(accountID)
		if a == nil {
			h.toast(v, "沒有可用的帳戶，請先登入一個帳戶")
			return
		}
		s.store.State.AccountID = a.ID
		s.store.Save()
		c := h.createDraft(v, a, folder)
		h.sendConversationWithDraft(v, c, paneID, schedules.CreatePrompt(s.machine))
		return
	}
	if !schedules.IsTask(task) || task == "auto" {
		return
	}
	t := h.findScheduleTask(path)
	if t == nil {
		h.toast(v, "找不到這個排程，請重新掃描")
		return
	}
	brief := h.briefOf(*t, s.scan.Env)
	prompt := schedules.BuildPrompt(task, brief)
	if existing := h.managementConversationOf(t.Path); existing != nil {
		h.sendConversationWithDraft(v, existing, paneID, prompt)
		return
	}
	a := h.pickScheduleAccount(accountID)
	if a == nil {
		h.toast(v, "沒有可用的帳戶，請先登入一個帳戶")
		return
	}
	s.store.State.AccountID = a.ID
	c := h.createDraft(v, a, brief.WorkFolder)
	c.Meta.Title = "排程 " + t.Name() + " 的管理"
	schedules.Put(s.store.State.Links, t.Path, c.Meta.ID)
	s.store.Save()
	h.sendConversationWithDraft(v, c, paneID, prompt)
	h.postSchedules(false)
}

// createDraft 開一個草稿（同 newConversation，資料夾另外指定；存在才用）
func (h *Hub) createDraft(v *View, a *Account, folder string) *Conv {
	d := h.conversationDefault(a.Provider)
	model := "default"
	if d.Model != nil {
		model = *d.Model
	} else if m := h.settings.LastModel[a.ID]; m != "" {
		model = m
	}
	c := &Conv{Meta: ConvMeta{ID: newID("c"), AccountID: a.ID, Provider: a.Provider, Title: "新對話", Model: model, Effort: "default",
		Permission: d.Permission, CreatedAt: nowMs(), UpdatedAt: nowMs(), IsDraft: true}}
	c.init()
	if strings.TrimSpace(folder) != "" {
		if fi, err := os.Stat(folder); err == nil && fi.IsDir() {
			c.Meta.CustomFolder, c.Meta.WorkingDirectory = true, folder
		}
	}
	h.convs[c.Meta.ID] = c
	return c
}

// sendConversationWithDraft 在這個視窗開對話、輸入框預先填好說明（輸入框是空的才填）
func (h *Hub) sendConversationWithDraft(v *View, c *Conv, paneID any, draft string) {
	v.open[c.Meta.ID] = true
	m := h.conversationMsg(c, paneID)
	m["draft"] = draft
	h.sendTo(v, m)
}

// schedulesMsg 整份的 schedules 訊息
func (h *Hub) schedulesMsg() map[string]any {
	return map[string]any{"type": "schedules", "schedules": h.renderSchedules()}
}

// postSchedules 送給這台的完整視窗（彈出視窗不送）；force＝內容沒變也送（狀態改了：讀取中、快速操作中）
func (h *Hub) postSchedules(force bool) {
	s := h.sched
	if s == nil {
		return
	}
	msg := h.schedulesMsg()
	var targets []*View
	for v := range h.views {
		if v.ready && v.mode != "popout" {
			targets = append(targets, v)
		}
	}
	if len(targets) == 0 {
		return
	}
	b, _ := json.Marshal(msg)
	sig := string(b)
	if !force && sig == s.sentSig {
		return
	}
	s.sentSig = sig
	for _, v := range targets {
		h.sendTo(v, msg)
	}
}

func msOf(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func nilStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// renderSchedules init 與 schedules 訊息的 ScheduleView；沒掛讀取器是 nil
func (h *Hub) renderSchedules() map[string]any {
	s := h.sched
	if s == nil {
		return nil
	}
	now := s.clock()
	entries := []any{}
	failing := 0
	if s.scan != nil && s.store != nil {
		for _, t := range s.scan.Tasks {
			e := h.renderScheduleEntry(t, s.scan.Env, now)
			entries = append(entries, e)
			if last, ok := e["last"].(map[string]any); ok && e["managed"] == true && last["level"] == "fail" {
				failing++
			}
		}
	}
	var scannedAt, errText any
	if s.scan != nil {
		scannedAt = s.scan.ScannedAt.UnixMilli()
		errText = nilStr(s.scan.Error)
	}
	var accountID any
	if s.store != nil {
		accountID = nilStr(s.store.State.AccountID)
	}
	return map[string]any{
		"machine": s.machine, "loaded": s.scan != nil, "scanning": s.scanning, "scannedAt": scannedAt, "error": errText,
		"entries": entries, "failing": failing, "autoDiagnose": h.scheduleAutoDiagnoseOn(), "accountId": accountID,
		"timed": h.renderTimed(),
		// 其他電腦最後回報的（多電腦同步，workstream 6）；還沒接上是空的
		"devices": []any{},
	}
}

func (h *Hub) renderScheduleEntry(t schedules.TaskInfo, env schedules.Environment, now time.Time) map[string]any {
	s := h.sched
	st := s.store.State
	d := t.Definition
	origin := h.originOf(t, env)
	runs := s.store.RunsOf(t.Path)
	// 還沒有自己的紀錄（剛裝好、還沒執行過）：用讀取器的上次結果
	var last any
	if n := len(runs); n > 0 {
		r := schedules.Classify(runs[n-1].Code)
		last = map[string]any{"code": r.Code, "level": r.Level.Name(), "text": r.Text}
	} else if t.LastRun != nil && schedules.IsFinal(t.LastResult) {
		r := schedules.Classify(t.LastResult)
		last = map[string]any{"code": r.Code, "level": r.Level.Name(), "text": r.Text}
	}
	state := "error"
	if d != nil {
		switch t.State {
		case schedules.StateRunning:
			state = "running"
		case schedules.StateDisabled:
			state = "disabled"
		case schedules.StateQueued:
			state = "queued"
		case schedules.StateReady:
			state = "ready"
		default:
			state = "disabled"
			if t.Enabled {
				state = "ready"
			}
		}
	}
	workFolder := ""
	var trigger, action, runAs, author, description, created any
	conditions := []string{}
	upcoming := []any{}
	if d != nil {
		workFolder = schedules.WorkFolder(d.Actions, env)
		trigger, action, runAs = schedules.DescribeAll(d.Triggers), schedules.ActionText(d.Actions), schedules.RunAsText(*d)
		conditions = schedules.Conditions(*d)
		author, description, created = nilStr(d.Author), nilStr(d.Description), msOf(d.Created)
		if t.Enabled {
			for _, x := range schedules.Occurrences(d.Triggers, now, now.Add(24*time.Hour), schedules.MaxOccurrences) {
				upcoming = append(upcoming, x.UnixMilli())
			}
		}
	}
	if workFolder == "" {
		workFolder = t.RepoRoot
	}
	var nextRun any
	if t.Enabled {
		nextRun = msOf(t.NextRun)
	}
	runList := []any{}
	for i, n := len(runs)-1, 0; i >= 0 && n < 30; i, n = i-1, n+1 {
		r := runs[i]
		c := schedules.Classify(r.Code)
		runList = append(runList, map[string]any{"at": r.At.UnixMilli(), "code": r.Code, "level": c.Level.Name(), "text": c.Text, "gap": r.Gap})
	}
	var provenance any
	if o, ok := schedules.Lookup(st.Origins, t.Path); ok {
		provenance = map[string]any{"source": o.Source, "verb": o.Verb, "at": o.At.UnixMilli(), "conversationId": nilStr(o.ConversationID),
			"title": nilStr(o.Title), "provider": nilStr(o.Provider), "exists": o.ConversationID != "" && h.convs[o.ConversationID] != nil,
			"importable": false, "cwd": nilStr(o.Cwd)}
	}
	var management any
	if m := h.managementConversationOf(t.Path); m != nil {
		management = map[string]any{"conversationId": m.Meta.ID, "title": m.Meta.Title, "provider": m.Meta.Provider, "running": m.running, "isDraft": m.Meta.IsDraft}
	}
	changes := []any{}
	for i, n := len(st.Changes)-1, 0; i >= 0 && n < 10; i-- {
		c := st.Changes[i]
		if !strings.EqualFold(c.Path, t.Path) {
			continue
		}
		n++
		changes = append(changes, map[string]any{"at": c.At.UnixMilli(), "kind": c.Kind, "text": schedules.ChangeText(c.Kind), "conversationId": nilStr(c.ConversationID),
			"title": nilStr(c.Title), "provider": nilStr(c.Provider), "exists": c.ConversationID != "" && h.convs[c.ConversationID] != nil})
	}
	var lastDiagnosis any
	var latest *schedules.DiagnoseRecord
	for i := range st.Diagnoses {
		x := st.Diagnoses[i]
		if strings.EqualFold(x.Path, t.Path) && (latest == nil || x.At.After(latest.At)) {
			latest = &x
		}
	}
	if latest != nil {
		lastDiagnosis = map[string]any{"at": latest.At.UnixMilli(), "conversationId": nilStr(latest.ConversationID), "exists": latest.ConversationID != "" && h.convs[latest.ConversationID] != nil}
	}
	var block any
	if b, ok := schedules.Lookup(st.DiagnoseBlocks, t.Path); ok {
		block = b
	}
	return map[string]any{
		"path": t.Path, "name": t.Name(), "folder": t.Folder(), "state": state, "enabled": t.Enabled, "busy": s.busy[strings.ToLower(t.Path)],
		"origin": origin.Name(), "managed": schedules.IsManaged(origin), "trigger": trigger, "action": action, "workFolder": nilStr(workFolder),
		"repoRoot": nilStr(t.RepoRoot), "runAs": runAs, "conditions": conditions, "author": author, "description": description, "created": created,
		"lastRun": msOf(t.LastRun), "last": last, "nextRun": nextRun, "missed": t.MissedRuns, "streak": schedules.ConsecutiveFailures(runs),
		"runs": runList, "upcoming": upcoming, "provenance": provenance, "management": management, "changes": changes,
		"autoDiagnose": !schedules.ContainsFold(st.NoAutoDiagnose, t.Path), "diagnoseBlock": block, "lastDiagnosis": lastDiagnosis,
		"error": nilStr(t.Error), "xml": nilStr(t.XML),
	}
}
