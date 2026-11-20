// Clode Myriagent 的 Mac 視窗程式：很薄的一層。
// 真正做事的是 myriagent agent（放在 app 裡的 Contents/MacOS/myriagent，以使用者的 LaunchAgent 常駐）；
// 這裡只負責：確保 agent 在跑、開視窗以 WKWebView 顯示 agent 提供的介面、系統通知、標準的 Mac 選單。
import AppKit
import UserNotifications
import WebKit

let dataDir = FileManager.default.homeDirectoryForCurrentUser
    .appendingPathComponent("Library/Application Support/Myriagent")
let agentFile = dataDir.appendingPathComponent("agent.json")

struct AgentInfo: Decodable {
    let url: String
    let pid: Int32
    let exe: String?
}

func readAgentInfo() -> AgentInfo? {
    guard let data = try? Data(contentsOf: agentFile) else { return nil }
    return try? JSONDecoder().decode(AgentInfo.self, from: data)
}

func processAlive(_ pid: Int32) -> Bool { pid > 0 && kill(pid, 0) == 0 }

/// 放在 app 裡的 agent 執行檔
func bundledAgent() -> String? {
    Bundle.main.url(forAuxiliaryExecutable: "myriagent")?.resolvingSymlinksInPath().path
}

/// 確保 agent 在跑而且是這個 app 帶的那一份；回傳介面網址
func ensureAgent() -> String? {
    let mine = bundledAgent()
    if let info = readAgentInfo(), processAlive(info.pid), mine == nil || info.exe == mine {
        return info.url
    }
    guard let exe = mine else { return readAgentInfo()?.url }
    // 安裝（或改指到這一份）登入時自動啟動的 LaunchAgent；它會立刻啟動 agent
    let p = Process()
    p.executableURL = URL(fileURLWithPath: exe)
    p.arguments = ["agent", "install"]
    try? p.run()
    p.waitUntilExit()
    let deadline = Date().addingTimeInterval(15)
    while Date() < deadline {
        if let info = readAgentInfo(), processAlive(info.pid), info.exe == exe {
            return info.url
        }
        Thread.sleep(forTimeInterval: 0.2)
    }
    return nil
}

final class WindowController: NSObject, NSWindowDelegate, WKUIDelegate, WKNavigationDelegate, WKScriptMessageHandler {
    let window: NSWindow
    let webView: WKWebView
    let home: URL

