package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kevinlincg/myriagent/internal/codex"
	"github.com/kevinlincg/myriagent/internal/render"
)

// 這個檔把 Codex app-server（codex app-server，JSON-RPC）的通知與請求轉成對話的項目：一輪的開始與結束、文字與推理串流、
// 工具卡（指令、改檔、MCP、網路搜尋、子代理）、核准、問卷、計畫。
// 一個對話一個 app-server 程序：第一則訊息 thread/start、程序重開後 thread/resume 續接；模型、推理強度、權限、沙箱、工作資料夾
// 都隨每輪 turn/start 帶上，切換設定不必重開程序（帳戶、執行檔改了才重開）。事件的意義照 C# 版（src/Myriagent/Agents/CodexAgentSession.cs、
// CodexAppServerRules.cs 的 CodexEventParser、CodexSubagentParser.cs）與中樞的處理（Hub/AgentHub.Events.cs）。

const (
	codexRequestTimeout  = 60 * time.Second
	codexInterruptWatch  = 15 * time.Second
	codexInvalidatedCard = "CLI 已重新啟動，這張卡已失效；代理會視需要重新提出"
)

// codexProc 是一個對話的 Codex 程序與這一輪的狀態（放在 Conv.proc；全部在 h.mu 底下讀寫）
type codexProc struct {
	cl        *codex.Client
	signature string // 帳戶＋設定資料夾＋執行檔：不同就換程序
	readyCh   chan struct{}
	initErr   error

	threadID  string
	turnID    string
	mainModel string

	turnSeq            int  // 每次送出加一：準備中的 goroutine 據此知道這一輪還是不是它的
	preparing          bool // turn/start 還沒有回應
	prepCancel         context.CancelFunc
	interruptRequested bool
	interruptSent      bool
	pausingForQuestion bool
	pausedQuestion     *Item
	deniedThisTurn     bool
	feedback           []deniedOperation

	// 等使用者回應的請求：請求鍵 → 請求、卡片；requestKeys 是「完全相同」比對鍵
	pending     map[string]*codex.Request
	reqItems    map[string]*Item
	requestKeys map[string]string
	// 本對話放行的：整類（只有檔案修改）、完全相同的請求
	sessionAllowed    map[string]bool
	sessionAllowedReq map[string]bool

	blocks      map[string]*Item // 這一輪的串流項目：項目 id → 回覆、推理或計畫卡
	tools       map[string]*Item // 工具卡（一輪結束後還在跑的背景指令也留著）
	fileChanges map[string]map[string]any
	planItem    *Item
	children    map[string]*codexChild

	curTurn      string // 主對話進行中的這一輪（turn/started 起）
	turnError    string
	retryNoticed bool
	prevTotal    map[string][2]int64 // 各 thread 上一則 tokenUsage 的 total（輸入、輸出）
	turnTokens   [2]int64
	lastCall     [2]int64
}

func codexOf(c *Conv) *codexProc {
	p, _ := c.proc.(*codexProc)
	return p
}

// codexChild：子代理（multi_agent）的 thread 畫成一張「子代理」工具卡
type codexChild struct {
	card      *Item
	label     string
	completed bool
	streamed  map[string]bool
}

// codexProvider：Codex CLI（codex app-server）
type codexProvider struct{}

func init() { registerProvider("Codex", codexProvider{}) }

// Steer：Codex 能以 turn/steer 把訊息插進進行中的一輪（中樞的插入流程接上後使用）
func (codexProvider) Steer() bool { return true }

func (codexProvider) Alive(c *Conv) bool {
	p := codexOf(c)
	return p != nil && p.cl.Alive()
}

func (codexProvider) Close(c *Conv) {
	if p := codexOf(c); p != nil {
		c.proc = nil
		// 等待中的卡片跟著程序作廢：新程序的請求 id 可能和舊的重複，不能把舊卡的回覆送給新程序
		for key, it := range p.reqItems {
			if it.Status == "Waiting" {
				it.Status = "Cancelled"
				it.Output = codexInvalidatedCard
				c.touch(it)
			}
			delete(c.pending, it.ID)
			delete(p.reqItems, key)
		}
		if p.prepCancel != nil {
			p.prepCancel()
		}
		go p.cl.Close(5 * time.Second)
	}
}

func (codexProvider) RefreshAccount(h *Hub, a *Account) { refreshCodexAccount(h, a) }

// codexSnapshot：這一輪送出當下的設定（準備的 goroutine 不持有鎖，先抄一份）
type codexSnapshot struct {
	convID, cwd, convDir, model, effort, instructions, sessionID string
	started                                                      bool
	policy                                                       codexPolicy
}

func (codexProvider) Send(h *Hub, c *Conv, text string, images [][2]string) error {
	a := h.account(c.Meta.AccountID)
	if a == nil {
		return fmt.Errorf("這個對話的帳戶已經移除，請先換一個帳戶")
	}
	cli := h.clis["Codex"]
	if cli == nil || cli.path == "" {
		return fmt.Errorf("這台電腦沒有安裝 Codex CLI（codex）")
	}
	wd := c.workDir(h)
	if !c.Meta.CustomFolder {
		if err := os.MkdirAll(wd, 0o755); err != nil {
			return err
		}
	} else if fi, err := os.Stat(wd); err != nil || !fi.IsDir() {
		// 使用者選的資料夾不存在就明確回報，不偷偷重建
		return fmt.Errorf("工作資料夾不存在：%s", wd)
	}
	sig := a.ID + "|" + a.ConfigDir + "|" + cli.path
	p := codexOf(c)
	if p != nil && (!p.cl.Alive() || p.signature != sig) {
		codexProvider{}.Close(c)
		p = nil
	}
	if p == nil {
		var err error
		if p, err = h.startCodex(c, a, cli.path, wd, sig); err != nil {
			return err
		}
	}
	p.turnSeq++
	p.preparing = true
	p.interruptRequested, p.interruptSent = false, false
	p.pausingForQuestion, p.pausedQuestion = false, nil
	p.deniedThisTurn = false
	p.turnID = ""
	ctx, cancel := context.WithCancel(context.Background())
	p.prepCancel = cancel
	snap := codexSnapshot{
		convID: c.Meta.ID, cwd: wd, convDir: h.convDir(c.Meta.ID), model: codexOptionValue(c.Meta.Model), effort: codexOptionValue(c.Meta.Effort),
		instructions: h.instructions(c), sessionID: c.Meta.SessionID, started: c.Meta.SessionStarted, policy: codexPolicyFor(c.Meta.Permission),
	}
	if snap.model != "" {
		p.mainModel = snap.model
	}
	go h.codexRunTurn(ctx, p, p.turnSeq, snap, text)
	return nil
}

