// Package codex 啟動 Codex CLI 的 app-server（codex app-server，JSON-RPC over stdio：一行一則、沒有 jsonrpc 欄位），
// 收送請求、回應與通知。這一層只管程序與訊息；訊息的意義（工具卡、核准、一輪的開始與結束）由 agent 套件解讀。
// 參數與協定照 C# 版實測過的（src/Myriagent/Agents/CodexAppServerRules.cs、CodexAgentSession.cs）。
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Options 是啟動一個 codex app-server 要的東西
type Options struct {
	Executable string   // codex 的完整路徑
	Args       []string // 參數（ServerArgs()；測試或帳戶查詢可以換）
	WorkDir    string
	CodexHome  string   // 隔離帳戶的設定資料夾（CODEX_HOME）；空＝預設帳戶（保留繼承的 CODEX_HOME）
	Env        []string // 另外加的環境變數（KEY=VALUE）
}

// ServerArgs：對話用的 app-server 參數。功能開關一律用「-c features.名稱=…」：
// 實測 --enable 遇到這一版不認得的功能名稱會直接結束（exit 1），-c 只會警告並忽略——CLI 會自動更新，不能讓改名的旗標弄壞啟動。
// 預設模式要開 default_mode_request_user_input 才有提問工具
func ServerArgs() []string {
	return []string{
		"app-server",
		"-c", "features.default_mode_request_user_input=true",
		"-c", "suppress_unstable_features_warning=true",
	}
}

// ProbeArgs：查帳戶與模型用的短命 app-server（不開提問功能）
func ProbeArgs() []string {
	return []string{"app-server", "-c", "suppress_unstable_features_warning=true"}
}

// secretVariables：永遠不傳給子程序的變數（API Key 會改走 API 計費而不是訂閱）
var secretVariables = []string{"OPENAI_API_KEY", "CODEX_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// BuildEnv：繼承的環境去掉 API Key 與父 Claude 工作階段帶來的變數；隔離帳戶以 CODEX_HOME 指向自己的資料夾，
// 預設帳戶保留使用者自己設的 CODEX_HOME
func BuildEnv(codexHome string, extra []string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		skip := name == "CLAUDECODE" || name == "CLAUDE_CODE_ENTRYPOINT" || (codexHome != "" && name == "CODEX_HOME")
		for _, s := range secretVariables {
			if strings.EqualFold(name, s) {
				skip = true
			}
		}
		if !skip {
			env = append(env, kv)
		}
	}
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	return append(env, extra...)
}

// RPCError 是 app-server 回的錯誤（error 物件）或連線層的錯誤
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string { return e.Message }

// ErrExited：程序已結束，請求沒有回應
var ErrExited = errors.New("Codex 程序已結束")

// Request 是 app-server 送來、等宿主回應的請求（核准、問卷、MCP 表單…）
type Request struct {
	ID     json.RawMessage // 原樣回傳
	Method string
	Params map[string]any
}

// Key：畫面與回應時用的鍵（JSON-RPC id 轉字串；字串 id 去掉引號）
func (r *Request) Key() string { return IDKey(r.ID) }

// IDKey：JSON-RPC id 的字串形式（serverRequest/resolved 的 requestId 也用它比對）
func IDKey(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// Handlers：讀取的 goroutine 上逐行呼叫（回應先交給等它的 Call，不轉出來）
type Handlers struct {
	Notify  func(method string, params map[string]any)
	Request func(req *Request)
	// Exit 在程序結束、所有輸出都讀完之後呼叫一次
	Exit func(err error, stderr string)
}

// Client 是一個執行中的 codex app-server
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	waiters map[int64]chan response
	done    chan struct{}
	stderr  *tailBuffer
}

type response struct {
	result map[string]any
	err    error
}

