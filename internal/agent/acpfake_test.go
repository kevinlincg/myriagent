package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 假的 grok（grok agent --no-leader stdio）與 agy（agy acp --stdio、agy --log-file <檔> models）：照 ACP 的 JSON-RPC 回應。
// 收到的每個請求與通知寫一行到 MYRIAGENT_FAKE_LOG（測試拿來確認送了什麼）。訊息文字（最後一段）決定劇本：
//   - 含「工具」：先畫工具卡，再要求核准一個指令；允許後回工具結果
//   - 含「編輯」：要求核准修改工作資料夾裡的 a.txt（自動接受編輯的測試）
//   - 含「提問」：grok 用 x.ai/ask_user_question、agy 用 elicitation/create，收到答案後把答案說出來
//   - 含「計畫」：grok 的 x.ai/exit_plan_mode
//   - 含「慢」：串流一段文字、開一個執行中的工具，等 session/cancel 才回應（stopReason cancelled）
//   - 含「卡住」：同上但不理 session/cancel（測試結束程序樹）
//   - 含「崩潰」：stderr 寫 boom、結束碼 3
//   - 其他：串流「收到：<文字>」（這個程序續接過對話就加「（續接）」）
func init() {
	fakeCLIs["grok"] = func() { fakeACP("grok") }
	fakeCLIs["agy"] = func() { fakeACP("agy") }
}

type fakeACPServer struct {
	flavor  string
	out     *bufio.Writer
	logFile string
	sid     string
	resumed bool
	newSeq  int
	nextReq int64
	prompt  json.RawMessage // 還沒回應的 session/prompt
	script  string
	waiting map[int64]string
}

