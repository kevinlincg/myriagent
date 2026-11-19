namespace Myriagent.Update;

/// <summary>
/// 受管理安裝的 <c>current</c> 指向目前版本的資料夾，macOS 版（Windows 版用目錄連接點，見 src/Myriagent/Update/Junction.cs；對外的成員相同）。
/// macOS 用符號連結；改指向時先建一個暫時的連結再改名蓋過去（改名是不可分割的，不會有「current 不見了」的空窗）
/// </summary>
internal static class Junction
{
    public static void Create(string link, string target)
    {
        Directory.CreateDirectory(Path.GetDirectoryName(Path.TrimEndingDirectorySeparator(link))!);
        Directory.CreateSymbolicLink(Path.TrimEndingDirectorySeparator(link), Path.TrimEndingDirectorySeparator(target));
    }

    public static void Retarget(string link, string target)
    {
        link = Path.TrimEndingDirectorySeparator(link);
        var temp = link + ".new-" + Guid.NewGuid().ToString("N")[..8];
        Directory.CreateSymbolicLink(temp, Path.TrimEndingDirectorySeparator(target));
        try
        {
            // 直接呼叫系統的 rename：兩邊都是指向資料夾的符號連結，File.Move 不認這種來源
            if (rename(temp, link) != 0)
                throw new IOException($"改不了 {link} 指向的版本（errno {System.Runtime.InteropServices.Marshal.GetLastPInvokeError()}）");
        }
        catch
        {
            try
            {
                File.Delete(temp);
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
            }
            throw;
        }
    }

    [System.Runtime.InteropServices.DllImport("libc", SetLastError = true)]
    private static extern int rename([System.Runtime.InteropServices.MarshalAs(System.Runtime.InteropServices.UnmanagedType.LPUTF8Str)] string from,
        [System.Runtime.InteropServices.MarshalAs(System.Runtime.InteropServices.UnmanagedType.LPUTF8Str)] string to);

    public static bool IsJunction(string path) => GetTarget(path) != null;

    /// <summary>連結指向的資料夾（完整路徑）；不是連結、不存在或讀不到回 null</summary>
    public static string? GetTarget(string link)
    {
        try
        {
            link = Path.TrimEndingDirectorySeparator(link);
            var info = new DirectoryInfo(link);
            if (info.LinkTarget is not { } target)
                return null;
            return Path.GetFullPath(target, Path.GetDirectoryName(link) ?? "/");
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
        {
            return null;
        }
    }
}
