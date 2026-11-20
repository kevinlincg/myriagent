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

// 帳戶、用量與 CLI 管理的假 CLI：測試執行檔本身在 MYRIAGENT_FAKE_ACCT＝劇本檔時改當成 claude／codex／agy／grok／npm
// （依參數判斷是哪一種呼叫），照劇本回應，並把每次呼叫（參數與幾個關鍵的環境變數）記在劇本指定的記錄檔，
// 測試以它確認「沒有跑 claude auth status」「隔離帳戶帶了 CODEX_HOME」之類的事。
// 在 init 裡處理（比 TestMain 早），不必改共用的 fakecli_test.go。
//
// 這個 init 也清掉繼承的 CLAUDE_CONFIG_DIR／CODEX_HOME／GROK_HOME：測試程序絕不能讀寫使用者真正的設定資料夾。

func init() {
	// 假 CLI：先照劇本回應（它要看中樞給的 CLAUDE_CONFIG_DIR 等，不能清掉）
	if spec := os.Getenv("MYRIAGENT_FAKE_ACCT"); spec != "" {
		os.Exit(runFakeAcct(spec, os.Args[1:]))
	}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		_ = os.Unsetenv(k)
	}
}

// fakeSpec：假 CLI 的劇本（測試寫進檔案，每次呼叫重新讀）
type fakeSpec struct {
	Log         string `json:"log"`
	Version     string `json:"version"`
	VersionFile string `json:"versionFile"`

	ClaudeAuthStatus string            `json:"claudeAuthStatus"`
	LoginExit        int               `json:"loginExit"`
	LoginWrites      map[string]string `json:"loginWrites"`
	LogoutExit       int               `json:"logoutExit"`

	UpdateOutput string `json:"updateOutput"`
	UpdateExit   int    `json:"updateExit"`
	UpdateTo     string `json:"updateTo"`

	CodexAccount map[string]any `json:"codexAccount"`
	CodexLimits  map[string]any `json:"codexLimits"`
	CodexLogin   bool           `json:"codexLogin"`
	ResetOutcome string         `json:"resetOutcome"`

	AgyUsage     string `json:"agyUsage"`
	AgyEmail     string `json:"agyEmail"`
	AgyCredits   string `json:"agyCredits"`
	AgyModelsOut string `json:"agyModelsOut"`

	GrokInfo         map[string]any `json:"grokInfo"`
	GrokBilling      map[string]any `json:"grokBilling"`
	GrokBillingError string         `json:"grokBillingError"`
	GrokAuthURL      string         `json:"grokAuthUrl"`
}

func readFakeSpec(path string) fakeSpec {
	var s fakeSpec
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &s)
	return s
}

