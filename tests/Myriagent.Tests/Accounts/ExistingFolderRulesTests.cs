using Myriagent.Accounts;
using Myriagent.Agents;

namespace Myriagent.Tests.Accounts;

/// <summary>「使用現有的設定資料夾」的純規則（路徑都以這台的暫存資料夾組出來：Windows 與 macOS 的完整路徑寫法不同）</summary>
public sealed class ExistingFolderRulesTests
{
    private static readonly string Home = Path.Combine(Path.GetTempPath(), "existing-folder-home");

    private static string InHome(string name) => Path.Combine(Home, name);

    [Fact]
    public void Clean_去空白與引號_波浪號換成使用者資料夾_去掉結尾的分隔符號()
    {
        ExistingFolderRules.Clean(null, Home).Should().BeNull();
        ExistingFolderRules.Clean("   ", Home).Should().BeNull();
        ExistingFolderRules.Clean("\"\"", Home).Should().BeNull();
        ExistingFolderRules.Clean("  \"" + InHome(".claude-work") + "\"  ", Home).Should().Be(InHome(".claude-work"));
        ExistingFolderRules.Clean("~", Home).Should().Be(Home);
        ExistingFolderRules.Clean("~/.claude-work", Home).Should().Be(InHome(".claude-work"));
        ExistingFolderRules.Clean("~\\.claude-work", Home).Should().Be(InHome(".claude-work"));
        ExistingFolderRules.Clean(InHome(".claude-work") + "/", Home).Should().Be(InHome(".claude-work"));
        ExistingFolderRules.Clean(InHome(".claude-work") + "\\\\", Home).Should().Be(InHome(".claude-work"));
        ExistingFolderRules.Clean("/", Home).Should().Be("/", "整個就是根目錄的不去");
        ExistingFolderRules.Clean("C:\\", Home).Should().Be("C:\\");
        ExistingFolderRules.Clean("~x/.claude", Home).Should().Be("~x/.claude", "只認自己的使用者資料夾");
        ExistingFolderRules.Clean("~//.claude-work", Home).Should().Be(InHome(".claude-work"), "~ 後面多打的分隔符號不能讓它變成根目錄底下的路徑");
        ExistingFolderRules.Clean("~/", Home).Should().Be(Home);
        var sep = Path.DirectorySeparatorChar;
        ExistingFolderRules.Clean(Home + sep + sep + ".claude-work", Home).Should().Be(InHome(".claude-work"), "重複的分隔符號化掉：app 與它啟動的 CLI 要看到同一個字串");
        ExistingFolderRules.Clean(InHome("x") + sep + ".." + sep + ".claude-work", Home).Should().Be(InHome(".claude-work"));
        ExistingFolderRules.Clean(".claude-work", Home).Should().Be(".claude-work", "不是完整路徑的原樣回傳，由 Check 擋下");
    }

