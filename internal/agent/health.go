package agent

import (
	"fmt"
	"sync"
	"time"
)

// 儀表板的健康度（照 C# 的 Hub/AgentHub.Health.cs；web-protocol 的 health／healthCheck／healthFix）：每個本機帳戶每次對話都會載入的東西
// （全域指示、自動記憶索引、skill 清單、MCP、外掛、設定檔）有沒有太肥，以及電腦層級的 CLAUDE.md／AGENTS.md／GEMINI.md 要不要合併與同步。
// 網頁切到儀表板時送 healthCheck，30 秒內分析過、帳戶也沒變就不重做；「重新分析」帶 force。掃描在 goroutine（只讀），結果回來才拿鎖存與送。
// 「用代理合併」「用代理精簡」「用代理整理」以選的帳戶開草稿，輸入框預先填好說明與流程（conversation 訊息的 draft；不自動送出）。
// 只送給主視窗（彈出視窗不送）。

// healthMinInterval：不帶 force 的 healthCheck 在這段時間內分析過（帳戶也沒變）就不重做
const healthMinInterval = 30 * time.Second

// healthState：一個中樞的健康度狀態（都在 h.mu 底下改）
type healthState struct {
	result       *healthScanResult
	accounts     []accountHealth
	instructions *instructionsReport
	running      bool
	again        bool
	finishedAt   time.Time
	signature    string
	// done：每做完一次分析通知（測試用；可為 nil）
	done func()
}

var healthStates sync.Map // *Hub → *healthState

func (h *Hub) health() *healthState {
	if v, ok := healthStates.Load(h); ok {
		return v.(*healthState)
	}
	v, _ := healthStates.LoadOrStore(h, &healthState{})
	return v.(*healthState)
}

func init() {
	registerInitField("health", func(h *Hub, v *View) any {
		if v.mode != "main" {
			return nil
		}
		return h.renderHealth()
	})
	registerCommand("healthCheck", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		force, _ := msg["force"].(bool)
		h.requestHealth(v, force)
	})
	registerCommand("healthFix", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		h.healthFix(v, str(msg, "task"), str(msg, "accountId"), str(msg, "targetAccountId"), msg["paneId"])
	})
}

// healthInputs：要分析的本機帳戶（持有 h.mu 時取快照：資料夾在這裡解析好）
func (h *Hub) healthInputs() []healthAccountInput {
	out := make([]healthAccountInput, 0, len(h.accounts))
	for _, a := range h.accounts {
		in := healthAccountInput{ID: a.ID, Label: a.Label, Provider: a.Provider, IsSystemDefault: a.IsSystemDefault,
			ConfigDir: a.resolveConfigDir(), SafeMode: a.SafeMode}
		if a.Provider == "Claude" {
			legacy, config := a.claudeGlobalConfigPaths()
			in.ClaudeGlobalConfig = config
			if fileExists(legacy) {
				in.ClaudeGlobalConfig = legacy
			}
		}
		// 共用系統預設帳戶的設定（隔離帳戶、支援、沒關掉、不在精簡模式）：記憶與指示在來源那邊
		if !a.IsSystemDefault && !a.ExistingFolder && supportsSharedCustomizations(a.Provider) && a.shares() && !a.SafeMode {
			in.SharedSource = (&Account{Provider: a.Provider, IsSystemDefault: true}).resolveConfigDir()
		}
		out = append(out, in)
	}
	return out
}

// healthSignature：帳戶的組成（id、資料夾、精簡模式、共用、名稱、跨 CLI 共用開關）：變了就不受 30 秒的限制
func (h *Hub) healthSignature(inputs []healthAccountInput) string {
	s := fmt.Sprint(h.settingBool("shareCrossCliCustomizations"))
	for _, i := range inputs {
		s += fmt.Sprintf("|%s;%s;%v;%s;%s", i.ID, i.ConfigDir, i.SafeMode, i.SharedSource, i.Label)
	}
	return s
}

