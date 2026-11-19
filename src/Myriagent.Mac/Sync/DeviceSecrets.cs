using System.Security.Cryptography;
using System.Text;
using Myriagent.Core;

namespace Myriagent.Sync;

/// <summary>
/// 裝置權杖（chd_）的保存，macOS 版（Windows 版是 DPAPI，見 src/Myriagent/Sync/DeviceSecrets.Win.cs；對外的成員相同）。
/// 以 AES-GCM 加密，金鑰是 <c>sync\secret.key</c>（32 位元組亂數，只有這個使用者讀得到：檔案權限 0600）。
/// 保護的程度和 DPAPI 的 CurrentUser 相當：換了使用者讀不到金鑰，密文被竄改驗不過；同一個使用者的其他程式本來就讀得到。
/// 不放鑰匙圈：沒有固定簽章的 app 每換一版，系統都會再問一次要不要讓它讀鑰匙圈。
/// cha_（存取權杖）只在記憶體，不經這裡
/// </summary>
internal static class DeviceSecrets
{
    private const int KeyBytes = 32;
    private const int NonceBytes = 12;
    private const int TagBytes = 16;

    /// <summary>格式版本（密文的第一個位元組）：之後換演算法時分得出舊的</summary>
    private const byte FormatVersion = 1;

    private static readonly byte[] AssociatedData = Encoding.UTF8.GetBytes("Myriagent.Sync.v1");
    private static readonly object Gate = new();

    private static string KeyPath => Path.Combine(AppPaths.SyncDirectory, "secret.key");

    public static string Protect(string token)
    {
        var key = LoadKey(create: true) ?? throw new CryptographicException("建立不了保護裝置權杖的金鑰");
        var plain = Encoding.UTF8.GetBytes(token);
        var output = new byte[1 + NonceBytes + TagBytes + plain.Length];
        output[0] = FormatVersion;
        var nonce = output.AsSpan(1, NonceBytes);
        RandomNumberGenerator.Fill(nonce);
        using var aes = new AesGcm(key, TagBytes);
        aes.Encrypt(nonce, plain, output.AsSpan(1 + NonceBytes + TagBytes), output.AsSpan(1 + NonceBytes, TagBytes), AssociatedData);
        return Convert.ToBase64String(output);
    }

    /// <summary>解不開（換了使用者、金鑰不見、被竄改、不是 base64）回 null</summary>
    public static string? Unprotect(string? protectedToken)
    {
        if (string.IsNullOrEmpty(protectedToken))
            return null;
        try
        {
            var input = Convert.FromBase64String(protectedToken);
            if (input.Length < 1 + NonceBytes + TagBytes || input[0] != FormatVersion || LoadKey(create: false) is not { } key)
                return null;
            var plain = new byte[input.Length - 1 - NonceBytes - TagBytes];
            using var aes = new AesGcm(key, TagBytes);
            aes.Decrypt(input.AsSpan(1, NonceBytes), input.AsSpan(1 + NonceBytes + TagBytes), input.AsSpan(1 + NonceBytes, TagBytes), plain, AssociatedData);
            return new UTF8Encoding(false, throwOnInvalidBytes: true).GetString(plain);
        }
        catch (Exception ex) when (ex is CryptographicException or FormatException or ArgumentException)
        {
            return null;
        }
    }

    /// <summary>讀金鑰；沒有而且 create 時建立（建立時就設成只有自己讀得到，不是寫完才改權限）</summary>
    private static byte[]? LoadKey(bool create)
    {
        lock (Gate)
        {
            var path = KeyPath;
            try
            {
                if (File.Exists(path))
                {
                    var existing = File.ReadAllBytes(path);
                    if (existing.Length == KeyBytes)
                        return existing;
                    if (!create)
                        return null;
                }
                else if (!create)
                {
                    return null;
                }

                Directory.CreateDirectory(Path.GetDirectoryName(path)!);
                var key = RandomNumberGenerator.GetBytes(KeyBytes);
                var temp = path + ".tmp";
                File.Delete(temp);
                var options = new FileStreamOptions
                {
                    Mode = FileMode.CreateNew,
                    Access = FileAccess.Write,
                    Share = FileShare.None,
                    UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite,
                };
                using (var stream = new FileStream(temp, options))
                {
                    stream.Write(key);
                    stream.Flush(flushToDisk: true);
                }
                File.Move(temp, path, overwrite: true);
                return key;
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                Log.Warn($"保護裝置權杖的金鑰讀寫失敗：{ex.Message}");
                return null;
            }
        }
    }
}
