using AppKit;
using CoreGraphics;
using Foundation;
using Myriagent.Core;
using Myriagent.Hub;

namespace Myriagent.UI;

/// <summary>
/// app 的一個視窗（NSWindow＋裡面的介面網頁）：主視窗、另外開的完整視窗、只顯示一個對話的彈出視窗共用的部分。
/// 位置與大小對外一律用左上原點的矩形（見 MacScreens），和 settings.json、共用的規則一致。主執行緒
/// </summary>
internal abstract class AppWindow : IWebViewOwner
{
    private static readonly NSColor DarkBackground = NSColor.FromCalibratedRgb((byte)0x18, (byte)0x18, (byte)0x1B);
    private static readonly NSColor LightBackground = NSColor.FromCalibratedRgb((byte)0xF7, (byte)0xF7, (byte)0xF8);

    protected readonly AppController App;
    private readonly WindowEvents events;

    protected AppWindow(AppController app, Rectangle bounds, Size minimumSize)
    {
        App = app;
        Window = new NSWindow(MacScreens.FromTopLeft(bounds),
            NSWindowStyle.Titled | NSWindowStyle.Closable | NSWindowStyle.Miniaturizable | NSWindowStyle.Resizable, NSBackingStore.Buffered, false)
        {
            // 關掉視窗不釋放原生物件：由這個類別的生命週期決定（關閉時還要讀位置、通知中樞）
            ReleasedWhenClosed = false,
            Title = ShellRules.AppTitle,
            MinSize = new CGSize((double)minimumSize.Width, (double)minimumSize.Height),
            // 多個視窗各自獨立，不要被系統併成一個視窗的分頁（介面自己有分頁）
            TabbingMode = NSWindowTabbingMode.Disallowed,
        };
        // 視窗的大小以整個外框計（和 Windows 版存的 Bounds 一樣含標題列）
        Window.SetFrame(MacScreens.FromTopLeft(bounds), false);
        events = new WindowEvents(this);
        Window.Delegate = events;
        ApplyWindowTheme(app.IsDark);
    }

    public NSWindow Window { get; }

    /// <summary>這個視窗的介面網頁</summary>
    public abstract AgentWebView WebView { get; }

    /// <summary>視窗已經關掉（不是藏起來）</summary>
    public bool IsClosed { get; private set; }

    /// <summary>看得到（沒有藏起來、沒有關掉；最小化到 Dock 也算，和 Windows 版的 Form.Visible 一樣）</summary>
    public bool Visible => !IsClosed && (Window.IsVisible || Window.IsMiniaturized);

    public bool Minimized => !IsClosed && Window.IsMiniaturized;

    public bool Maximized => !IsClosed && Window.IsZoomed;

    /// <summary>使用者正在操作的視窗：app 在前景、這個視窗看得到、沒有最小化，而且是作用中的視窗</summary>
    public bool IsForeground => !IsClosed && Window.IsVisible && !Window.IsMiniaturized && Window.IsKeyWindow && NSApplication.SharedApplication.Active;

    /// <summary>視窗外框的位置與大小（左上原點）</summary>
    public Rectangle Bounds
    {
        get => MacScreens.ToTopLeft(Window.Frame);
        set => Window.SetFrame(MacScreens.FromTopLeft(value), !IsClosed && Window.IsVisible);
    }

    public void SetTitle(string title)
    {
        if (!IsClosed)
            Window.Title = title;
    }

    /// <summary>顯示視窗；activate＝帶到前面並取得鍵盤焦點（false＝顯示但不搶焦點）</summary>
    public void Show(bool activate)
    {
        if (IsClosed)
            return;
        if (activate)
        {
            if (Window.IsMiniaturized)
                Window.Deminiaturize(null);
            Window.MakeKeyAndOrderFront(null);
            NSApplication.SharedApplication.ActivateIgnoringOtherApps(true);
        }
        else if (!Window.IsMiniaturized)
        {
            Window.OrderFront(null);
        }
        OnShown();
    }

    /// <summary>藏起來（不關閉：對話在背景繼續，之後叫得回來）</summary>
    public void Hide()
    {
        if (!IsClosed)
            Window.OrderOut(null);
    }

    public void Minimize()
    {
        if (!IsClosed)
            Window.Miniaturize(null);
    }

    /// <summary>程式關閉這個視窗（不經過「使用者按了關閉」的判斷）</summary>
    public void Close()
    {
        if (!IsClosed)
            Window.Close();
    }

    public virtual void ApplyTheme(bool dark)
    {
        if (IsClosed)
            return;
        ApplyWindowTheme(dark);
        WebView.SetBackground(dark);
    }

