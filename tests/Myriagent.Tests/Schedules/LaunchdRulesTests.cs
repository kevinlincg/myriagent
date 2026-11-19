using System.Xml;
using Myriagent.Hub;
using Myriagent.Schedules;

namespace Myriagent.Tests.Schedules;

/// <summary>macOS 的排程定義（launchd 的 plist）讀成排程頁共用的定義（以 2026-11-19 一台 Mac 上實際的工作當語料）</summary>
public sealed class LaunchdPlistRulesTests
{
    private static readonly DateTime Anchor = new(2026, 11, 19, 21, 13, 12, DateTimeKind.Local);

    /// <summary>使用者自己的清理工作：zsh 跑一支腳本（路徑有空白）、每小時一次、有輸出檔</summary>
    internal const string GuardianPlist = """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
          <key>Label</key>
          <string>com.example.disk-space-guardian</string>
          <key>ProgramArguments</key>
          <array>
            <string>/bin/zsh</string>
            <string>/Users/u/Library/Application Support/Guardian/cleanup.zsh</string>
            <string>--quiet</string>
          </array>
          <key>StartInterval</key>
          <integer>3600</integer>
          <key>RunAtLoad</key>
          <false/>
          <key>StandardOutPath</key>
          <string>/Users/u/Library/Logs/Guardian/out.log</string>
          <key>StandardErrorPath</key>
          <string>/Users/u/Library/Logs/Guardian/err.log</string>
        </dict>
        </plist>
        """;

    private static string Plist(string body)
        => "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n"
           + "<plist version=\"1.0\"><dict><key>Label</key><string>com.example.job</string>" + body + "</dict></plist>";

    private static LaunchdJob Job(string body, LaunchdDomain domain = LaunchdDomain.UserAgent)
        => LaunchdPlistRules.ToJob(Plist(body), domain, "u", null, Anchor);

    private static string When(string body, LaunchdDomain domain = LaunchdDomain.UserAgent)
        => ScheduleTriggerRules.Describe(Job(body, domain).Definition.Triggers);

    [Fact]
    public void ParsePlist_帶DOCTYPE的照樣讀_各種型別()
    {
        var plist = (Dictionary<string, object?>)LaunchdPlistRules.ParsePlist(Plist(
            "<key>N</key><integer>42</integer><key>R</key><real>1.5</real><key>T</key><true/><key>F</key><false/>"
            + "<key>A</key><array><string>x</string><integer>2</integer></array><key>D</key><dict><key>k</key><string>v</string></dict>"))!;

        plist["Label"].Should().Be("com.example.job");
        plist["N"].Should().Be(42L);
        plist["R"].Should().Be(1.5);
        plist["T"].Should().Be(true);
        plist["F"].Should().Be(false);
        ((List<object?>)plist["A"]!).Should().Equal("x", 2L);
        ((Dictionary<string, object?>)plist["D"]!)["k"].Should().Be("v");
    }

    [Fact]
    public void ParsePlist_不是plist或不是XML丟XmlException()
    {
        var notPlist = () => LaunchdPlistRules.ParsePlist("<Task><Actions/></Task>");
        notPlist.Should().Throw<XmlException>();
        var notXml = () => LaunchdPlistRules.ParsePlist("bplist00\u0001\u0002");
        notXml.Should().Throw<XmlException>();
        var notDict = () => LaunchdPlistRules.ToJob("<plist version=\"1.0\"><array/></plist>", LaunchdDomain.UserAgent, "u", null, null);
        notDict.Should().Throw<XmlException>();
    }

    [Fact]
    public void ToJob_實際的工作_指令_每小時_輸出檔_執行身分()
    {
        var job = LaunchdPlistRules.ToJob(GuardianPlist, LaunchdDomain.UserAgent, "u", new DateTime(2026, 1, 2, 3, 4, 5), Anchor);

        job.Label.Should().Be("com.example.disk-space-guardian");
        job.StandardOutPath.Should().Be("/Users/u/Library/Logs/Guardian/out.log");
        job.StandardErrorPath.Should().Be("/Users/u/Library/Logs/Guardian/err.log");
        var d = job.Definition;
        d.Actions.Should().ContainSingle();
        d.Actions[0].Should().Be(new ScheduleAction("Exec", "/bin/zsh", "\"/Users/u/Library/Application Support/Guardian/cleanup.zsh\" --quiet", null));
        ScheduleClassifyRules.ActionText(d.Actions).Should().Be("/bin/zsh \"/Users/u/Library/Application Support/Guardian/cleanup.zsh\" --quiet");
        ScheduleTriggerRules.Describe(d.Triggers).Should().Be("每小時（21:13 起）", "從上次開始執行的時間起算");
        d.Triggers.Should().ContainSingle("RunAtLoad 是 false，不算登入時");
        d.RunAs.Should().Be("u");
        d.LogonType.Should().Be(LaunchdPlistRules.AgentLogonType);
        d.Elevated.Should().BeFalse();
        d.Enabled.Should().BeTrue();
        d.Created.Should().Be(new DateTime(2026, 1, 2, 3, 4, 5));
        d.Description.Should().Be("輸出記到 /Users/u/Library/Logs/Guardian/out.log；錯誤記到 /Users/u/Library/Logs/Guardian/err.log");
        d.StartWhenAvailable.Should().BeFalse("每隔多久的工作睡著時不補跑");
        ScheduleAgentPromptRules.RunAsText(d).Should().Be("u，只在使用者登入時執行");

        // 接下來的執行時間從起算時間往後每小時一次
        ScheduleTriggerRules.Occurrences(d.Triggers, new DateTime(2026, 11, 19, 22, 0, 0), new DateTime(2026, 11, 20, 1, 0, 0))
            .Should().Equal(new DateTime(2026, 11, 19, 22, 13, 12), new DateTime(2026, 11, 19, 23, 13, 12), new DateTime(2026, 11, 20, 0, 13, 12));
    }

