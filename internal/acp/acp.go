// Package acp 是 Agent Client Protocol（JSON-RPC 2.0，stdio 一行一則）的客戶端：啟動代理 CLI 的程序、送請求並配對回應、
// 送通知、回覆代理對客戶端的請求。Grok（grok agent --no-leader stdio）與 Antigravity（agy acp --stdio）共用。
// 這一層只管程序與訊息的收送；訊息的意義（文字串流、工具卡、核准）由 agent 套件解讀。
// 封包格式照 C# 版實測過的（src/Myriagent/Agents/GrokAcpRules.cs、GrokAcpConnection.cs、AntigravityAcpRules.cs）。
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Error 是 JSON-RPC 的錯誤；Text 是給人看的完整說明（message 加上 data）
type Error struct {
	Code     int
	Message  string
	DataCode string // data.code（grok 的錯誤分類，例如 FS_NOT_FOUND）
	Text     string
}

func (e *Error) Error() string { return e.Text }

// Failure 是本機產生的錯誤（程序已結束、逾時）
func Failure(text string) *Error { return &Error{Message: text, Text: text} }

// Message 是解析後的一則訊息。Method 已去掉擴充方法在線上的底線前綴（_x.ai/… → x.ai/…）
type Message struct {
	ID     json.RawMessage // 請求與回應才有；通知是 nil
	Method string
	Params map[string]any
	Result any
	Err    *Error
}

// IsRequest：代理對客戶端的請求（有 id 與 method）
func (m *Message) IsRequest() bool { return m.Method != "" && m.ID != nil }

// IsResponse：回應客戶端的請求（有 id、沒有 method）
func (m *Message) IsResponse() bool { return m.Method == "" && m.ID != nil }

// Key 是請求 id 的字串形式（卡片用它找回請求）
func (m *Message) Key() string { return strings.Trim(string(m.ID), `"`) }

// Parse 解析一行輸出；不是 JSON-RPC 物件（啟動訊息、空行）回 nil
func Parse(line []byte) *Message {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	var raw struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
		Result any             `json:"result"`
		Error  map[string]any  `json:"error"`
	}
	if json.Unmarshal(line, &raw) != nil {
		return nil
	}
	if string(raw.ID) == "null" {
		raw.ID = nil
	}
	m := &Message{ID: raw.ID, Params: raw.Params}
	if m.Params == nil {
		m.Params = map[string]any{}
	}
	if raw.Method != "" {
		m.Method = StripExtensionPrefix(raw.Method)
		return m
	}
	if raw.ID == nil {
		return nil
	}
	if raw.Error != nil {
		m.Err = ParseError(raw.Error)
	} else {
		m.Result = raw.Result
	}
	return m
}

// StripExtensionPrefix：ACP 自訂方法在線上加了底線前綴（_x.ai/billing）：比對時一律去掉
func StripExtensionPrefix(method string) string { return strings.TrimPrefix(method, "_") }

// ParseError：{code, message, data}；data 可以是物件（code、detail／message）或字串
func ParseError(e map[string]any) *Error {
	out := &Error{Message: "未知的錯誤"}
	if c, ok := e["code"].(float64); ok {
		out.Code = int(c)
	}
	if s, ok := e["message"].(string); ok && s != "" {
		out.Message = s
	}
	detail := ""
	switch d := e["data"].(type) {
	case map[string]any:
		out.DataCode, _ = d["code"].(string)
		detail, _ = d["detail"].(string)
		if detail == "" {
			detail, _ = d["message"].(string)
		}
	case string:
		detail = d
	}
	out.Text = out.Message
	if strings.TrimSpace(detail) != "" && detail != out.Message {
		out.Text = out.Message + "（" + detail + "）"
	}
	return out
}

// Options 是啟動一個 ACP 代理程序要的東西
type Options struct {
	Executable string
	Args       []string
	Dir        string
	Env        []string // 完整的環境（KEY=VALUE）；nil＝繼承
}

// Conn 是一個執行中的代理程序與它的 ACP 連線
type Conn struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	waiters map[int64]chan reply
	asyncs  map[int64]func(id int64, result any, err *Error)
	done    chan struct{}
	exited  bool // 讀取結束、還在等的請求都已結束（之後的 Request 直接回錯誤）
	exitErr error
	stderr  *tailBuffer
}

type reply struct {
	result any
	err    *Error
}

