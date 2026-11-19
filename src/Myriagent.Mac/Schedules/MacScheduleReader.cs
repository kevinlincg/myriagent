using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;
using System.Xml;
using Myriagent.Core;

namespace Myriagent.Schedules;

/// <summary>
/// macOS 的排程讀取器（Windows 版是工作排程器的 COM，TaskSchedulerReader）：這台的 launchd 工作與目前使用者的 crontab。
/// <list type="bullet">
/// <item>launchd：~/Library/LaunchAgents（使用者自己的）、/Library/LaunchAgents、/Library/LaunchDaemons（廠商裝的，要系統管理員才能改）；
/// /System/Library 底下 Apple 自己的不讀（相當於 Windows 的 \Microsoft\）。路徑＝.plist 的完整路徑，名稱＝標籤</item>
/// <item>crontab：<c>crontab -l</c> 的每一行。路徑＝crontab/〈指令的雜湊〉，名稱＝指令</item>
/// </list>
/// 狀態從哪裡來見 <see cref="LaunchdStateRules"/>：launchctl（有沒有載入、在不在跑、結束碼、停用）一般權限就讀得到；
/// 上次什麼時候執行讀 launchd 自己的記錄檔（只有最近幾小時，每次掃描接著上次讀到的地方往下讀）。
/// cron 沒有任何執行紀錄：只知道排了什麼、下次什麼時候，上次執行只能看它轉向的記錄檔最後寫入的時間。
/// 全部在背景執行緒呼叫，失敗丟例外（訊息給使用者看）
/// </summary>
internal sealed class MacScheduleReader(IReadOnlyList<string>? workRoots = null) : IScheduleReader
{
    private const string LogDirectory = "/var/log/com.apple.xpc.launchd";
    private const string CronFolder = "crontab";
    private const int MaxPlistBytes = 1024 * 1024;
    private const int MaxLogBytes = 64 * 1024 * 1024;
    private static readonly TimeSpan CommandTimeout = TimeSpan.FromSeconds(10);
    private static readonly byte[] ExitedMark = "exited due to"u8.ToArray();
    private static readonly byte[] SpawnedMark = "Successfully spawned"u8.ToArray();

    private readonly object gate = new();
    private readonly Dictionary<string, LaunchdRun> runs = new(StringComparer.Ordinal);
    private bool logPrimed;
    private string? logHead;
    private long logOffset;

    [DllImport("libc")]
    private static extern uint getuid();

    public ScheduleScan Scan(CancellationToken ct)
    {
        var sw = Stopwatch.StartNew();
        var env = CurrentEnvironment(workRoots);
        var tasks = new List<ScheduledTaskInfo>();
        string? error = null;
        try
        {
            int uid = (int)getuid();
            var home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
            var now = DateTime.Now;
            Dictionary<string, LaunchdRun> seen;
            lock (gate)
            {
                ReadLog(uid, ct);
                seen = new Dictionary<string, LaunchdRun>(runs, StringComparer.Ordinal);
            }
            var userLoaded = LaunchdStateRules.ParseList(Capture("/bin/launchctl", "list"));
            var systemLoaded = LaunchdStateRules.ParseSystemServices(Capture("/bin/launchctl", "print", "system"));
            var userDisabled = LaunchdStateRules.ParseDisabled(Capture("/bin/launchctl", "print-disabled", $"gui/{uid}"));
            var systemDisabled = LaunchdStateRules.ParseDisabled(Capture("/bin/launchctl", "print-disabled", "system"));
            var bootTime = now - TimeSpan.FromMilliseconds(Environment.TickCount64);
            var repoCache = new Dictionary<string, string?>(StringComparer.Ordinal);
            foreach (var (directory, domain) in Folders(home))
            {
                ct.ThrowIfCancellationRequested();
                List<string> files;
                try
                {
                    if (!Directory.Exists(directory))
                        continue;
                    files = Directory.EnumerateFiles(directory, "*.plist").ToList();
                }
                catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
                {
                    Log.Info($"排程：讀不到資料夾 {directory}：{ex.Message}");
                    continue;
                }
                bool daemon = domain == LaunchdDomain.Daemon;
                foreach (var file in files)
                    tasks.Add(ReadJob(file, domain, uid, env, now, bootTime, seen, daemon ? systemLoaded : userLoaded, daemon ? systemDisabled : userDisabled, repoCache));
            }
            ct.ThrowIfCancellationRequested();
            tasks.AddRange(ReadCrontab(env, now, home, repoCache));
        }
        catch (OperationCanceledException)
        {
            throw;
        }
        catch (Exception ex)
        {
            Log.Error("排程：讀取 launchd 的工作失敗", ex);
            error = ex.Message;
        }
        return new ScheduleScan(DateTime.Now, tasks.OrderBy(t => t.Path, StringComparer.OrdinalIgnoreCase).ToList(), error, sw.ElapsedMilliseconds, env);
    }

