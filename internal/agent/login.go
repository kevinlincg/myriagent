package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 登入與登出（照 C# 的 AccountManager 的登入部分）：所有帳戶的登入排成一列（Codex 的登入回呼用固定的本機連接埠，同時開兩個會衝突），
// 排隊時先回報「等待另一個帳戶完成登入」；可以取消，移除帳戶也會取消它的登入。登入完成後重新檢查，做完才清掉 loginBusy。
//   - Claude：開終端機跑 claude auth login --claudeai（會開瀏覽器），視窗關閉後重新檢查
//   - Codex：app 內以 app-server 的 account/login/start 取得登入網址、開瀏覽器，等 account/login/completed；不行就開終端機跑 codex login
//   - Grok：grok agent 的 authenticate＋_x.ai/auth/get_url 取得登入網址、開瀏覽器，等 authenticate 回應
//   - Antigravity：沒有登入子指令：開終端機跑 agy（未登入時會走瀏覽器 Google 登入），每 5 秒以 agy models 確認，登入完成就關掉視窗
// 沒有終端機（Linux 沒有桌面）時顯示要執行的指令，之後每 5 秒重查登入狀態，登入了就結束。

const loginTimeout = 10 * time.Minute

// loginProgress：送給網頁的登入進度（done 時結束；結果由網頁顯示提示，宿主不另外送 toast）。呼叫時持有 h.mu
func (h *Hub) loginProgress(accountID, message string, done bool, success *bool) {
	h.broadcast(map[string]any{"type": "accountLogin", "accountId": accountID, "message": message, "done": done, "success": success})
}

func boolPtr(b bool) *bool { return &b }

// startLogin：登入（或重新登入）。呼叫時持有 h.mu
func (h *Hub) startLogin(a *Account) {
	cli := h.clis[a.Provider]
	if cli == nil || cli.path == "" {
		h.loginProgress(a.ID, describeAccountStatus(a.Provider, "notInstalled")+"，請先到設定安裝", true, boolPtr(false))
		return
	}
	if cli.cli.busy {
		h.loginProgress(a.ID, providerNames[a.Provider]+" CLI 正在更新，請等更新完成後再登入", true, boolPtr(false))
		return
	}
	if a.rt.loginBusy {
		return
	}
	a.rt.loginBusy = true
	ctx, cancel := context.WithCancel(a.lifetime())
	a.rt.loginCancel = cancel
	in := loginInput{acct: *a, cliPath: cli.path, env: accountEnv(a), r: h.ar()}
	in.acct.rt = accountRuntime{}
	h.pushAccounts()
	go h.runLogin(ctx, cancel, a, in)
}

// accountEnv：帳戶的 CLI 子程序環境（給登入、登出、各家的對話程序用）
func accountEnv(a *Account) []string {
	env := envMap(os.Environ())
	applyAccountEnv(a, env)
	return envList(env)
}

type loginInput struct {
	acct    Account
	cliPath string
	env     []string
	r       *acctRuntime
}

