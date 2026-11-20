package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kevinlincg/myriagent/internal/codex"
)

// Codex 帳戶的登入狀態與診斷（C# 的 AccountManager.RefreshCodexAsync、AccountRules.EvaluateCodexAccount、AgentHub.CodexDoctor.cs）。
// 登入憑證在 CODEX_HOME 的 auth.json：預設帳戶是 ~/.codex（或使用者自己設的 CODEX_HOME），隔離帳戶是它自己的資料夾（以 CODEX_HOME 指給 CLI）。
// app 不讀憑證內容：有沒有 auth.json 只決定要不要查；信箱、方案與登入方式由短命的 app-server 以 account/read 回報。

// codexHomeOf：帳戶的 CODEX_HOME
func codexHomeOf(a *Account) string {
	if a.ConfigDir != "" {
		return a.ConfigDir
	}
	if e := os.Getenv("CODEX_HOME"); e != "" {
		return e
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// codexProbing：正在查的帳戶（同一個帳戶同時只查一次）
var (
	codexProbingMu sync.Mutex
	codexProbing   = map[string]bool{}
)

// refreshCodexAccount：同步判斷沒裝、沒登入；有 auth.json 就在背景起 app-server 查 account/read 與 model/list
func refreshCodexAccount(h *Hub, a *Account) {
	cli := h.clis["Codex"]
	if cli == nil || cli.path == "" {
		a.status = "notInstalled"
		return
	}
	if _, err := os.Stat(filepath.Join(codexHomeOf(a), "auth.json")); err != nil {
		a.status, a.email, a.plan = "notLoggedIn", "", ""
		return
	}
	if a.status != "ready" && a.status != "apiKey" {
		a.status = "checking"
	}
	codexProbingMu.Lock()
	busy := codexProbing[a.ID]
	codexProbing[a.ID] = true
	codexProbingMu.Unlock()
	if busy {
		return
	}
	go h.probeCodexAccount(a.ID, cli.path, a.ConfigDir)
}

func (h *Hub) probeCodexAccount(accountID, exe, configDir string) {
	defer func() {
		codexProbingMu.Lock()
		delete(codexProbing, accountID)
		codexProbingMu.Unlock()
	}()
	status, email, plan, models, err := probeCodex(exe, configDir, filepath.Join(h.root, "cache"))
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.account(accountID)
	if a == nil {
		return
	}
	if err != nil {
		a.status = "error"
	} else {
		a.status, a.email, a.plan = status, email, plan
		if len(models) > 0 {
			a.models = models
			h.markAccountHeaders(a)
		}
	}
	h.pushAccounts()
	h.kick()
}

// probeCodex：短命的 app-server 連線查帳戶與模型，用完就關（權杖由 Codex 自己維護）
func probeCodex(exe, configDir, cacheDir string) (status, email, plan string, models []Option, err error) {
	_ = os.MkdirAll(cacheDir, 0o755)
	cl, err := codex.Start(codex.Options{Executable: exe, Args: codex.ProbeArgs(), WorkDir: cacheDir, CodexHome: configDir}, codex.Handlers{
		// 查詢用連線不處理任何伺服器請求
		Request: func(req *codex.Request) {},
	})
	if err != nil {
		return "", "", "", nil, err
	}
	defer cl.Close(3 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err = cl.Initialize(ctx, Version); err != nil {
		return "", "", "", nil, err
	}
	res, err := cl.CallTimeout("account/read", map[string]any{"refreshToken": false}, 20*time.Second)
	if err != nil {
		return "", "", "", nil, err
	}
	status, email, plan = codexAccountStatus(res)
	if status != "ready" && status != "apiKey" {
		return status, email, plan, nil, nil
	}
	if ml, err := cl.CallTimeout("model/list", map[string]any{}, 30*time.Second); err == nil {
		models = codexModels(ml)
	}
	return status, email, plan, models, nil
}

// ---- 診斷（codexDoctor）----

var (
	codexDoctorMu      sync.Mutex
	codexDoctorRunning = map[string]bool{}
)

func init() {
	registerCommand("codexDoctor", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		id := str(msg, "accountId")
		reply := map[string]any{"type": "codexDoctor", "accountId": id, "requestId": msg["requestId"]}
		a := h.account(id)
		cli := h.clis["Codex"]
		fail := func(e string) {
			reply["error"] = e
			h.sendTo(v, reply)
		}
		switch {
		case a == nil || a.Provider != "Codex":
			fail("請選擇這台電腦的 Codex 帳戶")
			return
		case cli == nil || cli.path == "":
			fail("請先安裝 Codex CLI")
			return
		}
		codexDoctorMu.Lock()
		busy := codexDoctorRunning[id]
		codexDoctorRunning[id] = true
		codexDoctorMu.Unlock()
		if busy {
			fail("這個帳戶正在診斷，請稍候")
			return
		}
		networkOnly, _ := msg["networkOnly"].(bool)
		exe, configDir, dir := cli.path, a.ConfigDir, filepath.Join(h.root, "cache", "codex-doctor")
		go func() {
			defer func() {
				codexDoctorMu.Lock()
				delete(codexDoctorRunning, id)
				codexDoctorMu.Unlock()
			}()
			report, err := runCodexDoctor(exe, configDir, dir, networkOnly)
			h.mu.Lock()
			defer h.mu.Unlock()
			if err != nil {
				reply["error"] = err.Error()
			} else if h.account(id) != a {
				reply["error"] = "帳戶已變更，請重新診斷"
			} else {
				reply["result"] = report
			}
			h.sendTo(v, reply)
			h.kick()
		}()
	})
}

// runCodexDoctor：在中立資料夾以帳戶的 CODEX_HOME 跑 codex doctor --json（90 秒逾時）。
// 只接受 CLI 遮蔽版的 schemaVersion 1；CLI 不支援或輸出格式不符時，不把原始輸出當成報告
func runCodexDoctor(exe, configDir, dir string, networkOnly bool) (map[string]any, error) {
	_ = os.MkdirAll(dir, 0o755)
	args := []string{"doctor", "--json"}
	if networkOnly {
		args = append(args, "--network-only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.Env = codex.BuildEnv(configDir, nil)
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("Codex 診斷逾時，請稍後重試")
	}
	return codexDoctorReport(out.Bytes())
}

func codexDoctorReport(stdout []byte) (map[string]any, error) {
	if len(stdout) > 512_000 {
		return nil, errors.New("Codex 診斷報告過大")
	}
	var report map[string]any
	if json.Unmarshal(bytes.TrimSpace(stdout), &report) != nil {
		return nil, errors.New("Codex 沒有回傳可讀取的診斷報告，請確認 CLI 已更新並可正常啟動")
	}
	checks, ok := report["checks"].(map[string]any)
	if v, _ := report["schemaVersion"].(float64); v != 1 || !ok {
		return nil, errors.New("這個 Codex 診斷格式尚不支援，請更新 Myriagent 或 CLI")
	}
	list := []any{}
	for _, name := range sortedKeys(checks) {
		chk, ok := checks[name].(map[string]any)
		if !ok {
			continue
		}
		if len(list) >= 200 {
			break
		}
		list = append(list, map[string]any{"id": name, "status": chk["status"], "summary": chk["summary"], "details": chk["details"], "remediation": chk["remediation"]})
	}
	return map[string]any{"version": report["codexVersion"], "status": report["overallStatus"], "generatedAt": report["generatedAt"], "checks": list}, nil
}

// sortedKeys：Go 的 map 沒有順序；JSON 物件的原始順序拿不到，改用名稱排序讓畫面穩定
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
