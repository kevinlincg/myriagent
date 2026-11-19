using Myriagent.Cli;

namespace Myriagent.Tests.Cli;

/// <summary>macOS 在「終端機」開視窗執行（登入、登出）的兩個小腳本</summary>
public sealed class TerminalConsoleRulesTests
{
    [Fact]
    public void 單引號包起來()
    {
        TerminalConsoleRules.Quote("a b").Should().Be("'a b'");
        TerminalConsoleRules.Quote("it's").Should().Be("'it'\\''s'");
        TerminalConsoleRules.Quote("").Should().Be("''");
    }

    [Fact]
    public void 變數名稱()
    {
        TerminalConsoleRules.IsVariableName("CLAUDE_CONFIG_DIR").Should().BeTrue();
        TerminalConsoleRules.IsVariableName("_x1").Should().BeTrue();
        TerminalConsoleRules.IsVariableName("1X").Should().BeFalse();
        TerminalConsoleRules.IsVariableName("A-B").Should().BeFalse();
        TerminalConsoleRules.IsVariableName("").Should().BeFalse();
        TerminalConsoleRules.IsVariableName("名稱").Should().BeFalse();
    }

    [Fact]
    public void 環境差異_只寫改了的與拿掉的()
    {
        var before = new Dictionary<string, string?> { ["A"] = "1", ["B"] = "2", ["C"] = "3", ["bad-name"] = "x", ["E"] = null };
        var after = new Dictionary<string, string?> { ["A"] = "1", ["B"] = "9", ["D"] = "4", ["1X"] = "5", ["E"] = null, ["F"] = null };
        var (set, unset) = TerminalConsoleRules.Changes(before, after);
        set.Should().Equal(new KeyValuePair<string, string>("B", "9"), new KeyValuePair<string, string>("D", "4"));
        unset.Should().Equal("C");
    }

    [Fact]
    public void 環境差異_API_Key一律拿掉_除非設定之後有值()
    {
        var (set, unset) = TerminalConsoleRules.Changes(
            new Dictionary<string, string?> { ["PATH"] = "/bin" },
            new Dictionary<string, string?> { ["PATH"] = "/bin", ["XAI_API_KEY"] = "k" },
            ["ANTHROPIC_API_KEY", "GEMINI_API_KEY", "XAI_API_KEY", "bad-name"]);
        unset.Should().Equal("ANTHROPIC_API_KEY", "GEMINI_API_KEY");
        set.Should().Equal(new KeyValuePair<string, string>("XAI_API_KEY", "k"));
        Myriagent.Accounts.AccountRules.CredentialVariables.Should().Contain(new[] { "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "XAI_API_KEY" });
    }

    [Fact]
    public void 環境差異_值設成null也算拿掉()
    {
        var (set, unset) = TerminalConsoleRules.Changes(
            new Dictionary<string, string?> { ["CLAUDECODE"] = "1" }, new Dictionary<string, string?> { ["CLAUDECODE"] = null });
        set.Should().BeEmpty();
        unset.Should().Equal("CLAUDECODE");
    }

    [Fact]
    public void 終端機裡的腳本_設環境_執行_寫結束碼_盯著等待的程序()
    {
        var script = TerminalConsoleRules.RunScript("/tmp/c 1", "Antigravity 登入", "/Users/k/.local/bin/agy", ["--x", "a'b"],
            [new("HOME", "/h/agy home"), new("ZDOTDIR", "/h/s")], ["CLAUDECODE"], "/work dir");
        script.Should().StartWith("#!/bin/sh\n");
        script.Should().Contain("D='/tmp/c 1'\n");
        script.Should().Contain("echo $$ > \"$D/run.pid\"\n");
        script.Should().Contain("printf '\\033]0;%s\\007' 'Antigravity 登入'\n");
        script.Should().Contain("( while kill -0 \"$W\" 2>/dev/null; do sleep 1; done; [ -f \"$D/done\" ] || { rm -rf \"$D\"; kill -TERM 0; } ) &\n");
        script.Should().Contain("export HOME='/h/agy home'\nexport ZDOTDIR='/h/s'\nunset CLAUDECODE\n");
        script.Should().Contain("cd '/work dir' 2>/dev/null\n'/Users/k/.local/bin/agy' '--x' 'a'\\''b'\nrc=$?\n");
        script.Should().Contain("echo \"$rc\" > \"$D/done.tmp\" && mv -f \"$D/done.tmp\" \"$D/done\"\n");
        script.Should().EndWith("exit \"$rc\"\n");
        script.IndexOf("export HOME", StringComparison.Ordinal).Should().BeLessThan(script.IndexOf("'/Users/k/.local/bin/agy'", StringComparison.Ordinal));
        script.IndexOf("( while kill", StringComparison.Ordinal).Should().BeLessThan(script.IndexOf("export HOME", StringComparison.Ordinal),
            "背景盯著的子程序要用 app 這邊的環境開始（之後的 export 不影響它）");
        script.Should().NotContain("\r");
    }

    [Fact]
    public void 終端機裡的腳本_沒有標題就不設()
        => TerminalConsoleRules.RunScript("/tmp/c", null, "/bin/echo", [], [], [], "/").Should().NotContain("printf");

    [Fact]
    public void 等待的腳本_開終端機_等結束碼_視窗被關或開不起來回1()
    {
        var script = TerminalConsoleRules.WatchScript("/tmp/c 1");
        script.Should().Contain("D='/tmp/c 1'\n");
        script.Should().Contain("echo $$ > \"$D/watcher.pid\"\n");
        script.Should().Contain("/usr/bin/open -a Terminal \"$D/run.command\" || { rm -rf \"$D\"; exit 127; }\n");
        script.Should().Contain("rc=$(cat \"$D/done\"); rm -rf \"$D\"; exit \"${rc:-1}\"");
        script.Should().Contain("if [ -n \"$p\" ] && ! kill -0 \"$p\" 2>/dev/null; then\n");
        script.Should().Contain("if [ \"$n\" -gt 300 ]; then rm -rf \"$D\"; exit 1; fi", "60 秒（每 0.2 秒一次）終端機都沒開起來");
        script.IndexOf("echo $$", StringComparison.Ordinal).Should().BeLessThan(script.IndexOf("/usr/bin/open", StringComparison.Ordinal),
            "終端機裡的腳本一開始就要讀到等待程序的 PID");
    }
}