    /// <summary>標題列與視窗底色跟著主題（網頁載入前露出來的就是這個底色）</summary>
    private void ApplyWindowTheme(bool dark)
    {
        Window.Appearance = NSAppearance.GetAppearance(dark ? NSAppearance.NameDarkAqua : NSAppearance.NameAqua);
        Window.BackgroundColor = dark ? DarkBackground : LightBackground;
    }

    /// <summary>第一次顯示之後（子類別在這裡開始載入介面）</summary>
    protected virtual void OnShown()
    {
    }

    /// <summary>使用者按了關閉（紅色按鈕、⌘W）：回 true 才真的關</summary>
    protected abstract bool OnUserClosing();

    /// <summary>視窗正要關閉（使用者關的、程式關的都會到這裡，只會一次）</summary>
    protected virtual void OnClosed()
    {
    }

    /// <summary>視窗變成作用中：鍵盤焦點交給網頁；剪貼簿裡有在 Finder 複製的檔案就先登記（之後貼上時網頁才找得到路徑）</summary>
    private void OnBecameKey()
    {
        if (IsClosed)
            return;
        WebView.Focus();
        WebView.RegisterPasteboardFiles();
        App.OnWindowActivated(this);
    }

    private void HandleClosed()
    {
        if (IsClosed)
            return;
        try
        {
            OnClosed();
        }
        catch (Exception ex)
        {
            Log.Error("關閉視窗時失敗", ex);
        }
        IsClosed = true;
        WebView.Dispose();
        Window.Delegate = null!;
    }

    private sealed class WindowEvents(AppWindow owner) : NSWindowDelegate
    {
        public override bool WindowShouldClose(NSObject sender)
        {
            try
            {
                return owner.OnUserClosing();
            }
            catch (Exception ex)
            {
                Log.Error("處理視窗關閉失敗", ex);
                return true;
            }
        }

        public override void WillClose(NSNotification notification) => owner.HandleClosed();

        public override void DidBecomeKey(NSNotification notification) => owner.OnBecameKey();
    }
}

/// <summary>
/// 完整的視窗：整個介面是一個網頁（帳戶側欄、分頁與窗格、儀表板都在網頁裡）；標題列配合主題。macOS 版，對應 Windows 版的 MainForm。
/// 主視窗之外可以另外開幾個（多視窗；extra 不是 null）：共用同一個中樞，各有自己的版面，關掉就只關那一個
/// </summary>
internal sealed class MainWindow : AppWindow
{
    /// <summary>介面驗證過的最小大小與預設大小上限</summary>
    private static readonly Size MinimumSize = new(960, 620);

    private static readonly Size DefaultSize = new(1500, 940);

    private readonly AppWindowState? extra;
    private bool initialized;

    /// <param name="extra">另外開的視窗的設定（null＝主視窗）</param>
    public MainWindow(AppController app, AppWindowState? extra = null)
        : base(app, InitialBounds(extra == null ? app.Settings.Window : extra.Placement, out bool maximize),
            ShellRules.FitMinimumSize(MinimumSize, MacScreens.WorkingAreas()))
    {
        this.extra = extra;
        View = new MainWebView(this, app, app.Hub, app.Thumbnails, WindowKey, IsPrimary);
        View.Loaded += () => ViewLoaded?.Invoke();
        Window.ContentView = View.View;
        if (maximize)
            Window.Zoom(null);
    }

    public MainWebView View { get; }

    public override AgentWebView WebView => View;

    /// <summary>主視窗（第一個視窗）；false＝另外開的視窗</summary>
    public bool IsPrimary => extra == null;

    /// <summary>另外開的視窗在 settings.json 的設定（主視窗是 null）</summary>
    public AppWindowState? Extra => extra;

    public string WindowKey => extra?.Key ?? WindowRules.PrimaryKey;

    /// <summary>網頁顯示完成時觸發</summary>
    public event Action? ViewLoaded;

    protected override void OnShown() => StartInitialization();

    /// <summary>載入介面（只做一次；可在視窗顯示前呼叫：藏著啟動時介面照常初始化）</summary>
    public void StartInitialization()
    {
        if (initialized || IsClosed)
            return;
        initialized = true;
        try
        {
            View.Initialize(string.Empty, App.IsDark);
        }
        catch (Exception ex)
        {
            Log.Error("介面初始化失敗", ex);
            MacDialogs.Show(Window, $"介面初始化失敗：{ex.Message}", ShellRules.AppTitle, NSAlertStyle.Critical, "好");
            App.ExitImmediately();
        }
    }