// startCodex 啟動 app-server（呼叫時持有 h.mu）；initialize 在背景做，送出的 goroutine 等 readyCh
func (h *Hub) startCodex(c *Conv, a *Account, exe, wd, sig string) (*codexProc, error) {
	p := &codexProc{
		signature: sig, readyCh: make(chan struct{}),
		pending: map[string]*codex.Request{}, reqItems: map[string]*Item{}, requestKeys: map[string]string{},
		sessionAllowed: map[string]bool{}, sessionAllowedReq: map[string]bool{},
		blocks: map[string]*Item{}, tools: map[string]*Item{}, fileChanges: map[string]map[string]any{},
		children: map[string]*codexChild{}, prevTotal: map[string][2]int64{},
	}
	convID, accountID := c.Meta.ID, a.ID
	cl, err := codex.Start(codex.Options{
		Executable: exe, Args: codex.ServerArgs(), WorkDir: wd, CodexHome: a.ConfigDir,
		Env: []string{"MYRIAGENT_AGENT_CONVERSATION=" + convID},
	}, codex.Handlers{
		Notify:  func(method string, params map[string]any) { h.onCodexNotify(convID, p, method, params) },
		Request: func(req *codex.Request) { h.onCodexRequest(convID, p, req) },
		Exit:    func(err error, stderr string) { h.onCodexExit(convID, p, err, stderr) },
	})
	if err != nil {
		return nil, err
	}
	p.cl = cl
	c.proc = p
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), codexRequestTimeout)
		_, err := cl.Initialize(ctx, Version)
		cancel()
		if err != nil {
			err = fmt.Errorf("%w%s", err, codexStderrSuffix(cl))
		}
		// 在鎖底下記：程序結束的處理（onCodexExit）要讀它，說明為什麼沒開始
		h.mu.Lock()
		p.initErr = err
		h.mu.Unlock()
		close(p.readyCh)
		if err != nil {
			cl.Kill()
			return
		}
		// 模型清單（不擋住送出）：記在帳戶上，畫面的模型選單跟著換
		res, err := cl.CallTimeout("model/list", map[string]any{}, codexRequestTimeout)
		if err != nil {
			return
		}
		models := codexModels(res)
		if len(models) == 0 {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if acc := h.account(accountID); acc != nil {
			acc.models = models
			h.markAccountHeaders(acc)
			h.pushAccounts()
		}
		h.kick()
	}()
	return p, nil
}

func codexStderrSuffix(cl *codex.Client) string {
	if t := cl.StderrTail(400); t != "" {
		return "\n\n```\n" + t + "\n```"
	}
	return ""
}

func (h *Hub) markAccountHeaders(a *Account) {
	for _, other := range h.convs {
		if other.Meta.AccountID == a.ID {
			other.dirtyHeader = true
		}
	}
}

// current：這個 goroutine 的這一輪還是不是目前的（呼叫時持有 h.mu）
func (h *Hub) codexCurrent(convID string, p *codexProc, seq int) *Conv {
	c := h.convs[convID]
	if c == nil || c.proc != p || p.turnSeq != seq || !c.running {
		return nil
	}
	return c
}

// codexRunTurn：準備這一輪（等 initialize、建立或續接 thread）再送 turn/start；不持有鎖，改狀態時自己拿
func (h *Hub) codexRunTurn(ctx context.Context, p *codexProc, seq int, snap codexSnapshot, text string) {
	fail := func(err error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		defer h.kick()
		c := h.codexCurrent(snap.convID, p, seq)
		if c == nil {
			return
		}
		// 程序已經結束（或初始化失敗、正在結束）：交給 onCodexExit 收尾，它有結束碼與錯誤輸出；
		// 這裡先結束這一輪的話，佇列的下一則會送進還沒標成結束的舊程序
		if errors.Is(err, codex.ErrExited) || !p.cl.Alive() || p.initErr != nil {
			return
		}
		p.preparing = false
		if p.interruptRequested {
			// 按了停止之後，準備中的請求被取消（或剛好失敗）：回報已停止，不是「送出失敗」
			h.codexFinishTurn(c, p, "interrupted", "")
			return
		}
		c.errorItem("沒辦法開始這一輪：" + err.Error())
		h.codexFinishTurn(c, p, "aborted", "")
	}
	select {
	case <-p.readyCh:
	case <-ctx.Done():
		fail(ctx.Err())
		return
	}
	if p.initErr != nil {
		fail(p.initErr)
		return
	}

	h.mu.Lock()
	c := h.codexCurrent(snap.convID, p, seq)
	if c == nil || p.interruptRequested {
		h.mu.Unlock()
		fail(context.Canceled)
		return
	}
	thread := p.threadID
	h.mu.Unlock()

	if thread == "" {
		var err error
		if thread, err = h.codexOpenThread(ctx, p, seq, snap); err != nil {
			fail(err)
			return
		}
	}

	params := codexTurnStartParams(thread, text, nil, snap.policy, []string{snap.convDir}, snap.model, snap.effort)
	params["cwd"] = snap.cwd
	h.mu.Lock()
	if c := h.codexCurrent(snap.convID, p, seq); c == nil || p.interruptRequested {
		h.mu.Unlock()
		fail(context.Canceled)
		return
	}
	h.mu.Unlock()
	// turn/start 送出後不再取消（codex 可能已經開始這一輪）：之後按停止改送 turn/interrupt
	res, err := p.cl.CallTimeout("turn/start", params, codexRequestTimeout)
	if err != nil {
		fail(err)
		return
	}
	turn, _ := res["turn"].(map[string]any)
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	c = h.codexCurrent(snap.convID, p, seq)
	if c == nil {
		return
	}
	p.preparing = false
	if p.turnID == "" {
		p.turnID = str(turn, "id")
	}
	// 準備期間就按了停止（或要等非同步提問的回答）：現在補送 turn/interrupt
	if p.interruptRequested && !p.interruptSent && p.turnID != "" {
		h.codexSendInterrupt(c, p)
	}
}

