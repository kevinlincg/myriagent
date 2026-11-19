namespace Myriagent.Update;

internal interface IRuntimeProbe
{
    bool HasAtLeast(string name, ReleaseVersion version);

    /// <summary>執行中的程序載入的 .NET 已被移除（.NET 更新時刪掉了舊的修補版）時是哪一版；都還在是 null（RuntimeRules.RemovedRuntime）</summary>
    string? RemovedRuntime => null;
}
