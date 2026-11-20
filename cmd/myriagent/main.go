// myriagent：一支程式、幾個子指令（agent／hub／desk／remote／schedule）。
// agent 是每台電腦常駐的本體（執行 CLI、帳戶、對話，並在本機提供介面網頁）；其他子指令見各自的說明。
package main

import (
	"fmt"
	"os"

	"github.com/kevinlincg/myriagent/internal/agent"
)

const usage = `用法：myriagent <子指令> [選項]

子指令：
  agent      這台電腦的本體：執行 AI 代理 CLI、管理帳戶與對話，在本機提供介面（預設 http://127.0.0.1:47800）
  hub        多台電腦同步與網頁版的伺服器（尚未完成）
  desk       遠端桌面的鎖定畫面支援（尚未完成）
  remote     在對話裡讓代理到其他電腦執行指令（尚未完成）
  schedule   在對話裡讓代理設定定時訊息（尚未完成）
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "agent":
		err = agent.Main(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "不認得的子指令：%s\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "myriagent:", err)
		os.Exit(1)
	}
}
