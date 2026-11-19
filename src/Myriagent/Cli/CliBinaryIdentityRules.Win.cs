using System.Buffers.Binary;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;
using Myriagent.Agents;
using Microsoft.Win32.SafeHandles;

namespace Myriagent.Cli;

/// <summary>讀檔案身分與 claude.exe 版本資源的 Windows 做法（檔案識別碼、FileVersionInfo）；macOS 的對應實作在 src/Myriagent.Mac/Cli/CliBinaryIdentityRules.Mac.cs</summary>
internal static partial class CliBinaryIdentityRules
{
    private const uint FILE_READ_ATTRIBUTES = 0x0080;
    private const uint FILE_ATTRIBUTE_DIRECTORY = 0x0010;
    private const int FileIdInfoClass = 18;   // FILE_INFO_BY_HANDLE_CLASS.FileIdInfo
    private const int FileIdInfoSize = 24;    // FILE_ID_INFO：ULONGLONG VolumeSerialNumber＋FILE_ID_128（16 個位元組）

    /// <summary>
    /// 讀檔案的身分欄位：只要 FILE_READ_ATTRIBUTES、共用 ReadWrite|Delete 開檔（不擋換檔），讀完立即關。
    /// 不存在、是資料夾、打不開回 null
    /// </summary>
    public static CliFileStamp? TryReadStamp(string? path)
    {
        if (string.IsNullOrWhiteSpace(path))
            return null;
        using var handle = CreateFile(path, FILE_READ_ATTRIBUTES, FileShare.ReadWrite | FileShare.Delete, IntPtr.Zero, FileMode.Open, 0, IntPtr.Zero);
        if (handle.IsInvalid)
            return null;
        if (!GetFileInformationByHandle(handle, out var info) || (info.FileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0)
            return null;
        var buffer = new byte[FileIdInfoSize];
        if (!GetFileInformationByHandleEx(handle, FileIdInfoClass, buffer, FileIdInfoSize))
            return null;
        var volume = BinaryPrimitives.ReadUInt64LittleEndian(buffer.AsSpan(0, 8));
        var fileId = ParseFileId(buffer.AsSpan(8, 16));
        var size = ((long)info.FileSizeHigh << 32) | info.FileSizeLow;
        var written = DateTime.FromFileTimeUtc(((long)info.LastWriteTimeHigh << 32) | info.LastWriteTimeLow);
        return new CliFileStamp(volume, fileId, size, written);
    }

    /// <summary>
    /// 純函式：FILE_ID_INFO.FileId（FILE_ID_128 的 16 個位元組）依 little-endian 解讀成 128 位元整數。
    /// 與 fsutil file queryfileid 印的十六進位相同（例：0x000000000000000000220000003bf109）
    /// </summary>
    public static UInt128 ParseFileId(ReadOnlySpan<byte> identifier)
    {
        if (identifier.Length != 16)
            throw new ArgumentException("FILE_ID_128 必須是 16 個位元組", nameof(identifier));
        return new UInt128(BinaryPrimitives.ReadUInt64LittleEndian(identifier[8..]), BinaryPrimitives.ReadUInt64LittleEndian(identifier[..8]));
    }

    /// <summary>
    /// claude.exe 的檔案版本資訊（FileVersionInfo，約 0.23 ms、不啟動程序）→ x.y.z；不存在、沒有版本資源回 null。
    /// codex.exe 沒有版本資源（每一份都是空的），Codex 要讀 package.json 或跑 --version
    /// </summary>
    public static string? ReadClaudeFileVersion(string? exePath)
    {
        if (string.IsNullOrWhiteSpace(exePath) || !File.Exists(exePath))
            return null;
        try
        {
            var info = FileVersionInfo.GetVersionInfo(exePath);
            return ParseFileVersion(info.FileVersion, info.FileMajorPart, info.FileMinorPart, info.FileBuildPart);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or ArgumentException)
        {
            return null;
        }
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct BY_HANDLE_FILE_INFORMATION
    {
        public uint FileAttributes;
        public uint CreationTimeLow;
        public uint CreationTimeHigh;
        public uint LastAccessTimeLow;
        public uint LastAccessTimeHigh;
        public uint LastWriteTimeLow;
        public uint LastWriteTimeHigh;
        public uint VolumeSerialNumber;
        public uint FileSizeHigh;
        public uint FileSizeLow;
        public uint NumberOfLinks;
        public uint FileIndexHigh;
        public uint FileIndexLow;
    }

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true, EntryPoint = "CreateFileW")]
    private static extern SafeFileHandle CreateFile(string fileName, uint desiredAccess, FileShare shareMode, IntPtr securityAttributes,
        FileMode creationDisposition, uint flagsAndAttributes, IntPtr templateFile);

    [DllImport("kernel32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool GetFileInformationByHandle(SafeFileHandle file, out BY_HANDLE_FILE_INFORMATION information);

    [DllImport("kernel32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool GetFileInformationByHandleEx(SafeFileHandle file, int informationClass, [Out] byte[] information, uint bufferSize);
}
