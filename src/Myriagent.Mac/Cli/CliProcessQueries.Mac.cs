using System.Runtime.InteropServices;
using System.Text;

namespace Myriagent.Cli;

/// <summary>讀別的程序命令列的 macOS 做法（sysctl 的 KERN_PROCARGS2；Windows 的對應實作在 src/Myriagent/Cli/CliProcessQueries.Win.cs）</summary>
internal static partial class CliProcessQueries
{
    private const int CtlKern = 1;
    private const int KernProcArgs2 = 49;
    private const int MaxCommandLineBytes = 1 << 20;

    /// <summary>
    /// 程序的命令列（各參數以空白接起來；只用來找關鍵字，不保證能原樣拆回去）。只讀得到自己這個使用者的程序；
    /// 已結束、沒有權限、讀不到回 null
    /// </summary>
    internal static string? TryGetCommandLine(int processId)
    {
        if (processId <= 0)
            return null;
        var name = new[] { CtlKern, KernProcArgs2, processId };
        nuint length = 0;
        if (sysctl(name, (uint)name.Length, null, ref length, IntPtr.Zero, 0) != 0 || length == 0 || length > MaxCommandLineBytes)
            return null;
        var buffer = new byte[(int)length];
        if (sysctl(name, (uint)name.Length, buffer, ref length, IntPtr.Zero, 0) != 0 || length < sizeof(int))
            return null;
        return ParseProcArgs(buffer.AsSpan(0, (int)length));
    }

    /// <summary>
    /// 純函式：KERN_PROCARGS2 的內容 → 命令列。格式：參數個數（int）、執行檔路徑、一個以上的 0、接著每個參數各以 0 結尾（之後是環境變數，不讀）
    /// </summary>
    internal static string? ParseProcArgs(ReadOnlySpan<byte> data)
    {
        if (data.Length < sizeof(int))
            return null;
        int count = BitConverter.ToInt32(data[..sizeof(int)]);
        if (count <= 0)
            return string.Empty;
        int position = sizeof(int);
        // 執行檔路徑
        while (position < data.Length && data[position] != 0)
            position++;
        // 路徑後面補齊用的 0
        while (position < data.Length && data[position] == 0)
            position++;
        var arguments = new List<string>(count);
        while (arguments.Count < count && position < data.Length)
        {
            int end = position;
            while (end < data.Length && data[end] != 0)
                end++;
            arguments.Add(Encoding.UTF8.GetString(data[position..end]));
            position = end + 1;
        }
        return string.Join(' ', arguments);
    }

    [DllImport("libc", SetLastError = true)]
    private static extern int sysctl(int[] name, uint nameLength, [Out] byte[]? oldValue, ref nuint oldLength, IntPtr newValue, nuint newLength);
}