// codexOpenThread：建立或續接 thread；續接不了就改開新的（並告訴使用者）
func (h *Hub) codexOpenThread(ctx context.Context, p *codexProc, seq int, snap codexSnapshot) (string, error) {
	call := func(method string, params map[string]any) (map[string]any, error) {
		cctx, cancel := context.WithTimeout(ctx, codexRequestTimeout)
		defer cancel()
		return p.cl.Call(cctx, method, params)
	}
	if snap.started && snap.sessionID != "" {
		res, err := call("thread/resume", codexThreadResumeParams(snap.sessionID, snap.cwd, snap.model, snap.policy))
		if err == nil {
			h.codexRememberThread(snap.convID, p, seq, snap.sessionID, res)
			return snap.sessionID, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var reason string
		switch classifyResumeError(err.Error()) {
		case resumeActiveWriter:
			// 同一個 CODEX_HOME 的其他程式正開著這個對話：另開新對話等於悄悄分岔，請使用者先關掉那邊
			return "", errors.New("這個 Codex 對話正在其他程式（例如 VS Code 的 Codex 擴充或 Codex 桌面版）開啟中，請先在那邊關閉這個對話再送出")
		case resumeNotFound:
			reason = "找不到原本的 Codex 對話紀錄（" + err.Error() + "）"
		default:
			reason = "無法續接原本的 Codex 對話（" + err.Error() + "）"
		}
		h.mu.Lock()
		if c := h.codexCurrent(snap.convID, p, seq); c != nil {
			c.notice(h, "", reason+"，已改開新的對話；先前的內容 Codex 不會記得。")
		}
		h.kick()
		h.mu.Unlock()
	}
	res, err := call("thread/start", codexThreadStartParams(snap.cwd, snap.model, snap.policy, snap.instructions))
	th, _ := res["thread"].(map[string]any)
	id := str(th, "id")
	if err != nil || id == "" {
		if err == nil {
			err = errors.New("沒有回傳對話 id")
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("Codex 無法建立對話：%w", err)
	}
	h.codexRememberThread(snap.convID, p, seq, id, res)
	return id, nil
}

// codexRememberThread：thread 開好了：記下主對話的 thread（存進對話的 SessionID，app 重開後以 thread/resume 續接）
func (h *Hub) codexRememberThread(convID string, p *codexProc, seq int, thread string, res map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.convs[convID]
	if c == nil || c.proc != p {
		return
	}
	p.threadID = thread
	if m := str(res, "model"); m != "" && p.mainModel == "" {
		p.mainModel = m
	}
	if c.Meta.SessionID != thread || !c.Meta.SessionStarted {
		c.Meta.SessionID, c.Meta.SessionStarted = thread, true
		c.needSave = true
	}
}

// ---- 停止 ----

func (codexProvider) Stop(h *Hub, c *Conv) {
	p := codexOf(c)
	if p == nil {
		return
	}
	h.codexInterrupt(c, p)
}

// codexInterrupt：停下這一輪（按停止，或為了等非同步提問的回答）。待回應的請求一律婉拒；
// 還沒送出 turn/start（準備中）就取消準備，不必等程序啟動或建立對話做完
func (h *Hub) codexInterrupt(c *Conv, p *codexProc) {
	if !c.running {
		return
	}
	p.interruptRequested = true
	h.codexDeclinePending(c, p, "")
	if p.turnID != "" && p.threadID != "" && !p.interruptSent {
		h.codexSendInterrupt(c, p)
	} else if p.turnID == "" && p.prepCancel != nil && p.preparing {
		p.prepCancel()
	}
}

// codexSendInterrupt：送 turn/interrupt；15 秒還沒結束就結束程序（呼叫時持有 h.mu）
func (h *Hub) codexSendInterrupt(c *Conv, p *codexProc) {
	p.interruptSent = true
	thread, turn, convID := p.threadID, p.turnID, c.Meta.ID
	cl := p.cl
	go func() {
		_, _ = cl.CallTimeout("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, codexRequestTimeout)
	}()
	time.AfterFunc(codexInterruptWatch, func() {
		h.mu.Lock()
		cc := h.convs[convID]
		stuck := cc != nil && cc.proc == p && cc.running && p.interruptRequested && p.turnID == turn
		h.mu.Unlock()
		if stuck {
			log.Printf("Codex 停止逾時，結束程序（對話 %s）", convID)
			cl.Kill()
		}
	})
}

// codexDeclinePending：收掉等待中的請求（問卷回空答案、核准拒絕），卡片改成取消；非同步提問不受影響（呼叫時持有 h.mu）
func (h *Hub) codexDeclinePending(c *Conv, p *codexProc, reason string) {
	for key, req := range p.pending {
		_ = p.cl.Respond(req.ID, codexDeclineResult(req.Method, req.Params))
		delete(p.pending, key)
		delete(p.requestKeys, key)
		if it := p.reqItems[key]; it != nil {
			delete(p.reqItems, key)
			delete(c.pending, it.ID)
			if it.Status == "Waiting" {
				it.Status = "Cancelled"
				if reason != "" {
					it.Output = reason
				}
				c.touch(it)
			}
		}
	}
}

// ---- 程序結束 ----

func (h *Hub) onCodexExit(convID string, p *codexProc, err error, stderr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	c := h.convs[convID]
	if c == nil || c.proc != p {
		return
	}
	c.proc = nil
	// 直接結束程序時 Codex 不會撤回待核准的請求：卡片作廢（非同步提問照舊等回答，答案當下一則訊息送出）
	for key, it := range p.reqItems {
		if it.Status == "Waiting" {
			it.Status = "Cancelled"
			it.Output = codexInvalidatedCard
			c.touch(it)
		}
		delete(c.pending, it.ID)
		delete(p.reqItems, key)
	}
	p.pending = map[string]*codex.Request{}
	for _, it := range p.tools {
		if it.Status == "Running" {
			it.Status = "Cancelled"
			if it.Output == "" {
				it.Output = "Codex 程序已結束，這個工具被中斷，結果未知"
			}
			c.touch(it)
		}
	}
	h.codexCancelChildren(c, p)
	if !c.running {
		c.changed()
		return
	}
	if p.interruptRequested {
		h.codexFinishTurn(c, p, "interrupted", "")
		return
	}
	code := "不明"
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		code = strconv.Itoa(ee.ExitCode())
	} else if err == nil {
		code = "0"
	}
	msg := "Codex 程序意外結束（結束碼 " + code + "）"
	if p.initErr != nil {
		msg = "沒辦法開始這一輪：" + p.initErr.Error()
	}
	if t := strings.TrimSpace(lastLines(stderr, 8)); t != "" {
		msg += "：\n\n```\n" + t + "\n```"
	}
	h.codexFinishTurn(c, p, "failed", msg)
}

// ---- 通知 ----

func (h *Hub) onCodexNotify(convID string, p *codexProc, method string, params map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	c := h.convs[convID]
	if c == nil || c.proc != p {
		return
	}
	// 主對話以外的 thread（multi_agent 的子代理）：這一輪的狀態只看主對話，子代理的過程畫在它的「子代理」卡片上
	if th := str(params, "threadId"); p.threadID != "" && th != "" && th != p.threadID {
		h.codexChildEvent(c, p, method, params)
		return
	}
	switch method {
	case "turn/started":
		turn, _ := params["turn"].(map[string]any)
		h.codexTurnStarted(c, p, str(turn, "id"))
	case "item/started":
		item, _ := params["item"].(map[string]any)
		h.codexItemStarted(c, p, item)
	case "item/completed":
		item, _ := params["item"].(map[string]any)
		h.codexItemCompleted(c, p, item)
	case "item/agentMessage/delta":
		if id, d := str(params, "itemId"), str(params, "delta"); id != "" && d != "" {
			it := h.codexBlock(c, p, id, "Assistant")
			it.Text += d
			c.touch(it)
			h.codexStatus(c, "回覆中…")
		}
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		if id, d := str(params, "itemId"), str(params, "delta"); id != "" && d != "" {
			it := h.codexBlock(c, p, id, "Thinking")
			it.Text += d
			c.touch(it)
			h.codexStatus(c, "思考中…")
		}
	case "item/reasoning/summaryPartAdded":
		// 推理摘要的新段落（文字片段本身不含分隔）：第一段之後每段前空一行，Markdown 才會分段
		if idx, _ := params["summaryIndex"].(float64); idx > 0 {
			if id := str(params, "itemId"); id != "" {
				it := h.codexBlock(c, p, id, "Thinking")
				it.Text += "\n\n"
				c.touch(it)
			}
		}
	case "item/plan/delta":
		if id, d := str(params, "itemId"), str(params, "delta"); id != "" && d != "" {
			h.codexPlanCard(c, p, id, d, false)
		}
	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		if it := p.tools[str(params, "itemId")]; it != nil {
			it.Output = truncateTail(it.Output+str(params, "delta"), 20000)
			c.touch(it)
		}
	case "turn/plan/updated":
		h.codexTurnPlan(c, p, params)
	case "thread/tokenUsage/updated":
		h.codexTokenUsage(p, params)
	case "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		errMsg := ""
		if e, ok := turn["error"].(map[string]any); ok {
			errMsg = str(e, "message")
		}
		status := str(turn, "status")
		if p.turnError != "" {
			if status != "failed" {
				c.errorItem(p.turnError) // 不是以失敗結束：錯誤不能就此消失
			} else if strings.TrimSpace(errMsg) == "" {
				errMsg = p.turnError
			}
		}
		h.codexFinishTurn(c, p, status, errMsg)
	case "error":
		e, _ := params["error"].(map[string]any)
		msg := orDefault(str(e, "message"), "Codex 發生錯誤")
		if retry, _ := params["willRetry"].(bool); !retry {
			// 最後的錯誤：codex 接著送 turn/completed（failed，error 是同一段文字），由這一輪的結束回報
			p.turnError = msg
			if usageLimitError(e) {
				p.turnError = msg + "（這個帳戶的 Codex 額度已用完）"
			}
		} else if !p.retryNoticed {
			// 串流斷線重試：每重試一次就有一則（實測一輪 9 則），同一輪只提示第一次
			p.retryNoticed = true
			c.notice(h, "", "Codex 連線不穩，正在重試："+msg)
		}
	case "serverRequest/resolved":
		h.codexRequestResolved(c, p, params["requestId"])
	case "thread/compacted":
		c.notice(h, "", "對話已很長，Codex 自動壓縮了較早的內容")
	}
}

func (h *Hub) codexStatus(c *Conv, s string) {
	if c.running && c.status != s {
		c.status = s
		c.dirtyHeader = true
	}
}

// codexBlock：這一輪的文字或推理項目（第一次收到片段時建立）
func (h *Hub) codexBlock(c *Conv, p *codexProc, id, kind string) *Item {
	if it := p.blocks[id]; it != nil {
		return it
	}
	it := c.addItem(kind)
	it.Status = "Running"
	p.blocks[id] = it
	return it
}

// codexFinishThinking：開始用工具或發問時，還在串流的推理收尾
func (h *Hub) codexFinishThinking(c *Conv, p *codexProc) {
	for id, it := range p.blocks {
		if it.Kind == "Thinking" && it.Status == "Running" {
			it.Status = "Done"
			c.touch(it)
			delete(p.blocks, id)
		}
	}
}

func (h *Hub) codexTurnStarted(c *Conv, p *codexProc, turnID string) {
	p.turnID, p.curTurn = turnID, turnID
	p.turnError, p.retryNoticed = "", false
	p.turnTokens = [2]int64{}
	if !c.running {
		// 閒置時 codex 自己開始了一輪（持續目標的下一輪、續接後立刻開始）：標成執行中，不然回覆一直出來、對話卻停不下來
		c.running, c.stopping = true, false
		c.startedAt = nowMs()
		c.status = "思考中…"
		p.turnSeq++
		p.preparing = false
		p.interruptRequested, p.interruptSent = false, false
		c.changed()
	}
	if p.interruptRequested && !p.interruptSent && p.threadID != "" {
		h.codexSendInterrupt(c, p)
	}
}

func (h *Hub) codexItemStarted(c *Conv, p *codexProc, item map[string]any) {
	id := str(item, "id")
	if item == nil || id == "" {
		return
	}
	var name, kind, title, detail, file string
	var diff []DiffLine
	var stats *DiffStats
	switch str(item, "type") {
	case "commandExecution":
		cmd := codexDescribeCommand(item)
		name, kind, title, detail = "執行指令", "bash", firstLine(cmd), cmd
	case "fileChange":
		p.fileChanges[id] = item
		name, kind = "修改檔案", "edit"
		title, file, diff, stats = codexFileChanges(item)
	case "mcpToolCall":
		name, kind, title, detail = "MCP："+str(item, "server"), "mcp", str(item, "tool"), truncate(indentJSON(item["arguments"]), 4000)
	case "dynamicToolCall":
		name, kind, title, detail = "工具", "other", str(item, "tool"), truncate(indentJSON(item["arguments"]), 4000)
	case "webSearch":
		name, kind, title = "網路搜尋", "web", str(item, "query")
	case "imageGeneration":
		name, kind, title, detail = "生成圖片", "image", "生成中…", str(item, "revisedPrompt")
	case "imageView":
		name, kind, title, file = "檢視圖片", "image", str(item, "path"), str(item, "path")
	case "collabAgentToolCall":
		name, kind, title, detail = "子代理", "agent", orDefault(str(item, "tool"), "子代理"), truncate(str(item, "prompt"), 4000)
	case "subAgentActivity":
		h.codexSubagentActivity(c, p, item)
		return
	default:
		return
	}
	h.codexFinishThinking(c, p)
	it := c.addItem("Tool")
	it.Status = "Running"
	it.ToolUseID = id
	it.ToolName, it.ToolKind, it.Title, it.Detail, it.FilePath, it.Diff, it.DiffStats = name, kind, title, detail, file, diff, stats
	if detail == "null" {
		it.Detail = ""
	}
	p.tools[id] = it
	h.codexStatus(c, name+"："+truncate(firstLine(title), 80))
}

func (h *Hub) codexItemCompleted(c *Conv, p *codexProc, item map[string]any) {
	id := str(item, "id")
	if item == nil || id == "" {
		return
	}
	typ := str(item, "type")
	switch typ {
	case "agentMessage":
		text := str(item, "text")
		it := p.blocks[id]
		if it == nil && strings.TrimSpace(text) != "" {
			it = c.addItem("Assistant")
		}
		if it != nil {
			if text != "" {
				it.Text = text
			}
			it.Status = "Done"
			c.touch(it)
			delete(p.blocks, id)
		}
		if qs := codexAsyncQuestions(item["questions"], id); len(qs) > 0 {
			h.codexAsyncQuestion(c, p, id, qs)
		}
		return
	case "reasoning":
		it := p.blocks[id]
		if it == nil {
			// 沒有串流的推理（只在完成時給摘要）
			var parts []string
			for _, s := range anyList(item["summary"]) {
				if t, _ := s.(string); strings.TrimSpace(t) != "" {
					parts = append(parts, t)
				}
			}
			if len(parts) == 0 {
				return
			}
			it = c.addItem("Thinking")
			it.Text = strings.Join(parts, "\n\n")
		}
		it.Status = "Done"
		c.touch(it)
		delete(p.blocks, id)
		return
	case "plan":
		h.codexPlanCard(c, p, id, str(item, "text"), true)
		return
	case "contextCompaction":
		c.notice(h, "", "對話已很長，Codex 自動壓縮了較早的內容")
		return
	case "subAgentActivity":
		h.codexSubagentActivity(c, p, item)
		return
	}
	it := p.tools[id]
	if it == nil {
		return
	}
	status, output := "Done", ""
	switch typ {
	case "commandExecution":
		exit, hasExit := item["exitCode"].(float64)
		switch {
		case str(item, "status") == "declined":
			status = "Declined"
		case str(item, "status") == "failed", hasExit && exit != 0:
			status = "Failed"
		}
		output = str(item, "aggregatedOutput")
	case "fileChange":
		p.fileChanges[id] = item
		status = codexItemStatus(str(item, "status"))
		if title, file, diff, stats := codexFileChanges(item); diff != nil {
			it.Title, it.FilePath, it.Diff, it.DiffStats = title, file, diff, stats
		}
	case "mcpToolCall":
		if e, ok := item["error"].(map[string]any); ok && str(e, "message") != "" {
			status, output = "Failed", str(e, "message")
		} else {
			status, output = codexItemStatus(str(item, "status")), codexMcpResult(item["result"])
		}
	case "dynamicToolCall":
		if ok, has := item["success"].(bool); has && !ok {
			status = "Failed"
		}
	case "imageGeneration":
		if _, failed := item["failure"].(map[string]any); failed {
			status, output = "Failed", "生圖額度已用完"
		} else {
			output = str(item, "revisedPrompt")
		}
	}
	it.Status = status
	if output != "" {
		it.Output = truncateTail(output, 20000)
	}
	c.touch(it)
	delete(p.tools, id)
	h.codexStatus(c, "思考中…")
}

func codexItemStatus(s string) string {
	switch s {
	case "failed":
		return "Failed"
	case "declined":
		return "Declined"
	}
	return "Done"
}

func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

// codexPlanCard：「先規劃」模式的計畫內容（item/plan/delta 串流、item/completed plan 給全文）：toolKind plan 的工具卡
func (h *Hub) codexPlanCard(c *Conv, p *codexProc, id, text string, completed bool) {
	key := "codex-plan:" + id
	it := p.blocks[key]
	if it == nil {
		it = c.addItem("Tool")
		it.ToolKind, it.ToolName, it.Title = "plan", "規劃內容", "Codex 的執行計畫"
		p.blocks[key] = it
	}
	if completed {
		it.Text = text
		it.Status = "Done"
		delete(p.blocks, key)
	} else {
		it.Text += text
		it.Status = "Running"
	}
	it.DetailHTML = render.Markdown(it.Text)
	c.touch(it)
	c.dirtyHeader = true
}

// codexTurnPlan：turn/plan/updated（update_plan 工具）→ 這一輪的待辦清單
func (h *Hub) codexTurnPlan(c *Conv, p *codexProc, params map[string]any) {
	var steps []PlanStep
	all := true
	for _, x := range anyList(params["plan"]) {
		s, _ := x.(map[string]any)
		text := str(s, "step")
		if text == "" {
			continue
		}
		st := orDefault(str(s, "status"), "pending")
		if st != "completed" {
			all = false
		}
		steps = append(steps, PlanStep{Text: text, Status: st})
	}
	if p.planItem == nil {
		p.planItem = c.addItem("Plan")
	}
	it := p.planItem
	it.Text = str(params, "explanation")
	it.HTML = render.Markdown(it.Text)
	it.Plan = steps
	it.Status = "Running"
	if len(steps) > 0 && all {
		it.Status = "Done"
	}
	c.touch(it)
}

// codexTokenUsage：thread/tokenUsage/updated → 這一輪的用量（total 的差；續接時一接上先送的那一則屬於上一輪，只當基準）
func (h *Hub) codexTokenUsage(p *codexProc, params map[string]any) {
	tu, _ := params["tokenUsage"].(map[string]any)
	total, hasTotal := codexBreakdown(tu["total"])
	last, hasLast := codexBreakdown(tu["last"])
	if hasLast {
		p.lastCall = last
	}
	if !hasTotal {
		return
	}
	thread := str(params, "threadId")
	prev, had := p.prevTotal[thread]
	p.prevTotal[thread] = total
	var d [2]int64
	switch {
	case !had:
		if turn := str(params, "turnId"); p.curTurn != "" && (turn == "" || turn == p.curTurn) {
			d = total
		}
	case total[0] < prev[0] || total[1] < prev[1]:
		// 壓縮後累計歸零：這次呼叫的用量用 last
		d = last
	default:
		d = [2]int64{total[0] - prev[0], total[1] - prev[1]}
	}
	p.turnTokens[0] += d[0]
	p.turnTokens[1] += d[1]
}

// codexBreakdown：一份用量的輸入（含快取）與輸出（含推理）
func codexBreakdown(v any) ([2]int64, bool) {
	m, _ := v.(map[string]any)
	if m == nil || (m["inputTokens"] == nil && m["outputTokens"] == nil) {
		return [2]int64{}, false
	}
	return [2]int64{num(m["inputTokens"]), num(m["outputTokens"])}, true
}

// codexFinishTurn：主對話這一輪結束（turn/completed、程序結束、準備失敗）。status：completed | interrupted | failed | aborted
func (h *Hub) codexFinishTurn(c *Conv, p *codexProc, status, errMsg string) {
	stopped := c.stopping
	endedByDeny := status == "interrupted" && p.deniedThisTurn && !stopped
	paused := status == "interrupted" && p.pausingForQuestion && !stopped
	if p.interruptRequested && status == "failed" && errMsg == "" {
		status = "interrupted"
	}
	// 這一輪的用量：記過的增量；沒記到（或比最後一次呼叫還少）時用最後一次呼叫的
	use := p.turnTokens
	if use[0]+use[1] < p.lastCall[0]+p.lastCall[1] {
		use = p.lastCall
	}
	if use[0]+use[1] > 0 {
		c.Meta.LastTurn = fmt.Sprintf("輸入 %s・輸出 %s tokens", commas(use[0]), commas(use[1]))
	}
	for id, it := range p.blocks {
		if it.Status == "Running" {
			it.Status = "Done"
			c.touch(it)
		}
		delete(p.blocks, id)
	}
	if p.planItem != nil && p.planItem.Status == "Running" {
		p.planItem.Status = "Done"
		c.touch(p.planItem)
	}
	p.planItem = nil
	for id, it := range p.tools {
		if it.Status != "Running" {
			delete(p.tools, id)
			continue
		}
		// 正常結束時還在跑的指令＝背景指令（unified_exec 的長時間指令會在之後才送 item/completed）：卡片留在執行中
		if status == "completed" && it.ToolKind == "bash" {
			continue
		}
		if status == "completed" {
			it.Status = "Done"
		} else {
			it.Status = "Cancelled"
		}
		c.touch(it)
		delete(p.tools, id)
	}
	// 子代理的 turn/completed 不會再來（停止、失敗）：卡片不能一直轉
	if status != "completed" {
		h.codexCancelChildren(c, p)
	}
	// codex 結束主對話這一輪時已撤銷主對話的請求；還留著的是子代理的：卡片隨這一輪結束失效，回婉拒讓子代理不會一直等
	h.codexDeclinePending(c, p, "")

	switch {
	case status == "failed":
		c.errorItem(orDefault(strings.TrimSpace(errMsg), "代理執行失敗"))
	case paused:
		// 提問卡寫著「回答會當作下一則訊息送出」，畫面上就是在等使用者回答
	case endedByDeny:
		if len(p.feedback) == 0 {
			c.notice(h, "", "你拒絕了這個操作，這一輪已結束")
		}
	case status == "interrupted":
		c.notice(h, "", "已停止這一輪")
	}
	// 拒絕時附的說明：Codex 沒有欄位可放，這一輪結束後當下一則訊息送出（排在佇列最前面）；使用者按停止就不送
	if msg := denyFeedbackMessage(p.feedback); msg != "" && !stopped {
		c.queue = append([]QueuedMessage{{ID: newID("q"), Text: msg}}, c.queue...)
		c.dirtyComp = true
	}
	p.feedback = nil
	p.turnID, p.curTurn = "", ""
	p.preparing = false
	p.interruptRequested, p.interruptSent = false, false
	p.pausingForQuestion = false
	p.deniedThisTurn = false
	p.turnError, p.retryNoticed = "", false
	p.turnTokens, p.lastCall = [2]int64{}, [2]int64{}
	if p.prepCancel != nil {
		p.prepCancel()
		p.prepCancel = nil
	}
	h.endTurn(c)
}

// ---- 子代理 ----

func (h *Hub) codexChild(c *Conv, p *codexProc, thread, label, model, effort string, restart bool) *codexChild {
	ch := p.children[thread]
	if ch == nil {
		ch = &codexChild{label: orDefault(label, "子代理"), streamed: map[string]bool{}}
		p.children[thread] = ch
	}
	if ch.card == nil || (restart && ch.completed) {
		if label != "" {
			ch.label = label
		}
		ch.completed = false
		ch.streamed = map[string]bool{}
		title := ch.label
		if model != "" {
			title += " · " + model
		}
		if effort != "" {
			title += " · " + effort
		}
		it := c.addItem("Tool")
		it.Status = "Running"
		it.ToolName, it.ToolKind, it.Title, it.Detail = "子代理", "agent", title, "展開查看任務、執行過程與結果"
		ch.card = it
	}
	return ch
}

func (h *Hub) codexChildOutput(c *Conv, ch *codexChild, text string) {
	if ch.completed || text == "" {
		return
	}
	// 長任務只保留最近的過程；完整紀錄仍由 Codex 保存
	ch.card.Output = truncateTail(ch.card.Output+text, 80000)
	c.touch(ch.card)
}

func (h *Hub) codexFinishChild(c *Conv, ch *codexChild, status string) {
	if ch.completed {
		return
	}
	ch.completed = true
	ch.card.Status = status
	c.touch(ch.card)
}

// codexCancelChildren：還沒結束的子代理卡片收成取消
func (h *Hub) codexCancelChildren(c *Conv, p *codexProc) {
	for _, ch := range p.children {
		if ch.card != nil && !ch.completed {
			h.codexFinishChild(c, ch, "Cancelled")
		}
	}
}

// codexSubagentActivity：主對話的 subAgentActivity 項目（started／interacted／completed／interrupted）
func (h *Hub) codexSubagentActivity(c *Conv, p *codexProc, item map[string]any) {
	thread := str(item, "agentThreadId")
	if thread == "" {
		return
	}
	kind := str(item, "kind")
	ch := h.codexChild(c, p, thread, str(item, "agentPath"), str(item, "model"), str(item, "reasoningEffort"), kind == "started" || kind == "interacted")
	switch kind {
	case "interrupted":
		h.codexFinishChild(c, ch, "Cancelled")
	case "completed":
		h.codexFinishChild(c, ch, "Done")
	}
}

// codexChildEvent：子代理 thread 的通知（app-server 會替連線自動訂閱子代理的 thread）
func (h *Hub) codexChildEvent(c *Conv, p *codexProc, method string, params map[string]any) {
	thread := str(params, "threadId")
	item, _ := params["item"].(map[string]any)
	switch method {
	case "serverRequest/resolved":
		h.codexRequestResolved(c, p, params["requestId"])
		return
	case "item/started", "item/completed":
		// 子代理的改檔核准卡要顯示差異
		if str(item, "type") == "fileChange" && str(item, "id") != "" {
			p.fileChanges[str(item, "id")] = item
		}
		if method == "item/started" && str(item, "type") == "subAgentActivity" {
			h.codexSubagentActivity(c, p, item)
		}
	case "turn/started", "turn/completed", "item/agentMessage/delta", "item/commandExecution/outputDelta", "error":
	default:
		return
	}
	ch := h.codexChild(c, p, thread, "", "", "", method == "turn/started")
	switch method {
	case "item/agentMessage/delta", "item/commandExecution/outputDelta":
		if id := str(params, "itemId"); id != "" {
			ch.streamed[id] = true
		}
		h.codexChildOutput(c, ch, str(params, "delta"))
	case "item/started":
		switch str(item, "type") {
		case "commandExecution":
			h.codexChildOutput(c, ch, "\n執行："+codexDescribeCommand(item)+"\n")
		case "fileChange":
			title, _, _, _ := codexFileChanges(item)
			h.codexChildOutput(c, ch, "\n修改："+title+"\n")
		case "mcpToolCall":
			h.codexChildOutput(c, ch, "\nMCP："+str(item, "server")+"／"+str(item, "tool")+"\n")
		case "webSearch":
			h.codexChildOutput(c, ch, "\n搜尋："+str(item, "query")+"\n")
		}
	case "item/completed":
		id := str(item, "id")
		switch str(item, "type") {
		case "agentMessage":
			if !ch.streamed[id] {
				h.codexChildOutput(c, ch, str(item, "text")+"\n")
			}
		case "userMessage":
			for _, x := range anyList(item["content"]) {
				cm, _ := x.(map[string]any)
				if str(cm, "type") == "text" {
					h.codexChildOutput(c, ch, "\n任務："+str(cm, "text")+"\n")
				}
			}
		case "commandExecution":
			if out := str(item, "aggregatedOutput"); out != "" && !ch.streamed[id] {
				h.codexChildOutput(c, ch, "\n"+out+"\n")
			}
		}
	case "error":
		e, _ := params["error"].(map[string]any)
		h.codexChildOutput(c, ch, "\n錯誤："+str(e, "message")+"\n")
	case "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		if e, ok := turn["error"].(map[string]any); ok && str(e, "message") != "" {
			h.codexChildOutput(c, ch, "\n錯誤："+str(e, "message")+"\n")
		}
		switch str(turn, "status") {
		case "failed":
			h.codexFinishChild(c, ch, "Failed")
		case "interrupted":
			h.codexFinishChild(c, ch, "Cancelled")
		default:
			h.codexFinishChild(c, ch, "Done")
		}
	}
}

// ---- 請求（核准、問卷、MCP）----

func (h *Hub) onCodexRequest(convID string, p *codexProc, req *codex.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	c := h.convs[convID]
	if c == nil || c.proc != p {
		_ = p.cl.Respond(req.ID, codexDeclineResult(req.Method, req.Params))
		return
	}
	pr := req.Params
	label := func(name string) string {
		// 請求來自子代理（multi_agent）的 thread：一樣要顯示、可以回應，卡片註明是子代理
		if th := str(pr, "threadId"); p.threadID != "" && th != "" && th != p.threadID {
			return name + "（子代理）"
		}
		return name
	}
	switch req.Method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		if h.codexAutoApprove(c, p, req, "", codexSessionApprovalKey(req.Method, pr)) {
			return
		}
		if network, ok := pr["networkApprovalContext"].(map[string]any); ok {
			// 受管網路的連線核准：沒有指令也沒有工作資料夾，只有主機；「本對話都允許」只放行這個主機
			host := orDefault(str(network, "host"), "未知的主機")
			proto := ""
			if s := str(network, "protocol"); s != "" {
				proto = "通訊協定：" + s
			}
			h.codexApprovalCard(c, p, req, label("網路存取"), "web", "連線到 "+host, joinLines(str(pr, "reason"), proto), "", nil, nil, true, "本對話都允許連線到 "+host)
			return
		}
		cmd := codexDescribeCommand(pr)
		if req.Method == "execCommandApproval" {
			var parts []string
			for _, x := range anyList(pr["command"]) {
				parts = append(parts, fmt.Sprint(x))
			}
			cmd = strings.Join(parts, " ")
		}
		cwd, extra := "", ""
		if s := str(pr, "cwd"); s != "" {
			cwd = "工作資料夾：" + s
		}
		if ap, ok := pr["additionalPermissions"].(map[string]any); ok {
			extra = "額外權限：" + indentJSON(ap)
		}
		h.codexApprovalCard(c, p, req, label("執行指令"), "bash", firstLine(cmd), joinLines(str(pr, "reason"), cmd, cwd, extra), "", nil, nil, true, "本對話同樣的指令都允許")
	case "item/fileChange/requestApproval", "applyPatchApproval":
		if h.codexAutoApprove(c, p, req, "fileChange", "") {
			return
		}
		itemID := firstNonBlank(str(pr, "itemId"), str(pr, "callId"))
		title, file := "代理要修改檔案", ""
		var diff []DiffLine
		var stats *DiffStats
		if fc := p.fileChanges[itemID]; fc != nil {
			var t string
			t, file, diff, stats = codexFileChanges(fc)
			if file != "" {
				title = t
			}
		}
		root := ""
		if s := str(pr, "grantRoot"); s != "" {
			root = "要求寫入：" + s
		}
		h.codexApprovalCard(c, p, req, label("修改檔案"), "edit", title, joinLines(str(pr, "reason"), root), file, diff, stats, true, "本對話的檔案修改都不再詢問")
	case "item/permissions/requestApproval":
		if h.codexAutoApprove(c, p, req, "", codexSessionApprovalKey(req.Method, pr)) {
			return
		}
		h.codexApprovalCard(c, p, req, label("擴大權限"), "other", "代理要求更多權限（網路或檔案存取）", joinLines(str(pr, "reason"), indentJSON(pr["permissions"])), "", nil, nil, true, "本對話同樣的權限要求都允許")
	case "item/tool/requestUserInput":
		qs := codexUserInputQuestions(pr)
		if len(qs) == 0 {
			_ = p.cl.Respond(req.ID, codexUserInputResponse(nil))
			return
		}
		h.codexFinishThinking(c, p)
		it := c.addItem("Question")
		it.Questions = qs
		it.Answers = map[string][]string{}
		h.codexWaitCard(c, p, req, it, "等待你回答問題")
	case "mcpServer/elicitation/request":
		if t := codexMcpToolApproval(pr); t != nil {
			// MCP 工具呼叫的核准（不是表單）：「本對話都允許」交給 Codex 自己記（_meta.persist），宿主只在完全自動時直接放行
			if h.codexAutoApprove(c, p, req, "", "") {
				return
			}
			lbl := ""
			if t.forSession {
				lbl = "本對話都允許這個 MCP 工具"
			}
			h.codexApprovalCard(c, p, req, label("MCP："+t.server), "mcp", t.title, t.detail, "", nil, nil, t.forSession, lbl)
			return
		}
		// 真正要填欄位的表單、開網址、驗證身分：這一版還不支援，明確婉拒
		_ = p.cl.Respond(req.ID, map[string]any{"action": "decline", "content": nil, "_meta": nil})
		c.notice(h, "", codexUnsupportedElicitation(pr))
	default:
		_ = p.cl.RespondError(req.ID, -32601, "Clode Myriagent 不支援此請求："+req.Method)
	}
}

