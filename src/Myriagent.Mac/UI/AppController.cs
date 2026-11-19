using System.Diagnostics;
using System.Net.Http.Headers;
using System.Net.NetworkInformation;
using AppKit;
using CoreGraphics;
using Foundation;
using Myriagent.Accounts;
using Myriagent.Agents;
using Myriagent.Background;
using Myriagent.Cli;
using Myriagent.Core;
using Myriagent.Files;
using Myriagent.Git;
using Myriagent.Health;
using Myriagent.Hub;
using Myriagent.RemoteExec;
using Myriagent.Speech;
using Myriagent.SystemStatus;
using Myriagent.Sync;
using Myriagent.Update;
using Myriagent.Voice;
using Newtonsoft.Json.Linq;
using ObjCRuntime;

namespace Myriagent.UI;

/// <summary>
/// app 的生命週期與 macOS 的外殼（對應 Windows 版的 src/Myriagent/UI/AppController.cs，建立服務的順序與接法照它）：
/// 建立各服務（CLI、帳戶、中樞、縮圖）、主視窗、彈出視窗、選單列的圖示；實作中樞需要的宿主能力。
/// 關閉主視窗時（設定允許）藏起來、對話在背景繼續；從選單「結束」才真正離開。
/// 還沒有 macOS 實作的服務（排程、執行列、背景程式、對話框的燈號、遠端桌面）先不接，中樞對沒接的服務本來就當成沒有。
/// 所有成員都在主執行緒（中樞與帳戶的狀態只在主執行緒讀寫）
/// </summary>
internal sealed class AppController : IAgentHost, IAppUpdateShell, IWindowHost, IDisposable
{
    private readonly HttpClient http;
    private readonly MacTray tray;
    private readonly MacNotifications notifications;
    private readonly List<PopoutWindow> popouts = new();
    private readonly MainWindow mainWindow;

    /// <summary>主視窗以外另外開的完整視窗（多視窗）；設定在 settings.json 的 Windows</summary>
    private readonly List<MainWindow> extraWindows = new();

    /// <summary>上次結束時開著的另外的視窗已經開回來了（登入時自動啟動藏著時，等主視窗第一次顯示才開）</summary>
    private bool extraWindowsRestored;
    private readonly AppStartup appStartup;
    private readonly AppUpdateManager updateManager;
    private readonly AppUpdateCoordinator updateCoordinator;
    private string? lastNotifiedConversation;

    /// <summary>交棒中啟動的換版幫手</summary>
    private readonly UpdateHelperProcess updateHelper = new();

    /// <summary>這台電腦的狀態（電腦狀態面板與 stat1；有視窗開著面板時才取樣）</summary>
    private readonly SystemMonitor systemMonitor = new();

    /// <summary>語音通知（系統內建的語音；第一次要念時才建立）</summary>
    private readonly VoiceAnnouncer voice = new(() => MacSpeechEngine.Create());

    /// <summary>輸入區語音訊息的本機辨識（faster-whisper 的常駐程序，第一則語音才啟動）</summary>
    private readonly WhisperVoiceTranscriber voiceTranscriber = new(Path.Combine(AppPaths.Root, "voice"));

    /// <summary>背景活動日誌（logs/background/）</summary>
    private readonly BackgroundJournalFile backgroundJournal = new(AppPaths.BackgroundJournalDirectory);

    /// <summary>最近一則通知（陸續到的 CLI 換版彙總合併成一則用）</summary>
    private TrayNotice? lastTrayNotice;
    private bool trayHintShown;
    private bool exitPromptOpen;
    private bool syncStartCancelled;
    private bool syncEventsAttached;
    private readonly List<NSObject> observers = new();
    private IDisposable? appearanceObserver;

    /// <summary>各視窗最後的外框狀態（鍵是 view id；中樞只在有變的時候送，換主題後照它重設）</summary>
    private Dictionary<string, WindowBorder> windowBorders = new(StringComparer.Ordinal);

