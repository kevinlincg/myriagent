namespace Myriagent;

internal static class Program
{
    /// <summary>進入點（先放最小的版本讓共用的原始碼編得過；外殼在 UI/ 底下接上）</summary>
    private static int Main(string[] args)
    {
        if (args.Length >= 1 && args[0] == "tool-remote")
            return MyriagentRemote.Program.Main(args[1..]);
        return 0;
    }
}
