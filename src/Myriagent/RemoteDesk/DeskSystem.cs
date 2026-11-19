
namespace Myriagent.RemoteDesk;

/// <summary>這台的鎖定畫面支援，用來決定擷取不到時怎麼說明（<see cref="DeskScreenRules.StatusText"/>）</summary>
internal enum DeskServiceState
{
    /// <summary>不知道（畫面來源不經鎖定畫面支援）：照原本的說法</summary>
    Unknown,

    /// <summary>沒裝：告訴看的一方怎麼裝</summary>
    NotInstalled,

    /// <summary>裝了但連不上、或幫手也擷取不到</summary>
    Unavailable,
}

/// <summary>畫面來源知道這台的鎖定畫面支援的狀態（<see cref="SecureDeskScreen"/>）</summary>
internal interface IDeskServiceStateSource
{
    DeskServiceState ServiceState { get; }
}

/// <summary>遠端桌面擷取的判斷（純函式）</summary>
internal static class DeskScreenRules
{
    /// <summary>DXGI 建立後連續幾次（每次等 0.5 秒）沒有第一張畫面就叫醒螢幕</summary>
    public const int WakeAfterMisses = 1;

    /// <summary>連續幾次沒有第一張畫面就改用 GDI（叫醒之後再給兩次機會：螢幕醒來 DWM 才開始出畫面）</summary>
    public const int FallBackAfterMisses = 3;

    /// <summary>擷取一直要重來（<see cref="DeskCapture.Retry"/>）連續幾次之後告訴看的一方（每次約 0.5 秒）</summary>
    public const int RetryStatusAfter = 6;

    /// <summary>
    /// 擷取不到的期間多久重送一次說明：這段期間沒有影格也沒有確認，伺服器 <see cref="Myriagent.Remote.DeskRules.IdleForget"/> 沒有封包就忘掉工作階段
    /// </summary>
    public const int StatusRepeatMs = 60_000;

    /// <summary>鎖定：系統回報鎖定，而且登入畫面的程序在（<paramref name="logonUiRunning"/> 只在系統回報鎖定時才問：列程序比較貴）</summary>
    public static bool Locked(bool flagsSayLocked, Func<bool> logonUiRunning) => flagsSayLocked && logonUiRunning();

    /// <summary>沒有第一張畫面的第 <paramref name="misses"/> 次要做什麼</summary>
    public static (bool Wake, bool FallBack) OnNoFirstFrame(int misses)
        => (misses == WakeAfterMisses, misses >= FallBackAfterMisses);

    /// <summary>
    /// 擷取不到時給看的一方的說明；可以擷取（或只是正在重來、還沒到 <see cref="RetryStatusAfter"/>）回 null。
    /// <paramref name="service"/> 是這台的鎖定畫面支援（2026-11-17 使用者要從遠端解鎖：以前只說「看不到也操作不了」，沒說裝了服務就可以）
    /// </summary>
    public static string? StatusText(DeskCapture captured, int retries, DeskServiceState service = DeskServiceState.Unknown) => captured switch
    {
        DeskCapture.Locked => service switch
        {
            DeskServiceState.NotInstalled => LockedNoServiceText,
            DeskServiceState.Unavailable => LockedServiceDownText,
            _ => LockedText,
        },
        DeskCapture.Failed => service switch
        {
            DeskServiceState.NotInstalled => SecureDesktopNoServiceText,
            DeskServiceState.Unavailable => SecureDesktopServiceDownText,
            _ => SecureDesktopText,
        },
        DeskCapture.Retry when retries >= RetryStatusAfter => RetryText,
        _ => null,
    };

    /// <summary>怎麼裝鎖定畫面支援（給看的一方；設定頁的那一區在「多電腦同步」）</summary>
    public const string InstallHint = "要能從遠端看鎖定畫面、輸入 PIN 或密碼解鎖：在那台電腦解除鎖定後，到「設定」→「多電腦同步」→「遠端桌面的鎖定畫面支援」按「安裝…」，"
        + "並在那台同意 Windows 的系統管理員確認（每台只要做一次）";

    public const string LockedNoServiceText = "那台電腦目前鎖定中，而且還沒安裝「鎖定畫面支援」，所以遠端看不到也操作不了鎖定畫面。" + InstallHint + "。現在要有人在那台解除鎖定，解除之後這裡會自動恢復";

    public const string LockedServiceDownText = "那台電腦目前鎖定中。那台裝了「鎖定畫面支援」，但這次連不上它（正在重試）；一直這樣的話，請在那台解除鎖定後到「設定」→「多電腦同步」→「遠端桌面的鎖定畫面支援」看狀態，必要時重新安裝";

    public const string SecureDesktopNoServiceText = "那台目前是系統管理員的確認畫面（或其他受保護的畫面），那台還沒安裝「鎖定畫面支援」，遠端看不到也操作不了。" + InstallHint + "。那台的人處理之後這裡就會恢復";

    public const string SecureDesktopServiceDownText = "那台目前是系統管理員的確認畫面（或其他受保護的畫面）。那台裝了「鎖定畫面支援」，但這次連不上它（正在重試）；那台的人處理之後這裡也會恢復";

    public const string LockedText = "那台電腦目前鎖定中：鎖定畫面遠端看不到也操作不了。要有人在那台電腦前解除鎖定，解除之後這裡會自動恢復";

    public const string SecureDesktopText = "那台目前是系統管理員的確認畫面（或其他受保護的畫面），遠端看不到也操作不了；那台的人處理之後就會恢復";

    public const string RetryText = "那台暫時擷取不到畫面（螢幕可能關著或顯示卡正在重設），正在重試";
}