    [Fact]
    public void StartCalendarInterval_沒寫的欄位是萬用字元()
    {
        const string cal = "<key>StartCalendarInterval</key>";
        When(cal + "<dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>30</integer></dict>").Should().Be("每天 03:30");
        When(cal + "<dict><key>Minute</key><integer>15</integer></dict>").Should().Be("每小時（00:15 起）", "只寫分鐘＝每小時的那一分");
        When(cal + "<dict><key>Hour</key><integer>2</integer></dict>").Should().Be("每天 02:00 起 每 1 分鐘，持續 1 小時", "只寫小時＝那一小時裡每分鐘");
        When(cal + "<dict/>").Should().Be("每 1 分鐘（00:00 起）");
        When(cal + "<dict><key>Day</key><integer>1</integer><key>Hour</key><integer>0</integer><key>Minute</key><integer>0</integer></dict>").Should().Be("每月 1 日 00:00");
        When(cal + "<dict><key>Weekday</key><integer>7</integer><key>Hour</key><integer>8</integer><key>Minute</key><integer>5</integer></dict>").Should().Be("每週日 08:05", "7 也是週日");
        When(cal + "<dict><key>Month</key><integer>3</integer><key>Day</key><integer>9</integer><key>Hour</key><integer>8</integer><key>Minute</key><integer>0</integer></dict>")
            .Should().Be("每年 3 月的 9 日 08:00");
        When(cal + "<dict><key>Minute</key><integer>99</integer><key>Hour</key><integer>3</integer></dict>").Should().Be("每天 03:00 起 每 1 分鐘，持續 1 小時", "超出範圍的當成沒寫");

        Job(cal + "<dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>30</integer></dict>").Definition.StartWhenAvailable
            .Should().BeTrue("時間到了電腦在睡，launchd 醒來會補跑");
        var daily = Job(cal + "<dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>30</integer></dict>").Definition.Triggers;
        ScheduleTriggerRules.Occurrences(daily, new DateTime(2026, 11, 19, 10, 0, 0), new DateTime(2026, 11, 21, 10, 0, 0))
            .Should().Equal(new DateTime(2026, 11, 20, 3, 30, 0), new DateTime(2026, 11, 21, 3, 30, 0));
    }

    [Fact]
    public void StartCalendarInterval_只差星期幾的幾個併成一個每週_日與星期都指定時兩個都算()
    {
        string Day(int weekday, int hour = 9) => $"<dict><key>Weekday</key><integer>{weekday}</integer><key>Hour</key><integer>{hour}</integer><key>Minute</key><integer>0</integer></dict>";
        var weekdays = Job("<key>StartCalendarInterval</key><array>" + Day(1) + Day(2) + Day(3) + Day(4) + Day(5) + "</array>").Definition.Triggers;
        weekdays.Should().ContainSingle();
        weekdays[0].Kind.Should().Be(ScheduleTriggerKind.Weekly);
        weekdays[0].DaysOfWeek.Should().Equal(DayOfWeek.Monday, DayOfWeek.Tuesday, DayOfWeek.Wednesday, DayOfWeek.Thursday, DayOfWeek.Friday);
        ScheduleTriggerRules.Describe(weekdays).Should().Be("平日 09:00");

        When("<key>StartCalendarInterval</key><array>" + Day(1) + Day(3, 18) + "</array>").Should().Be("每週一 09:00；每週三 18:00", "時間不同的不併");

        var either = Job("<key>StartCalendarInterval</key><dict><key>Day</key><integer>15</integer><key>Weekday</key><integer>5</integer>"
                         + "<key>Hour</key><integer>6</integer><key>Minute</key><integer>0</integer></dict>").Definition.Triggers;
        either.Select(t => t.Kind).Should().Equal(ScheduleTriggerKind.Monthly, ScheduleTriggerKind.Weekly);
        ScheduleTriggerRules.Describe(either).Should().Be("每月 15 日 06:00；每週五 06:00");

        var monthDow = Job("<key>StartCalendarInterval</key><dict><key>Month</key><integer>6</integer><key>Weekday</key><integer>1</integer>"
                           + "<key>Hour</key><integer>6</integer><key>Minute</key><integer>0</integer></dict>").Definition.Triggers;
        monthDow.Should().ContainSingle();
        monthDow[0].Kind.Should().Be(ScheduleTriggerKind.MonthlyDayOfWeek);
        ScheduleTriggerRules.Occurrences(monthDow, new DateTime(2026, 6, 1), new DateTime(2026, 7, 1)).Select(x => x.Day)
            .Should().Equal(new[] { 1, 8, 15, 22, 29 }, "六月的每個週一");
    }

    [Fact]
    public void StartInterval_一天除不盡的從起算時間一直重複()
    {
        var odd = Job("<key>StartInterval</key><integer>25200</integer>").Definition.Triggers;
        odd.Should().ContainSingle();
        odd[0].Kind.Should().Be(ScheduleTriggerKind.Once);
        odd[0].RepeatInterval.Should().Be(TimeSpan.FromHours(7));
        ScheduleTriggerRules.Occurrences(odd, Anchor.AddMinutes(1), Anchor.AddHours(15)).Should().Equal(Anchor.AddHours(7), Anchor.AddHours(14));

        Job("<key>StartInterval</key><integer>0</integer>").Definition.Triggers.Should().BeEmpty();
        LaunchdPlistRules.ToJob(Plist("<key>StartInterval</key><integer>900</integer>"), LaunchdDomain.UserAgent, "u", null, null)
            .Definition.Triggers[0].Start.Should().Be(LaunchdPlistRules.BaseDate, "不知道從什麼時候起算");
    }

    [Fact]
    public void RunAtLoad與KeepAlive_登入時或開機時_事件_停用()
    {
        When("<key>RunAtLoad</key><true/>").Should().Be("u 登入時");
        When("<key>RunAtLoad</key><true/>", LaunchdDomain.GlobalAgent).Should().Be("任何人登入時");
        When("<key>RunAtLoad</key><true/>", LaunchdDomain.Daemon).Should().Be("開機時");
        When("").Should().Be("只能手動執行");

        var keep = Job("<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>").Definition;
        ScheduleTriggerRules.Describe(keep.Triggers).Should().Be("u 登入時");
        keep.Description.Should().Be("保持執行：結束後 launchd 會重新啟動");
        Job("<key>KeepAlive</key><false/>").Definition.Triggers.Should().BeEmpty();

        var watch = Job("<key>WatchPaths</key><array><string>/Users/u/inbox</string></array><key>StartOnMount</key><true/>").Definition;
        ScheduleTriggerRules.Describe(watch.Triggers).Should().Be("發生特定事件時");
        watch.Description.Should().Be("這些路徑有變動時執行：/Users/u/inbox；掛上磁碟時執行");

        Job("<key>Disabled</key><true/>").Definition.Enabled.Should().BeFalse();
        Job("<key>StandardOutPath</key><string>/tmp/a.log</string><key>StandardErrorPath</key><string>/tmp/a.log</string>").Definition.Description
            .Should().Be("輸出與錯誤記到 /tmp/a.log");
    }

    [Fact]
    public void 執行身分_開機載入的預設是root_指定使用者就不是最高權限()
    {
        var root = Job("<key>Program</key><string>/usr/local/bin/tool</string>", LaunchdDomain.Daemon).Definition;
        root.RunAs.Should().Be("root");
        root.Elevated.Should().BeTrue();
        root.LogonType.Should().Be(LaunchdPlistRules.DaemonLogonType);
        ScheduleAgentPromptRules.RunAsText(root).Should().Be("root，開機後就執行，不論使用者是否登入，以最高權限執行");

        var named = Job("<key>UserName</key><string>_www</string>", LaunchdDomain.Daemon).Definition;
        named.RunAs.Should().Be("_www");
        named.Elevated.Should().BeFalse();

        var global = Job("", LaunchdDomain.GlobalAgent).Definition;
        global.RunAs.Should().BeNull();
        ScheduleAgentPromptRules.RunAsText(global).Should().Be("登入的使用者，每個使用者登入時各自執行");
    }

