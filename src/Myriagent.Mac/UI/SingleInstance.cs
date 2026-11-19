using System.Net.Sockets;
using System.Text;
using Myriagent.Core;
using Myriagent.Update;

namespace Myriagent.UI;

/// <summary>Acquire 的結果（回 null 時呼叫端據此決定要不要說明）</summary>
internal enum AcquireOutcome
{
    /// <summary>這一份成為實例（第一個，或等前一個結束後接手）</summary>
    Acquired,

    /// <summary>叫醒了已在執行的實例</summary>
    ActivatedExisting,

    /// <summary>已在執行的（較舊的）實例接下交棒請求：它會排空對話、啟動這一版的執行檔</summary>
    HandedOver,
}

/// <summary>
/// 單一實例（每個資料夾一個）的 macOS 版，對外的成員和 Windows 版（src/Myriagent/UI/SingleInstance.cs）相同。
/// Windows 版用具名的 mutex 與事件；.NET 在 macOS 沒有具名事件，這裡改成：
/// - 鎖定檔（獨佔開啟，行程結束時系統自動放開，當掉也不會留下假的鎖）決定誰是第一個實例；
/// - 第一個實例在 Unix domain socket 上聽「喚醒」「開新視窗」「交棒」，一行要求、一行回應。
/// 第一個實例正在結束（收尾代理程序）時關掉 socket、不回應：新實例等它放開鎖定檔後自己接手，這段期間重新啟動不會被吞掉。
/// 從 Finder、Dock 再開一次 app 不會走到這裡（系統直接叫醒已在執行的那一份，見 AppDelegate 的重新開啟）；
/// 這裡處理的是直接執行執行檔（終端機、換版幫手、交棒）的情況
/// </summary>
internal sealed class SingleInstance : IDisposable
{
    private const string ActivateCommand = "activate";
    private const string NewWindowCommand = "new-window";
    private const string HandoverCommand = "handover";
    private const string OkReply = "ok";
    private const string AcceptedReply = "accepted";

    /// <summary>等第一個實例回應多久（它的監聽執行緒收到喚醒會立刻回應）</summary>
    public static readonly TimeSpan DefaultAcknowledgeTimeout = TimeSpan.FromSeconds(3);

    /// <summary>第一個實例沒有回應時，等它結束（放開鎖定檔）多久；等不到就放棄</summary>
    public static readonly TimeSpan DefaultTakeoverTimeout = TimeSpan.FromSeconds(20);

    private static readonly UTF8Encoding Utf8 = new(false);

    private readonly FileStream lockFile;
    private readonly string socketPath;
    private readonly object gate = new();
    private Socket? listener;
    private Thread? listenerThread;
    private volatile bool stopListening;
    private bool disposed;

    /// <summary>監聽執行緒多久看一次要不要停（微秒）</summary>
    private const int PollMicroseconds = 200_000;

    private SingleInstance(FileStream lockFile, string socketPath)
    {
        this.lockFile = lockFile;
        this.socketPath = socketPath;
    }

    /// <summary>鎖定檔與 socket 放的資料夾：使用者自己的暫存資料夾（macOS 的 TMPDIR 每個使用者一個，路徑夠短放得下 socket 的 104 位元組上限）</summary>
    private static string DirectoryPath => Path.Combine(Path.GetTempPath(), "myriagent");

    private static string LockPath(string suffix) => Path.Combine(DirectoryPath, $"instance{suffix}.lock");

    private static string SocketPath(string suffix) => Path.Combine(DirectoryPath, $"instance{suffix}.sock");

    /// <summary>
    /// 成為這個資料夾的實例。已經有實例在執行就叫醒它並回 null（呼叫端直接結束）；它正在結束、沒有回應時，等它結束後由這個實例接手。
    /// </summary>
    /// <param name="suffix">名稱後綴（ShellRules.InstanceSuffix）</param>
    /// <param name="beforeSignal">叫醒前呼叫（macOS 不需要交出前景權限，照樣呼叫以維持同樣的流程）</param>
    public static SingleInstance? Acquire(string suffix, Action? beforeSignal = null, TimeSpan? acknowledgeTimeout = null, TimeSpan? takeoverTimeout = null,
        bool requestNewWindow = false)
        => Acquire(suffix, out _, beforeSignal, prepareHandover: null, acknowledgeTimeout, takeoverTimeout, requestNewWindow: requestNewWindow);

