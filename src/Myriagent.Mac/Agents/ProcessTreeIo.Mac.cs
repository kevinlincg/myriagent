using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;

namespace Myriagent.Agents;

/// <summary>
/// 讀一個程序的 CPU 時間、硬碟讀寫量與記憶體的 macOS 做法（proc_pid_rusage；Windows 的對應實作在 src/Myriagent/Agents/ProcessTreeIo.Win.cs）。
/// 和 Windows 不同的地方：這裡的讀寫量只有硬碟（Windows 的 I/O 計數器連管線一起算）
/// </summary>
internal sealed partial class ProcessTreeIo
{
    /// <summary>RUSAGE_INFO_V2 與 rusage_info_v2 的大小、欄位位移：2026-11-19 在 macOS 26.5（x86_64）以 C 的 offsetof 實測</summary>
    private const int RusageInfoV2 = 2;
    private const int RusageSize = 160;
    private const int UserTimeOffset = 16;
    private const int SystemTimeOffset = 24;
    private const int PhysicalFootprintOffset = 72;
    private const int DiskReadOffset = 144;
    private const int DiskWriteOffset = 152;

    /// <summary>ReadProcess 的讀寫量含不含管線：macOS 的 rusage 只算硬碟，中樞不必再扣 app 與 CLI 之間的管線</summary>
    public static bool IoIncludesPipes => false;

    /// <summary>rusage 的 CPU 時間是系統的計時單位（Intel 是 1 奈秒；Apple 晶片是 125/3 奈秒）：這個分數把它換成奈秒</summary>
    private static readonly (uint Numerator, uint Denominator) Timebase = ReadTimebase();

    /// <summary>
    /// 一個程序的啟動時間（UTC 的 DateTime ticks，和 ProcessTreeSnapshot 同一個來源：.NET 的 Process.StartTime）、硬碟讀＋寫的位元組、
    /// 用掉的 CPU 時間（核心＋使用者，100 奈秒為單位）、記憶體（實體記憶體用量，同「活動監視器」的「記憶體」欄）；
    /// 已結束、不存在、不是自己這個使用者的程序回 null
    /// </summary>
    internal static (long Start, ulong Io, ulong Cpu, ulong? Mem)? ReadProcess(int processId)
    {
        if (processId <= 0 || MacProcessTable.StartTicks(processId) is not { } start)
            return null;
        var usage = new byte[RusageSize];
        if (proc_pid_rusage(processId, RusageInfoV2, usage) != 0)
            return null;
        ulong cpuUnits = BitConverter.ToUInt64(usage, UserTimeOffset) + BitConverter.ToUInt64(usage, SystemTimeOffset);
        ulong cpu = (ulong)((UInt128)cpuUnits * Timebase.Numerator / Timebase.Denominator / 100);
        ulong io = BitConverter.ToUInt64(usage, DiskReadOffset) + BitConverter.ToUInt64(usage, DiskWriteOffset);
        return (start, io, cpu, BitConverter.ToUInt64(usage, PhysicalFootprintOffset));
    }

    private static (uint, uint) ReadTimebase()
    {
        var info = new uint[2];
        return mach_timebase_info(info) == 0 && info[0] != 0 && info[1] != 0 ? (info[0], info[1]) : (1u, 1u);
    }

    [DllImport("libproc.dylib")]
    private static extern int proc_pid_rusage(int pid, int flavor, [Out] byte[] buffer);

    [DllImport("libc")]
    private static extern int mach_timebase_info([Out] uint[] info);
}