    [Fact]
    public void 動作_Program與ProgramArguments_引號_沒有程式就沒有動作()
    {
        Job("<key>Program</key><string>/opt/x/bin/tool</string><key>ProgramArguments</key><array><string>tool</string><string>run</string><string></string></array>"
            + "<key>WorkingDirectory</key><string>/Users/u/proj</string>")
            .Definition.Actions.Should().Equal(new ScheduleAction("Exec", "/opt/x/bin/tool", "run \"\"", "/Users/u/proj"));
        Job("<key>ProgramArguments</key><array><string>/bin/sh</string><string>-c</string><string>echo \"hi\" there</string></array>")
            .Definition.Actions.Should().Equal(new ScheduleAction("Exec", "/bin/sh", "-c \"echo \\\"hi\\\" there\"", null));
        Job("<key>Program</key><string>/opt/x/bin/tool</string>").Definition.Actions.Should().Equal(new ScheduleAction("Exec", "/opt/x/bin/tool", null, null));
        Job("").Definition.Actions.Should().BeEmpty();
        LaunchdPlistRules.ToJob("<plist version=\"1.0\"><dict/></plist>", LaunchdDomain.UserAgent, "u", null, null).Label.Should().BeNull();
    }
}

/// <summary>crontab 讀成排程頁共用的定義</summary>
public sealed class CronRulesTests
{
    private static string When(string schedule) => ScheduleTriggerRules.Describe(CronRules.Triggers(schedule));

    [Fact]
    public void Parse_註解空行與環境變數不是排程_指令切成程式與參數()
    {
        const string crontab = """
            # 每 15 分鐘清一次
            SHELL=/bin/bash
            MAILTO=""

            */15 * * * * /Users/u/bin/guard.sh 8 4 >> /Users/u/guard.log 2>&1
            @reboot "/Users/u/my tools/start" --fast
            不是排程的一行
            0 9 * * 1-5
            """;

        var entries = CronRules.Parse(crontab, "u");

        entries.Should().HaveCount(2);
        entries[0].Schedule.Should().Be("*/15 * * * *");
        entries[0].Command.Should().Be("/Users/u/bin/guard.sh 8 4 >> /Users/u/guard.log 2>&1");
        entries[0].Line.Should().Be("*/15 * * * * /Users/u/bin/guard.sh 8 4 >> /Users/u/guard.log 2>&1");
        entries[0].Definition.Actions.Should().Equal(new ScheduleAction("Exec", "/Users/u/bin/guard.sh", "8 4 >> /Users/u/guard.log 2>&1", null));
        entries[0].Definition.RunAs.Should().Be("u");
        entries[0].Definition.LogonType.Should().Be(CronRules.LogonType);
        ScheduleAgentPromptRules.RunAsText(entries[0].Definition).Should().Be("u，不論使用者是否登入都執行（cron）");
        ScheduleTriggerRules.Describe(entries[0].Definition.Triggers).Should().Be("每 15 分鐘（00:00 起）");
        entries[1].Definition.Actions.Should().Equal(new ScheduleAction("Exec", "/Users/u/my tools/start", "--fast", null));
        ScheduleTriggerRules.Describe(entries[1].Definition.Triggers).Should().Be("開機時");

        CronRules.Variables(crontab).Should().Equal(new Dictionary<string, string> { ["SHELL"] = "/bin/bash", ["MAILTO"] = "" });
        CronRules.Parse(null, "u").Should().BeEmpty();
    }

    [Fact]
    public void Parse_身分跟著指令_改時間不換身分_同一個指令排兩次加編號()
    {
        var before = CronRules.Parse("0 3 * * * /a/b.sh\n", "u").Single();
        var after = CronRules.Parse("30 4 * * 1 /a/b.sh\n", "u").Single();
        after.Key.Should().Be(before.Key, "改的是時間");
        before.Key.Should().HaveLength(10);
        CronRules.Parse("0 3 * * * /a/other.sh\n", "u").Single().Key.Should().NotBe(before.Key);

        var twice = CronRules.Parse("0 3 * * * /a/b.sh\n0 15 * * * /a/b.sh\n", "u");
        twice.Select(e => e.Key).Should().Equal(before.Key, before.Key + "-2");
    }

    [Theory]
    [InlineData("0 3 * * *", "每天 03:00")]
    [InlineData("*/15 * * * *", "每 15 分鐘（00:00 起）")]
    [InlineData("* * * * *", "每 1 分鐘（00:00 起）")]
    [InlineData("30 * * * *", "每小時（00:30 起）")]
    [InlineData("0 */2 * * *", "每 2 小時（00:00 起）")]
    [InlineData("0 9 * * 1-5", "平日 09:00")]
    [InlineData("0 9 * * mon-fri", "平日 09:00")]
    [InlineData("0 10 * * 6,0", "週末 10:00")]
    [InlineData("0 10 * * 7", "每週日 10:00")]
    [InlineData("30 2 1 * *", "每月 1 日 02:30")]
    [InlineData("0 8 1 jan *", "每年 1 月的 1 日 08:00")]
    [InlineData("0 6,18 * * *", "每天 06:00；每天 18:00")]
    [InlineData("*/10 9 * * *", "每天 09:00 起 每 10 分鐘，持續 1 小時")]
    [InlineData("@hourly", "每小時（00:00 起）")]
    [InlineData("@daily", "每天 00:00")]
    [InlineData("@weekly", "每週日 00:00")]
    [InlineData("@monthly", "每月 1 日 00:00")]
    [InlineData("@reboot", "開機時")]
    [InlineData("61 * * * *", "其他條件")]
    [InlineData("* * *", "其他條件")]
    [InlineData("a b c d e", "其他條件")]
    [InlineData("0-59 0-23 * * *", "其他條件")]
    public void Triggers_時間的寫法(string schedule, string expected) => When(schedule).Should().Be(expected);

