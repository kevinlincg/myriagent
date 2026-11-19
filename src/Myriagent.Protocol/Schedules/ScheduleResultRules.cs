using System.Globalization;

namespace Myriagent.Schedules;

/// <summary>上次執行結果的等級：成功、資訊（執行中、還沒執行過…，不算結果）、警告（被停止、條件不符沒執行）、失敗</summary>
internal enum ScheduleResultLevel
{
    Ok,
    Info,
    Warn,
    Fail,
}

/// <summary>結果的說明與等級</summary>
internal sealed record ScheduleResult(int Code, ScheduleResultLevel Level, string Text)
{
    public string LevelName => ScheduleResultRules.LevelName(Level);
}

/// <summary>
/// 工作排程器的 LastTaskResult：0 是成功；0x413xx 是工作排程器自己的狀態碼（SCHED_S_*：執行中、還沒執行過…）；
/// 其他是程式的結束碼或啟動失敗的 HRESULT。只有 <see cref="ScheduleResultLevel.Fail"/> 算失敗（通知、連續失敗、自動診斷）
/// </summary>
internal static class ScheduleResultRules
{
    public const int Running = 0x41301;
    public const int HasNotRun = 0x41303;
    public const int Terminated = 0x41306;

    private static readonly Dictionary<uint, (ScheduleResultLevel Level, string Text)> Known = new()
    {
        [0x00041300] = (ScheduleResultLevel.Info, "準備就緒"),
        [0x00041301] = (ScheduleResultLevel.Info, "執行中"),
        [0x00041302] = (ScheduleResultLevel.Info, "已停用"),
        [0x00041303] = (ScheduleResultLevel.Info, "還沒執行過"),
        [0x00041304] = (ScheduleResultLevel.Info, "不會再執行"),
        [0x00041305] = (ScheduleResultLevel.Info, "沒有排定的時間"),
        [0x00041306] = (ScheduleResultLevel.Warn, "被停止"),
        [0x00041307] = (ScheduleResultLevel.Warn, "沒有有效的觸發條件"),
        [0x0004131C] = (ScheduleResultLevel.Warn, "以批次登入執行時出了問題"),
        [0x8004131F] = (ScheduleResultLevel.Warn, "上一次還在執行，這次沒有開始"),
        [0x80041326] = (ScheduleResultLevel.Info, "已停用"),
        [0x800710E0] = (ScheduleResultLevel.Warn, "條件不符，這次沒有執行（例如使用電池、使用者沒登入）"),
        [0x80070002] = (ScheduleResultLevel.Fail, "找不到要執行的檔案"),
        [0x80070003] = (ScheduleResultLevel.Fail, "找不到路徑"),
        [0x80070005] = (ScheduleResultLevel.Fail, "存取被拒"),
        [0x8007010B] = (ScheduleResultLevel.Fail, "工作資料夾不存在"),
        [0x800704DD] = (ScheduleResultLevel.Fail, "使用者沒有登入，無法執行"),
        [0xC000013A] = (ScheduleResultLevel.Warn, "被中斷（Ctrl+C 或關機）"),
        [0x40010004] = (ScheduleResultLevel.Warn, "被強制結束"),
        [0xC0000142] = (ScheduleResultLevel.Fail, "程式無法初始化"),
        [0xC0000005] = (ScheduleResultLevel.Fail, "程式當掉（存取違規）"),
        [0xC0000409] = (ScheduleResultLevel.Fail, "程式當掉（堆疊緩衝區溢位）"),
        [0xE0434352] = (ScheduleResultLevel.Fail, ".NET 程式有未處理的例外"),
    };

