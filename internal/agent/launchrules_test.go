package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchDotNetDetection(t *testing.T) {
	avalonia := `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup><OutputType>WinExe</OutputType><TargetFramework>net9.0</TargetFramework><AssemblyName>MyApp</AssemblyName></PropertyGroup>
  <ItemGroup><PackageReference Include="Avalonia" Version="11" /></ItemGroup>
</Project>`
	tg := launchFromDotNetProject("/p/App/App.csproj", "App/App.csproj", avalonia, "darwin")
	if tg == nil || tg.Name != "MyApp" || tg.Detail != "Avalonia · net9.0" || tg.Kind != launchDotNet || tg.Framework != "" {
		t.Fatalf("Avalonia 專案：%+v", tg)
	}
	cases := []struct {
		name, xml, goos string
		want            bool
	}{
		{"主控台程式不列", `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>Exe</OutputType></PropertyGroup></Project>`, "darwin", false},
		{"WinForms 只能在 Windows", `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>WinExe</OutputType><UseWindowsForms>true</UseWindowsForms></PropertyGroup></Project>`, "darwin", false},
		{"測試專案不列", `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>WinExe</OutputType><IsTestProject>true</IsTestProject></PropertyGroup></Project>`, "darwin", false},
		{"舊式專案不列", `<Project><PropertyGroup><OutputType>WinExe</OutputType></PropertyGroup></Project>`, "darwin", false},
		{"有 Condition 的屬性不算", `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup Condition="'$(X)'=='1'"><OutputType>WinExe</OutputType></PropertyGroup></Project>`, "darwin", false},
		{"DTD 一律當成壞掉", `<!DOCTYPE x [<!ENTITY a "b">]><Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>WinExe</OutputType></PropertyGroup></Project>`, "darwin", false},
		{"只有 -windows 的目標架構", `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>WinExe</OutputType><TargetFramework>net9.0-windows</TargetFramework></PropertyGroup></Project>`, "darwin", false},
		{"Import Sdk 也是 SDK 式", `<Project><Import Project="Sdk.props" Sdk="Microsoft.NET.Sdk" /><PropertyGroup><OutputType>WinExe</OutputType></PropertyGroup></Project>`, "linux", true},
		{"-macos 在 Linux 不能執行", `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>WinExe</OutputType><TargetFramework>net9.0-macos</TargetFramework></PropertyGroup></Project>`, "linux", false},
	}
	for _, c := range cases {
		if got := launchFromDotNetProject("/p/A.csproj", "A.csproj", c.xml, c.goos) != nil; got != c.want {
			t.Errorf("%s：得到 %v", c.name, got)
		}
	}
	// 多目標：macOS 優先選 -macos、Linux 選不分平台的
	multi := `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>WinExe</OutputType><TargetFrameworks>netstandard2.0;net9.0-windows;net9.0;net9.0-macos</TargetFrameworks></PropertyGroup></Project>`
	if tg := launchFromDotNetProject("/p/A.csproj", "A.csproj", multi, "darwin"); tg == nil || tg.Framework != "net9.0-macos" || tg.Detail != "macOS · net9.0-macos" {
		t.Fatalf("多目標（macOS）：%+v", tg)
	}
	if tg := launchFromDotNetProject("/p/A.csproj", "A.csproj", multi, "linux"); tg == nil || tg.Framework != "net9.0" {
		t.Fatalf("多目標（Linux）：%+v", tg)
	}
	b := launchBuildCommand(*launchFromDotNetProject("/p/A.csproj", "A.csproj", multi, "darwin"), launchRelease)
	if strings.Join(b.Args, " ") != "build /p/A.csproj -c Release -nologo -tl:off -nodeReuse:false -f net9.0-macos" || b.Dir != "/p" {
		t.Fatalf("建置指令：%+v", b)
	}
}

func TestLaunchElectronDetection(t *testing.T) {
	tg := launchFromPackageJSON("/p/app/package.json", "app/package.json", `{"name":"demo","devDependencies":{"electron":"30"},"scripts":{"dev":"electron .","start":" "}}`)
	if tg == nil || tg.Script != "dev" || tg.Name != "demo" || tg.Detail != "Electron · npm run dev" {
		t.Fatalf("Electron：%+v", tg)
	}
	if launchFromPackageJSON("/p/package.json", "package.json", `{"dependencies":{"react":"1"},"scripts":{"start":"x"}}`) != nil {
		t.Fatal("沒有 electron 不該列")
	}
	if launchFromPackageJSON("/p/package.json", "package.json", `{"dependencies":{"electron":"1"},"scripts":{"build":"x"}}`) != nil {
		t.Fatal("沒有 start、dev 不該列")
	}
	if tg := launchFromPackageJSON("/p/x/package.json", "x/package.json", `{"dependencies":{"electron":"1"},"scripts":{"start":"x"}}`); tg == nil || tg.Name != "x" {
		t.Fatalf("沒有 name 用資料夾名稱：%+v", tg)
	}
	r := launchRunCommand(*tg, launchDebug)
	if r.File != "npm" || strings.Join(r.Args, " ") != "run dev" || launchBuildCommand(*tg, launchDebug) != nil {
		t.Fatalf("Electron 的指令：%+v", r)
	}
}

