using Myriagent.Agents;

namespace Myriagent.Accounts;

/// <summary>
/// macOS 上 Antigravity 的隔離帳戶（Windows 版沒有：那邊的登入存在認證管理員的固定名稱）。agy 的登入存在系統鑰匙圈的固定名稱，
/// 設定在 ~/.gemini/antigravity-cli——兩者都跟著 HOME 走（鑰匙圈預設是 $HOME/Library/Keychains/login.keychain-db）。
/// 所以每個隔離帳戶給 agy 一個自己的「家」（帳戶資料夾的 agy-home）：裡面有自己的鑰匙圈與 .gemini/antigravity-cli，
/// 其他東西全部連結回真正的家（.gitconfig、.ssh、~/.agents 的 skills、~/.gemini 其他設定都照舊）。
/// agy 開的 shell 由 ZDOTDIR／BASH_ENV 指到的小腳本把 HOME 換回真正的家，代理跑的指令（gh、git 走鑰匙圈的登入…）和平常一樣。
/// 這裡只有規則（要連結什麼、環境變數、腳本內容）；建立資料夾、連結、鑰匙圈的是平台的 AntigravityHome
/// </summary>
internal static class AntigravityHomeRules
{
    /// <summary>帳戶資料夾裡 agy 的「家」（AccountManager 的隔離資料夾名稱）</summary>
    public const string FolderName = "agy-home";

    /// <summary>家裡放 shell 小腳本的資料夾（ZDOTDIR 指到這裡）</summary>
    public const string ShellFolder = ".myriagent-shell";

    /// <summary>bash 的小腳本（BASH_ENV 指到它）</summary>
    public const string BashEnvFile = "bashenv";

    /// <summary>使用者原本的 ZDOTDIR／BASH_ENV（有設才帶）：小腳本換回來</summary>
    public const string OriginalZdotdirVariable = "MYRIAGENT_ORIGINAL_ZDOTDIR";
    public const string OriginalBashEnvVariable = "MYRIAGENT_ORIGINAL_BASH_ENV";

    /// <summary>
    /// 要連結回真正的家的資料夾層級（相對路徑）與那一層自己保留的項目：家的最上層留 Library、.gemini 與小腳本資料夾；
    /// Library 留 Keychains（自己的鑰匙圈）；.gemini 留 antigravity-cli（自己的設定、對話）。其他項目一律是連結
    /// </summary>
    public static readonly IReadOnlyList<(string Folder, IReadOnlyList<string> Own)> MirroredFolders =
    [
        ("", ["Library", ".gemini", ShellFolder]),
        ("Library", ["Keychains"]),
        (".gemini", ["antigravity-cli"]),
    ];

    /// <summary>antigravity-cli 裡和真正的家共用的項目（有才連結）：設定、Skills、外掛、工作流程、全域指示、MCP。登入、對話、記憶各自一份</summary>
    public static readonly IReadOnlyList<string> SharedConfigEntries =
        ["settings.json", "skills", "plugins", "workflows", "agents", "GEMINI.md", "mcp_config.json"];

    /// <summary>這個帳戶的 agy 用自己的家（macOS 的隔離帳戶；windows＝null 時看實際的作業系統）</summary>
    public static bool IsShadowed(AgentAccount account, bool? windows = null)
        => account.Provider == AgentProvider.Antigravity && !account.IsSystemDefault && !account.ExistingFolder
           && !(windows ?? OperatingSystem.IsWindows());

    /// <summary>家裡 agy 的設定資料夾（.gemini/antigravity-cli）</summary>
    public static string ConfigDirectory(string home) => AntigravityStreamRules.ResolveHome(home);

    /// <summary>家裡自己的鑰匙圈</summary>
    public static string KeychainPath(string home) => Path.Combine(home, "Library", "Keychains", "login.keychain-db");