    [Fact]
    public void Triggers_日與星期都指定時兩個都算_間隔不是從頭開始的逐一列出()
    {
        CronRules.Triggers("0 6 15 * 5").Select(t => t.Kind).Should().Equal(ScheduleTriggerKind.Monthly, ScheduleTriggerKind.Weekly);
        When("5/20 3 * * *").Should().Be("每天 03:05 起 每 20 分鐘，持續 1 小時", "從 5 分起每 20 分鐘");
        ScheduleTriggerRules.Occurrences(CronRules.Triggers("5/20 3 * * *"), new DateTime(2026, 11, 19, 0, 0, 0), new DateTime(2026, 11, 20, 0, 0, 0))
            .Should().Equal(new DateTime(2026, 11, 19, 3, 5, 0), new DateTime(2026, 11, 19, 3, 25, 0), new DateTime(2026, 11, 19, 3, 45, 0));
        When("1,31 3 * * *").Should().Be("每天 03:01；每天 03:31", "不是整齊的間隔就逐一列出");
        CronRules.DisplayName("/a/b.sh x 2>/dev/null").Should().Be("b.sh x", "2> 的 2 是轉向的一部分");
        ScheduleTriggerRules.Occurrences(CronRules.Triggers("*/15 * * * *"), new DateTime(2026, 11, 19, 10, 1, 0), new DateTime(2026, 11, 19, 11, 0, 0))
            .Should().Equal(new DateTime(2026, 11, 19, 10, 15, 0), new DateTime(2026, 11, 19, 10, 30, 0), new DateTime(2026, 11, 19, 10, 45, 0));
        ScheduleTriggerRules.Occurrences(CronRules.Triggers("0 9 * * 1-5"), new DateTime(2026, 11, 20, 10, 0, 0), new DateTime(2026, 11, 24, 10, 0, 0))
            .Should().Equal(new[] { new DateTime(2026, 11, 23, 9, 0, 0), new DateTime(2026, 11, 24, 9, 0, 0) }, "11/20 是週五（已過），週末不跑");
    }

    [Fact]
    public void Field_數字範圍清單間隔與名稱()
    {
        CronRules.Field("*", 0, 59, null)!.All.Should().BeTrue();
        CronRules.Field("1,15", 1, 31, null)!.Values.Should().Equal(1, 15);
        CronRules.Field("1-5", 0, 7, null)!.Values.Should().Equal(1, 2, 3, 4, 5);
        CronRules.Field("0-30/10", 0, 59, null)!.Values.Should().Equal(0, 10, 20, 30);
        CronRules.Field("*/20", 0, 59, null)!.Step.Should().Be(20);
        CronRules.Field("1,*/20", 0, 59, null)!.Step.Should().BeNull("清單裡夾著間隔的不算整齊的每隔幾個");
        CronRules.Field("sep", 1, 12, new[] { "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec" })!.Values.Should().Equal(9);
        CronRules.Field("60", 0, 59, null).Should().BeNull();
        CronRules.Field("5-1", 0, 59, null).Should().BeNull();
        CronRules.Field("*/0", 0, 59, null).Should().BeNull();
        CronRules.Field("-1", 0, 59, null).Should().BeNull();
        CronRules.Field("", 0, 59, null).Should().BeNull();
    }

    [Fact]
    public void LogPath與DisplayName()
    {
        CronRules.LogPath("/a/b.sh 8 4 >> /Users/u/guard.log 2>&1").Should().Be("/Users/u/guard.log");
        CronRules.LogPath("/a/b.sh > \"/tmp/x.log\"").Should().Be("/tmp/x.log");
        CronRules.LogPath("/a/b.sh >/dev/null 2>&1").Should().BeNull();
        CronRules.LogPath("/a/b.sh 2>/tmp/err.log").Should().BeNull("只有錯誤輸出的轉向不算");
        CronRules.LogPath("/a/b.sh").Should().BeNull();

        CronRules.DisplayName("/Users/u/bin/guard.sh 8 4 >> /Users/u/guard.log 2>&1").Should().Be("guard.sh 8 4");
        CronRules.DisplayName("/usr/bin/true").Should().Be("true");
        CronRules.DisplayName("cd /a && ./run").Should().Be("cd /a");
        CronRules.DisplayName("/a/b.sh " + new string('x', 80)).Should().HaveLength(60).And.EndWith("…");
    }
}

/// <summary>launchd 工作的狀態：記錄檔、launchctl 的輸出、合出排程頁要的狀態</summary>
public sealed class LaunchdStateRulesTests
{
    private const string Spawned = "2026-11-19 21:13:12.378848 (gui/501/com.example.guardian [92888]) <Notice>: Successfully spawned zsh[92888] because interval";
    private const string Exited = "2026-11-19 21:13:19.806553 (gui/501/com.example.guardian [92888]) <Notice>: exited due to exit(3), ran for 7434ms";

    [Fact]
    public void ParseLogLine_開始與結束_被訊號結束是負的訊號編號_其他的行不管()
    {
        LaunchdStateRules.ParseLogLine(Spawned).Should().Be(new LaunchdLogEvent(new DateTime(2026, 11, 19, 21, 13, 12), "gui/501/com.example.guardian", false, 0));
        LaunchdStateRules.ParseLogLine(Exited).Should().Be(new LaunchdLogEvent(new DateTime(2026, 11, 19, 21, 13, 19), "gui/501/com.example.guardian", true, 3));
        LaunchdStateRules.ParseLogLine(Spawned)!.At.Kind.Should().Be(DateTimeKind.Local);
        LaunchdStateRules.ParseLogLine("2026-11-19 21:00:00.000001 (system/com.example.d [7]) <Notice>: exited due to SIGKILL | sent by mds[603]")
            .Should().Be(new LaunchdLogEvent(new DateTime(2026, 11, 19, 21, 0, 0), "system/com.example.d", true, -9));
        LaunchdStateRules.ParseLogLine("2026-11-19 21:00:00.000001 (system/com.example.d [7]) <Notice>: exited due to SIGTERM | sent by launchd[1]")!.Code.Should().Be(-15);
        LaunchdStateRules.ParseLogLine("2026-11-19 21:00:00.000001 (system/com.example.d [7]) <Notice>: exited due to SIGWEIRD | sent by x[1]")!.Code
            .Should().Be(-LaunchdStateRules.UnknownSignal);

        LaunchdStateRules.ParseLogLine("2026-11-19 21:13:12.371885 (gui/501/com.example.guardian [92888]) <Notice>: service state: running").Should().BeNull();
        LaunchdStateRules.ParseLogLine("2026-11-19 21:13:19.806578 (gui/501 [100002]) <Notice>: service inactive: com.example.guardian").Should().BeNull();
        LaunchdStateRules.ParseLogLine("亂寫的一行 exited due to exit(1)").Should().BeNull();
        LaunchdStateRules.ParseLogLine("").Should().BeNull();
        LaunchdStateRules.MayMatter(Spawned).Should().BeTrue();
        LaunchdStateRules.MayMatter("service state: running").Should().BeFalse();
    }

