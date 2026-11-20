package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
)

// launchScanner：在工作資料夾找可以執行的視窗程式專案（中樞在 goroutine 呼叫；測試換成假的）
type launchScanner interface {
	// Scan 依 launchOrder 排好；資料夾不存在、是網路磁碟、沒有專案回空清單
	Scan(ctx context.Context, folder string) ([]launchTarget, error)
}

// fsLaunchScanner：只讀。從工作資料夾往下最多 3 層、最多 2000 個資料夾，讀 .csproj／.fsproj／.vbproj 與 package.json（各最多 1 MB）。
// 不跟符號連結、跳過建置輸出與套件資料夾；網路磁碟（SMB、NFS…）不讀（光是列目錄就可能很慢）
type fsLaunchScanner struct{}

func (fsLaunchScanner) Scan(ctx context.Context, folder string) ([]launchTarget, error) {
	return launchScan(ctx, folder, runtime.GOOS), nil
}

type scanDir struct {
	path  string
	depth int
}

func launchScan(ctx context.Context, folder, goos string) []launchTarget {
	if !filepath.IsAbs(folder) || folderOnNetworkFS(folder) {
		return nil
	}
	if fi, err := os.Stat(folder); err != nil || !fi.IsDir() {
		return nil
	}
	var found []launchTarget
	queue := []scanDir{{folder, 0}}
	visited := 0
	for len(queue) > 0 && visited < launchMaxDirectories {
		if ctx.Err() != nil {
			return nil
		}
		d := queue[0]
		queue = queue[1:]
		visited++
		entries, err := os.ReadDir(d.path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			full := filepath.Join(d.path, e.Name())
			symlink := e.Type()&os.ModeSymlink != 0
			if e.IsDir() {
				if d.depth < launchMaxDepth && launchShouldDescend(e.Name(), symlink) {
					queue = append(queue, scanDir{full, d.depth + 1})
				}
				continue
			}
			if symlink || !e.Type().IsRegular() || !launchIsCandidateFile(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Size() > launchMaxProjectFileBytes {
				continue
			}
			b, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(folder, full)
			if err != nil {
				rel = e.Name()
			}
			var t *launchTarget
			if launchIsDotNetProject(e.Name()) {
				t = launchFromDotNetProject(full, rel, string(b), goos)
			} else {
				t = launchFromPackageJSON(full, rel, string(b))
			}
			if t != nil {
				found = append(found, *t)
			}
		}
	}
	return launchOrder(found)
}