    /// <param name="startup">從程序啟動起算的計時</param>
    /// <param name="appStartup">命令列與安裝佈局</param>
    public AppController(Stopwatch startup, AppStartup appStartup)
    {
        _ = startup;
        this.appStartup = appStartup;
        // 帳戶初始化的 await 要回到主執行緒（中樞與帳戶狀態只在主執行緒讀寫）
        if (SynchronizationContext.Current == null)
            SynchronizationContext.SetSynchronizationContext(new MainThreadSynchronizationContext());
        // Claude 的登入憑證在 macOS 的鑰匙圈（不是 .credentials.json）：建立帳戶管理之前接上
        MacClaudeKeychain.Install();
        Settings = AppSettings.Load();
        http = new HttpClient(new SocketsHttpHandler
        {
            PooledConnectionLifetime = TimeSpan.FromMinutes(10),
            AutomaticDecompression = System.Net.DecompressionMethods.All,
        })
        { Timeout = TimeSpan.FromMinutes(10) };
        var version = AppVersionInfo.VersionText;
        http.DefaultRequestHeaders.UserAgent.Add(new ProductInfoHeaderValue("Myriagent", version));

        Thumbnails = new ThumbnailService();
        AgentHub? hubRef = null;
        Cli = ProbeRules.NoCliRequested()
            ? new CliManager(http, provider => hubRef?.HasRunningSessions(provider) ?? false, () => false, startBackgroundChecks: false, ProbeRules.NoCliOptions())
            : new CliManager(http, provider => hubRef?.HasRunningSessions(provider) ?? false, () => Settings.AutoUpdateCli, startBackgroundChecks: true,
                MacCliLocator.Options());
        Accounts = new AccountManager(BeginInvoke, Cli, http, id => hubRef?.RunningForAccount(id) ?? 0);
        bool probing = !string.IsNullOrWhiteSpace(Environment.GetEnvironmentVariable(ProbeRules.ProbeVariable));
        Accounts.ClaudeTokenRefreshEnabled = () => !probing && Settings.ClaudeIdleTokenRefresh;
        Accounts.ExtraClaudeUsageSources = AccountManager.SystemDefaultClaudeUsageSources;
        Accounts.Shared = probing ? null : new SharedCustomizations();
        var store = new ConversationStore(AppPaths.ConversationsDirectory);
        var startupResume = AppUpdateCoordinator.LoadStartupResume(AppPaths.Root);
        var draftLeftovers = ResumeIntentRules.DraftLeftoversToDelete(store.FindDraftLeftovers(), startupResume);
        Hub = new AgentHub(store, this, Accounts, Cli, Settings);
        hubRef = Hub;
        Hub.AttachBackgroundJournal(backgroundJournal);
        Hub.JournalApp("app.start", new JObject
        {
            ["release"] = AppVersionInfo.Current.Release,
            ["build"] = AppVersionInfo.Current.Build,
            ["afterUpdate"] = startupResume != null,
            ["dataRoot"] = AppPaths.Root,
        });
        Hub.AttachSystemMonitor(systemMonitor);
        Hub.AttachGitStatus(new GitStatusReader());
        Hub.AttachGitTurnReader(new GitTurnReader());
        // 回合結束後代理開的程式還在跑：帳戶卡與對話標頭的「另開的程式」
        Hub.AttachBackgroundPrograms(BackgroundProgramMonitor.CreateDefault());
        // 對話框的燈號（資料流、網路、硬碟）與輸入框下方的 CPU 波形
        Hub.AttachActivity(new ActivitySampler());
        Hub.AttachHealth(new HealthScanner(new HealthMachine(Environment.GetFolderPath(Environment.SpecialFolder.UserProfile))));
        // 排程頁：這台的 launchd 工作與 crontab（Windows 版讀的是工作排程器）；Myriagent 的定時訊息跟著一起掛
        Hub.AttachSchedules(new Myriagent.Schedules.MacScheduleReader(new[] { Path.Combine(AppPaths.Root, "conversations") }), Path.Combine(AppPaths.Root, "schedules"));
        Hub.AttachScheduleTranscripts(Hub.DefaultTranscriptRoots);
        Hub.AttachVoice(voiceTranscriber);
        Hub.AttachMediaKeeper();
        _ = Task.Run(() => AgentHub.CleanStaleUploadTemp());
        Accounts.Initialize();
        if (Settings.ShareCrossCliCustomizations && Accounts.Shared is { } bootShared)
            _ = Task.Run(() => bootShared.SyncCrossCli());
        // 多電腦同步：建構子不做 I/O，延後到主視窗載入之後才 Start
        Sync = new SyncCoordinator(Hub, Accounts, this, Settings, SyncPaths.FromAppPaths(),
            SyncCoordinatorOptions.FromEnvironment(version), TimeProvider.System);
        Hub.AttachSync(Sync);
        // 遠端執行指令：被操作時執行其他電腦的代理下的指令（高風險的在這台問人）；發起時以本機管道收對話裡 myriagent-remote 的要求
        remoteExec = new RemoteExecService(Sync, new RemoteExecConfirmer(this), Path.Combine(Path.GetTempPath(), "myriagent-remote-exec"),
            RemoteExecPipeRules.PipeName(AppPaths.Root), ConversationTitleAsync, isElevated: () => Environment.IsPrivilegedProcess);
        Hub.AttachRemoteExec(remoteExec.Runner);
        Hub.AttachFileTransferActivity(() => Myriagent.Sync.FileTransferActivity.Active);
        Sync.AttachRemoteExec(remoteExec);
        // 對話裡的代理用的小工具（myriagent-remote、myriagent-schedule）：macOS 編在同一個執行檔裡，這裡放兩支轉呼叫的小腳本到 PATH 上
        var toolDirectory = MacTools.Install(Path.Combine(AppPaths.Root, "tools"));
        var remoteSync = Sync;
        Hub.AttachRemoteExecIssuer(new RemoteExecIssuerInfo(remoteExec.PipeName, remoteExec.TokenFor, toolDirectory, () => remoteSync.IsPairedSnapshot));
        remoteExec.TimedHandler = (conversationId, args) => InvokeOnUiAsync(() => Hub.HandleTimedCli(conversationId, args));
        Hub.AttachDeviceControls(new MacDeviceControls());
        remoteExec.ConversationHandler = (conversationId, request, directory)
            => InvokeOnUiAsync(() => Hub.HandleConversationCli(conversationId, request, directory)).Unwrap();
        remoteExec.FileHandler = (conversationId, request, directory, status, ct) => Hub.HandleFilesCli(conversationId, request, directory, status, ct);
        Hub.TimedToolAvailable = toolDirectory != null;
        remoteExec.StartPipe();
        Accounts.UsagePollPolicy = Sync;
        Sync.StatusChanged += OnSyncStatusChanged;
        if (!Settings.AntigravityAccountAdded && Accounts.EnsureSystemDefault(AgentProvider.Antigravity))
        {
            Settings.AntigravityAccountAdded = true;
            Settings.Save();
        }
        if (!Settings.GrokAccountAdded && Accounts.EnsureSystemDefault(AgentProvider.Grok))
        {
            Settings.GrokAccountAdded = true;
            Settings.Save();
        }
        // app 自我更新：macOS 還沒有受管理的安裝，更新管理員照樣建立（設定頁會顯示「這一份不會自動更新」），不會真的換版
        var managed = appStartup.ManagedRelease;
        var sync = Sync;
        updateManager = new AppUpdateManager(appStartup.Layout, AppVersionInfo.Current, new HubReleaseSource(() => sync.ReleaseHttp),
            () => InstallLayoutRules.Evaluate(managed != null, appStartup.UsesDefaultData, Settings.AutoUpdateApp, paired: sync.IsPairedSnapshot,
                hasTestSource: false, e2e: UpdateEnvironment.IsE2E),
            ReleaseKeys.Trusted(allowTestKey: UpdateEnvironment.IsE2E), new BundledRuntimeProbe());
        updateCoordinator = new AppUpdateCoordinator(Hub, updateManager, this, appStartup.Layout, AppPaths.Root, managed, appStartup.UsesDefaultData,
            UpdateEnvironment.E2EFast ? AppUpdateTimings.E2E : AppUpdateTimings.Normal);
        updateCoordinator.ScheduleStartupResume(appStartup.Options, startupResume);
        Hub.AutoStart = new AutoStartRegistration(MacTools.BundlePath, appStartup.UsesDefaultData && !UpdateEnvironment.IsE2E);
        sync.AppReleaseAnnounced += updateCoordinator.CheckSoon;
        updateCoordinator.AppStateChanged += sync.OfferAppState;
        if (draftLeftovers.Count > 0)
            _ = Task.Run(() => DeleteDraftLeftovers(store, draftLeftovers));

        NSApplication.SharedApplication.MainMenu = MacMainMenu.Build(this);
        tray = new MacTray(BuildTrayMenu);
        notifications = new MacNotifications(BeginInvoke, ShowConversation);

        mainWindow = new MainWindow(this);
        Hub.AttachWindows(this);
        MacSmoke.AttachIfRequested(this, mainWindow);
        MacDebugDriver.AttachIfRequested(this, mainWindow);
        // 同步在主視窗第一次載入 2.5 秒後才啟動；一直沒有載入時 10 秒後也啟動（Start 只有第一次有效）
        mainWindow.ViewLoaded += OnFirstViewLoaded;
        ScheduleSyncStart(SyncStartupRules.FallbackDelay);
        // 網路恢復、從睡眠喚醒：請同步引擎立刻重試
        observers.Add(NSWorkspace.SharedWorkspace.NotificationCenter.AddObserver(NSWorkspace.DidWakeNotification, _ => BeginInvoke(() =>
        {
            if (!Exiting)
                Sync.OnSystemResumed();
        })));
        NetworkChange.NetworkAvailabilityChanged += OnNetworkAvailabilityChanged;
        NetworkChange.NetworkAddressChanged += OnNetworkAddressChanged;
        syncEventsAttached = true;
        // 系統切換深淺色時，主題「跟隨系統」的標題列與底色跟著換（網頁自己以 prefers-color-scheme 切換）
        appearanceObserver = NSApplication.SharedApplication.AddObserver("effectiveAppearance", NSKeyValueObservingOptions.New, _ => BeginInvoke(() =>
        {
            if (Settings.Theme == "system" && !Exiting && !mainWindow.IsClosed)
                ApplyTheme(Settings.Theme);
        }));
        updateCoordinator.Start();
        // 介面初始化排到事件迴圈開始後才執行（和 Windows 版一樣：建構子裡不做會跳訊息框的事）
        BeginInvoke(mainWindow.StartInitialization);
        if (AutoStartRules.LockAfterStart(appStartup.Options.AutoStart, Settings.AutoStartLock))
        {
            _ = Task.Delay(AutoStartRules.LockDelay).ContinueWith(_ => BeginInvoke(() =>
            {
                var locked = NativeMethods.LockWorkstation();
                Log.Info(locked ? "登入時自動啟動：已鎖定螢幕（設定「自動啟動後鎖定電腦」）" : "登入時自動啟動：鎖定螢幕失敗");
            }), TaskScheduler.Default);
        }
        if (AutoStartRules.StartHidden(appStartup.Options.AutoStart, Settings.CloseToTray))
        {
            // 登入時自動啟動：只留選單列的圖示，點圖示或再開一次 app 才顯示
            Log.Info("登入時自動啟動：主視窗先藏著");
        }
        else
        {
            mainWindow.Show(activate: true);
            BeginInvoke(RestoreExtraWindows);
        }
    }

