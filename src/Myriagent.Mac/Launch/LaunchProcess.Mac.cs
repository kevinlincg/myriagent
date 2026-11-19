using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;
using Myriagent.Agents;
using Myriagent.Core;

namespace Myriagent.Launch;

// 執行列開程序的 macOS 實作（Windows 的在 src/Myriagent/Launch/LaunchProcess.Win.cs：工作物件、CreateProcess）。
// macOS 沒有工作物件，「一個程式連同它開的子程序」以程序群組代表：對群組送訊號＝停止（SIGKILL）、暫停（SIGSTOP）、繼續（SIGCONT）。
// .NET 的 Process.Start 不能讓子程序自成一個群組，所以經系統內建的 /usr/bin/perl 轉一手（兩支很短的腳本，寫在下面）。
// 2026-11-19 在 macOS 26.5 實測過這兩支腳本的行為：結束碼照傳、被訊號結束是 128＋訊號、找不到程式是 127、
// app 那一端的管線斷了整個群組結束（暫停中也一樣）、主程序先結束時剩下的子程序跟著結束、脫鉤的程式換了工作階段不受 app 結束影響

/// <summary>開程序（中樞在 UI 執行緒呼叫）</summary>
internal sealed class LaunchProcessStarter : ILaunchProcessStarter
{
    public ILaunchProcess Build(LaunchCommand command, Action<string> onLine) => AttachedLaunchProcess.Start(command, onLine);

    public ILaunchProcess Run(LaunchCommand command, string logPath, Action<string> onLine) => DetachedLaunchProcess.Start(command, logPath, onLine);

    public ILaunchProcess RunAttached(LaunchCommand command, Action<string> onLine) => AttachedLaunchProcess.Start(command, onLine);

    public ILaunchProcess? Reattach(IReadOnlyList<LaunchProcessRef> processes, string? logPath, Action<string> onLine)
        => DetachedLaunchProcess.Reattach(processes, logPath, onLine);
}

/// <summary>macOS 的終端機輸出都是 UTF-8；解不開的位元組以 Latin-1 一對一顯示（不丟例外）</summary>
internal sealed partial class LaunchLineSplitter
{
    public static Encoding ConsoleEncoding() => Encoding.Latin1;
}

/// <summary>
/// 包住程序樹的容器：macOS 沒有 Windows 工作物件那樣的東西，一律回「沒有」，呼叫端（遠端執行指令）結束時改以程序樹找子孫
/// （Process.Kill(entireProcessTree)）。成員與 Windows 的同名類別相同（src/Myriagent/Launch/ProcessJob.Win.cs）
/// </summary>
internal static class ProcessJob
{
    public static IDisposable? Create(bool killOnClose) => null;

    public static bool Assign(IDisposable job, Process process) => false;

    public static bool Terminate(IDisposable job) => false;
}

/// <summary>執行列在 macOS 用到的系統呼叫與兩支 perl 腳本</summary>
internal static class MacLaunchNative
{
    public const string Perl = "/usr/bin/perl";

    // macOS 的訊號編號（和 Linux 不同：SIGSTOP 是 17、SIGCONT 是 19）
    public const int SigKill = 9;
    public const int SigStop = 17;
    public const int SigCont = 19;

    [DllImport("libc", SetLastError = true)]
    private static extern int kill(int pid, int signal);

    /// <summary>對一個程序送訊號；成功回 true</summary>
    public static bool Signal(int pid, int signal) => pid > 1 && kill(pid, signal) == 0;

    /// <summary>對整個程序群組送訊號（群組的編號＝帶頭那個程序的 PID）；成功回 true</summary>
    public static bool SignalGroup(int groupId, int signal) => groupId > 1 && kill(-groupId, signal) == 0;

    /// <summary>程序還在不在（送 0 號訊號：不真的送，只檢查）</summary>
    public static bool Exists(int pid) => pid > 1 && (kill(pid, 0) == 0 || Marshal.GetLastPInvokeError() == 1 /* EPERM：在，只是不能對它送訊號 */);

