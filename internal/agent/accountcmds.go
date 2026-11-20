package agent

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// 帳戶的網頁指令（docs/web-protocol.md 第二節的 account*）：新增（隔離帳戶／現有資料夾／agy 的本機預設帳戶）、登入登出、改名改色、
// 精簡模式、共用設定、移除、排序、重查、開資料夾、Codex 的免費重置。

func init() {
	registerCommand("accountAdd", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		if dir, ok := msg["directory"].(string); ok && dir != "" && trimSpace(dir) != "" {
			h.addExistingAccount(v, str(msg, "provider"), str(msg, "label"), str(msg, "color"), dir)
			return
		}
		h.addAccount(v, str(msg, "provider"), str(msg, "label"), str(msg, "color"))
	})
	registerCommand("accountLogin", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		a := h.account(str(msg, "accountId"))
		if a == nil {
			return
		}
		switch m := msg["method"].(type) {
		case nil:
		case string:
			if m == "deviceCode" {
				h.loginProgress(a.ID, "這一版還不支援裝置碼登入，請改用一般登入", true, boolPtr(false))
				return
			}
			log.Printf("accountLogin 的 method 看不懂，忽略這個指令")
			return
		default:
			return
		}
		h.startLogin(a)
	})
	registerCommand("accountLoginCancel", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		if a := h.account(str(msg, "accountId")); a != nil {
			h.cancelLogin(a)
		}
	})
	registerCommand("accountLogout", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		if a := h.account(str(msg, "accountId")); a != nil {
			h.startLogout(a)
		}
	})
	registerCommand("accountUpdate", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		a := h.account(str(msg, "accountId"))
		if a == nil {
			return
		}
		var safe, share *bool
		if b, ok := msg["safeMode"].(bool); ok {
			safe = &b
		}
		if b, ok := msg["shareCustomizations"].(bool); ok {
			share = &b
		}
		h.updateAccount(a, str(msg, "label"), str(msg, "color"), safe, share)
	})
	registerCommand("accountRemove", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		del, _ := msg["deleteData"].(bool)
		h.removeAccount(v, str(msg, "accountId"), del)
	})
	registerCommand("accountReorder", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		ids, _ := msg["ids"].([]any)
		var list []string
		for _, x := range ids {
			if s, ok := x.(string); ok {
				list = append(list, s)
			}
		}
		h.reorderAccounts(list)
	})
	registerCommand("accountRefresh", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		if id := str(msg, "accountId"); id != "" {
			if a := h.account(id); a != nil {
				h.requestRefresh(a, true, reasonManual, false)
			}
			return
		}
		for _, a := range h.accounts {
			h.requestRefresh(a, true, reasonManual, false)
		}
	})
	registerCommand("accountOpenFolder", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		if a := h.account(str(msg, "accountId")); a != nil {
			dir := a.resolveConfigDir()
			_ = os.MkdirAll(dir, 0o700)
			revealInFileManager(dir)
		}
	})
	registerCommand("accountResetCredit", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		h.useResetCredit(v, h.account(str(msg, "accountId")))
	})
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func (h *Hub) toastLevel(v *View, text, level string) {
	h.sendTo(v, map[string]any{"type": "toast", "text": text, "level": level})
}

// addAccount：新增獨立帳戶（建立隔離的設定資料夾）並立即開始登入。不能新增隔離帳戶的種類改為加入本機預設帳戶
// （已經有了就說明），檢查狀態、沒登入才開始登入。呼叫時持有 h.mu
func (h *Hub) addAccount(v *View, provider, label, color string) *Account {
	if _, ok := providerNames[provider]; !ok || !h.providerAvailable(provider) {
		h.toastLevel(v, "這一版還不支援這種帳戶："+provider, "error")
		return nil
	}
	if !supportsIsolatedAccounts(provider) {
		for _, a := range h.accounts {
			if a.Provider == provider {
				h.toastLevel(v, providerShortName(provider)+" CLI 一台電腦只能登入一個帳戶，帳戶清單裡已經有了", "error")
				return nil
			}
		}
		a := newSystemDefault(provider)
		if l := cleanAccountLabel(label); l != "" {
			a.Label = l
		}
		if c := normalizeColor(color); c != "" {
			a.Color = c
		}
		a.status = "checking"
		h.accounts = append(h.accounts, a)
		h.saveAccounts()
		h.pushAccounts()
		wait := h.refreshAndWait(a, true, reasonManual)
		go func() {
			<-wait
			h.mu.Lock()
			defer h.mu.Unlock()
			if !a.rt.removed && a.status == "notLoggedIn" {
				h.startLogin(a)
			}
			h.kick()
		}()
		return a
	}
	a := &Account{ID: newAccountID(), Provider: provider, CreatedAt: nowMs()}
	a.Label = cleanAccountLabel(label)
	if a.Label == "" {
		a.Label = defaultAccountLabel(provider, h.accounts)
	}
	a.Color = normalizeColor(color)
	if a.Color == "" {
		a.Color = defaultAccountColor(provider, h.accounts)
	}
	a.ConfigDir = filepath.Join(h.accountsDir(), a.ID, isolatedFolderName(provider))
	if err := os.MkdirAll(a.ConfigDir, 0o700); err != nil {
		h.toastLevel(v, "無法建立帳戶的資料夾："+err.Error(), "error")
		return nil
	}
	h.prepareIsolatedFolder(a)
	a.status = "notLoggedIn"
	h.accounts = append(h.accounts, a)
	h.saveAccounts()
	h.pushAccounts()
	h.startLogin(a)
	return a
}