    public AppSettings Settings { get; }

    public CliManager Cli { get; }

    public AccountManager Accounts { get; }

    public AgentHub Hub { get; }

    public ThumbnailService Thumbnails { get; }

    /// <summary>多電腦同步的協調者（未配對時也有：配對指令、用量查詢閘門）</summary>
    public SyncCoordinator Sync { get; }

    /// <summary>遠端執行指令的桌面端（被操作與發起）</summary>
    private readonly RemoteExecService remoteExec;

    public bool Exiting { get; private set; }

    /// <summary>收尾已經做完，可以讓系統結束這個程序了（AppDelegate 的 applicationShouldTerminate 看這個）</summary>
    public bool ReadyToTerminate { get; private set; }

    public bool IsDark => Settings.Theme switch
    {
        "light" => false,
        "system" => NativeMethods.IsSystemDarkTheme(),
        _ => true,
    };

    /// <summary>在主執行緒執行、等結果（管道的要求：中樞的狀態只在主執行緒）</summary>
    private Task<T> InvokeOnUiAsync<T>(Func<T> func)
    {
        var done = new TaskCompletionSource<T>(TaskCreationOptions.RunContinuationsAsynchronously);
        BeginInvoke(() =>
        {
            try
            {
                done.TrySetResult(func());
            }
            catch (Exception ex)
            {
                done.TrySetException(ex);
            }
        });
        return done.Task;
    }

    private Task<string?> ConversationTitleAsync(string conversationId) => InvokeOnUiAsync(() => Hub.ConversationTitle(conversationId));

    // ───────────────────────────── 選單列的選單 ─────────────────────────────

    /// <summary>選單列圖示的選單：每次打開時重建，列出每個帳戶目前的用量（不必開視窗就能快速監看）</summary>
    private void BuildTrayMenu(NSMenu menu)
    {
        menu.AddItem(MacMenuItems.Action("開啟 Clode Myriagent", ShowMain));
        menu.AddItem(MacMenuItems.Action("開新視窗", OpenNewWindow));
        menu.AddItem(MacMenuItems.Action("帳戶儀表板", ShowDashboard));
        menu.AddItem(NSMenuItem.SeparatorItem);
        foreach (var account in Accounts.Accounts)
        {
            var usage = Accounts.GetUsage(account.Id);
            var windows = usage?.Windows.Take(3).Select(w => $"{w.Label} {w.Percent:0}%").ToList() ?? new List<string>();
            var state = Accounts.GetState(account.Id);
            var text = windows.Count > 0
                ? $"{account.Label}：{string.Join("・", windows)}{(usage!.Stale ? "（可能過時）" : string.Empty)}"
                : $"{account.Label}：{state.StatusText}";
            // 用量到了 80% 或被鎖住：前面加警示（Windows 版是紅字）
            if (usage?.Windows.Any(w => w.Percent >= 80 || w.Locked) == true)
                text = "⚠️ " + text;
            menu.AddItem(MacMenuItems.Action(text, ShowDashboard));
        }
        int running = Hub.RunningCount;
        if (running > 0)
            menu.AddItem(MacMenuItems.Disabled($"執行中的對話：{running}"));
        int background = Hub.BackgroundWaitingCount;
        if (background > 0)
            menu.AddItem(MacMenuItems.Disabled($"等背景工作回報的對話：{background}"));
        var syncItems = TraySyncRules.Items(Sync.Status, Sync.IsPaired, Settings.Sync.Paused, Settings.Sync.RemotePaused, Sync.Backfill, Sync.BackfillPaused);
        if (syncItems.Count > 0)
        {
            menu.AddItem(NSMenuItem.SeparatorItem);
            foreach (var syncItem in syncItems)
            {
                var action = syncItem.Action;
                menu.AddItem(syncItem.Enabled ? MacMenuItems.Action(syncItem.Text, () => OnTraySyncAction(action)) : MacMenuItems.Disabled(syncItem.Text));
            }
        }
        menu.AddItem(NSMenuItem.SeparatorItem);
        menu.AddItem(MacMenuItems.Action("結束", RequestExit));
    }

