using System.Runtime.InteropServices;
using System.Text;
using System.Text.RegularExpressions;
using Myriagent.Agents;

namespace Myriagent.Background;

/// <summary>
/// 列出目前的程序，macOS 版（背景程式的偵測與對話的 CPU 用量；Windows 的對應實作在 src/Myriagent/Background/NativeProcessSnapshot.Win.cs）：
/// 一次 sysctl 拿到自己這個使用者所有程序的 PID、父 PID、啟動時間與名稱，每個程序第一次出現時讀一次它的環境變數裡的對話標記
/// （sysctl 的 KERN_PROCARGS2），帶標記的再記下命令列與是不是視窗程式（執行檔在 .app 裡）。一律只讀。
/// <para>
/// macOS 不讓人讀系統內建程式（/bin/sleep、/bin/bash、/usr/bin 底下的工具）的環境變數（2026-11-19 實測：自己裝的 node 讀得到，
/// /bin/sleep 讀到的是空的）。代理用 nohup 開的 shell 迴圈正是這一類，所以讀不到環境變數的程序改用推斷的（<see cref="Attribute"/>）：
/// 先確認它是這個 app 開出來的（系統記的「負責的程序」是這個 app，脫離父程序之後也不會變），再依序看——父程序鏈上有沒有帶標記的、
/// 它的工作目錄是不是某個對話自己的資料夾（路徑裡有對話 id）、有沒有哪個對話的 CLI 的工作目錄包含它的。都對不上就不算。
/// app 重開之後，在使用者自選的資料夾裡跑的這類程式認不回來（負責的程序已經不是現在這個 app）
/// </para>
/// </summary>
internal sealed partial class NativeProcessSnapshot : IProcessSnapshotSource
{
    private const int CtlKern = 1;
    private const int KernProcArgs2 = 49;
    private const int MaxArgumentBytes = 1 << 20;

    /// <summary>PROC_PIDVNODEPATHINFO 與 proc_vnodepathinfo 的大小、工作目錄路徑的位移：2026-11-19 在 macOS 26.5（x86_64）以 C 實測</summary>
    private const int VnodePathInfoFlavor = 9;
    private const int VnodePathInfoSize = 2352;
    private const int WorkingDirectoryOffset = 152;
    private const int MaxPath = 1024;

    /// <summary>往上找父程序最多幾層（防環）</summary>
    private const int MaxDepth = 64;

    /// <summary>剛建立的程序可能還沒換成要執行的程式（fork 之後、exec 之前）：這段時間內認不出來就下一次再看</summary>
    private static readonly long RetryWindowTicks = TimeSpan.FromSeconds(10).Ticks;

    private sealed class Entry
    {
        public long CreatedAt;
        public string Name = string.Empty;
        public string? Marker;
        public bool? IsConsole;
        public string? CommandLine;
        public string? WorkingDirectory;
        public bool WorkingDirectoryRead;

        /// <summary>環境變數讀到的是空的（系統內建的程式）：標記要用推斷的</summary>
        public bool EnvironmentHidden;
        public bool Done;
    }

    private readonly Dictionary<(int Pid, long Seconds, int Microseconds), Entry> cache = new();
    private readonly int ownPid = Environment.ProcessId;

    public IReadOnlyList<ProcessRow> Snapshot()
    {
        var raw = MacProcessTable.ReadOwn();
        var live = new List<(int Pid, int ParentPid, Entry Entry)>(raw.Count);
        var seen = new HashSet<(int, long, int)>();
        var now = DateTime.UtcNow.ToFileTimeUtc();
        foreach (var process in raw)
        {
            var key = (process.Pid, process.StartSeconds, process.StartMicroseconds);
            if (!cache.TryGetValue(key, out var entry))
            {
                // 建立時間和別處（ProcessTreeSnapshot、ProcessTreeIo）用同一個來源，之後才對得上；讀不到（剛結束）就不列
                if (MacProcessTable.StartTicks(process.Pid) is not { } ticks)
                    continue;
                cache[key] = entry = new Entry { CreatedAt = new DateTime(ticks, DateTimeKind.Utc).ToFileTimeUtc(), Name = process.Name };
            }
            seen.Add(key);
            if (!entry.Done && !entry.EnvironmentHidden)
                Inspect(process.Pid, entry, now - entry.CreatedAt > RetryWindowTicks);
            live.Add((process.Pid, process.ParentPid, entry));
        }
        foreach (var gone in cache.Keys.Where(k => !seen.Contains(k)).ToList())
            cache.Remove(gone);

        // 讀不到環境變數的：等這一輪所有程序的標記都讀完再推斷（要看父程序與各對話的 CLI）
        if (live.Any(p => p.Entry.EnvironmentHidden && !p.Entry.Done))
        {
            var byPid = new Dictionary<int, (int ParentPid, Entry Entry)>();
            foreach (var p in live)
                byPid.TryAdd(p.Pid, (p.ParentPid, p.Entry));
            foreach (var p in live)
            {
                if (p.Entry.EnvironmentHidden && !p.Entry.Done)
                    Attribute(p.Pid, p.ParentPid, p.Entry, byPid, now - p.Entry.CreatedAt > RetryWindowTicks);
            }
        }

        var rows = new List<ProcessRow>(live.Count);
        foreach (var (pid, parentPid, entry) in live)
            rows.Add(new ProcessRow(pid, parentPid, entry.CreatedAt, entry.Name, entry.Marker, entry.IsConsole, entry.CommandLine));
        return rows;
    }

