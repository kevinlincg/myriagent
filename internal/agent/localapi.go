package agent

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 對話裡的代理找這台 agent 的本機介面（取代 C# 版的具名管道 MYRIAGENT_REMOTE_PIPE；myriagent schedule、之後的 myriagent remote 共用）。
//
// 對話的 CLI 經環境變數拿到：
//   - MYRIAGENT_AGENT_URL：這台 agent 的位址（http://127.0.0.1:<port>）
//   - MYRIAGENT_REMOTE_TOKEN：這個對話的權杖＝HMAC-SHA256(agent 這次執行的亂數, 對話 id)，只有這個 agent 開的對話拿得到，重開之後舊的就失效
//   - MYRIAGENT_AGENT_CONVERSATION：對話 id（背景程式也靠它認出是哪個對話開的）
//
// 協定：POST <MYRIAGENT_AGENT_URL>/local，本文是一個 JSON 物件 {v:1, op, conversation, token, …}（同 C# 管道的第一行）；
// 回應是一行一個 JSON（application/x-ndjson），錯誤一律是 {t:"error", code, message}（code：invalid、token、unknown）。
// 不看 cookie（CLI 沒有），改看權杖；帶 Origin 標頭的（瀏覽器）一律拒絕，網頁不能借道這裡。
// 各操作以 registerLocalOp 登記；處理時不持有 h.mu（可能要串流很久），要碰中樞狀態自己拿鎖。

const (
	localAPIVersion  = 1
	localMaxRequest  = 256 << 10
	envAgentURL      = "MYRIAGENT_AGENT_URL"
	envRemoteToken   = "MYRIAGENT_REMOTE_TOKEN"
	envConversation  = "MYRIAGENT_AGENT_CONVERSATION"
	localErrInvalid  = "invalid"
	localErrToken    = "token"
	localErrUnknown  = "unknown"
	localContentType = "application/x-ndjson; charset=utf-8"
)

// localOps：本機介面的操作（op → 處理）。emit 送出一行；回傳之後回應就結束
var localOps = map[string]func(ctx context.Context, h *Hub, req map[string]any, emit func(any) error){}

func registerLocalOp(op string, f func(ctx context.Context, h *Hub, req map[string]any, emit func(any) error)) {
	localOps[op] = f
}

// localKey：這次執行的亂數（第一次用到時產生；呼叫時不必持有 h.mu）
func (h *Hub) localKey() []byte {
	h.localOnce.Do(func() {
		h.localSecret = make([]byte, 32)
		_, _ = rand.Read(h.localSecret)
	})
	return h.localSecret
}

// localToken：對話的權杖
func (h *Hub) localToken(convID string) string {
	m := hmac.New(sha256.New, h.localKey())
	m.Write([]byte(convID))
	return hex.EncodeToString(m.Sum(nil))
}

func (h *Hub) verifyLocalToken(convID, token string) bool {
	if convID == "" || token == "" {
		return false
	}
	return hmac.Equal([]byte(h.localToken(convID)), []byte(token))
}

// SetLocalURL：Main 開始聽之後告訴中樞自己的位址（給對話的 CLI）
func (h *Hub) SetLocalURL(u string) {
	h.mu.Lock()
	h.localURL = u
	h.mu.Unlock()
}

// agentEnv：對話 CLI 要多帶的環境變數（呼叫時持有 h.mu）。各家 Provider 啟動程序時都要附上；
// myriagent 執行檔所在的資料夾放到 PATH 的最前面，代理直接打 myriagent schedule 就找得到同一版
func (h *Hub) agentEnv(c *Conv) []string {
	env := []string{envConversation + "=" + c.Meta.ID, envRemoteToken + "=" + h.localToken(c.Meta.ID)}
	if h.localURL != "" {
		env = append(env, envAgentURL+"="+h.localURL)
	}
	if exe, err := os.Executable(); err == nil {
		env = append(env, "PATH="+prependPath(filepath.Dir(exe), os.Getenv("PATH")))
	}
	return env
}

// agentEnvMap：agentEnv 換成 map 再加上 extra（ACP 的代理以 map 組環境變數）；呼叫時持有 h.mu
func (h *Hub) agentEnvMap(c *Conv, extra map[string]string) map[string]string {
	m := map[string]string{}
	for _, kv := range h.agentEnv(c) {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// prependPath 把資料夾放到 PATH 的最前面（已經在第一個就不重複）
func prependPath(dir, path string) string {
	if path == "" {
		return dir
	}
	first := strings.TrimRight(strings.SplitN(path, string(os.PathListSeparator), 2)[0], "/")
	if first == strings.TrimRight(dir, "/") {
		return path
	}
	return dir + string(os.PathListSeparator) + path
}

// serveLocal：POST /local
func (h *Hub) serveLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Origin") != "" {
		http.Error(w, "只給對話裡的代理用", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", localContentType)
	w.Header().Set("Cache-Control", "no-store")
	bw := bufio.NewWriter(w)
	flusher, _ := w.(http.Flusher)
	emit := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := bw.Write(append(b, '\n')); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	fail := func(code, message string) { _ = emit(map[string]any{"t": "error", "code": code, "message": message}) }
	body, err := io.ReadAll(io.LimitReader(r.Body, localMaxRequest+1))
	if err != nil || len(body) > localMaxRequest {
		fail(localErrInvalid, "要求是空的或太長")
		return
	}
	req, problem := parseLocalRequest(body)
	if problem != "" {
		fail(localErrInvalid, problem)
		return
	}
	if !h.verifyLocalToken(str(req, "conversation"), str(req, "token")) {
		fail(localErrToken, "權杖對不上：這台的 Myriagent 重新啟動過，讓對話重新開始一輪（CLI 以新的權杖重開）再試")
		return
	}
	f := localOps[str(req, "op")]
	if f == nil {
		fail(localErrInvalid, "不認得的操作")
		return
	}
	f(r.Context(), h, req, emit)
}

// parseLocalRequest：檢查版本、op、對話與權杖都有；不合格回原因
func parseLocalRequest(body []byte) (map[string]any, string) {
	var req map[string]any
	if strings.TrimSpace(string(body)) == "" {
		return nil, "要求是空的或太長"
	}
	if json.Unmarshal(body, &req) != nil || req == nil {
		return nil, "要求不是 JSON 物件"
	}
	if v, ok := req["v"].(float64); !ok || v != localAPIVersion {
		return nil, "myriagent 與這台的 Myriagent 版本不一致，請在對話裡重新開始（讓 CLI 以新版重開）"
	}
	if str(req, "op") == "" {
		return nil, "不認得的操作"
	}
	if str(req, "conversation") == "" || str(req, "token") == "" {
		return nil, "少了對話或權杖：只能在 Myriagent 的對話裡執行"
	}
	return req, ""
}