    /// <summary>
    /// 建置與 Debug 模式：看門的 perl 自成一個程序群組、開出要執行的程式，然後等兩件事——程式結束（把結束碼傳回去，順手結束它留下的子程序），
    /// 或標準輸入的管線斷了（app 結束或當掉：結束整個群組）。程式自己的標準輸入接到 /dev/null（等輸入的程式讀到檔案結尾，不會卡住）
    /// </summary>
    public const string WatchScript = """
        use strict; use POSIX ();
        setpgrp(0, 0);
        my $pid = fork();
        if (!defined $pid) { print STDERR "fork: $!\n"; exit 126; }
        if ($pid == 0) {
          open(STDIN, '<', '/dev/null');
          exec { $ARGV[0] } @ARGV or do { print STDERR "$ARGV[0]: $!\n"; POSIX::_exit(127); };
        }
        while (1) {
          my $r = waitpid($pid, POSIX::WNOHANG());
          if ($r == $pid) {
            my $st = $?;
            $SIG{TERM} = 'IGNORE'; kill('TERM', -$$);
            exit(($st & 127) ? 128 + ($st & 127) : ($st >> 8));
          }
          my $rin = ''; vec($rin, fileno(STDIN), 1) = 1;
          if (select(my $rout = $rin, undef, undef, 0.2) > 0) {
            my $n = sysread(STDIN, my $buf, 4096);
            if (!defined $n || $n == 0) { kill('KILL', -$$); exit 137; }
          }
        }
        """;

    /// <summary>
    /// Release 模式：換一個工作階段（和 app 脫鉤，自己也成了程序群組的頭）、輸出以附加的方式寫進輸出檔（app 讀完可以清空，程式接著寫在開頭），
    /// 然後直接變成要執行的程式（PID 不變）。第一個參數是輸出檔
    /// </summary>
    public const string DetachScript = """
        use strict; use POSIX ();
        my $log = shift @ARGV;
        POSIX::setsid();
        open(STDIN, '<', '/dev/null');
        open(STDOUT, '>>', $log) or POSIX::_exit(126);
        open(STDERR, '>&', \*STDOUT);
        $| = 1;
        exec { $ARGV[0] } @ARGV or do { print STDERR "$ARGV[0]: $!\n"; POSIX::_exit(127); };
        """;

    /// <summary>perl 加上腳本與要執行的指令；找不到 perl、找不到要執行的程式丟 InvalidOperationException（訊息給使用者看）</summary>
    public static ProcessStartInfo StartInfo(LaunchCommand command, string script, string? firstArgument)
    {
        if (!File.Exists(Perl))
            throw new InvalidOperationException($"這台沒有 {Perl}，執行列需要它來管理程式與它開的子程序");
        if (LaunchRules.ResolveExecutable(command.FileName, Environment.GetEnvironmentVariable("PATH"), File.Exists) is null)
            throw new InvalidOperationException($"找不到 {command.FileName}（不在 PATH 上）");
        var psi = new ProcessStartInfo(Perl)
        {
            UseShellExecute = false,
            WorkingDirectory = command.WorkingDirectory,
        };
        psi.ArgumentList.Add("-e");
        psi.ArgumentList.Add(script);
        psi.ArgumentList.Add("--");
        if (firstArgument != null)
            psi.ArgumentList.Add(firstArgument);
        psi.ArgumentList.Add(command.FileName);
        foreach (var arg in command.Arguments)
            psi.ArgumentList.Add(arg);
        // dotnet 的第一次使用說明與更新提示不要混進輸出
        psi.Environment["DOTNET_NOLOGO"] = "1";
        return psi;
    }
}

/// <summary>
/// 建置與 Debug 模式執行的程序：app 的子程序（看門的 perl 帶頭的程序群組），輸出經管線。停止＝對群組送 SIGKILL；
/// 暫停＝對群組送 SIGSTOP 再讓看門的繼續（它要醒著才知道 app 不見了）；app 結束或當掉時管線斷了，看門的結束整個群組。
/// Dispose 關掉管線，還在裡面的程序也會結束
/// </summary>
internal sealed class AttachedLaunchProcess : ILaunchProcess
{
    private readonly Process process;
    private int disposed;

    private AttachedLaunchProcess(Process process, Task stdout, Task stderr)
    {
        this.process = process;
        ProcessId = process.Id;
        Exited = WaitAsync(stdout, stderr);
    }

    public Task<int> Exited { get; }

    /// <summary>看門的 perl 的 PID（也是程序群組的編號）</summary>
    public int ProcessId { get; }

    public static AttachedLaunchProcess Start(LaunchCommand command, Action<string> onLine)
    {
        var psi = MacLaunchNative.StartInfo(command, MacLaunchNative.WatchScript, null);
        psi.RedirectStandardInput = true;
        psi.RedirectStandardOutput = true;
        psi.RedirectStandardError = true;
        Process process;
        try
        {
            process = Process.Start(psi) ?? throw new InvalidOperationException("程序沒有啟動");
        }
        catch (Win32Exception ex)
        {
            throw new InvalidOperationException($"找不到或無法執行 {command.FileName}（{ex.Message}）", ex);
        }
        // 標準輸入的管線留著不關：它斷掉（app 結束或當掉）就是看門的結束整個群組的信號
        var fallback = LaunchLineSplitter.ConsoleEncoding();
        var stdout = PipeReadThread.Start($"launch {process.Id} stdout", () => Pump(process.StandardOutput.BaseStream, new LaunchLineSplitter(fallback, onLine)));
        var stderr = PipeReadThread.Start($"launch {process.Id} stderr", () => Pump(process.StandardError.BaseStream, new LaunchLineSplitter(fallback, onLine)));
        return new AttachedLaunchProcess(process, stdout, stderr);
    }

