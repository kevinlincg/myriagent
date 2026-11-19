using System.ComponentModel;
using System.Diagnostics;
using Myriagent.Agents;

namespace Myriagent.Background;

/// <summary>
/// 背景程式的程序操作，macOS 版（Windows 的對應實作在 src/Myriagent/Background/NativeProgramControl.Win.cs）。
/// 程序以 PID＋建立時間認（建立時間是 FILETIME 刻度，和 NativeProcessSnapshot 給的同一個值）。
/// macOS 讀不到別人啟動的程序的結束碼（不是自己的子程序）：結束碼與結束時間一律回 null，畫面上只會說它結束了
/// </summary>
internal sealed class NativeProgramControl : IProgramProcessControl
{
    private readonly object gate = new();
    private readonly HashSet<(int Pid, long CreatedAt)> watched = new();

    public bool Watch(int pid, long createdAt)
    {
        if (!IsRunning(pid, createdAt))
            return false;
        lock (gate)
            watched.Add((pid, createdAt));
        return true;
    }

    public bool IsAlive(int pid, long createdAt)
    {
        lock (gate)
        {
            if (!watched.Contains((pid, createdAt)))
                return true;
        }
        return IsRunning(pid, createdAt);
    }

    public (int? ExitCode, long? ExitedAt) Release(int pid, long createdAt)
    {
        lock (gate)
            watched.Remove((pid, createdAt));
        return (null, null);
    }

    public bool KillTree(int pid, long createdAt)
    {
        if (!IsRunning(pid, createdAt))
            return false;
        try
        {
            // 先擷取樹（根還活著，子孫才接得上），再結束根與子孫
            var tree = ProcessTreeSnapshot.Capture(pid);
            using var process = Process.GetProcessById(pid);
            process.Kill();
            tree.KillAlive();
            return true;
        }
        catch (Exception ex) when (ex is ArgumentException or InvalidOperationException or Win32Exception or NotSupportedException)
        {
            Debug.WriteLine($"[NativeProgramControl] 結束程序 {pid} 失敗：{ex.Message}");
            return false;
        }
    }

    public void Dispose()
    {
        lock (gate)
            watched.Clear();
    }

    /// <summary>那個 PID 現在還是同一個程序（建立時間相同）而且還在執行</summary>
    private static bool IsRunning(int pid, long createdAt)
        => MacProcessTable.StartTicks(pid) is { } ticks && new DateTime(ticks, DateTimeKind.Utc).ToFileTimeUtc() == createdAt;
}