    private void OnTraySyncAction(TraySyncAction action)
    {
        if (Exiting)
            return;
        switch (action)
        {
            case TraySyncAction.None:
                break;
            case TraySyncAction.PauseSync:
                Sync.SetPaused(true);
                break;
            case TraySyncAction.ResumeSync:
                Sync.SetPaused(false);
                break;
            case TraySyncAction.PauseRemote:
                Sync.SetRemotePaused(true);
                break;
            case TraySyncAction.ResumeRemote:
                Sync.SetRemotePaused(false);
                break;
            default:
                throw new ArgumentOutOfRangeException(nameof(action), action, "不認得的同步項目");
        }
    }

    // ───────────────────────────── 同步的延後啟動與網路事件 ─────────────────────────────

    /// <summary>主視窗每次載入都會觸發 ViewLoaded：只有第一次排同步的啟動</summary>
    private void OnFirstViewLoaded()
    {
        mainWindow.ViewLoaded -= OnFirstViewLoaded;
        ScheduleSyncStart(SyncStartupRules.StartDelay);
    }

    private void ScheduleSyncStart(TimeSpan delay)
        => _ = Task.Delay(delay).ContinueWith(_ => BeginInvoke(() =>
        {
            if (!Exiting && !syncStartCancelled)
                Sync.Start();
        }), TaskScheduler.Default);

    private void OnNetworkAvailabilityChanged(object? sender, NetworkAvailabilityEventArgs e)
    {
        if (e.IsAvailable)
            Sync.OnNetworkChanged();
    }

    private void OnNetworkAddressChanged(object? sender, EventArgs e) => Sync.OnNetworkChanged();

    private void DetachSyncEvents()
    {
        if (!syncEventsAttached)
            return;
        syncEventsAttached = false;
        NetworkChange.NetworkAvailabilityChanged -= OnNetworkAvailabilityChanged;
        NetworkChange.NetworkAddressChanged -= OnNetworkAddressChanged;
    }

    /// <summary>同步狀態改變（主執行緒）：選單列提示文字的同步段</summary>
    private void OnSyncStatusChanged()
    {
        if (!Exiting)
            Hub.SetSyncTrayText(TraySyncRules.TrayText(Sync.Status, Sync.IsPaired, Sync.Backfill));
    }

    // ───────────────────────────── 視窗 ─────────────────────────────

    /// <summary>把主視窗帶到前面（另一個實例啟動、點 Dock 的圖示、選單）；正在結束時不再顯示：中樞已停止，視窗不會有反應</summary>
    public void ShowMain()
    {
        if (Exiting || mainWindow.IsClosed)
            return;
        mainWindow.Show(activate: true);
        RestoreExtraWindows();
    }

    /// <summary>再開一次 app（另一個程序送來「開新視窗」）：主視窗藏著時叫出主視窗，否則開一個新視窗</summary>
    public void OpenWindowFromLaunch()
    {
        if (Exiting || mainWindow.IsClosed)
            return;
        if (ShellRules.LaunchAgainOpensWindow(mainWindow.Visible))
            OpenNewWindow();
        else
            ShowMain();
    }

    /// <summary>主視窗切到帳戶儀表板</summary>
    public void ShowDashboard()
    {
        ShowMain();
        if (!mainWindow.IsClosed)
            mainWindow.View.PostToWeb(new JObject { ["type"] = "showView", ["view"] = "dashboard" });
    }

    /// <summary>所有完整的視窗（主視窗在最前面；不含彈出視窗）</summary>
    private IEnumerable<MainWindow> AllWindows() => extraWindows.Prepend(mainWindow).Where(w => !w.IsClosed);

    /// <summary>開一個新的完整視窗：從目前在前面的視窗錯開一點</summary>
    public void OpenNewWindow()
    {
        var anchor = AllWindows().FirstOrDefault(w => w.IsForeground) ?? AllWindows().FirstOrDefault(w => w.Visible);
        ((IWindowHost)this).OpenWindow(WindowRules.NewKey(), anchor?.View.Id, atCursor: false);
    }

    /// <summary>上次結束時開著的另外的視窗開回來（只做一次；藏著啟動時等主視窗第一次顯示）</summary>
    private void RestoreExtraWindows()
    {
        if (extraWindowsRestored || Exiting)
            return;
        extraWindowsRestored = true;
        foreach (var entry in Settings.Windows.ToList())
            ShowExtraWindow(entry, restoring: true);
    }

    private MainWindow ShowExtraWindow(AppWindowState entry, bool restoring)
    {
        var window = new MainWindow(this, entry);
        extraWindows.Add(window);
        window.Show(activate: !restoring);
        if (restoring && entry.Minimized)
            window.Minimize();
        return window;
    }

    /// <summary>除了這個視窗以外，還有看得到（沒有藏起來）的完整視窗</summary>
    public bool OtherWindowsVisible(MainWindow except) => AllWindows().Any(w => w != except && w.Visible);

    /// <summary>使用者關掉了另外開的視窗：從設定移除（下次不再開回來）</summary>
    public void ForgetWindow(MainWindow window)
    {
        if (window.Extra is not { } entry)
            return;
        Settings.Windows.RemoveAll(w => w.Key == entry.Key);
        Settings.Save();
    }

    /// <summary>結束 app 等原因關閉另外開的視窗：記下位置</summary>
    public void RememberWindow(MainWindow window)
    {
        if (window.Extra is not { } entry || !window.Visible)
            return;
        entry.Placement = window.CapturePlacement();
        entry.Minimized = window.Minimized;
    }

    public void OnMainWindowClosed(MainWindow window) => extraWindows.Remove(window);

    public void OnPopoutClosed(PopoutWindow window) => popouts.Remove(window);

    /// <summary>有視窗變成作用中（留給之後要看「哪個視窗在前面」的地方）</summary>
    public void OnWindowActivated(AppWindow window) => _ = window;

    /// <summary>「編輯 › 貼上」：先讓作用中的視窗登記剪貼簿裡的檔案，再照常貼到有焦點的元件</summary>
    public void Paste(NSObject? sender)
    {
        var key = NSApplication.SharedApplication.KeyWindow;
        if (key != null && WindowOf(key) is { } window)
            window.WebView.RegisterPasteboardFiles();
        NSApplication.SharedApplication.SendAction(new Selector("paste:"), null!, sender!);
    }

