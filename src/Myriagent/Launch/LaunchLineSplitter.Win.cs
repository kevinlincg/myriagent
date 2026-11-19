using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;
using Myriagent.Core;
using Microsoft.Win32.SafeHandles;

namespace Myriagent.Launch;

/// <summary>主控台的字碼頁（Windows 專屬；macOS 的對應實作在 src/Myriagent.Mac/Launch）</summary>
internal sealed partial class LaunchLineSplitter
{
    /// <summary>主控台的字碼頁（OEM，例如 950）；拿不到時用 Latin-1（不丟例外、位元組一對一）</summary>
    public static Encoding ConsoleEncoding()
    {
        try
        {
            Encoding.RegisterProvider(CodePagesEncodingProvider.Instance);
            return Encoding.GetEncoding((int)LaunchNative.GetOEMCP());
        }
        catch (Exception ex) when (ex is ArgumentException or NotSupportedException)
        {
            return Encoding.Latin1;
        }
    }
}