    private async Task<int> WaitAsync(Task stdout, Task stderr)
    {
        await process.WaitForExitAsync().ConfigureAwait(false);
        await Task.WhenAny(Task.WhenAll(stdout, stderr), Task.Delay(LaunchRules.OutputDrainTimeout)).ConfigureAwait(false);
        return process.ExitCode;
    }

    private static void Pump(Stream stream, LaunchLineSplitter splitter)
    {
        var buffer = new byte[8192];
        try
        {
            int read;
            while ((read = stream.Read(buffer, 0, buffer.Length)) > 0)
                splitter.Feed(buffer.AsSpan(0, read));
        }
        catch (Exception ex) when (ex is IOException or ObjectDisposedException)
        {
        }
        splitter.Flush();
    }

    public void Kill()
    {
        if (Volatile.Read(ref disposed) != 0)
            return;
        try
        {
            // 暫停中的程序也能直接結束（SIGKILL 不能被擋下或延後）
            if (MacLaunchNative.SignalGroup(ProcessId, MacLaunchNative.SigKill))
                return;
            // 剛啟動、看門的還沒自成群組：從程序樹找
            process.Kill(entireProcessTree: true);
        }
        catch (Exception ex) when (ex is InvalidOperationException or Win32Exception or NotSupportedException or ObjectDisposedException)
        {
            // 已經結束
        }
    }

    public bool Suspend()
    {
        if (Volatile.Read(ref disposed) != 0 || !MacLaunchNative.SignalGroup(ProcessId, MacLaunchNative.SigStop))
            return false;
        // 看門的要醒著：app 在暫停期間結束時，它才能結束整個群組
        MacLaunchNative.Signal(ProcessId, MacLaunchNative.SigCont);
        return true;
    }

    public bool Resume() => Volatile.Read(ref disposed) == 0 && MacLaunchNative.SignalGroup(ProcessId, MacLaunchNative.SigCont);

    public IReadOnlyList<LaunchProcessRef> Processes() => [];

    public void Dispose()
    {
        if (Interlocked.Exchange(ref disposed, 1) != 0)
            return;
        try
        {
            // 關掉管線：還在執行的話看門的會結束整個群組
            process.StandardInput.Close();
        }
        catch (Exception ex) when (ex is IOException or InvalidOperationException or ObjectDisposedException)
        {
        }
        try
        {
            process.Dispose();
        }
        catch (Exception ex) when (ex is InvalidOperationException or Win32Exception)
        {
        }
    }
}

/// <summary>
/// Release 模式執行的程式：和 app 脫鉤（自己一個工作階段，app 結束、當掉或重新啟動都不影響它），輸出寫進狀態資料夾的輸出檔，
/// 這裡另開一條專用執行緒跟著讀、每 200 毫秒看主程序結束了沒。主程序結束時，同一個群組剩下的子程序一起結束。
/// app 重新啟動後以 PID 加開始時間接回（<see cref="Reattach"/>）；接回的不是這個 app 的子程序，拿不到結束碼（回 0）。
/// Dispose 只是不再跟著讀，程式照樣執行
/// </summary>
internal sealed class DetachedLaunchProcess : ILaunchProcess
{
    private readonly LaunchProcessRef root;
    private readonly Process? child;
    private readonly string? logPath;
    private readonly TaskCompletionSource<int> exited = new(TaskCreationOptions.RunContinuationsAsynchronously);
    private readonly CancellationTokenSource stop = new();
    private long position;
    private int disposed;

    private DetachedLaunchProcess(LaunchProcessRef root, Process? child, string? logPath, Action<string> onLine)
    {
        this.root = root;
        this.child = child;
        this.logPath = logPath;
        var splitter = new LaunchLineSplitter(LaunchLineSplitter.ConsoleEncoding(), onLine);
        _ = PipeReadThread.Start($"launch {root.Pid} follow", () => Follow(splitter));
    }

    public Task<int> Exited => exited.Task;

    /// <summary>主程序的 PID（也是程序群組的編號）</summary>
    public int ProcessId => root.Pid;

