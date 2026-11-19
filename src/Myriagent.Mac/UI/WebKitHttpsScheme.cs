using System.Runtime.InteropServices;
using Foundation;
using Myriagent.Core;
using ObjCRuntime;
using WebKit;

namespace Myriagent.UI;

/// <summary>
/// 讓 WKWebView 的 https 請求交給 app 自己回應。
/// 介面網頁的來源是 <c>https://myriagent.local</c>（AgentMarkdownRenderer.Origin：回覆裡的圖片、檔案連結、CSP 的 'self' 都以它為準，
/// 和 Windows 版、伺服器、瀏覽器版共用）。Windows 的 WebView2 可以攔下任何網址；WKWebView 的 setURLSchemeHandler 不接受它「自己會處理」的協定
/// （<c>+[WKWebView handlesURLScheme:]</c> 回 YES 的，含 https），登記時直接丟例外。
/// 這裡把那個類別方法換成對 https 回 NO，登記就過得了；之後這個 WKWebView 的 https 請求全部進 AgentWebView 的處理常式
/// （介面只載入自己的資源，CSP 也擋掉其他來源；站外連結一律交給預設瀏覽器，不在這個 WKWebView 裡開）。
/// 2026-11-19 在 macOS 26.5 實測：頁面的來源、安全內容（麥克風要用）、圖片、fetch、影片的 Range 請求都正常。
/// 換不成（系統改了實作）時 <see cref="Install"/> 回 false，介面開不起來，啟動時會明講原因
/// </summary>
internal static unsafe class WebKitHttpsScheme
{
    private static bool attempted;
    private static bool installed;
    private static delegate* unmanaged<IntPtr, IntPtr, IntPtr, byte> original;

    /// <summary>只做一次（主執行緒，建立第一個 WKWebView 之前）；回傳換成了沒有</summary>
    public static bool Install()
    {
        if (attempted)
            return installed;
        attempted = true;
        try
        {
            var webViewClass = Class.GetHandle(typeof(WKWebView));
            var method = class_getClassMethod(webViewClass.Handle, Selector.GetHandle("handlesURLScheme:"));
            if (method == IntPtr.Zero)
            {
                Log.Warn("WKWebView 沒有 handlesURLScheme: 這個方法，https 的介面網址接不上");
                return false;
            }
            delegate* unmanaged<IntPtr, IntPtr, IntPtr, byte> replacement = &HandlesUrlScheme;
            original = (delegate* unmanaged<IntPtr, IntPtr, IntPtr, byte>)method_setImplementation(method, (IntPtr)replacement);
            installed = true;
        }
        catch (Exception ex) when (ex is EntryPointNotFoundException or DllNotFoundException or InvalidOperationException)
        {
            Log.Error("換不了 WKWebView 的 handlesURLScheme:", ex);
        }
        return installed;
    }

    /// <summary>+[WKWebView handlesURLScheme:] 的替身：https 回 NO（交給 app 的處理常式），其他照原本的</summary>
    [UnmanagedCallersOnly]
    private static byte HandlesUrlScheme(IntPtr self, IntPtr selector, IntPtr scheme)
    {
        try
        {
            if (scheme != IntPtr.Zero && string.Equals(NSString.FromHandle(new NativeHandle(scheme)), "https", StringComparison.OrdinalIgnoreCase))
                return 0;
        }
        catch (Exception)
        {
            // 從原生程式碼呼叫進來的：例外不能往外丟，照原本的方法回答
        }
        return original != null ? original(self, selector, scheme) : (byte)1;
    }

    [DllImport("/usr/lib/libobjc.dylib")]
    private static extern IntPtr class_getClassMethod(IntPtr cls, IntPtr selector);

    [DllImport("/usr/lib/libobjc.dylib")]
    private static extern IntPtr method_setImplementation(IntPtr method, IntPtr implementation);
}
