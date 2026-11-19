using AppKit;
using Foundation;
using ObjCRuntime;

namespace Myriagent.UI;

/// <summary>
/// 螢幕最上面的選單列。macOS 的 app 一定要有：複製、貼上、全選這些快速鍵是選單項目的快速鍵（沒有「編輯」選單，
/// 網頁裡 ⌘C、⌘V 都沒有作用），⌘Q、⌘W、⌘M、⌘H 也一樣。項目的動作沒有指定對象的會交給目前有焦點的元件（網頁、對話框的文字欄）
/// </summary>
internal static class MacMainMenu
{
    public static NSMenu Build(AppController app)
    {
        var main = new NSMenu();

        var appMenu = new NSMenu(ShellRules.AppTitle);
        appMenu.AddItem(MacMenuItems.Action("開啟 Clode Myriagent", app.ShowMain));
        appMenu.AddItem(MacMenuItems.Action("帳戶儀表板", app.ShowDashboard));
        appMenu.AddItem(NSMenuItem.SeparatorItem);
        appMenu.AddItem(Standard("隱藏 Clode Myriagent", "hide:", "h"));
        var hideOthers = Standard("隱藏其他", "hideOtherApplications:", "h");
        hideOthers.KeyEquivalentModifierMask = NSEventModifierMask.CommandKeyMask | NSEventModifierMask.AlternateKeyMask;
        appMenu.AddItem(hideOthers);
        appMenu.AddItem(Standard("顯示全部", "unhideAllApplications:", string.Empty));
        appMenu.AddItem(NSMenuItem.SeparatorItem);
        // 走系統的結束流程（AppDelegate 的 applicationShouldTerminate：有執行中的對話先確認、代理程序收尾）
        appMenu.AddItem(Standard("結束 Clode Myriagent", "terminate:", "q"));
        main.AddItem(new NSMenuItem(ShellRules.AppTitle) { Submenu = appMenu });

        var file = new NSMenu("檔案");
        file.AddItem(MacMenuItems.Action("新視窗", app.OpenNewWindow));
        file.AddItem(NSMenuItem.SeparatorItem);
        file.AddItem(Standard("關閉視窗", "performClose:", "w"));
        main.AddItem(new NSMenuItem("檔案") { Submenu = file });

        var edit = new NSMenu("編輯");
        edit.AddItem(Standard("還原", "undo:", "z"));
        var redo = Standard("重做", "redo:", "z");
        redo.KeyEquivalentModifierMask = NSEventModifierMask.CommandKeyMask | NSEventModifierMask.ShiftKeyMask;
        edit.AddItem(redo);
        edit.AddItem(NSMenuItem.SeparatorItem);
        edit.AddItem(Standard("剪下", "cut:", "x"));
        edit.AddItem(Standard("拷貝", "copy:", "c"));
        // 貼上先讓視窗登記剪貼簿裡的檔案（在 Finder 複製的），再照常貼：網頁才找得到它們的路徑（見 WebBridge）
        edit.AddItem(new NSMenuItem("貼上", "v", (sender, _) => app.Paste(sender as NSObject)));
        var pastePlain = Standard("貼上並符合樣式", "pasteAsPlainText:", "v");
        pastePlain.KeyEquivalentModifierMask = NSEventModifierMask.CommandKeyMask | NSEventModifierMask.AlternateKeyMask | NSEventModifierMask.ShiftKeyMask;
        edit.AddItem(pastePlain);
        edit.AddItem(Standard("全選", "selectAll:", "a"));
        main.AddItem(new NSMenuItem("編輯") { Submenu = edit });

        var window = new NSMenu("視窗");
        window.AddItem(Standard("縮到最小", "performMiniaturize:", "m"));
        window.AddItem(Standard("縮放", "performZoom:", string.Empty));
        window.AddItem(NSMenuItem.SeparatorItem);
        window.AddItem(Standard("全部移到最上層", "arrangeInFront:", string.Empty));
        main.AddItem(new NSMenuItem("視窗") { Submenu = window });
        // 系統會在這個選單底下列出開著的視窗
        NSApplication.SharedApplication.WindowsMenu = window;

        return main;
    }

    /// <summary>標準動作的項目：不指定對象，交給目前有焦點的元件</summary>
    private static NSMenuItem Standard(string title, string selector, string key) => new(title, new Selector(selector), key);
}
