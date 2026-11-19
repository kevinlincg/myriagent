using System.Diagnostics;
using System.Globalization;
using Myriagent.Core;

namespace Myriagent.SystemStatus;

/// <summary>
/// 這台的喇叭音量與螢幕，macOS 的實作（Windows 的是 WindowsDeviceControls；中樞在背景執行緒呼叫）。
/// 音量走系統的 AppleScript（和選單列的音量是同一個設定，不需要權限）；關螢幕是 <c>pmset displaysleepnow</c>
/// （動滑鼠或鍵盤就亮；設了「喚醒後需要密碼」的電腦會跟著鎖定，那是系統的設定）
/// </summary>
internal sealed class MacDeviceControls : IDeviceControls
{
    private static readonly TimeSpan Timeout = TimeSpan.FromSeconds(5);

    public (int Volume, bool Muted)? GetAudio()
        => Parse(Run("/usr/bin/osascript", "-e", "set s to get volume settings", "-e", "return ((output volume of s) as text) & \",\" & ((output muted of s) as text)"));

    public (int Volume, bool Muted)? SetAudio(int? volume, bool? mute)
    {
        if (volume is { } v)
            Run("/usr/bin/osascript", "-e", "set volume output volume " + Math.Clamp(v, 0, 100).ToString(CultureInfo.InvariantCulture));
        if (mute is { } m)
            Run("/usr/bin/osascript", "-e", "set volume output muted " + (m ? "true" : "false"));
        return GetAudio();
    }

    public bool ScreenOff() => Run("/usr/bin/pmset", "displaysleepnow") != null;

    /// <summary>「音量,是否靜音」：沒有可以調音量的輸出裝置（例如 HDMI 螢幕的喇叭）時音量是 missing value，回 null</summary>
    internal static (int Volume, bool Muted)? Parse(string? output)
    {
        var parts = (output ?? string.Empty).Trim().Split(',');
        if (parts.Length != 2 || !int.TryParse(parts[0].Trim(), NumberStyles.Integer, CultureInfo.InvariantCulture, out var volume))
            return null;
        return (Math.Clamp(volume, 0, 100), parts[1].Trim().Equals("true", StringComparison.OrdinalIgnoreCase));
    }

    /// <summary>執行系統的小工具並回標準輸出；啟動不了、逾時或結束碼不是 0 回 null</summary>
    private static string? Run(string fileName, params string[] arguments)
    {
        try
        {
            var info = new ProcessStartInfo(fileName) { RedirectStandardOutput = true, RedirectStandardError = true, UseShellExecute = false };
            foreach (var argument in arguments)
                info.ArgumentList.Add(argument);
            using var process = Process.Start(info);
            if (process == null)
                return null;
            var output = process.StandardOutput.ReadToEndAsync();
            _ = process.StandardError.ReadToEndAsync();
            if (!process.WaitForExit((int)Timeout.TotalMilliseconds))
            {
                process.Kill(entireProcessTree: true);
                return null;
            }
            return process.ExitCode == 0 ? output.GetAwaiter().GetResult() : null;
        }
        catch (Exception ex) when (ex is System.ComponentModel.Win32Exception or InvalidOperationException or IOException)
        {
            Log.Warn($"執行 {fileName} 失敗：{ex.Message}");
            return null;
        }
    }
}
