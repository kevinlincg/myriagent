package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	myriagent "github.com/kevinlincg/myriagent"
)

//go:embed bridge.js
var bridgeJS []byte

const cookieName = "myriagent_t"

// server 在本機提供介面網頁與 /ws：只聽 127.0.0.1，另以存在資料夾的權杖擋掉同一台電腦上其他網頁的跨站請求
// （第一次以 /?t=<權杖> 打開，之後靠 cookie）。
type server struct {
	hub   *Hub
	token string
	web   fs.FS
}

func loadToken(root string) (string, error) {
	file := filepath.Join(root, "agent.token")
	if b, err := os.ReadFile(file); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	t := hex.EncodeToString(raw)
	if err := os.WriteFile(file, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func newServer(hub *Hub, token string) (*server, error) {
	sub, err := fs.Sub(myriagent.Web, "web")
	if err != nil {
		return nil, err
	}
	return &server{hub: hub, token: token, web: sub}, nil
}

func (s *server) authorized(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 進入點：/?t=<權杖> 換成 cookie 再轉回 /
	if t := r.URL.Query().Get("t"); t != "" && r.URL.Path == "/" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) != 1 {
			http.Error(w, "權杖不對", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 400 * 24 * 3600})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("請用 myriagent agent 印出的網址打開（帶 ?t=…）。\n"))
		return
	}
	switch {
	case r.URL.Path == "/ws":
		s.serveWS(w, r)
	case r.URL.Path == "/bridge.js":
		s.headers(w)
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(bridgeJS)
	case r.URL.Path == "/media":
		s.hub.serveMedia(w, r)
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		s.serveIndex(w)
	default:
		s.serveStatic(w, r)
	}
}

func (s *server) headers(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; font-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
}

// serveIndex：在 app.js 之前插入 bridge.js（一般的 script 會比 type=module 的先執行，transport.js 載入時就看得到 chrome.webview），
// 根元素的深淺色照設定先寫好（init 到達前的第一個影格就是對的）
func (s *server) serveIndex(w http.ResponseWriter) {
	b, err := fs.ReadFile(s.web, "index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b = bytes.Replace(b, []byte(`<script type="module" src="app.js"></script>`), []byte(`<script src="bridge.js"></script>
  <script type="module" src="app.js"></script>`), 1)
	theme := s.hub.themeAttr()
	b = bytes.Replace(b, []byte(`data-theme="dark"`), []byte(`data-theme="`+theme+`"`), 1)
	s.headers(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *server) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(s.web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.headers(w)
	ct := mime.TypeByExtension(path.Ext(name))
	if path.Ext(name) == ".js" || path.Ext(name) == ".mjs" {
		ct = "text/javascript; charset=utf-8"
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	_, _ = w.Write(b)
}

func (s *server) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil) // 預設只接受同一個來源（Origin 要等於 Host）
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	v := s.hub.addView(func(b []byte) error {
		wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, b)
	})
	defer s.hub.removeView(v)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) && !errors.Is(err, context.Canceled) {
				log.Printf("ws 讀取結束：%v", err)
			}
			return
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		s.hub.handle(v, msg)
	}
}

// listen 聽 127.0.0.1:port；port 被占用時往後找最多 20 個
func listen(port int) (net.Listener, error) {
	var last error
	for p := port; p < port+20; p++ {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(p)))
		if err == nil {
			return l, nil
		}
		last = err
	}
	return nil, last
}
