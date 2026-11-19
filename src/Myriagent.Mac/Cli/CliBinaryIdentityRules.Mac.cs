using System.Runtime.InteropServices;

namespace Myriagent.Cli;

/// <summary>
/// 讀檔案身分與 Claude 版本的 macOS 做法（Windows 的對應實作在 src/Myriagent/Cli/CliBinaryIdentityRules.Win.cs）。
/// <para>
/// 檔案身分＝stat 的裝置編號＋inode＋大小＋修改時間。stat 會跟著捷徑（symlink）走到真正的檔案：macOS 的原生 Claude 是
/// <c>~/.local/bin/claude</c> → <c>~/.local/share/claude/versions/〈版本〉</c>，Claude 更新時把捷徑改指向新版本的檔案，身分就跟著變。
/// </para>
/// </summary>
internal static partial class CliBinaryIdentityRules
{
    /// <summary>struct stat（64 位元 inode 的版本）的大小與欄位位移：2026-11-19 在 macOS 26.5（x86_64）以 C 的 offsetof 實測</summary>
    private const int StatSize = 144;
    private const int StatDeviceOffset = 0;
    private const int StatModeOffset = 4;
    private const int StatInodeOffset = 8;
    private const int StatModifiedSecondsOffset = 48;
    private const int StatModifiedNanosecondsOffset = 56;
    private const int StatSizeOffset = 96;
    private const int FileTypeMask = 0xF000;       // S_IFMT
    private const int DirectoryType = 0x4000;      // S_IFDIR

    /// <summary>讀檔案的身分欄位（只查屬性，不開檔，不擋換檔）。不存在、是資料夾、讀不到回 null</summary>
    public static CliFileStamp? TryReadStamp(string? path)
    {
        if (string.IsNullOrWhiteSpace(path))
            return null;
        var buffer = new byte[StatSize];
        // x86_64 的「stat」是舊的 32 位元 inode 版本，64 位元的叫 stat$INODE64；arm64 只有一種，就叫 stat
        int result = RuntimeInformation.ProcessArchitecture == Architecture.X64 ? StatInode64(path, buffer) : Stat(path, buffer);
        if (result != 0)
            return null;
        int mode = BitConverter.ToUInt16(buffer, StatModeOffset);
        if ((mode & FileTypeMask) == DirectoryType)
            return null;
        ulong device = (uint)BitConverter.ToInt32(buffer, StatDeviceOffset);
        ulong inode = BitConverter.ToUInt64(buffer, StatInodeOffset);
        long size = BitConverter.ToInt64(buffer, StatSizeOffset);
        long seconds = BitConverter.ToInt64(buffer, StatModifiedSecondsOffset);
        long nanoseconds = BitConverter.ToInt64(buffer, StatModifiedNanosecondsOffset);
        var written = DateTime.UnixEpoch.AddTicks(seconds * TimeSpan.TicksPerSecond + nanoseconds / 100);
        return new CliFileStamp(device, inode, size, written);
    }

    /// <summary>
    /// 原生 Claude 的版本（不啟動程序）：macOS 的執行檔沒有版本資源，版本就是檔名——<c>claude</c> 這個捷徑指到的
    /// <c>…/claude/versions/2.1.326</c>。不存在、不是這種配置（檔名不是版本）回 null
    /// </summary>
    public static string? ReadClaudeFileVersion(string? exePath)
    {
        if (string.IsNullOrWhiteSpace(exePath) || !File.Exists(exePath))
            return null;
        try
        {
            var target = File.ResolveLinkTarget(exePath, returnFinalTarget: true)?.FullName ?? exePath;
            return CliVersionRules.ParseVersion(Path.GetFileName(target));
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or ArgumentException)
        {
            return null;
        }
    }

    [DllImport("libc", EntryPoint = "stat$INODE64", SetLastError = true)]
    private static extern int StatInode64(string path, [Out] byte[] buffer);

    [DllImport("libc", EntryPoint = "stat", SetLastError = true)]
    private static extern int Stat(string path, [Out] byte[] buffer);
}