    /// <summary>
    /// 還原上次的位置：標題列要抓得到（拔掉外接螢幕後只剩一角在畫面上時，移回螢幕內）；沒有存檔就在主螢幕置中
    /// </summary>
    private static Rectangle InitialBounds(WindowPlacementInfo? placement, out bool maximize)
    {
        maximize = false;
        var areas = MacScreens.WorkingAreas();
        var minimum = ShellRules.FitMinimumSize(MinimumSize, areas);
        if (placement != null && placement.Width >= MinimumSize.Width && placement.Height >= MinimumSize.Height)
        {
            var saved = new Rectangle(placement.X, placement.Y, Math.Max(placement.Width, minimum.Width), Math.Max(placement.Height, minimum.Height));
            if (ShellRules.FitToScreens(saved, areas) is { } rect)
            {
                maximize = placement.Maximized;
                return rect;
            }
        }
        var area = MacScreens.PrimaryWorkingArea();
        var size = ShellRules.FitWithin(DefaultSize, area, 0.85, 0.88);
        return new Rectangle(area.Left + (area.Width - size.Width) / 2, area.Top + (area.Height - size.Height) / 2, size.Width, size.Height);
    }

    public WindowPlacementInfo CapturePlacement()
    {
        var bounds = Bounds;
        return new WindowPlacementInfo
        {
            X = bounds.X,
            Y = bounds.Y,
            Width = bounds.Width,
            Height = bounds.Height,
            Maximized = Maximized,
        };
    }

    protected override bool OnUserClosing()
    {
        if (extra != null)
            return OnExtraWindowClosing();
        App.Settings.Window = CapturePlacement();
        switch (ShellRules.ClosingAction(CloseReason.UserClosing, App.Exiting, App.Settings.CloseToTray, App.OtherWindowsVisible(this)))
        {
            case MainClosingAction.HideToTray:
                Hide();
                App.ShowTrayHintOnce();
                return false;
            case MainClosingAction.Hide:
                // 還有另外開的視窗看得到：只藏起主視窗（選單列的圖示、Dock、再開一次 app 叫得回來），不提示「仍在背景執行」
                Hide();
                return false;
            case MainClosingAction.Exit:
                _ = App.ExitAsync();
                return false;
            default:
                return true;
        }
    }

    /// <summary>另外開的視窗關閉：使用者關掉的移除設定、結束 app 的途中留著（下次開回來）；不縮到選單列的最後一個視窗當成結束 app</summary>
    private bool OnExtraWindowClosing()
    {
        switch (ShellRules.ExtraWindowClosingAction(CloseReason.UserClosing, App.Exiting, App.Settings.CloseToTray, App.OtherWindowsVisible(this)))
        {
            case ExtraClosingAction.Discard:
                App.ForgetWindow(this);
                return true;
            case ExtraClosingAction.Keep:
                App.RememberWindow(this);
                return true;
            case ExtraClosingAction.Exit:
                _ = App.ExitAsync();
                return false;
            default:
                return true;
        }
    }

    protected override void OnClosed()
    {
        // 程式關的另外開的視窗（網頁搬走最後一個分頁後的 closeWindow）：和使用者關掉一樣移除設定；結束 app 的途中留著
        if (extra != null && !App.Exiting)
            App.ForgetWindow(this);
        App.OnMainWindowClosed(this);
    }
}

/// <summary>彈出視窗：只顯示一個對話（多螢幕並排用），與主視窗共用同一個中樞。macOS 版，對應 Windows 版的 PopoutForm</summary>
internal sealed class PopoutWindow : AppWindow
{
    private static readonly Size MinimumSize = new(520, 480);

    private static readonly Size DefaultSize = new(820, 900);

    private bool initialized;

    public PopoutWindow(AppController app, string conversationId, Rectangle? bounds)
        : base(app, bounds ?? DefaultBounds(), ShellRules.FitMinimumSize(MinimumSize, new[] { MacScreens.WorkingAreaAtCursor() }))
    {
        ConversationId = conversationId;
        View = new PopoutWebView(this, app, app.Hub, app.Thumbnails, conversationId);
        Window.ContentView = View.View;
    }

    public string ConversationId { get; }

    public PopoutWebView View { get; }

    public override AgentWebView WebView => View;

    /// <summary>預設放在滑鼠所在的螢幕中央；放不下就縮到工作區的九成</summary>
    private static Rectangle DefaultBounds()
    {
        var area = MacScreens.WorkingAreaAtCursor();
        var size = ShellRules.FitWithin(DefaultSize, area, 0.9, 0.9);
        return new Rectangle(area.Left + (area.Width - size.Width) / 2, area.Top + (area.Height - size.Height) / 2, size.Width, size.Height);
    }

    protected override void OnShown()
    {
        if (initialized || IsClosed)
            return;
        initialized = true;
        try
        {
            View.Initialize($"?mode=popout&conversation={Uri.EscapeDataString(ConversationId)}", App.IsDark);
        }
        catch (Exception ex)
        {
            Log.Error("彈出視窗初始化失敗", ex);
            Close();
        }
    }

    protected override bool OnUserClosing() => true;

    protected override void OnClosed() => App.OnPopoutClosed(this);
}
