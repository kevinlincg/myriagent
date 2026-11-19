using System.Runtime.InteropServices;
using System.Text;
using Myriagent.Agents;

namespace Myriagent.Background;

/// <summary>
/// 列出目前的程序，macOS 版（背景程式的偵測與對話的 CPU 用量；Windows 的對應實作在 src/Myriagent/Background/NativeProcessSnapshot.Win.cs）：
/// 一次 sysctl 拿到自己這個使用者所有程序的 PID、父 PID、啟動時間與名稱，每個程序第一次出現時讀一次它的環境變數裡的對話標記
/// （sysctl 的 KERN_PROCARGS2，同一個使用者的程序才讀得到），帶標記的再記下命令列與是不是視窗程式（執行檔在 .app 裡）。一律只讀
/// </summary>
internal sealed class NativeProcessSnapshot : IProcessSnapshotSource
{
    private const int CtlKern = 1;
    private const int KernProcArgs2 = 49;
    private const int MaxArgumentBytes = 1 << 20;

    /// <summary>剛建立的程序可能還沒換成要執行的程式（fork 之後、exec 之前，環境變數還是父程序的）：這段時間內讀到沒有標記就下一次再讀</summary>
    private static readonly long RetryWindowTicks = TimeSpan.FromSeconds(10).Ticks;

    private sealed class Entry
    {
        public long CreatedAt;
        public string Name = string.Empty;
        public string? Marker;
        public bool? IsConsole;
        public string? CommandLine;
        public bool Done;
    }

    private readonly Dictionary<(int Pid, long Seconds, int Microseconds), Entry> cache = new();

    public IReadOnlyList<ProcessRow> Snapshot()
    {
        var raw = MacProcessTable.ReadOwn();
        var rows = new List<ProcessRow>(raw.Count);
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
            if (!entry.Done)
                Inspect(process.Pid, entry, now - entry.CreatedAt > RetryWindowTicks);
            rows.Add(new ProcessRow(process.Pid, process.ParentPid, entry.CreatedAt, entry.Name, entry.Marker, entry.IsConsole, entry.CommandLine));
        }
        foreach (var gone in cache.Keys.Where(k => !seen.Contains(k)).ToList())
            cache.Remove(gone);
        return rows;
    }

    /// <summary>讀標記；有標記再記命令列、完整的名稱與是不是視窗程式。沒有標記而且程序還很新時不記成完成（下一次再讀）</summary>
    private static void Inspect(int pid, Entry entry, bool settled)
    {
        var data = ReadArguments(pid);
        if (data == null)
        {
            // 別的權限的程序、已經結束：不是這個 app 的代理開的
            entry.Done = true;
            return;
        }
        var (arguments, environment) = ParseArguments(data);
        entry.Marker = BackgroundProgramRules.MarkerOf(environment);
        if (entry.Marker is null)
        {
            entry.Done = settled;
            return;
        }
        entry.CommandLine = string.Join(' ', arguments.Select(Quote));
        var path = MacProcessTable.PathOf(pid);
        if (path != null)
        {
            // sysctl 給的名稱最多 16 個字：有路徑就用完整的檔名
            entry.Name = Path.GetFileName(path);
            entry.IsConsole = !path.Contains(".app/Contents/MacOS/", StringComparison.Ordinal);
        }
        entry.Done = true;
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
    /// 純函式：KERN_PROCARGS2 的內容 → 參數清單與環境區塊（NAME=VALUE 以 \0 分隔，給 BackgroundProgramRules.MarkerOf）。
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
        return (arguments, position < data.Length ? Encoding.UTF8.GetString(data[position..]) : string.Empty);
    }

    [DllImport("libc", SetLastError = true)]
    private static extern int sysctl(int[] name, uint nameLength, [Out] byte[]? oldValue, ref nuint oldLength, IntPtr newValue, nuint newLength);
}