func (h *Hub) runLogin(ctx context.Context, cancel context.CancelFunc, a *Account, in loginInput) {
	defer cancel()
	progress := func(msg string) {
		h.mu.Lock()
		h.loginProgress(a.ID, msg, false, nil)
		h.mu.Unlock()
		h.kick()
	}
	var ok bool
	var err error
	canceled := false
	select {
	case in.r.loginGate <- struct{}{}:
	default:
		progress("等待另一個帳戶完成登入…")
		select {
		case in.r.loginGate <- struct{}{}:
		case <-ctx.Done():
			canceled = true
		}
	}
	if !canceled {
		func() {
			defer func() { <-in.r.loginGate }()
			switch a.Provider {
			case "Claude":
				ok, err = h.loginConsole(ctx, a, in, []string{"auth", "login", "--claudeai"}, "Claude 登入",
					"已開啟登入視窗，請在瀏覽器完成 Claude 登入，完成後視窗會自動關閉…", progress)
			case "Codex":
				ok, err = h.loginCodex(ctx, a, in, progress)
			case "Grok":
				ok, err = h.loginGrok(ctx, in, progress)
			case "Antigravity":
				ok, err = h.loginAntigravity(ctx, a, in, progress)
			default:
				err = fmt.Errorf("這種代理沒有登入方式")
			}
		}()
		canceled = ctx.Err() != nil
	}
	h.mu.Lock()
	a.rt.loginCancel = nil
	if a.rt.removed {
		a.rt.loginBusy = false
		h.mu.Unlock()
		return
	}
	if err != nil && !canceled {
		log.Printf("登入 %s 失敗：%v", a.Label, err)
		h.loginProgress(a.ID, "登入失敗："+err.Error(), false, nil)
	}
	// 登入後的重新查詢做完才清 loginBusy：先清的話，查詢那幾秒內送出的帳戶清單會讓網頁以為登入沒有完成
	wait := h.refreshAndWait(a, true, reasonManual)
	h.mu.Unlock()
	select {
	case <-wait:
	case <-time.After(2 * time.Minute):
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.kick()
	a.rt.loginBusy = false
	if a.rt.removed {
		return
	}
	ready := a.status == "ready"
	msg := a.statusText()
	switch {
	case ready:
		msg = "「" + a.Label + "」已登入"
	case canceled:
		msg = "已取消登入"
	case !ok:
		msg = "登入沒有完成"
	}
	h.loginProgress(a.ID, msg, true, boolPtr(ready))
	h.pushAccounts()
}

// cancelLogin：取消進行中的登入（關掉登入視窗、請 CLI 取消 app 內登入；還在排隊的直接取消）
func (h *Hub) cancelLogin(a *Account) {
	if a.rt.loginCancel != nil {
		a.rt.loginCancel()
	}
}

// loginConsole：開終端機跑登入指令，等視窗關閉（最多 10 分鐘）；沒有終端機時顯示指令並等登入狀態變成已登入
func (h *Hub) loginConsole(ctx context.Context, a *Account, in loginInput, args []string, title, opened string, progress func(string)) (bool, error) {
	proc, manual, err := in.r.console.Start(in.cliPath, args, in.env, title)
	if errors.Is(err, errNoTerminal) {
		progress("找不到可以開啟的終端機，請在終端機執行：" + manual + "（完成後這裡會自動更新）")
		return h.waitLoggedIn(ctx, a)
	}
	if err != nil {
		return false, err
	}
	progress(opened)
	ctx2, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	code, err := proc.Wait(ctx2)
	if err != nil {
		return false, nil
	}
	return code == 0, nil
}

// waitLoggedIn：每 5 秒重查登入狀態，已登入就結束（最多 10 分鐘）
func (h *Hub) waitLoggedIn(ctx context.Context, a *Account) (bool, error) {
	deadline := time.Now().Add(loginTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false, nil
		case <-time.After(loginPollInterval):
		}
		h.mu.Lock()
		wait := h.refreshAndWait(a, true, reasonManual)
		h.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return false, nil
		}
		h.mu.Lock()
		ready := a.status == "ready"
		h.mu.Unlock()
		if ready {
			return true, nil
		}
	}
	return false, nil
}

// loginPollInterval：沒有終端機、agy 登入時多久確認一次（測試縮短）
var loginPollInterval = 5 * time.Second

// loginCodex：app 內登入（account/login/start {type:"chatgpt"}）；不支援或失敗就退回終端機的 codex login
func (h *Hub) loginCodex(ctx context.Context, a *Account, in loginInput, progress func(string)) (bool, error) {
	dir := ensureDir(in.r.cacheDir)
	c, err := startCodexAppServer(ctx, in.cliPath, in.env, dir)
	if err == nil {
		completed := make(chan [2]any, 1)
		c.setNotify(func(method string, params map[string]any) {
			if method == "account/login/completed" {
				ok, _ := params["success"].(bool)
				msg, _ := params["error"].(string)
				select {
				case completed <- [2]any{ok, msg}:
				default:
				}
			}
		})
		start, err := c.callMap(ctx, "account/login/start", map[string]any{"type": "chatgpt"}, 30*time.Second)
		authURL := jstr(start, "authUrl")
		if u, perr := url.Parse(authURL); err == nil && perr == nil && u.Scheme == "https" {
			in.r.openBrowser(authURL)
			progress("已開啟瀏覽器，請完成 ChatGPT 登入…")
			t := time.NewTimer(loginTimeout)
			defer t.Stop()
			defer c.close()
			select {
			case r := <-completed:
				ok, _ := r[0].(bool)
				if msg, _ := r[1].(string); !ok && strings.TrimSpace(msg) != "" {
					return false, errors.New(msg)
				}
				return ok, nil
			case <-c.exited:
				return false, errors.New("Codex 程序已結束，登入沒有完成")
			case <-t.C:
			case <-ctx.Done():
			}
			// 逾時或取消：請 app-server 取消這次登入（關掉它的登入回呼）
			_, _ = c.call(context.Background(), "account/login/cancel", map[string]any{"loginId": jstr(start, "loginId")}, 10*time.Second)
			return false, nil
		}
		c.close()
		log.Printf("Codex app 內登入不可用（%v），改用終端機登入", err)
	} else {
		log.Printf("Codex app 內登入失敗，改用終端機登入：%v", err)
	}
	if ctx.Err() != nil {
		return false, nil
	}
	return h.loginConsole(ctx, a, in, []string{"login"}, "Codex 登入", "已開啟登入視窗，請在瀏覽器完成 ChatGPT 登入…", progress)
}