    [Fact]
    public void Check_依序擋下不支援_空白_不完整_不存在_預設資料夾_已在用()
    {
        var work = InHome(".claude-work");
        var defaults = new[] { InHome(".claude") };
        var used = new[] { InHome(".claude-alt") };
        var forbidden = new[] { Home, InHome(".codex") };
        ExistingFolderCheck Run(AgentProvider provider, string? directory, bool exists = true, bool looksLikeConfig = true)
            => ExistingFolderRules.Check(provider, directory, _ => exists, defaults, used, forbidden, _ => looksLikeConfig);

        Run(AgentProvider.Claude, work).Should().Be(ExistingFolderCheck.Ok);
        Run(AgentProvider.Codex, work).Should().Be(ExistingFolderCheck.Ok);
        Run(AgentProvider.Grok, work).Should().Be(ExistingFolderCheck.Ok);
        Run(AgentProvider.Antigravity, work).Should().Be(ExistingFolderCheck.Unsupported, "一台電腦只有一個登入");
        Run(AgentProvider.Claude, null).Should().Be(ExistingFolderCheck.Empty);
        Run(AgentProvider.Claude, "  ").Should().Be(ExistingFolderCheck.Empty);
        Run(AgentProvider.Claude, ".claude-work").Should().Be(ExistingFolderCheck.NotAbsolute);
        Run(AgentProvider.Claude, work, exists: false).Should().Be(ExistingFolderCheck.Missing);
        Run(AgentProvider.Claude, InHome(".claude")).Should().Be(ExistingFolderCheck.IsDefault);
        Run(AgentProvider.Claude, InHome(".CLAUDE") + Path.DirectorySeparatorChar).Should().Be(ExistingFolderCheck.IsDefault, "不分大小寫、不管結尾的分隔符號");
        Run(AgentProvider.Claude, InHome(".claude-alt")).Should().Be(ExistingFolderCheck.InUse);
        Run(AgentProvider.Claude, InHome(".Claude-Alt")).Should().Be(ExistingFolderCheck.InUse);
        Run(AgentProvider.Claude, InHome(".claude"), exists: false).Should().Be(ExistingFolderCheck.Missing, "不存在的先說不存在");
        Run(AgentProvider.Claude, Home).Should().Be(ExistingFolderCheck.Forbidden, "使用者資料夾本身");
        Run(AgentProvider.Claude, Home + Path.DirectorySeparatorChar).Should().Be(ExistingFolderCheck.Forbidden);
        Run(AgentProvider.Claude, InHome(".codex")).Should().Be(ExistingFolderCheck.Forbidden, "別種 CLI 的設定資料夾");
        Run(AgentProvider.Claude, Path.GetPathRoot(Home)).Should().Be(ExistingFolderCheck.Forbidden, "磁碟的根目錄");
        Run(AgentProvider.Claude, InHome("Documents"), looksLikeConfig: false).Should().Be(ExistingFolderCheck.NotConfigFolder, "裡面有別的東西卻沒有用過的痕跡");
        Run(AgentProvider.Claude, Home, looksLikeConfig: false).Should().Be(ExistingFolderCheck.Forbidden, "不該用的先說不該用");
        Run(AgentProvider.Claude, InHome(".claude-alt"), looksLikeConfig: false).Should().Be(ExistingFolderCheck.InUse);
    }

    [Fact]
    public void IsRoot_HasMarker()
    {
        ExistingFolderRules.IsRoot(Path.GetPathRoot(Home)!).Should().BeTrue();
        ExistingFolderRules.IsRoot(Home).Should().BeFalse();
        var entries = new HashSet<string> { Path.Combine(InHome(".claude-work"), "projects"), Path.Combine(InHome(".codex-work"), "auth.json") };
        ExistingFolderRules.HasMarker(AgentProvider.Claude, InHome(".claude-work"), entries.Contains).Should().BeTrue();
        ExistingFolderRules.HasMarker(AgentProvider.Claude, InHome(".codex-work"), entries.Contains).Should().BeFalse("auth.json 是 Codex、Grok 的");
        ExistingFolderRules.HasMarker(AgentProvider.Codex, InHome(".codex-work"), entries.Contains).Should().BeTrue();
        ExistingFolderRules.HasMarker(AgentProvider.Grok, InHome(".codex-work"), entries.Contains).Should().BeTrue();
        ExistingFolderRules.HasMarker(AgentProvider.Antigravity, InHome(".codex-work"), _ => true).Should().BeFalse();
    }

    [Fact]
    public void Describe_每一種結果都有給人看的說明_通過是空字串()
    {
        ExistingFolderRules.Describe(ExistingFolderCheck.Ok, AgentProvider.Claude, "x").Should().BeEmpty();
        foreach (var check in Enum.GetValues<ExistingFolderCheck>().Where(c => c != ExistingFolderCheck.Ok))
            ExistingFolderRules.Describe(check, AgentProvider.Claude, "x").Should().NotBeNullOrWhiteSpace();
        ExistingFolderRules.Describe(ExistingFolderCheck.Missing, AgentProvider.Codex, "/a/b").Should().Be("找不到這個資料夾：/a/b");
        ExistingFolderRules.Describe(ExistingFolderCheck.InUse, AgentProvider.Codex, "/a/b").Should().Be("已經有帳戶在用這個資料夾：/a/b");
        ExistingFolderRules.Describe(ExistingFolderCheck.IsDefault, AgentProvider.Codex, "/a/b").Should().Contain("Codex 預設的設定資料夾");
    }

