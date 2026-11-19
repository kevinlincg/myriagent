using CoreGraphics;
using Foundation;
using ImageIO;

namespace Myriagent.Media;

/// <summary>
/// 圖片的解碼、縮圖與存檔（macOS 的 ImageIO；Windows 版用的是 GDI+）。
/// 這些都是純資料運算，可以在背景執行緒呼叫；呼叫端自己包 NSAutoreleasePool（執行緒集區的執行緒沒有現成的）
/// </summary>
internal static class MacImages
{
    private const string JpegType = "public.jpeg";
    private const string PngType = "public.png";

    /// <summary>開啟圖片檔（只讀檔頭）；不是 ImageIO 看得懂的格式回 null</summary>
    public static CGImageSource? Open(string path)
    {
        using var url = NSUrl.FromFilename(path);
        var source = CGImageSource.FromUrl(url);
        if (source == null)
            return null;
        if (source.ImageCount > 0)
            return source;
        source.Dispose();
        return null;
    }

    /// <summary>第一張圖的寬、高（像素，還沒照 EXIF 轉正）與 EXIF 方向（1＝不用轉；沒有記就是 1）；讀不到大小回 null</summary>
    public static (int Width, int Height, int Orientation)? Probe(CGImageSource source)
    {
        var properties = source.GetProperties(0);
        if (properties?.PixelWidth is not { } width || properties.PixelHeight is not { } height || width <= 0 || height <= 0)
            return null;
        int orientation = properties.Orientation is { } value ? (int)value : 1;
        return (width, height, orientation is >= 1 and <= 8 ? orientation : 1);
    }

    /// <summary>
    /// 完整解碼並照 EXIF 轉正，長邊縮到 maxEdge 以內（比 maxEdge 小的不放大）；解不出來回 null。
    /// 一定從原圖產生（不用檔案裡內嵌的小縮圖）：要給模型看、也用來確認整張圖解得出來
    /// </summary>
    public static CGImage? Decode(CGImageSource source, int maxEdge)
        => source.CreateThumbnail(0, new CGImageThumbnailOptions
        {
            MaxPixelSize = maxEdge,
            CreateThumbnailFromImageAlways = true,
            CreateThumbnailWithTransform = true,
        });

    /// <summary>存成 JPEG（透明的地方鋪白：JPEG 沒有透明，不鋪會變黑）；quality 是 0～1</summary>
    public static bool SaveJpeg(CGImage image, string path, float quality)
    {
        using var url = NSUrl.FromFilename(path);
        using var destination = CGImageDestination.Create(url, JpegType, 1);
        if (destination == null)
            return false;
        using var white = new CGColor(1, 1, 1, 1);
        destination.AddImage(image, new CGImageDestinationOptions { LossyCompressionQuality = quality, DestinationBackgroundColor = white });
        return destination.Close();
    }

    /// <summary>編成 PNG（保留透明）；編不出來回 null</summary>
    public static byte[]? EncodePng(CGImage image)
    {
        using var data = new NSMutableData();
        using (var destination = CGImageDestination.Create(data, PngType, 1))
        {
            if (destination == null)
                return null;
            destination.AddImage(image);
            if (!destination.Close())
                return null;
        }
        return data.Length == 0 ? null : data.ToArray();
    }
}
