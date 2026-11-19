namespace Myriagent.Agents;

internal static partial class AgentAttachmentProcessor
{
    // 暫時的版本（先讓共用的原始碼編得過）：原圖直接給模型。ImageIO 的縮圖與 EXIF 轉正接著補上
    private static void PrepareImage(AgentAttachment attachment, string modelImageDirectory)
    {
        _ = modelImageDirectory;
        attachment.ModelImagePaths = new List<string> { attachment.StoredPath };
    }
}
