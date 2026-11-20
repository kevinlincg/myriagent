# Go 版重寫計畫

把 Windows 的 .NET 版（`src/`，C#）整套改成 Go，在 macOS 與 Linux 上跑。介面網頁（`web/`）原封不動沿用。
**C# 原始碼就是規格**：每個功能照 C# 版的行為移植（含註解裡記的實測結果與踩過的坑），協定見 `docs/web-protocol.md`、`docs/sync-protocol.md`。

## 架構：一支程式、幾個子指令

| 子指令 | 對應 C# | 作用 |
|---|---|---|
| `myriagent agent` | `src/Myriagent`（桌面 app 的中樞，去掉視窗） | 每台電腦常駐：執行 Claude／Codex／agy／Grok CLI、帳戶、對話、排程…；在 127.0.0.1 提供介面網頁＋`/ws` |
| `myriagent hub` | `src/MyriagentHub` | 多台電腦同步、手機與瀏覽器的網頁版、配對、協作人、發佈新版 |
| `myriagent desk` | `src/MyriagentDeskService` | 遠端桌面的鎖定畫面支援（macOS：LaunchDaemon＋登入視窗的 LaunchAgent） |
| `myriagent remote` | `src/MyriagentRemote` | 對話裡的代理到其他電腦執行指令 |
| `myriagent schedule` | `src/MyriagentSchedule` | 對話裡的代理設定定時訊息 |

Mac 另有一層很薄的視窗程式（WKWebView 開 agent 的網址）；Linux 用瀏覽器。

## 程式碼的慣例（平行開發時一定要遵守）

- `internal/agent`：中樞。狀態全在 `Hub`（`h.mu` 保護）；送給網頁一律經 `h.sendTo`／`h.broadcast`／`h.sendToOpen`，
  串流中的變更以 `c.touch(item)`、`c.changed()` 標記，`flushLoop` 每 50 ms 合併送出。
- **新功能放新檔案**，盡量不改共用檔案：
  - 網頁的新指令：在自己的檔案 `func init() { registerCommand("cmd", func(h, v, c, msg) {...}) }`
  - `init` 訊息要多帶的欄位：`registerInitField("key", func(h, v) any {...})`
  - 中樞建好後要跑的背景工作：`registerStartHook(func(h *Hub) {...})`
  - 一種代理 CLI：實作 `Provider`（`provider.go`），`registerProvider("Codex", codexProvider{})`；
    程序狀態放 `c.proc`（自己的型別），不要在 `Conv` 加一堆欄位
- 送給網頁的資料形狀在 `types.go`（照 web-protocol 第三節）；缺的欄位補在那裡。
- 文字一律繁體中文（註解、給使用者看的訊息），風格同既有的 C#／Go 檔案。
- **測試**：每個功能都要有測試。CLI 用「假 CLI」測：測試執行檔在 `MYRIAGENT_FAKE_CLI=<名稱>` 時扮演那個 CLI
  （`internal/agent/fakecli_test.go` 的 `TestMain`），中樞照常以子程序啟動它。關鍵的修正要做變異測試（故意改壞看測試會不會紅）。
- 只用 Go 標準函式庫＋必要的小套件（目前：coder/websocket、goldmark）；不要 cgo（要能 `CGO_ENABLED=0` 交叉編譯）。
  macOS 的系統呼叫用 `golang.org/x/sys/unix` 或 `syscall`；需要 Objective-C 的（視窗、螢幕擷取）放在 Mac 視窗程式或 desk 的 Swift 小程式裡。
- `go vet ./...`、`go test -race ./...` 要過；CI 在 Linux 與 macOS 都跑（`.github/workflows/go.yml`）。

## 工作分配與狀態

| # | 範圍 | C# 來源 | 狀態 |
|---|---|---|---|
| 0 | 骨架：子指令、介面伺服器、WebSocket 橋接、Claude 基本對話、核准／問卷／計畫、佇列、停止、存檔 | Hub/、Agents/Claude* | ✅ |
| 1 | Claude 完整：附件與貼圖、子代理、背景工作、上下文用量、執行中插入（steer）、回溯、延伸、自動標題、輸出風格、自訂代理、/goal、token 計數 | Agents/Claude*、Hub/AgentHub.*.cs | 進行中 |
| 2 | Codex（app-server JSON-RPC） | Agents/Codex*、Cli/ | 進行中 |
| 3 | Antigravity、Grok（ACP） | Agents/Antigravity*、Agents/Grok* | 進行中 |
| 4 | 帳戶管理：隔離帳戶、登入登出（開終端機）、現有資料夾、用量查詢與儀表板、CLI 安裝更新、健康度 | Accounts/、Usage/、Cli/、Health/ | 進行中 |
| 5 | 排程與定時訊息（launchd／crontab）、執行列、git 狀態、背景程式、電腦狀態、檔案瀏覽與搜尋、語音通知 | Schedules/、Launch/、Git/、Background/、SystemStatus/、Files/、Speech/ | 進行中 |
| 6 | Hub 伺服器（`myriagent hub`）＋ agent 的同步：配對、對話同步、網頁版、遠端操作、協作人、發佈；Linux systemd／macOS launchd 安裝 | MyriagentHub/、Sync/、Myriagent.Protocol/ | 進行中 |
| 7 | Mac 視窗程式（Swift＋WKWebView）、開機自動啟動、打包簽章公證、自我更新 | UI/、Update/、Myriagent.Mac/ | 進行中 |
| 8 | 遠端桌面、遠端執行（`myriagent remote`）、Desk（鎖定畫面支援） | RemoteDesk/、RemoteExec/、MyriagentDeskService/ | 等 6 |