// codexAutoApprove：不必問就直接放行：完全自動一律允許；本對話已選過「都允許」的（kind＝整類、exactKey＝完全相同的請求）。
// 否則記為待回應（回傳 false，呼叫端接著畫卡片）
func (h *Hub) codexAutoApprove(c *Conv, p *codexProc, req *codex.Request, kind, exactKey string) bool {
	choice := ""
	switch {
	case normalizeCodexPermission(c.Meta.Permission) == "FullAccess":
		choice = "allow"
	case kind != "" && p.sessionAllowed[kind], exactKey != "" && p.sessionAllowedReq[exactKey]:
		choice = "allowSession"
	}
	if choice == "" {
		p.pending[req.Key()] = req
		if exactKey != "" {
			p.requestKeys[req.Key()] = exactKey
		}
		return false
	}
	_ = p.cl.Respond(req.ID, codexApprovalResult(req.Method, req.Params, choice))
	return true
}

func (h *Hub) codexApprovalCard(c *Conv, p *codexProc, req *codex.Request, name, kind, title, detail, file string, diff []DiffLine, stats *DiffStats, canSession bool, sessionLabel string) {
	h.codexFinishThinking(c, p)
	it := c.addItem("Approval")
	it.ToolName, it.ToolKind, it.Title, it.Detail, it.FilePath, it.Diff, it.DiffStats = name, kind, title, detail, file, diff, stats
	it.CanAllowForSession = canSession
	if canSession {
		it.AllowSessionLabel = sessionLabel
	}
	h.codexWaitCard(c, p, req, it, "等待你核准")
}

