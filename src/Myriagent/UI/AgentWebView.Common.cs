using Myriagent.Agents;
using Myriagent.Core;
using Myriagent.Hub;
using Myriagent.Sync;

namespace Myriagent.UI;

/// <summary>
/// 介面網頁宿主裡和平台無關的部分（Windows 的 WebView2 與 macOS 的 WKWebView 共用）：內容安全政策、/media 與複本媒體的回應怎麼準備、
/// 「沒有人看得到畫面」的狀態。和瀏覽器元件打交道的部分各平台自己寫（Windows：AgentWebView.cs；macOS：src/Myriagent.Mac/UI/AgentWebView.cs）
/// </summary>
internal partial class AgentWebView
{
    /// <summary>網頁的內容安全政策：只執行本頁腳本、圖片與影片只從本頁來源（含 /media、/thumb）載入</summary>
    public const string ContentSecurityPolicy =
        "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; " +
        "font-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'";

    /// <summary>/media 開檔最多等多久：網路磁碟離線時開檔會卡到 SMB 逾時（約 40 秒），先回 404，晚到的檔案直接關掉</summary>
    private static readonly TimeSpan MediaOpenTimeout = TimeSpan.FromSeconds(10);

    /// <summary>複本的 /m/、/t/ 最多等多久（來源自己 20 秒逾時，這裡是保險）</summary>
    private static readonly TimeSpan ReplicaFetchTimeout = TimeSpan.FromSeconds(25);

    /// <summary>沒有人看得到這個工作階段的畫面（鎖定、螢幕關閉；ScreenPresence 判斷，AppController 轉進來）。UI 執行緒</summary>
    internal static bool ScreenUnseen { get; private set; }

    private static event Action? ScreenUnseenChanged;

    /// <summary>畫面看不看得到變了：每個視窗的網頁跟著停畫或恢復（UI 執行緒）</summary>
    internal static void SetScreenUnseen(bool unseen)
    {
        if (ScreenUnseen == unseen)
            return;
        ScreenUnseen = unseen;
        ScreenUnseenChanged?.Invoke();
    }

    /// <summary>
    /// 複本路由要做的事（UI 執行緒呼叫；<paramref name="sourceFor"/> 是中樞的 <see cref="AgentHub.ReplicaMediaFor"/>）：
    /// 不合格的路由、沒有來源回 null（404，來源不呼叫）；否則回在背景執行的準備工作
    /// </summary>
    internal static Func<Task<PreparedResponse?>>? RouteReplica(ReplicaRoute route, string? rangeHeader, Func<string, string, IReplicaMediaSource?> sourceFor)
    {
        if (route.Kind != ReplicaRouteKind.Valid || route.Key is not { } key || route.Purpose is not { } purpose
            || sourceFor(key, purpose) is not { } source)
            return null;
        var range = purpose == MediaPurposes.Media ? rangeHeader : null;
        return () => PrepareReplicaAsync(source, key, purpose, route.Size, range, CancellationToken.None);
    }

    /// <summary>向來源要內容並準備回應（背景執行緒）；來源回 null、狀態碼或標頭不合格回 null（404）</summary>
    internal static async Task<PreparedResponse?> PrepareReplicaAsync(IReplicaMediaSource source, string key, string purpose, int? size, string? rangeHeader,
        CancellationToken ct)
    {
        if (await source.FetchAsync(key, purpose, size, rangeHeader, ct).ConfigureAwait(false) is not { } reply
            || ReplicaMediaRouteRules.Headers(reply, purpose == MediaPurposes.Media) is not { } headers)
            return null;
        return new PreparedResponse(new MemoryStream(reply.Body, writable: false), reply.Status, ReplicaMediaRouteRules.ReasonPhrase(reply.Status)!, headers);
    }

    /// <summary>開檔並依 Range 準備 200／206 回應（在背景執行緒呼叫）；找不到、是資料夾、沒有權限回 null（404）</summary>
    internal static PreparedResponse? PrepareMedia(string path, string contentType, string? rangeHeader)
    {
        FileStream? stream = null;
        try
        {
            stream = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.ReadWrite | FileShare.Delete, 1 << 16);
            long length = stream.Length;
            if (RangeRules.Parse(rangeHeader, length) is { } r)
            {
                long count = r.End - r.Start + 1;
                return new PreparedResponse(new SubStream(stream, r.Start, count), 206, "Partial Content",
                    $"Content-Type: {contentType}\r\nContent-Length: {count}\r\nContent-Range: bytes {r.Start}-{r.End}/{length}\r\nAccept-Ranges: bytes\r\nCache-Control: no-cache\r\nX-Content-Type-Options: nosniff");
            }

            return new PreparedResponse(stream, 200, "OK",
                $"Content-Type: {contentType}\r\nContent-Length: {length}\r\nAccept-Ranges: bytes\r\nCache-Control: no-cache\r\nX-Content-Type-Options: nosniff");
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
        {
            stream?.Dispose();
            return null;
        }
    }

    /// <summary>在背景準備好的回應（UI 執行緒只負責把它變成 WebView2 的回應物件）</summary>
    internal sealed record PreparedResponse(Stream? Content, int StatusCode, string ReasonPhrase, string Headers);

    internal static string? GetQueryValue(Uri uri, string key)
    {
        foreach (var part in uri.Query.TrimStart('?').Split('&', StringSplitOptions.RemoveEmptyEntries))
        {
            int eq = part.IndexOf('=');
            var name = eq < 0 ? part : part[..eq];
            if (!name.Equals(key, StringComparison.OrdinalIgnoreCase))
                continue;
            var value = eq < 0 ? string.Empty : part[(eq + 1)..];
            try
            {
                return Uri.UnescapeDataString(value.Replace('+', ' '));
            }
            catch (UriFormatException)
            {
                return null;
            }
        }
        return null;
    }

    /// <summary>是 app 自己的頁面（<c>https://myriagent.local</c>，不分大小寫）；其他來源（不該出現）不給權限</summary>
    internal static bool IsAppOrigin(string? uri)
        => Uri.TryCreate(uri, UriKind.Absolute, out var parsed)
           && string.Equals(parsed.GetLeftPart(UriPartial.Authority), AgentMarkdownRenderer.Origin, StringComparison.OrdinalIgnoreCase);
}
