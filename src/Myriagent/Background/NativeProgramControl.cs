using System.Diagnostics;
using System.Runtime.InteropServices;
using Myriagent.Agents;

namespace Myriagent.Background;

/// <summary>背景程式的程序操作（BackgroundProgramMonitor 用；測試換成假的）。程序一律以 PID＋建立時間（FILETIME 刻度）認，PID 會被重複使用</summary>
internal interface IProgramProcessControl : IDisposable
{
    /// <summary>
    /// 開始追蹤：拿著它的 handle，之後才讀得到結束碼（程序結束後 PID 也不會被別人拿走）。已經結束、或那個 PID 現在是別的程序回 false；
    /// 打不開（沒有權限）時照樣回 true，只是之後讀不到結束碼
    /// </summary>
    bool Watch(int pid, long createdAt);

    /// <summary>追蹤中的程序還在執行（結束了但還有人握著 handle 的程序仍在系統的程序清單裡）；沒有追蹤的回 true</summary>
    bool IsAlive(int pid, long createdAt);

    /// <summary>放開追蹤，回結束碼與結束時間（FILETIME 刻度）；還在執行、沒有追蹤或讀不到的是 null</summary>
    (int? ExitCode, long? ExitedAt) Release(int pid, long createdAt);

    /// <summary>結束這個程式與它的子孫（使用者按了停止）；那個 PID 已經不是它、已經結束或結束不了回 false</summary>
    bool KillTree(int pid, long createdAt);
}