    public static DetachedLaunchProcess Start(LaunchCommand command, string logPath, Action<string> onLine)
    {
        var psi = MacLaunchNative.StartInfo(command, MacLaunchNative.DetachScript, logPath);
        try
        {
            if (Path.GetDirectoryName(logPath) is { Length: > 0 } directory)
                Directory.CreateDirectory(directory);
            File.WriteAllBytes(logPath, []);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            throw new InvalidOperationException($"開不了輸出檔 {logPath}（{ex.Message}）", ex);
        }
        Process process;
        try
        {
            process = Process.Start(psi) ?? throw new InvalidOperationException("程序沒有啟動");
        }
        catch (Win32Exception ex)
        {
            throw new InvalidOperationException($"找不到或無法執行 {command.FileName}（{ex.Message}）", ex);
        }
        return new DetachedLaunchProcess(new LaunchProcessRef(process.Id, MacProcessTable.StartTicks(process.Id) ?? 0), process, logPath, onLine);
    }

    /// <summary>接回 app 重新啟動前開的程式：記得的主程序（第一個）還在、開始時間也對得上才接；不在了回 null</summary>
    public static DetachedLaunchProcess? Reattach(IReadOnlyList<LaunchProcessRef> processes, string? logPath, Action<string> onLine)
    {
        if (processes.Count == 0 || !IsAlive(processes[0]))
            return null;
        return new DetachedLaunchProcess(processes[0], null, logPath, onLine);
    }

    /// <summary>PID 還在、開始時間也一樣（PID 會被重複使用）</summary>
    private static bool IsAlive(LaunchProcessRef reference)
        => MacLaunchNative.Exists(reference.Pid) && MacProcessTable.StartTicks(reference.Pid) is { } started && (reference.Created == 0 || started == reference.Created);

    private bool RootExited(out int code)
    {
        code = 0;
        if (child != null)
        {
            try
            {
                if (!child.HasExited)
                    return false;
                code = child.ExitCode;
                return true;
            }
            catch (Exception ex) when (ex is InvalidOperationException or Win32Exception or NotSupportedException)
            {
                // Process 物件已經釋放：改看 PID
            }
        }
        return !IsAlive(root);
    }

    /// <summary>跟著讀輸出檔，直到主程序結束（再讀一次把剩下的讀完）或不再跟（Dispose）</summary>
    private void Follow(LaunchLineSplitter splitter)
    {
        var token = stop.Token;
        while (!token.IsCancellationRequested)
        {
            bool done = RootExited(out var code);
            ReadLog(splitter);
            if (done)
            {
                splitter.Flush();
                // 主程序結束了：同一個群組剩下的子程序一起結束
                MacLaunchNative.SignalGroup(root.Pid, MacLaunchNative.SigKill);
                exited.TrySetResult(code);
                return;
            }
            if (token.WaitHandle.WaitOne(LaunchRules.LogPollInterval))
                return;
        }
    }

    /// <summary>讀輸出檔新增的部分；太大時讀完就清空（程式以附加的方式寫，清空後接著寫在開頭）</summary>
    private void ReadLog(LaunchLineSplitter splitter)
    {
        if (logPath == null)
            return;
        try
        {
            using var stream = new FileStream(logPath, FileMode.Open, FileAccess.ReadWrite, FileShare.ReadWrite | FileShare.Delete);
            if (stream.Length < position)
                position = 0;
            if (stream.Length == position)
                return;
            stream.Position = position;
            var buffer = new byte[65536];
            int read;
            while ((read = stream.Read(buffer, 0, buffer.Length)) > 0)
            {
                splitter.Feed(buffer.AsSpan(0, read));
                position += read;
            }
            if (position > LaunchRules.MaxLogFileBytes && stream.Length == position)
            {
                stream.SetLength(0);
                position = 0;
            }
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            // 檔案暫時讀不到（被刪、權限）：下一次再試
        }
    }

    public void Kill()
    {
        // 暫停中的程序也能直接結束；群組不在了（主程序已經結束）就只對主程序送
        if (!MacLaunchNative.SignalGroup(root.Pid, MacLaunchNative.SigKill) && IsAlive(root))
            MacLaunchNative.Signal(root.Pid, MacLaunchNative.SigKill);
    }

    public bool Suspend() => IsAlive(root) && MacLaunchNative.SignalGroup(root.Pid, MacLaunchNative.SigStop);

    public bool Resume() => IsAlive(root) && MacLaunchNative.SignalGroup(root.Pid, MacLaunchNative.SigCont);

    public IReadOnlyList<LaunchProcessRef> Processes() => IsAlive(root) ? [root] : [];

    public void Dispose()
    {
        if (Interlocked.Exchange(ref disposed, 1) != 0)
            return;
        try
        {
            stop.Cancel();
        }
        catch (ObjectDisposedException)
        {
        }
        try
        {
            child?.Dispose();
        }
        catch (Exception ex) when (ex is InvalidOperationException or Win32Exception)
        {
        }
    }
}
