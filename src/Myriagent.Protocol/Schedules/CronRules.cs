using System.Globalization;
using System.Security.Cryptography;
using System.Text;
using System.Text.RegularExpressions;

namespace Myriagent.Schedules;

/// <summary>crontab 的一行排程（<see cref="CronRules.Parse"/>）。Key＝指令的雜湊（改時間不會換一個身分；同一個指令排兩次時後面的加編號）</summary>
internal sealed record CronEntry(string Key, string Line, string Schedule, string Command, ScheduleDefinition Definition);

/// <summary>
/// 使用者的 crontab（<c>crontab -l</c> 的輸出）讀成排程頁共用的 <see cref="ScheduleDefinition"/>。純函式。
/// 支援五個欄位（分 時 日 月 星期）的數字、範圍、清單、間隔（*/15、1-5、1,15、0-30/10）、月份與星期的英文縮寫，以及 @hourly、@daily、
/// @weekly、@monthly、@yearly、@reboot。組合太多（展開超過 <see cref="MaxTriggers"/> 個時刻）的寫成「其他條件」，不亂猜。
/// 註解、空行、環境變數（NAME=值）不是排程
/// </summary>
internal static class CronRules
{
    /// <summary>ScheduleDefinition.LogonType 的值</summary>
    public const string LogonType = "Cron";

    /// <summary>一行最多展開成幾個觸發條件</summary>
    public const int MaxTriggers = 48;

    private static readonly Regex Assignment = new(@"^[A-Za-z_][A-Za-z0-9_]*\s*=", RegexOptions.Compiled);

    private static readonly string[] MonthNames = { "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec" };

    private static readonly string[] DayNames = { "sun", "mon", "tue", "wed", "thu", "fri", "sat" };

