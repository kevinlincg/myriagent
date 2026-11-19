namespace Myriagent.Core;

/// <summary>各平台不同的檔名慣例（macOS 版與 Windows 版共用的程式碼用這裡的，不直接寫死 Windows 的名稱）</summary>
internal static class PlatformFiles
{
    /// <summary>執行檔的檔名：Windows 加 .exe，macOS、Linux 就是名稱本身（<c>Exe("git")</c> → git.exe／git）</summary>
    public static string Exe(string name) => OperatingSystem.IsWindows() ? name + ".exe" : name;
}
