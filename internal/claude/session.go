// Package claude 啟動 Claude Code CLI（claude -p，stream-json 雙向），一行一個 JSON 物件收送。
// 這一層只管程序與訊息的收送；訊息的意義（工具卡、核准、一輪的開始與結束）由 agent 套件解讀。
// 參數與控制訊息的格式照 C# 版實測過的（src/Myriagent/Agents/ClaudeStreamRules.cs）。
package claude

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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Options 是啟動一個 claude 程序要的東西
type Options struct {
	Executable     string // claude 的完整路徑
	WorkDir        string
	SessionID      string // 新對話預先產生的 UUID；Resume 時是要接續的 session
	Resume         bool
	Model          string // 空＝帳戶預設
	Effort         string // 空＝CLI 預設
	PermissionMode string // default | acceptEdits | plan | auto | bypassPermissions
	ConfigDir      string // 隔離帳戶的設定資料夾（CLAUDE_CONFIG_DIR）；空＝預設帳戶
	AppendSystem   string
	Env            []string // 另外加的環境變數（KEY=VALUE）
}

// DisallowedTools：app 自己有介面或不適合在 -p 模式用的工具（同 C# 版）
var DisallowedTools = []string{"CronCreate", "CronDelete", "CronList", "ScheduleWakeup", "RemoteTrigger", "EnterWorktree", "ExitWorktree"}

// Args 組出 claude 的參數（不含執行檔本身）
func Args(o Options) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--replay-user-messages",
		"--permission-prompt-tool", "stdio",
		"--permission-mode", orDefault(o.PermissionMode, "default"),
		"--allow-dangerously-skip-permissions",
	}
	if o.Resume {
		args = append(args, "--resume", o.SessionID)
	} else {
		args = append(args, "--session-id", o.SessionID)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
		b, _ := json.Marshal(map[string]any{"env": map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": o.Effort}})
		args = append(args, "--settings", string(b))
	}
	if len(DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", joinComma(DisallowedTools))
	}
	if o.AppendSystem != "" {
		args = append(args, "--append-system-prompt", o.AppendSystem)
	}
	return args
}

// Session 是一個執行中的 claude 程序
type Session struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	waiters map[string]chan map[string]any
	done    chan struct{}
	exitErr error
	stderr  *tailBuffer
}

// Start 啟動程序；onMessage 在讀取的 goroutine 上逐行呼叫（control_response 先交給等它的 Control 呼叫，不轉給 onMessage）。
// onExit 在程序結束、所有輸出都讀完之後呼叫一次。
func Start(o Options, onMessage func(map[string]any), onExit func(err error, stderr string)) (*Session, error) {
	if o.Executable == "" {
		return nil, errors.New("找不到 claude")
	}
	cmd := exec.Command(o.Executable, Args(o)...)
	cmd.Dir = o.WorkDir
	cmd.Env = buildEnv(o)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 自己一個程序群組：停止時連子程序一起
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &Session{cmd: cmd, stdin: stdin, waiters: map[string]chan map[string]any{}, done: make(chan struct{}), stderr: &tailBuffer{max: 16 << 10}}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var msg map[string]any
				if json.Unmarshal(line, &msg) == nil {
					if !s.deliverControlResponse(msg) {
						onMessage(msg)
					}
				}
			}
			if err != nil {
				break
			}
		}
		werr := cmd.Wait()
		s.mu.Lock()
		s.exitErr = werr
		for id, ch := range s.waiters {
			close(ch)
			delete(s.waiters, id)
		}
		s.mu.Unlock()
		close(s.done)
		if onExit != nil {
			onExit(werr, s.stderr.String())
		}
	}()
	return s, nil
}

func buildEnv(o Options) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		// 帳戶的設定資料夾由這裡決定；繼承來的 CLAUDE_CONFIG_DIR 會讓所有帳戶都變成同一個
		if hasPrefix(kv, "CLAUDE_CONFIG_DIR=") || hasPrefix(kv, "CLAUDECODE=") || hasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		env = append(env, kv)
	}
	if o.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+o.ConfigDir)
	}
	env = append(env, "DISABLE_AUTOUPDATER=1")
	return append(env, o.Env...)
}

// Done 在程序結束後關閉
func (s *Session) Done() <-chan struct{} { return s.done }

// Alive 程序還在跑
func (s *Session) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Write 寫一行 JSON 給 CLI
func (s *Session) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

// SendUser 送一則使用者訊息（images 是 base64 的圖片：[媒體型別, 資料]）
func (s *Session) SendUser(text string, uuid string, images [][2]string) error {
	content := []any{}
	for _, im := range images {
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": im[0], "data": im[1]}})
	}
	content = append(content, map[string]any{"type": "text", "text": text})
	msg := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
		"session_id":         "",
	}
	if uuid != "" {
		msg["uuid"] = uuid
	}
	return s.Write(msg)
}

// Control 送一個 control_request 並等回應（逾時或程序結束回錯誤）
func (s *Session) Control(ctx context.Context, request map[string]any) (map[string]any, error) {
	id := "req_" + strconv.FormatInt(s.nextID.Add(1), 10)
	ch := make(chan map[string]any, 1)
	s.mu.Lock()
	s.waiters[id] = ch
	s.mu.Unlock()
	if err := s.Write(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		s.mu.Lock()
		delete(s.waiters, id)
		s.mu.Unlock()
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("claude 已結束")
		}
		if sub, _ := resp["subtype"].(string); sub != "success" {
			msg, _ := resp["error"].(string)
			return nil, fmt.Errorf("claude 拒絕 %v：%s", request["subtype"], msg)
		}
		inner, _ := resp["response"].(map[string]any)
		return inner, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.waiters, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// ControlAsync 送 control_request，不等回應
func (s *Session) ControlAsync(request map[string]any) error {
	id := "req_" + strconv.FormatInt(s.nextID.Add(1), 10)
	return s.Write(map[string]any{"type": "control_request", "request_id": id, "request": request})
}

// Respond 回應 CLI 送來的 control_request（can_use_tool 等）
func (s *Session) Respond(requestID string, response map[string]any) error {
	return s.Write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": response}})
}

// RespondError 以錯誤回應 CLI 的 control_request
func (s *Session) RespondError(requestID, message string) error {
	return s.Write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": requestID, "error": message}})
}

// Interrupt 停止這一輪（程序留著）
func (s *Session) Interrupt() error { return s.ControlAsync(map[string]any{"subtype": "interrupt"}) }

// Close 關掉 stdin 讓 CLI 自己結束；grace 之後還在就結束整個程序群組
func (s *Session) Close(grace time.Duration) {
	s.writeMu.Lock()
	_ = s.stdin.Close()
	s.writeMu.Unlock()
	select {
	case <-s.done:
		return
	case <-time.After(grace):
	}
	if p := s.cmd.Process; p != nil {
		_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
		}
	}
}

// Pid 程序的 PID（還沒啟動是 0）
func (s *Session) Pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func (s *Session) deliverControlResponse(msg map[string]any) bool {
	if t, _ := msg["type"].(string); t != "control_response" {
		return false
	}
	resp, _ := msg["response"].(map[string]any)
	id, _ := resp["request_id"].(string)
	s.mu.Lock()
	ch, ok := s.waiters[id]
	if ok {
		delete(s.waiters, id)
	}
	s.mu.Unlock()
	if ok {
		ch <- resp
	}
	return true // 對不到等待者的（回送的）也不轉出去
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

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}
