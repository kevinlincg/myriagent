using Foundation;
using Myriagent.Core;

namespace Myriagent.UI;

/// <summary>
/// 對話裡的代理用的兩個小工具（myriagent-remote：在其他電腦執行指令；myriagent-schedule：定時訊息）。
/// Windows 版是兩個獨立的 exe，放在 app 的資料夾。macOS 的 app 把 .NET 包在 .app 裡，獨立的小程式沒有執行階段可用：
/// 兩個工具編進 app 的執行檔（Program.cs 的 tool-remote、tool-schedule），這裡在資料夾放兩支同名的小腳本轉呼叫，
/// 中樞把那個資料夾放到對話 CLI 的 PATH 最前面，代理照樣打 <c>myriagent-remote …</c>
/// </summary>
internal static class MacTools
{
    public const string RemoteCommand = "tool-remote";
    public const string ScheduleCommand = "tool-schedule";

    private const string RemoteName = "myriagent-remote";
    private const string ScheduleName = "myriagent-schedule";

    /// <summary>.app 的路徑；不是從 .app 執行（開發時直接跑執行檔）是 null</summary>
    public static string? BundlePath
    {
        get
        {
            try
            {
                var path = NSBundle.MainBundle.BundlePath;
                return !string.IsNullOrEmpty(path) && path.EndsWith(".app", StringComparison.OrdinalIgnoreCase) ? path : null;
            }
            catch (Exception)
            {
                return null;
            }
        }
    }

    /// <summary>在 <paramref name="directory"/> 放好兩支腳本（內容沒變就不重寫）；回那個資料夾，放不了回 null（代理就沒有這兩個工具）</summary>
    public static string? Install(string directory)
    {
        var executable = Environment.ProcessPath;
        if (string.IsNullOrEmpty(executable))
            return null;
        try
        {
            Directory.CreateDirectory(directory);
            Write(Path.Combine(directory, RemoteName), executable, RemoteCommand);
            Write(Path.Combine(directory, ScheduleName), executable, ScheduleCommand);
            return directory;
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            Log.Warn($"放不了代理用的小工具：{ex.Message}");
            return null;
        }
    }

    /// <summary>腳本的內容：以 app 的執行檔執行對應的工具，參數原樣轉過去（路徑以單引號包住，路徑裡的單引號另外跳脫）</summary>
    internal static string Script(string executable, string command)
        => "#!/bin/sh\nexec '" + executable.Replace("'", "'\\''", StringComparison.Ordinal) + "' " + command + " \"$@\"\n";

    private static void Write(string path, string executable, string command)
    {
        var script = Script(executable, command);
        if (File.Exists(path) && File.ReadAllText(path) == script)
            return;
        File.WriteAllText(path, script);
        File.SetUnixFileMode(path, UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute
                                   | UnixFileMode.GroupRead | UnixFileMode.GroupExecute | UnixFileMode.OtherRead | UnixFileMode.OtherExecute);
    }
}