func (h *Hub) codexWaitCard(c *Conv, p *codexProc, req *codex.Request, it *Item, status string) {
	key := req.Key()
	p.pending[key] = req
	p.reqItems[key] = it
	it.Status = "Waiting"
	it.RequestID = key
	c.pending[it.ID] = &pendingRequest{requestID: key, toolName: req.Method, input: req.Params}
	if c.running {
		c.status = status
	}
	if !h.viewing(c.Meta.ID) {
		c.unread = true
	}
	c.changed()
}

// codexRequestResolved：serverRequest/resolved（codex 撤銷了請求：這一輪結束、被別的回應取代）：還在等的卡片改成取消
func (h *Hub) codexRequestResolved(c *Conv, p *codexProc, id any) {
	var key string
	switch v := id.(type) {
	case string:
		key = v
	case float64:
		key = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return
	}
	delete(p.pending, key)
	delete(p.requestKeys, key)
	if it := p.reqItems[key]; it != nil {
		delete(p.reqItems, key)
		delete(c.pending, it.ID)
		if it.Status == "Waiting" {
			it.Status = "Cancelled"
			c.touch(it)
			c.changed()
		}
	}
}

// codexTake：取出卡片對到的請求（已經被撤銷或程序換了的回 nil）
func codexTake(c *Conv, pr *pendingRequest) (*codexProc, *codex.Request) {
	p := codexOf(c)
	if p == nil || pr == nil {
		return nil, nil
	}
	req := p.pending[pr.requestID]
	if req == nil {
		return p, nil
	}
	delete(p.pending, pr.requestID)
	delete(p.reqItems, pr.requestID)
	return p, req
}

