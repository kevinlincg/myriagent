using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.RegularExpressions;
using Myriagent.Core;

namespace Myriagent.Agents;

/// <summary>
/// app 的工作物件：app 直接啟動的 CLI 程序加入後，app 結束（含當掉）時由系統一併結束。
/// 設 SILENT_BREAKAWAY_OK：一般程序（cmd、PowerShell…）再開的子程序不會被綁進來，代理替使用者開的程式（例如編輯器）不會被一起關掉。
/// 例外是 node（libuv）：它把自己與開出的子程序放進自己的巢狀工作物件，那個工作物件掛在這個底下，
/// 所以 npm 版 Codex（node codex.js → codex.exe）的 codex.exe 也在這個工作物件裡——app 結束時一起結束，PID 清單也列得出它（實測）。
/// </summary>
internal static class ChildProcessJob
{
    private const int JobObjectBasicProcessIdList = 3;
    private const int JobObjectExtendedLimitInformation = 9;
    private const uint JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000;
    private const uint JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK = 0x00001000;
    private const uint PROCESS_QUERY_LIMITED_INFORMATION = 0x1000;
    private const uint STILL_ACTIVE = 259;
    private const int ERROR_MORE_DATA = 234;

    /// <summary>JOBOBJECT_BASIC_PROCESS_ID_LIST 的表頭（兩個 DWORD）；PID 陣列（ULONG_PTR）從這裡開始，x64 與 x86 都是 8</summary>
    private const int ProcessIdListOffset = 8;

    private static readonly Lazy<IntPtr> Job = new(CreateJob, LazyThreadSafetyMode.ExecutionAndPublication);

    /// <summary>工作物件已建立（第一次 TryAssign 時才建立；在那之前沒有任何程序在裡面）</summary>
    public static bool IsCreated => Job.IsValueCreated && Job.Value != IntPtr.Zero;

    public static void TryAssign(Process process)
    {
        try
        {
            var job = Job.Value;
            if (job != IntPtr.Zero)
                AssignProcessToJobObject(job, process.Handle);
        }
        catch (Exception ex)
        {
            Debug.WriteLine($"[ChildProcessJob] 加入工作物件失敗：{ex.Message}");
        }
    }

