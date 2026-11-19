using System.Diagnostics;

namespace Myriagent.Core;

/// <summary>
/// 從 Finder、Dock 啟動的 app 拿到的 PATH 只有系統的幾個資料夾（/usr/bin:/bin:/usr/sbin:/sbin），使用者在殼層設定檔加的都沒有：
/// Homebrew、~/.local/bin、nvm 的 node、npm 的全域資料夾——claude、codex、node、git、ffmpeg 都在那些地方。
/// 啟動時向使用者的登入殼層問一次 PATH（和 VS Code 等編輯器同樣的做法），設成這個程序的 PATH：
/// 之後找 CLI、啟動 CLI 與它們再啟動的工具都看得到和終端機一樣的環境
/// </summary>
internal static class MacEnvironment
{
    private const string Marker = "__MYRIAGENT_PATH__";

    /// <summary>殼層設定檔沒有設、但常放命令列工具的資料夾（存在才加，排在最後）</summary>
    private static readonly string[] CommonDirectories =
    [
        "~/.local/bin", "/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin", "~/.npm-global/bin", "~/.bun/bin", "~/.cargo/bin",
    ];

    /// <summary>
    /// 問登入殼層的 PATH 並併進這個程序的 PATH（殼層的在前面）。殼層起不來、太久沒回（設定檔卡住）就只補上常見的資料夾。
    /// 只在啟動時呼叫一次（任何執行緒）
    /// </summary>
    public static void ImportLoginShellPath(TimeSpan timeout)
    {
        var current = Environment.GetEnvironmentVariable("PATH") ?? string.Empty;
        var shellPath = ReadLoginShellPath(timeout);
        var home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        var common = CommonDirectories.Select(d => d.StartsWith("~/", StringComparison.Ordinal) ? Path.Combine(home, d[2..]) : d).Where(Directory.Exists);
        var merged = Merge(shellPath, current, common);
        if (!string.Equals(merged, current, StringComparison.Ordinal))
            Environment.SetEnvironmentVariable("PATH", merged);
        Log.Info(shellPath == null ? "沒問到登入殼層的 PATH：只補上常見的資料夾" : $"已套用登入殼層的 PATH（{merged.Split(':').Length} 個資料夾）");
    }

    /// <summary>依序合併、去掉重複與空的（保留先出現的）</summary>
    internal static string Merge(string? shellPath, string current, IEnumerable<string> extra)
    {
        var seen = new HashSet<string>(StringComparer.Ordinal);
        var result = new List<string>();
        foreach (var directory in (shellPath ?? string.Empty).Split(':').Concat(current.Split(':')).Concat(extra))
        {
            var trimmed = directory.Trim();
            if (trimmed.Length > 0 && seen.Add(trimmed))
                result.Add(trimmed);
        }
        return string.Join(':', result);
    }

    /// <summary>
    /// 以登入而且互動的方式啟動使用者的殼層（兩種設定檔都會讀：.zprofile、.zshrc 等），印出 PATH。
    /// 設定檔可能自己印東西：PATH 前後加記號，只取記號中間的
    /// </summary>
    private static string? ReadLoginShellPath(TimeSpan timeout)
    {
        var shell = Environment.GetEnvironmentVariable("SHELL");
        if (string.IsNullOrWhiteSpace(shell) || !File.Exists(shell))
            shell = "/bin/zsh";
        try
        {
            var info = new ProcessStartInfo(shell)
            {
                RedirectStandardInput = true,
                RedirectStandardOutput = true,
                RedirectStandardError = true,
                UseShellExecute = false,
            };
            info.ArgumentList.Add("-l");
            info.ArgumentList.Add("-i");
            info.ArgumentList.Add("-c");
            info.ArgumentList.Add($"printf '%s%s%s' '{Marker}' \"$PATH\" '{Marker}'");
            using var process = Process.Start(info);
            if (process == null)
                return null;
            process.StandardInput.Close();
            var output = process.StandardOutput.ReadToEndAsync();
            _ = process.StandardError.ReadToEndAsync();
            if (!process.WaitForExit((int)timeout.TotalMilliseconds))
            {
                try
                {
                    process.Kill(entireProcessTree: true);
                }
                catch (InvalidOperationException)
                {
                }
                return null;
            }
            return Extract(output.GetAwaiter().GetResult());
        }
        catch (Exception ex) when (ex is System.ComponentModel.Win32Exception or InvalidOperationException or IOException)
        {
            return null;
        }
    }

    /// <summary>取兩個記號中間的 PATH；沒有記號、中間是空的回 null</summary>
    internal static string? Extract(string output)
    {
        int start = output.IndexOf(Marker, StringComparison.Ordinal);
        if (start < 0)
            return null;
        start += Marker.Length;
        int end = output.IndexOf(Marker, start, StringComparison.Ordinal);
        if (end <= start)
            return null;
        var path = output[start..end].Trim();
        return path.Length == 0 ? null : path;
    }
}