func (codexProvider) Approve(h *Hub, c *Conv, it *Item, pr *pendingRequest, choice, message string) {
	p, req := codexTake(c, pr)
	if req == nil {
		it.Status = "Cancelled"
		c.touch(it)
		return
	}
	if choice != "deny" && choice != "allowSession" {
		choice = "allow"
	}
	if choice == "allowSession" {
		// 檔案修改整類放行；指令與權限只放行完全相同的請求（一次同意不會變成任意指令都不再詢問）
		if codexApprovalKind(req.Method) == "fileChange" {
			p.sessionAllowed["fileChange"] = true
		} else if k := p.requestKeys[pr.requestID]; k != "" {
			p.sessionAllowedReq[k] = true
		}
	}
	delete(p.requestKeys, pr.requestID)
	_ = p.cl.Respond(req.ID, codexApprovalResult(req.Method, req.Params, choice))
	it.Decision = choice
	if choice == "deny" {
		it.Status = "Declined"
		p.deniedThisTurn = true
		if fb := strings.TrimSpace(message); fb != "" {
			it.Output = "你的說明：" + fb
			p.feedback = append(p.feedback, deniedOperation{toolName: it.ToolName, title: it.Title, feedback: fb})
		}
		h.codexStatus(c, "已拒絕，等待代理回應…")
	} else {
		it.Status = "Done"
		h.codexStatus(c, "執行中…")
	}
	c.touch(it)
	c.changed()
}

