using Foundation;
using Myriagent.Core;
using UserNotifications;

namespace Myriagent.UI;

/// <summary>
/// 系統通知（通知中心；Windows 版是系統匣的氣球提示）。同一時間只留一則：新的蓋掉舊的（和氣球提示一樣），點了回到那個對話。
/// 第一次要通知時才向使用者要權限；不給、或不是從 .app 執行（沒有 bundle，系統不讓送通知）就安靜地不送
/// </summary>
internal sealed class MacNotifications : UNUserNotificationCenterDelegate
{
    private const string RequestId = "myriagent.notice";
    private const string ConversationKey = "conversation";

    private readonly Action<Action> beginInvoke;
    private readonly Action<string?> clicked;
    private readonly UNUserNotificationCenter? center;
    private bool asked;
    private bool granted;

    /// <param name="beginInvoke">排到主執行緒</param>
    /// <param name="clicked">使用者點了通知（主執行緒；參數是通知帶的對話，沒有對話的是 null）</param>
    public MacNotifications(Action<Action> beginInvoke, Action<string?> clicked)
    {
        this.beginInvoke = beginInvoke;
        this.clicked = clicked;
        try
        {
            if (string.IsNullOrEmpty(NSBundle.MainBundle.BundleIdentifier))
            {
                Log.Info("不是從 .app 執行：不送系統通知");
                return;
            }
            center = UNUserNotificationCenter.Current;
            center.Delegate = this;
        }
        catch (Exception ex)
        {
            Log.Warn($"系統通知用不了：{ex.Message}");
            center = null;
        }
    }

    /// <summary>送一則通知（主執行緒）；conversationId＝點了要回到的對話</summary>
    public void Show(string title, string message, string? conversationId)
    {
        if (center == null)
            return;
        if (!asked)
        {
            asked = true;
            center.RequestAuthorization(UNAuthorizationOptions.Alert | UNAuthorizationOptions.Sound, (ok, error) => beginInvoke(() =>
            {
                granted = ok;
                if (!ok)
                    Log.Info($"使用者沒有允許系統通知{(error == null ? string.Empty : "：" + error.LocalizedDescription)}");
                else
                    Post(title, message, conversationId);
            }));
            return;
        }
        if (granted)
            Post(title, message, conversationId);
    }

    private void Post(string title, string message, string? conversationId)
    {
        try
        {
            using var content = new UNMutableNotificationContent
            {
                Title = title,
                Body = message,
                Sound = UNNotificationSound.Default,
            };
            if (conversationId != null)
                content.UserInfo = NSDictionary.FromObjectAndKey(new NSString(conversationId), new NSString(ConversationKey));
            // 同一個識別碼：新的通知取代還沒看的那一則
            var request = UNNotificationRequest.FromIdentifier(RequestId, content, null!);
            center!.AddNotificationRequest(request, error =>
            {
                if (error != null)
                    Log.Warn($"送系統通知失敗：{error.LocalizedDescription}");
            });
        }
        catch (Exception ex)
        {
            Log.Warn($"送系統通知失敗：{ex.Message}");
        }
    }

    /// <summary>app 在前景時系統預設不顯示通知：要不要通知由中樞決定（正在看那個對話時它不會送），送了就照樣顯示</summary>
    public override void WillPresentNotification(UNUserNotificationCenter center, UNNotification notification,
        Action<UNNotificationPresentationOptions> completionHandler)
        => completionHandler(UNNotificationPresentationOptions.Banner | UNNotificationPresentationOptions.List | UNNotificationPresentationOptions.Sound);

    public override void DidReceiveNotificationResponse(UNUserNotificationCenter center, UNNotificationResponse response, Action completionHandler)
    {
        string? conversationId = null;
        try
        {
            conversationId = response.Notification.Request.Content.UserInfo?[new NSString(ConversationKey)]?.ToString();
        }
        catch (Exception)
        {
        }
        beginInvoke(() => clicked(conversationId));
        completionHandler();
    }
}
