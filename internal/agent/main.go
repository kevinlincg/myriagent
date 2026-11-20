package agent

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/kevinlincg/myriagent/internal/paths"
)

// Version、Build 由建置時的 -ldflags 寫入
var (
	Version = "0.3.0"
	Build   = "dev"
)

// Main 是 myriagent agent
func Main(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	data := fs.String("data", "", "資料夾（預設 macOS ~/Library/Application Support/Myriagent、Linux ~/.local/share/myriagent）")
	port := fs.Int("port", 47800, "本機介面的連接埠（被占用時往後找）")
	open := fs.Bool("open", false, "啟動後用預設瀏覽器打開介面")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := paths.DataRoot(*data)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(root, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		log.SetOutput(multiWriter{os.Stderr, logFile})
	}
	token, err := loadToken(root)
	if err != nil {
		return err
	}
	hub := newHub(root)
	srv, err := newServer(hub, token)
	if err != nil {
		return err
	}
	l, err := listen(*port)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://%s/?t=%s", l.Addr().String(), token)
	// 給 Mac 的視窗與其他工具找到介面：status 檔寫網址
	writeJSON(filepath.Join(root, "agent.json"), map[string]any{"url": url, "pid": os.Getpid(), "version": Version, "build": Build})
	log.Printf("myriagent agent %s（%s）：資料夾 %s", Version, Build, root)
	fmt.Println("介面：" + url)
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpSrv.Serve(l); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP 伺服器結束：%v", err)
		}
	}()
	if *open {
		openBrowser(url)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("收到結束訊號，停止中…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	hub.shutdown()
	return nil
}

func openBrowser(url string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", url).Start()
	} else {
		_ = exec.Command("xdg-open", url).Start()
	}
}

type multiWriter []interface{ Write([]byte) (int, error) }

func (m multiWriter) Write(p []byte) (int, error) {
	for _, w := range m {
		_, _ = w.Write(p)
	}
	return len(p), nil
}