func fakeACP(flavor string) {
	args := os.Args[1:]
	if flavor == "agy" && contains(args, "models") {
		fakeAgyModels(args)
		return
	}
	s := &fakeACPServer{flavor: flavor, out: bufio.NewWriter(os.Stdout), logFile: os.Getenv("MYRIAGENT_FAKE_LOG"), nextReq: 1000, waiting: map[int64]string{}}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result map[string]any  `json:"result"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil {
			continue
		}
		if msg.Method == "" {
			var id int64
			_ = json.Unmarshal(msg.ID, &id)
			script := s.waiting[id]
			delete(s.waiting, id)
			s.onReply(script, msg.Result)
			continue
		}
		s.log(msg.Method, msg.Params)
		s.onRequest(msg.ID, msg.Method, msg.Params)
	}
}

func fakeAgyModels(args []string) {
	if os.Getenv("MYRIAGENT_FAKE_AGY_SIGNED_OUT") == "1" {
		fmt.Fprintln(os.Stderr, "Fetching available models...")
		fmt.Println("You are not logged into Antigravity. Please sign in.")
		os.Exit(1)
	}
	for i, a := range args {
		if a == "--log-file" && i+1 < len(args) {
			_ = os.WriteFile(args[i+1], []byte("I1120 server_oauth.go:229] OAuth: authenticated successfully as agy@example.com\n"), 0o600)
		}
	}
	fmt.Fprintln(os.Stderr, "Fetching available models...")
	fmt.Print("gemini-3-flash-low\tGemini 3 Flash (Low)\ngemini-3-flash-high\tGemini 3 Flash (High)\nclaude-sonnet-4-6\tClaude Sonnet 4.6\n")
}

func (s *fakeACPServer) log(method string, params map[string]any) {
	if s.logFile == "" {
		return
	}
	b, _ := json.Marshal(params)
	f, err := os.OpenFile(s.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "%s %s %s\n", s.flavor, method, b)
		f.Close()
	}
}

func (s *fakeACPServer) emit(v any) { emit(s.out, v) }

func (s *fakeACPServer) respond(id json.RawMessage, result any) {
	s.emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *fakeACPServer) update(u map[string]any) {
	u2 := map[string]any{"sessionId": s.sid, "update": u}
	s.emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": u2})
}

func (s *fakeACPServer) ask(method string, params map[string]any, script string) {
	s.nextReq++
	s.waiting[s.nextReq] = script
	s.emit(map[string]any{"jsonrpc": "2.0", "id": s.nextReq, "method": method, "params": params})
}

func (s *fakeACPServer) say(text string) {
	for _, r := range []rune(text) {
		s.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": string(r)}})
	}
}

func (s *fakeACPServer) finish(stop string, in, out int) {
	if s.prompt == nil {
		return
	}
	usage := map[string]any{"inputTokens": in, "outputTokens": out}
	res := map[string]any{"stopReason": stop}
	if s.flavor == "grok" {
		res["_meta"] = map[string]any{"usage": usage}
	} else {
		res["usage"] = usage
	}
	s.respond(s.prompt, res)
	s.prompt = nil
}

func (s *fakeACPServer) models() map[string]any {
	if s.flavor == "grok" {
		return map[string]any{"currentModelId": "grok-4", "availableModels": []any{
			map[string]any{"modelId": "grok-4", "name": "Grok 4", "_meta": map[string]any{"reasoningEfforts": []any{map[string]any{"value": "high"}, map[string]any{"value": "low"}}}},
			map[string]any{"modelId": "grok-code", "name": "Grok Code"},
		}}
	}
	return map[string]any{"currentModelId": "gemini-3-flash", "availableModels": []any{
		map[string]any{"modelId": "gemini-3-flash", "name": "Gemini 3 Flash"},
		map[string]any{"modelId": "claude-sonnet-4-6", "name": "Claude Sonnet 4.6"},
	}}
}

func (s *fakeACPServer) onRequest(id json.RawMessage, method string, params map[string]any) {
	switch method {
	case "initialize":
		res := map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]any{"image": true}}}
		if s.flavor == "grok" {
			res["_meta"] = map[string]any{"agentVersion": "1.0.99", "modelState": s.models()}
		} else {
			res["agentInfo"] = map[string]any{"name": "antigravity-cli", "version": "1.5.12"}
		}
		s.respond(id, res)
	case "_x.ai/auth/info":
		s.respond(id, map[string]any{"email": "grok@example.com"})
	case "_x.ai/billing":
		s.respond(id, map[string]any{"subscription_tier": "SuperGrok"})
	case "session/new":
		s.newSeq++
		if s.flavor == "grok" {
			meta, _ := params["_meta"].(map[string]any)
			s.sid, _ = meta["sessionId"].(string)
		} else {
			s.sid = fmt.Sprintf("agy-%d-%d", os.Getpid(), s.newSeq)
		}
		s.resumed = false
		res := map[string]any{"sessionId": s.sid, "models": s.models()}
		if s.flavor == "agy" {
			res["modes"] = map[string]any{"currentModeId": "default"}
			res["configOptions"] = []any{map[string]any{"id": "thought_level", "currentValue": "high", "options": []any{map[string]any{"value": "low"}, map[string]any{"value": "high"}}}}
			res["_meta"] = map[string]any{"antigravity": map[string]any{"tier": map[string]any{"id": "g1-ultra-tier", "name": "Google AI Ultra"}}}
		}
		s.respond(id, res)
		s.update(map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": []any{
			map[string]any{"name": "compact", "description": "壓縮對話"}, map[string]any{"name": "always-approve", "description": "x"}}})
	case "session/resume":
		sid, _ := params["sessionId"].(string)
		if strings.HasPrefix(sid, "gone") {
			s.emit(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "Path not found", "data": map[string]any{"code": "FS_NOT_FOUND"}}})
			return
		}
		s.sid, s.resumed = sid, true
		s.respond(id, map[string]any{})
	case "session/prompt", "_x.ai/compact_conversation":
		s.prompt = id
		s.runScript(fakePromptText(params))
	case "session/cancel":
		if s.script == "slow" {
			s.finish("cancelled", 1, 1)
		}
	default:
		if id != nil {
			s.respond(id, map[string]any{})
		}
	}
}

// fakePromptText：prompt 裡的文字區塊，只取最後一段（前面可能是說明或前文節錄）
func fakePromptText(params map[string]any) string {
	arr, _ := params["prompt"].([]any)
	text := ""
	for _, b := range arr {
		m, _ := b.(map[string]any)
		if m["type"] == "text" {
			text, _ = m["text"].(string)
		}
	}
	parts := strings.Split(text, "\n\n")
	return parts[len(parts)-1]
}

func (s *fakeACPServer) toolCall(id, name, kind string, raw map[string]any, extra map[string]any) map[string]any {
	call := map[string]any{"sessionUpdate": "tool_call", "toolCallId": id, "kind": kind, "status": "pending", "rawInput": raw}
	if s.flavor == "grok" {
		call["title"] = name
		call["_meta"] = map[string]any{"x.ai/tool": map[string]any{"name": name}}
	} else {
		call["title"] = "Run command: " + fmt.Sprint(raw["CommandLine"])
	}
	for k, v := range extra {
		call[k] = v
	}
	return call
}

var fakePermissionOptions = []any{
	map[string]any{"optionId": "allow-once", "name": "Allow", "kind": "allow_once"},
	map[string]any{"optionId": "always-allow", "name": "Always", "kind": "allow_always"},
	map[string]any{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"},
}

func (s *fakeACPServer) runScript(text string) {
	s.script = ""
	switch {
	case strings.Contains(text, "工具"):
		raw := map[string]any{"command": "echo hello"}
		if s.flavor == "agy" {
			raw = map[string]any{"CommandLine": "echo hello"}
		}
		call := s.toolCall("t1", "run_terminal_command", "execute", raw, nil)
		s.update(call)
		delete(call, "sessionUpdate")
		s.ask("session/request_permission", map[string]any{"sessionId": s.sid, "toolCall": call, "options": fakePermissionOptions}, "tool")
	case strings.Contains(text, "編輯"):
		wd, _ := os.Getwd()
		path := filepath.Join(wd, "a.txt")
		call := s.toolCall("e1", "edit", "edit", map[string]any{"file_path": path}, map[string]any{
			"content":   []any{map[string]any{"type": "diff", "path": path, "oldText": "舊\n", "newText": "新\n"}},
			"locations": []any{map[string]any{"path": path}},
		})
		s.update(call)
		delete(call, "sessionUpdate")
		s.ask("session/request_permission", map[string]any{"sessionId": s.sid, "toolCall": call, "options": fakePermissionOptions}, "edit")
	case strings.Contains(text, "提問"):
		if s.flavor == "grok" {
			s.ask("_x.ai/ask_user_question", map[string]any{"sessionId": s.sid, "questions": []any{map[string]any{"question": "要哪一個？", "options": []any{map[string]any{"label": "甲"}, map[string]any{"label": "乙"}}}}}, "question")
		} else {
			s.ask("elicitation/create", map[string]any{"mode": "form", "message": "選一個", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{
				"pick": map[string]any{"type": "string", "title": "選擇", "description": "要哪一個？", "enum": []any{"甲", "乙"}}}}}, "question")
		}
	case strings.Contains(text, "計畫"):
		s.update(map[string]any{"sessionUpdate": "plan", "entries": []any{map[string]any{"content": "讀程式", "status": "completed"}, map[string]any{"content": "改程式", "status": "pending"}}})
		s.ask("_x.ai/exit_plan_mode", map[string]any{"sessionId": s.sid, "planContent": "1. 讀程式\n2. 改程式"}, "plan")
	case strings.Contains(text, "慢"), strings.Contains(text, "卡住"):
		s.script = "slow"
		if strings.Contains(text, "卡住") {
			s.script = "stuck"
		}
		s.say("慢慢來")
		s.update(s.toolCall("s1", "run_terminal_command", "execute", map[string]any{"command": "sleep 100", "CommandLine": "sleep 100"}, map[string]any{"status": "in_progress"}))
	case strings.Contains(text, "崩潰"):
		s.out.Flush()
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	default:
		s.update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "想一下"}})
		reply := "收到：" + text
		if s.resumed {
			reply += "（續接）"
		}
		s.say(reply)
		s.finish("end_turn", 10, 5)
	}
}

func (s *fakeACPServer) onReply(script string, result map[string]any) {
	switch script {
	case "tool", "edit":
		outcome, _ := result["outcome"].(map[string]any)
		opt, _ := outcome["optionId"].(string)
		id := "t1"
		if script == "edit" {
			id = "e1"
		}
		if strings.HasPrefix(opt, "allow") || opt == "always-allow" {
			if script == "edit" {
				s.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "completed"})
				s.say("改好了（" + opt + "）")
			} else {
				s.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "completed",
					"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "hello\n"}}}, "rawOutput": map[string]any{"type": "Bash", "exit_code": 0}})
				s.say("指令跑完了（" + opt + "）")
			}
		} else {
			s.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "failed", "_meta": map[string]any{"toolNotRun": "rejected"},
				"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "User rejected the execution"}}}})
			s.say("好，不跑了")
		}
		s.finish("end_turn", 100, 20)
	case "question":
		answer := ""
		if s.flavor == "grok" {
			answers, _ := result["answers"].(map[string]any)
			arr, _ := answers["要哪一個？"].([]any)
			if len(arr) > 0 {
				answer = fmt.Sprint(arr[0])
			}
			if ann, ok := result["annotations"].(map[string]any); ok {
				if n, ok := ann["要哪一個？"].(map[string]any); ok {
					answer += "／" + fmt.Sprint(n["notes"])
				}
			}
		} else {
			content, _ := result["content"].(map[string]any)
			answer = fmt.Sprint(content["pick"])
			if result["action"] != "accept" {
				answer = fmt.Sprint(result["action"])
			}
		}
		s.say("你選了 " + answer)
		s.finish("end_turn", 10, 5)
	case "plan":
		if result["outcome"] == "approved" {
			s.emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": s.sid, "update": map[string]any{"sessionUpdate": "current_mode_update", "currentModeId": "default"}}})
			s.say("開始執行")
		} else {
			s.say(fmt.Sprintf("繼續規劃：%v", result["feedback"]))
		}
		s.finish("end_turn", 10, 5)
	}
}

// fakeLog：讀假 CLI 記下的請求（每行「flavor method params」）
func fakeLog(path string) []string {
	b, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

var _ = time.Second
