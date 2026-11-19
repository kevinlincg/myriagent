using System.Text;

namespace Myriagent.Cli;

/// <summary>
/// macOS 的「主控台視窗」（CliRunner.StartConsole：登入、登出這類要使用者在畫面上操作的流程）：在「終端機」開一個視窗執行。
/// 終端機不是 app 的子程序，所以拆成兩個小腳本放在暫存資料夾：
/// <list type="bullet">
/// <item>watch.sh：app 啟動、等待的程序。記下自己的 PID、叫終端機開 run.command，等到 done 檔出現就以它的結束碼結束；
/// run.command 沒寫 done 就不見了（使用者關掉視窗）回 1；終端機一直沒開起來（StartTimeout）也回 1</item>
/// <item>run.command：在終端機裡執行。設好環境變數（只寫和 app 不同的：終端機的 shell 有使用者自己的環境），執行指令、把結束碼寫進 done。
/// 背景盯著 watch.sh：它被結束（逾時、取消、移除帳戶時 app 會砍它）就結束整個指令</item>
/// </list>
/// </summary>
internal static class TerminalConsoleRules
{
    public const string WatchFile = "watch.sh";
    public const string RunFile = "run.command";
    public const string WatcherPidFile = "watcher.pid";
    public const string RunPidFile = "run.pid";
    public const string DoneFile = "done";

    /// <summary>終端機要多久內開起來（run.command 寫下 PID）</summary>
    public static readonly TimeSpan StartTimeout = TimeSpan.FromSeconds(60);

    /// <summary>單引號包起來給 sh（裡面的單引號寫成 '\''）</summary>
    public static string Quote(string value) => "'" + value.Replace("'", "'\\''") + "'";

    /// <summary>sh 能 export 的變數名稱</summary>
    public static bool IsVariableName(string name)
        => name.Length > 0 && (char.IsAsciiLetter(name[0]) || name[0] == '_') && name.All(c => char.IsAsciiLetterOrDigit(c) || c == '_');

    /// <summary>設定環境之後和之前差在哪：要設的（新加或改了值）與要拿掉的；名稱排序，sh 不能用的名稱略過</summary>
    public static (IReadOnlyList<KeyValuePair<string, string>> Set, IReadOnlyList<string> Unset) Changes(
        IReadOnlyDictionary<string, string?> before, IReadOnlyDictionary<string, string?> after)
    {
        var set = after
            .Where(kv => kv.Value != null && IsVariableName(kv.Key) && (!before.TryGetValue(kv.Key, out var old) || old != kv.Value))
            .Select(kv => new KeyValuePair<string, string>(kv.Key, kv.Value!))
            .OrderBy(kv => kv.Key, StringComparer.Ordinal)
            .ToList();
        var unset = before
            .Where(kv => kv.Value != null && IsVariableName(kv.Key) && (!after.TryGetValue(kv.Key, out var now) || now == null))
            .Select(kv => kv.Key)
            .OrderBy(k => k, StringComparer.Ordinal)
            .ToList();
        return (set, unset);
    }

    /// <summary>在終端機裡執行的 run.command</summary>
    public static string RunScript(string folder, string? title, string fileName, IEnumerable<string> arguments,
        IReadOnlyList<KeyValuePair<string, string>> set, IReadOnlyList<string> unset, string workingDirectory)
    {
        var d = Quote(folder);
        var sb = new StringBuilder();
        sb.Append("#!/bin/sh\n");
        sb.Append("# Myriagent 開的視窗：指令結束後這個視窗可以關掉\n");
        sb.Append("D=").Append(d).Append('\n');
        sb.Append("echo $$ > \"$D/").Append(RunPidFile).Append("\"\n");
        if (!string.IsNullOrWhiteSpace(title))
            sb.Append("printf '\\033]0;%s\\007' ").Append(Quote(title.Trim())).Append('\n');
        sb.Append("W=$(cat \"$D/").Append(WatcherPidFile).Append("\" 2>/dev/null)\n");
        // app 那邊等待的程序不見了（逾時、取消）：結束這個視窗裡的整個指令（同一個程序群組）
        sb.Append("if [ -n \"$W\" ]; then\n");
        sb.Append("  ( while kill -0 \"$W\" 2>/dev/null; do sleep 1; done; [ -f \"$D/").Append(DoneFile).Append("\" ] || kill -TERM 0 ) &\n");
        sb.Append("  M=$!\n");
        sb.Append("fi\n");
        foreach (var (key, value) in set)
            sb.Append("export ").Append(key).Append('=').Append(Quote(value)).Append('\n');
        foreach (var key in unset)
            sb.Append("unset ").Append(key).Append('\n');
        sb.Append("cd ").Append(Quote(workingDirectory)).Append(" 2>/dev/null\n");
        sb.Append(Quote(fileName));
        foreach (var a in arguments)
            sb.Append(' ').Append(Quote(a));
        sb.Append('\n');
        sb.Append("rc=$?\n");
        sb.Append("echo \"$rc\" > \"$D/done.tmp\" && mv -f \"$D/done.tmp\" \"$D/").Append(DoneFile).Append("\"\n");
        sb.Append("[ -n \"$M\" ] && kill \"$M\" 2>/dev/null\n");
        sb.Append("exit \"$rc\"\n");
        return sb.ToString();
    }

    /// <summary>app 啟動、等待的 watch.sh</summary>
    public static string WatchScript(string folder)
    {
        var ticks = (int)(StartTimeout.TotalSeconds / 0.2);
        return "#!/bin/sh\n"
            + "D=" + Quote(folder) + "\n"
            + "echo $$ > \"$D/" + WatcherPidFile + "\"\n"
            + "/usr/bin/open -a Terminal \"$D/" + RunFile + "\" || { rm -rf \"$D\"; exit 127; }\n"
            + "n=0\n"
            + "while :; do\n"
            + "  if [ -f \"$D/" + DoneFile + "\" ]; then rc=$(cat \"$D/" + DoneFile + "\"); rm -rf \"$D\"; exit \"${rc:-1}\"; fi\n"
            + "  if [ -f \"$D/" + RunPidFile + "\" ]; then\n"
            + "    p=$(cat \"$D/" + RunPidFile + "\")\n"
            + "    if [ -n \"$p\" ] && ! kill -0 \"$p\" 2>/dev/null; then\n"
            + "      [ -f \"$D/" + DoneFile + "\" ] && continue\n"
            + "      rm -rf \"$D\"; exit 1\n"
            + "    fi\n"
            + "  else\n"
            + "    n=$((n+1)); if [ \"$n\" -gt " + ticks + " ]; then rm -rf \"$D\"; exit 1; fi\n"
            + "  fi\n"
            + "  sleep 0.2\n"
            + "done\n";
    }
}
