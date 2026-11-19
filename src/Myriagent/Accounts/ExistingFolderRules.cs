using Myriagent.Agents;

namespace Myriagent.Accounts;

/// <summary>一個現有的設定資料夾能不能加成帳戶（ExistingFolderRules.Check）</summary>
internal enum ExistingFolderCheck
{
    Ok,

    /// <summary>這種 CLI 一台電腦只有一個登入（Antigravity），沒有「另一個設定資料夾」</summary>
    Unsupported,

    /// <summary>沒有輸入路徑</summary>
    Empty,

    /// <summary>不是完整的路徑</summary>
    NotAbsolute,

    /// <summary>資料夾不存在</summary>
    Missing,

    /// <summary>一看就不該拿來當設定資料夾的地方：磁碟的根目錄、使用者資料夾本身、app 自己的資料夾、別種 CLI 的設定資料夾</summary>
    Forbidden,

    /// <summary>裡面有東西、卻沒有這種 CLI 用過的痕跡（多半是路徑打錯了：CLI 之後會把對話紀錄與登入寫進去）</summary>
    NotConfigFolder,

    /// <summary>就是「本機預設」帳戶用的資料夾（CLI 自己的預設位置，或這次啟動繼承的 CLAUDE_CONFIG_DIR／CODEX_HOME／GROK_HOME）</summary>
    IsDefault,

    /// <summary>清單裡已經有帳戶在用它</summary>
    InUse,
}

/// <summary>
/// 「使用現有的設定資料夾」的純函式規則（AccountManager.AddExisting、新增帳戶對話框列出的資料夾）：
/// 使用者在終端機以 CLAUDE_CONFIG_DIR=~/.claude-work 之類的方式分開的帳戶，直接把那個資料夾加成帳戶——登入、Skills、外掛、記憶、
/// CLI 對話紀錄都是同一份，不必在 app 裡重新登入、也不會多出一份設定
/// </summary>
internal static class ExistingFolderRules
{
    /// <summary>
    /// 這種 CLI 自己預設的設定資料夾（沒有設 CLAUDE_CONFIG_DIR／CODEX_HOME／GROK_HOME 時用的那個）。它就是「本機預設」帳戶，不能另外加成帳戶：
    /// 指定了設定資料夾的 Claude Code 在 macOS 把登入存在另一個鑰匙圈項目（MacClaudeKeychain.ServiceFor），加進來會看成沒登入
    /// </summary>
    public static string BuiltInDirectory(AgentProvider provider, string userProfile) => provider switch
    {
        AgentProvider.Claude => Path.Combine(userProfile, ".claude"),
        AgentProvider.Codex => Path.Combine(userProfile, ".codex"),
        AgentProvider.Antigravity => AntigravityStreamRules.ResolveHome(userProfile),
        AgentProvider.Grok => GrokAcpRules.ResolveHome(userProfile),
        _ => throw new ArgumentOutOfRangeException(nameof(provider), provider, null),
    };

    /// <summary>
    /// 使用者輸入的資料夾路徑：去掉頭尾的空白與引號、開頭的 ~ 換成使用者資料夾、完整路徑寫成正規的樣子（重複的分隔符號、. 與 .. 都化掉；
    /// 不解開符號連結）、去掉結尾的分隔符號（整個就是根目錄的不去）。空白回 null；不是完整路徑的原樣回傳（<see cref="Check"/> 會擋下）。
    /// 寫成正規的樣子是為了讓 app 啟動的 CLI 與 app 自己讀登入（macOS 的鑰匙圈項目名稱由 CLAUDE_CONFIG_DIR 的字面算出，MacClaudeKeychain）
    /// 看到同一個字串。已知的限制：使用者在終端機把 CLAUDE_CONFIG_DIR 寫成別的樣子（例如結尾多一個 /）時，macOS 的 Claude Code 把登入存在
    /// 另一個鑰匙圈項目，加進來會顯示未登入，要在 app 裡再登入一次
    /// </summary>
    public static string? Clean(string? input, string userProfile)
    {
        var text = input?.Trim().Trim('"', '\'').Trim();
        if (string.IsNullOrEmpty(text))
            return null;
        if (text == "~")
            text = userProfile;
        else if (text.StartsWith("~/", StringComparison.Ordinal) || text.StartsWith("~\\", StringComparison.Ordinal))
        {
            // ~//x 的 x 不能變成根目錄底下的 /x（Path.Combine 遇到分隔符號開頭的第二段會丟掉第一段）
            var rest = text[2..].TrimStart('/', '\\');
            text = rest.Length == 0 ? userProfile : Path.Combine(userProfile, rest);
        }
        if (Path.IsPathFullyQualified(text))
        {
            try
            {
                text = Path.GetFullPath(text);
            }
            catch (Exception ex) when (ex is ArgumentException or NotSupportedException or PathTooLongException)
            {
                return text;
            }
        }
        var trimmed = text.TrimEnd('/', '\\');
        return trimmed.Length == 0 || trimmed.EndsWith(':') ? text : trimmed;
    }

