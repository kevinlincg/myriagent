using System.Text;
using AppKit;
using CoreGraphics;
using Foundation;
using Myriagent.Agents;
using Myriagent.Core;
using Myriagent.Files;
using Myriagent.Hub;
using Myriagent.Sync;
using Newtonsoft.Json;
using Newtonsoft.Json.Linq;
using WebKit;

namespace Myriagent.UI;

/// <summary>AgentWebView 所在的視窗要提供的東西</summary>
internal interface IWebViewOwner
{
    /// <summary>視窗已經關掉（晚到的回應、訊息都不再處理）</summary>
    bool IsClosed { get; }

    /// <summary>視窗標題跟著網頁標題（目前對話的標題）</summary>
    void SetTitle(string title);
}

/// <summary>
/// 一個視窗裡的介面網頁，macOS 版（WKWebView；Windows 版是 WebView2，見 src/Myriagent/UI/AgentWebView.cs，和平台無關的部分在 AgentWebView.Common.cs）：
/// 提供嵌入的介面網頁（https://myriagent.local/，含 CSP）、/media（本機圖片影片，支援 Range）、/thumb（系統的縮圖，背景產生不卡主執行緒）、
/// /m/ 與 /t/（複本的圖片影片與縮圖，經伺服器向擁有者電腦要）、把網頁指令轉給中樞、攔下站外導覽改用預設瀏覽器開。
/// 所有成員都在主執行緒呼叫
/// </summary>
internal partial class AgentWebView : IAgentView, IDisposable
{
    /// <summary>回應內容一次交給 WKWebView 多少（影片可能很大，不整個讀進記憶體）</summary>
    private const int StreamChunkBytes = 256 * 1024;

    /// <summary>登記給網頁的磁碟檔案最多留幾筆（代號 → 路徑；超過就丟最舊的）</summary>
    private const int MaxKnownFiles = 2000;

    private readonly IWebViewOwner owner;
    private readonly IAgentHost host;
    private readonly AgentHub hub;
    private readonly ThumbnailService thumbnails;
    private readonly MacWebView webView;
    private readonly WebViewBridge bridge;

    /// <summary>WKWebView 還在等回應的請求（以原生物件的位址認）：它說停止之後再對那個請求回應會讓整個 app 當掉</summary>
    private readonly HashSet<nint> activeTasks = new();
    private readonly Dictionary<string, string> knownFiles = new(StringComparer.Ordinal);
    private readonly Queue<string> knownFileOrder = new();
    private IDisposable? titleObserver;
    private nint lastPasteboardChange = -1;
    private bool attached;
    private bool disposed;
    private bool darkTheme = true; // 送出 index.html 時寫進 <html data-theme>（見 WebAssetRules.ApplyInitialTheme）

    public AgentWebView(IWebViewOwner owner, IAgentHost host, AgentHub hub, ThumbnailService thumbnails, bool isPopout)
    {
        this.owner = owner;
        this.host = host;
        this.hub = hub;
        this.thumbnails = thumbnails;
        IsPopout = isPopout;
        if (!WebKitHttpsScheme.Install())
            throw new InvalidOperationException("這一版 macOS 的 WKWebView 接不上介面的網址（https://myriagent.local）");

        bridge = new WebViewBridge(this);
        var configuration = new WKWebViewConfiguration();
        configuration.SetUrlSchemeHandler(bridge, "https");
        configuration.UserContentController.AddUserScript(new WKUserScript(new NSString(WebBridge.Script), WKUserScriptInjectionTime.AtDocumentStart, true));
        configuration.UserContentController.AddScriptMessageHandler(bridge, WebBridge.HandlerName);
        bool devTools = System.Diagnostics.Debugger.IsAttached || Environment.GetEnvironmentVariable("MYRIAGENT_AGENT_DEVTOOLS") == "1";
        if (devTools)
            TrySetValue(configuration.Preferences, "developerExtrasEnabled", true);

        webView = new MacWebView(new CGRect(0, 0, 1200, 800), configuration)
        {
            AutoresizingMask = NSViewResizingMask.WidthSizable | NSViewResizingMask.HeightSizable,
            NavigationDelegate = bridge,
            UIDelegate = bridge,
        };
        webView.FilesIncoming = RegisterFiles;
        if (devTools && OperatingSystem.IsMacOSVersionAtLeast(13, 3))
            webView.Inspectable = true;
        // 載入前不畫白底：露出視窗的底色（跟主題），第一個影格前不會閃一下白
        TrySetValue(webView, "drawsBackground", false);
        titleObserver = webView.AddObserver("title", NSKeyValueObservingOptions.New, _ => OnTitleChanged());
    }

