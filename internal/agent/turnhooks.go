package agent

// 各功能掛在對話流程上的回呼（同 C# 的 HookTurnCompleted、OnScheduleToolStarted 那一串）：新功能放自己的檔案，在 init 裡登記，
// 不必改共用的流程。全部在持有 h.mu 時呼叫；要等 I/O 的事放到 goroutine

// 一輪結束的結果
const (
	turnCompleted   = "completed"
	turnFailed      = "failed"
	turnInterrupted = "interrupted"
)

var (
	turnStartHooks   []func(h *Hub, c *Conv)
	turnEndHooks     []func(h *Hub, c *Conv, outcome string)
	toolStartHooks   []func(h *Hub, c *Conv, it *Item)
	instructionHooks []func(h *Hub, c *Conv) string
)

// registerTurnEndHook 一輪結束時（endTurn，接著送佇列之前）；outcome 是 completed、failed、interrupted
func registerTurnEndHook(f func(h *Hub, c *Conv, outcome string)) {
	turnEndHooks = append(turnEndHooks, f)
}

// registerTurnHook 一輪開始（startTurn，送出之前）、結束時（同 registerTurnEndHook，不看結果）；不要的傳 nil
func registerTurnHook(start, end func(h *Hub, c *Conv)) {
	if start != nil {
		turnStartHooks = append(turnStartHooks, start)
	}
	if end != nil {
		registerTurnEndHook(func(h *Hub, c *Conv, _ string) { end(h, c) })
	}
}

// registerToolStartHook 工具卡的指令內容出來時（標題、細節已填好）
func registerToolStartHook(f func(h *Hub, c *Conv, it *Item)) {
	toolStartHooks = append(toolStartHooks, f)
}

// registerInstructions 附加給代理的說明多一段（回空字串＝不加）
func registerInstructions(f func(h *Hub, c *Conv) string) {
	instructionHooks = append(instructionHooks, f)
}

func (h *Hub) runTurnEndHooks(c *Conv, outcome string) {
	for _, f := range turnEndHooks {
		f(h, c, outcome)
	}
}

func (h *Hub) runToolStartHooks(c *Conv, it *Item) {
	for _, f := range toolStartHooks {
		f(h, c, it)
	}
}

func (h *Hub) extraInstructions(c *Conv) string {
	s := ""
	for _, f := range instructionHooks {
		s += f(h, c)
	}
	return s
}

// turnOutcome 這一輪怎麼結束的：使用者按了停止是 interrupted；這一輪（最後一則使用者訊息之後）有錯誤項目是 failed
func turnOutcome(c *Conv) string {
	if c.stopping {
		return turnInterrupted
	}
	for i := len(c.Items) - 1; i >= 0; i-- {
		switch c.Items[i].Kind {
		case "User":
			return turnCompleted
		case "Error":
			return turnFailed
		}
	}
	return turnCompleted
}