    private static IEnumerable<(string Directory, LaunchdDomain Domain)> Folders(string home)
    {
        yield return (Path.Combine(home, "Library", "LaunchAgents"), LaunchdDomain.UserAgent);
        yield return ("/Library/LaunchAgents", LaunchdDomain.GlobalAgent);
        yield return ("/Library/LaunchDaemons", LaunchdDomain.Daemon);
    }

    private static LaunchdDomain? DomainOf(string plistPath, string home)
    {
        var directory = Path.GetDirectoryName(plistPath);
        foreach (var (folder, domain) in Folders(home))
        {
            if (string.Equals(directory, folder, StringComparison.OrdinalIgnoreCase))
                return domain;
        }
        return null;
    }

    private static ScheduledTaskInfo ReadJob(string file, LaunchdDomain domain, int uid, ScheduleEnvironment env, DateTime now, DateTime bootTime,
        Dictionary<string, LaunchdRun> seen, Dictionary<string, LaunchdLoaded> loadedJobs, Dictionary<string, bool> disabledJobs, Dictionary<string, string?> repoCache)
    {
        var stem = LaunchdOriginRules.StemOf(file);
        try
        {
            var xml = ReadPlistXml(file);
            DateTime? created = null;
            try
            {
                created = File.GetCreationTime(file);
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
            }
            LaunchdJob job;
            try
            {
                job = LaunchdPlistRules.ToJob(xml, domain, env.UserName, created, bootTime);
            }
            catch (XmlException ex)
            {
                return new ScheduledTaskInfo(file, xml, null, "定義讀不懂：" + ex.Message, ScheduledTaskState.Unknown, false, null, 0, null, 0, null) { DisplayName = stem };
            }
            var label = job.Label ?? stem;
            seen.TryGetValue(LaunchdStateRules.ServiceKey(domain, uid, label), out var run);
            // 「每隔多久」從上次開始執行的時間起算（記錄檔看得到的話）
            if (run?.LastStart is { } lastStart)
                job = LaunchdPlistRules.ToJob(xml, domain, env.UserName, created, lastStart);
            loadedJobs.TryGetValue(label, out var loaded);
            bool? disabled = disabledJobs.TryGetValue(label, out var d) ? d : null;
            var state = LaunchdStateRules.Resolve(job.Definition.Enabled, disabled, loaded, run, loaded?.Pid is { } pid ? ProcessStart(pid) : null,
                LastWrite(job.StandardOutPath, job.StandardErrorPath));
            DateTime? next = null;
            if (state.Enabled && loaded != null)
            {
                var upcoming = ScheduleTriggerRules.Occurrences(job.Definition.Triggers, now, now.AddDays(35), 1);
                if (upcoming.Count > 0)
                    next = upcoming[0];
            }
            string? repo = null;
            foreach (var p in ScheduleClassifyRules.CandidatePaths(job.Definition.Actions, env))
            {
                repo = RepoRootOf(p, repoCache);
                if (repo != null)
                    break;
            }
            // 檔案在、launchd 卻不知道它：要等下次登入（或開機）才會載入
            var note = loaded == null && state.Enabled
                ? (domain == LaunchdDomain.Daemon ? "launchd 還沒有載入這個工作：下次開機才會開始（或以 sudo launchctl bootstrap system 載入）" : "launchd 還沒有載入這個工作：下次登入才會開始（或按「立即執行」載入）")
                : null;
            return new ScheduledTaskInfo(file, xml, job.Definition, note, (ScheduledTaskState)(int)state.State, state.Enabled, state.LastRun, state.LastResult, next, 0, repo)
            {
                DisplayName = label,
            };
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or InvalidOperationException)
        {
            return new ScheduledTaskInfo(file, null, null, "讀不到這個工作：" + ex.Message, ScheduledTaskState.Unknown, false, null, 0, null, 0, null) { DisplayName = stem };
        }
    }

