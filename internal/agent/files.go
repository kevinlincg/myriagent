package agent

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 對話內的檔案瀏覽器（listFiles → files）、輸入框的 @ 檔案提示（fileSearch）、點連結與開檔（openLink、openPath、revealPath）。
// 照 C# 的 Hub/AgentHub.Files.cs：讀檔案系統一律在背景（離線的網路磁碟會卡住），失敗也回帶 error 的 files，網頁才不會一直停在讀取中；
// 回覆只給發出的視窗。

func init() {
	registerCommand("listFiles", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		if c != nil {
			h.listFiles(v, c, msg["requestId"], str(msg, "path"))
		}
	})
	registerCommand("fileSearch", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		if c != nil {
			h.searchFilesCmd(v, c, msg["requestId"], str(msg, "query"))
		}
	})
	registerCommand("openLink", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		h.openLink(v, str(msg, "href"))
	})
	registerCommand("openPath", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		go h.openLocalPath(v, str(msg, "path"))
	})
	registerCommand("revealPath", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		go h.revealPath(v, str(msg, "path"))
	})
}

// mediaURL：本機檔案在介面伺服器上的網址（/media?path=…）
func mediaURL(p string) string { return "/media?path=" + url.QueryEscape(p) }

// listFiles（呼叫時持有 h.mu）：在背景列，列完拿鎖回給這個視窗
func (h *Hub) listFiles(v *View, c *Conv, requestID any, p string) {
	root := c.workDir(h)
	target := p
	if strings.TrimSpace(target) == "" {
		target = root
	}
	// 只建立對話自己的預設 workspace：使用者選的資料夾被刪掉時不偷偷重建（回「找不到資料夾」，送出時的存在檢查也才擋得下）
	createRoot := !c.Meta.CustomFolder
	convID := c.Meta.ID
	go func() {
		if createRoot {
			_ = os.MkdirAll(root, 0o755)
		}
		entries, truncated, err := listDir(target)
		var errMsg any
		if err != nil {
			entries, truncated, errMsg = []FileEntry{}, false, err.Error()
		}
		for i := range entries {
			e := &entries[i]
			if isDisplayableMedia(e.Path) {
				u := mediaURL(e.Path)
				e.URL = &u
				// 縮圖：可直接顯示的圖片用原圖（網址帶修改時間與大小，檔案一變網址就變）；影片的縮圖還沒有（之後接上縮圖服務）
				if e.Kind == "image" {
					t := u + "&v=" + strconv.FormatInt(e.Modified, 10) + "-" + strconv.FormatInt(derefSize(e.Size), 10)
					e.ThumbURL = &t
				}
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.sendTo(v, map[string]any{
			"type": "files", "requestId": requestID, "conversationId": convID, "path": target,
			"parent": nilIfEmpty(parentDir(target)), "root": root, "entries": entries, "truncated": truncated, "error": errMsg,
		})
	}()
}

func derefSize(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// searchFilesCmd（呼叫時持有 h.mu）：@ 檔案提示，在工作資料夾的索引中模糊搜尋
func (h *Hub) searchFilesCmd(v *View, c *Conv, requestID any, query string) {
	root := c.workDir(h)
	convID := c.Meta.ID
	go func() {
		results := []FileSearchResult{}
		if fi, err := os.Stat(root); isLocalPath(root) && err == nil && fi.IsDir() {
			results = searchFiles(fileIndexes.get(root), root, query, fileMaxResults)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.sendTo(v, map[string]any{"type": "fileSearch", "requestId": requestID, "conversationId": convID, "query": query, "results": results})
	}()
}

// openLink：回覆裡的連結。本機的 /open?path=、/media?path= 照開檔處理；http、https、mailto 交給預設瀏覽器；其他不開
func (h *Hub) openLink(v *View, href string) {
	if strings.TrimSpace(href) == "" || href == "#" {
		return
	}
	u, err := url.Parse(href)
	if err != nil {
		return
	}
	if (u.Path == "/open" || u.Path == "/media") && u.Query().Get("path") != "" && (u.Host == "" || isLoopbackHost(u.Hostname())) {
		go h.openLocalPath(v, u.Query().Get("path"))
		return
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
		shellOpen(u.String())
	}
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// shellOpen／shellReveal：交給系統開（測試換成假的）
var (
	shellOpen   = openInFileManager
	shellReveal = revealInFileManager
)

// probePath：背景確認路徑是檔案、資料夾或不存在；5 秒內沒結果（卡住的網路磁碟）回 "timeout"
func probePath(p string) string {
	ch := make(chan string, 1)
	go func() {
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			ch <- "missing"
		case fi.IsDir():
			ch <- "dir"
		default:
			ch <- "file"
		}
	}()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		return "timeout"
	}
}

// openLocalPath（goroutine，不持有鎖）：只認本機的完整路徑；資料夾在檔案管理員顯示；白名單內的文件與媒體用預設程式開，
// 其他（執行檔、腳本、HTML…）只在檔案管理員中選取，不直接執行
func (h *Hub) openLocalPath(v *View, p string) {
	if strings.TrimSpace(p) == "" {
		return
	}
	if !isLocalPath(p) {
		h.toastLocked(v, "只能開啟這台電腦磁碟上的檔案")
		return
	}
	switch probePath(p) {
	case "timeout":
		h.toastLocked(v, "無法存取（逾時）："+p)
	case "dir":
		shellReveal(p)
	case "missing":
		h.toastLocked(v, "找不到檔案："+p)
	default:
		if !isSafeToOpen(p) {
			shellReveal(p)
			h.toastLocked(v, "這類檔案不直接開啟，已在檔案管理員中選取，請自行確認後再開")
			return
		}
		shellOpen(p)
	}
}

// revealPath（goroutine，不持有鎖）：在檔案管理員中選取
func (h *Hub) revealPath(v *View, p string) {
	if strings.TrimSpace(p) == "" || !isLocalPath(p) {
		return
	}
	switch probePath(p) {
	case "file", "dir":
		shellReveal(p)
	case "timeout":
		h.toastLocked(v, "無法存取（逾時）："+p)
	default:
		h.toastLocked(v, "找不到："+p)
	}
}

// toastLocked：不持有鎖時送提示
func (h *Hub) toastLocked(v *View, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toast(v, text)
}