    private AppWindow? WindowOf(NSWindow native)
        => AllWindows().Cast<AppWindow>().Concat(popouts.Where(p => !p.IsClosed)).FirstOrDefault(w => w.Window.Handle == native.Handle);

    bool IWindowHost.OpenWindow(string key, string? sourceViewId, bool atCursor)
    {
        if (Exiting || mainWindow.IsClosed)
            return false;
        var source = (sourceViewId == null ? null : WindowOfView(sourceViewId) as MainWindow) ?? mainWindow;
        var bounds = ShellRules.NewWindowBounds(source.Bounds, atCursor ? MacScreens.Cursor() : null, MacScreens.Dpi, MacScreens.WorkingAreas());
        var entry = new AppWindowState
        {
            Key = key,
            Placement = new WindowPlacementInfo { X = bounds.X, Y = bounds.Y, Width = bounds.Width, Height = bounds.Height },
        };
        Settings.Windows.Add(entry);
        Settings.Save();
        ShowExtraWindow(entry, restoring: false);
        Log.Info($"開新視窗（{(atCursor ? "拖出分頁" : "錯開")}，共 {AllWindows().Count()} 個）");
        return true;
    }

    TabDropTarget IWindowHost.LocateTabDrop(string sourceViewId)
    {
        // 按 Esc 取消拖曳也會以「沒有人接住」結束：這時不開新視窗
        if (Exiting || NativeMethods.IsEscapeDown())
            return new TabDropTarget(TabDropKind.None);
        var number = NSWindow.WindowNumberAtPoint(NSEvent.CurrentMouseLocation, 0);
        var native = number <= 0 ? null : NSApplication.SharedApplication.WindowWithWindowNumber(number);
        // 游標下不是這個 app 的視窗（桌面、其他程式）：開新視窗
        if (native == null)
            return new TabDropTarget(TabDropKind.NewWindow);
        var window = AllWindows().FirstOrDefault(w => w.Visible && w.Window.Handle == native.Handle);
        if (window != null)
            return window.View.Id == sourceViewId ? new TabDropTarget(TabDropKind.None) : new TabDropTarget(TabDropKind.Window, window.View.Id);
        // 這個 app 的其他視窗（彈出視窗、對話框、選單）：不處理
        return new TabDropTarget(TabDropKind.None);
    }

    void IWindowHost.ActivateWindow(string viewId) => WindowOfView(viewId)?.Show(activate: true);

    void IWindowHost.CloseWindow(string viewId) => extraWindows.FirstOrDefault(w => !w.IsClosed && w.View.Id == viewId)?.Close();

    void IWindowHost.UpdateWindowBorders(IReadOnlyDictionary<string, WindowBorder> borders)
    {
        windowBorders = new Dictionary<string, WindowBorder>(borders, StringComparer.Ordinal);
        ApplyWindowBorders();
    }

    /// <summary>照各視窗的外框狀態畫一圈顏色（執行中、等你回應）；沒有狀態的不畫</summary>
    private void ApplyWindowBorders()
    {
        bool dark = IsDark;
        foreach (var window in AllWindows().Cast<AppWindow>().Concat(popouts.Where(p => !p.IsClosed)).ToList())
        {
            var border = windowBorders.TryGetValue(window.WebView.Id, out var b) ? b : WindowBorder.None;
            try
            {
                var view = window.WebView.View;
                view.WantsLayer = true;
                if (view.Layer is not { } layer)
                    continue;
                if (border == WindowBorder.None)
                {
                    layer.BorderWidth = 0;
                    continue;
                }
                var (r, g, bl) = WindowBorderRules.Rgb(border, dark);
                layer.BorderColor = new CGColor(r / 255f, g / 255f, bl / 255f, 1f);
                layer.BorderWidth = 2;
            }
            catch (Exception ex)
            {
                Log.Warn($"畫視窗外框失敗：{ex.Message}");
            }
        }
    }

    /// <summary>點系統通知：對話開在彈出視窗或另外開的視窗就把那個視窗帶到前面，否則在主視窗切到（或開啟）這個對話</summary>
    private void ShowConversation(string? conversationId)
    {
        conversationId ??= lastNotifiedConversation;
        var popout = conversationId == null ? null : popouts.FirstOrDefault(p => p.ConversationId == conversationId && !p.IsClosed);
        if (popout != null)
        {
            popout.Show(activate: true);
            return;
        }
        if (conversationId != null && !Exiting)
        {
            var showing = Hub.ViewIdsOf(conversationId);
            var window = AllWindows().FirstOrDefault(w => w.Visible && showing.Contains(w.View.Id));
            if (window != null)
            {
                window.Show(activate: true);
                window.View.PostToWeb(new JObject { ["type"] = "focusConversation", ["conversationId"] = conversationId });
                return;
            }
        }
        ShowMain();
        if (conversationId != null && !mainWindow.IsClosed)
            mainWindow.View.PostToWeb(new JObject { ["type"] = "focusConversation", ["conversationId"] = conversationId });
    }

    public void ShowTrayHintOnce()
    {
        if (trayHintShown)
            return;
        trayHintShown = true;
        var running = Hub.RunningCount;
        lastTrayNotice = null;
        lastNotifiedConversation = null;
        notifications.Show("Clode Myriagent 仍在背景執行",
            running > 0 ? $"還有 {running} 個對話在執行；點選單列或 Dock 的圖示可回到視窗" : "點選單列或 Dock 的圖示可回到視窗；在選單按「結束」才會完全關閉", null);
    }

    // ───────────────────────────── IAgentHost ─────────────────────────────

    public void BeginInvoke(Action action) => NSApplication.SharedApplication.BeginInvokeOnMainThread(() =>
    {
        try
        {
            action();
        }
        catch (Exception ex)
        {
            Log.Error("主執行緒工作失敗", ex);
        }
    });

    private NSWindow? NativeWindowOf(IAgentView view) => (WindowOfView(view.Id) ?? mainWindow).Window;

    public IReadOnlyList<string> PickFiles(IAgentView view)
    {
        _ = NativeWindowOf(view);
        var panel = NSOpenPanel.OpenPanel;
        panel.Title = "選擇要附加的檔案";
        panel.CanChooseFiles = true;
        panel.CanChooseDirectories = false;
        panel.AllowsMultipleSelection = true;
        if (panel.RunModal() != (long)NSModalResponse.OK)
            return Array.Empty<string>();
        return panel.Urls.Select(u => u.Path).Where(p => !string.IsNullOrEmpty(p)).Select(p => p!).ToList();
    }

