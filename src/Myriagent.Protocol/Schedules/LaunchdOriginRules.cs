using System.Text.RegularExpressions;

namespace Myriagent.Schedules;

/// <summary>
/// 從代理執行的指令文字找出它對 macOS 的排程做了什麼（Windows 版找的是 schtasks 與 ScheduledTask 的 cmdlet，ScheduleOriginRules）：
/// <c>launchctl</c> 的 load／bootstrap（載入）、unload／bootout／remove（卸載）、enable、disable、kickstart／start（執行）、kill／stop（停止），
/// 提到 LaunchAgents、LaunchDaemons 底下的 .plist（寫入、複製、刪除），以及改 crontab（crontab -e、crontab 檔案、| crontab -、crontab -r）。
/// 名稱＝工作的標籤（或 .plist 的檔名去掉副檔名）；crontab 看不出是哪一行，名稱是 null。
/// 只是找線索：判斷錯了頂多多掃一次、少一個連結，不會因此動到任何排程
/// </summary>
internal static class LaunchdOriginRules
{
    private static readonly Regex Launchctl = new(@"(?<![\w.-])launchctl\s+(?<sub>[a-z-]+)(?<rest>[^|;&\r\n]*)", RegexOptions.Compiled);

    /// <summary>LaunchAgents／LaunchDaemons 底下的 .plist（路徑可以用引號包著、有空白時要包）</summary>
    private static readonly Regex PlistPath = new(@"(?<path>(?:~|\$HOME|\$\{HOME\}|/)[^\s""'|;&<>]*?/Launch(?:Agents|Daemons)/(?<name>[^/\s""'|;&<>]+)\.plist)", RegexOptions.Compiled);

    private static readonly Regex Crontab = new(@"(?<![\w.-])crontab(?<rest>[^|;&\r\n]*)", RegexOptions.Compiled);

    private static readonly Regex Delete = new(@"(?<![\w.-])(?:rm|unlink|trash)\s", RegexOptions.Compiled);

    /// <summary>網域開頭的工作名稱：gui/501/標籤、user/501/標籤、system/標籤</summary>
    private static readonly Regex ServiceTarget = new(@"^(?:gui|user)/\d+/(?<label>.+)$|^system/(?<label>.+)$", RegexOptions.Compiled);

    /// <summary>快速篩選：文字裡有沒有可能碰到 macOS 排程的字</summary>
    public static bool MayTouch(string text)
        => text.Contains("launchctl", StringComparison.Ordinal)
           || text.Contains("LaunchAgents", StringComparison.Ordinal)
           || text.Contains("LaunchDaemons", StringComparison.Ordinal)
           || text.Contains("crontab", StringComparison.Ordinal);

    public static IReadOnlyList<ScheduleCommandHit> Parse(string? text)
    {
        var hits = new List<ScheduleCommandHit>();
        if (string.IsNullOrEmpty(text) || !MayTouch(text))
            return hits;
        foreach (Match m in Launchctl.Matches(text))
        {
            ScheduleVerb? verb = m.Groups["sub"].Value switch
            {
                "load" or "bootstrap" => ScheduleVerb.Create,
                "unload" or "bootout" or "remove" => ScheduleVerb.Delete,
                "enable" => ScheduleVerb.Enable,
                "disable" => ScheduleVerb.Disable,
                "kickstart" or "start" => ScheduleVerb.Run,
                "kill" or "stop" => ScheduleVerb.End,
                _ => null,
            };
            if (verb == null)
                continue;
            hits.Add(new ScheduleCommandHit(verb.Value, TargetIn(m.Groups["rest"].Value)));
        }
        // 直接碰 plist 檔（寫入、複製、刪除；讀取也會算進來，但沒有變更就不會記成它做的）。launchctl load 〈plist〉 已經算過的不重複
        foreach (Match m in PlistPath.Matches(text))
        {
            var name = m.Groups["name"].Value;
            int lineStart = text.LastIndexOfAny(new[] { '\n', ';', '&', '|' }, m.Index) + 1;
            var before = text[lineStart..m.Index];
            if (before.Contains("launchctl", StringComparison.Ordinal))
                continue;
            hits.Add(new ScheduleCommandHit(Delete.IsMatch(before) ? ScheduleVerb.Delete : ScheduleVerb.Change, name));
        }
        foreach (Match m in Crontab.Matches(text))
        {
            var rest = m.Groups["rest"].Value.Trim();
            // crontab -l 只是列出；後面沒有東西而且前面不是管線（單獨一個 crontab 會等輸入，也算在改）
            if (rest == "-l" || rest.StartsWith("-l ", StringComparison.Ordinal))
                continue;
            hits.Add(new ScheduleCommandHit(rest.StartsWith("-r", StringComparison.Ordinal) ? ScheduleVerb.Delete : ScheduleVerb.Unknown, null));
        }
        return hits.Distinct().ToList();
    }

    /// <summary>launchctl 子指令後面的對象：網域開頭的工作名稱、.plist 的檔名、或單獨一個標籤（remove、start、stop）；看不出來回 null</summary>
    private static string? TargetIn(string rest)
    {
        string? fallback = null;
        foreach (var token in ScheduleOriginRules.Tokens(rest))
        {
            if (token.Length == 0 || token.StartsWith('-'))
                continue;
            var service = ServiceTarget.Match(token);
            if (service.Success)
                return service.Groups["label"].Value;
            if (token.EndsWith(".plist", StringComparison.OrdinalIgnoreCase))
                return StemOf(token);
            // 網域本身（bootstrap gui/501 〈plist〉）、訊號（kill SIGTERM …）、變數不是名稱
            if (token.Contains('/') || token.StartsWith("SIG", StringComparison.Ordinal) || token.Contains('$') || int.TryParse(token, out _))
                continue;
            fallback ??= token;
        }
        // 對象是算出來的（gui/$(id -u)/標籤 會被切散）：寧可說看不出來，也不要把 id 之類的字當成名稱
        return rest.Contains("$(", StringComparison.Ordinal) || rest.Contains('`') ? null : fallback;
    }

    /// <summary>.plist 路徑的檔名去掉副檔名（launchd 的慣例：檔名就是標籤）</summary>
    public static string StemOf(string path)
    {
        var name = path[(path.LastIndexOf('/') + 1)..];
        return name.EndsWith(".plist", StringComparison.OrdinalIgnoreCase) ? name[..^6] : name;
    }

    /// <summary>
    /// 指令裡的名稱對不對得上 macOS 的排程（taskPath 是 .plist 的完整路徑）：名稱是路徑的比結尾（~ 開頭的去掉 ~），否則比檔名去掉副檔名。
    /// crontab 的排程（taskPath 不是 / 開頭）沒有名稱可比
    /// </summary>
    public static bool NameMatches(string? hitName, string taskPath)
    {
        if (string.IsNullOrWhiteSpace(hitName) || !taskPath.StartsWith('/'))
            return false;
        var name = hitName.Trim();
        if (name.Contains('/'))
        {
            var tail = name.StartsWith('~') ? name[1..] : name;
            return taskPath.Equals(tail, StringComparison.OrdinalIgnoreCase) || (name.StartsWith('~') && taskPath.EndsWith(tail, StringComparison.OrdinalIgnoreCase));
        }
        return StemOf(taskPath).Equals(name, StringComparison.OrdinalIgnoreCase);
    }
}
