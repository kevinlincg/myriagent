package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kevinlincg/myriagent/internal/localclient"
)

func TestLocalAPITokenAndOps(t *testing.T) {
	h, _ := newTestHub(t)
	registerLocalOp("echoTest", func(ctx context.Context, h *Hub, req map[string]any, emit func(any) error) {
		_ = emit(map[string]any{"t": "echo", "text": str(req, "text")})
		_ = emit(map[string]any{"t": "done"})
	})
	t.Cleanup(func() { delete(localOps, "echoTest") })
	srv := httptest.NewServer(&server{hub: h, token: "x"})
	defer srv.Close()

	good := localclient.Env{URL: srv.URL, Token: h.localToken("c1"), Conversation: "c1"}
	var lines []map[string]any
	if err := good.Call(context.Background(), "echoTest", map[string]any{"text": "嗨"}, func(m map[string]any) bool { lines = append(lines, m); return true }); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0]["text"] != "嗨" || lines[1]["t"] != "done" {
		t.Fatalf("回應不對：%v", lines)
	}

	// 別的對話的權杖不能用
	bad := localclient.Env{URL: srv.URL, Token: h.localToken("c2"), Conversation: "c1"}
	var got map[string]any
	_ = bad.Call(context.Background(), "echoTest", nil, func(m map[string]any) bool { got = m; return false })
	if got["t"] != "error" || got["code"] != "token" {
		t.Fatalf("權杖錯了應該拒絕：%v", got)
	}

	// 不認得的操作
	_ = good.Call(context.Background(), "nope", nil, func(m map[string]any) bool { got = m; return false })
	if got["code"] != "invalid" {
		t.Fatalf("不認得的操作應該 invalid：%v", got)
	}

	// 瀏覽器（帶 Origin）擋掉
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/local", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("帶 Origin 應該 403，得到 %d", resp.StatusCode)
	}
}

func TestAgentEnvCarriesTokenAndURL(t *testing.T) {
	h, _ := newTestHub(t)
	h.SetLocalURL("http://127.0.0.1:1")
	h.mu.Lock()
	env := h.agentEnv(&Conv{Meta: ConvMeta{ID: "cX"}})
	h.mu.Unlock()
	joined := strings.Join(env, "\n")
	for _, want := range []string{"MYRIAGENT_AGENT_CONVERSATION=cX", "MYRIAGENT_REMOTE_TOKEN=" + h.localToken("cX"), "MYRIAGENT_AGENT_URL=http://127.0.0.1:1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("少了 %s：%v", want, env)
		}
	}
	if prependPath("/a", "/a:/b") != "/a:/b" || prependPath("/a", "/b") != "/a:/b" || prependPath("/a", "") != "/a" {
		t.Fatal("prependPath 不對")
	}
}
