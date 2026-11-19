using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Text;

namespace Myriagent.Background;

/// <summary>列出目前的程序（測試換成假的）</summary>
internal interface IProcessSnapshotSource
{
    /// <summary>這一刻的程序；帶標記的程序附上命令列與子系統。任何執行緒呼叫，但同一時間只會有一個呼叫</summary>
    IReadOnlyList<ProcessRow> Snapshot();
}
