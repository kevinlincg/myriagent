using System.Globalization;
using System.Text;
using System.Xml;
using System.Xml.Linq;

namespace Myriagent.Schedules;

/// <summary>launchd 的工作放在哪裡（決定誰執行、什麼時候載入、誰能改）</summary>
internal enum LaunchdDomain
{
    /// <summary>~/Library/LaunchAgents：這個使用者登入時載入、以他的身分執行，自己就能改</summary>
    UserAgent,

    /// <summary>/Library/LaunchAgents：每個使用者登入時載入、以登入的人執行，要系統管理員才能改</summary>
    GlobalAgent,

    /// <summary>/Library/LaunchDaemons：開機時載入、預設以 root 執行，要系統管理員才能改</summary>
    Daemon,
}

/// <summary>一個 launchd 工作（.plist 讀出來的；狀態與上次結果另外由 launchctl 與 launchd 的記錄檔取得）</summary>
internal sealed record LaunchdJob(string? Label, ScheduleDefinition Definition, string? StandardOutPath, string? StandardErrorPath);

/// <summary>
/// macOS 的排程（launchd 的 .plist）讀成排程頁共用的 <see cref="ScheduleDefinition"/>（Windows 版讀的是工作排程器的 XML，ScheduleXmlRules）。
/// 純函式：不讀檔、不看現在時間（「每隔多久」的起算時間由呼叫端給）。對應關係：
/// StartCalendarInterval → 每天／每週／每月（沒寫的欄位是萬用字元：只寫分鐘＝每小時、只寫小時＝那一小時裡每分鐘）；
/// StartInterval → 每隔多久；RunAtLoad、KeepAlive → 登入時（LaunchAgent）或開機時（LaunchDaemon）；
/// WatchPaths、QueueDirectories、StartOnMount、Sockets、MachServices → 事件。讀不懂的鍵忽略
/// </summary>
internal static class LaunchdPlistRules
{
    /// <summary>ScheduleDefinition.LogonType 的值：登入時才載入的工作</summary>
    public const string AgentLogonType = "LaunchAgent";

    /// <summary>ScheduleDefinition.LogonType 的值：開機就載入的工作</summary>
    public const string DaemonLogonType = "LaunchDaemon";

    /// <summary>時間觸發的基準日（只用它的時刻；要早於任何查詢的日期）</summary>
    internal static readonly DateTime BaseDate = new(2020, 1, 1, 0, 0, 0, DateTimeKind.Local);

    /// <summary>
    /// 解析 XML 寫法的 plist：dict 是 Dictionary、array 是 List、string／date／data 是字串、integer 是 long、real 是 double、true／false 是 bool。
    /// 不是合法的 XML、最外層不是 plist 丟 XmlException（二進位的 plist 由呼叫端先以 plutil 轉成 XML）
    /// </summary>
    public static object? ParsePlist(string xml)
    {
        // plist 都帶 DOCTYPE（指到 apple.com 的 DTD）：略過它，不連出去
        var settings = new XmlReaderSettings { DtdProcessing = DtdProcessing.Ignore, XmlResolver = null };
        using var reader = XmlReader.Create(new StringReader(xml), settings);
        var doc = XDocument.Load(reader);
        var root = doc.Root ?? throw new XmlException("沒有根元素");
        if (root.Name.LocalName != "plist")
            throw new XmlException("不是 plist");
        var first = root.Elements().FirstOrDefault();
        return first == null ? null : Read(first);
    }

    private static object? Read(XElement e)
    {
        switch (e.Name.LocalName)
        {
            case "dict":
            {
                var dict = new Dictionary<string, object?>(StringComparer.Ordinal);
                string? key = null;
                foreach (var child in e.Elements())
                {
                    if (child.Name.LocalName == "key")
                    {
                        key = child.Value;
                        continue;
                    }
                    if (key != null)
                        dict[key] = Read(child);
                    key = null;
                }
                return dict;
            }
            case "array":
                return e.Elements().Select(Read).ToList();
            case "string":
            case "date":
            case "data":
                return e.Value;
            case "integer":
                return long.TryParse(e.Value.Trim(), NumberStyles.Integer, CultureInfo.InvariantCulture, out var i) ? i : 0L;
            case "real":
                return double.TryParse(e.Value.Trim(), NumberStyles.Float, CultureInfo.InvariantCulture, out var d) ? d : 0d;
            case "true":
                return true;
            case "false":
                return false;
            default:
                return null;
        }
    }

