using System.Diagnostics;
using System.Globalization;
using Myriagent.Core;

namespace Myriagent.Speech;

/// <summary>
/// 語音通知的聲音引擎，macOS 版（Windows 版是 SAPI，見 src/Myriagent/Speech/VoiceAnnouncer.cs 的 SapiSpeechEngine）：
/// 用系統內建的 <c>say</c> 念（不必載入任何語音元件，念完程序就結束）。聲音先找台灣中文的（美佳），沒有就用系統預設的聲音。
/// 語速以每分鐘字數給 <c>say</c>（倍速 × 基準），音調以內嵌的指令升降（<c>[[pbas ±n]]</c>，單位是半音）。
/// 文字一律當純文字念：代理的文字可能有 <c>[[</c>，先拆掉才不會被當成指令
/// </summary>
internal sealed class MacSpeechEngine : ISpeechEngine
{
    /// <summary>倍速 1 對應的每分鐘字數（<c>say</c> 的預設語速大約是這個值）</summary>
    private const double BaseWordsPerMinute = 180;

    /// <summary>優先用的聲音（都沒有裝就不指定，用系統設定的）</summary>
    private static readonly string[] PreferredVoices = ["Meijia", "Mei-Jia", "美佳"];

    private readonly string? voice;

    private MacSpeechEngine(string? voice) => this.voice = voice;

    /// <summary>建立引擎：這台沒有 <c>say</c> 就丟例外（VoiceAnnouncer 記一次警告，之後都不念）</summary>
    public static MacSpeechEngine Create()
    {
        if (!File.Exists("/usr/bin/say"))
            throw new InvalidOperationException("這台沒有系統的語音工具（/usr/bin/say）");
        return new MacSpeechEngine(FindVoice());
    }

    /// <summary><c>say -v ?</c> 列出裝了的聲音（每行以聲音的名稱開頭）：挑第一個偏好的</summary>
    private static string? FindVoice()
    {
        try
        {
            var info = new ProcessStartInfo("/usr/bin/say") { RedirectStandardOutput = true, RedirectStandardError = true, UseShellExecute = false };
            info.ArgumentList.Add("-v");
            info.ArgumentList.Add("?");
            using var process = Process.Start(info);
            if (process == null)
                return null;
            var listing = process.StandardOutput.ReadToEndAsync();
            _ = process.StandardError.ReadToEndAsync();
            if (!process.WaitForExit(5000))
            {
                process.Kill();
                return null;
            }
            var lines = listing.GetAwaiter().GetResult().Split('\n');
            foreach (var name in PreferredVoices)
            {
                if (lines.Any(l => l.StartsWith(name + " ", StringComparison.OrdinalIgnoreCase)))
                    return name;
            }
            // 沒有偏好的：任何一個台灣中文的聲音
            var taiwanese = lines.FirstOrDefault(l => l.Contains(" zh_TW ", StringComparison.Ordinal));
            return taiwanese?.Split("  ", StringSplitOptions.RemoveEmptyEntries).FirstOrDefault()?.Trim();
        }
        catch (Exception ex) when (ex is System.ComponentModel.Win32Exception or InvalidOperationException or IOException)
        {
            Log.Warn($"列不出這台的語音：{ex.Message}");
            return null;
        }
    }

    public void Speak(string text, VoiceTuning tuning, Func<bool> shouldStop, Action onStart)
    {
        var info = new ProcessStartInfo("/usr/bin/say") { RedirectStandardInput = true, RedirectStandardError = true, UseShellExecute = false };
        if (voice != null)
        {
            info.ArgumentList.Add("-v");
            info.ArgumentList.Add(voice);
        }
        info.ArgumentList.Add("-r");
        info.ArgumentList.Add(((int)Math.Round(BaseWordsPerMinute * Math.Clamp(tuning.Speed, 0.5, 3))).ToString(CultureInfo.InvariantCulture));
        // 要念的文字從標準輸入給：不受命令列長度限制，也不會被當成選項
        info.ArgumentList.Add("-f");
        info.ArgumentList.Add("-");

        using var process = Process.Start(info) ?? throw new InvalidOperationException("啟動不了系統的語音工具");
        _ = process.StandardError.ReadToEndAsync();
        int semitones = (int)Math.Round(tuning.PitchSemitones);
        var pitch = semitones == 0 ? string.Empty : $"[[pbas {(semitones > 0 ? "+" : "-")}{Math.Abs(semitones).ToString(CultureInfo.InvariantCulture)}]] ";
        process.StandardInput.Write(pitch + text.Replace("[[", "[ [", StringComparison.Ordinal));
        process.StandardInput.Close();
        onStart();
        while (!process.WaitForExit(50))
        {
            if (!shouldStop())
                continue;
            try
            {
                process.Kill();
            }
            catch (InvalidOperationException)
            {
            }
            break;
        }
    }

    public void Dispose()
    {
    }
}
