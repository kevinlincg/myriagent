using Myriagent.Launch;

namespace Myriagent.Tests.Launch;

/// <summary>執行列的規則在 macOS 的樣子（偵測專案、執行的指令、找執行檔、路徑）；Windows 的行為不變</summary>
public sealed class LaunchMacRulesTests
{
    private const string Proj = "/Users/u/work/App/App.csproj";
    private const string Rel = "App/App.csproj";

    private static LaunchTarget? DotNet(string properties, bool windows, string items = "")
        => LaunchRules.FromDotNetProject(Proj, Rel, $"<Project Sdk=\"Microsoft.NET.Sdk\"><PropertyGroup>{properties}</PropertyGroup>{items}</Project>", windows);

    [Fact]
    public void FromDotNetProject_只能在Windows執行的在macOS不列()
    {
        DotNet("<OutputType>WinExe</OutputType><TargetFramework>net10.0-windows</TargetFramework><UseWindowsForms>true</UseWindowsForms>", windows: false).Should().BeNull("WinForms");
        DotNet("<OutputType>WinExe</OutputType><TargetFramework>net10.0-windows</TargetFramework><UseWPF>true</UseWPF>", windows: false).Should().BeNull("WPF");
        DotNet("<OutputType>WinExe</OutputType><TargetFramework>net10.0-windows</TargetFramework>", windows: false).Should().BeNull("目標架構只有 Windows");
        DotNet("<OutputType>WinExe</OutputType><TargetFramework>net10.0-android</TargetFramework>", windows: false).Should().BeNull();
        DotNet("<OutputType>WinExe</OutputType><TargetFrameworks>net10.0-windows;netstandard2.0</TargetFrameworks>", windows: false).Should().BeNull("多目標裡沒有一個能在這台執行");

        // 同樣的專案在 Windows 照舊
        DotNet("<OutputType>WinExe</OutputType><TargetFramework>net10.0-windows</TargetFramework><UseWindowsForms>true</UseWindowsForms>", windows: true)!.Detail
            .Should().Be("WinForms · net10.0-windows");
        DotNet("<OutputType>WinExe</OutputType><TargetFrameworks>net10.0-windows;netstandard2.0</TargetFrameworks>", windows: true)!.Framework.Should().Be("net10.0-windows");
    }

    [Fact]
    public void FromDotNetProject_macOS能執行的照列_多目標優先選macos()
    {
        var avalonia = DotNet("<OutputType>WinExe</OutputType><TargetFramework>net10.0</TargetFramework>", windows: false,
            "<ItemGroup><PackageReference Include=\"Avalonia.Desktop\" Version=\"11.0.0\" /></ItemGroup>")!;
        avalonia.Kind.Should().Be(LaunchKind.DotNet);
        avalonia.Detail.Should().Be("Avalonia · net10.0");
        avalonia.Framework.Should().BeNull("只有一個目標架構不必指定");

        var native = DotNet("<OutputType>Exe</OutputType><TargetFramework>net10.0-macos26.0</TargetFramework>", windows: false);
        native.Should().BeNull("Exe 是主控台程式（要 WinExe 才算視窗程式）");
        DotNet("<OutputType>WinExe</OutputType><TargetFramework>net10.0-macos26.0</TargetFramework>", windows: false)!.Detail.Should().Be("macOS · net10.0-macos26.0");

        var multi = DotNet("<OutputType>WinExe</OutputType><TargetFrameworks>net10.0-windows;net10.0-macos;net10.0</TargetFrameworks>", windows: false)!;
        multi.Framework.Should().Be("net10.0-macos");
        multi.Detail.Should().Be("macOS · net10.0-macos");
        DotNet("<OutputType>WinExe</OutputType><TargetFrameworks>net10.0-windows;net10.0-macos;net10.0</TargetFrameworks>", windows: true)!.Framework
            .Should().Be("net10.0-windows", "Windows 上優先選 Windows 的");
        DotNet("<OutputType>WinExe</OutputType><TargetFrameworks>netstandard2.0;net10.0-windows;net10.0</TargetFrameworks>", windows: false)!.Framework
            .Should().Be("net10.0", "沒有 macos 的就選不分平台的");
        LaunchRules.BuildCommand(multi, LaunchMode.Debug)!.Arguments.Should().EndWith(["-f", "net10.0-macos"]);

        LaunchRules.IsOtherPlatformFramework("net10.0-windows10.0.19041").Should().BeTrue();
        LaunchRules.IsOtherPlatformFramework("net10.0-ios").Should().BeTrue();
        LaunchRules.IsOtherPlatformFramework("net10.0-macos26.0").Should().BeFalse();
        LaunchRules.IsOtherPlatformFramework("net10.0").Should().BeFalse();
    }