    /// <summary>plist 的內容（XML）；二進位的（bplist 開頭）以 plutil 轉成 XML</summary>
    private static string ReadPlistXml(string file)
    {
        var info = new FileInfo(file);
        if (info.Length > MaxPlistBytes)
            throw new InvalidOperationException("檔案太大，不像是 launchd 的定義");
        var bytes = File.ReadAllBytes(file);
        if (bytes.AsSpan().StartsWith("bplist"u8))
            return Capture("/usr/bin/plutil", "-convert", "xml1", "-o", "-", file);
        int skip = bytes.Length >= 3 && bytes[0] == 0xEF && bytes[1] == 0xBB && bytes[2] == 0xBF ? 3 : 0;
        return Encoding.UTF8.GetString(bytes, skip, bytes.Length - skip);
    }

    private static DateTime? ProcessStart(int pid)
    {
        try
        {
            using var process = Process.GetProcessById(pid);
            return process.StartTime;
        }
        catch (Exception ex) when (ex is ArgumentException or InvalidOperationException or System.ComponentModel.Win32Exception or NotSupportedException)
        {
            return null;
        }
    }

    private static DateTime? LastWrite(params string?[] paths)
    {
        DateTime? latest = null;
        foreach (var path in paths)
        {
            if (string.IsNullOrEmpty(path) || !path.StartsWith('/'))
                continue;
            try
            {
                if (File.Exists(path) && File.GetLastWriteTime(path) is var written && (latest == null || written > latest))
                    latest = written;
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
            }
        }
        return latest;
    }

    // ───────────────────────────── crontab ─────────────────────────────

    private static IEnumerable<ScheduledTaskInfo> ReadCrontab(ScheduleEnvironment env, DateTime now, string home, Dictionary<string, string?> repoCache)
    {
        var result = new List<ScheduledTaskInfo>();
        string text;
        try
        {
            text = Capture("/usr/bin/crontab", "-l");
        }
        catch (InvalidOperationException)
        {
            // 沒有 crontab（結束碼 1）、或系統不讓讀：當作沒有
            return result;
        }
        foreach (var entry in CronRules.Parse(text, env.UserName))
        {
            var upcoming = ScheduleTriggerRules.Occurrences(entry.Definition.Triggers, now, now.AddDays(35), 1);
            string? repo = null;
            foreach (var p in ScheduleClassifyRules.CandidatePaths(entry.Definition.Actions, env))
            {
                repo = RepoRootOf(p, repoCache);
                if (repo != null)
                    break;
            }
            var log = CronRules.LogPath(entry.Command) is { } target ? ScheduleClassifyRules.Expand(target, env) : null;
            if (log != null && !log.StartsWith('/'))
                log = Path.Combine(home, log);
            result.Add(new ScheduledTaskInfo(CronFolder + "/" + entry.Key, entry.Line, entry.Definition, null, ScheduledTaskState.Ready, true,
                LastWrite(log), ScheduleResultRules.HasNotRun, upcoming.Count > 0 ? upcoming[0] : null, 0, repo)
            {
                DisplayName = CronRules.DisplayName(entry.Command),
            });
        }
        return result;
    }

    private static bool IsCronPath(string path) => path.StartsWith(CronFolder + "/", StringComparison.Ordinal);