    /// <summary>讀標記；有標記再記命令列、完整的名稱與是不是視窗程式。沒有標記而且程序還很新時不記成完成（下一次再讀）</summary>
    private void Inspect(int pid, Entry entry, bool settled)
    {
        var data = ReadArguments(pid);
        if (data == null)
        {
            // 別的權限的程序、已經結束：不是這個 app 的代理開的
            entry.Done = true;
            return;
        }
        var (arguments, environment) = ParseArguments(data);
        entry.CommandLine = string.Join(' ', arguments.Select(Quote));
        if (environment.Length == 0)
        {
            // 系統內建的程式：環境變數不給讀，留給 Attribute 推斷
            entry.EnvironmentHidden = true;
            return;
        }
        entry.Marker = BackgroundProgramRules.MarkerOf(environment);
        if (entry.Marker is null)
        {
            entry.CommandLine = null;
            entry.Done = settled;
            return;
        }
        Describe(pid, entry);
        entry.Done = true;
    }

    /// <summary>讀不到環境變數的程序屬於哪個對話（見類別說明）；認不出來、而且程序已經不新了就不再看</summary>
    private void Attribute(int pid, int parentPid, Entry entry, Dictionary<int, (int ParentPid, Entry Entry)> byPid, bool settled)
    {
        if (responsibility_get_pid_responsible_for_pid(pid) != ownPid)
        {
            entry.CommandLine = null;
            entry.Done = true;
            return;
        }
        var marker = MarkerOfAncestors(parentPid, entry.CreatedAt, byPid) ?? MarkerOfWorkingDirectory(pid, entry, byPid);
        if (marker is null)
        {
            if (settled)
            {
                entry.CommandLine = null;
                entry.Done = true;
            }
            return;
        }
        entry.Marker = marker;
        Describe(pid, entry);
        entry.Done = true;
    }

    /// <summary>活著的父程序鏈上第一個帶標記的（父程序要建立得比子程序早，PID 被重複使用時不會誤接）</summary>
    private static string? MarkerOfAncestors(int parentPid, long createdAt, Dictionary<int, (int ParentPid, Entry Entry)> byPid)
    {
        long childCreatedAt = createdAt;
        int current = parentPid;
        for (int depth = 0; depth < MaxDepth && current > 1; depth++)
        {
            if (!byPid.TryGetValue(current, out var parent) || parent.Entry.CreatedAt > childCreatedAt)
                return null;
            if (parent.Entry.Marker is { } marker)
                return marker;
            childCreatedAt = parent.Entry.CreatedAt;
            current = parent.ParentPid;
        }
        return null;
    }

    /// <summary>
    /// 依工作目錄：在某個對話自己的資料夾裡（…/conversations/〈對話 id〉/…）就是那個對話；否則找工作目錄包含它的 CLI
    /// （這個 app 直接啟動、帶標記的程序），不只一個對話時取比它早啟動的裡面最晚的那個
    /// </summary>
    private string? MarkerOfWorkingDirectory(int pid, Entry entry, Dictionary<int, (int ParentPid, Entry Entry)> byPid)
    {
        var directory = WorkingDirectoryOf(pid, entry);
        if (directory is null)
            return null;
        if (ConversationFolder().Match(directory) is { Success: true } match && BackgroundProgramRules.IsMarker(match.Groups[1].Value))
            return match.Groups[1].Value;
        string? best = null;
        long bestCreatedAt = long.MinValue;
        foreach (var (cliPid, (cliParent, cli)) in byPid)
        {
            if (cliParent != ownPid || cli.Marker is null || cli.CreatedAt > entry.CreatedAt || cli.CreatedAt <= bestCreatedAt)
                continue;
            if (WorkingDirectoryOf(cliPid, cli) is { } root && IsSameOrUnder(directory, root))
            {
                best = cli.Marker;
                bestCreatedAt = cli.CreatedAt;
            }
        }
        return best;
    }

