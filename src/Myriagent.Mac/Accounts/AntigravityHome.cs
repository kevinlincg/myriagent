using System.Collections.Concurrent;
using System.Diagnostics;
using Myriagent.Core;

namespace Myriagent.Accounts;

/// <summary>
/// Antigravity 隔離帳戶的「家」（規則在 AntigravityHomeRules）：每次要跑這個帳戶的 agy 之前整理好——
/// 自己的鑰匙圈（沒有就建立、空密碼、不自動上鎖，每次解鎖）、shell 小腳本、連回真正的家的連結（真正的家多了東西就補、少了就拿掉）。
/// 連結的整理最多每 30 秒一次（狀態檢查、用量、對話會連續呼叫）；鑰匙圈只在第一次與連結整理時檢查。
/// 失敗只記錄：agy 照樣啟動，看起來就是沒登入
/// </summary>
internal static class AntigravityHome
{
    private static readonly TimeSpan RefreshInterval = TimeSpan.FromSeconds(30);
    private static readonly ConcurrentDictionary<string, DateTime> lastRefresh = new(StringComparer.Ordinal);
    private static readonly object gate = new();

    public static void Prepare(string home)
    {
        var now = DateTime.UtcNow;
        if (lastRefresh.TryGetValue(home, out var last) && now - last < RefreshInterval)
            return;
        lock (gate)
        {
            if (lastRefresh.TryGetValue(home, out last) && DateTime.UtcNow - last < RefreshInterval)
                return;
            try
            {
                var realHome = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
                Directory.CreateDirectory(AntigravityHomeRules.ConfigDirectory(home));
                Directory.CreateDirectory(Path.GetDirectoryName(AntigravityHomeRules.KeychainPath(home))!);
                WriteShellScripts(home, realHome);
                SyncLinks(home, realHome);
                EnsureKeychain(home);
                lastRefresh[home] = DateTime.UtcNow;
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or InvalidOperationException or System.ComponentModel.Win32Exception)
            {
                Log.Warn($"整理 agy 帳戶的資料夾失敗（{home}）：{ex.Message}");
            }
        }
    }

    /// <summary>
    /// 刪除帳戶資料夾之前先拿掉家裡所有連結（只拿掉連結本身，不進去）：連結指向真正的家，刪資料夾絕不能順著連結刪到那邊
    /// </summary>
    public static void RemoveLinks(string accountRoot)
    {
        var home = Path.Combine(accountRoot, AntigravityHomeRules.FolderName);
        if (!Directory.Exists(home) || new DirectoryInfo(home).LinkTarget != null)
            return;
        foreach (var (folder, _) in AntigravityHomeRules.MirroredFolders)
            RemoveLinksIn(folder.Length == 0 ? home : Path.Combine(home, folder));
        RemoveLinksIn(AntigravityHomeRules.ConfigDirectory(home));
        lastRefresh.TryRemove(home, out _);
    }

    private static void RemoveLinksIn(string folder)
    {
        try
        {
            if (!Directory.Exists(folder) || new DirectoryInfo(folder).LinkTarget != null)
                return;
            foreach (var path in Directory.EnumerateFileSystemEntries(folder))
            {
                if (new FileInfo(path).LinkTarget != null)
                    File.Delete(path);
            }
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            Log.Warn($"拿掉 agy 帳戶資料夾裡的連結失敗（{folder}）：{ex.Message}");
        }
    }

    private static void WriteShellScripts(string home, string realHome)
    {
        var shell = Path.Combine(home, AntigravityHomeRules.ShellFolder);
        Directory.CreateDirectory(shell);
        WriteIfChanged(Path.Combine(shell, ".zshenv"), AntigravityHomeRules.ZshEnvScript(realHome));
        WriteIfChanged(Path.Combine(shell, AntigravityHomeRules.BashEnvFile), AntigravityHomeRules.BashEnvScript(realHome));
    }

    private static void WriteIfChanged(string path, string content)
    {
        if (File.Exists(path) && File.ReadAllText(path) == content)
            return;
        File.WriteAllText(path, content);
    }

    /// <summary>補上該有的連結、換掉指錯的、拿掉多出來的連結（只動連結；同名的真檔案或資料夾是 agy 自己建的，不動）</summary>
    private static void SyncLinks(string home, string realHome)
    {
        var planned = AntigravityHomeRules.PlanLinks(home, realHome, List);
        var wanted = planned.ToDictionary(p => p.Link, p => p.Target, StringComparer.Ordinal);
        foreach (var (link, target) in planned)
        {
            var info = new FileInfo(link);
            if (info.LinkTarget is { } current)
            {
                if (current == target)
                    continue;
                File.Delete(link);
            }
            else if (info.Exists || Directory.Exists(link))
                continue;
            Directory.CreateDirectory(Path.GetDirectoryName(link)!);
            File.CreateSymbolicLink(link, target);
        }
        var folders = AntigravityHomeRules.MirroredFolders.Select(m => m.Folder.Length == 0 ? home : Path.Combine(home, m.Folder))
            .Append(AntigravityHomeRules.ConfigDirectory(home));
        foreach (var folder in folders)
        {
            if (!Directory.Exists(folder))
                continue;
            foreach (var path in Directory.EnumerateFileSystemEntries(folder))
            {
                if (new FileInfo(path).LinkTarget != null && !wanted.ContainsKey(path))
                    File.Delete(path);
            }
        }
    }

    private static IEnumerable<string> List(string folder)
    {
        try
        {
            return Directory.Exists(folder)
                ? Directory.EnumerateFileSystemEntries(folder).Select(Path.GetFileName).OfType<string>().ToList()
                : [];
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            return [];
        }
    }

    /// <summary>
    /// 自己的鑰匙圈：沒有就以 HOME＝家 建立（空密碼；不加進使用者的鑰匙圈清單），設成不自動上鎖，每次解鎖（重開機後是鎖著的）。
    /// agy 經由 /usr/bin/security 存取鑰匙圈，HOME 換成家時它的預設鑰匙圈就是這一個
    /// </summary>
    private static void EnsureKeychain(string home)
    {
        var keychain = AntigravityHomeRules.KeychainPath(home);
        if (!File.Exists(keychain))
        {
            Security(home, "create-keychain", "-p", "", keychain);
            Security(home, "set-keychain-settings", keychain);
        }
        Security(home, "unlock-keychain", "-p", "", keychain);
    }

    private static void Security(string home, params string[] arguments)
    {
        var psi = new ProcessStartInfo("/usr/bin/security")
        {
            UseShellExecute = false,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
        };
        foreach (var a in arguments)
            psi.ArgumentList.Add(a);
        psi.Environment["HOME"] = home;
        using var process = Process.Start(psi) ?? throw new InvalidOperationException("無法執行 security");
        var error = process.StandardError.ReadToEndAsync();
        process.StandardOutput.ReadToEnd();
        if (!process.WaitForExit(15000))
        {
            try { process.Kill(); } catch (InvalidOperationException) { }
            throw new InvalidOperationException($"security {arguments[0]} 逾時");
        }
        if (process.ExitCode != 0)
            throw new InvalidOperationException($"security {arguments[0]} 失敗：{error.Result.Trim()}");
    }
}
