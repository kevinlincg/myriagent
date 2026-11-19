using Myriagent.Core;

namespace Myriagent.UI;

/// <summary>
/// 開發時從終端機操作主視窗的網頁（平常不啟用）：設了環境變數 <c>MYRIAGENT_MAC_DEBUG=〈資料夾〉</c> 時，每隔一小段時間看那個資料夾——
/// <list type="bullet">
/// <item><c>名稱.js</c>：在主視窗的網頁執行這段 JavaScript，結果（字串）寫到 <c>名稱.out</c>，出錯寫到 <c>名稱.err</c></item>
/// <item><c>名稱.shot</c>：把主視窗網頁目前的畫面存成 <c>名稱.png</c></item>
/// </list>
/// 處理完把要求的檔案刪掉。用來在真的電腦上按按鈕、送訊息、看畫面，不必有人坐在螢幕前
/// </summary>
internal static class MacDebugDriver
{
    public const string Variable = "MYRIAGENT_MAC_DEBUG";

    private static readonly TimeSpan PollInterval = TimeSpan.FromMilliseconds(250);

    public static void AttachIfRequested(AppController app, MainWindow window)
    {
        var folder = Environment.GetEnvironmentVariable(Variable);
        if (string.IsNullOrWhiteSpace(folder))
            return;
        try
        {
            Directory.CreateDirectory(folder);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            Log.Warn($"開發用的操作資料夾建不出來：{ex.Message}");
            return;
        }
        Log.Info($"開發用的操作資料夾：{folder}");
        _ = Task.Run(async () =>
        {
            while (!app.Exiting)
            {
                await Task.Delay(PollInterval).ConfigureAwait(false);
                string[] requests;
                try
                {
                    requests = Directory.GetFiles(folder).Where(f => f.EndsWith(".js", StringComparison.Ordinal) || f.EndsWith(".shot", StringComparison.Ordinal))
                        .OrderBy(f => f, StringComparer.Ordinal).ToArray();
                }
                catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
                {
                    continue;
                }
                foreach (var request in requests)
                {
                    var done = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
                    var stem = Path.Combine(folder, Path.GetFileNameWithoutExtension(request));
                    try
                    {
                        if (request.EndsWith(".js", StringComparison.Ordinal))
                        {
                            var script = File.ReadAllText(request);
                            app.BeginInvoke(() => window.View.EvaluateString(script, (text, error) =>
                            {
                                Write(error == null ? stem + ".out" : stem + ".err", error ?? text ?? string.Empty);
                                done.TrySetResult();
                            }));
                        }
                        else
                        {
                            app.BeginInvoke(() => window.View.SavePng(stem + ".png", error =>
                            {
                                if (error != null)
                                    Write(stem + ".err", error);
                                done.TrySetResult();
                            }));
                        }
                        await Task.WhenAny(done.Task, Task.Delay(TimeSpan.FromSeconds(20))).ConfigureAwait(false);
                    }
                    catch (Exception ex)
                    {
                        Write(stem + ".err", ex.ToString());
                    }
                    try
                    {
                        File.Delete(request);
                    }
                    catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
                    {
                    }
                }
            }
        });
    }

    private static void Write(string path, string text)
    {
        try
        {
            // 先寫暫存檔再改名：等結果的那一邊不會讀到寫到一半的內容
            File.WriteAllText(path + ".tmp", text);
            File.Move(path + ".tmp", path, overwrite: true);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
        {
            Log.Warn($"開發用的操作結果寫不出去：{ex.Message}");
        }
    }
}
