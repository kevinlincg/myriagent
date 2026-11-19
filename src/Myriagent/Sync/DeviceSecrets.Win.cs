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

/// <summary><c>sync\pending-key.json</c>：建金鑰之前寫、配對完成或放棄之後刪（當機時留下的孤兒金鑰靠它找到）</summary>
internal sealed record PendingKeyRecord(string KeyName, string KeyStorage, long CreatedAt)
{
    public static void Write(string path, PendingKeyRecord record)
    {
        Directory.CreateDirectory(Path.GetDirectoryName(Path.GetFullPath(path))!);
        var json = new JObject
        {
            ["keyName"] = record.KeyName,
            ["keyStorage"] = record.KeyStorage,
            ["createdAt"] = record.CreatedAt,
        };
        var temp = path + ".tmp";
        using (var stream = new FileStream(temp, FileMode.Create, FileAccess.Write, FileShare.None))
        {
            var bytes = new UTF8Encoding(false).GetBytes(json.ToString(Formatting.None));
            stream.Write(bytes, 0, bytes.Length);
            stream.Flush(flushToDisk: true);
        }

        File.Move(temp, path, overwrite: true);
    }

    /// <summary>不存在回 (false, null)；存在但讀不懂回 (true, null)</summary>
    public static (bool Exists, PendingKeyRecord? Record) Read(string path)
    {
        if (!File.Exists(path))
            return (false, null);
        try
        {
            if (JsonConvert.DeserializeObject<JToken>(File.ReadAllText(path, new UTF8Encoding(false))) is not JObject json)
                return (true, null);
            var name = json["keyName"] is JValue { Type: JTokenType.String } n ? (string?)n : null;
            var storage = json["keyStorage"] is JValue { Type: JTokenType.String } s ? (string?)s : null;
            var created = json["createdAt"] is JValue { Type: JTokenType.Integer, Value: long c } ? c : 0;
            return string.IsNullOrEmpty(name) || !PairingRules.IsKeyStorage(storage) ? (true, null) : (true, new PendingKeyRecord(name, storage!, created));
        }
        catch (Exception ex) when (ex is JsonException or IOException or UnauthorizedAccessException)
        {
            return (true, null);
        }
    }
}

/// <summary>配對中途當掉（或失敗、取消）留下的東西：孤兒的 CNG 金鑰、pending-key.json、建到一半的 sync.db</summary>
internal static class PairingRecovery
{
    /// <summary>
    /// 有 pending-key.json 時：device.json 不存在→刪金鑰、pending 檔與 sync.db／-wal／-shm；device.json 存在而且是同一把→只刪 pending 檔；
    /// 不同把→刪 pending 那把與 pending 檔。device.json 讀不到或損毀時什麼都不刪（分不出 pending 那把是不是它的）。沒有 pending 檔就什麼都不做
    /// </summary>
    public static void Cleanup(SyncPaths paths, Action<string>? log)
    {
        var (exists, pending) = PendingKeyRecord.Read(paths.PendingKeyFile);
        if (!exists)
            return;
        var (status, device) = DeviceFile.Load(paths.DeviceFile);
        switch (status)
        {
            case DeviceFileStatus.Missing:
                if (pending is not null)
                    DeleteKey(pending, log);
                DeleteFile(paths.PendingKeyFile, log);
                DeleteFile(paths.DatabaseFile, log);
                DeleteFile(paths.DatabaseFile + "-wal", log);
                DeleteFile(paths.DatabaseFile + "-shm", log);
                break;
            case DeviceFileStatus.Ok:
                if (pending is not null && !string.Equals(pending.KeyName, device!.KeyName, StringComparison.Ordinal))
                    DeleteKey(pending, log);
                DeleteFile(paths.PendingKeyFile, log);
                break;
            case DeviceFileStatus.Corrupt or DeviceFileStatus.Unreadable:
                log?.Invoke($"device.json 讀不到或損毀（{status}），配對留下的金鑰先不清理");
                break;
            default:
                throw new ArgumentOutOfRangeException(nameof(status), status, null);
        }
    }

    /// <summary>
    /// 一次配對失敗或取消（commit 沒完成）：先照 <see cref="Cleanup"/>，再直接刪這次建的那把與記著它的 pending 檔。
    /// commit 沒完成時 device.json 不可能引用這把，不能只靠 device.json 的狀態判斷——device.json 損毀或讀不到時 Cleanup 什麼都不刪，
    /// 每次失敗的配對都會留下一把孤兒金鑰。唯一的例外是 device.json 讀得到而且記的就是這把（提交其實已經完成），那就不刪
    /// </summary>
    /// <param name="keyStorage">建好的金鑰的存放位置；還沒建好（null）時兩個位置都試</param>
    public static void Abandon(SyncPaths paths, string keyName, string? keyStorage, Action<string>? log)
    {
        Cleanup(paths, log);
        var (status, device) = DeviceFile.Load(paths.DeviceFile);
        if (status == DeviceFileStatus.Ok && string.Equals(device!.KeyName, keyName, StringComparison.Ordinal))
            return;
        DeleteKey(new PendingKeyRecord(keyName, PairingRules.IsKeyStorage(keyStorage) ? keyStorage! : PairingRules.KeyStorageSoftware, 0), log);
        var (exists, pending) = PendingKeyRecord.Read(paths.PendingKeyFile);
        // 只刪記著這把的 pending 檔：記著別把的是之前當掉留下的（device.json 損毀時 Cleanup 留著它），要留給下次清理
        if (exists && pending is not null && string.Equals(pending.KeyName, keyName, StringComparison.Ordinal))
            DeleteFile(paths.PendingKeyFile, log);
    }

    /// <summary>刪掉 pending 記的那把：先試記下的存放位置，不在那裡就試另一個（TPM 失敗退回軟體 KSP 時，記錄可能還是建立前寫的）</summary>
    private static void DeleteKey(PendingKeyRecord pending, Action<string>? log)
    {
        var other = pending.KeyStorage == PairingRules.KeyStorageTpm ? PairingRules.KeyStorageSoftware : PairingRules.KeyStorageTpm;
        if (DeviceKey.Delete(pending.KeyName, pending.KeyStorage) || DeviceKey.Delete(pending.KeyName, other))
            log?.Invoke($"已刪除配對留下的裝置金鑰 {pending.KeyName}");
    }

    private static void DeleteFile(string path, Action<string>? log)
    {
        try
        {
            if (File.Exists(path))
                File.Delete(path);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            log?.Invoke($"刪不掉 {Path.GetFileName(path)}：{ex.Message}");
        }
    }
}
