namespace Myriagent.Accounts;

/// <summary>
/// Antigravity 隔離帳戶的「家」（AntigravityHomeRules）：只有 macOS 有，Windows 的 agy 登入存在認證管理員的固定名稱，
/// 沒有隔離帳戶（AccountRules.SupportsIsolatedAccounts），這裡什麼都不做
/// </summary>
internal static class AntigravityHome
{
    public static void Prepare(string home)
    {
    }

    public static void RemoveLinks(string accountRoot)
    {
    }
}
