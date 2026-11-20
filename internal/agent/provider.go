package agent

import (
	"fmt"
	"github.com/kevinlincg/myriagent/internal/render"
	"time"
)

// Provider 是一種代理 CLI 的接法（Claude、Codex、Antigravity、Grok 各一個）。
// 中樞的共用流程（送出、佇列、停止、卡片回應、換選項）只透過這個介面碰 CLI；各家的程序與串流解讀放在各自的檔案。
// 所有方法都在持有 h.mu 時呼叫；要等 I/O 的事放到 goroutine，回來時自己拿鎖。
type Provider interface {
	// Send 送出一則使用者訊息、開始一輪（必要時先啟動程序）。使用者項目已經由中樞加好；失敗回錯誤（中樞會加錯誤提示、結束這一輪）
	Send(h *Hub, c *Conv, text string, images [][2]string) error
	// Stop 停止這一輪（程序留著）
	Stop(h *Hub, c *Conv)
	// Approve／Answer／ApprovePlan 回應等使用者的卡片（c.pending 裡的請求已由中樞取出，卡片狀態由各家更新）
	Approve(h *Hub, c *Conv, it *Item, p *pendingRequest, choice, message string)
	Answer(h *Hub, c *Conv, it *Item, p *pendingRequest, answers map[string][]string, skip bool)
	ApprovePlan(h *Hub, c *Conv, it *Item, p *pendingRequest, choice, message string)
	// SetOption 換模型、推理強度或權限模式之後呼叫（Meta 已經改好）；回 false＝這一家要下一則訊息重開程序才生效
	SetOption(h *Hub, c *Conv, key, value string) bool
	// Alive 這個對話的程序還在
	Alive(c *Conv) bool
	// Close 結束這個對話的程序（不等它結束）
	Close(c *Conv)
	// RefreshAccount 讀帳戶的登入狀態（同步、要快；要跑 CLI 的放 goroutine）
	RefreshAccount(h *Hub, a *Account)
	// Steer 執行中送出的訊息能直接插進這一輪
	Steer() bool
}

var providers = map[string]Provider{}

func registerProvider(name string, p Provider) { providers[name] = p }

func (h *Hub) provider(c *Conv) Provider { return providers[c.Meta.Provider] }

// startTurn 送出一則訊息、開始一輪（呼叫時持有 h.mu）
func (h *Hub) startTurn(c *Conv, text string) {
	p := h.provider(c)
	if p == nil {
		c.errorItem(fmt.Sprintf("這一版還不支援 %s 的對話", c.Meta.Provider))
		c.changed()
		return
	}
	user := c.addItem("User")
	user.Text = text
	user.HTML = renderUserText(text)
	c.running, c.stopping = true, false
	c.startedAt = nowMs()
	c.status = "思考中…"
	c.Meta.UpdatedAt = nowMs()
	c.changed()
	if err := p.Send(h, c, text, nil); err != nil {
		c.running = false
		c.status = ""
		c.errorItem("沒辦法開始這一輪：" + err.Error())
	}
	h.kick()
}

// endTurn 是各家在一輪結束時呼叫的共用收尾：清掉串流狀態、標未讀、接著送佇列
func (h *Hub) endTurn(c *Conv) {
	h.onTurnEndedAttention(c) // 閃對話、語音通知（speech.go；要在清掉 stopping 之前）
	c.running, c.stopping = false, false
	c.status = ""
	c.Meta.UpdatedAt = nowMs()
	if !h.viewing(c.Meta.ID) {
		c.unread = true
		if len(c.queue) == 0 {
			h.notify(c, "代理已回覆")
		}
	}
	c.changed()
	h.fireConvEvent(convTurnEnded, nil, c)
	if len(c.queue) > 0 {
		next := c.queue[0]
		c.queue = c.queue[1:]
		c.dirtyComp = true
		h.startTurn(c, next.Text)
	}
}

// closeSession 結束程序（下一則訊息以新的設定重開）
func (h *Hub) closeSession(c *Conv) {
	if p := h.provider(c); p != nil {
		p.Close(c)
	}
}

func (h *Hub) alive(c *Conv) bool {
	p := h.provider(c)
	return p != nil && p.Alive(c)
}

var _ = time.Second

// respondCard：approve／approvePlan／answer／skipQuestion 共用的前半段（找卡片、取出等待中的請求）
func (h *Hub) respondCard(c *Conv, cmd string, msg map[string]any) {
	itemID := str(msg, "itemId")
	pr := c.pending[itemID]
	it := c.item(itemID)
	p := h.provider(c)
	if pr == nil || it == nil || p == nil {
		return
	}
	delete(c.pending, itemID)
	switch cmd {
	case "approve":
		p.Approve(h, c, it, pr, str(msg, "choice"), str(msg, "message"))
	case "approvePlan":
		p.ApprovePlan(h, c, it, pr, str(msg, "choice"), str(msg, "message"))
	case "answer":
		p.Answer(h, c, it, pr, toAnswers(msg["answers"]), false)
	case "skipQuestion":
		p.Answer(h, c, it, pr, nil, true)
	}
	if c.running {
		c.status = "思考中…"
	}
	c.touch(it)
	c.changed()
}

func renderUserText(s string) string { return render.Text(s) }

// notify 發系統通知（設定 notifications 關掉時不發）：只送給一個 view，多個視窗不會重複跳
func (h *Hub) notify(c *Conv, body string) {
	if !h.settingBool("notifications") {
		return
	}
	for v := range h.views {
		if v.ready && v.mode == "main" {
			h.sendTo(v, map[string]any{"type": "nativeNotify", "title": c.Meta.Title, "body": body, "conversationId": c.Meta.ID})
			return
		}
	}
}
