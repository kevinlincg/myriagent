using System.Runtime.InteropServices;

namespace Myriagent.RemoteDesk;

/// <summary>
/// 遠端桌面擷取前要知道的系統狀態，macOS 版（Windows 版見 src/Myriagent/RemoteDesk/DeskSystem.Win.cs；對外的成員相同）
/// </summary>
internal static class DeskSystem
{
    /// <summary>多久重問一次系統（擷取每秒二三十次，鎖定不必問得那麼密）</summary>
    public const int RecheckMs = 1000;

    private const string CoreGraphics = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics";
    private const string CoreFoundation = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation";
    private const string IOKit = "/System/Library/Frameworks/IOKit.framework/IOKit";
    private const uint CFStringEncodingUtf8 = 0x08000100;
    private const uint AssertionLevelOn = 255;

    [ThreadStatic]
    private static uint displayAssertion;

    /// <summary>這個工作階段現在鎖定中：系統的工作階段資訊（CGSessionCopyCurrentDictionary）有 CGSSessionScreenIsLocked；讀不到當成沒鎖</summary>
    public static bool Locked()
    {
        var session = CGSessionCopyCurrentDictionary();
        if (session == IntPtr.Zero)
            return false;
        var key = CFStringCreateWithCString(IntPtr.Zero, "CGSSessionScreenIsLocked", CFStringEncodingUtf8);
        try
        {
            // 鎖定時才有這個鍵（值是 true）：有就算鎖定
            return key != IntPtr.Zero && CFDictionaryContainsKey(session, key) != 0;
        }
        finally
        {
            if (key != IntPtr.Zero)
                CFRelease(key);
            CFRelease(session);
        }
    }

    /// <summary>
    /// 擷取期間螢幕不要關、電腦不要睡（只在擷取執行緒呼叫；要求跟著執行緒，和 Windows 版一樣）：螢幕關閉時系統不再出畫面，
    /// 看的一方只看不動時畫面會停在螢幕關閉前的那一張
    /// </summary>
    public static void KeepDisplayOn(bool on)
    {
        if (on)
        {
            if (displayAssertion != 0)
                return;
            var type = CFStringCreateWithCString(IntPtr.Zero, "PreventUserIdleDisplaySleep", CFStringEncodingUtf8);
            var reason = CFStringCreateWithCString(IntPtr.Zero, "Clode Myriagent 遠端桌面", CFStringEncodingUtf8);
            try
            {
                if (type != IntPtr.Zero && reason != IntPtr.Zero && IOPMAssertionCreateWithName(type, AssertionLevelOn, reason, out var id) == 0)
                    displayAssertion = id;
            }
            finally
            {
                if (type != IntPtr.Zero)
                    CFRelease(type);
                if (reason != IntPtr.Zero)
                    CFRelease(reason);
            }
        }
        else if (displayAssertion != 0)
        {
            _ = IOPMAssertionRelease(displayAssertion);
            displayAssertion = 0;
        }
    }

    [DllImport(CoreGraphics)]
    private static extern IntPtr CGSessionCopyCurrentDictionary();

    [DllImport(CoreFoundation)]
    private static extern IntPtr CFStringCreateWithCString(IntPtr allocator, [MarshalAs(UnmanagedType.LPUTF8Str)] string text, uint encoding);

    [DllImport(CoreFoundation)]
    private static extern byte CFDictionaryContainsKey(IntPtr dictionary, IntPtr key);

    [DllImport(CoreFoundation)]
    private static extern void CFRelease(IntPtr value);

    [DllImport(IOKit)]
    private static extern int IOPMAssertionCreateWithName(IntPtr assertionType, uint level, IntPtr reason, out uint assertionId);

    [DllImport(IOKit)]
    private static extern int IOPMAssertionRelease(uint assertionId);
}
