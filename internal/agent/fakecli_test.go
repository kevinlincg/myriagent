package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// 測試用的假 CLI：測試執行檔本身在 MYRIAGENT_FAKE_CLI 有值時改當成那一家的 CLI（照它的 stdin／stdout 協定回應），
// 中樞照常以子程序啟動它。這樣不用真的帳戶與網路，也能在 CI 跑完整的流程。
func TestMain(m *testing.M) {
	switch name := os.Getenv("MYRIAGENT_FAKE_CLI"); name {
	case "claude":
		fakeClaude()
		os.Exit(0)
	default:
		if f := fakeCLIs[name]; f != nil {
			f()
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// fakeCLIs：其他代理的假 CLI（各自的測試檔在 init 裡登記）
var fakeCLIs = map[string]func(){}

func emit(w *bufio.Writer, v any) {
	b, _ := json.Marshal(v)
	w.Write(b)
	w.WriteByte('\n')
	w.Flush()
}

// fakeClaude：照 claude -p 的 stream-json 回應。訊息文字決定劇本：
//   - 含「工具」：先要求核准一個 Bash 指令，核准後回工具結果
//   - 含「提問」：AskUserQuestion，收到答案後把答案說出來
//   - 其他：串流一段文字回覆
func fakeClaude() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	out := bufio.NewWriter(os.Stdout)
	var args = strings.Join(os.Args[1:], " ")
	// 同真的 CLI：init 回報啟動時的 --permission-mode
	mode := "default"
	for i, a := range os.Args {
		if a == "--permission-mode" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
		}
	}
	emit(out, map[string]any{"type": "system", "subtype": "init", "session_id": "s", "permissionMode": mode, "args": args})
	turn := 0
	waiting := map[string]string{} // control request id → 劇本
	for in.Scan() {
		var msg map[string]any
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		switch msg["type"] {
		case "control_request":
			req, _ := msg["request"].(map[string]any)
			id, _ := msg["request_id"].(string)
			switch req["subtype"] {
			case "initialize":
				emit(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{
					"commands": []any{map[string]any{"name": "compact", "description": "壓縮對話", "argumentHint": ""}},
					"models":   []any{map[string]any{"value": "default", "displayName": "預設"}, map[string]any{"value": "opus", "displayName": "Opus", "supportedEffortLevels": []any{"low", "high"}}},
					"account":  map[string]any{"email": "fake@example.com", "subscriptionType": "max"},
				}}})
			case "interrupt":
				emit(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{}}})
				emit(out, map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": false, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
			default:
				emit(out, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{}}})
			}
		case "control_response":
			resp, _ := msg["response"].(map[string]any)
			id, _ := resp["request_id"].(string)
			inner, _ := resp["response"].(map[string]any)
			script := waiting[id]
			delete(waiting, id)
			turn++
			mid := fmt.Sprintf("m%d", turn)
			switch script {
			case "tool":
				if inner["behavior"] == "allow" {
					emit(out, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "hello\n"}}}})
					streamText(out, mid, "指令跑完了")
				} else {
					emit(out, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": inner["message"], "is_error": true}}}})
					streamText(out, mid, "好，不跑了")
				}
			case "question":
				up, _ := inner["updatedInput"].(map[string]any)
				ans, _ := up["answers"].(map[string]any)
				streamText(out, mid, fmt.Sprintf("你選了 %v", ans["要哪一個？"]))
			}
			emit(out, map[string]any{"type": "result", "subtype": "success", "is_error": false, "usage": map[string]any{"input_tokens": 100, "cache_read_input_tokens": 1000, "output_tokens": 20}})
		case "user":
			m, _ := msg["message"].(map[string]any)
			content, _ := m["content"].([]any)
			text := ""
			for _, c := range content {
				cm, _ := c.(map[string]any)
				if t, ok := cm["text"].(string); ok {
					text += t
				}
			}
			turn++
			mid := fmt.Sprintf("m%d", turn)
			switch {
			case strings.Contains(text, "工具"):
				emit(out, map[string]any{"type": "assistant", "message": map[string]any{"id": mid, "content": []any{map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "echo hello"}}}}})
				waiting["cr1"] = "tool"
				emit(out, map[string]any{"type": "control_request", "request_id": "cr1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "echo hello"}, "tool_use_id": "tu1",
					"permission_suggestions": []any{map[string]any{"type": "addRules", "behavior": "allow", "rules": []any{map[string]any{"toolName": "Bash", "ruleContent": "echo:*"}}, "destination": "localSettings"}}}})
			case strings.Contains(text, "提問"):
				waiting["cr2"] = "question"
				emit(out, map[string]any{"type": "control_request", "request_id": "cr2", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "input": map[string]any{
					"questions": []any{map[string]any{"question": "要哪一個？", "header": "選擇", "multiSelect": false, "options": []any{map[string]any{"label": "甲"}, map[string]any{"label": "乙"}}}}}}})
			case strings.Contains(text, "慢"):
				streamText(out, mid, "慢慢來")
				// 不送 result：等 interrupt
			default:
				streamText(out, mid, "收到："+text)
				emit(out, map[string]any{"type": "result", "subtype": "success", "is_error": false, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
			}
		}
	}
}

func streamText(out *bufio.Writer, mid, text string) {
	emit(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": mid}}})
	emit(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}})
	for _, r := range []rune(text) {
		emit(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": string(r)}}})
	}
	emit(out, map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_stop", "index": 0}})
	emit(out, map[string]any{"type": "assistant", "message": map[string]any{"id": mid, "content": []any{map[string]any{"type": "text", "text": text}}}})
}
