// Package paths 決定本機資料放在哪裡。
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// DataRoot 是 agent 的資料夾：--data 優先，其次 MYRIAGENT_AGENT_DATA，
// 都沒有時 macOS 用 ~/Library/Application Support/Myriagent、Linux 用 $XDG_DATA_HOME/myriagent（預設 ~/.local/share/myriagent）
func DataRoot(flag string) string {
	if flag != "" {
		return abs(flag)
	}
	if env := os.Getenv("MYRIAGENT_AGENT_DATA"); env != "" {
		return abs(env)
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Myriagent")
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "myriagent")
	}
	return filepath.Join(home, ".local", "share", "myriagent")
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
