package agent

// 啟動一個帳戶的對話 CLI 之前要做的準備（各家的 Provider 在起程序前呼叫 h.prepareAccountLaunch）：
//   - agy 的隔離帳戶：整理它的家（鑰匙圈、連結、shell 小腳本；最多每 30 秒一次）
//   - 隔離帳戶共用系統預設帳戶的設定（sharedcustom.go 登記 sharedLaunchHook）：同步全域指示與連結，回傳要帶給 CLI 的記憶資料夾、
//     MCP 設定檔、Codex 的 -c 覆寫
// 只做檔案操作，可以在持有 h.mu 時呼叫。

// sharedLaunch：啟動 CLI 前準備好的共用設定（照 C# 的 AgentSharedLaunch）；全部是空的＝沒有要帶的
type sharedLaunch struct {
	// ClaudeMemoryDir：Claude 的自動記憶資料夾（共用來源的那一份）
	ClaudeMemoryDir string
	// ClaudeMcpConfig：Claude 要以 --mcp-config 帶上的 MCP 設定檔（來源的 MCP 伺服器）
	ClaudeMcpConfig string
	// CodexOverrides：Codex app-server 要帶的 -c 設定（來源的 MCP 伺服器）
	CodexOverrides []string
}

// sharedLaunchHook：共用設定的準備（sharedcustom.go 登記；nil＝沒有）
var sharedLaunchHook func(h *Hub, a *Account, workDir string) sharedLaunch

// prepareAccountLaunch：起這個帳戶的 CLI 之前呼叫；workDir 是對話的工作資料夾（Claude 的自動記憶以專案根目錄分資料夾）
func (h *Hub) prepareAccountLaunch(a *Account, workDir string) sharedLaunch {
	if a == nil {
		return sharedLaunch{}
	}
	if a.agyShadowed() {
		prepareAgyHome(a.ConfigDir)
	}
	if f := sharedLaunchHook; f != nil {
		return f(h, a, workDir)
	}
	return sharedLaunch{}
}
