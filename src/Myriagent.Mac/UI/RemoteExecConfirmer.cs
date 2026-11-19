using AppKit;
using Myriagent.Core;
using Myriagent.Hub;
using Myriagent.RemoteExec;

namespace Myriagent.UI;

/// <summary>
/// 高風險的遠端執行指令在這台問人，macOS 版（Windows 版是自己畫的 WinForms 視窗，見 src/Myriagent/UI/RemoteExecConfirmer.cs）：
/// 置頂的系統訊息框列出發起的電腦、對話、高風險的原因與完整的指令，預設按鈕是「拒絕」（按 Enter 不會誤允許），
/// 倒數到 0、按 Esc 都當成拒絕。app 藏起來時照樣跳出來（不依附主視窗）。
/// 訊息框開著時主執行緒照常處理排進來的工作（對話繼續跑），和 Windows 版的 MessageBox 一樣
/// </summary>
internal sealed class RemoteExecConfirmer(IAgentHost host) : IRemoteExecConfirmer
{
    public Task<RemoteExecConfirmResult> ConfirmAsync(RemoteExecConfirmation confirmation, TimeSpan timeout, CancellationToken cancellationToken)
    {
        ArgumentNullException.ThrowIfNull(confirmation);
        var result = new TaskCompletionSource<RemoteExecConfirmResult>(TaskCreationOptions.RunContinuationsAsynchronously);
        host.BeginInvoke(() =>
        {
            try
            {
                result.TrySetResult(Ask(confirmation, timeout, cancellationToken));
            }
            catch (Exception ex)
            {
                Log.Error("遠端執行指令的確認視窗開不起來", ex);
                result.TrySetResult(RemoteExecConfirmResult.Denied);
            }
        });
        return result.Task;
    }

    /// <summary>顯示訊息框並等回應（主執行緒；巢狀的事件迴圈）</summary>
    private RemoteExecConfirmResult Ask(RemoteExecConfirmation confirmation, TimeSpan timeout, CancellationToken cancellationToken)
    {
        var request = confirmation.Request;
        var deadline = DateTime.UtcNow + timeout;
        var body = $"高風險：{confirmation.Risk.Category}（符合「{confirmation.Risk.Match}」）\n\n"
                   + (request.Label is { } label ? $"對話：{label}\n" : string.Empty)
                   + $"殼層：{request.Shell}　工作資料夾：{request.Cwd ?? "（使用者的家目錄）"}　上限：{request.TimeoutSeconds} 秒\n"
                   + "以這台目前登入的使用者執行；需要系統管理員的操作另外會跳出系統的密碼詢問。\n\n"
                   + request.Command.Replace("\r\n", "\n", StringComparison.Ordinal);

        using var alert = new NSAlert
        {
            AlertStyle = NSAlertStyle.Critical,
            MessageText = $"{confirmation.IssuerLabel} 的代理要在這台執行一個高風險的指令",
            InformativeText = body + "\n\n" + Remaining(timeout),
        };
        alert.AddButton("拒絕");
        alert.AddButton("允許執行");
        alert.Window.Level = NSWindowLevel.Floating;

        RemoteExecConfirmResult? forced = null;
        bool closed = false;
        void Abort(RemoteExecConfirmResult outcome)
        {
            if (closed || forced != null)
                return;
            forced = outcome;
            NSApplication.SharedApplication.AbortModal();
        }

        using var registration = cancellationToken.Register(() => host.BeginInvoke(() => Abort(RemoteExecConfirmResult.Denied)));
        _ = CountdownAsync();
        NSApplication.SharedApplication.ActivateIgnoringOtherApps(true);
        NSSound.FromName("Funk")?.Play();
        long response = alert.RunModal();
        closed = true;
        if (forced is { } outcome)
            return outcome;
        return response == (long)NSAlertButtonReturn.Second ? RemoteExecConfirmResult.Allowed : RemoteExecConfirmResult.Denied;

        // 每秒更新剩下的時間；到了就當成沒有回應
        async Task CountdownAsync()
        {
            while (!closed && forced == null)
            {
                await Task.Delay(TimeSpan.FromSeconds(1)).ConfigureAwait(false);
                host.BeginInvoke(() =>
                {
                    if (closed || forced != null)
                        return;
                    var left = deadline - DateTime.UtcNow;
                    if (left <= TimeSpan.Zero)
                        Abort(RemoteExecConfirmResult.TimedOut);
                    else
                        alert.InformativeText = body + "\n\n" + Remaining(left);
                });
            }
        }
    }

    private static string Remaining(TimeSpan left) => $"{(int)left.TotalMinutes}:{left.Seconds:00} 後沒有回應就自動拒絕";
}
