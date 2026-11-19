using System.Collections.Concurrent;
using System.Runtime.InteropServices;
using Myriagent.Agents;

namespace Myriagent.Files;

/// <summary>檔名自然排序的 Windows 做法（和檔案總管同一個函式）；macOS 的對應實作在 src/Myriagent.Mac/Files/FileBrowser.Mac.cs</summary>
internal static partial class FileBrowser
{
    /// <summary>自然排序（檔名裡的數字依數值比：img2 在 img10 前面）</summary>
    public static int NaturalCompare(string? a, string? b) => StrCmpLogicalW(a ?? string.Empty, b ?? string.Empty);

    [DllImport("shlwapi.dll", CharSet = CharSet.Unicode, ExactSpelling = true)]
    private static extern int StrCmpLogicalW(string x, string y);
}