    /// <summary>
    /// macOS（launchd、cron）的結束碼：程式自己的結束碼是 0～255；被訊號結束記成負的訊號編號（launchctl list 也是這樣寫）。
    /// 被停止、被強制結束是警告（有人或系統叫它停，不是它自己壞了：launchctl kill、登出、重新載入都會這樣）；當掉的才算失敗
    /// </summary>
    private static readonly Dictionary<int, (ScheduleResultLevel Level, string Text)> KnownPosix = new()
    {
        [78] = (ScheduleResultLevel.Fail, "launchd 啟動不了（設定有問題：找不到程式、沒有權限或工作資料夾不存在）"),
        [126] = (ScheduleResultLevel.Fail, "找到檔案但不能執行（沒有執行權限）"),
        [127] = (ScheduleResultLevel.Fail, "找不到要執行的指令"),
        [-1] = (ScheduleResultLevel.Warn, "終端機關閉而結束（SIGHUP）"),
        [-2] = (ScheduleResultLevel.Warn, "被中斷（SIGINT）"),
        [-3] = (ScheduleResultLevel.Warn, "被要求結束（SIGQUIT）"),
        [-4] = (ScheduleResultLevel.Fail, "程式當掉（不合法的指令）"),
        [-5] = (ScheduleResultLevel.Fail, "程式當掉（SIGTRAP）"),
        [-6] = (ScheduleResultLevel.Fail, "程式自己中止（abort）"),
        [-8] = (ScheduleResultLevel.Fail, "程式當掉（算術錯誤）"),
        [-9] = (ScheduleResultLevel.Warn, "被強制結束（SIGKILL）"),
        [-10] = (ScheduleResultLevel.Fail, "程式當掉（記憶體存取錯誤）"),
        [-11] = (ScheduleResultLevel.Fail, "程式當掉（記憶體存取錯誤）"),
        [-13] = (ScheduleResultLevel.Warn, "輸出的對象關閉了（SIGPIPE）"),
        [-15] = (ScheduleResultLevel.Warn, "被停止（SIGTERM）"),
    };

    /// <summary>這台電腦的結果說明（Windows 是工作排程器的碼，其他是 macOS 的結束碼）</summary>
    public static ScheduleResult Classify(int code) => Classify(code, !OperatingSystem.IsWindows());

    /// <summary>posix＝macOS（launchd、cron）的結束碼；工作排程器自己的狀態碼（0x413xx：執行中、還沒執行過）兩邊都認，macOS 的讀取器拿它們當記號</summary>
    public static ScheduleResult Classify(int code, bool posix)
    {
        if (code == 0)
            return new ScheduleResult(0, ScheduleResultLevel.Ok, "成功");
        if (posix && KnownPosix.TryGetValue(code, out var unix))
            return new ScheduleResult(code, unix.Level, unix.Text);
        if (Known.TryGetValue(unchecked((uint)code), out var known) && (!posix || unchecked((uint)code) is >= 0x00041300 and <= 0x0004131F))
            return new ScheduleResult(code, known.Level, known.Text);
        return new ScheduleResult(code, ScheduleResultLevel.Fail, CodeText(code, posix));
    }

    /// <summary>結束碼的寫法：0～65535 寫十進位（程式自己的結束碼），其他寫十六進位（HRESULT、NTSTATUS）</summary>
    public static string CodeText(int code) => CodeText(code, !OperatingSystem.IsWindows());

    /// <summary>posix：負的是訊號（「被訊號 31 結束」），其他寫十進位</summary>
    public static string CodeText(int code, bool posix)
    {
        if (posix)
            return code < 0 ? $"被訊號 {-(long)code} 結束" : $"結束碼 {code}";
        return code is > 0 and <= 65535 ? $"結束碼 {code}" : $"結束碼 0x{unchecked((uint)code).ToString("X8", CultureInfo.InvariantCulture)}";
    }

    public static string LevelName(ScheduleResultLevel level) => level switch
    {
        ScheduleResultLevel.Ok => "ok",
        ScheduleResultLevel.Info => "info",
        ScheduleResultLevel.Warn => "warn",
        ScheduleResultLevel.Fail => "fail",
        _ => throw new ArgumentOutOfRangeException(nameof(level), level, null),
    };

    /// <summary>這個結果是一次執行的最後結果（不是執行中、還沒執行過這類狀態）：才記進執行紀錄</summary>
    public static bool IsFinal(int code)
        => code != Running && code != HasNotRun && unchecked((uint)code) is not (0x00041300 or 0x00041302 or 0x00041304 or 0x00041305 or 0x80041326);
}
