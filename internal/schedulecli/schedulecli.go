// Package schedulecli 是 myriagent schedule：建立、列出、刪除 Myriagent 的定時訊息（時間到了 agent 把文字送進這個對話，或每次開新對話）。
// 只能在 Myriagent 的對話裡執行：從環境變數拿到這台 agent 的本機介面、這個對話的權杖與對話 id（C# 的 MyriagentSchedule，管道換成 HTTP）。
// 時間的寫法與檢查都在 agent 那邊（schedules.ParseWhen），這裡只轉送
package schedulecli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kevinlincg/myriagent/internal/localclient"
)

// Usage 說明
const Usage = `myriagent schedule：Myriagent 的定時訊息（時間到了把文字送進這個對話，agent 開著時才送；錯過的重複訊息只補一次）

  myriagent schedule add <時間> [--new] [--label 名稱] -- <要送的文字>
      時間（擇一）：
        --at "2026-10-27 14:00"   一次（也可以只寫 14:00＝今天或明天的 14:00）
        --daily 09:00             每天
        --weekdays 09:00          平日（週一到週五）
        --every 30m | 2h          每隔一段時間（分鐘要能整除 60、至少 5 分鐘；小時要能整除 24）
        --cron "0 9 * * 1-5"      五欄的 cron（分 時 日 月 星期，本機時間）
      --new                       每次開新對話（帳戶、模型、權限模式、資料夾照這個對話）；不加就送進這個對話
      文字也可以用 --text "…"
  myriagent schedule list               列出這台的定時訊息（* 是這個對話建立或送進這個對話的）
  myriagent schedule remove <id>        刪除
  myriagent schedule pause <id>         暫停
  myriagent schedule resume <id>        繼續
  加 --json 以 JSON 輸出

結束碼：0 成功、1 被 agent 拒絕（訊息在標準錯誤）、2 參數不對、3 不在對話裡或連不上 agent。
`

// Args 參數
type Args struct {
	Op        string
	WhenKind  string
	WhenValue string
	Text      *string
	Label     *string
	New       bool
	ID        string
	JSON      bool
	Help      bool
	Error     string
}

var whenFlags = map[string]bool{"--at": true, "--daily": true, "--weekdays": true, "--every": true, "--cron": true}

// Parse 解析參數
func Parse(args []string) Args {
	fail := func(msg string) Args { return Args{Error: msg} }
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return Args{Help: true}
	}
	a := Args{Op: args[0]}
	for i := 1; i < len(args); i++ {
		x := args[i]
		if x == "--" {
			t := strings.Join(args[i+1:], " ")
			a.Text = &t
			break
		}
		if whenFlags[x] {
			if a.WhenKind != "" {
				return fail("時間只能給一個（--at、--daily、--weekdays、--every、--cron 擇一）")
			}
			if i+1 >= len(args) {
				return fail(x + " 後面要接時間")
			}
			a.WhenKind = x[2:]
			i++
			a.WhenValue = args[i]
			continue
		}
		switch x {
		case "--new":
			a.New = true
		case "--json":
			a.JSON = true
		case "--label":
			if i+1 >= len(args) {
				return fail("--label 後面要接名稱")
			}
			i++
			l := args[i]
			a.Label = &l
		case "--text":
			if i+1 >= len(args) {
				return fail("--text 後面要接文字")
			}
			i++
			t := args[i]
			a.Text = &t
		default:
			if strings.HasPrefix(x, "--") {
				return fail("不認得的參數 " + x)
			}
			if a.ID != "" {
				return fail("多了一個參數 " + x + "（文字請放在 -- 後面）")
			}
			a.ID = x
		}
	}
	switch a.Op {
	case "add":
		if a.WhenKind == "" {
			return fail("add 要給時間：--at、--daily、--weekdays、--every 或 --cron")
		}
		if a.Text == nil || strings.TrimSpace(*a.Text) == "" {
			return fail("add 要給文字：放在 -- 後面，或用 --text")
		}
		if a.ID != "" {
			return fail("多了一個參數 " + a.ID + "（文字請放在 -- 後面）")
		}
	case "list":
	case "remove", "pause", "resume":
		if a.ID == "" {
			return fail(a.Op + " 要給 id（myriagent schedule list 看得到）")
		}
	default:
		return fail("不認得的操作 " + a.Op + "（add、list、remove、pause、resume）")
	}
	return a
}

// Request 送給 agent 的 timed 欄位：{op, when?, text?, label?, new?, id?}
func Request(a Args) map[string]any {
	timed := map[string]any{"op": a.Op}
	if a.WhenKind != "" {
		timed["when"] = map[string]any{"kind": a.WhenKind, "value": a.WhenValue}
	}
	if a.Text != nil {
		timed["text"] = *a.Text
	}
	if a.Label != nil {
		timed["label"] = *a.Label
	}
	if a.New {
		timed["new"] = true
	}
	if a.ID != "" {
		timed["id"] = a.ID
	}
	return timed
}

// Print 印出 agent 的回覆，回結束碼
func Print(o map[string]any, stdout, stderr io.Writer, asJSON bool) int {
	msg, _ := o["message"].(string)
	if o["t"] == "error" {
		if msg == "" {
			msg = "被拒絕"
		}
		fmt.Fprintln(stderr, "myriagent schedule："+msg)
		if o["code"] == "token" {
			return 3
		}
		return 1
	}
	ok, _ := o["ok"].(bool)
	if asJSON {
		b, _ := json.Marshal(o)
		if ok {
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		fmt.Fprintln(stderr, string(b))
		return 1
	}
	if !ok {
		if msg == "" {
			msg = "被拒絕"
		}
		fmt.Fprintln(stderr, "myriagent schedule："+msg)
		return 1
	}
	if items, isList := o["items"].([]any); isList {
		if len(items) == 0 {
			fmt.Fprintln(stdout, "沒有定時訊息")
		}
		for _, x := range items {
			it, _ := x.(map[string]any)
			mine := " "
			if it["mine"] == true {
				mine = "*"
			}
			state := ""
			if it["enabled"] != true {
				state = "（停用）"
			}
			next := ""
			if n, ok := it["next"].(string); ok && n != "" {
				next = "，下一次 " + n
			}
			where := "每次開新對話"
			if it["newConversation"] != true {
				title, _ := it["conversation"].(string)
				if title == "" {
					title = "已刪除的對話"
				}
				where = "送進「" + title + "」"
			}
			fmt.Fprintf(stdout, "%s %v  %v｜%v%s｜%s%s\n", mine, it["id"], it["label"], it["when"], next, where, state)
		}
		return 0
	}
	if msg == "" {
		msg = "完成"
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// Main myriagent schedule 的進入點，回結束碼
func Main(args []string, stdout, stderr io.Writer) int {
	a := Parse(args)
	if a.Help {
		fmt.Fprint(stdout, Usage)
		return 0
	}
	if a.Error != "" {
		fmt.Fprintln(stderr, "myriagent schedule："+a.Error)
		fmt.Fprintln(stderr, "用法見 myriagent schedule --help")
		return 2
	}
	env, err := localclient.FromEnv()
	if err != nil {
		fmt.Fprintln(stderr, "myriagent schedule："+err.Error())
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var reply map[string]any
	err = env.Call(ctx, "timed", map[string]any{"timed": Request(a)}, func(m map[string]any) bool {
		reply = m
		return false
	})
	if err != nil {
		fmt.Fprintln(stderr, "myriagent schedule："+err.Error())
		return 3
	}
	return Print(reply, stdout, stderr, a.JSON)
}