    [Fact]
    public void Apply_開始之後還沒結束是執行中_結束記下那一次的開始與結束碼_沒看到開始的用結束的時間()
    {
        var runs = new Dictionary<string, LaunchdRun>();
        const string key = "gui/501/com.example.guardian";
        var t0 = new DateTime(2026, 11, 19, 21, 13, 12);

        LaunchdStateRules.Apply(runs, new LaunchdLogEvent(t0, key, false, 0));
        runs[key].Running.Should().BeTrue();
        runs[key].Code.Should().BeNull();

        LaunchdStateRules.Apply(runs, new LaunchdLogEvent(t0.AddSeconds(7), key, true, 3));
        runs[key].Should().Be(new LaunchdRun(t0, t0, t0.AddSeconds(7), 3));
        runs[key].Running.Should().BeFalse();

        LaunchdStateRules.Apply(runs, new LaunchdLogEvent(t0.AddHours(1), key, false, 0));
        runs[key].Running.Should().BeTrue("又開始了一次");
        runs[key].FinishedStart.Should().Be(t0, "上一次跑完的那次還在");
        runs[key].Code.Should().Be(3);

        LaunchdStateRules.Apply(runs, new LaunchdLogEvent(t0.AddHours(1).AddSeconds(5), key, true, 0));
        runs[key].Should().Be(new LaunchdRun(t0.AddHours(1), t0.AddHours(1), t0.AddHours(1).AddSeconds(5), 0));

        // 記錄檔已經輪替、只看到結束：當成結束的那一刻開始的
        LaunchdStateRules.Apply(runs, new LaunchdLogEvent(t0, "system/x", true, 78));
        runs["system/x"].Should().Be(new LaunchdRun(null, t0, t0, 78));
        // 同一個開始不會被第二個結束重複拿來用
        LaunchdStateRules.Apply(runs, new LaunchdLogEvent(t0.AddHours(2), key, true, 1));
        runs[key].FinishedStart.Should().Be(t0.AddHours(2), "這次結束之前沒有新的開始");
    }

    [Fact]
    public void ServiceKey_使用者的在gui網域_開機載入的在system()
    {
        LaunchdStateRules.ServiceKey(LaunchdDomain.UserAgent, 501, "com.x").Should().Be("gui/501/com.x");
        LaunchdStateRules.ServiceKey(LaunchdDomain.GlobalAgent, 501, "com.x").Should().Be("gui/501/com.x");
        LaunchdStateRules.ServiceKey(LaunchdDomain.Daemon, 501, "com.x").Should().Be("system/com.x");
    }

    [Fact]
    public void 解析launchctl的輸出()
    {
        var list = LaunchdStateRules.ParseList("PID\tStatus\tLabel\n-\t0\tcom.example.guardian\n65880\t-15\tcom.example.bot\n-\t78\tcom.example.broken\n\n壞掉的行\n");
        list.Should().HaveCount(3);
        list["com.example.guardian"].Should().Be(new LaunchdLoaded(null, 0));
        list["com.example.bot"].Should().Be(new LaunchdLoaded(65880, -15));
        list["com.example.broken"].Should().Be(new LaunchdLoaded(null, 78));
        LaunchdStateRules.ParseList(null).Should().BeEmpty();

        var system = LaunchdStateRules.ParseSystemServices("""
            system = {
            	type = system
            	services = {
            		     668      - 	com.apple.driver
            		       0     78 	com.example.daemon
            		       0      - 	com.example.idle
            	}

            	other = {
            		       1      - 	not.a.service
            	}
            }
            """);
        system.Should().HaveCount(3);
        system["com.apple.driver"].Should().Be(new LaunchdLoaded(668, null));
        system["com.example.daemon"].Should().Be(new LaunchdLoaded(null, 78));
        system["com.example.idle"].Should().Be(new LaunchdLoaded(null, null));

        var disabled = LaunchdStateRules.ParseDisabled("\tdisabled services = {\n\t\t\"com.example.a\" => enabled\n\t\t\"com.example.b\" => disabled\n\t\t\"com.example.c\" => true\n\t}\n");
        disabled.Should().Equal(new Dictionary<string, bool> { ["com.example.a"] = false, ["com.example.b"] = true, ["com.example.c"] = true });
    }

    [Fact]
    public void Resolve_狀態與上次執行從最準的來源取()
    {
        var start = new DateTime(2026, 11, 19, 21, 13, 12);
        var loaded = new LaunchdLoaded(null, 0);
        var finished = new LaunchdRun(start, start, start.AddSeconds(7), 3);

        // 記錄檔有：最準
        LaunchdStateRules.Resolve(true, null, loaded, finished, null, start.AddDays(-1))
            .Should().Be(new LaunchdState(ScheduledTaskStateKind.Ready, true, start, 3));
        // 在跑：結果是「執行中」，不算一次執行的結果
        LaunchdStateRules.Resolve(true, null, new LaunchdLoaded(42, 0), new LaunchdRun(start, null, null, null), start.AddMinutes(-5), null)
            .Should().Be(new LaunchdState(ScheduledTaskStateKind.Running, true, start, ScheduleResultRules.Running));
        LaunchdStateRules.Resolve(true, null, new LaunchdLoaded(42, -15), null, start.AddMinutes(-5), null)
            .Should().Be(new LaunchdState(ScheduledTaskStateKind.Running, true, start.AddMinutes(-5), ScheduleResultRules.Running), "記錄檔沒有就用程序的開始時間");
        // 記錄檔沒有、沒在跑：輸出檔最後寫入的時間配 launchctl 的結束碼
        LaunchdStateRules.Resolve(true, null, new LaunchdLoaded(null, 78), null, null, start)
            .Should().Be(new LaunchdState(ScheduledTaskStateKind.Ready, true, start, 78));
        // 什麼都不知道：不亂猜
        LaunchdStateRules.Resolve(true, null, loaded, null, null, null)
            .Should().Be(new LaunchdState(ScheduledTaskStateKind.Ready, true, null, ScheduleResultRules.HasNotRun));
        // 檔案在、launchd 還沒載入
        LaunchdStateRules.Resolve(true, null, null, null, null, start)
            .Should().Be(new LaunchdState(ScheduledTaskStateKind.Unknown, true, null, ScheduleResultRules.HasNotRun));
        // 停用：launchctl 的記載優先於 plist
        LaunchdStateRules.Resolve(true, true, null, null, null, null).State.Should().Be(ScheduledTaskStateKind.Disabled);
        LaunchdStateRules.Resolve(false, null, loaded, null, null, null).Enabled.Should().BeFalse();
        LaunchdStateRules.Resolve(false, false, loaded, null, null, null).Should().Be(new LaunchdState(ScheduledTaskStateKind.Ready, true, null, ScheduleResultRules.HasNotRun));
        ((int)ScheduledTaskStateKind.Running).Should().Be((int)ScheduledTaskState.Running, "兩個列舉的值要一樣（讀取器直接轉）");
        ((int)ScheduledTaskStateKind.Disabled).Should().Be((int)ScheduledTaskState.Disabled);
        ((int)ScheduledTaskStateKind.Ready).Should().Be((int)ScheduledTaskState.Ready);
        ((int)ScheduledTaskStateKind.Unknown).Should().Be((int)ScheduledTaskState.Unknown);
    }
}

/// <summary>從代理的指令看出它碰了哪個 macOS 的排程</summary>
public sealed class LaunchdOriginRulesTests
{
    private static ScheduleCommandHit Hit(ScheduleVerb verb, string? name) => new(verb, name);

