using System.Collections.Concurrent;
using CoreGraphics;
using Foundation;
using Myriagent.Core;
using Myriagent.Media;
using QuickLookThumbnailing;

namespace Myriagent.Files;

/// <summary>
/// 縮圖的 macOS 版，對外的成員和 Windows 版（src/Myriagent/Files/ThumbnailService.cs）相同，輸出 PNG（保留透明）。
/// 圖片以 ImageIO 直接縮（含 EXIF 轉正）；影片與其他檔案向系統的 Quick Look 要縮圖（和 Finder 看到的一樣，系統自己有快取）。
/// 都在背景做，不佔主執行緒；結果放記憶體快取（以路徑＋修改時間＋大小為鍵）。
/// </summary>
internal sealed class ThumbnailService : IDisposable
{
    private const int MaxCacheEntries = 400;

    /// <summary>同時最多幾個縮圖在做（一次捲出很多縮圖時不把所有核心佔滿）</summary>
    private const int MaxConcurrency = 2;

    /// <summary>Quick Look 最多等多久（壞掉的影片、離線的網路磁碟）</summary>
    private static readonly TimeSpan QuickLookTimeout = TimeSpan.FromSeconds(15);

    private static readonly HashSet<string> ImageExtensions = new(StringComparer.OrdinalIgnoreCase)
    {
        ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".heic", ".heif", ".avif", ".ico",
    };

    private readonly ConcurrentDictionary<string, byte[]> cache = new(StringComparer.Ordinal);
    private readonly ConcurrentQueue<string> cacheOrder = new();
    private readonly SemaphoreSlim slots = new(MaxConcurrency);
    private volatile bool disposed;

    /// <summary>取得縮圖（PNG）；不是本機檔案、取不到回 null</summary>
    public Task<byte[]?> GetAsync(string path, int size)
    {
        size = Math.Clamp(size, 32, 1024);
        FileInfo info;
        try
        {
            path = Path.GetFullPath(path);
            info = new FileInfo(path);
            if (!info.Exists)
                return Task.FromResult<byte[]?>(null);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
        {
            return Task.FromResult<byte[]?>(null);
        }

        var key = $"{path}|{info.LastWriteTimeUtc.Ticks}|{info.Length}|{size}";
        if (cache.TryGetValue(key, out var cached))
            return Task.FromResult<byte[]?>(cached);
        if (disposed)
            return Task.FromResult<byte[]?>(null);
        return Task.Run(() => RenderAsync(path, size, key));
    }

    private async Task<byte[]?> RenderAsync(string path, int size, string key)
    {
        await slots.WaitAsync().ConfigureAwait(false);
        byte[]? bytes;
        try
        {
            if (disposed)
                return null;
            bytes = ImageExtensions.Contains(Path.GetExtension(path)) ? RenderImage(path, size) : null;
            bytes ??= await RenderQuickLookAsync(path, size).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            Log.Warn($"縮圖失敗 {path}：{ex.Message}");
            return null;
        }
        finally
        {
            slots.Release();
        }

        if (bytes != null && cache.TryAdd(key, bytes))
        {
            cacheOrder.Enqueue(key);
            while (cache.Count > MaxCacheEntries && cacheOrder.TryDequeue(out var old))
                cache.TryRemove(old, out _);
        }
        return bytes;
    }

    /// <summary>圖片：ImageIO 直接解碼縮小；看不懂的格式回 null（改問 Quick Look）</summary>
    private static byte[]? RenderImage(string path, int size)
    {
        using var pool = new NSAutoreleasePool();
        using var source = MacImages.Open(path);
        if (source == null)
            return null;
        using var image = MacImages.Decode(source, size);
        return image == null ? null : MacImages.EncodePng(image);
    }

    /// <summary>影片與其他檔案：Quick Look 的縮圖（只要真的縮圖，不要檔案類型的圖示，和 Windows 版的 THUMBNAILONLY 一樣）；沒有縮圖回 null</summary>
    private static async Task<byte[]?> RenderQuickLookAsync(string path, int size)
    {
        var done = new TaskCompletionSource<byte[]?>(TaskCreationOptions.RunContinuationsAsynchronously);
        using var url = NSUrl.FromFilename(path);
        using var request = new QLThumbnailGenerationRequest(url, new CGSize(size, size), 1, QLThumbnailGenerationRequestRepresentationTypes.Thumbnail);
        QLThumbnailGenerator.SharedGenerator.GenerateBestRepresentation(request, (representation, error) =>
        {
            try
            {
                using var pool = new NSAutoreleasePool();
                var image = error == null ? representation?.CGImage : null;
                done.TrySetResult(image == null ? null : MacImages.EncodePng(image));
            }
            catch (Exception ex)
            {
                done.TrySetException(ex);
            }
        });
        try
        {
            return await done.Task.WaitAsync(QuickLookTimeout).ConfigureAwait(false);
        }
        catch (TimeoutException)
        {
            QLThumbnailGenerator.SharedGenerator.CancelRequest(request);
            return null;
        }
    }

    public void Dispose()
    {
        disposed = true;
    }
}