func (codexProvider) Answer(h *Hub, c *Conv, it *Item, pr *pendingRequest, answers map[string][]string, skip bool) {
	if pr.toolName == codexAsyncQuestionTool {
		h.codexAnswerAsync(c, it, answers, skip)
		return
	}
	p, req := codexTake(c, pr)
	if req == nil {
		it.Status = "Cancelled"
		c.touch(it)
		return
	}
	if skip {
		_ = p.cl.Respond(req.ID, codexUserInputResponse(nil))
		it.Status, it.Decision = "Declined", "skip"
	} else {
		_ = p.cl.Respond(req.ID, codexUserInputResponse(answers))
		it.Status, it.Decision = "Done", "answered"
		it.Answers = hideSecretAnswers(it.Questions, answers)
	}
	c.touch(it)
	c.changed()
}

// hideSecretAnswers：密碼類題目的答案只送給代理、不留在畫面與存檔
func hideSecretAnswers(qs []Question, answers map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range answers {
		out[k] = v
		for _, q := range qs {
			if q.ID == k && q.Secret && len(v) > 0 {
				out[k] = []string{"（已隱藏）"}
			}
		}
	}
	return out
}

// Codex 沒有計畫確認卡（先規劃模式的計畫是工具卡，照一般訊息確認）
func (codexProvider) ApprovePlan(h *Hub, c *Conv, it *Item, pr *pendingRequest, choice, message string) {
}