    /// <summary>
    /// 這個資料夾（已經過 <see cref="Clean"/>）能不能加成帳戶。defaultDirectories＝「本機預設」帳戶用的資料夾（CLI 自己的預設位置與這次啟動實際用的）；
    /// usedDirectories＝清單裡同一種 CLI 的帳戶實際用的資料夾；forbiddenDirectories＝一看就不該用的地方（使用者資料夾本身、app 自己的資料夾、
    /// 別種 CLI 的設定資料夾；磁碟的根目錄這裡自己判斷）；looksLikeConfig＝資料夾裡有這種 CLI 用過的痕跡（<see cref="Markers"/>）或是空的
    /// （空的資料夾可以當成新的設定資料夾，之後在帳戶卡登入）
    /// </summary>
    public static ExistingFolderCheck Check(AgentProvider provider, string? directory, Func<string, bool> directoryExists,
        IEnumerable<string> defaultDirectories, IEnumerable<string> usedDirectories, IEnumerable<string> forbiddenDirectories, Func<string, bool> looksLikeConfig)
    {
        if (!AccountRules.SupportsIsolatedAccounts(provider))
            return ExistingFolderCheck.Unsupported;
        if (string.IsNullOrWhiteSpace(directory))
            return ExistingFolderCheck.Empty;
        if (!Path.IsPathFullyQualified(directory))
            return ExistingFolderCheck.NotAbsolute;
        if (!directoryExists(directory))
            return ExistingFolderCheck.Missing;
        if (defaultDirectories.Any(d => SameFolder(d, directory)))
            return ExistingFolderCheck.IsDefault;
        if (usedDirectories.Any(d => SameFolder(d, directory)))
            return ExistingFolderCheck.InUse;
        if (IsRoot(directory) || forbiddenDirectories.Any(d => SameFolder(d, directory)))
            return ExistingFolderCheck.Forbidden;
        if (!looksLikeConfig(directory))
            return ExistingFolderCheck.NotConfigFolder;
        return ExistingFolderCheck.Ok;
    }

    /// <summary>整個路徑就是磁碟的根目錄（/、C:\）</summary>
    public static bool IsRoot(string directory)
    {
        try
        {
            return SameFolder(Path.GetPathRoot(Path.GetFullPath(directory)), directory);
        }
        catch (Exception ex) when (ex is ArgumentException or NotSupportedException or PathTooLongException)
        {
            return false;
        }
    }

    /// <summary>
    /// 資料夾裡有沒有這種 CLI 用過的痕跡（<see cref="Markers"/> 的任何一個）。entryExists＝這個路徑的檔案或資料夾存不存在
    /// </summary>
    public static bool HasMarker(AgentProvider provider, string directory, Func<string, bool> entryExists)
        => Markers(provider).Any(m => entryExists(Path.Combine(directory, m)));

    /// <summary>檢查不過時給使用者看的說明（Ok 回空字串）</summary>
    public static string Describe(ExistingFolderCheck check, AgentProvider provider, string? directory)
    {
        var name = AccountRules.ProviderName(provider);
        return check switch
        {
            ExistingFolderCheck.Ok => string.Empty,
            ExistingFolderCheck.Unsupported => $"{name} 一台電腦只有一個登入，不能另外指定設定資料夾",
            ExistingFolderCheck.Empty => "請輸入設定資料夾的路徑",
            ExistingFolderCheck.NotAbsolute => $"請輸入完整的路徑：{directory}",
            ExistingFolderCheck.Missing => $"找不到這個資料夾：{directory}",
            ExistingFolderCheck.Forbidden => $"這個資料夾不能當成 {name} 的設定資料夾：{directory}",
            ExistingFolderCheck.NotConfigFolder => $"這個資料夾看起來不是 {name} 的設定資料夾（裡面有別的東西，卻沒有 {name} 用過的痕跡）：{directory}",
            ExistingFolderCheck.IsDefault => $"這是 {name} 預設的設定資料夾，「本機預設」帳戶用的就是它：{directory}",
            ExistingFolderCheck.InUse => $"已經有帳戶在用這個資料夾：{directory}",
            _ => throw new ArgumentOutOfRangeException(nameof(check), check, null),
        };
    }

    /// <summary>兩個路徑是不是同一個資料夾（不分大小寫、不管結尾的分隔符號；Windows 與 macOS 預設的檔案系統都不分大小寫）</summary>
    public static bool SameFolder(string? a, string? b)
    {
        if (string.IsNullOrWhiteSpace(a) || string.IsNullOrWhiteSpace(b))
            return false;
        try
        {
            return string.Equals(Path.TrimEndingDirectorySeparator(Path.GetFullPath(a)), Path.TrimEndingDirectorySeparator(Path.GetFullPath(b)),
                StringComparison.OrdinalIgnoreCase);
        }
        catch (Exception ex) when (ex is ArgumentException or NotSupportedException or PathTooLongException)
        {
            return false;
        }
    }