    [Fact]
    public void Parse_launchctl的各種子指令()
    {
        LaunchdOriginRules.Parse("launchctl bootstrap gui/501 ~/Library/LaunchAgents/com.example.job.plist").Should().Equal(Hit(ScheduleVerb.Create, "com.example.job"));
        LaunchdOriginRules.Parse("launchctl load -w \"/Users/u/Library/LaunchAgents/com.example.job.plist\"").Should().Equal(Hit(ScheduleVerb.Create, "com.example.job"));
        LaunchdOriginRules.Parse("launchctl bootout gui/$(id -u)/com.example.job").Should().Equal(new[] { Hit(ScheduleVerb.Delete, null) }, "網域是算出來的，看不出名稱");
        LaunchdOriginRules.Parse("launchctl bootout gui/501/com.example.job").Should().Equal(Hit(ScheduleVerb.Delete, "com.example.job"));
        LaunchdOriginRules.Parse("sudo launchctl bootout system/com.example.daemon").Should().Equal(Hit(ScheduleVerb.Delete, "com.example.daemon"));
        LaunchdOriginRules.Parse("launchctl remove com.example.job").Should().Equal(Hit(ScheduleVerb.Delete, "com.example.job"));
        LaunchdOriginRules.Parse("launchctl kickstart -k gui/501/com.example.job").Should().Equal(Hit(ScheduleVerb.Run, "com.example.job"));
        LaunchdOriginRules.Parse("launchctl kill SIGTERM gui/501/com.example.job").Should().Equal(Hit(ScheduleVerb.End, "com.example.job"));
        LaunchdOriginRules.Parse("launchctl disable user/501/com.example.job && launchctl enable gui/501/com.example.other")
            .Should().Equal(Hit(ScheduleVerb.Disable, "com.example.job"), Hit(ScheduleVerb.Enable, "com.example.other"));
        LaunchdOriginRules.Parse("launchctl list | grep example").Should().BeEmpty("只是列出");
        LaunchdOriginRules.Parse("launchctl print gui/501/com.example.job").Should().BeEmpty();
        LaunchdOriginRules.Parse("echo hello").Should().BeEmpty();
        LaunchdOriginRules.Parse(null).Should().BeEmpty();
    }

    [Fact]
    public void Parse_直接碰plist檔_刪除_crontab()
    {
        LaunchdOriginRules.Parse("Write\n/Users/u/Library/LaunchAgents/com.example.job.plist").Should().Equal(Hit(ScheduleVerb.Change, "com.example.job"));
        LaunchdOriginRules.Parse("cat > ~/Library/LaunchAgents/com.example.job.plist <<'EOF'").Should().Equal(Hit(ScheduleVerb.Change, "com.example.job"));
        LaunchdOriginRules.Parse("sudo cp job.plist /Library/LaunchDaemons/com.example.daemon.plist").Should().Equal(Hit(ScheduleVerb.Change, "com.example.daemon"));
        LaunchdOriginRules.Parse("rm -f $HOME/Library/LaunchAgents/com.example.job.plist").Should().Equal(Hit(ScheduleVerb.Delete, "com.example.job"));
        LaunchdOriginRules.Parse("launchctl bootstrap gui/501 ~/Library/LaunchAgents/a.plist; cp x ~/Library/LaunchAgents/b.plist")
            .Should().Equal(Hit(ScheduleVerb.Create, "a"), Hit(ScheduleVerb.Change, "b"));
        LaunchdOriginRules.Parse("ls /Users/u/Documents/x.plist").Should().BeEmpty("不在 LaunchAgents、LaunchDaemons 底下");

        LaunchdOriginRules.Parse("crontab -l").Should().BeEmpty("只是列出");
        LaunchdOriginRules.Parse("crontab -l | grep guard").Should().BeEmpty();
        LaunchdOriginRules.Parse("(crontab -l; echo '0 3 * * * /a/b.sh') | crontab -").Should().Equal(Hit(ScheduleVerb.Unknown, null));
        LaunchdOriginRules.Parse("crontab /tmp/new-crontab").Should().Equal(Hit(ScheduleVerb.Unknown, null));
        LaunchdOriginRules.Parse("crontab -r").Should().Equal(Hit(ScheduleVerb.Delete, null));
        LaunchdOriginRules.MayTouch("跑一下 crontab").Should().BeTrue();
        LaunchdOriginRules.MayTouch("schtasks /create").Should().BeFalse();
    }

    [Fact]
    public void NameMatches_標籤比檔名_路徑比結尾_Windows的工作照原本的規則()
    {
        const string path = "/Users/u/Library/LaunchAgents/com.example.job.plist";
        LaunchdOriginRules.NameMatches("com.example.job", path).Should().BeTrue();
        LaunchdOriginRules.NameMatches("COM.Example.Job", path).Should().BeTrue();
        LaunchdOriginRules.NameMatches("com.example", path).Should().BeFalse();
        LaunchdOriginRules.NameMatches(path, path).Should().BeTrue();
        LaunchdOriginRules.NameMatches("~/Library/LaunchAgents/com.example.job.plist", path).Should().BeTrue();
        LaunchdOriginRules.NameMatches("~/Library/LaunchAgents/other.plist", path).Should().BeFalse();
        LaunchdOriginRules.NameMatches(null, path).Should().BeFalse();
        LaunchdOriginRules.NameMatches("guard.sh", "crontab/0123456789").Should().BeFalse("crontab 的排程沒有名稱可比");
        LaunchdOriginRules.StemOf(path).Should().Be("com.example.job");

        // 共用的入口：不是反斜線開頭的路徑走 macOS 的規則，Windows 的工作不變
        ScheduleOriginRules.NameMatches("com.example.job", path).Should().BeTrue();
        ScheduleOriginRules.NameMatches("TkReportSync", @"\TkReportSync").Should().BeTrue();
        ScheduleOriginRules.NameMatches("TkReportSync", @"\Folder\TkReportSync").Should().BeTrue();
        ScheduleOriginRules.NameMatches("Other", @"\TkReportSync").Should().BeFalse();
    }
}

/// <summary>排程分類、結果說明、給代理的說明在 macOS 的樣子</summary>
public sealed class LaunchdClassifyRulesTests
{
    private const string Conversations = "/Users/u/Library/Application Support/Myriagent/conversations";

    private static readonly ScheduleEnvironment Env = new("u", "mac", new Dictionary<string, string> { ["HOME"] = "/Users/u", ["USER"] = "u" },
        new[] { Conversations }, Posix: true);

    private static ScheduleOrigin Classify(string command, string? arguments, string? workingDirectory = null, bool inRepo = false)
        => ScheduleClassifyRules.Classify(new ScheduleClassifyInput("/Users/u/Library/LaunchAgents/x.plist", null,
            new[] { new ScheduleAction("Exec", command, arguments, workingDirectory) }, inRepo, Env));

