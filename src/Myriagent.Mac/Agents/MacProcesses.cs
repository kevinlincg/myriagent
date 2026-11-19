using System.Collections.Concurrent;
using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;

namespace Myriagent.Agents;

/// <summary>macOS 的程序資訊（libproc）：程序表與執行檔路徑</summary>
internal static class MacProcessTable
{
    /// <summary>PROC_PIDT_SHORTBSDINFO：任何使用者的程序都查得到的精簡資訊（PID 在位移 0、父 PID 在位移 4，共 64 位元組）</summary>
    private const int ShortInfoFlavor = 13;
    private const int ShortInfoSize = 64;
    private const int MaxPathSize = 4096;

    /// <summary>目前所有程序的（PID, 父 PID）；讀不到回空的</summary>
    public static List<(int Id, int ParentId)> Read()
    {
        var table = new List<(int Id, int ParentId)>(1024);
        int estimate = proc_listallpids(null, 0);
        if (estimate <= 0)
            return table;
        // 列的當下程序還會變多：多留一些
        var pids = new int[estimate + 256];
        int count = proc_listallpids(pids, pids.Length * sizeof(int));
        var info = new byte[ShortInfoSize];
        for (int i = 0; i < count && i < pids.Length; i++)
        {
            if (pids[i] <= 0)
                continue;
            if (proc_pidinfo(pids[i], ShortInfoFlavor, 0, info, ShortInfoSize) == ShortInfoSize)
                table.Add((BitConverter.ToInt32(info, 0), BitConverter.ToInt32(info, 4)));
        }
        return table;
    }

    /// <summary>程序的執行檔完整路徑；不存在、沒有權限回 null</summary>
    public static string? PathOf(int processId)
    {
        if (processId <= 0)
            return null;
        var buffer = new byte[MaxPathSize];
        int length = proc_pidpath(processId, buffer, (uint)buffer.Length);
        return length <= 0 ? null : Encoding.UTF8.GetString(buffer, 0, length);
    }

    [DllImport("libproc.dylib")]
    private static extern int proc_listallpids([Out] int[]? buffer, int bufferSize);

    [DllImport("libproc.dylib")]
    private static extern int proc_pidinfo(int pid, int flavor, ulong arg, [Out] byte[] buffer, int bufferSize);

    [DllImport("libproc.dylib")]
    private static extern int proc_pidpath(int pid, [Out] byte[] buffer, uint bufferSize);
}

/// <summary>
/// app 直接啟動的 CLI 程序，macOS 版（Windows 版把它們放進工作物件，見 src/Myriagent/Agents/AgentProcess.Win.cs；對外的成員相同）。
/// macOS 沒有工作物件：這裡只記下 app 啟動了哪些程序，回答「這個程序是不是 app 的」「app 的程序有哪些」。
/// 和 Windows 版一樣，一般程序再開的子程序不算（代理替使用者開的程式、背景程式要分得出來）；node 開的子程序算
/// （npm 版 Codex 是 node 再啟動真正的 codex）。
/// app 當掉時這些程序不會被系統一起結束——它們的標準輸入跟著關閉，CLI 讀到結尾會自己結束
/// </summary>
internal static class ChildProcessJob
{
    /// <summary>app 啟動的程序：PID → 建立時間（PID 會被重複使用，兩者都相同才是同一個）</summary>
    private static readonly ConcurrentDictionary<int, long> Assigned = new();

    public static bool IsCreated => !Assigned.IsEmpty;

    public static void TryAssign(Process process)
    {
        try
        {
            Assigned[process.Id] = process.StartTime.ToUniversalTime().Ticks;
        }
        catch (Exception ex) when (ex is InvalidOperationException or Win32Exception or NotSupportedException)
        {
            Debug.WriteLine($"[ChildProcessJob] 記不下啟動的程序：{ex.Message}");
        }
    }

    /// <summary>這個程序是不是還在執行、而且是 app 啟動的（或 app 啟動的 node 開的子程序）</summary>
    public static bool Contains(int processId) => processId > 0 && GetProcessIds().Contains(processId);

    /// <summary>目前還在執行、app 啟動的程序（加上其中 node 開的子程序）；已結束的順便忘掉</summary>
    public static IReadOnlyList<int> GetProcessIds()
    {
        var roots = new List<int>();
        foreach (var (pid, ticks) in Assigned)
        {
            if (IsAlive(pid, ticks))
                roots.Add(pid);
            else
                Assigned.TryRemove(new KeyValuePair<int, long>(pid, ticks));
        }
        var nodes = roots.Where(IsNode).ToHashSet();
        if (nodes.Count == 0)
            return roots;
        var result = new HashSet<int>(roots);
        foreach (var (id, parent) in MacProcessTable.Read())
        {
            if (nodes.Contains(parent))
                result.Add(id);
        }
        return result.ToList();
    }

    private static bool IsNode(int processId)
        => string.Equals(Path.GetFileName(MacProcessTable.PathOf(processId)), "node", StringComparison.Ordinal);

    private static bool IsAlive(int processId, long startTicks)
    {
        try
        {
            using var process = Process.GetProcessById(processId);
            return !process.HasExited && process.StartTime.ToUniversalTime().Ticks == startTicks;
        }
        catch (Exception ex) when (ex is ArgumentException or InvalidOperationException or Win32Exception or NotSupportedException)
        {
            return false;
        }
    }
}

/// <summary>程序的執行檔路徑，macOS 版（proc_pidpath）</summary>
internal static class ProcessImage
{
    public static string? TryGetPath(int processId) => MacProcessTable.PathOf(processId);
}

/// <summary>程序表的讀法：macOS 用 libproc（Windows 的對應實作在 AgentProcess.Win.cs）</summary>
internal sealed partial class ProcessTreeSnapshot
{
    internal static List<(int Id, int ParentId)> ReadProcessTable() => MacProcessTable.Read();
}
