using System.Diagnostics;
using System.Security.Cryptography;
using System.Text;
using Myriagent.Core;

namespace Myriagent.Accounts;

/// <summary>
/// 從登入鑰匙圈讀 Claude Code 的登入憑證（macOS 的 Claude Code 不寫 .credentials.json，同樣內容的 JSON 存在鑰匙圈）。
/// 項目的名稱照 Claude Code 自己的規則：預設的設定資料夾（沒有設 CLAUDE_CONFIG_DIR）是「Claude Code-credentials」；
/// 指定了設定資料夾的（app 的隔離帳戶都是）後面再加「-」與資料夾路徑 SHA-256 的前 8 個十六進位字元。
/// 讀法也和 Claude Code 一樣：執行系統的 <c>security</c> 工具。那些項目本來就是 <c>security</c> 建的，由它讀不會跳出
/// 「要不要允許這個 app 使用鑰匙圈」的詢問（app 自己直接讀就會問，而且每換一版再問一次）。
/// 只讀、不寫：app 絕不自己換發或改動權杖（和 Windows 版一樣）；讀到的內容不記進記錄檔
/// </summary>
internal static class MacClaudeKeychain
{
    private const string ServiceName = "Claude Code-credentials";

    /// <summary>同一個帳戶連續讀（狀態檢查、查用量前後各讀一次）不必每次都起一個程序</summary>
    private static readonly TimeSpan CacheFor = TimeSpan.FromSeconds(2);

    private static readonly TimeSpan Timeout = TimeSpan.FromSeconds(5);
    private static readonly object Gate = new();
    private static readonly Dictionary<string, (DateTime At, string? Json)> Cache = new(StringComparer.Ordinal);

    /// <summary>把鑰匙圈接到 <see cref="ClaudeCredentialStore.Fallback"/>（啟動時呼叫一次）</summary>
    public static void Install() => ClaudeCredentialStore.Fallback = Read;

    /// <summary>
    /// 鑰匙圈項目的名稱。configDirectory＝這個帳戶的設定資料夾；inheritedConfigDirectory＝app 自己的環境變數 CLAUDE_CONFIG_DIR（沒設是 null）。
    /// 系統預設帳戶在沒有設 CLAUDE_CONFIG_DIR 時用的是 ~/.claude，Claude Code 這時不加後綴
    /// </summary>
    internal static string ServiceFor(string configDirectory, string? inheritedConfigDirectory, string userProfile)
    {
        bool isDefault = string.IsNullOrWhiteSpace(inheritedConfigDirectory)
                         && string.Equals(Path.TrimEndingDirectorySeparator(configDirectory), Path.Combine(userProfile, ".claude"), StringComparison.Ordinal);
        if (isDefault)
            return ServiceName;
        var hash = Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(configDirectory)));
        return ServiceName + "-" + hash[..8];
    }

    /// <summary>讀這個憑證檔所屬帳戶在鑰匙圈的憑證 JSON；沒有、讀不到回 null</summary>
    public static string? Read(string credentialFile)
    {
        var configDirectory = Path.GetDirectoryName(credentialFile);
        if (string.IsNullOrEmpty(configDirectory))
            return null;
        var service = ServiceFor(configDirectory, Environment.GetEnvironmentVariable("CLAUDE_CONFIG_DIR"),
            Environment.GetFolderPath(Environment.SpecialFolder.UserProfile));
        lock (Gate)
        {
            if (Cache.TryGetValue(service, out var cached) && DateTime.UtcNow - cached.At < CacheFor)
                return cached.Json;
        }
        var json = Find(service);
        lock (Gate)
        {
            Cache[service] = (DateTime.UtcNow, json);
        }
        return json;
    }

    /// <summary><c>security find-generic-password -a 〈使用者〉 -s 〈項目〉 -w</c>：標準輸出就是存的內容；沒有這個項目時結束碼不是 0</summary>
    private static string? Find(string service)
    {
        try
        {
            var info = new ProcessStartInfo("/usr/bin/security") { RedirectStandardOutput = true, RedirectStandardError = true, UseShellExecute = false };
            foreach (var argument in new[] { "find-generic-password", "-a", Environment.UserName, "-s", service, "-w" })
                info.ArgumentList.Add(argument);
            using var process = Process.Start(info);
            if (process == null)
                return null;
            var output = process.StandardOutput.ReadToEndAsync();
            _ = process.StandardError.ReadToEndAsync();
            if (!process.WaitForExit((int)Timeout.TotalMilliseconds))
            {
                process.Kill();
                return null;
            }
            if (process.ExitCode != 0)
                return null;
            var text = output.GetAwaiter().GetResult().Trim();
            // 內容是 JSON 物件才算（security 對含特殊字元的內容會改印十六進位；Claude Code 存的是純 JSON）
            return text.StartsWith('{') ? text : null;
        }
        catch (Exception ex) when (ex is System.ComponentModel.Win32Exception or InvalidOperationException or IOException)
        {
            Log.Warn($"讀不了鑰匙圈（security）：{ex.Message}");
            return null;
        }
    }
}