// loginGrok：grok agent 的 authenticate（methodId grok.com）＋_x.ai/auth/get_url 取得登入網址、開瀏覽器
func (h *Hub) loginGrok(ctx context.Context, in loginInput, progress func(string)) (bool, error) {
	c, _, err := startGrokAgent(ctx, in.cliPath, in.env, ensureDir(in.r.cacheDir))
	if err != nil {
		return false, err
	}
	defer c.close()
	info, _ := c.callMap(ctx, "_x.ai/auth/info", map[string]any{}, 20*time.Second)
	if st, _ := evaluateGrokAuthInfo(info); st == "ready" {
		return true, nil
	}
	type authResult struct{ err error }
	auth := make(chan authResult, 1)
	go func() {
		_, err := c.call(ctx, "authenticate", map[string]any{"methodId": "grok.com"}, loginTimeout)
		auth <- authResult{err}
	}()
	cancelAuth := func() {
		_, _ = c.call(context.Background(), "_x.ai/auth/cancel", map[string]any{}, 10*time.Second)
	}
	deadline := time.Now().Add(60 * time.Second)
	opened := false
	for !opened && time.Now().Before(deadline) {
		res, _ := c.callMap(ctx, "_x.ai/auth/get_url", map[string]any{}, 10*time.Second)
		if u, err := url.Parse(jstr(res, "auth_url")); err == nil && u.Scheme == "https" {
			in.r.openBrowser(u.String())
			progress("已開啟瀏覽器，請完成 Grok 登入…")
			opened = true
			break
		}
		select {
		case r := <-auth:
			return r.err == nil, r.err
		case <-ctx.Done():
			cancelAuth()
			return false, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !opened {
		cancelAuth()
		return false, errors.New("Grok 沒有提供登入網址，登入沒有完成")
	}
	select {
	case r := <-auth:
		if r.err != nil {
			if ctx.Err() == nil {
				cancelAuth()
			}
			return false, r.err
		}
		return true, nil
	case <-ctx.Done():
		cancelAuth()
		return false, nil
	}
}

// loginAntigravity：開終端機跑 agy（未登入時會走瀏覽器 Google 登入）；登入後 agy 停在互動介面不會自己結束，每 5 秒以 agy models 確認
func (h *Hub) loginAntigravity(ctx context.Context, a *Account, in loginInput, progress func(string)) (bool, error) {
	proc, manual, err := in.r.console.Start(in.cliPath, nil, in.env, "Antigravity 登入")
	if errors.Is(err, errNoTerminal) {
		progress("找不到可以開啟的終端機，請在終端機執行：" + manual + "，依畫面完成 Google 登入（完成後這裡會自動更新）")
		return h.waitLoggedIn(ctx, a)
	}
	if err != nil {
		return false, err
	}
	progress("已開啟 agy 視窗，請依畫面在瀏覽器完成 Google 登入；登入完成後視窗會自動關閉…")
	work := ensureDir(filepath.Join(in.r.cacheDir, "agy-empty"))
	deadline := time.Now().Add(loginTimeout)
	defer proc.Kill()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false, nil
		case <-time.After(loginPollInterval):
		}
		res := runTool(ctx, in.cliPath, []string{"models"}, in.env, work, 30*time.Second)
		if st, _ := evaluateAgyModels(res.exitCode, res.stdout, res.stderr, res.timedOut); st == "ready" {
			return true, nil
		}
		if proc.Exited() {
			return false, nil
		}
	}
	return false, nil
}

// ---- 登出 ----