    public string? PickFolder(IAgentView view, string? initialDirectory)
    {
        _ = NativeWindowOf(view);
        var panel = NSOpenPanel.OpenPanel;
        panel.Message = "選擇代理的工作資料夾（代理會在這個資料夾讀寫檔案）";
        panel.CanChooseFiles = false;
        panel.CanChooseDirectories = true;
        panel.CanCreateDirectories = true;
        panel.AllowsMultipleSelection = false;
        // 起始資料夾存不存在在背景確認、最多等 2 秒（網路磁碟離線時會卡很久）；確認不了就不指定
        if (!string.IsNullOrWhiteSpace(initialDirectory) && PathProbe.Check(() => Directory.Exists(initialDirectory), PathProbe.ShortTimeout) == true)
            panel.DirectoryUrl = NSUrl.FromFilename(initialDirectory);
        return panel.RunModal() == (long)NSModalResponse.OK ? panel.Url?.Path : null;
    }

    public string? PickSaveFile(IAgentView view, string fileName)
    {
        _ = NativeWindowOf(view);
        var panel = NSSavePanel.SavePanel;
        panel.Title = "另存新檔";
        panel.NameFieldStringValue = fileName;
        panel.CanCreateDirectories = true;
        if (KnownDownloadsFolder() is { } downloads)
            panel.DirectoryUrl = NSUrl.FromFilename(downloads);
        return panel.RunModal() == (long)NSModalResponse.OK ? panel.Url?.Path : null;
    }

    /// <summary>使用者的「下載項目」資料夾（~/Downloads）；不存在回 null</summary>
    private static string? KnownDownloadsFolder()
    {
        var profile = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        if (string.IsNullOrEmpty(profile))
            return null;
        var downloads = Path.Combine(profile, "Downloads");
        return Directory.Exists(downloads) ? downloads : null;
    }

    public void OpenWithShell(string target)
    {
        try
        {
            // 完整路徑當成檔案；其他（http、mailto 等）當成網址
            using var url = Path.IsPathRooted(target) ? NSUrl.FromFilename(target) : NSUrl.FromString(target);
            if (url == null || !NSWorkspace.SharedWorkspace.OpenUrl(url))
                MacDialogs.Show(mainWindow.Window, $"無法開啟：{target}", ShellRules.AppTitle, NSAlertStyle.Warning, "好");
        }
        catch (Exception ex)
        {
            MacDialogs.Show(mainWindow.Window, $"無法開啟：{ex.Message}", ShellRules.AppTitle, NSAlertStyle.Warning, "好");
        }
    }

    public void RevealInExplorer(string path) => _ = RevealInFinderAsync(path);

    /// <summary>檔案就在 Finder 選取、資料夾就開啟。是不是檔案在背景確認；確認不了就只選取（不會開啟或執行檔案）</summary>
    private async Task RevealInFinderAsync(string path)
    {
        var isFile = await PathProbe.CheckAsync(() => File.Exists(path), PathProbe.ShortTimeout);
        BeginInvoke(() =>
        {
            try
            {
                using var url = NSUrl.FromFilename(path);
                if (isFile != false)
                    NSWorkspace.SharedWorkspace.ActivateFileViewer(new[] { url });
                else if (!NSWorkspace.SharedWorkspace.OpenUrl(url))
                    MacDialogs.Show(mainWindow.Window, $"無法在 Finder 開啟：{path}", ShellRules.AppTitle, NSAlertStyle.Warning, "好");
            }
            catch (Exception ex)
            {
                MacDialogs.Show(mainWindow.Window, $"無法開啟 Finder：{ex.Message}", ShellRules.AppTitle, NSAlertStyle.Warning, "好");
            }
        });
    }

    public void CopyText(string text)
    {
        try
        {
            var pasteboard = NSPasteboard.GeneralPasteboard;
            pasteboard.ClearContents();
            pasteboard.SetStringForType(text, "public.utf8-plain-text");
        }
        catch (Exception ex)
        {
            Log.Warn($"複製失敗：{ex.Message}");
        }
    }

    public bool CopyFile(string path)
    {
        try
        {
            var pasteboard = NSPasteboard.GeneralPasteboard;
            pasteboard.ClearContents();
            using var url = NSUrl.FromFilename(path);
            return pasteboard.WriteObjects(new INSPasteboardWriting[] { url });
        }
        catch (Exception ex)
        {
            Log.Warn($"複製檔案失敗：{ex.Message}");
            return false;
        }
    }

    public void OpenPopout(string conversationId) => OpenPopout(conversationId, null);

    /// <param name="bounds">app 更新前的位置與大小（x, y, w, h）；不在任何螢幕的工作區內就用預設位置</param>
    public void OpenPopout(string conversationId, int[]? bounds)
    {
        var existing = popouts.FirstOrDefault(p => p.ConversationId == conversationId && !p.IsClosed);
        if (existing != null)
        {
            existing.Show(activate: true);
            return;
        }
        var window = new PopoutWindow(this, conversationId, AppControllerRules.RestorableBounds(bounds, MacScreens.WorkingAreas()));
        popouts.Add(window);
        window.Show(activate: true);
    }

    public void ConversationDeleted(string conversationId)
    {
        foreach (var popout in popouts.Where(p => p.ConversationId == conversationId).ToList())
            popout.Close();
    }

    /// <summary>顯示這個 view 的視窗（主視窗、另外開的視窗或彈出視窗）；view 所在的視窗已關閉時回 null</summary>
    private AppWindow? WindowOfView(string viewId)
    {
        if (AllWindows().FirstOrDefault(w => w.View.Id == viewId) is { } window)
            return window;
        return popouts.FirstOrDefault(p => !p.IsClosed && p.View.Id == viewId);
    }

    public bool IsViewForeground(string viewId) => WindowOfView(viewId) is { IsForeground: true };

    /// <summary>
    /// 系統通知。對話的通知點了回到那個對話；中樞在一波 CLI 換版都處理完後送的彙總（conversationId 為 null）點了開主視窗，
    /// 同時到的幾則合併成一則（AppControllerRules.NextTrayNotice）
    /// </summary>
    public void Notify(string title, string message, string? conversationId)
    {
        var notice = AppControllerRules.NextTrayNotice(lastTrayNotice, title, message, conversationId, DateTime.UtcNow);
        lastTrayNotice = notice;
        lastNotifiedConversation = notice.ConversationId;
        notifications.Show(notice.Title, notice.Message, notice.ConversationId);
    }