    init(url: URL) {
        home = url
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .default()
        config.preferences.setValue(true, forKey: "developerExtrasEnabled")
        let content = WKUserContentController()
        config.userContentController = content
        webView = WKWebView(frame: .zero, configuration: config)
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1400, height: 900),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
                          backing: .buffered, defer: false)
        super.init()
        content.add(self, name: "myriagent")
        window.title = "Clode Myriagent"
        window.titlebarAppearsTransparent = false
        window.contentView = webView
        window.delegate = self
        window.setFrameAutosaveName("MyriagentMain")
        window.isReleasedWhenClosed = false
        webView.uiDelegate = self
        webView.navigationDelegate = self
        webView.allowsMagnification = true
        if window.frame.origin == .zero { window.center() }
        webView.load(URLRequest(url: url))
    }

    // 介面裡的 target=_blank 連結：用預設瀏覽器開
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = navigationAction.request.url { NSWorkspace.shared.open(url) }
        return nil
    }

    // 只在 agent 的網址裡導覽；其他網址交給預設瀏覽器
    func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = navigationAction.request.url else { return decisionHandler(.cancel) }
        if url.host == home.host && url.port == home.port { return decisionHandler(.allow) }
        if url.scheme == "http" || url.scheme == "https" || url.scheme == "mailto" { NSWorkspace.shared.open(url) }
        decisionHandler(.cancel)
    }

    // 冒煙測試（MYRIAGENT_SHELL_SMOKE=<檔案>）：介面載入後把網頁的樣子寫進檔案、結束
    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        guard let out = ProcessInfo.processInfo.environment["MYRIAGENT_SHELL_SMOKE"] else { return }
        DispatchQueue.main.asyncAfter(deadline: .now() + 4) {
            let js = "JSON.stringify({title: document.title, nodes: document.querySelectorAll('*').length, accounts: document.querySelectorAll('#sidebar button').length, bridge: !!(window.chrome && window.chrome.webview), href: location.pathname})"
            webView.evaluateJavaScript(js) { result, error in
                let text = (result as? String) ?? "{\"error\": \"\(error?.localizedDescription ?? "unknown")\"}"
                try? text.write(toFile: out, atomically: true, encoding: .utf8)
                NSApp.terminate(nil)
            }
        }
    }

    // agent 重開時網頁連不上：過一下重新載入
    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in
            guard let self else { return }
            if let u = ensureAgent(), let url = URL(string: u) { self.webView.load(URLRequest(url: url)) }
        }
    }

    // <input type=file>
    func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping ([URL]?) -> Void) {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = parameters.allowsMultipleSelection
        panel.canChooseDirectories = parameters.allowsDirectories
        panel.beginSheetModal(for: window) { r in completionHandler(r == .OK ? panel.urls : nil) }
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.beginSheetModal(for: window) { _ in completionHandler() }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.addButton(withTitle: "確定")
        a.addButton(withTitle: "取消")
        a.beginSheetModal(for: window) { r in completionHandler(r == .alertFirstButtonReturn) }
    }

    // 網頁經 bridge.js 轉來的系統通知（window.webkit.messageHandlers.myriagent）
    func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
        guard let body = message.body as? [String: Any], let kind = body["kind"] as? String else { return }
        switch kind {
        case "notify":
            let c = UNMutableNotificationContent()
            c.title = body["title"] as? String ?? "Clode Myriagent"
            c.body = body["body"] as? String ?? ""
            if let id = body["conversationId"] as? String { c.userInfo = ["conversationId": id] }
            UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: UUID().uuidString, content: c, trigger: nil))
        case "badge":
            let n = body["count"] as? Int ?? 0
            NSApp.dockTile.badgeLabel = n > 0 ? String(n) : nil
        case "attention":
            NSApp.requestUserAttention(.informationalRequest)
        default:
            break
        }
    }

    func show() {
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    var main: WindowController?

    func applicationDidFinishLaunching(_ notification: Notification) {
        buildMenu()
        UNUserNotificationCenter.current().delegate = self
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { _, _ in }
        DispatchQueue.global().async {
            let url = ensureAgent()
            DispatchQueue.main.async { self.open(url) }
        }
    }

    func open(_ url: String?) {
        guard let u = url, let parsed = URL(string: u) else {
            let a = NSAlert()
            a.messageText = "Clode Myriagent 的背景程式沒有啟動"
            a.informativeText = "請查看 ~/Library/Application Support/Myriagent/agent.out.log"
            a.runModal()
            NSApp.terminate(nil)
            return
        }
        main = WindowController(url: parsed)
        main?.show()
    }

    // 關掉視窗不結束：agent 照樣在背景跑；按 Dock 圖示再開回來
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        main?.show()
        return true
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }

    // 點通知：開視窗、切到那個對話
    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                withCompletionHandler completionHandler: @escaping () -> Void) {
        main?.show()
        if let id = response.notification.request.content.userInfo["conversationId"] as? String,
           let data = try? JSONSerialization.data(withJSONObject: [id]), let arr = String(data: data, encoding: .utf8) {
            main?.webView.evaluateJavaScript("window.myriagentFocus && window.myriagentFocus(\(arr)[0])")
        }
        completionHandler()
    }

    func buildMenu() {
        let menu = NSMenu()
        let appItem = NSMenuItem()
        let app = NSMenu()
        app.addItem(withTitle: "關於 Clode Myriagent", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "隱藏 Clode Myriagent", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        app.addItem(.separator())
        app.addItem(withTitle: "結束視窗程式（背景繼續執行）", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = app
        menu.addItem(appItem)

        let editItem = NSMenuItem()
        let edit = NSMenu(title: "編輯")
        edit.addItem(withTitle: "還原", action: Selector(("undo:")), keyEquivalent: "z")
        edit.addItem(withTitle: "重做", action: Selector(("redo:")), keyEquivalent: "Z")
        edit.addItem(.separator())
        edit.addItem(withTitle: "剪下", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "拷貝", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "貼上", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "全選", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit
        menu.addItem(editItem)

        let viewItem = NSMenuItem()
        let view = NSMenu(title: "顯示方式")
        view.addItem(withTitle: "重新載入", action: #selector(AppDelegate.reload), keyEquivalent: "r")
        view.addItem(withTitle: "全螢幕", action: #selector(NSWindow.toggleFullScreen(_:)), keyEquivalent: "f").keyEquivalentModifierMask = [.command, .control]
        viewItem.submenu = view
        menu.addItem(viewItem)

        let winItem = NSMenuItem()
        let win = NSMenu(title: "視窗")
        win.addItem(withTitle: "縮到最小", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        win.addItem(withTitle: "關閉視窗", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        winItem.submenu = win
        menu.addItem(winItem)
        NSApp.windowsMenu = win
        NSApp.mainMenu = menu
    }

    @objc func reload() { main?.webView.reload() }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