// SetOption：模型、推理強度、權限都隨每輪 turn/start 帶上，下一則訊息生效、不必重開程序。
// 切到完全自動時，畫面上還在等的核准卡直接放行（問卷照常等使用者回答）
func (codexProvider) SetOption(h *Hub, c *Conv, key, value string) bool {
	if key == "permission" && normalizeCodexPermission(value) == "FullAccess" {
		if p := codexOf(c); p != nil {
			for k, req := range p.pending {
				if req.Method == "item/tool/requestUserInput" || (req.Method == "mcpServer/elicitation/request" && codexMcpToolApproval(req.Params) == nil) {
					continue
				}
				_ = p.cl.Respond(req.ID, codexApprovalResult(req.Method, req.Params, "allow"))
				delete(p.pending, k)
				delete(p.requestKeys, k)
				if it := p.reqItems[k]; it != nil {
					delete(p.reqItems, k)
					delete(c.pending, it.ID)
					it.Status, it.Decision, it.Output = "Done", "allow", "已切到完全自動，自動允許"
					c.touch(it)
				}
			}
			if c.running {
				c.status = "執行中…"
			}
		}
	}
	return key == "model" || key == "effort" || key == "permission"
}

// ---- 非同步提問（agentMessage.questions）----

const codexAsyncQuestionTool = "asyncQuestion"

// codexAsyncQuestion：代理在回覆裡附的非同步提問：畫成問卷，答案當作下一則訊息送出；執行中就停下這一輪等回答
func (h *Hub) codexAsyncQuestion(c *Conv, p *codexProc, itemID string, qs []Question) {
	it := c.addItem("Question")
	it.Questions = qs
	it.Answers = map[string][]string{}
	it.AnswerAsMessage = true
	it.Status = "Waiting"
	it.RequestID = "async:" + itemID
	c.pending[it.ID] = &pendingRequest{requestID: it.RequestID, toolName: codexAsyncQuestionTool}
	if !h.viewing(c.Meta.ID) {
		c.unread = true
	}
	c.changed()
	if c.running && !c.stopping {
		p.pausedQuestion = it
		if !p.pausingForQuestion {
			p.pausingForQuestion = true
			c.status = "等待你回答問題"
			h.codexInterrupt(c, p)
		}
	}
}

func (h *Hub) codexAnswerAsync(c *Conv, it *Item, answers map[string][]string, skip bool) {
	p := codexOf(c)
	paused := p != nil && p.pausedQuestion == it
	if p != nil && paused {
		p.pausedQuestion = nil
	}
	var msg string
	if skip {
		it.Status, it.Decision = "Declined", "skip"
		// 為了等這個回答而停下的這一輪：略過就請代理依自己的判斷接著做
		if paused {
			msg = skippedAsyncQuestionMessage
		}
	} else {
		it.Status, it.Decision = "Done", "answered"
		it.Answers = answers
		msg = composeAnswerMessage(it.Questions, answers)
	}
	c.touch(it)
	c.changed()
	if msg == "" {
		return
	}
	if c.running {
		// 停下途中作答：排進佇列，停下之後當下一則訊息送出
		c.queue = append(c.queue, QueuedMessage{ID: newID("q"), Text: msg})
		c.dirtyComp = true
		return
	}
	h.startTurn(c, msg)
}