    [Fact]
    public void RunCommand_Electron在macOS直接執行npm_Windows經cmd()
    {
        var electron = LaunchRules.FromPackageJson("/Users/u/work/app/package.json", "app/package.json",
            """{"name":"demo","devDependencies":{"electron":"^30"},"scripts":{"dev":"electron ."}}""")!;

        var mac = LaunchRules.RunCommand(electron, null, LaunchMode.Debug, windows: false);
        mac.FileName.Should().Be("npm");
        mac.Arguments.Should().Equal("run", "dev");
        mac.WorkingDirectory.Should().Be(electron.Directory);
        LaunchRules.RunCommand(electron, @"C:\Windows\system32\cmd.exe", LaunchMode.Release, windows: false).Should().BeEquivalentTo(mac, "macOS 不用 ComSpec，兩種模式的指令相同");

        var windows = LaunchRules.RunCommand(electron, null, LaunchMode.Debug, windows: true);
        windows.FileName.Should().Be("cmd.exe");
        windows.Arguments.Should().Equal("/d", "/s", "/c", "npm run dev");
        LaunchRules.BuildCommand(electron, LaunchMode.Debug).Should().BeNull();
    }

    [Fact]
    public void ResolveExecutable_macOS的PATH以冒號分隔_不補副檔名()
    {
        var files = new HashSet<string> { "/opt/homebrew/bin/npm", "/usr/local/share/dotnet/dotnet", "/Users/u/bin/tool" };
        const string path = "/usr/bin: /opt/homebrew/bin :relative/dir::/usr/local/share/dotnet/";

        LaunchRules.ResolveExecutable("npm", path, files.Contains, windows: false).Should().Be("/opt/homebrew/bin/npm");
        LaunchRules.ResolveExecutable("dotnet", path, files.Contains, windows: false).Should().Be("/usr/local/share/dotnet/dotnet", "PATH 項目結尾的斜線不重複");
        LaunchRules.ResolveExecutable("/Users/u/bin/tool", null, files.Contains, windows: false).Should().Be("/Users/u/bin/tool");
        LaunchRules.ResolveExecutable("/Users/u/bin/nope", path, files.Contains, windows: false).Should().BeNull();
        LaunchRules.ResolveExecutable("node", path, files.Contains, windows: false).Should().BeNull();
        LaunchRules.ResolveExecutable("bin/tool", path, files.Contains, windows: false).Should().BeNull("相對路徑不猜");
        LaunchRules.ResolveExecutable("npm", null, files.Contains, windows: false).Should().BeNull();
        LaunchRules.ResolveExecutable("", path, files.Contains, windows: false).Should().BeNull();
        // Windows 的規則不變：補 .exe
        LaunchRules.ResolveExecutable("npm", @"C:\tools", f => f == @"C:\tools\npm.exe", windows: true).Should().Be(@"C:\tools\npm.exe");
    }

    [Fact]
    public void IsInside_macOS的路徑以斜線分隔()
    {
        LaunchRules.IsInside("/Users/u/work/App/App.csproj", "/Users/u/work").Should().BeTrue();
        LaunchRules.IsInside("/Users/u/work", "/Users/u/work").Should().BeTrue();
        LaunchRules.IsInside("/Users/u/workshop/App.csproj", "/Users/u/work").Should().BeFalse("只是開頭相同的別的資料夾");
        LaunchRules.IsInside("/Users/u/work/App.csproj", "/").Should().BeTrue();
        LaunchRules.IsInside(@"C:\Repo\App\App.csproj", @"C:\Repo").Should().BeTrue();
        LaunchRules.IsInside(@"C:\Repository\App.csproj", @"C:\Repo").Should().BeFalse();
        LaunchRules.IsInside(@"C:\Repo\App.csproj", @"C:\").Should().BeTrue();
    }
}
