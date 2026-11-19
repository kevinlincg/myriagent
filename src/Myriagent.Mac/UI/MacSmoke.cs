using Myriagent.Core;
using Newtonsoft.Json.Linq;

namespace Myriagent.UI;

/// <summary>
/// 冒煙測試（CI 用，不影響平常使用）：設了環境變數 <c>MYRIAGENT_MAC_SMOKE=〈檔案〉</c> 時，主視窗的介面載入後過幾秒，
/// 把網頁實際的樣子（標題、來源、有沒有接上宿主、畫面上的文字、元素數量）寫進那個檔案，然後結束 app。
/// 用來在沒有人看螢幕的機器上確認：app 起得來、視窗建得出來、介面網頁真的載入而且和宿主接上了
/// </summary>
internal static class MacSmoke
{
    public const string Variable = "MYRIAGENT_MAC_SMOKE";

    /// <summary>介面載入之後等多久才看（讓網頁收完 init、畫完第一個畫面）</summary>
    private static readonly TimeSpan Settle = TimeSpan.FromSeconds(5);

    /// <summary>一直沒有載入（介面起不來）時多久放棄</summary>
    private static readonly TimeSpan GiveUp = TimeSpan.FromSeconds(45);

    private const string Script = """
        JSON.stringify({
          title: document.title,
          origin: location.origin,
          secure: window.isSecureContext,
          bridge: !!(window.chrome && window.chrome.webview),
          theme: document.documentElement.dataset.theme || null,
          nodes: document.querySelectorAll('*').length,
          text: document.body ? document.body.innerText.slice(0, 2000) : null,
        })
        """;

    public static void AttachIfRequested(AppController app, MainWindow window)
    {
        var file = Environment.GetEnvironmentVariable(Variable);
        if (string.IsNullOrWhiteSpace(file))
            return;
        bool done = false;
        void Finish(JObject report)
        {
            if (done)
                return;
            done = true;
            try
            {
                File.WriteAllText(file, report.ToString());
            }
            catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
            {
                Log.Warn($"冒煙測試的結果寫不出去：{ex.Message}");
            }
            Log.Info("冒煙測試完成，結束 app");
            app.ExitImmediately();
        }

        window.ViewLoaded += () => _ = Task.Delay(Settle).ContinueWith(_ => app.BeginInvoke(() =>
        {
            if (done)
                return;
            window.View.EvaluateString(Script, (text, error) =>
            {
                JObject report;
                try
                {
                    report = text == null ? new JObject() : JObject.Parse(text);
                }
                catch (Newtonsoft.Json.JsonException)
                {
                    report = new JObject { ["raw"] = text };
                }
                report["loaded"] = true;
                if (error != null)
                    report["error"] = error;
                // 連畫面一起存（同一個檔名加 .png）：看得到 macOS 上實際畫出來的樣子
                window.View.SavePng(file + ".png", snapshotError =>
                {
                    if (snapshotError != null)
                        report["snapshotError"] = snapshotError;
                    Finish(report);
                });
            });
        }), TaskScheduler.Default);
        _ = Task.Delay(GiveUp).ContinueWith(_ => app.BeginInvoke(() => Finish(new JObject { ["loaded"] = false, ["error"] = "介面沒有載入" })),
            TaskScheduler.Default);
    }
}
