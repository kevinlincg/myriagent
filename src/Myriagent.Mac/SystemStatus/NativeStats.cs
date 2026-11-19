using System.Runtime.InteropServices;
using System.Text;
using Myriagent.Core;
using Myriagent.Sync;

namespace Myriagent.SystemStatus;

/// <summary>
/// 這台電腦的讀值，macOS 版（Windows 版見 src/Myriagent/SystemStatus/NativeStats.cs；對外的成員相同）：
/// CPU 的累計時間（host_statistics 的 HOST_CPU_LOAD_INFO）、實體記憶體（hw.memsize 與 host_statistics64 的分頁統計）、
/// CPU 名稱（machdep.cpu.brand_string）。顯示卡使用率與 CPU 溫度 macOS 沒有公開的讀法，先不提供（面板上那兩格不顯示）
/// </summary>
internal static class NativeStats
{
    private const int HostCpuLoadInfo = 3;
    private const int HostCpuLoadInfoCount = 4;
    private const int HostVmInfo64 = 4;

    /// <summary>vm_statistics64 的大小（以 32 位元整數計；系統多回的欄位不讀）</summary>
    private const int HostVmInfo64Count = 38;

    // vm_statistics64 裡各欄位的位置（以 32 位元整數為單位；64 位元的欄位佔兩格，見 <mach/vm_statistics.h>）
    private const int VmWireCount = 3;
    private const int VmPurgeableCount = 22;
    private const int VmCompressorPageCount = 32;
    private const int VmInternalPageCount = 35;

    /// <summary>這台的取樣器（建構時讀一次累計值，第一次取樣是建構到那時的平均）</summary>
    public static DeviceStatsSampler CreateSampler()
        => new(ReadCpuTimes, ReadMemory, ReadCpuName(), Log.Warn, readGpus: null, readCpuTemp: null, ReadNetwork);

    private static (long Rx, long Tx)? ReadNetwork()
        => NetworkTotals.Read() is { } totals ? ((long)Math.Min(totals.Rx, long.MaxValue), (long)Math.Min(totals.Tx, long.MaxValue)) : null;

    /// <summary>
    /// 所有核心加總的累計時間（單位是系統的 tick，只拿來算比例）。Windows 的 kernel 時間含閒置（CpuTimes 照那個定義，
    /// DeviceStatsRules.CpuPercent 以 kernel＋user 當總量）：這裡把閒置加進 Kernel，nice 算在 User
    /// </summary>
    public static CpuTimes? ReadCpuTimes()
    {
        var ticks = new uint[HostCpuLoadInfoCount];
        int count = HostCpuLoadInfoCount;
        if (host_statistics(mach_host_self(), HostCpuLoadInfo, ticks, ref count) != 0 || count < HostCpuLoadInfoCount)
            return null;
        // 順序：user、system、idle、nice
        ulong user = ticks[0], system = ticks[1], idle = ticks[2], nice = ticks[3];
        return new CpuTimes(idle, system + idle, user + nice);
    }

    /// <summary>已用的實體記憶體：和「活動監視器」的「已使用的記憶體」同一個算法（app 記憶體＋系統核心保留＋已壓縮）</summary>
    public static (long Used, long Total)? ReadMemory()
    {
        if (ReadSysctlUInt64("hw.memsize") is not { } totalBytes || totalBytes == 0)
            return null;
        var pages = new uint[HostVmInfo64Count];
        int count = HostVmInfo64Count;
        if (host_statistics64(mach_host_self(), HostVmInfo64, pages, ref count) != 0 || count <= VmInternalPageCount)
            return null;
        long app = Math.Max(0, (long)pages[VmInternalPageCount] - pages[VmPurgeableCount]);
        long usedPages = app + pages[VmWireCount] + pages[VmCompressorPageCount];
        var total = (long)Math.Min(totalBytes, (ulong)DeviceStatsRules.MaxBytes);
        var used = Math.Clamp(usedPages * Environment.SystemPageSize, 0, total);
        return (used, total);
    }

    public static string? ReadCpuName() => ReadSysctlString("machdep.cpu.brand_string");

    private static ulong? ReadSysctlUInt64(string name)
    {
        ulong value = 0;
        nuint size = sizeof(ulong);
        return sysctlbyname(name, ref value, ref size, IntPtr.Zero, 0) == 0 ? value : null;
    }

    private static string? ReadSysctlString(string name)
    {
        nuint size = 0;
        if (sysctlbyname(name, IntPtr.Zero, ref size, IntPtr.Zero, 0) != 0 || size == 0 || size > 1024)
            return null;
        var buffer = new byte[(int)size];
        if (sysctlbyname(name, buffer, ref size, IntPtr.Zero, 0) != 0)
            return null;
        var text = Encoding.UTF8.GetString(buffer, 0, (int)size).TrimEnd('\0').Trim();
        return text.Length == 0 ? null : text;
    }

    [DllImport("libSystem.dylib")]
    private static extern uint mach_host_self();

    [DllImport("libSystem.dylib")]
    private static extern int host_statistics(uint host, int flavor, [Out] uint[] info, ref int count);

    [DllImport("libSystem.dylib")]
    private static extern int host_statistics64(uint host, int flavor, [Out] uint[] info, ref int count);

    [DllImport("libSystem.dylib")]
    private static extern int sysctlbyname([MarshalAs(UnmanagedType.LPStr)] string name, ref ulong value, ref nuint size, IntPtr newValue, nuint newSize);

    [DllImport("libSystem.dylib")]
    private static extern int sysctlbyname([MarshalAs(UnmanagedType.LPStr)] string name, IntPtr value, ref nuint size, IntPtr newValue, nuint newSize);

    [DllImport("libSystem.dylib")]
    private static extern int sysctlbyname([MarshalAs(UnmanagedType.LPStr)] string name, [Out] byte[] value, ref nuint size, IntPtr newValue, nuint newSize);
}