    /// <summary>
    /// plist（XML）讀成工作。userName＝這台登入的使用者（LaunchAgent 的執行身分）；created＝檔案的建立時間（plist 自己沒有記）；
    /// intervalAnchor＝「每隔多久」（StartInterval）從什麼時候起算（上次開始執行的時間；不知道就給開機時間）。
    /// 不是合法的 plist、最外層不是 dict 丟 XmlException
    /// </summary>
    public static LaunchdJob ToJob(string xml, LaunchdDomain domain, string userName, DateTime? created, DateTime? intervalAnchor)
    {
        if (ParsePlist(xml) is not Dictionary<string, object?> plist)
            throw new XmlException("最外層不是 dict");
        bool daemon = domain == LaunchdDomain.Daemon;
        var label = Str(plist, "Label");
        var notes = new List<string>();

        var triggers = new List<ScheduleTrigger>();
        triggers.AddRange(CalendarTriggers(plist.GetValueOrDefault("StartCalendarInterval")));
        if (Int(plist, "StartInterval") is long seconds and > 0)
            triggers.Add(IntervalTrigger(TimeSpan.FromSeconds(seconds), intervalAnchor ?? BaseDate));
        bool keepAlive = plist.GetValueOrDefault("KeepAlive") is true or Dictionary<string, object?>;
        if (Bool(plist, "RunAtLoad") || keepAlive)
        {
            triggers.Add(new ScheduleTrigger
            {
                Kind = daemon ? ScheduleTriggerKind.Boot : ScheduleTriggerKind.Logon,
                UserId = daemon || domain == LaunchdDomain.GlobalAgent ? null : userName,
            });
        }
        if (keepAlive)
            notes.Add("保持執行：結束後 launchd 會重新啟動");
        var events = new List<string>();
        if (Strings(plist, "WatchPaths") is { Count: > 0 } watch)
            events.Add("這些路徑有變動時執行：" + string.Join("、", watch));
        if (Strings(plist, "QueueDirectories") is { Count: > 0 } queue)
            events.Add("這些資料夾裡有檔案時執行：" + string.Join("、", queue));
        if (Bool(plist, "StartOnMount"))
            events.Add("掛上磁碟時執行");
        if (plist.ContainsKey("Sockets") || plist.ContainsKey("MachServices") || plist.ContainsKey("LaunchEvents"))
            events.Add("有程式連進來或發生系統事件時由 launchd 啟動");
        if (events.Count > 0)
        {
            triggers.Add(new ScheduleTrigger { Kind = ScheduleTriggerKind.Event });
            notes.AddRange(events);
        }

        var stdout = Str(plist, "StandardOutPath");
        var stderr = Str(plist, "StandardErrorPath");
        if (stdout != null && stdout == stderr)
            notes.Add("輸出與錯誤記到 " + stdout);
        else
        {
            if (stdout != null)
                notes.Add("輸出記到 " + stdout);
            if (stderr != null)
                notes.Add("錯誤記到 " + stderr);
        }

        var runAs = daemon ? Str(plist, "UserName") ?? "root" : domain == LaunchdDomain.UserAgent ? userName : null;
        var definition = new ScheduleDefinition
        {
            Description = notes.Count > 0 ? string.Join("；", notes) : null,
            Created = created,
            Triggers = triggers,
            Actions = Actions(plist),
            RunAs = runAs,
            LogonType = daemon ? DaemonLogonType : AgentLogonType,
            Elevated = daemon && runAs == "root",
            Enabled = !Bool(plist, "Disabled"),
            // 時間到了電腦在睡，launchd 醒來會補跑一次（cron 不會）
            StartWhenAvailable = plist.ContainsKey("StartCalendarInterval"),
        };
        return new LaunchdJob(label, definition, stdout, stderr);
    }

