package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// 帳戶與 CLI 管理用的短命子程序（照 C# 的 Cli/CliRunner.cs）：不開視窗執行 CLI 子指令（版本、登入狀態、登出、更新），收集輸出；
// 逾時結束整個程序群組。以及查帳戶與用量用的短命 JSON-RPC 連線（codex app-server、grok agent stdio），用完就關。

// cliResult：執行結果
type cliResult struct {
	exitCode int
	stdout   string
	stderr   string
	timedOut bool
}

func (r cliResult) combined() string { return strings.TrimSpace(r.stdout + "\n" + r.stderr) }

// runCLI 執行一個子指令。timeout<=0＝不設時限；ctx 取消時一樣結束程序（回 exitCode -1）
func runShort(ctx context.Context, path string, args []string, env []string, dir string, timeout time.Duration) cliResult {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: 4 << 20}
	cmd.Stderr = &limitedWriter{w: &errb, n: 1 << 20}
	if err := cmd.Start(); err != nil {
		return cliResult{exitCode: -1, stderr: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		return cliResult{exitCode: code, stdout: out.String(), stderr: errb.String()}
	case <-ctx.Done():
		killGroup(cmd)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return cliResult{exitCode: -1, stderr: "執行逾時", timedOut: true}
	}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

// limitedWriter：超過上限就丟掉（仍排空管線，子程序不會卡住）
type limitedWriter struct {
	mu sync.Mutex
	w  io.Writer
	n  int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n > 0 {
		k := len(p)
		if k > l.n {
			k = l.n
		}
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// ---- JSON-RPC（一行一則） ----

// rpcError：伺服器回的錯誤
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
}

// rpcClient：短命的 JSON-RPC 連線（codex app-server 不帶 jsonrpc 欄位；grok 的 ACP 帶 "jsonrpc":"2.0"）
type rpcClient struct {
	name    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	jsonrpc bool
	nextID  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	closed  bool
	exited  chan struct{}
	stderr  limitedBuffer

	// onNotify：伺服器通知（method、params），在讀取的 goroutine 上呼叫
	onNotify func(method string, params map[string]any)
}

type limitedBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, p...)
	if len(l.b) > 8192 {
		l.b = l.b[len(l.b)-8192:]
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.b)
}

// startRPC 啟動程序、開始讀輸出（還沒初始化）
func startRPC(name, path string, args, env []string, dir string, jsonrpc bool) (*rpcClient, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &rpcClient{name: name, cmd: cmd, stdin: stdin, jsonrpc: jsonrpc, pending: map[int64]chan rpcResponse{}, exited: make(chan struct{})}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go c.readLoop(stdout)
	return c, nil
}

func (c *rpcClient) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
		switch {
		case hasID && msg.Method != "":
			// 查詢用的連線不處理任何伺服器請求
			c.write(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "不支援"}})
		case hasID:
			var id int64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- rpcResponse{result: msg.Result, err: msg.Error}
			}
		case msg.Method != "":
			c.mu.Lock()
			f := c.onNotify
			c.mu.Unlock()
			if f != nil {
				f(msg.Method, msg.Params)
			}
		}
	}
	_ = c.cmd.Wait()
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- rpcResponse{err: &rpcError{Code: -1, Message: c.name + " 程序已結束：" + lastLines(strings.TrimSpace(c.stderr.String()), 3)}}
	}
	c.mu.Unlock()
	close(c.exited)
}

func (c *rpcClient) write(v map[string]any) error {
	if c.jsonrpc {
		v["jsonrpc"] = "2.0"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// setNotify 設定伺服器通知的處理（讀取的 goroutine 上呼叫）
func (c *rpcClient) setNotify(f func(method string, params map[string]any)) {
	c.mu.Lock()
	c.onNotify = f
	c.mu.Unlock()
}

// notify 送一則通知
func (c *rpcClient) notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(m)
}

// call 送一個請求、等回應。逾時或 ctx 取消回錯誤
func (c *rpcClient) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s 程序已結束：%s", c.name, lastLines(strings.TrimSpace(c.stderr.String()), 3))
	}
	c.pending[id] = ch
	c.mu.Unlock()
	m := map[string]any{"id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	if err := c.write(m); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s 程序已結束", c.name)
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-t.C:
	case <-ctx.Done():
	}
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("%s 逾時", method)
}

// callMap：call 之後把結果解成物件（null 或不是物件回 nil）
func (c *rpcClient) callMap(ctx context.Context, method string, params any, timeout time.Duration) (map[string]any, error) {
	raw, err := c.call(ctx, method, params, timeout)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m, nil
}

// close 關掉 stdin、等程序結束（最多 3 秒），之後強制結束
func (c *rpcClient) close() {
	_ = c.stdin.Close()
	select {
	case <-c.exited:
	case <-time.After(3 * time.Second):
		killGroup(c.cmd)
		<-c.exited
	}
}

// ---- codex app-server、grok agent ----

func appClientInfo() map[string]any {
	return map[string]any{"name": "myriagent", "title": "Clode Myriagent", "version": Version}
}

// startCodexAppServer：codex app-server、initialize、initialized
func startCodexAppServer(ctx context.Context, path string, env []string, dir string) (*rpcClient, error) {
	c, err := startRPC("Codex", path, []string{"app-server", "-c", "suppress_unstable_features_warning=true"}, env, dir, false)
	if err != nil {
		return nil, err
	}
	if _, err := c.call(ctx, "initialize", map[string]any{
		"clientInfo":   appClientInfo(),
		"capabilities": map[string]any{"experimentalApi": true, "requestAttestation": false},
	}, 30*time.Second); err != nil {
		c.close()
		return nil, fmt.Errorf("Codex 初始化失敗：%v", err)
	}
	_ = c.notify("initialized", nil)
	return c, nil
}

// startGrokAgent：grok agent --no-leader stdio、initialize（回 initialize 的結果）
func startGrokAgent(ctx context.Context, path string, env []string, dir string) (*rpcClient, map[string]any, error) {
	c, err := startRPC("Grok", path, []string{"agent", "--no-leader", "stdio"}, env, dir, true)
	if err != nil {
		return nil, nil, err
	}
	init, err := c.callMap(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         appClientInfo(),
	}, 30*time.Second)
	if err != nil {
		c.close()
		return nil, nil, fmt.Errorf("Grok 初始化失敗：%v", err)
	}
	return c, init, nil
}

// grokAuthRequired：沒有登入（session/new、billing 回 -32000 Authentication required）
func grokAuthRequired(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "authentication required")
}
