using Myriagent.Agents;
using Myriagent.Core;

namespace Myriagent.Cli;

/// <summary>
/// 在 macOS 找本機的 Claude Code、Codex、Antigravity（agy）、Grok Build CLI（Windows 版的找法見 src/Myriagent/Cli/CliLocator.cs）。
/// macOS 的 CLI 就是一個可以直接執行的檔案（原生執行檔，或開頭寫明用 node 執行的腳本），沒有 Windows 的 .exe／.cmd 之分：
/// 依序在各家官方安裝位置與 PATH 的資料夾找同名的檔案，第一個就是。安裝方式看它實際指向哪裡：
/// - Claude 官方安裝：~/.local/bin/claude 是指向 ~/.local/share/claude/versions/〈版本〉 的連結 → Native（以 claude update 更新）
/// - npm 全域安裝：bin 裡的連結指向 …/node_modules/〈套件〉/… → Npm
/// - app 自行下載的 Codex → Managed
/// - 其他（Homebrew、各家的安裝程式、手動放的）→ Other（以 CLI 自己的 update 指令更新）
/// PATH 已經在啟動時換成登入殼層的（MacEnvironment），所以終端機找得到的這裡也找得到
/// </summary>
internal static class MacCliLocator
{
    public static string UserProfile => Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);

    /// <summary>Claude 官方安裝放連結的位置</summary>
    public static string ClaudeNativeDirectory => Path.Combine(UserProfile, ".local", "bin");

    /// <summary>Claude 官方安裝放各版本執行檔的位置</summary>
    public static string ClaudeVersionsDirectory => Path.Combine(UserProfile, ".local", "share", "claude", "versions");

    /// <summary>Grok 官方安裝的位置</summary>
    public static string GrokInstallDirectory => Path.Combine(UserProfile, ".grok", "bin");

    /// <summary>給 CliManager 的設定：找 CLI 與 npm 用 macOS 的找法，沒有 WinGet</summary>
    public static CliManagerOptions Options() => new()
    {
        Locate = Locate,
        LocateNpm = LocateNpm,
        LocateWinget = () => null,
    };

    public static string ExecutableName(AgentProvider provider) => provider switch
    {
        AgentProvider.Claude => "claude",
        AgentProvider.Codex => "codex",
        AgentProvider.Antigravity => "agy",
        AgentProvider.Grok => "grok",
        _ => throw new ArgumentOutOfRangeException(nameof(provider), provider, null),
    };

    public static List<string> GetSearchDirectories(AgentProvider provider)
    {
        string[] first = provider switch
        {
            AgentProvider.Claude => [ClaudeNativeDirectory],
            AgentProvider.Codex => [],
            AgentProvider.Antigravity => [ClaudeNativeDirectory],
            AgentProvider.Grok => [GrokInstallDirectory],
            _ => throw new ArgumentOutOfRangeException(nameof(provider), provider, null),
        };
        string[] last = provider == AgentProvider.Codex ? [AppPaths.ManagedCodexDirectory] : [];
        return CliLocator.MergeSearchDirectories(first.Append(Environment.GetEnvironmentVariable("PATH")), last);
    }

    public static CliInstallation? Locate(AgentProvider provider)
        => Resolve(provider, GetSearchDirectories(provider), File.Exists, RealPath, ClaudeNativeDirectory, ClaudeVersionsDirectory, AppPaths.ManagedCodexDirectory);

    /// <summary>純函式版本（測試用）：依目錄順序找 CLI 並判斷安裝方式。realPath＝連結最後指向的檔案（不是連結就是它自己）</summary>
    internal static CliInstallation? Resolve(
        AgentProvider provider,
        IReadOnlyList<string> directories,
        Func<string, bool> fileExists,
        Func<string, string> realPath,
        string claudeNativeDirectory,
        string claudeVersionsDirectory,
        string managedCodexDirectory)
    {
        var name = ExecutableName(provider);
        foreach (var directory in directories)
        {
            var path = Path.Combine(directory, name);
            if (!fileExists(path))
                continue;
            var real = realPath(path);
            var launcher = new AgentLauncher(path, Array.Empty<string>());
            if (NpmPackageRootOf(provider, real) is { } packageRoot)
            {
                // npm 版 Codex 的入口是 node 腳本，真正執行的原生檔在平台套件裡；Claude 的入口本身就是代表（換版時會被換掉）
                var native = provider == AgentProvider.Codex ? CodexNativeExecutable(packageRoot, fileExists) ?? real : real;
                return new CliInstallation(provider, launcher, CliInstallKind.Npm, path) { NativeExecutable = native };
            }
            var kind = provider == AgentProvider.Claude && (SamePath(directory, claudeNativeDirectory) || IsUnder(real, claudeVersionsDirectory)) ? CliInstallKind.Native
                : provider == AgentProvider.Codex && SamePath(directory, managedCodexDirectory) ? CliInstallKind.Managed
                : CliInstallKind.Other;
            return new CliInstallation(provider, launcher, kind, path) { NativeExecutable = real };
        }
        return null;
    }

    /// <summary>實際的檔案在 npm 套件裡（…/node_modules/@scope/name/…）時回套件的根目錄；不是 npm 安裝（或這種 CLI 沒有 npm 版）回 null</summary>
    internal static string? NpmPackageRootOf(AgentProvider provider, string realPath)
    {
        var package = provider switch
        {
            AgentProvider.Claude => CliLocator.ClaudeNpmPackage,
            AgentProvider.Codex => CliLocator.CodexNpmPackage,
            _ => null,
        };
        if (package == null)
            return null;
        var marker = "/node_modules/" + package + "/";
        int at = realPath.IndexOf(marker, StringComparison.Ordinal);
        return at < 0 ? null : realPath[..(at + marker.Length - 1)];
    }

    /// <summary>
    /// npm 版 Codex 實際執行的原生檔（照 codex.js 的找法）：這台架構的平台套件裡的 vendor/〈目標〉/codex/codex；
    /// 巢狀在套件的 node_modules 裡，或被提升到旁邊。都不存在回 null
    /// </summary>
    internal static string? CodexNativeExecutable(string packageRoot, Func<string, bool> fileExists)
    {
        bool arm = System.Runtime.InteropServices.RuntimeInformation.OSArchitecture == System.Runtime.InteropServices.Architecture.Arm64;
        var targets = arm
            ? new[] { ("aarch64-apple-darwin", "codex-darwin-arm64"), ("x86_64-apple-darwin", "codex-darwin-x64") }
            : new[] { ("x86_64-apple-darwin", "codex-darwin-x64"), ("aarch64-apple-darwin", "codex-darwin-arm64") };
        var scope = Path.GetDirectoryName(packageRoot);
        foreach (var (triple, platformPackage) in targets)
        {
            var roots = new List<string> { Path.Combine(packageRoot, "node_modules", "@openai", platformPackage, "vendor", triple) };
            if (scope != null)
                roots.Add(Path.Combine(scope, platformPackage, "vendor", triple));
            roots.Add(Path.Combine(packageRoot, "vendor", triple));
            foreach (var root in roots)
            {
                var candidate = Path.Combine(root, "codex", "codex");
                if (fileExists(candidate))
                    return candidate;
            }
        }
        return null;
    }

    /// <summary>找 npm（安裝或更新 npm 版 CLI 用）：PATH 上的 npm</summary>
    public static string? LocateNpm()
        => CliLocator.MergeSearchDirectories(new[] { Environment.GetEnvironmentVariable("PATH") }, Array.Empty<string>())
            .Select(directory => Path.Combine(directory, "npm"))
            .FirstOrDefault(File.Exists);

    /// <summary>連結最後指向的檔案；不是連結、讀不到就是原路徑</summary>
    private static string RealPath(string path)
    {
        try
        {
            return File.ResolveLinkTarget(path, returnFinalTarget: true)?.FullName ?? path;
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            return path;
        }
    }

    private static bool SamePath(string a, string b) => string.Equals(a.TrimEnd('/'), b.TrimEnd('/'), StringComparison.Ordinal);

    private static bool IsUnder(string path, string directory) => path.StartsWith(directory.TrimEnd('/') + "/", StringComparison.Ordinal);
}