    /// <summary>要執行的指令：Program 是程式、ProgramArguments 是 argv（沒有 Program 時第一個就是程式）；都沒有就沒有動作</summary>
    private static IReadOnlyList<ScheduleAction> Actions(Dictionary<string, object?> plist)
    {
        var program = Str(plist, "Program");
        var args = Strings(plist, "ProgramArguments") ?? new List<string>();
        var command = program ?? args.FirstOrDefault();
        if (string.IsNullOrWhiteSpace(command))
            return Array.Empty<ScheduleAction>();
        var rest = args.Skip(1).ToList();
        return new[] { new ScheduleAction("Exec", command, rest.Count > 0 ? JoinArguments(rest) : null, Str(plist, "WorkingDirectory")) };
    }

    /// <summary>argv 寫成一行：有空白、引號或是空字串的以雙引號包起來（引號與反斜線加反斜線）</summary>
    internal static string JoinArguments(IEnumerable<string> args)
    {
        var sb = new StringBuilder();
        foreach (var a in args)
        {
            if (sb.Length > 0)
                sb.Append(' ');
            if (a.Length > 0 && !a.Any(ch => char.IsWhiteSpace(ch) || ch is '"' or '\''))
            {
                sb.Append(a);
                continue;
            }
            sb.Append('"').Append(a.Replace("\\", "\\\\").Replace("\"", "\\\"")).Append('"');
        }
        return sb.ToString();
    }

    /// <summary>
    /// StartCalendarInterval（一個 dict 或一串 dict）。只差在星期幾、時間相同的幾個 dict 併成一個「每週」（平日 09:00 通常寫成五個 dict）
    /// </summary>
    internal static IReadOnlyList<ScheduleTrigger> CalendarTriggers(object? value)
    {
        var entries = value switch
        {
            Dictionary<string, object?> one => new List<Dictionary<string, object?>> { one },
            List<object?> many => many.OfType<Dictionary<string, object?>>().ToList(),
            _ => new List<Dictionary<string, object?>>(),
        };
        var specs = entries.Select(e => new CalendarSpec(
            Range(Int(e, "Minute"), 0, 59), Range(Int(e, "Hour"), 0, 23), Range(Int(e, "Day"), 1, 31),
            Int(e, "Weekday") is long w and >= 0 and <= 7 ? (int?)(int)(w % 7) : null, Range(Int(e, "Month"), 1, 12))).ToList();
        var result = new List<ScheduleTrigger>();
        // 只指定星期幾的：時間相同的併在一起
        foreach (var group in specs.Where(s => s.Weekday != null && s.Day == null && s.Month == null).GroupBy(s => (s.Hour, s.Minute)))
        {
            var days = group.Select(s => (DayOfWeek)s.Weekday!.Value).Distinct().Order().ToList();
            result.Add(CalendarTriggerRules.Make(ScheduleTriggerKind.Weekly, group.Key.Hour, group.Key.Minute, daysOfWeek: days));
        }
        foreach (var s in specs.Where(s => !(s.Weekday != null && s.Day == null && s.Month == null)))
            result.AddRange(CalendarTriggerRules.FromFields(s.Minute, s.Hour, s.Day, s.Month, s.Weekday));
        return result;
    }

    private sealed record CalendarSpec(int? Minute, int? Hour, int? Day, int? Weekday, int? Month);

    private static int? Range(long? value, int min, int max) => value is { } v && v >= min && v <= max ? (int)v : null;

    /// <summary>
    /// 每隔多久（StartInterval）：一天除得盡的寫成「每天、從起算時間開始每隔多久」（排程頁顯示「每小時（21:13 起）」），
    /// 除不盡的（例如每 7 小時）寫成從起算時間一直重複
    /// </summary>
    internal static ScheduleTrigger IntervalTrigger(TimeSpan interval, DateTime anchor)
    {
        bool dividesDay = interval <= TimeSpan.FromDays(1) && TimeSpan.FromDays(1).Ticks % interval.Ticks == 0;
        return new ScheduleTrigger
        {
            Kind = dividesDay ? ScheduleTriggerKind.Daily : ScheduleTriggerKind.Once,
            Start = anchor,
            RepeatInterval = interval,
        };
    }

    private static string? Str(Dictionary<string, object?> d, string key) => d.GetValueOrDefault(key) is string { Length: > 0 } s ? s : null;

    private static bool Bool(Dictionary<string, object?> d, string key) => d.GetValueOrDefault(key) is true;

    private static long? Int(Dictionary<string, object?> d, string key) => d.GetValueOrDefault(key) switch
    {
        long l => l,
        double x when x is >= long.MinValue and <= long.MaxValue => (long)x,
        _ => null,
    };

