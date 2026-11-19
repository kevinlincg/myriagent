using AppKit;
using CoreGraphics;

namespace Myriagent.UI;

/// <summary>
/// 螢幕與視窗位置的換算。共用的規則（ShellRules、AppControllerRules）與 settings.json 存的視窗位置用的是 Windows 的座標：
/// 原點在主螢幕左上角、y 往下、整數。AppKit 的原點在主螢幕左下角、y 往上、單位是點（Retina 螢幕一點是兩個像素，視窗大小本來就以點計）。
/// 這裡只把 y 翻過來，數值的單位就是點：規則裡「邏輯像素依 DPI 換算」的地方一律給 96（不縮放）
/// </summary>
internal static class MacScreens
{
    /// <summary>給共用規則的 DPI：macOS 的視窗大小以點計，不必依螢幕縮放換算</summary>
    public const int Dpi = 96;

    /// <summary>主螢幕（有選單列的那個，座標原點）的高度</summary>
    private static double PrimaryHeight => NSScreen.Screens is { Length: > 0 } screens ? screens[0].Frame.Height : 0;

    /// <summary>AppKit 的矩形 → 左上原點的整數矩形</summary>
    public static Rectangle ToTopLeft(CGRect rect)
        => new((int)Math.Round((double)rect.X), (int)Math.Round(PrimaryHeight - (double)rect.Y - (double)rect.Height),
            (int)Math.Round((double)rect.Width), (int)Math.Round((double)rect.Height));

    /// <summary>左上原點的矩形 → AppKit 的矩形</summary>
    public static CGRect FromTopLeft(Rectangle rect)
        => new((double)rect.X, PrimaryHeight - rect.Y - rect.Height, (double)rect.Width, (double)rect.Height);

    /// <summary>AppKit 的點 → 左上原點的整數點</summary>
    public static Point ToTopLeft(CGPoint point) => new((int)Math.Round((double)point.X), (int)Math.Round(PrimaryHeight - (double)point.Y));

    /// <summary>各螢幕扣掉選單列與 Dock 之後可以放視窗的範圍（左上原點）</summary>
    public static List<Rectangle> WorkingAreas() => (NSScreen.Screens ?? Array.Empty<NSScreen>()).Select(s => ToTopLeft(s.VisibleFrame)).ToList();

    /// <summary>主螢幕可以放視窗的範圍；沒有螢幕時給一個合理的預設</summary>
    public static Rectangle PrimaryWorkingArea() => NSScreen.MainScreen is { } main ? ToTopLeft(main.VisibleFrame) : new Rectangle(0, 0, 1600, 900);

    /// <summary>滑鼠游標的位置（左上原點）</summary>
    public static Point Cursor() => ToTopLeft(NSEvent.CurrentMouseLocation);

    /// <summary>游標所在螢幕可以放視窗的範圍</summary>
    public static Rectangle WorkingAreaAtCursor()
    {
        var cursor = Cursor();
        var areas = WorkingAreas();
        var hit = areas.FirstOrDefault(a => a.Contains(cursor));
        return hit.IsEmpty ? PrimaryWorkingArea() : hit;
    }
}