// requestHealth：healthCheck。分析中就排一次（force 才排）；不帶 force 時剛分析過、帳戶沒變就只對送指令的視窗重送目前的結果。
// 呼叫時持有 h.mu；v 可為 nil（補做的那一次）
func (h *Hub) requestHealth(v *View, force bool) {
	st := h.health()
	if st.running {
		st.again = st.again || force
		return
	}
	inputs := h.healthInputs()
	sig := h.healthSignature(inputs)
	if !force && st.result != nil && sig == st.signature && time.Since(st.finishedAt) < healthMinInterval {
		if v != nil {
			h.sendTo(v, map[string]any{"type": "health", "health": h.renderHealth()})
		}
		return
	}
	st.running, st.again = true, false
	h.postHealth()
	home := userHome()
	go func() {
		res := healthScanner{home: home}.scan(inputs)
		h.mu.Lock()
		defer h.mu.Unlock()
		defer h.kick()
		st.running = false
		st.result = res
		st.accounts = make([]accountHealth, 0, len(res.Accounts))
		for _, s := range res.Accounts {
			st.accounts = append(st.accounts, evaluateHealth(s))
		}
		r := compareInstructions(res.Docs)
		st.instructions = &r
		st.signature = sig
		st.finishedAt = time.Now()
		h.postHealth()
		if st.done != nil {
			st.done()
		}
		if st.again {
			h.requestHealth(nil, true)
		}
	}()
}

// postHealth：送給主視窗（彈出視窗不送）
func (h *Hub) postHealth() {
	msg := map[string]any{"type": "health", "health": h.renderHealth()}
	for v := range h.views {
		if v.ready && v.mode == "main" {
			h.sendTo(v, msg)
		}
	}
}

// renderHealth：HealthView
func (h *Hub) renderHealth() map[string]any {
	st := h.health()
	accounts := map[string]any{}
	for _, a := range st.accounts {
		accounts[a.AccountID] = renderAccountHealth(a)
	}
	var checked, instr any
	if st.result != nil {
		checked = st.result.CheckedAt.UnixMilli()
	}
	if st.instructions != nil {
		instr = renderInstructions(*st.instructions)
	}
	return map[string]any{"running": st.running, "checkedAt": checked, "accounts": accounts, "instructions": instr}
}

// healthFix：以 accountId 的帳戶開草稿、輸入框預先填好說明（merge：合併電腦層級的指示檔；trim：精簡 targetAccountId 的全域指示；
// memory：整理 targetAccountId 超過或接近上限的自動記憶索引）。工作資料夾是使用者資料夾（各 CLI 的設定資料夾都在底下）。呼叫時持有 h.mu
func (h *Hub) healthFix(v *View, task, accountID, targetID string, paneID any) {
	a := h.account(accountID)
	if a == nil {
		h.sendTo(v, map[string]any{"type": "toast", "text": "找不到要用的帳戶", "level": "error"})
		return
	}
	st := h.health()
	now := time.Now()
	fail := func(text string) { h.sendTo(v, map[string]any{"type": "toast", "text": text, "level": "error"}) }
	var scan *accountScan
	var health *accountHealth
	if st.result != nil {
		for i, s := range st.result.Accounts {
			if s.Account.ID == targetID {
				scan = s
				if i < len(st.accounts) {
					health = &st.accounts[i]
				}
			}
		}
	}
	var prompt string
	switch task {
	case "merge":
		if st.instructions == nil || !st.instructions.mergeable() {
			fail("目前沒有需要合併的指示檔，請先重新分析")
			return
		}
		prompt = buildMergePrompt(*st.instructions, now)
	case "trim":
		if scan == nil || health == nil || len(scan.Instructions) == 0 {
			fail("這個帳戶沒有可以精簡的全域指示，請先重新分析")
			return
		}
		prompt = buildTrimPrompt(scan, *health, now)
	case "memory":
		if scan != nil {
			prompt = buildMemoryPrompt(scan, now)
		}
		if prompt == "" {
			fail("這個帳戶的記憶索引沒有超過或接近上限，請先重新分析")
			return
		}
	default:
		return
	}
	d := h.conversationDefault(a.Provider)
	model := "default"
	if d.Model != nil {
		model = *d.Model
	} else if m := h.settings.LastModel[a.ID]; m != "" {
		model = m
	}
	c := &Conv{Meta: ConvMeta{
		ID: newID("c"), AccountID: a.ID, Provider: a.Provider, Title: "新對話", Model: model, Effort: "default",
		Permission: d.Permission, CreatedAt: nowMs(), UpdatedAt: nowMs(), IsDraft: true,
		CustomFolder: true, WorkingDirectory: userHome(),
	}}
	c.init()
	h.convs[c.Meta.ID] = c
	v.open[c.Meta.ID] = true
	msg := h.conversationMsg(c, paneID)
	msg["draft"] = prompt
	h.sendTo(v, msg)
}
