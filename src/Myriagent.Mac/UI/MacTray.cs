using AppKit;
using CoreGraphics;
using Foundation;
using Myriagent.Core;

namespace Myriagent.UI;

/// <summary>
/// 選單列的圖示（Windows 版是系統匣圖示）：點了出現選單，每次打開時重建，列出每個帳戶目前的用量（不必開視窗就能快速監看）。
/// 執行中與等你回應的數量顯示在圖示旁邊，Dock 的圖示上也有數字（Windows 版是工作列按鈕的疊加圖示）
/// </summary>
internal sealed class MacTray : IDisposable
{
    private readonly NSStatusItem item;
    private readonly NSMenu menu;
    private readonly MenuEvents events;
    private TrayBadge lastBadge = new(TaskbarState.None, string.Empty);
    private bool disposed;

    /// <param name="buildMenu">選單要打開了：把項目加進去（主執行緒）</param>
    public MacTray(Action<NSMenu> buildMenu)
    {
        item = NSStatusBar.SystemStatusBar.CreateStatusItem(NSStatusItemLength.Variable);
        menu = new NSMenu { AutoEnablesItems = false };
        events = new MenuEvents(buildMenu);
        menu.Delegate = events;
        item.Menu = menu;
        if (item.Button is { } button)
        {
            button.Image = StatusImage();
            button.ImagePosition = NSCellImagePosition.ImageLeft;
            button.ToolTip = ShellRules.AppTitle;
        }
    }

    /// <summary>選單列用的小圖（app 圖示縮成選單列的高度）；拿不到圖示就只顯示文字</summary>
    private static NSImage? StatusImage()
    {
        try
        {
            if (NSApplication.SharedApplication.ApplicationIconImage?.Copy() is not NSImage image)
                return null;
            image.Size = new CGSize(18.0, 18.0);
            return image;
        }
        catch (Exception ex)
        {
            Log.Warn($"選單列的圖示建立失敗：{ex.Message}");
            return null;
        }
    }

    public bool Visible
    {
        get => !disposed && item.Visible;
        set
        {
            if (!disposed)
                item.Visible = value;
        }
    }

    /// <summary>提示文字與狀態：等你回應時圖示旁顯示數量、Dock 圖示上也有；只是執行中就顯示一個點</summary>
    public void Update(string tooltip, TrayBadge badge)
    {
        if (disposed)
            return;
        if (item.Button is { } button)
        {
            button.ToolTip = string.IsNullOrWhiteSpace(tooltip) ? ShellRules.AppTitle : tooltip;
            if (badge != lastBadge || button.Image == null)
            {
                button.Title = badge.State switch
                {
                    TaskbarState.Waiting => " " + badge.Text,
                    TaskbarState.Running => " ●",
                    _ => button.Image == null ? "Myriagent" : string.Empty,
                };
            }
        }
        if (badge != lastBadge)
        {
            lastBadge = badge;
            NSApplication.SharedApplication.DockTile.BadgeLabel = badge.State == TaskbarState.Waiting ? badge.Text : null!;
        }
    }

    public void Dispose()
    {
        if (disposed)
            return;
        disposed = true;
        menu.Delegate = null!;
        NSStatusBar.SystemStatusBar.RemoveStatusItem(item);
        NSApplication.SharedApplication.DockTile.BadgeLabel = null!;
    }

    private sealed class MenuEvents(Action<NSMenu> build) : NSMenuDelegate
    {
        public override void NeedsUpdate(NSMenu menu)
        {
            menu.RemoveAllItems();
            try
            {
                build(menu);
            }
            catch (Exception ex)
            {
                Log.Error("建立選單列的選單失敗", ex);
            }
        }

        public override void MenuWillHighlightItem(NSMenu menu, NSMenuItem item)
        {
        }
    }
}

/// <summary>建立選單項目的小工具</summary>
internal static class MacMenuItems
{
    public static NSMenuItem Action(string title, Action action, bool enabled = true)
        => new(title, (_, _) => action()) { Enabled = enabled };

    public static NSMenuItem Disabled(string title) => new(title) { Enabled = false };
}