func fakeAcctLog(s fakeSpec, line string) {
	if s.Log == "" {
		return
	}
	f, err := os.OpenFile(s.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	env := []string{}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "HOME", "ANTHROPIC_API_KEY", "XAI_API_KEY", "GROK_DISABLE_AUTOUPDATER", "DISABLE_AUTOUPDATER"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	fmt.Fprintf(f, "%s\t%s\n", line, strings.Join(env, " "))
}

func runFakeAcct(specPath string, args []string) int {
	s := readFakeSpec(specPath)
	joined := strings.Join(args, " ")
	fakeAcctLog(s, joined)
	switch {
	case joined == "--version":
		v := s.Version
		if b, err := os.ReadFile(s.VersionFile); err == nil && s.VersionFile != "" {
			v = strings.TrimSpace(string(b))
		}
		fmt.Println(v + " (fake)")
		return 0
	case joined == "auth status --json":
		fmt.Println("warning: {not json}")
		fmt.Println(s.ClaudeAuthStatus)
		return 0
	case strings.HasPrefix(joined, "auth login"), joined == "login":
		for p, c := range s.LoginWrites {
			_ = os.MkdirAll(filepath.Dir(p), 0o700)
			_ = os.WriteFile(p, []byte(c), 0o600)
		}
		return s.LoginExit
	case joined == "auth logout", joined == "logout":
		return s.LogoutExit
	case joined == "update":
		if s.UpdateTo != "" && s.VersionFile != "" {
			_ = os.WriteFile(s.VersionFile, []byte(s.UpdateTo), 0o600)
		}
		fmt.Println(s.UpdateOutput)
		return s.UpdateExit
	case strings.HasPrefix(joined, "install -g "):
		if s.UpdateTo != "" && s.VersionFile != "" {
			_ = os.WriteFile(s.VersionFile, []byte(s.UpdateTo), 0o600)
		}
		return s.UpdateExit
	case strings.HasPrefix(joined, "app-server"):
		fakeRPC(s, false)
		return 0
	case strings.HasPrefix(joined, "agent --no-leader stdio"):
		fakeRPC(s, true)
		return 0
	case len(args) >= 2 && args[0] == "--log-file":
		if s.AgyEmail != "" {
			_ = os.WriteFile(args[1], []byte("I1120 server_oauth.go:229] OAuth: authenticated successfully as "+s.AgyEmail+"\n"), 0o600)
		}
		if strings.Contains(joined, "/usage") {
			fmt.Println(s.AgyUsage)
		} else {
			fmt.Println(s.AgyModelsOut)
		}
		return 0
	case strings.Contains(joined, "-p /credits"):
		fmt.Println(s.AgyCredits)
		return 0
	case joined == "models":
		fmt.Println(s.AgyModelsOut)
		if strings.Contains(s.AgyModelsOut, "sign in") {
			return 1
		}
		return 0
	case joined == "":
		// agy 的互動介面（登入視窗）：一直開著，等 app 關掉
		time.Sleep(time.Minute)
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake: 不認得的參數 "+joined)
	return 2
}

// fakeRPC：codex app-server（jsonrpc=false）或 grok agent（jsonrpc=true）
func fakeRPC(s fakeSpec, grok bool) {
	in := bufio.NewScanner(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	send := func(m map[string]any) {
		if grok {
			m["jsonrpc"] = "2.0"
		}
		b, _ := json.Marshal(m)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	reply := func(id any, result any) { send(map[string]any{"id": id, "result": result}) }
	fail := func(id any, msg string) {
		send(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": msg}})
	}
	writeLogin := func() {
		for p, c := range s.LoginWrites {
			_ = os.MkdirAll(filepath.Dir(p), 0o700)
			_ = os.WriteFile(p, []byte(c), 0o600)
		}
	}
	for in.Scan() {
		var m map[string]any
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		id, method := m["id"], fmt.Sprint(m["method"])
		if id == nil {
			continue
		}
		fakeAcctLog(s, "rpc "+method)
		switch method {
		case "initialize":
			reply(id, map[string]any{"userAgent": "myriagent/0.0.1"})
		case "account/read":
			reply(id, s.CodexAccount)
		case "account/rateLimits/read":
			reply(id, s.CodexLimits)
		case "account/login/start":
			reply(id, map[string]any{"authUrl": "https://auth.openai.com/oauth?x=1", "loginId": "L1"})
			if s.CodexLogin {
				writeLogin()
				send(map[string]any{"method": "account/login/completed", "params": map[string]any{"success": true, "loginId": "L1"}})
			}
		case "account/login/cancel":
			reply(id, map[string]any{})
		case "account/rateLimitResetCredit/consume":
			reply(id, map[string]any{"outcome": s.ResetOutcome})
		case "_x.ai/auth/info":
			reply(id, s.GrokInfo)
		case "_x.ai/billing":
			if s.GrokBillingError != "" {
				fail(id, s.GrokBillingError)
			} else {
				reply(id, s.GrokBilling)
			}
		case "_x.ai/auth/get_url":
			reply(id, map[string]any{"auth_url": s.GrokAuthURL})
		case "authenticate":
			writeLogin()
			reply(id, map[string]any{})
		case "_x.ai/auth/cancel":
			reply(id, map[string]any{})
		default:
			fail(id, "unknown method "+method)
		}
	}
}
