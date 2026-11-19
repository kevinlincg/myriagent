using AppKit;

namespace Myriagent.UI;

/// <summary>系統的訊息框（Windows 版的 MessageBox）</summary>
internal static class MacDialogs
{
    /// <summary>
    /// 顯示訊息框並等使用者按（強制回應）。回傳按了第幾個按鈕（0 起算）；第一個是預設按鈕（Enter），名稱是「取消」的那個接 Esc。
    /// 主執行緒呼叫。window 只用來把訊息框放在那個視窗所在的螢幕（不做成附在視窗上的表單：呼叫的地方要同步拿到答案）
    /// </summary>
    public static int Show(NSWindow? window, string message, string title, NSAlertStyle style, params string[] buttons)
    {
        using var alert = new NSAlert
        {
            MessageText = string.IsNullOrWhiteSpace(title) ? ShellRules.AppTitle : title,
            InformativeText = message ?? string.Empty,
            AlertStyle = style,
        };
        foreach (var button in buttons.Length == 0 ? new[] { "好" } : buttons)
            alert.AddButton(button);
        NSApplication.SharedApplication.ActivateIgnoringOtherApps(true);
        _ = window;
        long response = alert.RunModal();
        long index = response - (long)NSAlertButtonReturn.First;
        return index < 0 ? 0 : (int)index;
    }

    /// <summary>是／否的確認（Windows 版的 MessageBoxButtons.YesNo）：回 true＝是</summary>
    public static bool Confirm(NSWindow? window, string message, string title, NSAlertStyle style = NSAlertStyle.Warning)
        => Show(window, message, title, style, "是", "否") == 0;
}