// startLogout：在帳戶自己的設定資料夾跑 CLI 的登出（最多 30 秒），之後清身分快取與用量、重新檢查。
// Antigravity 沒有登出子指令：開 agy 視窗請使用者輸入 /logout。呼叫時持有 h.mu
func (h *Hub) startLogout(a *Account) {
	cli := h.clis[a.Provider]
	if cli == nil || cli.path == "" {
		return
	}
	if cli.cli.busy {
		h.loginProgress(a.ID, providerNames[a.Provider]+" CLI 正在更新，請等更新完成後再登出", true, boolPtr(false))
		return
	}
	path, env, r := cli.path, commandEnv(a), h.ar()
	ctx := a.lifetime()
	provider := a.Provider
	acctEnv := accountEnv(a)
	go func() {
		switch provider {
		case "Antigravity":
			proc, manual, err := r.console.Start(path, nil, acctEnv, "Antigravity 登出")
			h.mu.Lock()
			switch {
			case errors.Is(err, errNoTerminal):
				h.loginProgress(a.ID, "請在終端機執行 "+manual+"，輸入 /logout 後按 Enter 再結束", true, nil)
			case err != nil:
				h.loginProgress(a.ID, "無法開啟 agy 視窗："+err.Error(), true, boolPtr(false))
			default:
				h.loginProgress(a.ID, "已開啟 agy 視窗：請輸入 /logout 後按 Enter，再關閉視窗", false, nil)
			}
			h.mu.Unlock()
			if proc != nil {
				ctx2, cancel := context.WithTimeout(ctx, loginTimeout)
				_, _ = proc.Wait(ctx2)
				cancel()
				h.mu.Lock()
				h.loginProgress(a.ID, "agy 視窗已關閉，重新檢查登入狀態…", true, nil)
				h.mu.Unlock()
			}
		default:
			args := []string{"logout"}
			if provider == "Claude" {
				args = []string{"auth", "logout"}
			}
			res := runTool(ctx, path, args, env, ensureDir(r.cacheDir), 30*time.Second)
			if res.exitCode != 0 && !res.timedOut {
				log.Printf("登出 %s 失敗：%s", a.Label, truncateEllipsis(res.combined(), 200))
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if a.rt.removed {
			return
		}
		if a.LastIdentity != nil {
			a.LastIdentity = nil
			h.saveAccounts()
		}
		a.rt.usage = nil
		h.requestRefresh(a, true, reasonManual, false)
		h.pushAccounts()
		h.kick()
	}()
}

// ---- Codex 免費重置 ----

// resetCreditBlock：送出前的檢查；可以送回空字串，否則回給使用者看的原因
func resetCreditBlock(a *Account, busy, cliBusy, installed bool) string {
	switch {
	case a == nil:
		return "找不到這個帳戶"
	case a.Provider != "Codex":
		return "只有 Codex 帳戶有免費重置"
	case busy:
		return "正在使用免費重置，請稍候"
	case a.status != "ready":
		return "這個帳戶還沒登入"
	case !installed:
		return "這台電腦沒有安裝 Codex CLI"
	case cliBusy:
		return "Codex CLI 正在更新，請稍後再試"
	case a.rt.usage != nil && a.rt.usage.ResetCredits != nil && *a.rt.usage.ResetCredits == 0:
		return "沒有可用的免費重置次數"
	}
	return ""
}

// describeResetCredit：consume 的回應 → 給使用者看的結果與 toast 等級
func describeResetCredit(result map[string]any, err error) (string, string) {
	if err != nil {
		return "使用免費重置時發生問題：" + err.Error() + "。請看帳戶卡的次數確認有沒有用掉", "error"
	}
	switch outcome := jstr(result, "outcome"); outcome {
	case "reset":
		return "已使用 1 次免費重置，額度已重置", "success"
	case "alreadyRedeemed":
		return "這次的免費重置已經完成", "success"
	case "nothingToReset":
		return "Codex 回報目前沒有需要重置的額度", "info"
	case "noCredit":
		return "沒有可用的免費重置次數", "info"
	default:
		return "Codex 回了不認得的結果（" + orDefault(outcome, "沒有 outcome") + "），請看帳戶卡的次數確認有沒有用掉", "error"
	}
}

// useResetCredit：使用一次 Codex 的免費重置（會真的用掉一次；網頁先確認過）。之後以同一個 app-server 重讀用量。呼叫時持有 h.mu
func (h *Hub) useResetCredit(v *View, a *Account) {
	cli := h.clis["Codex"]
	installed := cli != nil && cli.path != ""
	if block := resetCreditBlock(a, a != nil && a.rt.resetCreditBusy, installed && cli.cli.busy, installed); block != "" {
		h.sendTo(v, map[string]any{"type": "toast", "text": block, "level": "info"})
		return
	}
	a.rt.resetCreditBusy = true
	h.pushAccounts()
	path, env, dir, ctx := cli.path, commandEnv(a), ensureDir(h.ar().cacheDir), a.lifetime()
	go func() {
		text, level := "", ""
		var limits map[string]any
		c, err := startCodexAppServer(ctx, path, env, dir)
		if err != nil {
			text, level = describeResetCredit(nil, err)
		} else {
			res, err := c.callMap(ctx, "account/rateLimitResetCredit/consume", map[string]any{"idempotencyKey": newUUID()}, 60*time.Second)
			text, level = describeResetCredit(res, err)
			limits, _ = c.callMap(ctx, "account/rateLimits/read", nil, 30*time.Second)
			c.close()
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		defer h.kick()
		a.rt.resetCreditBusy = false
		if !a.rt.removed && limits != nil {
			if s, _ := codexSnapshot(limits); s != nil {
				h.storeUsage(a, s)
			}
		}
		log.Printf("Codex 免費重置（%s）：%s", a.ID, text)
		if !v.closed {
			h.sendTo(v, map[string]any{"type": "toast", "text": text, "level": level})
		}
		h.pushAccounts()
	}()
}