    private static List<string>? Strings(Dictionary<string, object?> d, string key) => d.GetValueOrDefault(key) switch
    {
        List<object?> list => list.OfType<string>().ToList(),
        // WatchPaths 之類偶爾寫成單一字串
        string { Length: > 0 } one => new List<string> { one },
        _ => null,
    };
}

/// <summary>
/// 「分、時、日、月、星期」這種寫法（launchd 的 StartCalendarInterval、cron）換成排程頁的觸發條件。沒寫的欄位是萬用字元：
/// 分與時都有＝那個時刻；只有分＝每小時的那一分；只有時＝那一小時裡每分鐘；都沒有＝每分鐘。
/// 日與星期都指定時兩個都算（任何一個符合就執行，launchd 與 cron 都是這樣）
/// </summary>
internal static class CalendarTriggerRules
{
    public static IReadOnlyList<ScheduleTrigger> FromFields(int? minute, int? hour, int? day, int? month, int? weekday)
    {
        var months = month is { } m ? new[] { m } : Array.Empty<int>();
        var result = new List<ScheduleTrigger>();
        if (day is { } d)
            result.Add(Make(ScheduleTriggerKind.Monthly, hour, minute, months: months, daysOfMonth: new[] { d }));
        if (weekday is { } w)
        {
            result.Add(month == null
                ? Make(ScheduleTriggerKind.Weekly, hour, minute, daysOfWeek: new[] { (DayOfWeek)w })
                : Make(ScheduleTriggerKind.MonthlyDayOfWeek, hour, minute, months: months, daysOfWeek: new[] { (DayOfWeek)w }, everyWeek: true));
        }
        if (day == null && weekday == null)
        {
            result.Add(month == null
                ? Make(ScheduleTriggerKind.Daily, hour, minute)
                : Make(ScheduleTriggerKind.Monthly, hour, minute, months: months, daysOfMonth: Enumerable.Range(1, 31).ToList()));
        }
        return result;
    }

    /// <summary>一個觸發條件：種類＋時刻（hour、minute 沒有的照萬用字元換成重複間隔）</summary>
    public static ScheduleTrigger Make(ScheduleTriggerKind kind, int? hour, int? minute, IReadOnlyList<int>? months = null, IReadOnlyList<int>? daysOfMonth = null,
        IReadOnlyList<DayOfWeek>? daysOfWeek = null, bool everyWeek = false)
    {
        var (time, interval, duration) = (hour, minute) switch
        {
            ({ } h, { } m) => (new TimeSpan(h, m, 0), (TimeSpan?)null, (TimeSpan?)null),
            (null, { } m) => (new TimeSpan(0, m, 0), (TimeSpan?)TimeSpan.FromHours(1), (TimeSpan?)null),
            ({ } h, null) => (new TimeSpan(h, 0, 0), (TimeSpan?)TimeSpan.FromMinutes(1), (TimeSpan?)TimeSpan.FromHours(1)),
            _ => (TimeSpan.Zero, (TimeSpan?)TimeSpan.FromMinutes(1), (TimeSpan?)null),
        };
        return Make(kind, time, interval, duration, months, daysOfMonth, daysOfWeek, everyWeek);
    }

    /// <summary>一個觸發條件：種類＋時刻＋重複（cron 的 */15 之類由呼叫端算好間隔）</summary>
    public static ScheduleTrigger Make(ScheduleTriggerKind kind, TimeSpan time, TimeSpan? interval, TimeSpan? duration, IReadOnlyList<int>? months = null,
        IReadOnlyList<int>? daysOfMonth = null, IReadOnlyList<DayOfWeek>? daysOfWeek = null, bool everyWeek = false)
        => new()
        {
            Kind = kind,
            Start = LaunchdPlistRules.BaseDate + time,
            RepeatInterval = interval,
            RepeatDuration = interval != null ? duration : null,
            Months = months ?? Array.Empty<int>(),
            DaysOfMonth = daysOfMonth ?? Array.Empty<int>(),
            DaysOfWeek = daysOfWeek ?? Array.Empty<DayOfWeek>(),
            WeeksOfMonth = everyWeek ? new[] { 1, 2, 3, 4 } : Array.Empty<int>(),
            LastWeekOfMonth = everyWeek,
        };
}
