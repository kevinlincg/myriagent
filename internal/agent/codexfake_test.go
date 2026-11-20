package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func init() { fakeCLIs["codex"] = fakeCodex }

// fakeCodex：照 codex app-server 的 JSON-RPC（一行一則、沒有 jsonrpc 欄位）回應，形狀照 codex 0.170.0 的 generate-ts。
// 收到的每一行另外記到 MYRIAGENT_FAKE_CODEX_LOG（測試據此檢查送出的參數）。turn/start 的文字決定劇本：
//   - 含「指令」：要求核准 echo hello，允許就跑、拒絕就不跑（decline 只拒這一步，這一輪照常結束）
//   - 含「改檔」：修改 a.txt 要求核准（核准卡要有差異）
//   - 含「慢」：串流一段文字後等 turn/interrupt
//   - 含「提問」：回覆裡帶非同步提問（agentMessage.questions），之後等 turn/interrupt
//   - 含「表單」：item/tool/requestUserInput，收到答案後把答案說出來
//   - 含「計畫」：推理摘要、turn/plan/updated、回覆
//   - 含「失敗」：error（willRetry:false）＋turn/completed failed
//   - 含「當掉」：程序直接結束（結束碼 3）
//   - 含「子代理」：子代理 thread 的通知；「子代理掛了」：子代理開始後程序直接結束
//
// MYRIAGENT_FAKE_CODEX_CRASH_ON＝方法名稱時，收到那個請求就直接結束（結束碼 3）
//   - 其他：串流「收到：…」
func fakeCodex() {
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		fmt.Println(`{"schemaVersion":1,"codexVersion":"0.170.0","overallStatus":"ok","generatedAt":"2026-11-20T00:00:00Z","checks":{"auth":{"status":"ok","summary":"已登入"},"network":{"status":"warn","summary":"慢","remediation":"檢查網路"}}}`)
		return
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	out := bufio.NewWriter(os.Stdout)
	var logf *os.File
	if p := os.Getenv("MYRIAGENT_FAKE_CODEX_LOG"); p != "" {
		logf, _ = os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	}
	home := os.Getenv("CODEX_HOME")
	nextReq := 100
	turnN, threadN := 0, 0
	tokens := map[string]int64{}
	waiting := map[string]func(result map[string]any){} // 伺服器請求 id → 收到回應後的劇本
	var curThread, curTurn string
	running := false // 這一輪還沒結束（等 turn/interrupt 的劇本）

	notify := func(method string, params map[string]any) {
		emit(out, map[string]any{"method": method, "params": params})
	}
	request := func(method string, params map[string]any, then func(map[string]any)) {
		nextReq++
		id := nextReq
		waiting[fmt.Sprint(id)] = then
		emit(out, map[string]any{"id": id, "method": method, "params": params})
	}
	item := func(started bool, it map[string]any) {
		m := "item/completed"
		if started {
			m = "item/started"
		}
		notify(m, map[string]any{"threadId": curThread, "turnId": curTurn, "item": it})
	}
	say := func(id, text string) {
		item(true, map[string]any{"type": "agentMessage", "id": id, "text": ""})
		for _, r := range []rune(text) {
			notify("item/agentMessage/delta", map[string]any{"threadId": curThread, "turnId": curTurn, "itemId": id, "delta": string(r)})
		}
		item(false, map[string]any{"type": "agentMessage", "id": id, "text": text})
	}
	complete := func(status string, errMsg string) {
		tokens[curThread] += 1000
		total := tokens[curThread]
		notify("thread/tokenUsage/updated", map[string]any{"threadId": curThread, "turnId": curTurn, "tokenUsage": map[string]any{
			"total": map[string]any{"inputTokens": total, "outputTokens": total / 10, "cachedInputTokens": 0, "totalTokens": total + total/10},
			// 一輪有兩次模型呼叫：last 只是最後一次（600／60），這一輪的用量要以 total 的差算（1000／100）
			"last": map[string]any{"inputTokens": 600, "outputTokens": 60, "cachedInputTokens": 0, "totalTokens": 660},
		}})
		var e any
		if errMsg != "" {
			e = map[string]any{"message": errMsg, "codexErrorInfo": nil}
		}
		notify("turn/completed", map[string]any{"threadId": curThread, "turn": map[string]any{"id": curTurn, "status": status, "error": e, "items": []any{}}})
		running = false
	}

	for in.Scan() {
		line := in.Bytes()
		if logf != nil {
			logf.Write(append(append([]byte{}, line...), '\n'))
		}
		var msg map[string]any
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		method, _ := msg["method"].(string)
		id, hasID := msg["id"]
		params, _ := msg["params"].(map[string]any)
		if method == "" && hasID {
			// 宿主回應我們的請求
			key := fmt.Sprint(id)
			if then := waiting[key]; then != nil {
				delete(waiting, key)
				res, _ := msg["result"].(map[string]any)
				notify("serverRequest/resolved", map[string]any{"threadId": curThread, "requestId": id})
				then(res)
			}
			continue
		}
		reply := func(result map[string]any) { emit(out, map[string]any{"id": id, "result": result}) }
		replyErr := func(text string) {
			emit(out, map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": text}})
		}
		if method != "" && method == os.Getenv("MYRIAGENT_FAKE_CODEX_CRASH_ON") {
			fmt.Fprintln(os.Stderr, "panic: 在 "+method+" 當掉")
			os.Exit(3)
		}
		switch method {
		case "initialize":
			reply(map[string]any{"userAgent": "myriagent/0.170.0 (fake)", "codexHome": home, "platformFamily": "unix", "platformOs": "macos"})
		case "initialized":
		case "account/read":
			reply(map[string]any{"account": map[string]any{"type": "chatgpt", "email": "codex@example.com", "planType": "pro"}, "requiresOpenaiAuth": true})
		case "model/list":
			reply(map[string]any{"data": []any{
				map[string]any{"id": "gpt-b", "model": "gpt-b", "displayName": "GPT B", "description": "", "hidden": false, "isDefault": false,
					"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "low"}}, "defaultReasoningEffort": "low"},
				map[string]any{"id": "gpt-a", "model": "gpt-a", "displayName": "GPT A", "description": "預設", "hidden": false, "isDefault": true,
					"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "medium"}, map[string]any{"reasoningEffort": "high"}}, "defaultReasoningEffort": "medium"},
				map[string]any{"id": "secret", "model": "secret", "displayName": "藏起來", "hidden": true},
			}, "nextCursor": nil})
		case "thread/start":
			threadN++
			curThread = fmt.Sprintf("th-%d-%d", os.Getpid(), threadN)
			reply(map[string]any{"thread": map[string]any{"id": curThread}, "model": "gpt-a"})
		case "thread/resume":
			th, _ := params["threadId"].(string)
			if strings.HasPrefix(th, "missing") {
				replyErr("no rollout found for thread id " + th)
				continue
			}
			if strings.HasPrefix(th, "busy") {
				replyErr("thread " + th + " already has an active writer")
				continue
			}
			curThread = th
			reply(map[string]any{"thread": map[string]any{"id": th}, "model": "gpt-a"})
		case "turn/interrupt":
			reply(map[string]any{})
			if running {
				complete("interrupted", "")
			}
		case "turn/start":
			turnN++
			curTurn = fmt.Sprintf("turn-%d", turnN)
			running = true
			reply(map[string]any{"turn": map[string]any{"id": curTurn, "status": "inProgress", "items": []any{}, "error": nil}})
			notify("turn/started", map[string]any{"threadId": curThread, "turn": map[string]any{"id": curTurn, "status": "inProgress", "items": []any{}}})
			inputs, _ := params["input"].([]any)
			first, _ := inputs[0].(map[string]any)
			text, _ := first["text"].(string)
			switch {
			case strings.Contains(text, "指令"):
				cmd := map[string]any{"type": "commandExecution", "id": "cmd1", "command": "/bin/zsh -lc 'echo hello'", "cwd": "/tmp", "status": "inProgress",
					"commandActions": []any{map[string]any{"type": "unknown", "command": "echo hello"}}}
				item(true, cmd)
				request("item/commandExecution/requestApproval", map[string]any{"threadId": curThread, "turnId": curTurn, "itemId": "cmd1",
					"command": "echo hello", "cwd": "/tmp", "reason": "要跑測試"}, func(res map[string]any) {
					if res["decision"] == "accept" || res["decision"] == "acceptForSession" {
						notify("item/commandExecution/outputDelta", map[string]any{"threadId": curThread, "turnId": curTurn, "itemId": "cmd1", "delta": "hello\n"})
						cmd["status"], cmd["exitCode"], cmd["aggregatedOutput"] = "completed", 0, "hello\n"
						item(false, cmd)
						say("m-cmd", "指令跑完了")
					} else {
						cmd["status"] = "declined"
						item(false, cmd)
						say("m-cmd", fmt.Sprintf("好，不跑了（%v）", res["decision"]))
					}
					complete("completed", "")
				})
			case strings.Contains(text, "改檔"):
				fc := map[string]any{"type": "fileChange", "id": fmt.Sprintf("fc%d", turnN), "status": "inProgress", "changes": []any{
					map[string]any{"path": "/tmp/a.txt", "kind": map[string]any{"type": "update", "move_path": nil}, "diff": "@@ -1 +1 @@\n-舊\n+新\n"}}}
				item(true, fc)
				request("item/fileChange/requestApproval", map[string]any{"threadId": curThread, "turnId": curTurn, "itemId": fc["id"]}, func(res map[string]any) {
					fc["status"] = "completed"
					if res["decision"] != "accept" && res["decision"] != "acceptForSession" {
						fc["status"] = "declined"
					}
					item(false, fc)
					say("m-fc", fmt.Sprintf("改檔（%s）：%v", curTurn, res["decision"]))
					complete("completed", "")
				})
			case strings.Contains(text, "慢"):
				say("m-slow", "慢慢來")
			case strings.Contains(text, "提問"):
				item(false, map[string]any{"type": "agentMessage", "id": "m-ask", "text": "我先問一下",
					"questions": []any{map[string]any{"title": "要哪一個？", "options": []any{"甲", "乙"}}}})
			case strings.Contains(text, "表單"):
				request("item/tool/requestUserInput", map[string]any{"threadId": curThread, "turnId": curTurn, "itemId": "ask1", "questions": []any{
					map[string]any{"id": "q1", "header": "選擇", "question": "要哪一個？", "isOther": false, "isSecret": false,
						"options": []any{map[string]any{"label": "甲", "description": ""}, map[string]any{"label": "乙", "description": ""}}}}},
					func(res map[string]any) {
						ans, _ := res["answers"].(map[string]any)
						q1, _ := ans["q1"].(map[string]any)
						say("m-form", fmt.Sprintf("你選了 %v", q1["answers"]))
						complete("completed", "")
					})
			case strings.Contains(text, "計畫"):
				notify("item/reasoning/summaryTextDelta", map[string]any{"threadId": curThread, "turnId": curTurn, "itemId": "r1", "delta": "想一想"})
				notify("item/reasoning/summaryPartAdded", map[string]any{"threadId": curThread, "turnId": curTurn, "itemId": "r1", "summaryIndex": 1})
				notify("item/reasoning/summaryTextDelta", map[string]any{"threadId": curThread, "turnId": curTurn, "itemId": "r1", "delta": "再想"})
				notify("turn/plan/updated", map[string]any{"threadId": curThread, "turnId": curTurn, "explanation": "分兩步", "plan": []any{
					map[string]any{"step": "讀檔", "status": "completed"}, map[string]any{"step": "改檔", "status": "inProgress"}}})
				say("m-plan", "照計畫做")
				complete("completed", "")
			case strings.Contains(text, "失敗"):
				notify("error", map[string]any{"threadId": curThread, "turnId": curTurn, "willRetry": true, "error": map[string]any{"message": "斷線"}})
				notify("error", map[string]any{"threadId": curThread, "turnId": curTurn, "willRetry": true, "error": map[string]any{"message": "斷線"}})
				notify("error", map[string]any{"threadId": curThread, "turnId": curTurn, "willRetry": false, "error": map[string]any{"message": "伺服器壞了"}})
				complete("failed", "")
			case strings.Contains(text, "當掉"):
				fmt.Fprintln(os.Stderr, "panic: 假的當掉")
				os.Exit(3)
			case strings.Contains(text, "子代理掛了"):
				item(true, map[string]any{"type": "subAgentActivity", "id": "sa1", "kind": "started", "agentThreadId": "child-x", "agentPath": "探路者"})
				notify("turn/started", map[string]any{"threadId": "child-x", "turn": map[string]any{"id": "cx"}})
				out.Flush()
				os.Exit(4)
			case strings.Contains(text, "子代理"):
				child := "child-1"
				item(true, map[string]any{"type": "subAgentActivity", "id": "sa1", "kind": "started", "agentThreadId": child, "agentPath": "探路者", "model": "gpt-b"})
				notify("turn/started", map[string]any{"threadId": child, "turn": map[string]any{"id": "ct1"}})
				notify("item/started", map[string]any{"threadId": child, "item": map[string]any{"type": "commandExecution", "id": "cc1", "command": "ls"}})
				notify("item/agentMessage/delta", map[string]any{"threadId": child, "itemId": "cm1", "delta": "找到了"})
				notify("turn/completed", map[string]any{"threadId": child, "turn": map[string]any{"id": "ct1", "status": "completed"}})
				say("m-sub", "子代理做完了")
				complete("completed", "")
			default:
				say("m"+curTurn, "收到："+text)
				complete("completed", "")
			}
		default:
			if hasID {
				emit(out, map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "Method not found: " + method}})
			}
		}
	}
}
