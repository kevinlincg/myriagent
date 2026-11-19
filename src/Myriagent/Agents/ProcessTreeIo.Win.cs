using System.Runtime.InteropServices;
using Myriagent.Background;
using Myriagent.Hub;
using Myriagent.SystemStatus;

namespace Myriagent.Agents;

/// <summary>讀一個程序的 CPU 時間、I/O 量與記憶體的 Windows 做法；macOS 的對應實作在 src/Myriagent.Mac/Agents/ProcessTreeIo.Mac.cs</summary>
internal sealed partial class ProcessTreeIo
{
    private const uint ProcessQueryLimitedInformation = 0x1000;

    /// <summary>ReadProcess 的讀寫量含不含管線（Windows 的 I/O 計數器連管線一起算，所以中樞要扣掉 app 與 CLI 之間的那一段）</summary>
    public static bool IoIncludesPipes => true;

    /// <summary>
    /// 一個程序的啟動時間（UTC 的 DateTime ticks，同 ProcessTreeSnapshot）、讀＋寫的位元組、用掉的 CPU 時間（核心＋使用者，100 奈秒為單位）、
    /// 記憶體（私人工作集的位元組，同工作管理員的「記憶體」欄；讀不到時 0；程序已經結束、只是 handle 還有人握著時 null——
    /// 那種程序照樣讀得到 CPU 與 I/O，也還剩幾十 KB 的工作集，2026-10-26 測試時被算進記憶體）；開不了（已結束、沒有權限）回 null
    /// </summary>
    internal static (long Start, ulong Io, ulong Cpu, ulong? Mem)? ReadProcess(int processId)
    {
        if (processId <= 0)
            return null;
        var handle = OpenProcess(ProcessQueryLimitedInformation, false, (uint)processId);
        if (handle == IntPtr.Zero)
            return null;
        try
        {
            if (!GetProcessTimes(handle, out var creation, out _, out var kernel, out var user) || !GetProcessIoCounters(handle, out var io))
                return null;
            var running = GetExitCodeProcess(handle, out var exitCode) && exitCode == StillActive;
            return (DateTime.FromFileTimeUtc(creation).Ticks, io.ReadTransferCount + io.WriteTransferCount, (ulong)Math.Max(0, kernel) + (ulong)Math.Max(0, user),
                running ? ReadMemory(handle) : null);
        }
        catch (ArgumentOutOfRangeException)
        {
            return null;
        }
        finally
        {
            CloseHandle(handle);
        }
    }

    /// <summary>
    /// 私人工作集（PROCESS_MEMORY_COUNTERS_EX2.PrivateWorkingSetSize：Windows 11／Server 2022 起，同一個只能查詢的 handle 就讀得到，2026-10-26 實測）；
    /// 舊的 Windows 不認這個大小時退回私人位元組（PrivateUsage，認可的私人記憶體，通常比工作集大一些）；都讀不到 0
    /// </summary>
    private static ulong ReadMemory(IntPtr handle)
    {
        var counters = new PROCESS_MEMORY_COUNTERS_EX2 { cb = (uint)Marshal.SizeOf<PROCESS_MEMORY_COUNTERS_EX2>() };
        if (K32GetProcessMemoryInfo(handle, ref counters, counters.cb))
            return (ulong)counters.PrivateWorkingSetSize;
        counters.cb = (uint)Marshal.OffsetOf<PROCESS_MEMORY_COUNTERS_EX2>(nameof(PROCESS_MEMORY_COUNTERS_EX2.PrivateWorkingSetSize));
        return K32GetProcessMemoryInfo(handle, ref counters, counters.cb) ? (ulong)counters.PrivateUsage : 0;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct PROCESS_MEMORY_COUNTERS_EX2
    {
        public uint cb;
        public uint PageFaultCount;
        public nuint PeakWorkingSetSize;
        public nuint WorkingSetSize;
        public nuint QuotaPeakPagedPoolUsage;
        public nuint QuotaPagedPoolUsage;
        public nuint QuotaPeakNonPagedPoolUsage;
        public nuint QuotaNonPagedPoolUsage;
        public nuint PagefileUsage;
        public nuint PeakPagefileUsage;
        public nuint PrivateUsage;
        public nuint PrivateWorkingSetSize;
        public ulong SharedCommitUsage;
    }

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool K32GetProcessMemoryInfo(IntPtr process, ref PROCESS_MEMORY_COUNTERS_EX2 counters, uint cb);

    /// <summary>GetExitCodeProcess 對還在跑的程序回的值</summary>
    private const uint StillActive = 259;

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetExitCodeProcess(IntPtr process, out uint exitCode);

    [StructLayout(LayoutKind.Sequential)]
    private struct IO_COUNTERS
    {
        public ulong ReadOperationCount;
        public ulong WriteOperationCount;
        public ulong OtherOperationCount;
        public ulong ReadTransferCount;
        public ulong WriteTransferCount;
        public ulong OtherTransferCount;
    }

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr OpenProcess(uint desiredAccess, bool inheritHandle, uint processId);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetProcessTimes(IntPtr process, out long creationTime, out long exitTime, out long kernelTime, out long userTime);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetProcessIoCounters(IntPtr process, out IO_COUNTERS counters);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr handle);
}
