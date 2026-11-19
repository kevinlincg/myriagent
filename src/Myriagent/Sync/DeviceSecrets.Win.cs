using System.Security.Cryptography;
using System.Text;

namespace Myriagent.Sync;

/// <summary>
/// 裝置權杖（chd_）的保存：DPAPI CurrentUser＋固定的 entropy（sync-protocol 2.3）。換了 Windows 使用者、或密文被竄改就解不開。
/// cha_（存取權杖）只在記憶體，不經這裡
/// </summary>
internal static class DeviceSecrets
{
    private static readonly byte[] Entropy = Encoding.UTF8.GetBytes("Myriagent.Sync.v1");

    public static string Protect(string token) =>
        Convert.ToBase64String(ProtectedData.Protect(Encoding.UTF8.GetBytes(token), Entropy, DataProtectionScope.CurrentUser));

    /// <summary>解不開（換了 Windows 使用者、被竄改、不是 base64）回 null</summary>
    public static string? Unprotect(string? protectedToken)
    {
        if (string.IsNullOrEmpty(protectedToken))
            return null;
        try
        {
            var bytes = ProtectedData.Unprotect(Convert.FromBase64String(protectedToken), Entropy, DataProtectionScope.CurrentUser);
            return new UTF8Encoding(false, throwOnInvalidBytes: true).GetString(bytes);
        }
        catch (Exception ex) when (ex is CryptographicException or FormatException or ArgumentException)
        {
            return null;
        }
    }
}
