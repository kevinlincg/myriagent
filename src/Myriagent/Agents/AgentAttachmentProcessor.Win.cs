using System.Drawing;
using System.Drawing.Drawing2D;
using System.Drawing.Imaging;

namespace Myriagent.Agents;

/// <summary>圖片的解碼、EXIF 轉正與另存 JPEG：Windows 以 GDI+ 做（macOS 的對應實作在 src/Myriagent.Mac/Agents/）</summary>
internal static partial class AgentAttachmentProcessor
{
    private static void PrepareImage(AgentAttachment attachment, string modelImageDirectory)
    {
        string? contentType = null;
        try
        {
            using var stream = File.OpenRead(attachment.StoredPath);
            contentType = DetectContentType(stream);
            using var image = Image.FromStream(stream, useEmbeddedColorManagement: false, validateImageData: false);
            int orientation = GetExifOrientation(image);
            if (orientation <= 1 && !AgentAttachmentRules.NeedsModelCopy(attachment.StoredPath, contentType, attachment.SizeBytes, image.Width, image.Height))
            {
                // 上面只讀了標頭：原圖直接給模型前先完整解碼一次，資料損毀的圖送出去 API 會拒絕整輪
                new Bitmap(image).Dispose();
                attachment.ModelImagePaths = new List<string> { attachment.StoredPath };
                return;
            }

            Directory.CreateDirectory(modelImageDirectory);
            var target = Path.Combine(modelImageDirectory, $"{attachment.Id}.jpg");
            using var oriented = (Image)image.Clone();
            ApplyExifOrientation(oriented, orientation);
            var (width, height) = AgentAttachmentRules.FitWithin(oriented.Width, oriented.Height, AgentAttachmentRules.ModelImageMaxEdge);
            using var bitmap = new Bitmap(width, height);
            using (var graphics = Graphics.FromImage(bitmap))
            {
                graphics.Clear(Color.White);   // 透明背景存 JPEG 會變黑，先鋪白
                graphics.InterpolationMode = InterpolationMode.HighQualityBicubic;
                graphics.SmoothingMode = SmoothingMode.HighQuality;
                graphics.PixelOffsetMode = PixelOffsetMode.HighQuality;
                graphics.DrawImage(oriented, 0, 0, width, height);
            }
            SaveJpeg(bitmap, target);
            attachment.ModelImagePaths = new List<string> { target };
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            // GDI+ 讀不了（WebP、損毀檔）或縮圖失敗（超大尺寸配不出點陣圖時是 OverflowException）：完整的 WebP 照原樣給，其他只給路徑
            System.Diagnostics.Debug.WriteLine($"[AgentAttachmentProcessor] 圖片處理失敗 {attachment.StoredPath}：{ex.Message}");
            attachment.ModelImagePaths = AgentAttachmentRules.CanSendUndecoded(contentType, attachment.SizeBytes)
                ? UseUndecodedImage(attachment, modelImageDirectory)
                : new List<string>();
        }
    }

    private static int GetExifOrientation(Image image)
    {
        const int OrientationId = 0x0112;
        try
        {
            if (image.PropertyIdList.Contains(OrientationId))
            {
                var item = image.GetPropertyItem(OrientationId);
                if (item?.Value is { Length: >= 2 } value)
                    return BitConverter.ToUInt16(value, 0);
            }
        }
        catch (ArgumentException)
        {
        }
        return 1;
    }

    private static void ApplyExifOrientation(Image image, int orientation)
    {
        var flip = orientation switch
        {
            2 => RotateFlipType.RotateNoneFlipX,
            3 => RotateFlipType.Rotate180FlipNone,
            4 => RotateFlipType.Rotate180FlipX,
            5 => RotateFlipType.Rotate90FlipX,
            6 => RotateFlipType.Rotate90FlipNone,
            7 => RotateFlipType.Rotate270FlipX,
            8 => RotateFlipType.Rotate270FlipNone,
            _ => RotateFlipType.RotateNoneFlipNone,
        };
        if (flip != RotateFlipType.RotateNoneFlipNone)
            image.RotateFlip(flip);
    }

    private static void SaveJpeg(Image image, string path)
    {
        var codec = ImageCodecInfo.GetImageEncoders().First(c => c.FormatID == ImageFormat.Jpeg.Guid);
        using var parameters = new EncoderParameters(1);
        parameters.Param[0] = new EncoderParameter(System.Drawing.Imaging.Encoder.Quality, JpegQuality);
        image.Save(path, codec, parameters);
    }
}