    public string Id { get; } = Guid.NewGuid().ToString("N");

    public bool IsPopout { get; }

    /// <summary>載入完成（網頁送出 ready 之前）時觸發</summary>
    public event Action? Loaded;

    /// <summary>放進視窗的 view</summary>
    public NSView View => webView;

    private static void TrySetValue(NSObject target, string key, bool value)
    {
        try
        {
            target.SetValueForKey(NSNumber.FromBoolean(value), new NSString(key));
        }
        catch (Exception ex)
        {
            // 這兩個鍵不在公開的 API 裡：哪一版系統拿掉了也只是少了那個效果
            Log.Warn($"WKWebView 不認得 {key}：{ex.Message}");
        }
    }

    /// <summary>載入介面（可在視窗顯示前呼叫）</summary>
    public void Initialize(string query, bool dark)
    {
        darkTheme = dark;
        hub.AttachView(this);
        attached = true;
        webView.LoadRequest(new NSUrlRequest(new NSUrl($"{AgentMarkdownRenderer.Origin}/index.html{query}")));
    }

    /// <summary>主題的深淺（下一次送出 index.html 時用；底色是視窗的）</summary>
    public void SetBackground(bool dark) => darkTheme = dark;

    public void Detach()
    {
        if (!attached)
            return;
        attached = false;
        hub.DetachView(Id);
    }

    /// <summary>視窗關掉時：不再收送，放開 WKWebView（訊息處理常式被 WKWebView 的設定抓著，不移除會整個留在記憶體）</summary>
    public void Dispose()
    {
        if (disposed)
            return;
        disposed = true;
        Detach();
        titleObserver?.Dispose();
        titleObserver = null;
        activeTasks.Clear();
        try
        {
            webView.StopLoading();
            webView.Configuration.UserContentController.RemoveScriptMessageHandler(WebBridge.HandlerName);
            webView.NavigationDelegate = null!;
            webView.UIDelegate = null!;
        }
        catch (Exception ex)
        {
            Log.Warn($"關閉介面元件：{ex.Message}");
        }
    }

    /// <summary>把鍵盤焦點交給網頁（視窗變成作用中時）</summary>
    public void Focus() => webView.Window?.MakeFirstResponder(webView);

    public void Reload()
    {
        if (!disposed)
            webView.Reload();
    }

    // ───────────────────────────── IAgentView ─────────────────────────────

    public void PostToWeb(JObject message)
    {
        if (disposed || owner.IsClosed)
            return;
        // 以 JSON.parse 還原：直接把 JSON 當成 JavaScript 的物件常值，「__proto__」這種鍵的意思會不一樣
        // 網頁還沒載入（橋還沒注入）時送的訊息直接丟掉，和 WebView2 一樣：網頁載入後送 ready，中樞會把完整的狀態再送一次
        var script = "window.chrome&&window.chrome.webview&&window.chrome.webview.__deliver&&window.chrome.webview.__deliver(JSON.parse("
                     + JsonConvert.ToString(message.ToString(Formatting.None)) + "));0";
        Evaluate(script);
    }

    /// <summary>在網頁裡執行一段腳本並拿回字串結果（冒煙測試用）；done 的第二個參數是錯誤訊息（沒有錯誤是 null）</summary>
    public void EvaluateString(string script, Action<string?, string?> done)
    {
        try
        {
            webView.EvaluateJavaScript(script, (result, error) => done(result?.ToString(), error?.LocalizedDescription));
        }
        catch (Exception ex)
        {
            done(null, ex.Message);
        }
    }

    /// <summary>把網頁目前的畫面存成 PNG（冒煙測試用；不必在螢幕上看得到）；done 的參數是錯誤訊息（成功是 null）</summary>
    public void SavePng(string path, Action<string?> done)
    {
        try
        {
            webView.TakeSnapshot(null!, (image, error) =>
            {
                try
                {
                    if (image == null || error != null)
                    {
                        done(error?.LocalizedDescription ?? "沒有拿到畫面");
                        return;
                    }
                    using var tiff = image.AsTiff();
                    using var bitmap = new NSBitmapImageRep(tiff!);
                    using var png = bitmap.RepresentationUsingTypeProperties(NSBitmapImageFileType.Png);
                    File.WriteAllBytes(path, png.ToArray());
                    done(null);
                }
                catch (Exception ex)
                {
                    done(ex.Message);
                }
            });
        }
        catch (Exception ex)
        {
            done(ex.Message);
        }
    }

