namespace Myriagent.Update;

/// <summary>
/// 「安裝到這台電腦」的 macOS 版（Windows 版把檔案複製到受管理的位置、建開始選單捷徑與移除項目，見 src/Myriagent/Update/SelfInstaller.cs）。
/// macOS 的 app 是拖進「應用程式」就算安裝，沒有另外的安裝步驟：這裡先一律不提供（設定頁不顯示「安裝到這台電腦」），
/// 自我更新接上之後再決定要不要搬進受管理的位置
/// </summary>
internal static class SelfInstaller
{
    public static bool CanOfferInstall(string? managedRelease, ReleaseIdentity current, InstallLayout layout) => false;

    public static string Install(string sourceDirectory, InstallLayout layout, ReleaseIdentity identity, bool registerShell)
        => throw new PlatformNotSupportedException("macOS 版不需要另外安裝：把 app 放進「應用程式」資料夾就可以了");
}
