using System.Drawing;
using System.Drawing.Imaging;
using System.Runtime.InteropServices;
using Myriagent.Remote;

namespace Myriagent.RemoteDesk;

/// <summary>遠端桌面的影格壓成 JPEG：Windows 以 GDI+ 做（macOS 的對應實作在 src/Myriagent.Mac/RemoteDesk/）</summary>
internal sealed partial class DeskFrameEncoder
{
    private static readonly ImageCodecInfo JpegCodec = ImageCodecInfo.GetImageEncoders().First(c => c.FormatID == ImageFormat.Jpeg.Guid);

    /// <summary>緩衝裡的一塊壓成 JPEG</summary>
    public static byte[] Jpeg(byte[] bgra, int width, DeskRect rect, int quality)
    {
        using var bitmap = new Bitmap(rect.W, rect.H, PixelFormat.Format32bppRgb);
        var data = bitmap.LockBits(new Rectangle(0, 0, rect.W, rect.H), ImageLockMode.WriteOnly, PixelFormat.Format32bppRgb);
        try
        {
            var stride = width * 4;
            for (var y = 0; y < rect.H; y++)
                Marshal.Copy(bgra, (rect.Y + y) * stride + rect.X * 4, data.Scan0 + y * data.Stride, rect.W * 4);
        }
        finally
        {
            bitmap.UnlockBits(data);
        }

        using var parameters = new EncoderParameters(1);
        parameters.Param[0] = new EncoderParameter(Encoder.Quality, (long)Math.Clamp(quality, 10, 95));
        using var stream = new MemoryStream();
        bitmap.Save(stream, JpegCodec, parameters);
        return stream.ToArray();
    }
}
