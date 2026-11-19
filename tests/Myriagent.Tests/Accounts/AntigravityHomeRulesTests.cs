using Myriagent.Accounts;
using Myriagent.Agents;

namespace Myriagent.Tests.Accounts;

/// <summary>macOS 的 agy 隔離帳戶：每個帳戶給 agy 一個自己的家（規則在 Windows 上也測得到：作業系統以參數指定）</summary>
public sealed class AntigravityHomeRulesTests
{
    private static readonly string Real = Path.Combine(Path.GetTempPath(), "agy-real-home");
    private static readonly string Home = Path.Combine(Path.GetTempPath(), "agy-accounts", "0123456789abcdef0123456789abcdef", AntigravityHomeRules.FolderName);

    private static AgentAccount Agy(string? folder = null, bool existing = false) => new()
    {
        Provider = AgentProvider.Antigravity,
        Label = "agy 2",
        ConfigDirectory = folder,
        ExistingFolder = existing,
    };

    [Fact]
    public void 只有macOS的agy隔離帳戶用自己的家()
    {
        AntigravityHomeRules.IsShadowed(Agy(Home), windows: false).Should().BeTrue();
        AntigravityHomeRules.IsShadowed(Agy(Home), windows: true).Should().BeFalse("Windows 的 agy 登入存在認證管理員，換 HOME 沒有用");
        AntigravityHomeRules.IsShadowed(Agy(), windows: false).Should().BeFalse("本機預設帳戶用真正的家");
        AntigravityHomeRules.IsShadowed(Agy(Home, existing: true), windows: false).Should().BeFalse();
        AntigravityHomeRules.IsShadowed(new AgentAccount { Provider = AgentProvider.Claude, Label = "c", ConfigDirectory = Home }, windows: false).Should().BeFalse();
    }

    [Fact]
    public void macOS可以新增agy隔離帳戶_Windows不行_都不能用現有的資料夾()
    {
        AccountRules.SupportsIsolatedAccounts(AgentProvider.Antigravity, windows: false).Should().BeTrue();
        AccountRules.SupportsIsolatedAccounts(AgentProvider.Antigravity, windows: true).Should().BeFalse();
        AccountRules.SupportsIsolatedAccounts(AgentProvider.Claude, windows: true).Should().BeTrue();
        ExistingFolderRules.Supported(AgentProvider.Antigravity).Should().BeFalse("agy 的登入跟著整個家走，不是一個設定資料夾");
        ExistingFolderRules.Supported(AgentProvider.Claude).Should().BeTrue();
    }

    [Fact]
    public void 從資料夾還原帳戶_每種CLI都有顏色()
    {
        // macOS 的 agy 也會從 accounts\<id>\agy-home 還原：沒有顏色會丟例外，app 啟動失敗
        foreach (var provider in AgentProviders.All)
            AccountManager.RecoveredColor(provider).Should().StartWith("#");
        AccountManager.RecoveredColor(AgentProvider.Antigravity).Should().NotBe(AccountRules.SystemDefaultAppearance(AgentProvider.Antigravity).Color, "和本機預設帳戶區分");
    }

    [Fact]
    public void 設定資料夾與鑰匙圈在家裡()
    {
        AntigravityHomeRules.ConfigDirectory(Home).Should().Be(Path.Combine(Home, ".gemini", "antigravity-cli"));
        AntigravityHomeRules.KeychainPath(Home).Should().Be(Path.Combine(Home, "Library", "Keychains", "login.keychain-db"));
    }

    [Fact]
    public void 連結_每一層自己保留的不連結_agy設定只共用指定項目()
    {
        var listing = new Dictionary<string, string[]>
        {
            [Real] = ["Documents", ".gitconfig", ".ssh", "Library", ".gemini", AntigravityHomeRules.ShellFolder],
            [Path.Combine(Real, "Library")] = ["Keychains", "Preferences", "Application Support"],
            [Path.Combine(Real, ".gemini")] = ["antigravity-cli", "config", "GEMINI.md"],
            [Path.Combine(Real, ".gemini", "antigravity-cli")] = ["settings.json", "conversations", "skills", "brain"],
        };
        var links = AntigravityHomeRules.PlanLinks(Home, Real, dir => listing.TryGetValue(dir, out var names) ? names : []);

        links.Should().BeEquivalentTo(new[]
        {
            (Path.Combine(Home, ".gitconfig"), Path.Combine(Real, ".gitconfig")),
            (Path.Combine(Home, ".ssh"), Path.Combine(Real, ".ssh")),
            (Path.Combine(Home, "Documents"), Path.Combine(Real, "Documents")),
            (Path.Combine(Home, "Library", "Application Support"), Path.Combine(Real, "Library", "Application Support")),
            (Path.Combine(Home, "Library", "Preferences"), Path.Combine(Real, "Library", "Preferences")),
            (Path.Combine(Home, ".gemini", "GEMINI.md"), Path.Combine(Real, ".gemini", "GEMINI.md")),
            (Path.Combine(Home, ".gemini", "config"), Path.Combine(Real, ".gemini", "config")),
            (Path.Combine(Home, ".gemini", "antigravity-cli", "settings.json"), Path.Combine(Real, ".gemini", "antigravity-cli", "settings.json")),
            (Path.Combine(Home, ".gemini", "antigravity-cli", "skills"), Path.Combine(Real, ".gemini", "antigravity-cli", "skills")),
        });
        links.Select(l => l.Link).Should().NotContain(new[]
        {
            Path.Combine(Home, "Library"), Path.Combine(Home, ".gemini"), Path.Combine(Home, AntigravityHomeRules.ShellFolder),
            Path.Combine(Home, "Library", "Keychains"), Path.Combine(Home, ".gemini", "antigravity-cli"),
        }, "鑰匙圈、agy 設定是這個帳戶自己的，連到真正的家就又共用同一個登入了");
        links.Select(l => l.Link).Should().NotContain(Path.Combine(Home, ".gemini", "antigravity-cli", "conversations"), "對話紀錄各自一份");
    }