    /// <summary>
    /// 這個程序是不是還在執行、而且在 app 的工作物件裡（含 node 開出、在巢狀工作物件裡的子程序）；已結束或打不開回 false
    /// </summary>
    public static bool Contains(int processId)
    {
        if (!IsCreated || processId <= 0)
            return false;
        var handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, (uint)processId);
        if (handle == IntPtr.Zero)
            return false;
        try
        {
            return IsStillActive(handle) && IsProcessInJob(handle, Job.Value, out var inJob) && inJob;
        }
        finally
        {
            CloseHandle(handle);
        }
    }

    /// <summary>
    /// 目前還在執行、在 app 工作物件裡的所有程序 PID（QueryInformationJobObject 的 JobObjectBasicProcessIdList，含巢狀工作物件裡的程序）。
    /// 已結束、但還有人握著它的 handle 的程序仍掛在工作物件上，這裡排除。工作物件還沒建立或查詢失敗回空清單。
    /// 熱更新用來確認某種 CLI 的程序都已結束（呼叫端再依執行檔路徑分類）
    /// </summary>
    public static IReadOnlyList<int> GetProcessIds() => ReadJobProcessIds().Where(IsStillActive).ToList();

    /// <summary>工作物件上登記的所有 PID（含已結束、handle 還沒全部關掉的）</summary>
    private static IReadOnlyList<int> ReadJobProcessIds()
    {
        if (!IsCreated)
            return Array.Empty<int>();
        int capacity = 64;
        for (int attempt = 0; attempt < 8; attempt++)
        {
            int size = ProcessIdListOffset + capacity * IntPtr.Size;
            var buffer = Marshal.AllocHGlobal(size);
            try
            {
                if (QueryInformationJobObject(Job.Value, JobObjectBasicProcessIdList, buffer, (uint)size, out _))
                {
                    int count = Marshal.ReadInt32(buffer, 4);   // NumberOfProcessIdsInList
                    var ids = new int[count];
                    for (int i = 0; i < count; i++)
                        ids[i] = (int)Marshal.ReadIntPtr(buffer, ProcessIdListOffset + i * IntPtr.Size).ToInt64();
                    return ids;
                }
                if (Marshal.GetLastWin32Error() != ERROR_MORE_DATA)
                    return Array.Empty<int>();
                // 緩衝區不夠：表頭的 NumberOfAssignedProcesses 是目前的總數（查詢期間可能又多了，多留一些）
                capacity = Math.Max(capacity * 2, Marshal.ReadInt32(buffer, 0) + 16);
            }
            finally
            {
                Marshal.FreeHGlobal(buffer);
            }
        }
        return Array.Empty<int>();
    }

    /// <summary>程序還在執行（工作物件上的 PID 在程序物件釋放前不會被重複使用，所以開到的一定是同一個程序）</summary>
    private static bool IsStillActive(int processId)
    {
        var handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, (uint)processId);
        if (handle == IntPtr.Zero)
            return false;
        try
        {
            return IsStillActive(handle);
        }
        finally
        {
            CloseHandle(handle);
        }
    }

    private static bool IsStillActive(IntPtr processHandle) => GetExitCodeProcess(processHandle, out var code) && code == STILL_ACTIVE;

    private static IntPtr CreateJob()
    {
        var job = CreateJobObject(IntPtr.Zero, null);
        if (job == IntPtr.Zero)
            return IntPtr.Zero;

        var info = new JOBOBJECT_EXTENDED_LIMIT_INFORMATION
        {
            BasicLimitInformation = new JOBOBJECT_BASIC_LIMIT_INFORMATION
            {
                LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK,
            },
        };
        int length = Marshal.SizeOf<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>();
        var buffer = Marshal.AllocHGlobal(length);
        try
        {
            Marshal.StructureToPtr(info, buffer, false);
            if (!SetInformationJobObject(job, JobObjectExtendedLimitInformation, buffer, (uint)length))
            {
                CloseHandle(job);
                return IntPtr.Zero;
            }
        }
        finally
        {
            Marshal.FreeHGlobal(buffer);
        }

        return job;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_BASIC_LIMIT_INFORMATION
    {
        public long PerProcessUserTimeLimit;
        public long PerJobUserTimeLimit;
        public uint LimitFlags;
        public UIntPtr MinimumWorkingSetSize;
        public UIntPtr MaximumWorkingSetSize;
        public uint ActiveProcessLimit;
        public UIntPtr Affinity;
        public uint PriorityClass;
        public uint SchedulingClass;
    }

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

    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_EXTENDED_LIMIT_INFORMATION
    {
        public JOBOBJECT_BASIC_LIMIT_INFORMATION BasicLimitInformation;
        public IO_COUNTERS IoInfo;
        public UIntPtr ProcessMemoryLimit;
        public UIntPtr JobMemoryLimit;
        public UIntPtr PeakProcessMemoryUsed;
        public UIntPtr PeakJobMemoryUsed;
    }

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateJobObject(IntPtr lpJobAttributes, string? lpName);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetInformationJobObject(IntPtr hJob, int infoType, IntPtr lpJobObjectInfo, uint cbJobObjectInfoLength);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool AssignProcessToJobObject(IntPtr hJob, IntPtr hProcess);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool QueryInformationJobObject(IntPtr hJob, int infoClass, IntPtr lpJobObjectInfo, uint cbJobObjectInfoLength, out uint lpReturnLength);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool IsProcessInJob(IntPtr processHandle, IntPtr jobHandle, out bool result);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr OpenProcess(uint desiredAccess, bool inheritHandle, uint processId);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetExitCodeProcess(IntPtr processHandle, out uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr hObject);
}

/// <summary>程序的執行檔路徑（QueryFullProcessImageName；不用 WMI——大量查詢曾把 WmiPrvSE 打到 400% CPU）</summary>
internal static class ProcessImage
{
    private const uint PROCESS_QUERY_LIMITED_INFORMATION = 0x1000;