    [Fact]
    public void Classify_自己的腳本是疑似_廠商位置的是其他()
    {
        Classify("/bin/zsh", "/Users/u/work/run.zsh").Should().Be(ScheduleOrigin.Suspected, "殼層跑使用者資料夾裡的腳本");
        Classify("/Users/u/.claude-personal/bin/guard.sh", "8 4 >> /Users/u/.claude-personal/guard.log 2>&1").Should().Be(ScheduleOrigin.Suspected, "直接執行自己的腳本");
        Classify("/Users/u/bin/tool", null).Should().Be(ScheduleOrigin.Suspected, "使用者資料夾裡的程式");
        Classify("/bin/sh", "-c \"echo hi\"").Should().Be(ScheduleOrigin.Suspected, "一行指令、沒有路徑：plist 沒有作者，當成使用者自己的");
        Classify("/usr/bin/python3", "/opt/vendor/agent.py", inRepo: true).Should().Be(ScheduleOrigin.Suspected, "在 git repo 裡的一律算");
        Classify("/bin/zsh", "\"/Users/u/Library/Application Support/Myriagent/conversations/abc/run.sh\"").Should().Be(ScheduleOrigin.Suspected, "Myriagent 對話的工作區不是廠商的");

        Classify("/bin/zsh", "\"/Users/u/Library/Application Support/Guardian/cleanup.zsh\"").Should().Be(ScheduleOrigin.Other, "~/Library 是程式自己放東西的地方");
        Classify("/Applications/Foo.app/Contents/MacOS/foo", "--background").Should().Be(ScheduleOrigin.Other);
        Classify("/Library/PrivilegedHelperTools/com.docker.socket", "/Users/u/.docker/run/docker.sock /var/run/docker.sock").Should().Be(ScheduleOrigin.Other);
        Classify("/usr/local/bin/watchman", "--foreground").Should().Be(ScheduleOrigin.Other);
        ScheduleClassifyRules.Classify(new ScheduleClassifyInput("x", null, Array.Empty<ScheduleAction>(), false, Env)).Should().Be(ScheduleOrigin.Other);
    }

    [Fact]
    public void 路徑_參數裡的絕對路徑_波浪號與HOME_工作資料夾()
    {
        ScheduleClassifyRules.PathsIn("\"/Users/u/Library/Application Support/G/cleanup.zsh\" --quiet", Env)
            .Should().Equal("/Users/u/Library/Application Support/G/cleanup.zsh");
        ScheduleClassifyRules.PathsIn("-c \"cd ~/proj && ./run.sh >> $HOME/logs/x.log 2>/dev/null\"", Env)
            .Should().Equal("/Users/u/proj", "/Users/u/logs/x.log");
        ScheduleClassifyRules.PathsIn("8 4 >> /Users/u/guard.log 2>&1", Env).Should().Equal("/Users/u/guard.log");
        ScheduleClassifyRules.PathsIn("--config=/etc/tool.conf ~/data/", Env).Should().Equal("/etc/tool.conf", "/Users/u/data");
        ScheduleClassifyRules.PathsIn("relative/path ./x ../y -v", Env).Should().BeEmpty();
        ScheduleClassifyRules.PathsIn(null, Env).Should().BeEmpty();

        ScheduleClassifyRules.Expand("~/x", Env).Should().Be("/Users/u/x");
        ScheduleClassifyRules.Expand("${HOME}/x", Env).Should().Be("/Users/u/x");
        ScheduleClassifyRules.Expand("\"/a b/c\"", Env).Should().Be("/a b/c");
        ScheduleClassifyRules.Expand("$NOPE/x", Env).Should().BeNull("不知道在哪");
        ScheduleClassifyRules.PosixTokens("a \"b c\" 'd e' f\\ g \"\" \"say \\\"hi\\\"\"").Should().Equal("a", "b c", "d e", "f g", "", "say \"hi\"");

        var script = new[] { new ScheduleAction("Exec", "/bin/zsh", "/Users/u/work/run.zsh", null) };
        ScheduleClassifyRules.WorkFolder(script, Env).Should().Be("/Users/u/work");
        ScheduleClassifyRules.CandidatePaths(script, Env).Should().Equal("/bin/zsh", "/Users/u/work/run.zsh");
        ScheduleClassifyRules.WorkFolder(new[] { new ScheduleAction("Exec", "/bin/zsh", "/Users/u/work/run.zsh", "/Users/u/proj/") }, Env).Should().Be("/Users/u/proj");
        ScheduleClassifyRules.WorkFolder(new[] { new ScheduleAction("Exec", "/Users/u/bin/tool", null, null) }, Env).Should().Be("/Users/u/bin");
        ScheduleClassifyRules.WorkFolder(new[] { new ScheduleAction("Exec", "/Applications/Foo.app/Contents/MacOS/foo", null, null) }, Env).Should().BeNull();

        ScheduleClassifyRules.IsVendorPath("/Users/u/Library/Logs/x.log", Env).Should().BeTrue();
        ScheduleClassifyRules.IsVendorPath("/users/U/library", Env).Should().BeTrue();
        ScheduleClassifyRules.IsVendorPath(Conversations + "/abc/run.sh", Env).Should().BeFalse();
        ScheduleClassifyRules.IsVendorPath("/Users/u/work/x", Env).Should().BeFalse();
        ScheduleClassifyRules.IsVendorPath("/Applications", Env).Should().BeTrue();
    }

    [Fact]
    public void Windows的判斷不變()
    {
        var windows = new ScheduleEnvironment("U", "PC", new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase)
        {
            ["ProgramFiles"] = @"C:\Program Files", ["LOCALAPPDATA"] = @"C:\Users\U\AppData\Local", ["USERPROFILE"] = @"C:\Users\U",
        });
        windows.Posix.Should().BeFalse();
        windows.VendorRoots.Should().Equal(@"c:\program files\", @"c:\users\u\appdata\local\");
        ScheduleClassifyRules.PathsIn(@"-File C:\Repo\tools\sync.ps1 -Verbose", windows).Should().Equal(@"C:\Repo\tools\sync.ps1");
        ScheduleClassifyRules.Expand(@"%USERPROFILE%\x", windows).Should().Be(@"C:\Users\U\x");
        ScheduleClassifyRules.IsVendorPath(@"C:\Program Files\App\a.exe", windows).Should().BeTrue();
        ScheduleClassifyRules.WorkFolder(new[] { new ScheduleAction("Exec", "powershell.exe", @"-File C:\Repo\tools\sync.ps1", null) }, windows).Should().Be(@"C:\Repo\tools");
        Env.VendorRoots.Should().Contain("/users/u/library/").And.Contain("/applications/");
    }