    // AgentProvider 是 internal，不能當 public 測試方法的參數：以名稱傳
    [Theory]
    [InlineData("Claude", ".claude", true)]
    [InlineData("Claude", ".claude-work", true)]
    [InlineData("Claude", ".claude_alt", true)]
    [InlineData("Claude", ".claude.old", true)]
    [InlineData("Claude", ".Claude-Work", true)]
    [InlineData("Claude", ".claudex", false)]
    [InlineData("Claude", "claude-work", false)]
    [InlineData("Claude", ".codex-work", false)]
    [InlineData("Claude", "", false)]
    [InlineData("Claude", null, false)]
    [InlineData("Codex", ".codex-work", true)]
    [InlineData("Codex", ".claude-work", false)]
    [InlineData("Grok", ".grok-2", true)]
    [InlineData("Antigravity", ".gemini", false)]
    [InlineData("Antigravity", ".claude-work", false)]
    public void IsCandidateName_名稱要是設定資料夾的開頭再接分隔字元(string provider, string? name, bool expected)
        => ExistingFolderRules.IsCandidateName(Enum.Parse<AgentProvider>(provider), name).Should().Be(expected);

    [Fact]
    public void Find_只列名稱像_有用過的痕跡_不是預設也沒人在用的資料夾_依名稱排序()
    {
        var all = new[] { ".claude", ".claude-work", ".claude-alt", ".claude-empty", ".claude-used", ".claudex", ".codex-work", ".config" }.Select(InHome).ToList();
        var entries = new HashSet<string>(StringComparer.OrdinalIgnoreCase)
        {
            Path.Combine(InHome(".claude"), ".credentials.json"),
            Path.Combine(InHome(".claude-work"), "projects"),
            Path.Combine(InHome(".claude-alt"), ".claude.json"),
            Path.Combine(InHome(".claude-used"), ".claude.json"),
            Path.Combine(InHome(".claudex"), ".claude.json"),
            Path.Combine(InHome(".codex-work"), "auth.json"),
            Path.Combine(InHome(".claude-empty"), "settings.json"),
        };
        var defaults = new[] { InHome(".claude") };
        var used = new[] { InHome(".claude-used") + Path.DirectorySeparatorChar };

        ExistingFolderRules.Find(AgentProvider.Claude, all, entries.Contains, defaults, used)
            .Should().Equal(InHome(".claude-alt"), InHome(".claude-work"));
        ExistingFolderRules.Find(AgentProvider.Codex, all, entries.Contains, new[] { InHome(".codex") }, Array.Empty<string>())
            .Should().Equal(InHome(".codex-work"));
        ExistingFolderRules.Find(AgentProvider.Grok, all, entries.Contains, Array.Empty<string>(), Array.Empty<string>()).Should().BeEmpty();
        ExistingFolderRules.Find(AgentProvider.Antigravity, all, _ => true, Array.Empty<string>(), Array.Empty<string>()).Should().BeEmpty("沒有隔離帳戶");
        ExistingFolderRules.Find(AgentProvider.Claude, all, _ => false, defaults, used).Should().BeEmpty("裡面什麼都沒有的不算");
    }

