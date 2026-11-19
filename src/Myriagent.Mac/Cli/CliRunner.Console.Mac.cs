using System.Diagnostics;
using Myriagent.Agents;
using Myriagent.Core;

namespace Myriagent.Cli;

internal static partial class CliRunner
{
    /// <summary>
    /// 在「終端機」開新視窗執行（登入、登出這類要使用者在畫面上操作的流程；Windows 版是新的主控台視窗）。
    /// 回傳的是 app 這邊等待的程序（TerminalConsoleRules 的 watch.sh）：它的結束碼就是指令的結束碼，結束它（逾時、取消）會一併結束視窗裡的指令
    /// </summary>
    public static Process StartConsole(AgentLauncher launcher, IEnumerable<string> args, Action<IDictionary<string, string?>>? configureEnvironment, string? title = null)
    {
        var psi = new ProcessStartInfo("/bin/sh") { UseShellExecute = false };
        var before = new Dictionary<string, string?>(psi.Environment, StringComparer.Ordinal);
        configureEnvironment?.Invoke(psi.Environment);
        var after = new Dictionary<string, string?>(psi.Environment, StringComparer.Ordinal);
        var (set, unset) = TerminalConsoleRules.Changes(before, after);

        var folder = Path.Combine(Path.GetTempPath(), "myriagent-console", Guid.NewGuid().ToString("N"));
        Directory.CreateDirectory(folder);
        var executable = UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute;
        var run = Path.Combine(folder, TerminalConsoleRules.RunFile);
        File.WriteAllText(run, TerminalConsoleRules.RunScript(folder, title, launcher.FileName, launcher.PrefixArguments.Concat(args), set, unset, AppPaths.CacheDirectory));
        File.SetUnixFileMode(run, executable);
        var watch = Path.Combine(folder, TerminalConsoleRules.WatchFile);
        File.WriteAllText(watch, TerminalConsoleRules.WatchScript(folder));
        File.SetUnixFileMode(watch, executable);

        // 等待的程序本身不需要帳戶的環境（指令在終端機裡跑，環境寫在 run.command）
        foreach (var key in after.Keys.Where(k => !before.ContainsKey(k)))
            psi.Environment.Remove(key);
        foreach (var (key, value) in before)
            psi.Environment[key] = value;
        psi.ArgumentList.Add(watch);
        var process = Process.Start(psi) ?? throw new InvalidOperationException($"無法開啟{title ?? "終端機"}視窗");
        process.EnableRaisingEvents = true;
        return process;
    }
}