// addExistingAccount：把一個現有的設定資料夾加成帳戶（資料夾原封不動；不開始登入，只檢查狀態）。呼叫時持有 h.mu
func (h *Hub) addExistingAccount(v *View, provider, label, color, directory string) *Account {
	home := userHome()
	dir := cleanFolderInput(directory, home)
	check := checkExistingFolder(provider, dir, dirExists, defaultConfigDirs(provider, home), h.usedConfigDirs(provider),
		forbiddenConfigDirs(provider, home, h.root), func(d string) bool { return looksLikeConfigFolder(provider, d) })
	if !h.providerAvailable(provider) && check == folderOK {
		check = folderUnsupported
	}
	if check != folderOK {
		h.toastLevel(v, describeFolderCheck(check, provider, dir), "error")
		return nil
	}
	a := &Account{ID: newAccountID(), Provider: provider, ConfigDir: dir, ExistingFolder: true, ShareCustom: boolPtr(false), CreatedAt: nowMs()}
	a.Label = cleanAccountLabel(label)
	if a.Label == "" {
		a.Label = existingFolderLabel(provider, dir, h.accounts)
	}
	a.Color = normalizeColor(color)
	if a.Color == "" {
		a.Color = defaultAccountColor(provider, h.accounts)
	}
	a.status = "checking"
	h.accounts = append(h.accounts, a)
	h.saveAccounts()
	h.refreshAccount(a)
	h.requestRefresh(a, true, reasonManual, false)
	h.pushAccounts()
	h.toastLevel(v, "已加入「"+a.Label+"」（"+displayFolder(dir, home)+"）", "success")
	return a
}

// updateAccount：改名、改色、精簡模式、共用設定。呼叫時持有 h.mu
func (h *Hub) updateAccount(a *Account, label, color string, safe, share *bool) {
	if l := cleanAccountLabel(label); l != "" {
		a.Label = l
	}
	if c := normalizeColor(color); c != "" {
		a.Color = c
	}
	if safe != nil && supportsSafeMode(a.Provider) && a.SafeMode != *safe {
		a.SafeMode = *safe
		// 下一則訊息以新的設定重開程序
		h.restartAccountSessions(a)
	}
	// 使用者原有的資料夾不共用：維持 false
	if share != nil && !a.IsSystemDefault && !a.ExistingFolder && supportsSharedCustomizations(a.Provider) && a.shares() != *share {
		v := *share
		a.ShareCustom = &v
		if f := shareChangedHook; f != nil {
			f(h, a)
		}
		h.restartAccountSessions(a)
	}
	h.saveAccounts()
	h.pushAccounts()
}

// shareChangedHook：共用設定改變時（關掉時拿掉指向來源的連結、移回原件；sharedcustom.go 登記）
var shareChangedHook func(h *Hub, a *Account)

// restartAccountSessions：這個帳戶沒在執行的對話關掉程序（下一則訊息以新的設定重開）
func (h *Hub) restartAccountSessions(a *Account) {
	for _, c := range h.convs {
		if c.Meta.AccountID == a.ID && !c.running {
			h.closeSession(c)
		}
	}
}

// reorderAccounts：側欄拖曳排序；沒列到的接在後面並維持原相對順序
func (h *Hub) reorderAccounts(ids []string) {
	var ordered []*Account
	seen := map[*Account]bool{}
	for _, id := range ids {
		if a := h.account(id); a != nil && !seen[a] {
			seen[a] = true
			ordered = append(ordered, a)
		}
	}
	for _, a := range h.accounts {
		if !seen[a] {
			ordered = append(ordered, a)
		}
	}
	h.accounts = ordered
	h.saveAccounts()
	h.pushAccounts()
}