    [Fact]
    public void 結果說明_macOS的結束碼與訊號()
    {
        ScheduleResultRules.Classify(0, posix: true).Should().Be(new ScheduleResult(0, ScheduleResultLevel.Ok, "成功"));
        ScheduleResultRules.Classify(1, posix: true).Should().Be(new ScheduleResult(1, ScheduleResultLevel.Fail, "結束碼 1"));
        ScheduleResultRules.Classify(78, posix: true).Level.Should().Be(ScheduleResultLevel.Fail);
        ScheduleResultRules.Classify(78, posix: true).Text.Should().StartWith("launchd 啟動不了");
        ScheduleResultRules.Classify(127, posix: true).Text.Should().Be("找不到要執行的指令");
        ScheduleResultRules.Classify(-15, posix: true).Should().Be(new ScheduleResult(-15, ScheduleResultLevel.Warn, "被停止（SIGTERM）"));
        ScheduleResultRules.Classify(-9, posix: true).Level.Should().Be(ScheduleResultLevel.Warn, "有人叫它停，不是它自己壞了");
        ScheduleResultRules.Classify(-11, posix: true).Level.Should().Be(ScheduleResultLevel.Fail, "當掉");
        ScheduleResultRules.Classify(-31, posix: true).Should().Be(new ScheduleResult(-31, ScheduleResultLevel.Fail, "被訊號 31 結束"));
        // 讀取器拿來當記號的兩個碼
        ScheduleResultRules.Classify(ScheduleResultRules.Running, posix: true).Should().Be(new ScheduleResult(ScheduleResultRules.Running, ScheduleResultLevel.Info, "執行中"));
        ScheduleResultRules.Classify(ScheduleResultRules.HasNotRun, posix: true).Text.Should().Be("還沒執行過");
        ScheduleResultRules.IsFinal(-15).Should().BeTrue();
        ScheduleResultRules.IsFinal(ScheduleResultRules.Running).Should().BeFalse();

        // Windows 的說明不變
        ScheduleResultRules.Classify(78, posix: false).Text.Should().Be("結束碼 78");
        ScheduleResultRules.Classify(unchecked((int)0x80070002), posix: false).Text.Should().Be("找不到要執行的檔案");
        ScheduleResultRules.Classify(unchecked((int)0x80070002), posix: true).Level.Should().Be(ScheduleResultLevel.Fail);
        ScheduleResultRules.Classify(unchecked((int)0x80070002), posix: true).Text.Should().NotBe("找不到要執行的檔案", "Windows 的碼在 macOS 沒有意義");
        ScheduleResultRules.CodeText(3, posix: true).Should().Be("結束碼 3");
        ScheduleResultRules.CodeText(-6, posix: true).Should().Be("被訊號 6 結束");
        ScheduleResultRules.CodeText(unchecked((int)0xC0000005), posix: false).Should().Be("結束碼 0xC0000005");
    }

    [Fact]
    public void 排程的名稱_macOS用標籤_Windows照路徑的最後一段()
    {
        var mac = new ScheduledTaskInfo("/Users/u/Library/LaunchAgents/com.example.job.plist", null, null, null, ScheduledTaskState.Ready, true, null, 0, null, 0, null)
        {
            DisplayName = "com.example.job",
        };
        mac.Name.Should().Be("com.example.job");
        mac.Folder.Should().Be("/Users/u/Library/LaunchAgents");
        (mac with { DisplayName = null }).Name.Should().Be("com.example.job.plist");
        var cron = new ScheduledTaskInfo("crontab/0123456789", null, null, null, ScheduledTaskState.Ready, true, null, 0, null, 0, null) { DisplayName = "guard.sh 8 4" };
        cron.Name.Should().Be("guard.sh 8 4");
        cron.Folder.Should().Be("crontab");

        var windows = new ScheduledTaskInfo(@"\Folder\TkReportSync", null, null, null, ScheduledTaskState.Ready, true, null, 0, null, 0, null);
        windows.Name.Should().Be("TkReportSync");
        windows.Folder.Should().Be(@"\Folder");
        new ScheduledTaskInfo(@"\TkReportSync", null, null, null, ScheduledTaskState.Ready, true, null, 0, null, 0, null).Folder.Should().Be(@"\");
    }

    [Fact]
    public void 給代理的說明_macOS講launchd與crontab_Windows講schtasks()
    {
        var job = LaunchdPlistRules.ToJob(LaunchdPlistRulesTests.GuardianPlist, LaunchdDomain.UserAgent, "u", null, null);
        var task = new ScheduledTaskInfo("/Users/u/Library/LaunchAgents/com.example.disk-space-guardian.plist", LaunchdPlistRulesTests.GuardianPlist, job.Definition, null,
            ScheduledTaskState.Ready, true, null, ScheduleResultRules.HasNotRun, null, 0, null) { DisplayName = job.Label };
        var brief = new ScheduleBrief(task, "Kevin 的 iMac", ScheduleOrigin.Adopted, null, Array.Empty<ScheduleRunRecord>(), Array.Empty<ScheduleChangeEntry>(), null);

        var mac = ScheduleAgentPromptRules.Build("explain", brief, launchd: true);
        mac.Should().Contain("（Kevin 的 iMac）的 launchd 排程").And.Contain("完整定義（launchd 的 plist）").And.Contain("launchctl kickstart")
            .And.Contain("`/Users/u/Library/LaunchAgents/com.example.disk-space-guardian.plist`").And.NotContain("schtasks").And.NotContain("UAC");
        var windows = ScheduleAgentPromptRules.Build("explain", brief, launchd: false);
        windows.Should().Contain("的 Windows 排程").And.Contain("schtasks").And.Contain("工作排程器的 XML").And.NotContain("launchctl");

        var entry = CronRules.Parse("*/15 * * * * /Users/u/bin/guard.sh 8 4\n", "u").Single();
        var cronTask = new ScheduledTaskInfo("crontab/" + entry.Key, entry.Line, entry.Definition, null, ScheduledTaskState.Ready, true, null, ScheduleResultRules.HasNotRun, null, 0, null);
        ScheduleAgentPromptRules.IsCron(cronTask).Should().BeTrue();
        ScheduleAgentPromptRules.IsCron(task).Should().BeFalse();
        var cron = ScheduleAgentPromptRules.Build("retime", brief with { Task = cronTask }, launchd: true);
        cron.Should().Contain("的 crontab 排程").And.Contain("完整定義（crontab 的這一行）").And.Contain("*/15 * * * * /Users/u/bin/guard.sh 8 4")
            .And.Contain("crontab -l").And.NotContain("launchctl kickstart");

        AgentHub.ScheduleCreatePrompt("Kevin 的 iMac", launchd: true).Should().Contain("~/Library/LaunchAgents/").And.Contain("launchctl bootstrap").And.NotContain("schtasks");
        AgentHub.ScheduleCreatePrompt("PC", launchd: false).Should().Contain("Windows 排程").And.Contain("schtasks").And.NotContain("launchctl");
    }
}