    /// <summary>
    /// 家裡該有的連結（連結路徑 → 真正的家裡的目標）：list 列出真正的家某一層的項目名稱（列不出來回空的）。
    /// 每一層自己保留的項目不連結；antigravity-cli 只連結 SharedConfigEntries 裡真的存在的
    /// </summary>
    public static IReadOnlyList<(string Link, string Target)> PlanLinks(string home, string realHome, Func<string, IEnumerable<string>> list)
    {
        var links = new List<(string, string)>();
        foreach (var (folder, own) in MirroredFolders)
        {
            var source = folder.Length == 0 ? realHome : Path.Combine(realHome, folder);
            var target = folder.Length == 0 ? home : Path.Combine(home, folder);
            foreach (var name in list(source).Where(n => !own.Contains(n, StringComparer.Ordinal)).Order(StringComparer.Ordinal))
                links.Add((Path.Combine(target, name), Path.Combine(source, name)));
        }
        var realConfig = ConfigDirectory(realHome);
        var present = list(realConfig).ToHashSet(StringComparer.Ordinal);
        foreach (var name in SharedConfigEntries.Where(present.Contains))
            links.Add((Path.Combine(ConfigDirectory(home), name), Path.Combine(realConfig, name)));
        return links;
    }

    /// <summary>
    /// agy 程序的環境：HOME 換成它的家，shell 的小腳本（zsh 讀 ZDOTDIR 的 .zshenv、非互動的 bash 讀 BASH_ENV）把 HOME 換回來；
    /// 使用者原本設的 ZDOTDIR／BASH_ENV 另外帶著，小腳本換回去
    /// </summary>
    public static void ApplyEnvironment(IDictionary<string, string?> env, string home)
    {
        var shell = Path.Combine(home, ShellFolder);
        Keep(env, "ZDOTDIR", OriginalZdotdirVariable, shell);
        Keep(env, "BASH_ENV", OriginalBashEnvVariable, Path.Combine(shell, BashEnvFile));
        env["HOME"] = home;
        env["ZDOTDIR"] = shell;
        env["BASH_ENV"] = Path.Combine(shell, BashEnvFile);
    }

    private static void Keep(IDictionary<string, string?> env, string name, string original, string ours)
    {
        if (env.TryGetValue(name, out var value) && !string.IsNullOrEmpty(value) && value != ours)
            env[original] = value;
        else
            env.Remove(original);
    }

    /// <summary>zsh 的小腳本（家/.myriagent-shell/.zshenv）：換回真正的家、還原 ZDOTDIR／BASH_ENV，再讀使用者自己的 .zshenv</summary>
    public static string ZshEnvScript(string realHome) => Header(realHome) + """
        if [ -f "${ZDOTDIR:-$HOME}/.zshenv" ]; then . "${ZDOTDIR:-$HOME}/.zshenv"; fi

        """;

    /// <summary>bash 的小腳本（家/.myriagent-shell/bashenv）：換回真正的家、還原 ZDOTDIR／BASH_ENV，再讀使用者自己的 BASH_ENV</summary>
    public static string BashEnvScript(string realHome) => Header(realHome) + """
        if [ -n "${BASH_ENV:-}" ] && [ -f "$BASH_ENV" ]; then . "$BASH_ENV"; fi

        """;

    private static string Header(string realHome) =>
        "# Myriagent：這個 agy 帳戶的 HOME 是它自己的家（登入與設定各自一份）；agy 開的 shell 換回真正的家，指令照平常執行\n"
        + "export HOME=" + ShellQuote(realHome) + "\n"
        + Restore("ZDOTDIR", OriginalZdotdirVariable)
        + Restore("BASH_ENV", OriginalBashEnvVariable)
        + "unset " + OriginalZdotdirVariable + " " + OriginalBashEnvVariable + "\n";

    private static string Restore(string name, string original) =>
        "if [ -n \"${" + original + ":-}\" ]; then export " + name + "=\"$" + original + "\"; else unset " + name + "; fi\n";

    /// <summary>單引號包起來給 sh（裡面的單引號寫成 '\''）</summary>
    public static string ShellQuote(string value) => "'" + value.Replace("'", "'\\''") + "'";
}
