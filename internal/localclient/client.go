// Package localclient：對話裡的代理（myriagent schedule、myriagent remote）呼叫這台 agent 的本機介面（internal/agent/localapi.go）。
// 位址、權杖與對話 id 從環境變數拿（agent 啟動 CLI 時給的）。
package localclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Env：從環境變數讀到的連線資訊
type Env struct {
	URL          string
	Token        string
	Conversation string
}

// ErrNotInConversation：不在 Myriagent 的對話裡（環境變數不齊）
var ErrNotInConversation = errors.New("只能在 Myriagent 的對話裡執行（找不到 MYRIAGENT_AGENT_URL、MYRIAGENT_REMOTE_TOKEN 或 MYRIAGENT_AGENT_CONVERSATION；agent 剛更新時，讓對話重新開始一輪再試）")

// FromEnv 讀環境變數；不齊回 ErrNotInConversation
func FromEnv() (Env, error) {
	e := Env{URL: os.Getenv("MYRIAGENT_AGENT_URL"), Token: os.Getenv("MYRIAGENT_REMOTE_TOKEN"), Conversation: os.Getenv("MYRIAGENT_AGENT_CONVERSATION")}
	if e.URL == "" || e.Token == "" || e.Conversation == "" {
		return e, ErrNotInConversation
	}
	return e, nil
}

// Call 送一個要求（op 與其他欄位；v、conversation、token 由這裡補），回應的每一行交給 onLine（回 false 停止讀取）。
// 連不上、連線中斷回錯誤（呼叫端以結束碼 3 結束）
func (e Env) Call(ctx context.Context, op string, fields map[string]any, onLine func(map[string]any) bool) error {
	req := map[string]any{}
	for k, v := range fields {
		req[k] = v
	}
	req["v"], req["op"], req["conversation"], req["token"] = 1, op, e.Conversation, e.Token
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.URL, "/")+"/local", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return fmt.Errorf("連不上這台的 Myriagent（%v）；agent 要開著，而且是在它的對話裡執行", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("這台的 Myriagent 回了 %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	got := false
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			return errors.New("這台的 Myriagent 的回覆看不懂")
		}
		got = true
		if !onLine(m) {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("和這台的 Myriagent 的連線中斷（%v）", err)
	}
	if !got {
		return errors.New("和這台的 Myriagent 的連線中斷")
	}
	return nil
}