type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params map[string]any  `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// Start 啟動程序並開始讀取（還沒 initialize：呼叫端接著送 Initialize）
func Start(o Options, hd Handlers) (*Client, error) {
	if o.Executable == "" {
		return nil, errors.New("找不到 codex")
	}
	args := o.Args
	if args == nil {
		args = ServerArgs()
	}
	cmd := exec.Command(o.Executable, args...)
	cmd.Dir = o.WorkDir
	cmd.Env = BuildEnv(o.CodexHome, o.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 自己一個程序群組：停止時連它開的指令一起
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &Client{cmd: cmd, stdin: stdin, waiters: map[int64]chan response{}, done: make(chan struct{}), stderr: &tailBuffer{max: 16 << 10}}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go c.read(stdout, hd)
	return c, nil
}

func (c *Client) read(stdout io.Reader, hd Handlers) {
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if t := strings.TrimSpace(string(line)); strings.HasPrefix(t, "{") {
			c.dispatch([]byte(t), hd)
		} else if t != "" {
			_, _ = c.stderr.Write(append([]byte(t), '\n')) // 不是 JSON 的輸出當成診斷訊息
		}
		if err != nil {
			break
		}
	}
	werr := c.cmd.Wait()
	// 先標成已結束再叫醒等回應的請求：等待的一方拿到 ErrExited 時 Alive() 一定已經是 false
	close(c.done)
	c.mu.Lock()
	for id, ch := range c.waiters {
		ch <- response{err: ErrExited}
		delete(c.waiters, id)
	}
	c.mu.Unlock()
	if hd.Exit != nil {
		hd.Exit(werr, c.stderr.String())
	}
}

func (c *Client) dispatch(line []byte, hd Handlers) {
	var m wireMessage
	if json.Unmarshal(line, &m) != nil {
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case hasID && m.Method == "":
		id, err := strconv.ParseInt(strings.TrimSpace(string(m.ID)), 10, 64)
		if err != nil {
			return
		}
		c.mu.Lock()
		ch, ok := c.waiters[id]
		delete(c.waiters, id)
		c.mu.Unlock()
		if ok {
			ch <- parseResponse(m)
		}
	case hasID:
		if hd.Request != nil {
			p := m.Params
			if p == nil {
				p = map[string]any{}
			}
			hd.Request(&Request{ID: append(json.RawMessage{}, m.ID...), Method: m.Method, Params: p})
		}
	case m.Method != "":
		if hd.Notify != nil {
			p := m.Params
			if p == nil {
				p = map[string]any{}
			}
			hd.Notify(m.Method, p)
		}
	}
}

func parseResponse(m wireMessage) response {
	if len(m.Error) > 0 && string(m.Error) != "null" {
		var e struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(m.Error, &e) == nil && e.Message != "" {
			return response{err: &RPCError{Code: e.Code, Message: e.Message}}
		}
		return response{err: &RPCError{Message: string(m.Error)}}
	}
	var result map[string]any
	_ = json.Unmarshal(m.Result, &result)
	if result == nil {
		result = map[string]any{}
	}
	return response{result: result}
}

// Call 送一個請求並等回應（ctx 逾時或取消時不再等；程序結束回 ErrExited）
func (c *Client) Call(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.mu.Lock()
	c.waiters[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.write(msg); err != nil {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%w：%s", ErrExited, lastLines(c.stderr.String(), 400))
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s 逾時", method)
		}
		return nil, ctx.Err()
	}
}

// CallTimeout：Call 加上逾時
func (c *Client) CallTimeout(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Call(ctx, method, params)
}

// Notify 送一則通知（沒有 id）
func (c *Client) Notify(method string, params map[string]any) error {
	msg := map[string]any{"method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.write(msg)
}

// Respond 回應 app-server 的請求
func (c *Client) Respond(id json.RawMessage, result map[string]any) error {
	if result == nil {
		result = map[string]any{}
	}
	return c.write(map[string]any{"id": id, "result": result})
}

// RespondError 以錯誤回應 app-server 的請求
func (c *Client) RespondError(id json.RawMessage, code int, message string) error {
	return c.write(map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}})
}

func (c *Client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// Initialize：initialize 請求＋initialized 通知（experimentalApi：背景指令等實驗性的方法要它）。回傳 initialize 的結果
func (c *Client) Initialize(ctx context.Context, version string) (map[string]any, error) {
	res, err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "myriagent", "title": "Clode Myriagent", "version": version},
		"capabilities": map[string]any{"experimentalApi": true, "requestAttestation": false},
	})
	if err != nil {
		return nil, fmt.Errorf("Codex 初始化失敗：%w", err)
	}
	if err := c.Notify("initialized", nil); err != nil {
		return nil, err
	}
	return res, nil
}

// Done 在程序結束後關閉
func (c *Client) Done() <-chan struct{} { return c.done }

// Alive 程序還在跑
func (c *Client) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// StderrTail：最近的錯誤輸出（最多 n 字元）
func (c *Client) StderrTail(n int) string { return lastLines(c.stderr.String(), n) }

// Close 關掉 stdin 讓 app-server 自己結束（實測 0.05 秒、偶爾 1.8 秒）；grace 之後還在就結束整個程序群組
func (c *Client) Close(grace time.Duration) {
	c.writeMu.Lock()
	_ = c.stdin.Close()
	c.writeMu.Unlock()
	select {
	case <-c.done:
		return
	case <-time.After(grace):
	}
	c.Kill()
}

// Kill 結束整個程序群組（先 SIGTERM，3 秒後 SIGKILL）
func (c *Client) Kill() {
	p := c.cmd.Process
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
}

// Pid 程序的 PID
func (c *Client) Pid() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
