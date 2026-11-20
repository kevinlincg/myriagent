package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileScoreTiers(t *testing.T) {
	exact := fileScore("app", "tools/app")
	prefix := fileScore("app", "src/app.js")
	contains := fileScore("app", "lib/myapp.ts")
	pathContains := fileScore("app", "app/index.html")
	subsequence := fileScore("app", "a/plan/p.txt")
	if !(exact > prefix && prefix > contains && contains > pathContains && pathContains > subsequence && subsequence > 0) {
		t.Fatalf("級距不對：%d %d %d %d %d", exact, prefix, contains, pathContains, subsequence)
	}
	if fileScore("app", "readme.md") != 0 {
		t.Fatal("字元沒有依序出現就不命中")
	}
	if fileScore("APP", "src/App.js") != prefix {
		t.Fatal("不分大小寫")
	}
	if fileScore("app", "src/app.js") <= fileScore("app", "src/deep/folder/app.js") {
		t.Fatal("路徑短的優先")
	}
	if fileScore("ab", "x/a_b") <= fileScore("ab", "x/axxb") {
		t.Fatal("緊接在分隔字元後面加分")
	}
}

func TestSearchFilesOrderAndSlashes(t *testing.T) {
	files := []string{"a/plan/p.txt", "readme.md", "app/index.html", "lib/myapp.ts", "src/app.js", "tools/app"}
	res := searchFiles(files, "/proj", "app", 30)
	var got []string
	for _, r := range res {
		got = append(got, r.RelativePath)
	}
	want := []string{"tools/app", "src/app.js", "lib/myapp.ts", "app/index.html", "a/plan/p.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("排序不對：%v", got)
	}
	if res[1].FullPath != "/proj/src/app.js" || res[1].Kind != "code" {
		t.Fatalf("完整路徑或種類不對：%+v", res[1])
	}
	if len(searchFiles(files, "/proj", "app", 2)) != 2 {
		t.Fatal("筆數上限")
	}
	files = []string{"src/web/app.js", "b.txt", "docs/guide.md"}
	if r := searchFiles(files, "/p", `web\app`, 30); len(r) != 1 || r[0].RelativePath != "src/web/app.js" {
		t.Fatalf("反斜線也要當分隔字元：%v", r)
	}
	got = nil
	for _, r := range searchFiles(files, "/p", "  ", 30) {
		got = append(got, r.RelativePath)
	}
	if !reflect.DeepEqual(got, []string{"b.txt", "docs/guide.md", "src/web/app.js"}) {
		t.Fatalf("空查詢依路徑長度：%v", got)
	}
}

func TestBuildFileIndexSkipsOutputs(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.txt", "app.csproj", "src/b.cs", "src/obj/generated.cs", "node_modules/pkg/index.js", "bin/Release/app.dll",
		"obj/project.assets.json", ".git/HEAD", "Bin2/keep.txt"} {
		writeTestFile(t, root, f, "")
	}
	idx := buildFileIndex(root)
	got := append([]string{}, idx...)
	sort.Strings(got)
	want := []string{"Bin2/keep.txt", "a.txt", "app.csproj", "src/b.cs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("索引不對：%v", idx)
	}
	if idx[0] != "a.txt" && idx[0] != "app.csproj" {
		t.Fatalf("廣度優先：根目錄的檔案先列：%v", idx)
	}

	root = t.TempDir()
	for _, f := range []string{"package.json", "packages/ui/src/Button.tsx", "bin/cli.js", "dotnet/App.csproj", "dotnet/Program.cs",
		"dotnet/bin/Release/App.dll", "legacy/Legacy.sln", "legacy/packages/N/lib/N.dll"} {
		writeTestFile(t, root, f, "")
	}
	got = buildFileIndex(root)
	sort.Strings(got)
	want = []string{"bin/cli.js", "dotnet/App.csproj", "dotnet/Program.cs", "legacy/Legacy.sln", "package.json", "packages/ui/src/Button.tsx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JS 專案的 packages、bin 要索引，.NET 旁的才略過：%v", got)
	}
}