    private void Evaluate(string script)
    {
        try
        {
            webView.EvaluateJavaScript(script, (_, error) =>
            {
                if (error != null && !disposed)
                    Log.Warn($"送訊息給網頁失敗：{error.LocalizedDescription}");
            });
        }
        catch (Exception ex)
        {
            Log.Warn($"送訊息給網頁失敗：{ex.Message}");
        }
    }

    // ───────────────────────────── 檔案的路徑 ─────────────────────────────

    /// <summary>
    /// 檔案要進到網頁之前（拖進視窗、貼上、選檔）先登記：網頁之後以 postMessageWithAdditionalObjects 送回代號，這裡換成路徑（見 WebBridge）。
    /// 不存在或讀不到的略過
    /// </summary>
    public void RegisterFiles(IReadOnlyList<string> paths)
    {
        if (disposed || paths.Count == 0)
            return;
        var entries = new JArray();
        foreach (var path in paths)
        {
            try
            {
                var info = new FileInfo(path);
                if (!info.Exists)
                    continue;
                var token = Guid.NewGuid().ToString("N");
                knownFiles[token] = info.FullName;
                knownFileOrder.Enqueue(token);
                entries.Add(new JObject
                {
                    ["token"] = token,
                    ["name"] = info.Name,
                    ["size"] = info.Length,
                    ["modified"] = new DateTimeOffset(info.LastWriteTimeUtc).ToUnixTimeMilliseconds(),
                });
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
            {
            }
        }
        while (knownFileOrder.Count > MaxKnownFiles)
            knownFiles.Remove(knownFileOrder.Dequeue());
        if (entries.Count > 0)
            Evaluate("window.chrome&&window.chrome.webview&&window.chrome.webview.__files&&window.chrome.webview.__files(" + entries.ToString(Formatting.None) + ");0");
    }

    /// <summary>剪貼簿裡有檔案（在 Finder 複製的）而且和上次看的不一樣就登記：之後貼上時網頁才找得到路徑。視窗變成作用中、按貼上之前呼叫</summary>
    public void RegisterPasteboardFiles()
    {
        if (disposed)
            return;
        try
        {
            var pasteboard = NSPasteboard.GeneralPasteboard;
            if (pasteboard.ChangeCount == lastPasteboardChange)
                return;
            lastPasteboardChange = pasteboard.ChangeCount;
            RegisterFiles(MacPasteboard.FilePaths(pasteboard));
        }
        catch (Exception ex)
        {
            Log.Warn($"讀剪貼簿的檔案失敗：{ex.Message}");
        }
    }

    // ───────────────────────────── 資源 ─────────────────────────────

    private void StartTask(IWKUrlSchemeTask task)
    {
        var key = task.Handle.Handle;
        activeTasks.Add(key);
        var url = task.Request.Url;
        if (disposed || !Uri.TryCreate(url?.AbsoluteString, UriKind.Absolute, out var uri)
            || !uri.Host.Equals(AgentMarkdownRenderer.Host, StringComparison.OrdinalIgnoreCase))
        {
            Respond(task, NotFound());
            return;
        }

        // 複本（其他電腦的對話）的 /m/、/t/ 先比對前綴（本機的 /media、/thumb、/open 照舊）
        var replica = ReplicaMediaRouteRules.Match(uri);
        if (replica.Kind != ReplicaRouteKind.None)
        {
            var range = replica is { Kind: ReplicaRouteKind.Valid, Purpose: MediaPurposes.Media } ? RangeHeaderOf(task) : null;
            if (RouteReplica(replica, range, hub.ReplicaMediaFor) is { } work)
                _ = ServeInBackgroundAsync(task, "取複本的媒體", work, ReplicaFetchTimeout);
            else
                Respond(task, NotFound());
            return;
        }

        switch (uri.AbsolutePath.ToLowerInvariant())
        {
            case "/media":
                ServeMedia(task, GetQueryValue(uri, "path"));
                return;
            case "/thumb":
                ServeThumbnail(task, GetQueryValue(uri, "path"), int.TryParse(GetQueryValue(uri, "size"), out var size) ? size : 160);
                return;
            case "/open":
                Respond(task, NotFound());
                return;
        }

        var name = uri.AbsolutePath.TrimStart('/');
        if (name.Length == 0)
            name = "index.html";
        var contentType = WebAssets.ContentType(name);
        var stream = contentType == null || name.Contains('/') ? null : WebAssets.Open(name);
        if (stream == null)
        {
            Respond(task, NotFound());
            return;
        }

        var headers = $"Content-Type: {contentType}\r\nCache-Control: no-store\r\nX-Content-Type-Options: nosniff";
        if (name.Equals("index.html", StringComparison.OrdinalIgnoreCase))
        {
            headers += $"\r\nContent-Security-Policy: {ContentSecurityPolicy}";
            stream = WithInitialTheme(stream);
        }
        Respond(task, new PreparedResponse(stream, 200, "OK", headers));
    }

    private void StopTask(IWKUrlSchemeTask task) => activeTasks.Remove(task.Handle.Handle);

    /// <summary>index.html 的 &lt;html data-theme&gt; 換成目前主題：init 到達前的第一個影格就是正確的深淺色</summary>
    private MemoryStream WithInitialTheme(Stream source)
    {
        using (source)
        using (var reader = new StreamReader(source, Encoding.UTF8))
            return new MemoryStream(Encoding.UTF8.GetBytes(WebAssetRules.ApplyInitialTheme(reader.ReadToEnd(), darkTheme)));
    }

    /// <summary>
    /// 本機圖片／影片：只給中樞登記過、本機磁碟上、白名單格式的檔案；有 Range 標頭就回 206 部分內容。
    /// 開檔在背景執行緒：路徑也可能在掛載的網路磁碟上，離線時開檔會卡很久，不能卡住主執行緒（所有視窗會一起凍結）
    /// </summary>
    private void ServeMedia(IWKUrlSchemeTask task, string? path)
    {
        var contentType = path == null ? null : AgentAttachmentRules.GetMediaContentType(path);
        if (path == null || contentType == null || !AgentAttachmentRules.IsLocalDrivePath(path) || !hub.IsMediaAllowed(path))
        {
            Respond(task, NotFound());
            return;
        }

        var rangeHeader = RangeHeaderOf(task);
        _ = ServeInBackgroundAsync(task, "讀取媒體檔", () => Task.FromResult(PrepareMedia(path, contentType, rangeHeader)), MediaOpenTimeout);
    }

    private static string? RangeHeaderOf(IWKUrlSchemeTask task)
    {
        try
        {
            return task.Request.Headers?["Range"]?.ToString();
        }
        catch (Exception)
        {
            return null;
        }
    }

    /// <summary>縮圖在背景產生，完成後回主執行緒回應</summary>
    private void ServeThumbnail(IWKUrlSchemeTask task, string? path, int size)
    {
        if (path == null || !AgentAttachmentRules.IsLocalDrivePath(path) || !hub.IsThumbAllowed(path))
        {
            Respond(task, NotFound());
            return;
        }

        _ = ServeInBackgroundAsync(task, "縮圖", async () => await thumbnails.GetAsync(path, size) is { } bytes
            ? new PreparedResponse(new MemoryStream(bytes), 200, "OK", "Content-Type: image/png\r\nCache-Control: max-age=300\r\nX-Content-Type-Options: nosniff")
            : null, timeout: null);
    }

    /// <summary>
    /// 回應內容在背景執行緒準備，好了再回到主執行緒交給 WKWebView（await 之後回到主執行緒）。
    /// timeout：準備太久就先回 404，晚到的內容直接釋放；null＝等到好為止
    /// </summary>
    private async Task ServeInBackgroundAsync(IWKUrlSchemeTask task, string what, Func<Task<PreparedResponse?>> prepare, TimeSpan? timeout)
    {
        var work = Task.Run(prepare);
        PreparedResponse? prepared = null;
        try
        {
            prepared = timeout is { } limit ? await work.WaitAsync(limit) : await work;
        }
        catch (TimeoutException) when (timeout != null)
        {
            Log.Warn($"{what}逾時（超過 {timeout.Value.TotalSeconds:0} 秒，網路磁碟可能離線）");
            _ = work.ContinueWith(t => t.Result?.Content?.Dispose(), CancellationToken.None, TaskContinuationOptions.OnlyOnRanToCompletion, TaskScheduler.Default);
        }
        catch (Exception ex)
        {
            Log.Warn($"{what}失敗：{ex.Message}");
        }

        // 這裡不一定在主執行緒（沒有同步內容時 await 之後留在背景）：一律排回主執行緒才碰 WKWebView 的物件
        var response = prepared ?? NotFound();
        host.BeginInvoke(() => Respond(task, response));
    }

    private static PreparedResponse NotFound() => new(null, 404, "Not Found", "Content-Type: text/plain");

    /// <summary>把準備好的回應交給 WKWebView（主執行緒）：先送標頭，內容分塊送。請求已經被停止就只釋放內容</summary>
    private void Respond(IWKUrlSchemeTask task, PreparedResponse response)
    {
        var key = task.Handle.Handle;
        if (disposed || !activeTasks.Contains(key))
        {
            response.Content?.Dispose();
            return;
        }

        try
        {
            using var headers = HeaderDictionary(response.Headers);
            using var http = new NSHttpUrlResponse(task.Request.Url, response.StatusCode, "HTTP/1.1", headers);
            task.DidReceiveResponse(http);
        }
        catch (Exception ex)
        {
            activeTasks.Remove(key);
            response.Content?.Dispose();
            Log.Warn($"回應介面的請求失敗：{ex.Message}");
            return;
        }

        if (response.Content == null)
        {
            Finish(task, key);
            return;
        }
        _ = PumpAsync(task, key, response.Content);
    }

    /// <summary>"名稱: 值\r\n名稱: 值" 變成 NSDictionary（和 Windows 版用同一種寫法的標頭字串）</summary>
    private static NSDictionary HeaderDictionary(string headers)
    {
        var names = new List<object>();
        var values = new List<object>();
        foreach (var line in headers.Split("\r\n", StringSplitOptions.RemoveEmptyEntries))
        {
            int colon = line.IndexOf(':');
            if (colon <= 0)
                continue;
            names.Add(line[..colon].Trim());
            values.Add(line[(colon + 1)..].Trim());
        }
        return NSDictionary.FromObjectsAndKeys(values.ToArray(), names.ToArray());
    }

    /// <summary>
    /// 內容分塊交給 WKWebView：背景讀檔、一塊一塊排回主執行緒送，送完一塊才讀下一塊（大的影片不會整個堆在記憶體）。
    /// WKWebView 中途停止這個請求（使用者拖動影片進度、關掉視窗）就不再送
    /// </summary>
    private async Task PumpAsync(IWKUrlSchemeTask task, nint key, Stream content)
    {
        try
        {
            await using (content)
            {
                var buffer = new byte[StreamChunkBytes];
                while (true)
                {
                    int read = await content.ReadAsync(buffer).ConfigureAwait(false);
                    if (read <= 0)
                        break;
                    if (!await OnMainAsync(() => SendChunk(task, key, buffer, read)).ConfigureAwait(false))
                        return;
                }
            }
            host.BeginInvoke(() => Finish(task, key));
        }
        catch (Exception ex)
        {
            Log.Warn($"傳送介面的資源失敗：{ex.Message}");
            host.BeginInvoke(() => Fail(task, key));
        }
    }

    /// <summary>在主執行緒執行並等結果</summary>
    private Task<bool> OnMainAsync(Func<bool> action)
    {
        var done = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        host.BeginInvoke(() =>
        {
            try
            {
                done.TrySetResult(action());
            }
            catch (Exception ex)
            {
                done.TrySetException(ex);
            }
        });
        return done.Task;
    }

    private unsafe bool SendChunk(IWKUrlSchemeTask task, nint key, byte[] buffer, int length)
    {
        if (disposed || !activeTasks.Contains(key))
            return false;
        fixed (byte* bytes = buffer)
        {
            using var data = NSData.FromBytes((nint)bytes, (nuint)length);
            task.DidReceiveData(data);
        }
        return true;
    }

    private void Finish(IWKUrlSchemeTask task, nint key)
    {
        if (disposed || !activeTasks.Remove(key))
            return;
        try
        {
            task.DidFinish();
        }
        catch (Exception ex)
        {
            Log.Warn($"結束介面的請求失敗：{ex.Message}");
        }
    }

    private void Fail(IWKUrlSchemeTask task, nint key)
    {
        if (disposed || !activeTasks.Remove(key))
            return;
        try
        {
            using var error = new NSError(NSError.NSUrlErrorDomain, -1);
            task.DidFailWithError(error);
        }
        catch (Exception ex)
        {
            Log.Warn($"結束介面的請求失敗：{ex.Message}");
        }
    }

    // ───────────────────────────── 訊息與導覽 ─────────────────────────────

    private void OnScriptMessage(WKScriptMessage message)
    {
        if (disposed)
            return;
        var frame = message.FrameInfo;
        if (frame == null || !frame.MainFrame || !string.Equals(frame.SecurityOrigin?.Host, AgentMarkdownRenderer.Host, StringComparison.OrdinalIgnoreCase))
            return;
        if (message.Body is not NSString body)
            return;

        JObject envelope;
        try
        {
            envelope = JObject.Parse(body.ToString());
        }
        catch (JsonException)
        {
            return;
        }
        if (envelope["message"] is not JObject command)
            return;

        // 拖放、貼上的檔案：網頁送回登記時拿到的代號，這裡換成本機路徑（不認得的代號略過）
        var paths = new List<string>();
        if (envelope["files"] is JArray tokens)
        {
            foreach (var token in tokens)
            {
                if (token.Type == JTokenType.String && knownFiles.TryGetValue((string)token!, out var path))
                    paths.Add(path);
            }
        }

        // 延到這次事件處理結束後才執行：指令可能開選檔對話框（巢狀的事件迴圈）
        host.BeginInvoke(() => hub.HandleCommand(this, command, paths));
    }

    /// <summary>主框架只能留在介面的首頁；其他導覽（漏網的連結、沒被網頁接住的拖放）一律取消，交給中樞判斷要不要用預設程式開</summary>
    private WKNavigationActionPolicy DecideNavigation(WKNavigationAction action)
    {
        var href = action.Request.Url?.AbsoluteString;
        if (string.IsNullOrEmpty(href))
            return WKNavigationActionPolicy.Cancel;
        // 子框架（介面沒有用）與 about:blank 不管
        if (action.TargetFrame is { MainFrame: false })
            return WKNavigationActionPolicy.Allow;
        var index = $"{AgentMarkdownRenderer.Origin}/index.html";
        if (href.StartsWith(index, StringComparison.OrdinalIgnoreCase) || href.Equals($"{AgentMarkdownRenderer.Origin}/", StringComparison.OrdinalIgnoreCase))
            return WKNavigationActionPolicy.Allow;
        OpenLink(href);
        return WKNavigationActionPolicy.Cancel;
    }

    private void OpenLink(string? href)
    {
        if (string.IsNullOrEmpty(href) || href.StartsWith("file:", StringComparison.OrdinalIgnoreCase) || disposed)
            return;
        host.BeginInvoke(() => hub.HandleCommand(this, new JObject { ["cmd"] = "openLink", ["href"] = href }));
    }

    /// <summary>網頁內容的程序結束（當掉、記憶體不足被系統收掉）：重新載入，網頁依存檔的版面還原分頁（對話狀態都在中樞，不會遺失）</summary>
    private void OnContentProcessTerminated()
    {
        if (disposed || owner.IsClosed)
            return;
        Log.Warn("WKWebView 的網頁內容程序結束，重新載入介面");
        activeTasks.Clear();
        webView.Reload();
    }

    /// <summary>視窗標題跟著網頁標題（目前對話的標題）：多個視窗在 Mission Control 與「視窗」選單才分得出來</summary>
    private void OnTitleChanged()
    {
        if (disposed || owner.IsClosed)
            return;
        owner.SetTitle(ShellRules.WindowTitle(webView.Title));
    }

    /// <summary>
    /// WKWebView 的委派與處理常式：一個 Objective-C 物件接下資源請求、網頁訊息、導覽與介面的詢問，轉給 AgentWebView。
    /// 這些方法都由 WKWebView 在主執行緒呼叫
    /// </summary>
    private sealed class WebViewBridge(AgentWebView view) : NSObject, IWKUrlSchemeHandler, IWKScriptMessageHandler, IWKNavigationDelegate, IWKUIDelegate
    {
        [Export("webView:startURLSchemeTask:")]
        public void StartUrlSchemeTask(WKWebView webView, IWKUrlSchemeTask urlSchemeTask) => view.StartTask(urlSchemeTask);

        [Export("webView:stopURLSchemeTask:")]
        public void StopUrlSchemeTask(WKWebView webView, IWKUrlSchemeTask urlSchemeTask) => view.StopTask(urlSchemeTask);

        [Export("userContentController:didReceiveScriptMessage:")]
        public void DidReceiveScriptMessage(WKUserContentController userContentController, WKScriptMessage message) => view.OnScriptMessage(message);

        [Export("webView:decidePolicyForNavigationAction:decisionHandler:")]
        public void DecidePolicy(WKWebView webView, WKNavigationAction navigationAction, Action<WKNavigationActionPolicy> decisionHandler)
            => decisionHandler(view.DecideNavigation(navigationAction));

        [Export("webView:didFinishNavigation:")]
        public void DidFinishNavigation(WKWebView webView, WKNavigation navigation) => view.Loaded?.Invoke();

        [Export("webViewWebContentProcessDidTerminate:")]
        public void ContentProcessDidTerminate(WKWebView webView) => view.OnContentProcessTerminated();

        /// <summary>target=_blank 的連結、window.open：不開新的 WKWebView，交給中樞用預設瀏覽器開</summary>
        [Export("webView:createWebViewWithConfiguration:forNavigationAction:windowFeatures:")]
        public WKWebView? CreateWebView(WKWebView webView, WKWebViewConfiguration configuration, WKNavigationAction navigationAction, WKWindowFeatures windowFeatures)
        {
            view.OpenLink(navigationAction.Request.Url?.AbsoluteString);
            return null;
        }

        /// <summary>輸入區的語音訊息（錄音）：本頁要用麥克風時直接允許，不跳 WKWebView 的詢問（系統自己的麥克風權限照問一次）；其他一律不給</summary>
        [Export("webView:requestMediaCapturePermissionForOrigin:initiatedByFrame:type:decisionHandler:")]
        public void RequestMediaCapturePermission(WKWebView webView, WKSecurityOrigin origin, WKFrameInfo frame, WKMediaCaptureType type,
            Action<WKPermissionDecision> decisionHandler)
        {
            bool appOrigin = string.Equals(origin.Protocol, "https", StringComparison.OrdinalIgnoreCase)
                             && string.Equals(origin.Host, AgentMarkdownRenderer.Host, StringComparison.OrdinalIgnoreCase);
            decisionHandler(appOrigin && type == WKMediaCaptureType.Microphone ? WKPermissionDecision.Grant : WKPermissionDecision.Deny);
        }

        [Export("webView:runJavaScriptAlertPanelWithMessage:initiatedByFrame:completionHandler:")]
        public void RunJavaScriptAlertPanel(WKWebView webView, string message, WKFrameInfo frame, Action completionHandler)
        {
            MacDialogs.Show(webView.Window, message, ShellRules.AppTitle, NSAlertStyle.Informational, "好");
            completionHandler();
        }

        [Export("webView:runJavaScriptConfirmPanelWithMessage:initiatedByFrame:completionHandler:")]
        public void RunJavaScriptConfirmPanel(WKWebView webView, string message, WKFrameInfo frame, Action<bool> completionHandler)
            => completionHandler(MacDialogs.Show(webView.Window, message, ShellRules.AppTitle, NSAlertStyle.Informational, "好", "取消") == 0);

        /// <summary>網頁的 &lt;input type=file&gt;：開系統的選檔對話框，選到的檔案先登記路徑再交給網頁</summary>
        [Export("webView:runOpenPanelWithParameters:initiatedByFrame:completionHandler:")]
        public void RunOpenPanel(WKWebView webView, WKOpenPanelParameters parameters, WKFrameInfo frame, Action<NSUrl[]> completionHandler)
        {
            var panel = NSOpenPanel.OpenPanel;
            panel.CanChooseFiles = true;
            panel.CanChooseDirectories = false;
            panel.AllowsMultipleSelection = parameters.AllowsMultipleSelection;
            if (panel.RunModal() == (nint)(long)NSModalResponse.OK && panel.Urls is { Length: > 0 } urls)
            {
                view.RegisterFiles(urls.Select(u => u.Path).Where(p => !string.IsNullOrEmpty(p)).Select(p => p!).ToList());
                completionHandler(urls);
            }
            else
            {
                completionHandler(null!);
            }
        }
    }
}

/// <summary>
/// 介面用的 WKWebView：檔案拖進視窗時先讓宿主登記路徑（見 WebBridge）、右鍵選單拿掉會離開介面的項目。
/// </summary>
[Register("MyriagentWebView")]
internal sealed class MacWebView : WKWebView
{
    /// <summary>右鍵選單裡不留的項目：重新載入會丟掉還沒送出的草稿，上一頁、開新視窗、下載在這個介面沒有意義</summary>
    private static readonly HashSet<string> HiddenMenuItems = new(StringComparer.Ordinal)
    {
        "WKMenuItemIdentifierReload", "WKMenuItemIdentifierGoBack", "WKMenuItemIdentifierGoForward",
        "WKMenuItemIdentifierOpenLinkInNewWindow", "WKMenuItemIdentifierOpenImageInNewWindow", "WKMenuItemIdentifierOpenMediaInNewWindow",
        "WKMenuItemIdentifierOpenFrameInNewWindow", "WKMenuItemIdentifierDownloadLinkedFile", "WKMenuItemIdentifierDownloadImage",
        "WKMenuItemIdentifierDownloadMedia",
    };