    /// <summary>設定資料夾名稱的開頭（.claude、.codex、.grok）；沒有隔離帳戶的 CLI 回 null</summary>
    private static string? NamePrefix(AgentProvider provider) => provider switch
    {
        AgentProvider.Claude => ".claude",
        AgentProvider.Codex => ".codex",
        AgentProvider.Grok => ".grok",
        AgentProvider.Antigravity => null,
        _ => throw new ArgumentOutOfRangeException(nameof(provider), provider, null),
    };

    /// <summary>
    /// 使用者資料夾底下這個名稱像不像這種 CLI 的設定資料夾：.claude 本身，或 .claude 後面接 -、_、. 的（.claude-work、.claude_alt）。
    /// .claudex 之類只是開頭相同的不算
    /// </summary>
    public static bool IsCandidateName(AgentProvider provider, string? name)
    {
        if (NamePrefix(provider) is not { } prefix || string.IsNullOrEmpty(name) || !name.StartsWith(prefix, StringComparison.OrdinalIgnoreCase))
            return false;
        return name.Length == prefix.Length || name[prefix.Length] is '-' or '_' or '.';
    }

    /// <summary>CLI 用過的設定資料夾才會有的東西（有任何一個就算；別的工具剛好取了像的名稱的資料夾不會有）</summary>
    public static IReadOnlyList<string> Markers(AgentProvider provider) => provider switch
    {
        AgentProvider.Claude => new[] { ".claude.json", ".credentials.json", "projects" },
        AgentProvider.Codex or AgentProvider.Grok => new[] { "auth.json", "config.toml", "sessions" },
        AgentProvider.Antigravity => Array.Empty<string>(),
        _ => throw new ArgumentOutOfRangeException(nameof(provider), provider, null),
    };

    /// <summary>
    /// 使用者資料夾底下可以直接加成帳戶的設定資料夾（新增帳戶對話框列出來讓使用者點）：名稱像（<see cref="IsCandidateName"/>）、
    /// 裡面有 CLI 用過的痕跡（<see cref="Markers"/>）、不是預設帳戶的資料夾、也還沒有帳戶在用。依名稱排序。
    /// homeDirectories＝使用者資料夾底下的資料夾（完整路徑）；entryExists＝這個路徑的檔案或資料夾存不存在
    /// </summary>
    public static IReadOnlyList<string> Find(AgentProvider provider, IEnumerable<string> homeDirectories, Func<string, bool> entryExists,
        IEnumerable<string> defaultDirectories, IEnumerable<string> usedDirectories)
    {
        if (!AccountRules.SupportsIsolatedAccounts(provider))
            return Array.Empty<string>();
        var excluded = defaultDirectories.Concat(usedDirectories).ToList();
        return homeDirectories
            .Where(dir => IsCandidateName(provider, Path.GetFileName(Path.TrimEndingDirectorySeparator(dir))))
            .Where(dir => !excluded.Any(x => SameFolder(x, dir)))
            .Where(dir => HasMarker(provider, dir, entryExists))
            .OrderBy(dir => dir, StringComparer.OrdinalIgnoreCase)
            .ToList();
    }

    /// <summary>
    /// 現有資料夾的預設帳戶名稱：資料夾名稱去掉開頭的 .claude- 之後還有字就用「Claude 〈那段字〉」（~/.claude-work → Claude work），
    /// 沒有、或同名的帳戶已經有了就照一般的編號（AccountRules.DefaultLabel）
    /// </summary>
    public static string DefaultLabel(AgentProvider provider, string directory, IReadOnlyCollection<AgentAccount> existing)
    {
        var name = Path.GetFileName(Path.TrimEndingDirectorySeparator(directory));
        if (NamePrefix(provider) is { } prefix && IsCandidateName(provider, name))
        {
            var rest = name[prefix.Length..].Trim('-', '_', '.', ' ');
            if (rest.Length > 0)
            {
                var label = $"{AccountRules.ProviderName(provider)} {rest}";
                if (label.Length > 40)
                    label = label[..40];
                if (!existing.Any(a => string.Equals(a.Label, label, StringComparison.OrdinalIgnoreCase)))
                    return label;
            }
        }
        return AccountRules.DefaultLabel(provider, existing);
    }

    /// <summary>給人看的路徑：使用者資料夾底下的寫成 ~/…（對話框的按鈕與提示用；送回宿主的仍是完整路徑）</summary>
    public static string Display(string directory, string userProfile)
    {
        var home = Path.TrimEndingDirectorySeparator(userProfile);
        if (home.Length > 0 && directory.Length > home.Length && directory.StartsWith(home, StringComparison.OrdinalIgnoreCase)
            && directory[home.Length] is '/' or '\\')
            return "~" + directory[home.Length..];
        return directory;
    }
}
