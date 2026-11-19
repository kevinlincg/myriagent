using System.Runtime.InteropServices;
using Myriagent.UI;

namespace Myriagent.RemoteDesk;

/// <summary>
/// 遠端桌面擷取前要知道的系統狀態（只讀；docs/sync-protocol.md 14.16）。2026-11-12 使用者回報從手機開遠端桌面「無法正常執行」：
/// 那台鎖定中，擷取靠「輸入桌面的名稱不是 Default」判斷鎖定——Windows 11 26300 鎖定時輸入桌面照樣是 Default，判斷不出來，
/// DXGI 一張畫面都不給、工作階段一直空轉，看的一方只看到「連線到…」。所以鎖定另外問系統（和 <see cref="ScreenPresence"/> 同一個來源）
/// </summary>
internal static class DeskSystem
{
    /// <summary>多久重問一次系統（擷取每秒二三十次，鎖定不必問得那麼密）</summary>
    public const int RecheckMs = 1000;

    /// <summary>
    /// 這個工作階段現在鎖定中：系統回報鎖定（WTSINFOEX 的 SessionFlags）而且登入畫面的程序在。兩個來源都這麼說才算——
    /// 判斷成鎖定就不擷取，只憑一個來源判斷錯，遠端桌面在那台就永遠是一句「鎖定中」
    /// </summary>
    public static bool Locked()
        => ScreenPresence.NativeSession.Query() is { } session
           && DeskScreenRules.Locked(ScreenPresenceRules.LockedFromSessionFlags(session.Flags), ScreenPresence.NativeSession.LogonUiRunning);

    /// <summary>
    /// 擷取期間螢幕不要關、電腦不要睡（只在擷取執行緒呼叫；要求跟著執行緒）：螢幕關閉時 DWM 不再出畫面，DXGI 等不到新的影格，
    /// 看的一方只看不動時畫面會停在螢幕關閉前的那一張
    /// </summary>
    public static void KeepDisplayOn(bool on)
        => SetThreadExecutionState(on ? EsContinuous | EsDisplayRequired | EsSystemRequired : EsContinuous);

    private const uint EsSystemRequired = 0x00000001;
    private const uint EsDisplayRequired = 0x00000002;
    private const uint EsContinuous = 0x80000000;

    [DllImport("kernel32.dll")]
    private static extern uint SetThreadExecutionState(uint flags);
}
