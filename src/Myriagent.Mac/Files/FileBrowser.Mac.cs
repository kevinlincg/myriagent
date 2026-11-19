namespace Myriagent.Files;

/// <summary>檔名自然排序的 macOS 做法（Windows 用檔案總管的 StrCmpLogicalW，見 src/Myriagent/Files/FileBrowser.Win.cs）</summary>
internal static partial class FileBrowser
{
    /// <summary>
    /// 自然排序（檔名裡的數字依數值比：img2 在 img10 前面），不分大小寫。連續的數字當成一個數（前面的 0 不影響大小；
    /// 數值相同時位數少的在前），其他字元逐字比
    /// </summary>
    public static int NaturalCompare(string? a, string? b)
    {
        a ??= string.Empty;
        b ??= string.Empty;
        int i = 0, j = 0;
        while (i < a.Length && j < b.Length)
        {
            if (char.IsAsciiDigit(a[i]) && char.IsAsciiDigit(b[j]))
            {
                int startA = i, startB = j;
                while (i < a.Length && char.IsAsciiDigit(a[i]))
                    i++;
                while (j < b.Length && char.IsAsciiDigit(b[j]))
                    j++;
                var digitsA = a.AsSpan(startA, i - startA).TrimStart('0');
                var digitsB = b.AsSpan(startB, j - startB).TrimStart('0');
                if (digitsA.Length != digitsB.Length)
                    return digitsA.Length < digitsB.Length ? -1 : 1;
                int order = digitsA.SequenceCompareTo(digitsB);
                if (order != 0)
                    return order < 0 ? -1 : 1;
                // 數值相同：前面補的 0 比較少的在前（「1」在「01」前）
                if (i - startA != j - startB)
                    return i - startA < j - startB ? -1 : 1;
                continue;
            }
            int compared = char.ToUpperInvariant(a[i]).CompareTo(char.ToUpperInvariant(b[j]));
            if (compared != 0)
                return compared < 0 ? -1 : 1;
            i++;
            j++;
        }
        return (a.Length - i).CompareTo(b.Length - j) switch { < 0 => -1, > 0 => 1, _ => 0 };
    }
}
