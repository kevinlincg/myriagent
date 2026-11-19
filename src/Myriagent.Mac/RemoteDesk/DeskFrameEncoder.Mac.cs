using CoreGraphics;
using Foundation;
using ImageIO;
using Myriagent.Remote;

namespace Myriagent.RemoteDesk;

/// <summary>遠端桌面的影格壓成 JPEG：macOS 以 ImageIO 做（Windows 的對應實作是 DeskFrameEncoder.Win.cs）</summary>
internal sealed partial class DeskFrameEncoder
{
    /// <summary>緩衝（BGRA、由上而下、寬 <paramref name="width"/>）裡的一塊壓成 JPEG</summary>
    public static byte[] Jpeg(byte[] bgra, int width, DeskRect rect, int quality)
    {
        // 只取這一塊的像素（每列緊接著排）：CGImage 直接讀這塊記憶體，壓完才放掉
        var tile = new byte[rect.W * rect.H * 4];
        var stride = width * 4;
        for (var y = 0; y < rect.H; y++)
            Buffer.BlockCopy(bgra, (rect.Y + y) * stride + rect.X * 4, tile, y * rect.W * 4, rect.W * 4);

        using var pool = new NSAutoreleasePool();
        using var provider = new CGDataProvider(tile, 0, tile.Length);
        using var colorSpace = CGColorSpace.CreateDeviceRGB();
        // BGRA 在小端序就是一個 32 位元的 xRGB：第一個位元組以外的三個是顏色，alpha 不用
        using var image = new CGImage(rect.W, rect.H, 8, 32, rect.W * 4, colorSpace, CGBitmapFlags.ByteOrder32Little | CGBitmapFlags.NoneSkipFirst,
            provider, null, false, CGColorRenderingIntent.Default);
        using var data = new NSMutableData();
        using (var destination = CGImageDestination.Create(data, "public.jpeg", 1)
                                 ?? throw new InvalidOperationException("建立不了 JPEG 的編碼器"))
        {
            destination.AddImage(image, new CGImageDestinationOptions { LossyCompressionQuality = Math.Clamp(quality, 10, 95) / 100f });
            if (!destination.Close())
                throw new InvalidOperationException("影格壓不成 JPEG");
        }
        return data.ToArray();
    }
}
