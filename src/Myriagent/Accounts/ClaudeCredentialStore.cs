namespace Myriagent.Accounts;

/// <summary>
/// Claude 的登入憑證不在 .credentials.json 時還能從哪裡讀。
/// Windows 的憑證就是設定資料夾裡的 .credentials.json（或 Windows 認證管理員，那個 app 讀不到，另有保守模式）；
/// macOS 的 Claude Code 把同樣內容的 JSON 存在登入鑰匙圈，設定資料夾裡沒有這個檔案。
/// 外殼在啟動時把平台的讀法掛到 <see cref="Fallback"/>（macOS：src/Myriagent.Mac/Accounts/MacClaudeKeychain.cs）；沒掛（Windows）就只有檔案
/// </summary>
internal static class ClaudeCredentialStore
{
    /// <summary>
    /// 憑證檔不存在時的另一個來源：參數是原本要讀的憑證檔路徑（從它的資料夾認出是哪個帳戶），回憑證的 JSON；沒有回 null。
    /// 任何執行緒都可能呼叫；不丟例外（讀不到就是 null）
    /// </summary>
    public static Func<string, string?>? Fallback { get; set; }

    /// <summary>憑證檔不存在時呼叫：有另一個來源就問它，沒有回 null</summary>
    public static string? ReadFallback(string credentialFile) => Fallback?.Invoke(credentialFile);
}
