namespace Myriagent.Update;

/// <summary>
/// macOS 版的執行階段檢查（Windows 版讀登錄，見 src/Myriagent/Update/RuntimeProbe.Win.cs）：.NET 包在 .app 裡面，
/// 每一版帶自己的執行階段，不會有「這台沒裝新版要的 .NET」，也不會被系統的 .NET 更新移除
/// </summary>
internal sealed class BundledRuntimeProbe : IRuntimeProbe
{
    public bool HasAtLeast(string name, ReleaseVersion version) => true;
}