func TestLaunchScanSkipsAndOrders(t *testing.T) {
	root := t.TempDir()
	write := func(rel, text string) {
		p := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(text), 0o644)
	}
	electron := `{"dependencies":{"electron":"1"},"scripts":{"start":"electron ."}}`
	write("package.json", electron)
	write("b/package.json", electron)
	write("a/c/package.json", electron)
	write("node_modules/x/package.json", electron)
	write(".hidden/package.json", electron)
	write("a/b/c/d/package.json", electron) // 第 4 層：太深
	got := launchScan(context.Background(), root, "darwin")
	var rels []string
	for _, tg := range got {
		rels = append(rels, tg.RelativePath)
	}
	if strings.Join(rels, ",") != "package.json,b/package.json,a/c/package.json" {
		t.Fatalf("偵測結果：%v", rels)
	}
}

func TestLaunchPlan(t *testing.T) {
	cases := []struct {
		phase  launchPhase
		action string
		want   launchStep
	}{
		{launchIdle, "start", launchStepStart},
		{launchPaused, "start", launchStepResume},
		{launchRunning, "start", launchStepNone},
		{launchBuilding, "stop", launchStepStop},
		{launchIdle, "stop", launchStepNone},
		{launchRunning, "pause", launchStepPause},
		{launchPaused, "resume", launchStepResume},
		{launchIdle, "restart", launchStepStart},
		{launchStopping, "restart", launchStepRestart},
		{launchStopping, "stop", launchStepNone},
		{launchRunning, "bogus", launchStepNone},
	}
	for _, c := range cases {
		if got := launchPlan(c.phase, c.action); got != c.want {
			t.Errorf("%s 在 %s：得到 %d，要 %d", c.action, launchPhaseName(c.phase), got, c.want)
		}
	}
}

func TestLaunchOutputRules(t *testing.T) {
	if got := launchCleanLine("\x1b[31m紅\x1b[0m\tX\x07\uFEFF"); got != "紅    X\uFEFF" {
		t.Fatalf("清理：%q", got)
	}
	if got := launchCleanLine("\uFEFFabc"); got != "abc" {
		t.Fatalf("開頭的 BOM：%q", got)
	}
	long := strings.Repeat("a", 2500)
	if got := launchCleanLine(long); len([]rune(got)) != 2001 || !strings.HasSuffix(got, "…") {
		t.Fatalf("截斷：%d", len([]rune(got)))
	}
	if launchDecodeLine([]byte{0xe4, 0xb8, 0xad}) != "中" || launchDecodeLine([]byte{0xff, 'a'}) != "\u00ffa" {
		t.Fatal("解碼")
	}
	lines := []string{
		"  Determining projects to restore...",
		`/src/App/Program.cs(12,5): error CS1002: ; expected [/src/App/App.csproj]`,
		`C:\src\App\Form1.cs(3,1): error CS0103: x [C:\src\App\App.csproj]`,
	}
	if got := launchBuildErrorSummary(lines); got != "Program.cs(12,5): CS1002 ; expected" {
		t.Fatalf("錯誤摘要：%q", got)
	}
	if got := launchBuildErrorSummary(lines[2:]); got != "Form1.cs(3,1): CS0103 x" {
		t.Fatalf("Windows 路徑的錯誤摘要：%q", got)
	}
	if got := launchBuildErrorSummary([]string{"MSBUILD : error MSB1009: Project file does not exist."}); got != "MSB1009 Project file does not exist." {
		t.Fatalf("沒有位置的錯誤：%q", got)
	}
	if launchBuildFailedEnd(1, nil, time.Now()).Text != "建置失敗（結束碼 1）" {
		t.Fatal("沒有錯誤行時的說明")
	}
	if launchFormatExitCode(-532462766) != "0xE0434352" || launchFormatExitCode(3) != "3" {
		t.Fatal("結束碼的寫法")
	}
	if launchExitedEnd(0, time.Now()).Text != "已結束" || launchExitedEnd(2, time.Now()).Text != "已結束（結束碼 2）" {
		t.Fatal("結束的說明")
	}
	var got []string
	s := &launchLineSplitter{onLine: func(l string) { got = append(got, l) }}
	s.feed([]byte("a\r\nb"))
	s.feed([]byte("c\n"))
	s.feed([]byte("tail"))
	s.flush()
	if strings.Join(got, "|") != "a|bc|tail" {
		t.Fatalf("切行：%v", got)
	}
}

