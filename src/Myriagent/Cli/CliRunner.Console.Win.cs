using System.Diagnostics;
using Myriagent.Agents;

namespace Myriagent.Cli;

internal static partial class CliRunner
{
    /// <summary>開新的主控台視窗執行（登入、官方安裝程式這類要使用者看到或操作的流程）；回傳程序供呼叫端等它結束</summary>
    public static Process StartConsole(AgentLauncher launcher, IEnumerable<string> args, Action<IDictionary<string, string?>>? configureEnvironment, string? title = null)
    {
        var psi = new ProcessStartInfo
        {
            FileName = launcher.FileName,
            UseShellExecute = false,
            CreateNoWindow = false,
        };
        foreach (var a in launcher.PrefixArguments.Concat(args))
            psi.ArgumentList.Add(a);
        configureEnvironment?.Invoke(psi.Environment);
        var process = Process.Start(psi) ?? throw new InvalidOperationException($"無法開啟{title ?? "主控台"}視窗");
        process.EnableRaisingEvents = true;
        return process;
    }
}
