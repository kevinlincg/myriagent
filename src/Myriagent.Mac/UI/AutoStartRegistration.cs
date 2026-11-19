using System.Security;
using Myriagent.Core;
using Myriagent.Hub;

namespace Myriagent.UI;

/// <summary>
/// 登入時自動啟動，macOS 版（Windows 版寫登錄的 Run 機碼，見 src/Myriagent/UI/AutoStartRegistration.cs）：
/// 在 <c>~/Library/LaunchAgents</c> 放一個登入時執行的項目（不需要系統管理員），內容是用 <c>open</c> 打開這個 app 並帶 <c>--autostart</c>。
/// 狀態以那個檔案為準、不存在 settings.json（和 Windows 版一樣：系統設定的「登入項目」也看得到、關得掉它）。
/// app 搬了位置之後檔案裡的路徑會過時：<see cref="IsEnabled"/> 發現不一樣就當成沒開，使用者在設定頁再開一次就更新
/// </summary>
internal sealed class AutoStartRegistration : IAutoStart
{
    private const string Label = "dev.clode.myriagent.autostart";

    private readonly string? bundlePath;

    /// <param name="bundlePath">.app 的路徑；不是從 .app 執行（開發時直接跑執行檔）是 null</param>
    /// <param name="available">這次執行能不能設定：false 時只讀不寫</param>
    public AutoStartRegistration(string? bundlePath, bool available)
    {
        this.bundlePath = bundlePath;
        Available = available && bundlePath != null;
    }

    public bool Available { get; }

    private static string PlistPath
        => Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.UserProfile), "Library", "LaunchAgents", Label + ".plist");

    public bool IsEnabled()
    {
        try
        {
            return bundlePath != null && File.Exists(PlistPath) && File.ReadAllText(PlistPath).Contains(Escape(bundlePath), StringComparison.Ordinal);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or SecurityException)
        {
            Log.Warn($"讀登入時自動啟動的設定失敗：{ex.Message}");
            return false;
        }
    }

    public void SetEnabled(bool enabled)
    {
        if (!Available || bundlePath == null)
            throw new InvalidOperationException("這次執行不能設定登入時自動啟動");
        if (enabled)
        {
            Directory.CreateDirectory(Path.GetDirectoryName(PlistPath)!);
            File.WriteAllText(PlistPath, Plist(bundlePath));
            Log.Info($"已開啟登入時自動啟動：{bundlePath}");
        }
        else
        {
            Remove();
            Log.Info("已關閉登入時自動啟動");
        }
    }

    /// <summary>刪掉登入項目（關閉時；沒有就什麼都不做）</summary>
    public static void Remove() => File.Delete(PlistPath);

    /// <summary>登入項目的內容：登入時（RunAtLoad）執行一次 <c>open -a 〈app〉 --args --autostart</c></summary>
    internal static string Plist(string bundlePath) => $"""
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
          <key>Label</key>
          <string>{Label}</string>
          <key>ProgramArguments</key>
          <array>
            <string>/usr/bin/open</string>
            <string>-a</string>
            <string>{Escape(bundlePath)}</string>
            <string>--args</string>
            <string>{AutoStartRules.Argument}</string>
          </array>
          <key>RunAtLoad</key>
          <true/>
          <key>ProcessType</key>
          <string>Interactive</string>
        </dict>
        </plist>

        """;

    private static string Escape(string text) => SecurityElement.Escape(text) ?? string.Empty;
}
