using System.Diagnostics;

namespace Myriagent.Launch;

/// <summary>
/// 包住一個程序樹的容器，Windows 的實作：工作物件（<see cref="LaunchNative"/>）。遠端執行指令（RemoteExecRunner）經這裡用，不直接碰 Win32；
/// macOS 沒有對應的東西（src/Myriagent.Mac/Launch/LaunchProcess.Mac.cs 的同名類別一律回「沒有」，呼叫端改以程序樹結束子孫）
/// </summary>
internal static class ProcessJob
{
    /// <summary>建立容器；killOnClose＝容器關掉（app 結束或當掉）時系統結束裡面所有的程序。建立不了回 null</summary>
    public static IDisposable? Create(bool killOnClose) => LaunchNative.CreateJob(killOnClose);

    /// <summary>把程序放進容器（之後它開的子程序也在裡面）</summary>
    public static bool Assign(IDisposable job, Process process)
        => job is SafeKernelHandle handle && LaunchNative.AssignProcessToJobObject(handle, process.SafeHandle);

    /// <summary>結束容器裡所有的程序</summary>
    public static bool Terminate(IDisposable job) => job is SafeKernelHandle handle && LaunchNative.TerminateJobObject(handle, 1);
}
