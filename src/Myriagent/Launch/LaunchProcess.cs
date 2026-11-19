using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;
using Myriagent.Core;
using Microsoft.Win32.SafeHandles;

namespace Myriagent.Launch;

/// <summary>執行列開的一個程序（連同它開出的子程序）</summary>
internal interface ILaunchProcess : IDisposable
{
    /// <summary>主程序結束（輸出也讀完）時完成，值是結束碼。Release 模式執行的程式在 Dispose 之後不會完成（程式照樣執行）</summary>
    Task<int> Exited { get; }

    /// <summary>結束整個程序樹（暫停中的也一樣）</summary>
    void Kill();

    /// <summary>暫停整個程序樹；做不到（沒有權限、已經結束）回 false</summary>
    bool Suspend();

    bool Resume();

    /// <summary>現在屬於它、還在執行的程序（主程序在第一個；記進 launch\runs.json，app 重新啟動後接回）。建置一律是空的</summary>
    IReadOnlyList<LaunchProcessRef> Processes();
}

/// <summary>開程序（中樞在 UI 執行緒呼叫，要很快返回；測試換成假的）。輸出一行一行交給 onLine（背景執行緒）</summary>
internal interface ILaunchProcessStarter
{
    /// <summary>建置：app 的子程序、輸出經管線；app 結束時中樞結束它（建置被打斷之後也不會執行）。開不起來丟例外（訊息給使用者看）</summary>
    ILaunchProcess Build(LaunchCommand command, Action<string> onLine);

    /// <summary>執行（Release 模式）：和 app 脫鉤——app 結束、當掉或重新啟動都不影響它；輸出寫進 <paramref name="logPath"/>，這裡跟著讀。開不起來丟例外</summary>
    ILaunchProcess Run(LaunchCommand command, string logPath, Action<string> onLine);

    /// <summary>
    /// 執行（Debug 模式）：app 的子程序、輸出經管線，工作物件設 KILL_ON_JOB_CLOSE——app 結束、當掉或重新啟動時系統結束整個程序樹
    /// （正常結束時中樞也會先結束它）。開不起來丟例外
    /// </summary>
    ILaunchProcess RunAttached(LaunchCommand command, Action<string> onLine);

    /// <summary>
    /// 接回 app 重新啟動前開的程式：<paramref name="processes"/> 裡還在執行、PID 與建立時間都對得上的；一個都不在了回 null。
    /// 輸出檔從頭讀起（app 不在時寫的也看得到）
    /// </summary>
    ILaunchProcess? Reattach(IReadOnlyList<LaunchProcessRef> processes, string? logPath, Action<string> onLine);
}

/// <summary>把輸出的位元組以換行切開，每行各自判斷編碼（dotnet 是 UTF-8，程式自己的 Console 輸出是主控台字碼頁）；一直沒有換行的（進度列）滿了先送出</summary>
internal sealed partial class LaunchLineSplitter(Encoding fallback, Action<string> onLine)
{
    private readonly List<byte> pending = new(256);

    public void Feed(ReadOnlySpan<byte> data)
    {
        foreach (var b in data)
        {
            if (b == (byte)'\n')
                Emit();
            else
            {
                pending.Add(b);
                if (pending.Count >= LaunchRules.MaxLineLength * 4)
                    Emit();
            }
        }
    }

    /// <summary>最後一行沒有換行也送出</summary>
    public void Flush()
    {
        if (pending.Count > 0)
            Emit();
    }

    private void Emit()
    {
        var bytes = CollectionsMarshal.AsSpan(pending);
        if (bytes.Length > 0 && bytes[^1] == (byte)'\r')
            bytes = bytes[..^1];
        try
        {
            onLine(LaunchRules.CleanLine(LaunchRules.DecodeLine(bytes, fallback)));
        }
        catch (Exception ex)
        {
            Log.Warn($"執行列處理輸出失敗：{ex.Message}");
        }
        pending.Clear();
    }
}