    /// <summary>提醒使用者（Windows 版閃工作列按鈕）：app 不在前景時讓 Dock 的圖示跳一下</summary>
    public void RequestAttention(IReadOnlyCollection<string> viewIds)
    {
        _ = viewIds;
        if (!NSApplication.SharedApplication.Active)
            NSApplication.SharedApplication.RequestUserAttention(NSRequestUserAttentionType.InformationalRequest);
    }

    public void Speak(string text, VoiceTuning tuning, Action? started) => voice.Speak(text, started == null ? null : () => BeginInvoke(started), tuning);

    public void StopSpeaking() => voice.Stop();

    public void ApplyTheme(string theme)
    {
        _ = theme;
        bool dark = IsDark;
        foreach (var window in AllWindows().ToList())
            window.ApplyTheme(dark);
        foreach (var popout in popouts.Where(p => !p.IsClosed).ToList())
            popout.ApplyTheme(dark);
        ApplyWindowBorders();
    }

    public void UpdateTray(string tooltip, int running, int waiting)
    {
        // 建構期間中樞就可能呼叫（選單列的圖示還沒建）
        tray?.Update(tooltip, TrayBadgeRules.Decide(running, waiting));
    }

    // ───────────────────────────── app 自我更新（IAppUpdateShell）─────────────────────────────

    /// <summary>app 在前景而且有看得到的視窗（含選檔、確認這些對話框）</summary>
    bool IAppUpdateShell.AnyWindowForeground => NSApplication.SharedApplication.Active && NSApplication.SharedApplication.KeyWindow != null;

    bool IAppUpdateShell.MainWindowHidden => mainWindow.IsClosed || !mainWindow.Visible || mainWindow.Minimized;

    TimeSpan IAppUpdateShell.SinceLastInput => NativeMethods.IdleTime();

    bool IAppUpdateShell.LoginInProgress => Accounts.HasActiveLogin;

    bool IAppUpdateShell.CliUpdating => AgentHub.AppUpdateProviders.Any(p => AppUpdateRestartRules.IsCliUpdating(Cli.Get(p)));

    IReadOnlyList<ResumePopout> IAppUpdateShell.CapturePopouts()
        => popouts.Where(p => !p.IsClosed).Select(p =>
        {
            var b = p.Bounds;
            return new ResumePopout(p.ConversationId, new[] { b.X, b.Y, b.Width, b.Height });
        }).ToList();

    void IAppUpdateShell.OpenPopouts(IReadOnlyList<ResumePopout> list)
    {
        foreach (var p in list)
        {
            if (Hub.GetConversation(p.ConversationId) != null)
                OpenPopout(p.ConversationId, p.Bounds);
        }
    }

    void IAppUpdateShell.Notify(string title, string message) => Notify(title, message, conversationId: null);

    int? IAppUpdateShell.StartHelper(string executable, IReadOnlyList<string> arguments) => updateHelper.Start(executable, arguments);

    void IAppUpdateShell.StopHelper(int pid) => updateHelper.Stop(pid);

    void IAppUpdateShell.ExitForUpdate(IReadOnlySet<string> keepDrafts) => _ = ExitForUpdateAsync(keepDrafts);

    void IAppUpdateShell.RestartInto(string executable)
    {
        launchAfterExit = executable;
        RequestExit();
    }

    /// <summary>「安裝到這台電腦」之後，結束時要啟動的那一份</summary>
    private string? launchAfterExit;

    /// <summary>單一實例的監聽執行緒：較新的版本請這一份交棒。請求合格才接下，排到主執行緒由更新協調器排空與交棒</summary>
    public bool AcceptHandover()
    {
        var request = InstanceHandover.TakeRequest(AppPaths.Root);
        if (InstanceHandoverRules.CheckRequest(request, AppVersionInfo.Current.Version, DateTime.UtcNow, File.Exists) is { } problem)
        {
            Log.Info($"不接交棒請求：{problem}");
            return false;
        }
        Log.Info($"接下交棒請求：{request!.Release}（{request.Executable}）");
        BeginInvoke(() => updateCoordinator.BeginHandover(request.Executable, request.Release));
        return true;
    }

    bool IAppUpdateShell.HandOver(string executable, IReadOnlyList<string> arguments, IReadOnlySet<string> keepDrafts)
    {
        if (Exiting)
            return false;
        Exiting = true;
        var process = UpdateHelperProcess.StartProcess(executable, arguments);
        if (process == null)
        {
            Exiting = false;
            return false;
        }
        process.Dispose();
        _ = HandOverExitAsync(keepDrafts);
        return true;
    }

    private async Task HandOverExitAsync(IReadOnlySet<string> keepDrafts)
    {
        InstanceHandover.ClearRunning(AppPaths.Root);
        await ShutdownAgentsAsync(keepDrafts);
        ExitImmediately();
    }

    private async Task ExitForUpdateAsync(IReadOnlySet<string> keepDrafts)
    {
        if (Exiting)
            return;
        Exiting = true;
        await ShutdownAgentsAsync(keepDrafts);
        ExitImmediately();
    }

    // ───────────────────────────── 結束 ─────────────────────────────

    /// <summary>選單的「結束」、關掉最後一個視窗：走完結束流程後請系統結束這個程序</summary>
    public void RequestExit() => _ = ExitAsync();

    /// <summary>
    /// 結束 app：有執行中的對話、或 CLI 正在安裝／更新時先確認，代理程序正常收尾（最多 8 秒）。
    /// 回 true＝已收尾並結束；false＝使用者按了不結束（或已經在結束、確認框還開著）
    /// </summary>
    public async Task<bool> ExitAsync()
    {
        if (Exiting || exitPromptOpen)
            return false;
        int running = Hub.RunningCount;
        var states = AgentProviders.All.Select(Cli.Get).ToList();
        var warnings = AppControllerRules.ExitWarnings(running, states, Hub.BackgroundWaitingCount);
        while (warnings.Count > 0)
        {
            ShowMain();
            exitPromptOpen = true;   // 確認框開著時再按一次「結束」不會跳第二個
            try
            {
                if (!MacDialogs.Confirm(mainWindow.Window, $"{string.Join("\n", warnings)}\n\n確定要結束嗎？", ShellRules.AppTitle))
                {
                    launchAfterExit = null;
                    return false;
                }
            }
            finally
            {
                exitPromptOpen = false;
            }
            var nowRunning = Hub.RunningCount;
            var nowStates = AgentProviders.All.Select(Cli.Get).ToList();
            if (!AppControllerRules.NeedsExitReconfirm(running, states, nowRunning, nowStates))
                break;
            running = nowRunning;
            states = nowStates;
            warnings = AppControllerRules.ExitWarnings(running, states, Hub.BackgroundWaitingCount);
        }

        Exiting = true;
        var exitAction = updateCoordinator.PrepareExit(windowsShuttingDown: false);
        await ShutdownAgentsAsync(keepDrafts: null);
        if (exitAction == AppExitAction.ApplyOnExit)
            updateCoordinator.LaunchExitHelper();
        if (launchAfterExit != null)
            UpdateHelperProcess.StartProcess(launchAfterExit, Array.Empty<string>())?.Dispose();
        ExitImmediately();
        return true;
    }