    /// <param name="outcome">回 null 時是 ActivatedExisting 或 HandedOver</param>
    /// <param name="prepareHandover">已有實例時、叫醒之前呼叫：寫好交棒請求回 true（再送交棒、等對方接下）；null 或回 false＝照原本叫醒</param>
    /// <param name="handoverTimeout">等對方接下交棒多久（預設 InstanceHandoverRules.AcceptTimeout）</param>
    /// <param name="requestNewWindow">已有實例時請它開新視窗（使用者再開一次 app）；對方不懂就照原本叫醒</param>
    public static SingleInstance? Acquire(string suffix, out AcquireOutcome outcome, Action? beforeSignal = null, Func<bool>? prepareHandover = null,
        TimeSpan? acknowledgeTimeout = null, TimeSpan? takeoverTimeout = null, TimeSpan? handoverTimeout = null, bool requestNewWindow = false)
    {
        Directory.CreateDirectory(DirectoryPath);
        var lockPath = LockPath(suffix);
        var socketPath = SocketPath(suffix);
        outcome = AcquireOutcome.Acquired;
        var held = TryLock(lockPath);
        if (held == null)
        {
            if (prepareHandover != null && prepareHandover())
            {
                beforeSignal?.Invoke();
                if (Send(socketPath, HandoverCommand, handoverTimeout ?? InstanceHandoverRules.AcceptTimeout) == AcceptedReply)
                    outcome = AcquireOutcome.HandedOver;
            }

            if (outcome != AcquireOutcome.HandedOver)
            {
                beforeSignal?.Invoke();
                var timeout = acknowledgeTimeout ?? DefaultAcknowledgeTimeout;
                var reply = requestNewWindow ? Send(socketPath, NewWindowCommand, timeout) : null;
                // 對方不懂「開新視窗」（回了別的）就照原本叫醒；完全沒回應（正在結束）就不再試，直接等它放開鎖定檔
                if (reply != OkReply && (!requestNewWindow || reply != null))
                    reply = Send(socketPath, ActivateCommand, timeout);
                if (reply == OkReply)
                {
                    outcome = AcquireOutcome.ActivatedExisting;
                }
                else
                {
                    held = WaitForLock(lockPath, takeoverTimeout ?? DefaultTakeoverTimeout);
                    outcome = held != null ? AcquireOutcome.Acquired : AcquireOutcome.ActivatedExisting;
                }
            }

            if (outcome != AcquireOutcome.Acquired || held == null)
                return null;
        }

        return new SingleInstance(held, socketPath);
    }

    /// <summary>
    /// 這個資料夾現在有沒有實例（正在執行，或正在啟動、等著接手）。只探測、不持有鎖：換版幫手切換版本之前，
    /// 確認使用者沒有在換版的空窗中自己重新開 app
    /// </summary>
    public static bool IsRunning(string suffix)
    {
        var path = LockPath(suffix);
        if (!File.Exists(path))
            return false;
        var probe = TryLock(path);
        if (probe == null)
            return true;
        probe.Dispose();
        return false;
    }

    /// <summary>獨佔開啟鎖定檔；已經被別的行程開著回 null</summary>
    private static FileStream? TryLock(string path)
    {
        try
        {
            return new FileStream(path, FileMode.OpenOrCreate, FileAccess.ReadWrite, FileShare.None, bufferSize: 1, FileOptions.None);
        }
        catch (IOException)
        {
            return null;
        }
        catch (UnauthorizedAccessException)
        {
            return null;
        }
    }

    private static FileStream? WaitForLock(string path, TimeSpan timeout)
    {
        var deadline = DateTime.UtcNow + timeout;
        while (true)
        {
            if (TryLock(path) is { } held)
                return held;
            if (DateTime.UtcNow >= deadline)
                return null;
            Thread.Sleep(100);
        }
    }

    /// <summary>向第一個實例送一行要求、等一行回應；連不上、逾時、對方沒回就關掉連線回 null</summary>
    private static string? Send(string socketPath, string command, TimeSpan timeout)
    {
        try
        {
            using var socket = new Socket(AddressFamily.Unix, SocketType.Stream, ProtocolType.Unspecified);
            int milliseconds = (int)Math.Clamp(timeout.TotalMilliseconds, 1, int.MaxValue);
            socket.SendTimeout = milliseconds;
            socket.ReceiveTimeout = milliseconds;
            socket.Connect(new UnixDomainSocketEndPoint(socketPath));
            socket.Send(Utf8.GetBytes(command + "\n"));
            return ReadLine(socket);
        }
        catch (Exception ex) when (ex is SocketException or IOException or ObjectDisposedException)
        {
            return null;
        }
    }

