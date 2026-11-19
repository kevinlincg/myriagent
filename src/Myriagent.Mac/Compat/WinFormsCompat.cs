// 共用的規則檔（UI/ShellRules.cs 等）用到 WinForms 與 WebView2 的幾個列舉；macOS 沒有這兩個程式庫，
// 在這裡以同樣的命名空間與名稱補上，讓那些規則檔原封不動編得過（規則的測試也照樣適用）。
// 只放列舉與純資料型別：有行為的型別（視窗、控制項）不在這裡模擬，macOS 的外殼另外寫。

namespace System.Windows.Forms
{
    /// <summary>視窗為什麼關閉（成員與 WinForms 相同；macOS 的外殼把自己的關閉原因對應到這幾個）</summary>
    internal enum CloseReason
    {
        None = 0,
        WindowsShutDown = 1,
        MdiFormClosing = 2,
        UserClosing = 3,
        TaskManagerClosing = 4,
        FormOwnerClosing = 5,
        ApplicationExitCall = 6,
    }
}

namespace Microsoft.Web.WebView2.Core
{
    /// <summary>WebView2 的程序失敗種類（成員與 WebView2 相同；macOS 的 WKWebView 只會回報網頁內容程序結束，對應 RenderProcessExited）</summary>
    internal enum CoreWebView2ProcessFailedKind
    {
        BrowserProcessExited = 0,
        RenderProcessExited = 1,
        RenderProcessUnresponsive = 2,
        FrameRenderProcessExited = 3,
        UtilityProcessExited = 4,
        SandboxHelperProcessExited = 5,
        GpuProcessExited = 6,
        PpapiPluginProcessExited = 7,
        PpapiBrokerProcessExited = 8,
        UnknownProcessExited = 9,
    }
}