func TestListDirSortsAndMarks(t *testing.T) {
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, "zeta"), 0o755)
	_ = os.Mkdir(filepath.Join(root, "Alpha"), 0o755)
	writeTestFile(t, root, "img10.png", "1234567890")
	writeTestFile(t, root, "img2.png", "")
	writeTestFile(t, root, "img1.png", "")
	writeTestFile(t, root, "notes.md", "")
	writeTestFile(t, root, ".env", "")
	entries, truncated, err := listDir(root)
	if err != nil || truncated {
		t.Fatal(err, truncated)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "Alpha,zeta,.env,img1.png,img2.png,img10.png,notes.md" {
		t.Fatalf("順序不對：%v", names)
	}
	if entries[0].Kind != "dir" || entries[0].Size != nil {
		t.Fatal("資料夾沒有大小")
	}
	img10 := entries[5]
	if img10.Kind != "image" || *img10.Size != 10 || img10.Path != filepath.Join(root, "img10.png") {
		t.Fatalf("img10 不對：%+v", img10)
	}
	if !entries[2].Hidden || entries[6].Hidden || entries[6].Kind != "doc" {
		t.Fatal("點開頭的視為隱藏；notes.md 是 doc")
	}
	if _, _, err := listDir("relative/folder"); err == nil {
		t.Fatal("相對路徑要拒絕")
	}
	if _, _, err := listDir("//server/share"); err == nil {
		t.Fatal("// 開頭要拒絕")
	}
	if _, _, err := listDir(filepath.Join(root, "沒有")); err == nil || !strings.Contains(err.Error(), "找不到資料夾") {
		t.Fatalf("找不到資料夾：%v", err)
	}
}

func TestListDirTruncatesFilesKeepsDirs(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < fileMaxEntries+5; i++ {
		_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("a%04d.txt", i)), nil, 0o644)
	}
	_ = os.Mkdir(filepath.Join(root, "b-sub"), 0o755)
	_ = os.Mkdir(filepath.Join(root, "zz-sub"), 0o755)
	entries, truncated, _ := listDir(root)
	if !truncated || len(entries) != fileMaxEntries || entries[0].Name != "b-sub" || entries[1].Name != "zz-sub" || entries[2].Kind == "dir" {
		t.Fatalf("截斷不對：%v %d %v", truncated, len(entries), entries[:3])
	}
}

func TestNaturalCompareAndParent(t *testing.T) {
	if naturalCompare("img2", "img10") >= 0 || naturalCompare("img10", "img2") <= 0 || naturalCompare("a", "A") != 0 || naturalCompare("", "a") >= 0 {
		t.Fatal("自然排序不對")
	}
	if naturalCompare("1", "01") >= 0 {
		t.Fatal("數值相同時位數少的在前")
	}
	if parentDir("/a/b") != "/a" || parentDir("/a/b/") != "/a" || parentDir("/a") != "/" || parentDir("/") != "" {
		t.Fatal("上一層不對")
	}
	if !isSafeToOpen("/x/a.PDF") || isSafeToOpen("/x/run.sh") || isSafeToOpen("/x/a.xlsm") {
		t.Fatal("開檔白名單不對")
	}
}

func TestFileIndexCacheTrimAndRefresh(t *testing.T) {
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	fc := &fileIndexCache{entries: map[string]*fileIndexEntry{}, now: clock}
	var roots []string
	for i := 0; i < fileMaxCachedIndex+3; i++ {
		r := t.TempDir()
		writeTestFile(t, r, "f.txt", "")
		roots = append(roots, r)
		mu.Lock()
		now = now.Add(time.Second)
		mu.Unlock()
		if got := fc.get(r); len(got) != 1 {
			t.Fatalf("索引：%v", got)
		}
	}
	fc.mu.Lock()
	n := len(fc.entries)
	_, keptLast := fc.entries[roots[len(roots)-1]]
	_, keptFirst := fc.entries[roots[0]]
	fc.mu.Unlock()
	if n != fileMaxCachedIndex || !keptLast || keptFirst {
		t.Fatalf("快取上限：%d 個、最新的留著 %v、最舊的丟掉 %v", n, keptLast, !keptFirst)
	}
	// 過期：這次先回舊的，背景重建後看得到新檔
	last := roots[len(roots)-1]
	writeTestFile(t, last, "g.txt", "")
	mu.Lock()
	now = now.Add(2 * fileIndexLifetime)
	mu.Unlock()
	if got := fc.get(last); len(got) != 1 {
		t.Fatalf("過期時先回舊的索引：%v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(fc.get(last)) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("背景重建沒有完成")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
