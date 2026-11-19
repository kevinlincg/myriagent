using System.Globalization;
using System.Text.RegularExpressions;

namespace Myriagent.Schedules;

/// <summary>launchd 記錄檔的一筆（<see cref="LaunchdStateRules.ParseLogLine"/>）：某個工作開始執行，或結束（Code＝結束碼；被訊號結束是負的訊號編號）</summary>
internal sealed record LaunchdLogEvent(DateTime At, string Service, bool Exited, int Code);

/// <summary>
/// 一個工作從記錄檔看到的執行：LastStart＝最近一次開始；FinishedStart／FinishedAt／Code＝最近一次「跑完」的那次的開始、結束與結束碼。
/// 開始之後還沒看到結束就是執行中
/// </summary>
internal sealed record LaunchdRun(DateTime? LastStart, DateTime? FinishedStart, DateTime? FinishedAt, int? Code)
{
    public bool Running => LastStart is { } start && (FinishedAt is not { } end || start > end);
}

/// <summary><c>launchctl list</c>／<c>launchctl print system</c> 列出的一個已載入的工作：Pid（沒在跑是 null）、上次的結束碼（沒有是 null）</summary>
internal sealed record LaunchdLoaded(int? Pid, int? Status);

/// <summary>排程頁要的狀態（<see cref="LaunchdStateRules.Resolve"/>）</summary>
internal sealed record LaunchdState(ScheduledTaskStateKind State, bool Enabled, DateTime? LastRun, int LastResult);

/// <summary>同 Myriagent 的 ScheduledTaskState（那個型別在 app 的組件裡，這裡是共用的規則：以名稱對應）</summary>
internal enum ScheduledTaskStateKind
{
    Unknown = 0,
    Disabled = 1,
    Queued = 2,
    Ready = 3,
    Running = 4,
}

/// <summary>
/// launchd 工作的狀態從哪裡來（macOS 沒有 Windows 工作排程器那種「上次執行時間」的 API）：
/// <list type="bullet">
/// <item>有沒有載入、在不在跑、上次的結束碼：<c>launchctl list</c>（這個使用者的）與 <c>launchctl print system</c>（開機載入的），一般權限就讀得到</item>
/// <item>停用：<c>launchctl print-disabled</c>（launchctl disable 記在系統裡，不寫回 plist）</item>
/// <item>上次什麼時候開始、結束碼：/var/log/com.apple.xpc.launchd/launchd.log（所有人可讀；幾 MB 就輪替，只有最近幾小時）</item>
/// </list>
/// 這裡都是純函式（解析文字、合出狀態）；讀檔與執行 launchctl 在 macOS 版的讀取器
/// </summary>
internal static class LaunchdStateRules
{
    /// <summary>2026-11-19 21:13:12.378848 (gui/501/com.example.job [92888]) &lt;Notice&gt;: 訊息</summary>
    private static readonly Regex LogLine = new(
        @"^(?<at>\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)(?:\.\d+)? \((?<svc>[^ )]+)(?: \[\d+\])?\) <\w+>: (?<msg>.*)$", RegexOptions.Compiled);

    private static readonly Regex ExitMessage = new(@"^exited due to (?:exit\((?<code>-?\d+)\)|(?<sig>SIG[A-Z0-9]+))", RegexOptions.Compiled);

    private static readonly Dictionary<string, int> Signals = new(StringComparer.Ordinal)
    {
        ["SIGHUP"] = 1, ["SIGINT"] = 2, ["SIGQUIT"] = 3, ["SIGILL"] = 4, ["SIGTRAP"] = 5, ["SIGABRT"] = 6, ["SIGFPE"] = 8, ["SIGKILL"] = 9,
        ["SIGBUS"] = 10, ["SIGSEGV"] = 11, ["SIGSYS"] = 12, ["SIGPIPE"] = 13, ["SIGALRM"] = 14, ["SIGTERM"] = 15,
    };

    /// <summary>看不出是哪個訊號時用的編號（結束碼記成它的負數）</summary>
    public const int UnknownSignal = 99;

    /// <summary>快速篩選：這一行可能是開始或結束（記錄檔絕大多數的行不是）</summary>
    public static bool MayMatter(string line)
        => line.Contains("Successfully spawned", StringComparison.Ordinal) || line.Contains("exited due to", StringComparison.Ordinal);

    /// <summary>解析記錄檔的一行；不是開始或結束回 null。時間是本機時間</summary>
    public static LaunchdLogEvent? ParseLogLine(string line)
    {
        if (!MayMatter(line))
            return null;
        var m = LogLine.Match(line);
        if (!m.Success)
            return null;
        if (!DateTime.TryParseExact(m.Groups["at"].Value, "yyyy-MM-dd HH:mm:ss", CultureInfo.InvariantCulture, DateTimeStyles.None, out var at))
            return null;
        at = DateTime.SpecifyKind(at, DateTimeKind.Local);
        var service = m.Groups["svc"].Value;
        var message = m.Groups["msg"].Value;
        if (message.StartsWith("Successfully spawned ", StringComparison.Ordinal))
            return new LaunchdLogEvent(at, service, false, 0);
        var exit = ExitMessage.Match(message);
        if (!exit.Success)
            return null;
        if (exit.Groups["code"].Success)
            return int.TryParse(exit.Groups["code"].Value, NumberStyles.Integer, CultureInfo.InvariantCulture, out var code) ? new LaunchdLogEvent(at, service, true, code) : null;
        return new LaunchdLogEvent(at, service, true, -(Signals.TryGetValue(exit.Groups["sig"].Value, out var signal) ? signal : UnknownSignal));
    }

