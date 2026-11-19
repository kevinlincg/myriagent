using System.Net.NetworkInformation;

namespace Myriagent.SystemStatus;

/// <summary>
/// 整台電腦實體網卡的累計收送量，macOS 版（Windows 版用 GetIfTable2，見 src/Myriagent/SystemStatus/NetworkTotals.cs）。
/// 只算有線與 Wi-Fi（介面名稱 en 開頭、正在使用的）：迴路、VPN 的通道（utun）、AirDrop（awdl、llw）、橋接都不算，
/// VPN 開著時同一份流量才不會算兩次。網卡的清單每 <see cref="RelistMs"/> 重新列一次（插拔、開關 Wi-Fi）
/// </summary>
internal static class NetworkTotals
{
    /// <summary>多久重新列一次網卡</summary>
    internal const int RelistMs = 60_000;

    private static readonly object Gate = new();
    private static NetworkInterface[] listed = Array.Empty<NetworkInterface>();
    private static long listedAt = long.MinValue;

    public static (ulong Rx, ulong Tx)? Read()
    {
        lock (Gate)
        {
            try
            {
                long now = Environment.TickCount64;
                if (listedAt == long.MinValue || now - listedAt >= RelistMs)
                {
                    listed = NetworkInterface.GetAllNetworkInterfaces().Where(IsHardware).ToArray();
                    listedAt = now;
                }

                ulong rx = 0, tx = 0;
                foreach (var nic in listed)
                {
                    var statistics = nic.GetIPStatistics();
                    rx += (ulong)Math.Max(0, statistics.BytesReceived);
                    tx += (ulong)Math.Max(0, statistics.BytesSent);
                }
                return (rx, tx);
            }
            catch (Exception ex) when (ex is NetworkInformationException or PlatformNotSupportedException or InvalidOperationException)
            {
                listedAt = long.MinValue;
                return null;
            }
        }
    }

    private static bool IsHardware(NetworkInterface nic)
        => nic.Name.StartsWith("en", StringComparison.Ordinal)
           && nic.OperationalStatus == OperationalStatus.Up
           && nic.NetworkInterfaceType is NetworkInterfaceType.Ethernet or NetworkInterfaceType.Wireless80211 or NetworkInterfaceType.GigabitEthernet;
}