    private nint lastDragSequence = -1;

    public MacWebView(CGRect frame, WKWebViewConfiguration configuration)
        : base(frame, configuration)
    {
    }

    /// <summary>有檔案要進到網頁了（拖進視窗）：參數是它們的路徑</summary>
    public Action<IReadOnlyList<string>>? FilesIncoming { get; set; }

    public override NSDragOperation DraggingEntered(INSDraggingInfo sender)
    {
        NoteDraggedFiles(sender);
        return base.DraggingEntered(sender);
    }

    public override bool PerformDragOperation(INSDraggingInfo sender)
    {
        NoteDraggedFiles(sender);
        return base.PerformDragOperation(sender);
    }

    /// <summary>同一次拖曳只登記一次（進入視窗時就登記：放開之前網頁那邊已經收到，找得到路徑）</summary>
    private void NoteDraggedFiles(INSDraggingInfo sender)
    {
        try
        {
            if (sender.DraggingSequenceNumber == lastDragSequence)
                return;
            lastDragSequence = sender.DraggingSequenceNumber;
            var paths = MacPasteboard.FilePaths(sender.DraggingPasteboard);
            if (paths.Count > 0)
                FilesIncoming?.Invoke(paths);
        }
        catch (Exception ex)
        {
            Log.Warn($"讀拖曳的檔案失敗：{ex.Message}");
        }
    }