// removeAccount：移除帳戶；deleteData＝同時刪除 app 建立的隔離設定資料夾（登入憑證與 CLI 對話紀錄）。
// 系統預設帳戶與使用者原有的資料夾一律不刪。執行中的對話會被停止；對話保留在歷史（可換帳戶繼續）。呼叫時持有 h.mu
func (h *Hub) removeAccount(v *View, id string, deleteData bool) {
	a := h.account(id)
	if a == nil {
		return
	}
	root := ""
	if deleteData {
		root = deletableAccountRoot(a, h.accountsDir())
	}
	for cid, c := range h.convs {
		if c.Meta.AccountID != a.ID {
			continue
		}
		if c.running {
			if p := h.provider(c); p != nil {
				p.Stop(h, c)
			}
		}
		h.closeSession(c)
		if c.Meta.IsDraft {
			for vv := range h.views {
				if vv.open[cid] {
					h.sendTo(vv, map[string]any{"type": "conversationClosed", "conversationId": cid})
				}
				delete(vv.open, cid)
			}
			delete(h.convs, cid)
			continue
		}
		c.running = false
		c.changed()
	}
	a.rt.removed = true
	if a.rt.loginCancel != nil {
		a.rt.loginCancel()
	}
	if a.rt.cancel != nil {
		a.rt.cancel()
	}
	for _, w := range a.rt.waiters {
		close(w)
	}
	a.rt.waiters = nil
	list := h.accounts[:0]
	for _, x := range h.accounts {
		if x != a {
			list = append(list, x)
		}
	}
	h.accounts = list
	h.ar().history.remove(a.ID)
	for _, f := range accountRemovedHooks {
		f(h, a)
	}
	if h.settings.LastAccount == a.ID {
		h.settings.LastAccount = ""
	}
	delete(h.settings.LastModel, a.ID)
	h.saveSettings()
	h.saveAccounts()
	h.pushAccounts()
	h.pushConversations()
	if root == "" {
		h.toastLevel(v, "已移除「"+a.Label+"」", "success")
		return
	}
	label := a.Label
	go func() {
		ok := deleteAccountFolder(root)
		h.mu.Lock()
		defer h.mu.Unlock()
		if ok {
			h.toastLevel(v, "已移除「"+label+"」", "success")
		} else {
			h.toastLevel(v, "已移除「"+label+"」，但資料夾有檔案正在使用中，沒有完全刪除，下次啟動時會再刪："+root, "error")
		}
		h.kick()
	}()
}

// accountRemovedHooks：移除帳戶時各功能要清的東西（用量分段、token 計數…；持有 h.mu）
var accountRemovedHooks []func(h *Hub, a *Account)

// deleteAccountFolder：先在旁邊留下待刪標記（刪到一半失敗、帳戶清單又遺失時不會被當成帳戶還原），等登入與查詢的程序放開檔案，
// 拿掉 agy 家裡指向真正的家的連結（絕不能順著連結刪到那邊），再刪；被占用時重試，最多約 5 秒
func deleteAccountFolder(root string) bool {
	marker := root + pendingDeleteSuffix
	if dirExists(root) {
		_ = os.WriteFile(marker, []byte(filepath.Base(root)), 0o600)
	}
	time.Sleep(500 * time.Millisecond)
	removeAgyHomeLinks(filepath.Join(root, agyHomeFolder))
	for i := 0; i < 10; i++ {
		if err := removeAllNoFollow(root); err == nil {
			_ = os.Remove(marker)
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// removeAllNoFollow：刪除整個資料夾；遇到連結只刪連結本身（os.RemoveAll 本來就不跟進連結，這裡另外把唯讀的檔案改成可寫）
func removeAllNoFollow(root string) error {
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o200 == 0 {
			_ = os.Chmod(p, info.Mode().Perm()|0o200)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// retryPendingDeletes：上次移除帳戶時沒刪乾淨的資料夾（留有待刪標記）：再刪一次（啟動時在背景）
func retryPendingDeletes(accountsDir string) {
	for _, n := range listNames(accountsDir) {
		if filepath.Ext(n) != pendingDeleteSuffix {
			continue
		}
		root := filepath.Join(accountsDir, n[:len(n)-len(pendingDeleteSuffix)])
		if !isAccountFolderName(filepath.Base(root)) {
			continue
		}
		if dirExists(filepath.Join(root, agyHomeFolder)) {
			removeAgyHomeLinks(filepath.Join(root, agyHomeFolder))
		}
		if removeAllNoFollow(root) == nil {
			_ = os.Remove(root + pendingDeleteSuffix)
		} else {
			log.Printf("已移除帳戶的資料夾仍有檔案使用中，下次啟動再刪：%s", root)
		}
	}
}

func init() {
	registerStartHook(func(h *Hub) { retryPendingDeletes(h.accountsDir()) })
}