// Start 啟動程序。onMessage 在讀取的 goroutine 上依序收到代理的請求與通知（回應先交給等它的 Request／Send，不轉給 onMessage）；
// onExit 在程序結束、所有輸出都讀完之後呼叫一次（還在等的 Request 已先以錯誤結束，Send 的回呼不再呼叫）。
func Start(o Options, onMessage func(*Message), onExit func(err error, stderr string)) (*Conn, error) {
	if o.Executable == "" {
		return nil, errors.New("找不到代理的執行檔")
	}
	cmd := exec.Command(o.Executable, o.Args...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 自己一個程序群組：結束時連子程序一起
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &Conn{cmd: cmd, stdin: stdin, waiters: map[int64]chan reply{}, asyncs: map[int64]func(int64, any, *Error){}, done: make(chan struct{}), stderr: &tailBuffer{max: 16 << 10}}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if m := Parse(line); m != nil {
				if m.IsResponse() {
					c.deliver(m)
				} else if onMessage != nil {
					onMessage(m)
				}
			}
			if err != nil {
				break
			}
		}
		werr := cmd.Wait()
		c.mu.Lock()
		c.exited = true
		c.exitErr = werr
		for id, ch := range c.waiters {
			ch <- reply{err: Failure("代理程序已結束：" + lastLine(c.stderr.String()))}
			delete(c.waiters, id)
		}
		c.asyncs = map[int64]func(int64, any, *Error){}
		c.mu.Unlock()
		close(c.done)
		if onExit != nil {
			onExit(werr, c.stderr.String())
		}
	}()
	return c, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func (c *Conn) deliver(m *Message) {
	id, err := strconv.ParseInt(strings.Trim(string(m.ID), `"`), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch, waiting := c.waiters[id]
	delete(c.waiters, id)
	cb := c.asyncs[id]
	delete(c.asyncs, id)
	c.mu.Unlock()
	if waiting {
		ch <- reply{result: m.Result, err: m.Err}
	}
	if cb != nil {
		cb(id, m.Result, m.Err)
	}
}

// write 寫一行 JSON
func (c *Conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func orEmpty(p map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	return p
}

// Request 送出請求並等回應；ctx 取消或逾時回錯誤（程序結束也回錯誤）
func (c *Conn) Request(ctx context.Context, method string, params map[string]any) (any, *Error) {
	id := c.nextID.Add(1)
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.exited {
		c.mu.Unlock()
		return nil, Failure("代理程序已結束")
	}
	c.waiters[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": orEmpty(params)}); err != nil {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		return nil, Failure("代理程序已結束：" + lastLine(c.stderr.String()))
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, Failure(method + " 逾時")
		}
		return nil, Failure("已取消")
	}
}

// Send 送出請求、不等回應（session/prompt 要等這一輪結束才回應）；回應到了在讀取的 goroutine 上呼叫 done。
// 回呼收到請求 id，呼叫端以它確認是不是自己在等的那一個
func (c *Conn) Send(method string, params map[string]any, done func(id int64, result any, err *Error)) (int64, error) {
	id := c.nextID.Add(1)
	c.mu.Lock()
	c.asyncs[id] = done
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": orEmpty(params)}); err != nil {
		c.mu.Lock()
		delete(c.asyncs, id)
		c.mu.Unlock()
		return 0, err
	}
	return id, nil
}

// Notify 送通知（session/cancel、_x.ai/yolo_mode_changed）
func (c *Conn) Notify(method string, params map[string]any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": orEmpty(params)})
}

// Respond 回覆代理的請求
func (c *Conn) Respond(id json.RawMessage, result any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// RespondError 以錯誤回覆代理的請求
func (c *Conn) RespondError(id json.RawMessage, code int, message string) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

// Done 在程序結束、輸出讀完後關閉
func (c *Conn) Done() <-chan struct{} { return c.done }

// Alive 程序還在跑
func (c *Conn) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Stderr 到目前為止 stderr 的尾段
func (c *Conn) Stderr() string { return c.stderr.String() }

// Close 關掉 stdin 讓代理自己結束；grace 之後還在就結束整個程序群組
func (c *Conn) Close(grace time.Duration) {
	c.writeMu.Lock()
	_ = c.stdin.Close()
	c.writeMu.Unlock()
	select {
	case <-c.done:
		return
	case <-time.After(grace):
	}
	c.signal(syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		c.signal(syscall.SIGKILL)
	}
}

// Kill 立刻結束整個程序群組（停止後代理沒有回應）
func (c *Conn) Kill() { c.signal(syscall.SIGKILL) }

func (c *Conn) signal(sig syscall.Signal) {
	if p := c.cmd.Process; p != nil {
		_ = syscall.Kill(-p.Pid, sig)
	}
}

// Pid 程序的 PID
func (c *Conn) Pid() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
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

// String 方便除錯
func (m *Message) String() string {
	return fmt.Sprintf("acp{id=%s method=%s}", string(m.ID), m.Method)
}
