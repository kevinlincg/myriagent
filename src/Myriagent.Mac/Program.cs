using AppKit;
using Myriagent.Core;
using Myriagent.UI;
using Myriagent.Update;

namespace Myriagent;

internal static class Program
{
    /// <summary>
    /// 進入點（macOS；Windows 版見 src/Myriagent/Program.cs）。單一實例：已經在執行就叫醒它（帶到前面或開新視窗）後結束；
    /// 它正在結束時等它結束後由這個實例接手（見 SingleInstance）。
    /// 參數 --data &lt;資料夾&gt;：改用指定的資料夾存帳戶與對話（不影響正常資料）。
    /// 另外有幾種不開視窗的用法：對話裡的代理用的小工具（tool-remote、tool-schedule，見 MacTools）、帳戶委派的 MCP 橋接（--delegation-mcp）
    /// </summary>
    private static int Main(string[] args)
    {
        // 這幾種不是桌面實例：不讀設定、不取得單一實例鎖，也不啟動視窗
        if (args.Length >= 1 && args[0] == MacTools.RemoteCommand)
            return MyriagentRemote.Program.Main(args[1..]);
        if (args.Length >= 1 && args[0] == MacTools.ScheduleCommand)
            return MyriagentSchedule.Program.Main(args[1..]);
        if (args.Length == 2 && args[0] == "--delegation-mcp")
        {
            using var input = new StreamReader(Console.OpenStandardInput(), new System.Text.UTF8Encoding(false));
            using var output = new StreamWriter(Console.OpenStandardOutput(), new System.Text.UTF8Encoding(false)) { AutoFlush = true, NewLine = "\n" };
            return Agents.DelegationMcpBridge.RunAsync(args[1], input, output).GetAwaiter().GetResult();
        }

        var startup = System.Diagnostics.Stopwatch.StartNew();
        var options = StartupOptions.Parse(args);
        // 資料夾正規化成完整路徑並固定下來：相對路徑不會隨工作目錄改變
        var root = ShellRules.NormalizeDataRoot(options.DataRoot ?? AppPaths.Root);
        AppPaths.Override(root);

        // app 自我更新與安裝的參數是 Windows 受管理安裝用的：macOS 還沒有對應的流程
        if (options.ApplyUpdate || options.InstallHere || options.InstallPackage || options.Uninstall)
        {
            Log.Warn("macOS 版還不支援 app 自我更新與安裝的參數");
            Log.Flush();
            return 2;
        }

        var layout = InstallLayout.Resolve();
        // 不同資料夾（--data 或 MYRIAGENT_AGENT_DATA）視為不同實例
        var instanceSuffix = ShellRules.InstanceSuffix(root, AppPaths.DefaultRoot);
        var self = AppVersionInfo.Current;
        // 使用者再開一次 app（不是登入時自動啟動）：已在執行的實例開一個新視窗
        var instance = SingleInstance.Acquire(instanceSuffix, out _, requestNewWindow: !options.AutoStart);
        if (instance == null)
        {
            Log.Flush();
            return 0;
        }

        Log.Cleanup();
        Log.Info($"啟動 Clode Myriagent {AppVersionInfo.Display}（macOS），資料夾 {AppPaths.Root}");
        // 記下自己（版本與執行檔）：之後開的較新版本據此請這一份交棒
        InstanceHandover.MarkRunning(root, self);
        // 從 Finder、Dock 啟動時 PATH 只有系統的資料夾：先套用使用者登入殼層的 PATH，之後才找得到 claude、codex、node、git
        MacEnvironment.ImportLoginShellPath(TimeSpan.FromSeconds(8));

        AppDomain.CurrentDomain.UnhandledException += (_, e) =>
        {
            Log.Error("未處理的例外", e.ExceptionObject as Exception);
            Log.Flush();   // 程序接著就會結束，不能等背景寫檔
        };
        TaskScheduler.UnobservedTaskException += (_, e) =>
        {
            Log.Error("未觀察的工作例外", e.Exception);
            e.SetObserved();
        };

        NSApplication.Init();
        var application = NSApplication.SharedApplication;
        application.ActivationPolicy = NSApplicationActivationPolicy.Regular;
        application.Delegate = new AppDelegate(startup, new AppStartup(options, layout, ManagedRelease: null, UsesDefaultData: instanceSuffix.Length == 0), instance);
        application.Run();
        // 平常不會走到這裡（系統結束程序時不回到 Main，收尾在 AppDelegate.WillTerminate）
        InstanceHandover.ClearRunning(root);
        instance.Dispose();
        return 0;
    }
}
