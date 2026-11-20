package agent

// 對話的事件（給各功能接在共用流程上，不必改共用的 switch）：開了分頁、切到看得到、換了工作資料夾、這一輪結束。
// 以 registerConvEvent 登記；呼叫時持有 h.mu，處理要很快（要等 I/O 的放 goroutine）。
// 對應 C# 的 OnXxxConversationPosted／OnXxxFolderChanged／OnXxxTurnCompleted 與 visible 時的 RequestGit(Open)。

type convEventKind int

const (
	// convOpened：整份對話剛送給這個 view（open、新草稿）；v 是那個 view
	convOpened convEventKind = iota
	// convVisible：這個 view 回報看得到這個對話（切到這個分頁、視窗回到前景）
	convVisible
	// convFolderChanged：換了工作資料夾（setFolder、resetFolder）；v 是 nil
	convFolderChanged
	// convTurnEnded：這一輪結束（代理可能改了檔案、提交了、建立了專案）；v 是 nil
	convTurnEnded
)

var convEventHooks []func(h *Hub, kind convEventKind, v *View, c *Conv)

func registerConvEvent(f func(h *Hub, kind convEventKind, v *View, c *Conv)) {
	convEventHooks = append(convEventHooks, f)
}

func (h *Hub) fireConvEvent(kind convEventKind, v *View, c *Conv) {
	if c == nil {
		return
	}
	for _, f := range convEventHooks {
		f(h, kind, v, c)
	}
}

// shutdownHooks：中樞關閉時各功能要做的收尾（持有 h.mu 時呼叫）
var shutdownHooks []func(h *Hub)

func registerShutdownHook(f func(h *Hub)) { shutdownHooks = append(shutdownHooks, f) }