    /// <summary>確定結束之後的收尾（Exiting 已設定）：藏起視窗與選單列圖示，代理程序正常收尾最多 8 秒</summary>
    private async Task ShutdownAgentsAsync(IReadOnlySet<string>? keepDrafts)
    {
        Cli.BeginShutdown();
        tray.Visible = false;
        if (mainWindow.Visible)
            Settings.Window = mainWindow.CapturePlacement();
        mainWindow.Hide();
        foreach (var window in extraWindows.ToList())
        {
            RememberWindow(window);
            window.Hide();
        }
        foreach (var popout in popouts.ToList())
            popout.Hide();
        syncStartCancelled = true;
        try
        {
            await Sync.StopAsync(SyncStartupRules.ExitStopTimeout).WaitAsync(SyncStartupRules.ExitStopGuard);
        }
        catch (Exception ex)
        {
            Log.Warn($"停止同步：{ex.Message}");
        }
        try
        {
            await Hub.ShutdownAsync(keepDrafts).WaitAsync(TimeSpan.FromSeconds(8));
        }
        catch (Exception ex)
        {
            Log.Warn($"關閉代理程序：{ex.Message}");
        }
    }

    /// <summary>
    /// 立即結束（初始化失敗、收尾做完之後）：存檔、釋放各服務，再請系統結束這個程序。
    /// 每一步各自接住例外並記下來：哪個服務收尾失敗都不能讓 app 關不掉（視窗已經藏起來，卡住的話使用者只能強制結束）
    /// </summary>
    public void ExitImmediately()
    {
        if (ReadyToTerminate)
            return;
        Exiting = true;
        syncStartCancelled = true;
        ExitStep("網路事件", DetachSyncEvents);
        ExitStep("遠端執行指令", remoteExec.Dispose);
        ExitStep("同步", Sync.Dispose);
        ExitStep("設定存檔", () =>
        {
            foreach (var window in extraWindows.ToList())
                RememberWindow(window);
            Settings.Save();
        });
        ExitStep("更新協調器", updateCoordinator.Dispose);
        ExitStep("更新管理員", updateManager.Dispose);
        ExitStep("換版幫手", updateHelper.Dispose);
        ExitStep("中樞", Hub.Dispose);
        ExitStep("背景活動日誌", backgroundJournal.Dispose);
        ExitStep("電腦狀態", systemMonitor.Dispose);
        ExitStep("語音通知", voice.Dispose);
        ExitStep("語音辨識", voiceTranscriber.Dispose);
        ExitStep("帳戶", Accounts.Dispose);
        ExitStep("CLI", Cli.Dispose);
        ExitStep("縮圖", Thumbnails.Dispose);
        ExitStep("選單列與事件", Dispose);
        ExitStep("視窗", () =>
        {
            foreach (var popout in popouts.ToList())
                popout.Close();
            foreach (var window in extraWindows.ToList())
                window.Close();
            mainWindow.Close();
        });
        ReadyToTerminate = true;
        Log.Info("收尾完成，結束程序");
        Log.Flush();
        NSApplication.SharedApplication.Terminate(NSApplication.SharedApplication);
    }

    /// <summary>結束流程的一步：失敗只記錄，繼續下一步；花太久的也記下來（查「關不掉」用）</summary>
    private static void ExitStep(string name, Action action)
    {
        var watch = Stopwatch.StartNew();
        try
        {
            action();
        }
        catch (Exception ex)
        {
            Log.Error($"結束時「{name}」收尾失敗", ex);
        }
        if (watch.ElapsedMilliseconds >= 500)
            Log.Info($"結束時「{name}」收尾花了 {watch.ElapsedMilliseconds} 毫秒");
    }

    /// <summary>刪掉上次沒送出的草稿留下的資料夾（背景執行緒；刪不乾淨的下次啟動再試）</summary>
    private static void DeleteDraftLeftovers(ConversationStore store, IReadOnlyList<string> conversationIds)
    {
        int deleted = 0;
        foreach (var id in conversationIds)
        {
            if (store.DeleteConversation(id))
                deleted++;
        }
        Log.Info($"清除上次沒送出的草稿資料夾：{deleted}／{conversationIds.Count}");
    }

    private bool disposed;

    public void Dispose()
    {
        if (disposed)
            return;
        disposed = true;
        DetachSyncEvents();
        appearanceObserver?.Dispose();
        appearanceObserver = null;
        foreach (var observer in observers)
            NSWorkspace.SharedWorkspace.NotificationCenter.RemoveObserver(observer);
        observers.Clear();
        tray.Dispose();
        http.Dispose();
    }
}

/// <summary>Program 交給 AppController 的啟動資訊</summary>
/// <param name="ManagedRelease">從受管理的安裝執行時的 release；macOS 目前一律是 null</param>
/// <param name="UsesDefaultData">資料夾是預設的（沒有 --data、MYRIAGENT_AGENT_DATA）</param>
internal sealed record AppStartup(StartupOptions Options, InstallLayout Layout, string? ManagedRelease, bool UsesDefaultData);

/// <summary>await 之後回到主執行緒（AppKit 沒有替主執行緒裝同步內容時用）</summary>
internal sealed class MainThreadSynchronizationContext : SynchronizationContext
{
    public override void Post(SendOrPostCallback d, object? state) => NSApplication.SharedApplication.BeginInvokeOnMainThread(() => d(state));

    public override void Send(SendOrPostCallback d, object? state) => NSApplication.SharedApplication.InvokeOnMainThread(() => d(state));

    public override SynchronizationContext CreateCopy() => new MainThreadSynchronizationContext();
}
