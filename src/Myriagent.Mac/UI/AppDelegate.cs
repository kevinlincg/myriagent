using System.Diagnostics;
using AppKit;
using Foundation;
using Myriagent.Core;
using Myriagent.Update;

namespace Myriagent.UI;

/// <summary>
/// AppKit 的 app 委派：事件迴圈起來之後建立 AppController，接系統的幾件事——
/// 再開一次 app（點 Dock 的圖示、在 Finder 再開一次）、結束（⌘Q、Dock 的「結束」、登出與關機）、另一個實例的喚醒
/// </summary>
internal sealed class AppDelegate(Stopwatch startup, AppStartup appStartup, SingleInstance instance) : NSApplicationDelegate
{
    private AppController? app;
    private bool terminationPending;

    public override void DidFinishLaunching(NSNotification notification)
    {
        try
        {
            app = new AppController(startup, appStartup);
        }
        catch (Exception ex)
        {
            Log.Error("啟動失敗", ex);
            Log.Flush();
            MacDialogs.Show(null, $"啟動失敗：{ex.Message}", ShellRules.AppTitle, NSAlertStyle.Critical, "好");
            Environment.Exit(1);
            return;
        }
        var controller = app;
        // 正在結束（收尾代理程序）時不回應喚醒：新啟動的實例會等這個實例結束後接手
        instance.Listen(() => !controller.Exiting, () => controller.BeginInvoke(controller.ShowMain), controller.AcceptHandover,
            () => controller.BeginInvoke(controller.OpenWindowFromLaunch));
    }

    /// <summary>關掉所有視窗不結束：對話在背景繼續，選單列的圖示與 Dock 都叫得回來（要不要結束由視窗的關閉規則決定）</summary>
    public override bool ApplicationShouldTerminateAfterLastWindowClosed(NSApplication sender) => false;

    /// <summary>點 Dock 的圖示、在 Finder 再開一次：沒有看得到的視窗就把主視窗叫出來</summary>
    public override bool ApplicationShouldHandleReopen(NSApplication sender, bool hasVisibleWindows)
    {
        if (!hasVisibleWindows)
            app?.ShowMain();
        return true;
    }

    /// <summary>
    /// 系統要結束這個 app（⌘Q、Dock 的「結束」、登出、關機）：收尾還沒做就先做（有執行中的對話先確認、代理程序收尾），
    /// 做完才回答可以結束；使用者按了不結束就回答不行（登出也跟著取消）
    /// </summary>
    public override NSApplicationTerminateReply ApplicationShouldTerminate(NSApplication sender)
    {
        if (app == null || app.ReadyToTerminate)
            return NSApplicationTerminateReply.Now;
        if (terminationPending)
            return NSApplicationTerminateReply.Later;
        if (app.Exiting)
        {
            // 收尾進行中（從選單按了結束）：做完它自己會再請系統結束一次
            return NSApplicationTerminateReply.Cancel;
        }
        terminationPending = true;
        _ = ReplyWhenDoneAsync(sender, app);
        return NSApplicationTerminateReply.Later;
    }

    private async Task ReplyWhenDoneAsync(NSApplication sender, AppController controller)
    {
        bool exited = false;
        try
        {
            exited = await controller.ExitAsync();
        }
        catch (Exception ex)
        {
            Log.Error("結束時失敗", ex);
            exited = true;
        }
        terminationPending = false;
        // ExitAsync 收尾完會自己請系統結束（那一次直接回「現在」）；這裡回覆原本這一次的詢問
        controller.BeginInvoke(() => sender.ReplyToApplicationShouldTerminate(exited));
    }

    public override void WillTerminate(NSNotification notification)
    {
        InstanceHandover.ClearRunning(AppPaths.Root);
        instance.Dispose();
        Log.Flush();
    }
}