    /// <summary>
    /// 程序目前的執行檔完整路徑（Win32 格式）；打不開（不存在、沒有權限）回 null。
    /// 研究實測：執行中的 exe 被改名（claude.exe → claude.exe.old.&lt;ms&gt;.&lt;pid&gt;）後，這裡回的是改名後的路徑，
    /// Process.MainModule 與 WMI 仍是舊路徑；被改名的是上層資料夾（npm 把套件退役成 .codex-&lt;hash&gt;）時，這裡也仍是原路徑
    /// </summary>
    public static string? TryGetPath(int processId)
    {
        if (processId <= 0)
            return null;
        var handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, (uint)processId);
        if (handle == IntPtr.Zero)
            return null;
        try
        {
            var buffer = new char[32768];
            uint size = (uint)buffer.Length;
            return QueryFullProcessImageName(handle, 0, buffer, ref size) ? new string(buffer, 0, (int)size) : null;
        }
        finally
        {
            CloseHandle(handle);
        }
    }

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr OpenProcess(uint desiredAccess, bool inheritHandle, uint processId);

    [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode, EntryPoint = "QueryFullProcessImageNameW")]
    private static extern bool QueryFullProcessImageName(IntPtr process, uint flags, [Out] char[] exeName, ref uint size);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr hObject);
}

/// <summary>程序表的讀法：Windows 用 NtQuerySystemInformation（macOS 的對應實作在 src/Myriagent.Mac/Agents/）</summary>
internal sealed partial class ProcessTreeSnapshot
{
    /// <summary>
    /// 目前所有程序的（PID, 父 PID）：NtQuerySystemInformation(SystemProcessInformation) 一次取（背景程式的 NativeProcessSnapshot 用同一個呼叫與欄位位移）。
    /// 對話框燈號每 3 秒重列一次（ActivityRules.ProcessTableRefreshMs）。以前用 Toolhelp32：核心裡一樣要列所有程序與執行緒，另外每個程序把 260 字的名稱轉成字串，
    /// 2026-10-25 實測 873 個程序、3.3 萬條執行緒時一次約 64 ms，這裡約 47 ms（CPU）。讀不到回空的
    /// </summary>
    internal static List<(int Id, int ParentId)> ReadProcessTable()
    {
        var table = new List<(int Id, int ParentId)>(1024);
        var size = Math.Max(Volatile.Read(ref processTableBytes), 1 << 20);
        for (var attempt = 0; attempt < 6; attempt++)
        {
            var buffer = Marshal.AllocHGlobal(size);
            try
            {
                var status = NtQuerySystemInformation(SystemProcessInformation, buffer, size, out var needed);
                if (status == StatusInfoLengthMismatch)
                {
                    size = Math.Max(size * 2, needed + (256 << 10));
                    continue;
                }
                if (status != 0)
                    return table;
                // 下次直接配夠大（程序與執行緒會變多一點）
                Volatile.Write(ref processTableBytes, needed + (256 << 10));
                var offset = 0L;
                while (true)
                {
                    // SYSTEM_PROCESS_INFORMATION（x64）：0x00 NextEntryOffset、0x50 UniqueProcessId、0x58 InheritedFromUniqueProcessId
                    var entry = buffer + (nint)offset;
                    var next = Marshal.ReadInt32(entry, 0x00);
                    table.Add(((int)Marshal.ReadInt64(entry, 0x50), (int)Marshal.ReadInt64(entry, 0x58)));
                    if (next == 0)
                        break;
                    offset += next;
                }
                return table;
            }
            finally
            {
                Marshal.FreeHGlobal(buffer);
            }
        }
        return table;
    }

    /// <summary>上次列程序表用到的緩衝區大小（下次照它配，不必先試一次太小的）</summary>
    private static int processTableBytes;

    private const int SystemProcessInformation = 5;
    private const int StatusInfoLengthMismatch = unchecked((int)0xC0000004);

    [DllImport("ntdll.dll")]
    private static extern int NtQuerySystemInformation(int infoClass, IntPtr info, int length, out int returnLength);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr hObject);
}