    /// <summary>把一筆記到各工作的執行狀況上（依記錄檔的順序呼叫）</summary>
    public static void Apply(IDictionary<string, LaunchdRun> runs, LaunchdLogEvent e)
    {
        runs.TryGetValue(e.Service, out var run);
        if (!e.Exited)
        {
            runs[e.Service] = (run ?? new LaunchdRun(null, null, null, null)) with { LastStart = e.At };
            return;
        }
        // 這次結束對應的開始：上一次結束之後有開始過就是它；沒看到開始（記錄檔已經輪替、啟動就失敗）就當成結束的那一刻
        var start = run?.LastStart is { } s && s <= e.At && (run.FinishedAt is not { } previousEnd || s > previousEnd) ? s : e.At;
        runs[e.Service] = new LaunchdRun(run?.LastStart, start, e.At, e.Code);
    }

    /// <summary>記錄檔裡的工作名稱：這個使用者登入後載入的是 gui/〈uid〉/〈標籤〉，開機載入的是 system/〈標籤〉</summary>
    public static string ServiceKey(LaunchdDomain domain, int uid, string label)
        => domain == LaunchdDomain.Daemon ? "system/" + label : $"gui/{uid.ToString(CultureInfo.InvariantCulture)}/{label}";

    /// <summary><c>launchctl list</c> 的輸出（PID、Status、Label 三欄，以 Tab 分隔；沒在跑的 PID 是 -）</summary>
    public static Dictionary<string, LaunchdLoaded> ParseList(string? text)
    {
        var result = new Dictionary<string, LaunchdLoaded>(StringComparer.Ordinal);
        foreach (var line in (text ?? "").Split('\n'))
        {
            var parts = line.Split('\t');
            if (parts.Length < 3 || parts[2].Trim() is not { Length: > 0 } label || label == "Label")
                continue;
            result[label] = new LaunchdLoaded(Number(parts[0]) is int pid and > 0 ? (int?)pid : null, Number(parts[1]));
        }
        return result;
    }

    /// <summary>
    /// <c>launchctl print system</c> 的 services = { … } 那一段（每行：PID、狀態、標籤；沒在跑的 PID 是 0、沒有結束碼的是 -）
    /// </summary>
    public static Dictionary<string, LaunchdLoaded> ParseSystemServices(string? text)
    {
        var result = new Dictionary<string, LaunchdLoaded>(StringComparer.Ordinal);
        bool inside = false;
        foreach (var raw in (text ?? "").Split('\n'))
        {
            var line = raw.Trim();
            if (!inside)
            {
                inside = line == "services = {";
                continue;
            }
            if (line == "}")
                break;
            var parts = line.Split(new[] { ' ', '\t' }, StringSplitOptions.RemoveEmptyEntries);
            if (parts.Length < 3)
                continue;
            result[parts[^1]] = new LaunchdLoaded(Number(parts[0]) is int pid and > 0 ? (int?)pid : null, Number(parts[1]));
        }
        return result;
    }

    /// <summary><c>launchctl print-disabled 〈網域〉</c> 的輸出：「"標籤" =&gt; disabled」是停用、enabled 是明確啟用；沒列到的照 plist</summary>
    public static Dictionary<string, bool> ParseDisabled(string? text)
    {
        var result = new Dictionary<string, bool>(StringComparer.Ordinal);
        foreach (var raw in (text ?? "").Split('\n'))
        {
            var line = raw.Trim();
            int arrow = line.IndexOf("=>", StringComparison.Ordinal);
            if (arrow <= 0 || !line.StartsWith('"'))
                continue;
            var label = line[..arrow].Trim().Trim('"');
            var value = line[(arrow + 2)..].Trim();
            if (label.Length > 0 && value is "disabled" or "enabled" or "true" or "false")
                result[label] = value is "disabled" or "true";
        }
        return result;
    }

    private static int? Number(string text)
        => int.TryParse(text.Trim(), NumberStyles.Integer, CultureInfo.InvariantCulture, out var n) ? n : null;

    /// <summary>
    /// 合出排程頁要的狀態。enabledInPlist＝plist 沒有寫 Disabled；disabledOverride＝launchctl print-disabled 的記載（沒列到是 null）；
    /// loaded＝launchctl 列出的（沒載入是 null）；run＝記錄檔看到的（沒有是 null）；processStart＝在跑的那個程序的開始時間（查不到是 null）；
    /// logWritten＝工作的輸出檔最後寫入的時間（沒有設定輸出檔、檔案不存在是 null）。
    /// 上次執行：記錄檔有就用它（最準）；沒有時在跑的用程序的開始時間，沒在跑的用輸出檔最後寫入的時間配 launchctl 記的結束碼；
    /// 都沒有就是「還沒執行過」（其實是不知道：launchd 不記時間）
    /// </summary>
    public static LaunchdState Resolve(bool enabledInPlist, bool? disabledOverride, LaunchdLoaded? loaded, LaunchdRun? run, DateTime? processStart, DateTime? logWritten)
    {
        bool enabled = disabledOverride is { } disabled ? !disabled : enabledInPlist;
        bool running = loaded?.Pid != null;
        var state = running ? ScheduledTaskStateKind.Running
            : !enabled ? ScheduledTaskStateKind.Disabled
            : loaded != null ? ScheduledTaskStateKind.Ready
            : ScheduledTaskStateKind.Unknown;
        if (running)
            return new LaunchdState(state, enabled, run is { Running: true } ? run.LastStart : processStart ?? run?.LastStart, ScheduleResultRules.Running);
        if (run is { FinishedStart: { } finished, Code: { } code })
            return new LaunchdState(state, enabled, finished, code);
        if (loaded?.Status is { } status && logWritten is { } written)
            return new LaunchdState(state, enabled, written, status);
        return new LaunchdState(state, enabled, null, ScheduleResultRules.HasNotRun);
    }
}
