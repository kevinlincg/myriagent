using Foundation;
using Myriagent.Media;

namespace Myriagent.Agents;

/// <summary>圖片的解碼、EXIF 轉正與另存 JPEG：macOS 以 ImageIO 做（Windows 的對應實作是 AgentAttachmentProcessor.Win.cs，流程相同）</summary>
internal static partial class AgentAttachmentProcessor
{
    /// <summary>確認原圖整張解得出來時縮到多大（只是要走完解碼，結果不留）</summary>
    private const int ValidateEdge = 256;

    private static void PrepareImage(AgentAttachment attachment, string modelImageDirectory)
    {
        string? contentType = null;
        try
        {
            using (var stream = File.OpenRead(attachment.StoredPath))
                contentType = DetectContentType(stream);
            using var pool = new NSAutoreleasePool();
            using var source = MacImages.Open(attachment.StoredPath) ?? throw new InvalidDataException("不是看得懂的圖片格式");
            var (width, height, orientation) = MacImages.Probe(source) ?? throw new InvalidDataException("讀不到圖片的大小");
            if (orientation <= 1 && !AgentAttachmentRules.NeedsModelCopy(attachment.StoredPath, contentType, attachment.SizeBytes, width, height))
            {
                // 上面只讀了檔頭：原圖直接給模型前先完整解碼一次，資料損毀的圖送出去 API 會拒絕整輪
                using var decoded = MacImages.Decode(source, ValidateEdge) ?? throw new InvalidDataException("圖片解不出來");
                attachment.ModelImagePaths = new List<string> { attachment.StoredPath };
                return;
            }

            Directory.CreateDirectory(modelImageDirectory);
            var target = Path.Combine(modelImageDirectory, $"{attachment.Id}.jpg");
            // 解碼、照 EXIF 轉正、縮到模型收的大小一次做完（比上限小的不放大，和 AgentAttachmentRules.FitWithin 一樣）
            using var scaled = MacImages.Decode(source, AgentAttachmentRules.ModelImageMaxEdge) ?? throw new InvalidDataException("圖片解不出來");
            if (!MacImages.SaveJpeg(scaled, target, JpegQuality / 100f))
                throw new IOException("存不了給模型看的圖");
            attachment.ModelImagePaths = new List<string> { target };
        }
        catch (Exception ex) when (ex is not OperationCanceledException)
        {
            // ImageIO 讀不了（損毀檔、不認得的格式）或縮圖失敗：完整的 WebP 照原樣給，其他只給路徑
            System.Diagnostics.Debug.WriteLine($"[AgentAttachmentProcessor] 圖片處理失敗 {attachment.StoredPath}：{ex.Message}");
            attachment.ModelImagePaths = AgentAttachmentRules.CanSendUndecoded(contentType, attachment.SizeBytes)
                ? UseUndecodedImage(attachment, modelImageDirectory)
                : new List<string>();
        }
    }
}