    public static IReadOnlyList<CronEntry> Parse(string? crontab, string userName)
    {
        var result = new List<CronEntry>();
        var seen = new Dictionary<string, int>(StringComparer.Ordinal);
        foreach (var raw in (crontab ?? "").Replace("\r\n", "\n").Split('\n'))
        {
            var line = raw.Trim();
            if (line.Length == 0 || line[0] == '#' || Assignment.IsMatch(line))
                continue;
            var split = Split(line);
            if (split == null || split.Value.Command.Length == 0)
                continue;
            var (schedule, command) = split.Value;
            var triggers = Triggers(schedule);
            var (program, arguments) = SplitCommand(command);
            var key = Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(command))).ToLowerInvariant()[..10];
            seen[key] = seen.GetValueOrDefault(key) + 1;
            if (seen[key] > 1)
                key += "-" + seen[key].ToString(CultureInfo.InvariantCulture);
            result.Add(new CronEntry(key, line, schedule, command, new ScheduleDefinition
            {
                Triggers = triggers,
                Actions = new[] { new ScheduleAction("Exec", program, arguments, null) },
                RunAs = userName,
                LogonType = LogonType,
            }));
        }
        return result;
    }

    /// <summary>crontab 裡設的環境變數（NAME=值 的行；值外層的引號拿掉）：立即執行時照樣帶上</summary>
    public static IReadOnlyDictionary<string, string> Variables(string? crontab)
    {
        var result = new Dictionary<string, string>(StringComparer.Ordinal);
        foreach (var raw in (crontab ?? "").Replace("\r\n", "\n").Split('\n'))
        {
            var line = raw.Trim();
            if (line.Length == 0 || line[0] == '#' || !Assignment.IsMatch(line))
                continue;
            int equals = line.IndexOf('=');
            var value = line[(equals + 1)..].Trim();
            if (value.Length >= 2 && value[0] is '"' or '\'' && value[^1] == value[0])
                value = value[1..^1];
            result[line[..equals].Trim()] = value;
        }
        return result;
    }

    private static readonly Regex Redirect = new(@"(?<![0-9&])>>?\s*(?<path>[^\s;&|<>]+)", RegexOptions.Compiled);

    /// <summary>
    /// 指令把輸出轉向到哪個檔案（「&gt;&gt; 檔案」「&gt; 檔案」的第一個；/dev/null、&amp;1 這類不算）。cron 不記執行結果，
    /// 只能從這個檔案最後寫入的時間看出上次大概什麼時候跑過；沒有轉向回 null
    /// </summary>
    public static string? LogPath(string command)
    {
        foreach (Match m in Redirect.Matches(command))
        {
            var path = m.Groups["path"].Value.Trim('"', '\'');
            if (path.Length > 0 && !path.StartsWith("/dev/", StringComparison.Ordinal) && !path.StartsWith('&'))
                return path;
        }
        return null;
    }

    /// <summary>給人看的名稱：程式的檔名加上參數（轉向之前的），最多 60 個字</summary>
    public static string DisplayName(string command)
    {
        var (program, arguments) = SplitCommand(command);
        var name = program[(program.LastIndexOf('/') + 1)..];
        if (arguments != null)
        {
            int cut = arguments.IndexOfAny(new[] { '>', '|', ';', '&' });
            int end = cut;
            // 「2>檔案」的 2 是轉向的一部分（緊貼著 >、前面是空白），不是參數
            if (cut > 0 && arguments[cut] == '>' && char.IsDigit(arguments[cut - 1]) && (cut == 1 || arguments[cut - 2] == ' '))
                end = cut - 1;
            var shown = (cut >= 0 ? arguments[..end] : arguments).Trim();
            if (shown.Length > 0)
                name += " " + shown;
        }
        return name.Length <= 60 ? name : name[..59] + "…";
    }

    /// <summary>一行切成「時間」與「指令」；不是排程的寫法回 null</summary>
    internal static (string Schedule, string Command)? Split(string line)
    {
        if (line.StartsWith('@'))
        {
            int space = line.IndexOfAny(new[] { ' ', '\t' });
            return space < 0 ? null : (line[..space], line[space..].Trim());
        }
        int index = 0;
        for (int field = 0; field < 5; field++)
        {
            while (index < line.Length && char.IsWhiteSpace(line[index]))
                index++;
            int start = index;
            while (index < line.Length && !char.IsWhiteSpace(line[index]))
                index++;
            if (index == start)
                return null;
        }
        return index >= line.Length ? null : (line[..index].Trim(), line[index..].Trim());
    }

    /// <summary>指令切成程式與其餘的參數（第一個空白前；引號包著的整個算程式）</summary>
    internal static (string Program, string? Arguments) SplitCommand(string command)
    {
        command = command.Trim();
        if (command.Length > 0 && command[0] is '"' or '\'')
        {
            int close = command.IndexOf(command[0], 1);
            if (close > 0)
                return (command[1..close], command[(close + 1)..].Trim() is { Length: > 0 } rest ? rest : null);
        }
        int space = command.IndexOfAny(new[] { ' ', '\t' });
        return space < 0 ? (command, null) : (command[..space], command[space..].Trim() is { Length: > 0 } tail ? tail : null);
    }

    /// <summary>時間的寫法換成觸發條件；看不懂或太複雜的是一個「其他條件」</summary>
    internal static IReadOnlyList<ScheduleTrigger> Triggers(string schedule)
    {
        var unknown = new[] { new ScheduleTrigger { Kind = ScheduleTriggerKind.Unknown } };
        switch (schedule.Trim().ToLowerInvariant())
        {
            case "@reboot":
                return new[] { new ScheduleTrigger { Kind = ScheduleTriggerKind.Boot } };
            case "@hourly":
                schedule = "0 * * * *";
                break;
            case "@daily":
            case "@midnight":
                schedule = "0 0 * * *";
                break;
            case "@weekly":
                schedule = "0 0 * * 0";
                break;
            case "@monthly":
                schedule = "0 0 1 * *";
                break;
            case "@yearly":
            case "@annually":
                schedule = "0 0 1 1 *";
                break;
        }
        var fields = schedule.Split(new[] { ' ', '\t' }, StringSplitOptions.RemoveEmptyEntries);
        if (fields.Length != 5)
            return unknown;
        var minute = Field(fields[0], 0, 59, null);
        var hour = Field(fields[1], 0, 23, null);
        var day = Field(fields[2], 1, 31, null);
        var month = Field(fields[3], 1, 12, MonthNames);
        var weekday = Field(fields[4], 0, 7, DayNames);
        if (minute == null || hour == null || day == null || month == null || weekday == null)
            return unknown;

        var times = Times(minute, hour);
        if (times == null)
            return unknown;
        var months = month.All ? Array.Empty<int>() : month.Values;
        var weekdays = weekday.Values.Select(w => (DayOfWeek)(w % 7)).Distinct().Order().ToList();
        var result = new List<ScheduleTrigger>();
        foreach (var (time, interval, duration) in times)
        {
            // 日與星期都指定時兩個都算（任何一個符合就執行）
            if (!day.All)
                result.Add(CalendarTriggerRules.Make(ScheduleTriggerKind.Monthly, time, interval, duration, months: months, daysOfMonth: day.Values));
            if (!weekday.All)
            {
                result.Add(month.All
                    ? CalendarTriggerRules.Make(ScheduleTriggerKind.Weekly, time, interval, duration, daysOfWeek: weekdays)
                    : CalendarTriggerRules.Make(ScheduleTriggerKind.MonthlyDayOfWeek, time, interval, duration, months: months, daysOfWeek: weekdays, everyWeek: true));
            }
            if (day.All && weekday.All)
            {
                result.Add(month.All
                    ? CalendarTriggerRules.Make(ScheduleTriggerKind.Daily, time, interval, duration)
                    : CalendarTriggerRules.Make(ScheduleTriggerKind.Monthly, time, interval, duration, months: months, daysOfMonth: Enumerable.Range(1, 31).ToList()));
            }
            if (result.Count > MaxTriggers)
                return unknown;
        }
        return result;
    }

    /// <summary>一個欄位：All＝*；Step＝從 Values[0] 起每隔幾個（*/n、a-b/n 才有）；Values 由小到大</summary>
    internal sealed record CronField(bool All, int? Step, IReadOnlyList<int> Values);

    /// <summary>解析一個欄位；看不懂、超出範圍回 null</summary>
    internal static CronField? Field(string text, int min, int max, string[]? names)
    {
        if (text == "*")
            return new CronField(true, 1, Enumerable.Range(min, max - min + 1).ToList());
        var values = new SortedSet<int>();
        int? step = null;
        var parts = text.Split(',');
        foreach (var part in parts)
        {
            var range = part;
            int by = 1;
            int slash = part.IndexOf('/');
            if (slash >= 0)
            {
                if (!int.TryParse(part[(slash + 1)..], NumberStyles.None, CultureInfo.InvariantCulture, out by) || by <= 0)
                    return null;
                range = part[..slash];
            }
            int from, to;
            if (range == "*")
            {
                (from, to) = (min, max);
            }
            else
            {
                int dash = range.IndexOf('-');
                if (dash > 0)
                {
                    if (Number(range[..dash], names, min) is not { } a || Number(range[(dash + 1)..], names, min) is not { } b)
                        return null;
                    (from, to) = (a, b);
                }
                else
                {
                    if (Number(range, names, min) is not { } single)
                        return null;
                    // 「5/15」＝從 5 起每 15 個
                    (from, to) = (single, slash >= 0 ? max : single);
                }
            }
            if (from < min || to > max || from > to)
                return null;
            for (int v = from; v <= to; v += by)
                values.Add(v);
            // 整個欄位就是一個帶間隔的寫法才算「每隔幾個」
            if (slash >= 0 && parts.Length == 1)
                step = by;
        }
        return values.Count == 0 ? null : new CronField(false, step, values.ToList());
    }

    private static int? Number(string text, string[]? names, int min)
    {
        if (int.TryParse(text, NumberStyles.None, CultureInfo.InvariantCulture, out var n))
            return n;
        if (names == null)
            return null;
        int index = Array.IndexOf(names, text.ToLowerInvariant());
        // 月份的名稱從 1 起算、星期從 0（週日）起算
        return index < 0 ? null : index + (min == 1 ? 1 : 0);
    }

    /// <summary>
    /// 分與時換成一天裡的時刻：每個是（時刻、重複間隔、重複持續多久）。「*/15 *」是整天每 15 分鐘；「0 */2」是整天每 2 小時；
    /// 「*/10 9-17」是那幾個小時各自每 10 分鐘；其他逐一列出。超過 <see cref="MaxTriggers"/> 個回 null
    /// </summary>
    private static List<(TimeSpan Time, TimeSpan? Interval, TimeSpan? Duration)>? Times(CronField minute, CronField hour)
    {
        var result = new List<(TimeSpan, TimeSpan?, TimeSpan?)>();
        // 分鐘是「每隔幾分鐘」而且一小時除得盡、從頭開始
        bool minuteEvery = minute.Step is { } ms && 60 % ms == 0 && minute.Values[0] < ms && minute.Values.Count == 60 / ms;
        bool hourEvery = hour.Step is { } hs && 24 % hs == 0 && hour.Values[0] < hs && hour.Values.Count == 24 / hs;
        if (minuteEvery && hour.All)
        {
            result.Add((TimeSpan.FromMinutes(minute.Values[0]), TimeSpan.FromMinutes(minute.Step!.Value), null));
            return result;
        }
        if (minuteEvery)
        {
            foreach (var h in hour.Values)
                result.Add((new TimeSpan(h, minute.Values[0], 0), TimeSpan.FromMinutes(minute.Step!.Value), TimeSpan.FromHours(1)));
            return result.Count > MaxTriggers ? null : result;
        }
        if (hourEvery)
        {
            foreach (var m in minute.Values)
                result.Add((new TimeSpan(hour.Values[0], m, 0), TimeSpan.FromHours(hour.Step!.Value), null));
            return result.Count > MaxTriggers ? null : result;
        }
        if ((long)minute.Values.Count * hour.Values.Count > MaxTriggers)
            return null;
        foreach (var h in hour.Values)
        {
            foreach (var m in minute.Values)
                result.Add((new TimeSpan(h, m, 0), null, null));
        }
        return result;
    }
}