    [Fact]
    public void DefaultLabel_用資料夾名稱後面那段_看不出來或撞名就照一般編號()
    {
        var none = Array.Empty<AgentAccount>();
        ExistingFolderRules.DefaultLabel(AgentProvider.Claude, InHome(".claude-work"), none).Should().Be("Claude work");
        ExistingFolderRules.DefaultLabel(AgentProvider.Claude, InHome(".claude-work") + Path.DirectorySeparatorChar, none).Should().Be("Claude work");
        ExistingFolderRules.DefaultLabel(AgentProvider.Codex, InHome(".codex_alt"), none).Should().Be("Codex alt");
        ExistingFolderRules.DefaultLabel(AgentProvider.Claude, InHome(".claude"), none).Should().Be(AccountRules.DefaultLabel(AgentProvider.Claude, none));
        ExistingFolderRules.DefaultLabel(AgentProvider.Claude, InHome("profiles"), none).Should().Be(AccountRules.DefaultLabel(AgentProvider.Claude, none));
        var taken = new[] { new AgentAccount { Provider = AgentProvider.Claude, Label = "claude WORK" } };
        ExistingFolderRules.DefaultLabel(AgentProvider.Claude, InHome(".claude-work"), taken).Should().Be(AccountRules.DefaultLabel(AgentProvider.Claude, taken), "同名的帳戶已經有了");
        ExistingFolderRules.DefaultLabel(AgentProvider.Claude, InHome(".claude-" + new string('a', 60)), none).Should().HaveLength(40);
    }

    [Fact]
    public void Display_使用者資料夾底下的寫成波浪號()
    {
        ExistingFolderRules.Display(InHome(".claude-work"), Home).Should().Be("~" + Path.DirectorySeparatorChar + ".claude-work");
        ExistingFolderRules.Display(InHome(".claude-work"), Home + Path.DirectorySeparatorChar).Should().Be("~" + Path.DirectorySeparatorChar + ".claude-work");
        ExistingFolderRules.Display(Home, Home).Should().Be(Home);
        ExistingFolderRules.Display(Home + "2" + Path.DirectorySeparatorChar + "x", Home).Should().Be(Home + "2" + Path.DirectorySeparatorChar + "x", "只是開頭相同的別的資料夾");
    }

    [Fact]
    public void BuiltInDirectory_各家CLI自己的預設位置()
    {
        ExistingFolderRules.BuiltInDirectory(AgentProvider.Claude, Home).Should().Be(InHome(".claude"));
        ExistingFolderRules.BuiltInDirectory(AgentProvider.Codex, Home).Should().Be(InHome(".codex"));
        ExistingFolderRules.BuiltInDirectory(AgentProvider.Grok, Home).Should().Be(InHome(".grok"));
    }

    [Fact]
    public void 現有資料夾的帳戶_已登入時不提供裝置碼登入_比照本機預設帳戶()
    {
        var existing = new AgentAccount { Provider = AgentProvider.Codex, ConfigDirectory = InHome(".codex-work"), ExistingFolder = true };
        var isolated = new AgentAccount { Provider = AgentProvider.Codex, ConfigDirectory = InHome("accounts") };
        CodexDeviceCodeRules.CanUseDeviceCode(isolated, AccountStatus.Ready).Should().BeTrue("對照：app 建的隔離帳戶隨時可以");
        CodexDeviceCodeRules.CanUseDeviceCode(existing, AccountStatus.Ready).Should().BeFalse("那份登入他在終端機也在用，重新登入會撤銷它");
        CodexDeviceCodeRules.CanUseDeviceCode(existing, AccountStatus.NotLoggedIn).Should().BeTrue("本來就沒登入的可以");
        CodexDeviceCodeRules.Refusal(existing, AccountStatus.Ready).Should().Contain("原有的設定資料夾");
        CodexDeviceCodeRules.Refusal(existing, AccountStatus.NotLoggedIn).Should().BeNull();
        var grok = new AgentAccount { Provider = AgentProvider.Grok, ConfigDirectory = InHome(".grok-work"), ExistingFolder = true };
        CodexDeviceCodeRules.CanUseDeviceCode(grok, AccountStatus.Ready).Should().BeFalse();
        CodexDeviceCodeRules.Refusal(grok, AccountStatus.Ready).Should().Contain("原有的設定資料夾");
    }

    [Fact]
    public void 現有資料夾的帳戶一律不共用預設帳戶的設定()
    {
        var account = new AgentAccount { Provider = AgentProvider.Claude, ConfigDirectory = InHome(".claude-work") };
        SharedCustomizationRules.IsActive(account).Should().BeTrue("一般的隔離帳戶預設共用");
        account.ExistingFolder = true;
        SharedCustomizationRules.IsActive(account).Should().BeFalse("使用者原有的資料夾不能把 skills 搬走換成連結");
    }
}