func TestLaunchLogRead(t *testing.T) {
	l := newLaunchLog(3)
	l.restart()
	for _, s := range []string{"1", "2", "3", "4"} {
		l.append(s)
	}
	// 緩衝區是 2,3,4（序號 2～4）
	if s := l.read(1, 2); s.Reset || strings.Join(s.Lines, "") != "34" || s.Seq != 4 {
		t.Fatalf("接著讀：%+v", s)
	}
	if s := l.read(1, 0); !s.Reset || strings.Join(s.Lines, "") != "234" {
		t.Fatalf("要的已被丟掉要整份：%+v", s)
	}
	if s := l.read(0, 3); !s.Reset {
		t.Fatalf("epoch 不同要整份：%+v", s)
	}
	if s := l.read(1, 9); !s.Reset {
		t.Fatalf("序號太大要整份：%+v", s)
	}
	if s := l.read(1, 1); s.Reset || strings.Join(s.Lines, "") != "234" {
		t.Fatalf("剛好接得上：%+v", s)
	}
	if l.appendIf(0, "x") {
		t.Fatal("舊 epoch 的輸出不該加")
	}
}

func TestLaunchPrefsAndRecords(t *testing.T) {
	var p launchPrefsFile
	id := launchTargetID("/p/A.csproj")
	if !launchIsTargetID(id) || len(id) != 13 {
		t.Fatalf("目標的鍵：%s", id)
	}
	if !launchRememberSelection(&p, "/p", id) || launchRememberSelection(&p, "/p", id) {
		t.Fatal("記選擇：第一次要改、第二次不用")
	}
	if launchSelectedFromPrefs(&p, "/p") != id || launchSelectedFromPrefs(&p, "/q") != "" {
		t.Fatal("讀選擇")
	}
	launchRememberSelection(&p, "/q", id)
	if p.Folders[0].Folder != "/q" || len(p.Folders) != 2 {
		t.Fatalf("最近的在前：%+v", p.Folders)
	}
	if launchModeFromPrefs(&p, id) != launchDebug {
		t.Fatal("沒記過是 Debug")
	}
	launchRememberMode(&p, id, launchRelease)
	if launchModeFromPrefs(&p, id) != launchRelease {
		t.Fatal("記模式")
	}
	for i := 0; i < 250; i++ {
		launchRememberSelection(&p, filepath.Join("/x", itoa(i)), id)
	}
	if len(p.Folders) != launchMaxPrefs {
		t.Fatalf("最多 200 筆：%d", len(p.Folders))
	}

	tg := launchTarget{ID: id, Kind: launchDotNet, Name: "A", ProjectPath: "/p/A.csproj", RelativePath: "A.csproj", Detail: "d", Framework: "net9.0"}
	now := time.UnixMilli(1791580000123)
	name := launchLogFileName(id, now)
	if !launchIsLogFileName(name) || launchIsLogFileName("../"+name) {
		t.Fatalf("輸出檔名：%s", name)
	}
	rec := launchToRecord(tg, &now, true, name, []launchProcRef{{Pid: 5, Created: 9}})
	back := launchTargetFromRecord(rec)
	if back == nil || *back != tg || launchSinceFromRecord(rec).UnixMilli() != now.UnixMilli() {
		t.Fatalf("記錄還原：%+v", back)
	}
	rec.TargetID = "t000000000000"
	if launchTargetFromRecord(rec) != nil {
		t.Fatal("鍵和專案檔對不上要略過")
	}
}

func TestLaunchRender(t *testing.T) {
	a := launchTarget{ID: "ta", Name: "A", RelativePath: "A.csproj"}
	b := launchTarget{ID: "tb", Name: "B", Kind: launchElectron, RelativePath: "b/package.json"}
	since := time.UnixMilli(1000)
	rel := launchRelease
	runs := map[string]*launchSnapshot{"tb": {Phase: launchRunning, Since: &since, LogSeq: 7, LogEpoch: 2, RunMode: &rel}}
	out := renderLaunch("/p", []launchTarget{a, b}, "tb", func(id string) *launchSnapshot { return runs[id] }, func(string) launchMode { return launchDebug })
	if out["selected"] != "tb" || out["phase"] != "running" || out["runMode"] != "release" || out["mode"] != "debug" || out["since"] != int64(1000) || out["logSeq"] != int64(7) {
		t.Fatalf("LaunchView：%v", out)
	}
	ts := out["targets"].([]any)
	if ts[0].(map[string]any)["phase"] != "idle" || ts[1].(map[string]any)["kind"] != "electron" {
		t.Fatalf("目標清單：%v", ts)
	}
	if renderLaunch("/p", nil, "", nil, nil) != nil {
		t.Fatal("沒有目標是 nil")
	}
}

func TestLaunchResolveExecutable(t *testing.T) {
	exists := func(p string) bool { return p == "/b/npm" || p == "/abs/x" }
	if launchResolveExecutable("npm", "rel:/a:/b", exists) != "/b/npm" {
		t.Fatal("PATH 上找")
	}
	if launchResolveExecutable("npm", "rel", exists) != "" || launchResolveExecutable("a/npm", "/b", exists) != "" {
		t.Fatal("相對路徑不找")
	}
	if launchResolveExecutable("/abs/x", "", exists) != "/abs/x" {
		t.Fatal("完整路徑")
	}
}
