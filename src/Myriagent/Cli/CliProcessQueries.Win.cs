using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using Myriagent.Agents;
using Myriagent.Core;
using Microsoft.Win32.SafeHandles;

namespace Myriagent.Cli;

/// <summary>讀別的程序命令列的 Windows 做法（NtQueryInformationProcess）；macOS 的對應實作在 src/Myriagent.Mac/Cli/CliProcessQueries.Mac.cs</summary>
internal static partial class CliProcessQueries
{
    private const uint PROCESS_QUERY_LIMITED_INFORMATION = 0x1000;
    private const int ProcessCommandLineInformation = 60;
    private const int MaxCommandLineBytes = 1 << 20;

    /// <summary>程序的命令列（NtQueryInformationProcess 的 ProcessCommandLineInformation，Windows 8.1 起；只要 PROCESS_QUERY_LIMITED_INFORMATION）</summary>
    internal static string? TryGetCommandLine(int processId)
    {
        using var handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, (uint)processId);
        if (handle.IsInvalid)
            return null;
        NtQueryInformationProcess(handle, ProcessCommandLineInformation, IntPtr.Zero, 0, out var length);
        if (length <= 0 || length > MaxCommandLineBytes)
            return null;
        var buffer = Marshal.AllocHGlobal(length);
        try
        {
            if (NtQueryInformationProcess(handle, ProcessCommandLineInformation, buffer, length, out _) < 0)
                return null;
            var text = Marshal.PtrToStructure<UNICODE_STRING>(buffer);
            return text.Buffer == IntPtr.Zero ? string.Empty : Marshal.PtrToStringUni(text.Buffer, text.Length / 2);
        }
        finally
        {
            Marshal.FreeHGlobal(buffer);
        }
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct UNICODE_STRING
    {
        public ushort Length;
        public ushort MaximumLength;
        public IntPtr Buffer;
    }

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern SafeProcessHandle OpenProcess(uint desiredAccess, bool inheritHandle, uint processId);

    [DllImport("ntdll.dll")]
    private static extern int NtQueryInformationProcess(SafeProcessHandle processHandle, int processInformationClass, IntPtr processInformation, int processInformationLength, out int returnLength);
}