    public override void WillOpenMenu(NSMenu menu, NSEvent theEvent)
    {
        foreach (var item in menu.Items)
        {
            if (item.Identifier is { } identifier && HiddenMenuItems.Contains(identifier))
                item.Hidden = true;
        }
        base.WillOpenMenu(menu, theEvent);
    }
}

/// <summary>完整視窗（主視窗或另外開的視窗，多視窗）的介面網頁：版面以 WindowKey 存，分頁可以在視窗之間拖曳搬移</summary>
internal sealed class MainWebView : AgentWebView, IWindowView
{
    public MainWebView(IWebViewOwner owner, IAgentHost host, AgentHub hub, ThumbnailService thumbnails, string windowKey, bool isPrimaryWindow)
        : base(owner, host, hub, thumbnails, isPopout: false)
    {
        WindowKey = windowKey;
        IsPrimaryWindow = isPrimaryWindow;
    }

    public string WindowKey { get; }

    public bool IsPrimaryWindow { get; }
}

/// <summary>彈出視窗的介面網頁（只顯示一個對話）</summary>
internal sealed class PopoutWebView : AgentWebView, IPopoutView
{
    public PopoutWebView(IWebViewOwner owner, IAgentHost host, AgentHub hub, ThumbnailService thumbnails, string conversationId)
        : base(owner, host, hub, thumbnails, isPopout: true)
    {
        ConversationId = conversationId;
    }

    public string ConversationId { get; }
}

/// <summary>從剪貼簿、拖曳的內容讀出檔案的路徑</summary>
internal static class MacPasteboard
{
    private const string FileUrlType = "public.file-url";

    public static IReadOnlyList<string> FilePaths(NSPasteboard? pasteboard)
    {
        var result = new List<string>();
        if (pasteboard?.PasteboardItems is not { } items)
            return result;
        foreach (var item in items)
        {
            var text = item.GetStringForType(FileUrlType);
            if (string.IsNullOrEmpty(text))
                continue;
            using var url = NSUrl.FromString(text);
            if (url is not { IsFileUrl: true })
                continue;
            // 檔案參照網址（file:///.file/id=…）先換成一般的路徑
            var path = (url.FilePathUrl ?? url).Path;
            if (!string.IsNullOrEmpty(path))
                result.Add(path);
        }
        return result;
    }
}
