using System.Runtime.InteropServices;
using AppKit;
using CoreGraphics;
using Myriagent.Core;

namespace Myriagent.UI;

/// <summary>外殼要問系統的幾件事，macOS 版（Windows 版見 src/Myriagent/UI/NativeMethods.cs）</summary>
internal static class NativeMethods
{
    /// <summary>Esc 鍵的虛擬鍵碼（kVK_Escape）</summary>
    private const ushort EscapeKeyCode = 53;

    /// <summary>「任何輸入事件」（kCGAnyInputEventType）</summary>
    private const CGEventType AnyInputEvent = unchecked((CGEventType)uint.MaxValue);

    /// <summary>系統目前是深色外觀（主題「跟隨系統」時用）。主執行緒</summary>
    public static bool IsSystemDarkTheme()
    {
        try
        {
            var best = NSApplication.SharedApplication.EffectiveAppearance.FindBestMatch(
                new[] { NSAppearance.NameAqua.ToString(), NSAppearance.NameDarkAqua.ToString() });
            return best == NSAppearance.NameDarkAqua.ToString();
        }
        catch (Exception)
        {
            return true;
        }
    }

    /// <summary>使用者多久沒有動滑鼠鍵盤了（整個登入階段，不只這個 app）</summary>
    public static TimeSpan IdleTime()
    {
        try
        {
            return TimeSpan.FromSeconds(Math.Max(0, CGEventSource.GetSecondsSinceLastEventType(CGEventSourceStateID.CombinedSession, AnyInputEvent)));
        }
        catch (Exception)
        {
            return TimeSpan.Zero;
        }
    }

    /// <summary>Esc 現在按著（拖曳分頁時按 Esc 取消）</summary>
    public static bool IsEscapeDown()
    {
        try
        {
            return CGEventSource.GetKeyState(CGEventSourceStateID.CombinedSession, EscapeKeyCode);
        }
        catch (Exception)
        {
            return false;
        }
    }

    /// <summary>
    /// 立即鎖定螢幕（設定「自動啟動後鎖定電腦」）。系統沒有公開的 API：用登入視窗的 SACLockScreenImmediate
    /// （選單列「鎖定螢幕」背後的同一個函式）；哪一版系統拿掉了就回 false
    /// </summary>
    public static bool LockWorkstation()
    {
        try
        {
            return SACLockScreenImmediate() == 0;
        }
        catch (Exception ex) when (ex is DllNotFoundException or EntryPointNotFoundException)
        {
            Log.Warn($"這一版 macOS 鎖定不了螢幕：{ex.Message}");
            return false;
        }
    }

    [DllImport("/System/Library/PrivateFrameworks/login.framework/Versions/Current/login")]
    private static extern int SACLockScreenImmediate();
}