    /// <summary>讀到換行為止（最多 256 位元組）；對方關掉連線而且什麼都沒送回 null</summary>
    private static string? ReadLine(Socket socket)
    {
        var buffer = new byte[256];
        int length = 0;
        while (length < buffer.Length)
        {
            int read = socket.Receive(buffer, length, buffer.Length - length, SocketFlags.None);
            if (read <= 0)
                break;
            length += read;
            if (Array.IndexOf(buffer, (byte)'\n', 0, length) >= 0)
                break;
        }
        if (length == 0)
            return null;
        return Utf8.GetString(buffer, 0, length).Trim();
    }

    /// <summary>
    /// 在背景執行緒等喚醒與交棒並回應；canShow() 為 false（正在結束）時不回應、停止聽，讓新實例等這個實例結束後接手。
    /// 交棒：acceptHandover() 回 true（請求合格、已排給主執行緒處理）才回「已接下」；回 false 時回別的，新版改送喚醒。
    /// 開新視窗：回應後呼叫 openWindow()（沒給就當成喚醒）
    /// </summary>
    public void Listen(Func<bool> canShow, Action show, Func<bool>? acceptHandover = null, Action? openWindow = null)
    {
        Socket socket;
        try
        {
            // 上一個實例當掉會留下 socket 檔：鎖定檔已經歸這個實例，可以安全刪掉重建
            File.Delete(socketPath);
            socket = new Socket(AddressFamily.Unix, SocketType.Stream, ProtocolType.Unspecified);
            socket.Bind(new UnixDomainSocketEndPoint(socketPath));
            socket.Listen(8);
        }
        catch (Exception ex) when (ex is SocketException or IOException or UnauthorizedAccessException)
        {
            Log.Warn($"單一實例的 socket 開不起來（再開一次 app 時叫不醒這一份）：{ex.Message}");
            return;
        }
        lock (gate)
        {
            if (disposed)
            {
                socket.Dispose();
                return;
            }
            listener = socket;
        }

        var thread = new Thread(() =>
        {
            try
            {
                while (true)
                {
                    // 不直接卡在 Accept：macOS 上另一條執行緒關掉正在 Accept 的 socket 叫不醒它，關的那一方會一直等（app 結束時整個卡住）。
                    // 先以 Poll 等連線（每 200 毫秒看一次要不要停），有連線才 Accept
                    if (stopListening)
                        break;
                    if (!socket.Poll(PollMicroseconds, SelectMode.SelectRead))
                        continue;
                    using var client = socket.Accept();
                    client.ReceiveTimeout = 2000;
                    client.SendTimeout = 2000;
                    string? command;
                    try
                    {
                        command = ReadLine(client);
                    }
                    catch (SocketException)
                    {
                        continue;
                    }
                    if (!canShow())
                        break;
                    try
                    {
                        switch (command)
                        {
                            case HandoverCommand:
                                client.Send(Utf8.GetBytes((acceptHandover?.Invoke() == true ? AcceptedReply : "declined") + "\n"));
                                break;
                            case NewWindowCommand:
                                client.Send(Utf8.GetBytes(OkReply + "\n"));
                                if (openWindow != null)
                                    openWindow();
                                else
                                    show();
                                break;
                            case ActivateCommand:
                                client.Send(Utf8.GetBytes(OkReply + "\n"));
                                show();
                                break;
                            default:
                                client.Send(Utf8.GetBytes("unknown\n"));
                                break;
                        }
                    }
                    catch (SocketException)
                    {
                        // 對方等不及先走了：這一次的要求照樣處理完，繼續聽下一個
                    }
                }
            }
            catch (Exception ex) when (ex is SocketException or ObjectDisposedException)
            {
                // 結束時 socket 已關閉
            }
            finally
            {
                CloseListener();
            }
        })
        { IsBackground = true, Name = "Activate" };
        listenerThread = thread;
        thread.Start();
    }

    private void CloseListener()
    {
        Socket? socket;
        lock (gate)
        {
            socket = listener;
            listener = null;
        }
        if (socket == null)
            return;
        socket.Dispose();
        try
        {
            File.Delete(socketPath);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
        }
    }

    /// <summary>放開鎖定檔（結束期間被叫醒的新實例接著啟動）並關閉 socket</summary>
    public void Dispose()
    {
        lock (gate)
        {
            if (disposed)
                return;
            disposed = true;
        }
        // 先請監聽執行緒停下、等它離開 Poll（最多一秒），再關 socket
        stopListening = true;
        var thread = listenerThread;
        if (thread != null && thread != Thread.CurrentThread)
            thread.Join(TimeSpan.FromSeconds(1));
        CloseListener();
        lockFile.Dispose();
    }
}