    /// <summary>立即執行 crontab 的一行：照 cron 的方式（/bin/sh -c、在使用者資料夾、很少的環境變數加上 crontab 自己設的），不等它結束</summary>
    private static void RunCron(string path)
    {
        string text;
        try
        {
            text = Capture("/usr/bin/crontab", "-l");
        }
        catch (InvalidOperationException ex)
        {
            throw new InvalidOperationException("讀不到 crontab：" + ex.Message, ex);
        }
        var user = Environment.UserName;
        var entry = CronRules.Parse(text, user).FirstOrDefault(e => CronFolder + "/" + e.Key == path)
                    ?? throw new InvalidOperationException("找不到這個排程（可能已被刪除）");
        var home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        var info = new ProcessStartInfo("/bin/sh")
        {
            UseShellExecute = false,
            WorkingDirectory = home,
            RedirectStandardInput = true,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
        };
        info.ArgumentList.Add("-c");
        info.ArgumentList.Add(entry.Command);
        info.Environment.Clear();
        info.Environment["HOME"] = home;
        info.Environment["LOGNAME"] = user;
        info.Environment["USER"] = user;
        info.Environment["SHELL"] = "/bin/sh";
        info.Environment["PATH"] = "/usr/bin:/bin";
        foreach (var (name, value) in CronRules.Variables(text))
            info.Environment[name] = value;
        var process = Process.Start(info) ?? throw new InvalidOperationException("啟動不了 /bin/sh");
        process.StandardInput.Close();
        // 沒有轉向的輸出丟掉（cron 會寄信，這裡不寄）；程序結束時釋放
        _ = process.StandardOutput.ReadToEndAsync();
        _ = process.StandardError.ReadToEndAsync();
        _ = process.WaitForExitAsync().ContinueWith(_ => process.Dispose(), TaskScheduler.Default);
    }

    // ───────────────────────────── 操作 ─────────────────────────────

    public void Run(string path)
    {
        if (IsCronPath(path))
        {
            RunCron(path);
            return;
        }
        var target = Locate(path);
        if (!IsLoaded(target.Service))
            Launchctl(target, "bootstrap", target.Domain, path);
        Launchctl(target, "kickstart", target.Service);
    }

    public void Stop(string path)
    {
        if (IsCronPath(path))
            throw new InvalidOperationException("crontab 的排程不能從這裡停止，請交給代理處理");
        var target = Locate(path);
        Launchctl(target, "kill", "SIGTERM", target.Service);
    }

    /// <summary>
    /// 停用：卸載（之後不會再觸發；正在跑的那一次會被結束）並記成停用（下次登入也不載入）。啟用：取消停用並載入。
    /// launchctl disable 自己只影響下次載入，所以要連同卸載、載入一起做
    /// </summary>
    public void SetEnabled(string path, bool enabled)
    {
        if (IsCronPath(path))
            throw new InvalidOperationException("crontab 的排程不能從這裡" + (enabled ? "啟用" : "停用") + "（要改 crontab），請交給代理處理");
        var target = Locate(path);
        if (enabled)
        {
            Launchctl(target, "enable", target.Service);
            if (!IsLoaded(target.Service))
                Launchctl(target, "bootstrap", target.Domain, path);
            return;
        }
        if (IsLoaded(target.Service))
            Launchctl(target, "bootout", target.Service);
        Launchctl(target, "disable", target.Service);
    }

    /// <summary>launchctl 要的對象：Domain＝gui/〈uid〉或 system；Service＝Domain/〈標籤〉</summary>
    private sealed record Target(string Domain, string Service, bool NeedsRoot);

    private static Target Locate(string path)
    {
        var home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        if (DomainOf(path, home) is not { } domain)
            throw new InvalidOperationException("找不到這個排程（可能已被刪除）");
        if (!File.Exists(path))
            throw new InvalidOperationException("找不到這個排程（可能已被刪除）");
        string? label = null;
        try
        {
            label = LaunchdPlistRules.ToJob(ReadPlistXml(path), domain, Environment.UserName, null, null).Label;
        }
        catch (Exception ex) when (ex is XmlException or IOException or UnauthorizedAccessException or InvalidOperationException)
        {
        }
        label ??= LaunchdOriginRules.StemOf(path);
        bool daemon = domain == LaunchdDomain.Daemon;
        var where = daemon ? "system" : $"gui/{getuid()}";
        return new Target(where, where + "/" + label, daemon);
    }