    private static bool IsSameOrUnder(string directory, string root)
    {
        root = root.TrimEnd('/');
        return root.Length > 0 && directory.StartsWith(root, StringComparison.Ordinal) && (directory.Length == root.Length || directory[root.Length] == '/');
    }

    /// <summary>程序目前的工作目錄（每個程序只讀一次；CLI 的工作目錄整個執行期間不變）；讀不到回 null</summary>
    private static string? WorkingDirectoryOf(int pid, Entry entry)
    {
        if (entry.WorkingDirectoryRead)
            return entry.WorkingDirectory;
        entry.WorkingDirectoryRead = true;
        var buffer = new byte[VnodePathInfoSize];
        if (proc_pidinfo(pid, VnodePathInfoFlavor, 0, buffer, VnodePathInfoSize) != VnodePathInfoSize)
            return null;
        int end = Array.IndexOf(buffer, (byte)0, WorkingDirectoryOffset, MaxPath);
        int length = (end < 0 ? WorkingDirectoryOffset + MaxPath : end) - WorkingDirectoryOffset;
        return entry.WorkingDirectory = length == 0 ? null : Encoding.UTF8.GetString(buffer, WorkingDirectoryOffset, length);
    }

    /// <summary>帶標記的程序：完整的名稱（sysctl 給的最多 16 個字）與是不是視窗程式（執行檔在 .app 裡）</summary>
    private static void Describe(int pid, Entry entry)
    {
        if (MacProcessTable.PathOf(pid) is not { } path)
            return;
        entry.Name = Path.GetFileName(path);
        entry.IsConsole = !path.Contains(".app/Contents/MacOS/", StringComparison.Ordinal);
    }

    private static string Quote(string argument)
        => argument.Length > 0 && !argument.Any(char.IsWhiteSpace) ? argument : "\"" + argument + "\"";

    private static byte[]? ReadArguments(int pid)
    {
        var name = new[] { CtlKern, KernProcArgs2, pid };
        nuint length = 0;
        if (sysctl(name, (uint)name.Length, null, ref length, IntPtr.Zero, 0) != 0 || length < sizeof(int) || length > MaxArgumentBytes)
            return null;
        var buffer = new byte[(int)length];
        if (sysctl(name, (uint)name.Length, buffer, ref length, IntPtr.Zero, 0) != 0 || length < sizeof(int))
            return null;
        return buffer.AsSpan(0, (int)length).ToArray();
    }

    /// <summary>
    /// 純函式：KERN_PROCARGS2 的內容 → 參數清單與環境區塊（NAME=VALUE 以 \0 分隔，給 BackgroundProgramRules.MarkerOf；系統不給讀時是空的）。
    /// 格式：參數個數（int）、執行檔路徑、一個以上的 0、每個參數各以 0 結尾、接著每個環境變數各以 0 結尾
    /// </summary>
    internal static (List<string> Arguments, string Environment) ParseArguments(ReadOnlySpan<byte> data)
    {
        var arguments = new List<string>();
        if (data.Length < sizeof(int))
            return (arguments, string.Empty);
        int count = BitConverter.ToInt32(data[..sizeof(int)]);
        int position = sizeof(int);
        while (position < data.Length && data[position] != 0)
            position++;
        while (position < data.Length && data[position] == 0)
            position++;
        while (arguments.Count < count && position < data.Length)
        {
            int end = position;
            while (end < data.Length && data[end] != 0)
                end++;
            arguments.Add(Encoding.UTF8.GetString(data[position..end]));
            position = end + 1;
        }
        var environment = position < data.Length ? Encoding.UTF8.GetString(data[position..]).TrimEnd('\0') : string.Empty;
        return (arguments, environment);
    }

    /// <summary>對話自己的資料夾：…/conversations/〈32 個小寫十六進位〉（後面可以再有子資料夾）</summary>
    [GeneratedRegex(@"/conversations/([0-9a-f]{32})(?:/|$)")]
    private static partial Regex ConversationFolder();

    [DllImport("libc", SetLastError = true)]
    private static extern int sysctl(int[] name, uint nameLength, [Out] byte[]? oldValue, ref nuint oldLength, IntPtr newValue, nuint newLength);

    [DllImport("libc")]
    private static extern int proc_pidinfo(int pid, int flavor, ulong arg, [Out] byte[] buffer, int bufferSize);

    [DllImport("libc")]
    private static extern int responsibility_get_pid_responsible_for_pid(int pid);
}