    [Fact]
    public void 真正的家列不出來_只是沒有連結()
        => AntigravityHomeRules.PlanLinks(Home, Real, _ => []).Should().BeEmpty();

    [Fact]
    public void 環境_HOME換成家_shell小腳本換回來_原本的ZDOTDIR另外帶著()
    {
        var shell = Path.Combine(Home, AntigravityHomeRules.ShellFolder);
        var env = new Dictionary<string, string?> { ["HOME"] = Real, ["ZDOTDIR"] = "/custom/zsh", ["PATH"] = "/usr/bin" };
        AntigravityHomeRules.ApplyEnvironment(env, Home);

        env["HOME"].Should().Be(Home);
        env["ZDOTDIR"].Should().Be(shell);
        env["BASH_ENV"].Should().Be(Path.Combine(shell, AntigravityHomeRules.BashEnvFile));
        env[AntigravityHomeRules.OriginalZdotdirVariable].Should().Be("/custom/zsh");
        env.Should().NotContainKey(AntigravityHomeRules.OriginalBashEnvVariable, "原本沒有 BASH_ENV");
        env["PATH"].Should().Be("/usr/bin");

        AntigravityHomeRules.ApplyEnvironment(env, Home);
        env[AntigravityHomeRules.OriginalZdotdirVariable].Should().Be("/custom/zsh", "再套一次不能把自己的小腳本記成使用者原本的");
        env.Should().NotContainKey(AntigravityHomeRules.OriginalBashEnvVariable);
    }

    [Fact]
    public void 環境_原本沒有ZDOTDIR_不帶原值()
    {
        var env = new Dictionary<string, string?> { ["HOME"] = Real, ["BASH_ENV"] = "/my/bashenv", [AntigravityHomeRules.OriginalZdotdirVariable] = "/stale" };
        AntigravityHomeRules.ApplyEnvironment(env, Home);
        env.Should().NotContainKey(AntigravityHomeRules.OriginalZdotdirVariable, "繼承來的舊值不能留著");
        env[AntigravityHomeRules.OriginalBashEnvVariable].Should().Be("/my/bashenv");
    }

    [Fact]
    public void 小腳本_換回真正的家_還原變數_再讀使用者自己的設定()
    {
        var real = "/Users/o'neil";
        var zsh = AntigravityHomeRules.ZshEnvScript(real);
        zsh.Should().Contain("export HOME='/Users/o'\\''neil'\n");
        zsh.Should().Contain("if [ -n \"${MYRIAGENT_ORIGINAL_ZDOTDIR:-}\" ]; then export ZDOTDIR=\"$MYRIAGENT_ORIGINAL_ZDOTDIR\"; else unset ZDOTDIR; fi\n");
        zsh.Should().Contain("if [ -n \"${MYRIAGENT_ORIGINAL_BASH_ENV:-}\" ]; then export BASH_ENV=\"$MYRIAGENT_ORIGINAL_BASH_ENV\"; else unset BASH_ENV; fi\n");
        zsh.Should().Contain("unset MYRIAGENT_ORIGINAL_ZDOTDIR MYRIAGENT_ORIGINAL_BASH_ENV\n");
        zsh.Should().EndWith("if [ -f \"${ZDOTDIR:-$HOME}/.zshenv\" ]; then . \"${ZDOTDIR:-$HOME}/.zshenv\"; fi\n");
        zsh.IndexOf("export HOME", StringComparison.Ordinal).Should().BeLessThan(zsh.IndexOf(".zshenv\"; fi", StringComparison.Ordinal), "先換回家才讀使用者的 .zshenv");

        var bash = AntigravityHomeRules.BashEnvScript(real);
        bash.Should().StartWith(zsh[..zsh.IndexOf("if [ -f", StringComparison.Ordinal)]);
        bash.Should().EndWith("if [ -n \"${BASH_ENV:-}\" ] && [ -f \"$BASH_ENV\" ]; then . \"$BASH_ENV\"; fi\n");
        bash.Should().NotContain("\r");
    }
}