    private static bool IsLoaded(string service)
    {
        try
        {
            Capture("/bin/launchctl", "print", service);
            return true;
        }
        catch (InvalidOperationException)
        {
            return false;
        }
    }

    private static void Launchctl(Target target, params string[] arguments)
    {
        // 開機載入的工作（/Library/LaunchDaemons）要 root：app 不提高權限
        if (target.NeedsRoot)
            throw new InvalidOperationException("權限不足（這個排程要系統管理員才能改），請交給代理處理");
        try
        {
            Capture("/bin/launchctl", arguments);
        }
        catch (InvalidOperationException ex) when (ex.Message.Contains("Operation not permitted", StringComparison.OrdinalIgnoreCase)
                                                   || ex.Message.Contains("Not privileged", StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException("權限不足（這個排程要系統管理員才能改），請交給代理處理", ex);
        }
        catch (InvalidOperationException ex) when (ex.Message.Contains("Could not find service", StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException("launchd 裡沒有這個工作（還沒有載入）", ex);
        }
        catch (InvalidOperationException ex) when (ex.Message.Contains("disabled", StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException("這個排程停用中，請先啟用", ex);
        }
    }

    // ───────────────────────────── 記錄檔 ─────────────────────────────

    /// <summary>
    /// 把 launchd 的記錄檔讀到最新（在鎖裡）：第一次連同輪替出去的兩份一起讀（舊的先），之後接著上次讀到的地方。
    /// 記錄檔輪替（launchd.log 改名成 launchd.log.1、另開新檔）以「第一行變了」判斷：先把改名的那份剩下的讀完，再從頭讀新的
    /// </summary>
    private void ReadLog(int uid, CancellationToken ct)
    {
        var current = Path.Combine(LogDirectory, "launchd.log");
        var rotated = Path.Combine(LogDirectory, "launchd.log.1");
        try
        {
            if (!logPrimed)
            {
                logPrimed = true;
                ReadFrom(Path.Combine(LogDirectory, "launchd.log.2"), 0, uid, ct);
                ReadFrom(rotated, 0, uid, ct);
                logHead = HeadOf(current);
                logOffset = ReadFrom(current, 0, uid, ct);
                return;
            }
            var head = HeadOf(current);
            if (head != logHead)
            {
                if (logHead != null && HeadOf(rotated) == logHead)
                    ReadFrom(rotated, logOffset, uid, ct);
                logHead = head;
                logOffset = 0;
            }
            logOffset = ReadFrom(current, logOffset, uid, ct);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            Log.Info($"排程：讀不到 launchd 的記錄檔（上次執行的時間會不知道）：{ex.Message}");
        }
    }

    /// <summary>檔案開頭的一小段（第一行的時間與工作名稱）；沒有檔案、空的是 null</summary>
    private static string? HeadOf(string path)
    {
        try
        {
            if (!File.Exists(path))
                return null;
            using var stream = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.ReadWrite | FileShare.Delete);
            Span<byte> buffer = stackalloc byte[96];
            int read = stream.ReadAtLeast(buffer, buffer.Length, throwOnEndOfStream: false);
            return read == 0 ? null : Encoding.Latin1.GetString(buffer[..read]);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            return null;
        }
    }

    /// <summary>從 offset 讀到檔尾，完整的行才算；回傳讀到哪（下一次從那裡接）。只留這個使用者登入後載入的與開機載入的工作</summary>
    private long ReadFrom(string path, long offset, int uid, CancellationToken ct)
    {
        if (!File.Exists(path))
            return 0;
        using var stream = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.ReadWrite | FileShare.Delete, 1 << 16, FileOptions.SequentialScan);
        if (offset > stream.Length)
            offset = 0;
        long remaining = Math.Min(stream.Length - offset, MaxLogBytes);
        if (remaining <= 0)
            return offset;
        stream.Position = offset;
        var bytes = new byte[remaining];
        int total = stream.ReadAtLeast(bytes, bytes.Length, throwOnEndOfStream: false);
        var data = bytes.AsSpan(0, total);
        var userPrefix = $"gui/{uid}/";
        int start = 0;
        while (start < data.Length)
        {
            ct.ThrowIfCancellationRequested();
            int newline = data[start..].IndexOf((byte)'\n');
            if (newline < 0)
                break;
            var line = data.Slice(start, newline);
            start += newline + 1;
            if (line.IndexOf(ExitedMark.AsSpan()) < 0 && line.IndexOf(SpawnedMark.AsSpan()) < 0)
                continue;
            if (LaunchdStateRules.ParseLogLine(Encoding.UTF8.GetString(line)) is { } e
                && (e.Service.StartsWith(userPrefix, StringComparison.Ordinal) || e.Service.StartsWith("system/", StringComparison.Ordinal)))
                LaunchdStateRules.Apply(runs, e);
        }
        return offset + start;
    }

    // ───────────────────────────── 共用 ─────────────────────────────

    /// <summary>執行一個系統工具、回標準輸出；結束碼不是 0 丟 InvalidOperationException（訊息是它的錯誤輸出）</summary>
    private static string Capture(string file, params string[] arguments)
    {
        var info = new ProcessStartInfo(file)
        {
            UseShellExecute = false,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            StandardOutputEncoding = Encoding.UTF8,
            StandardErrorEncoding = Encoding.UTF8,
        };
        foreach (var a in arguments)
            info.ArgumentList.Add(a);
        using var process = Process.Start(info) ?? throw new InvalidOperationException($"啟動不了 {file}");
        var stdout = process.StandardOutput.ReadToEndAsync();
        var stderr = process.StandardError.ReadToEndAsync();
        if (!process.WaitForExit((int)CommandTimeout.TotalMilliseconds))
        {
            try
            {
                process.Kill(entireProcessTree: true);
            }
            catch (Exception ex) when (ex is InvalidOperationException or System.ComponentModel.Win32Exception)
            {
            }
            throw new InvalidOperationException($"{Path.GetFileName(file)} 沒有回應");
        }
        process.WaitForExit();
        var output = stdout.GetAwaiter().GetResult();
        if (process.ExitCode != 0)
        {
            var message = stderr.GetAwaiter().GetResult().Trim();
            throw new InvalidOperationException(message.Length > 0 ? message : $"{Path.GetFileName(file)} 結束碼 {process.ExitCode}");
        }
        return output;
    }

    /// <summary>往上找 .git（資料夾或 worktree 的檔案）；外接與網路磁碟（/Volumes、/net）不碰（檢查存在可能卡在斷線的掛載點）</summary>
    internal static string? RepoRootOf(string path, Dictionary<string, string?> cache)
    {
        if (!path.StartsWith('/') || path.StartsWith("/Volumes/", StringComparison.Ordinal) || path.StartsWith("/net/", StringComparison.Ordinal))
            return null;
        string? dir;
        try
        {
            dir = Directory.Exists(path) ? path : Path.GetDirectoryName(path);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            return null;
        }
        var visited = new List<string>();
        while (!string.IsNullOrEmpty(dir) && dir != "/")
        {
            if (cache.TryGetValue(dir, out var known))
            {
                foreach (var v in visited)
                    cache[v] = known;
                return known;
            }
            visited.Add(dir);
            var git = Path.Combine(dir, ".git");
            if (Directory.Exists(git) || File.Exists(git))
            {
                foreach (var v in visited)
                    cache[v] = dir;
                return dir;
            }
            dir = Path.GetDirectoryName(dir);
        }
        foreach (var v in visited)
            cache[v] = null;
        return null;
    }

    /// <summary>這台電腦的使用者與判斷用的變數（~ 與 $HOME 展開成使用者資料夾；launchd 自己不展開變數，sh -c 的字串才會有）</summary>
    internal static ScheduleEnvironment CurrentEnvironment(IReadOnlyList<string>? workRoots = null)
    {
        var home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        var vars = new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["HOME"] = home,
            ["USER"] = Environment.UserName,
            ["LOGNAME"] = Environment.UserName,
        };
        return new ScheduleEnvironment(Environment.UserName, Environment.MachineName, vars, workRoots, Posix: true);
    }
}
