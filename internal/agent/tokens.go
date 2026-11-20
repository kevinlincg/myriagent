package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 側欄帳戶卡的 token 計數（照 C# 的 Usage/TokenTallyRules.cs、TokenTallyStore.cs）：各家 CLI 每次 API 呼叫回報的用量換成增量，
// 依帳戶、依本機日期累加，存在 usage-tokens.json、保留 30 天。只記這台 app 經手的對話（直接在終端機用 CLI 的看不到）。
// 數字一律來自 CLI 回報的真實用量：算不出增量時寧可少算，不推估。
//
// 給各家 Provider 用的入口：h.recordTokenUsage(帳戶, 模型, tokenDelta{…})（持有 h.mu）；同一筆也加進每週 5% 分段紀錄（usagesegments.go）。

const tokenRetentionDays = 30

// tokenDelta：一筆增量。Input 含快取（Claude 是 input＋cache_creation＋cache_read），Cached 是其中的快取讀取，CacheWrite 是其中的快取寫入
type tokenDelta struct {
	Input, Cached, CacheWrite, Output int64
}

func (d tokenDelta) zero() bool {
	return d.Input == 0 && d.Cached == 0 && d.Output == 0 && d.CacheWrite == 0
}

// tokenIncrement：累計值從 prev 變成 cur 的增量：逐欄相減、負的當 0（累計值變小代表換了基準，不倒扣）
func tokenIncrement(prev, cur tokenDelta) tokenDelta {
	sub := func(a, b int64) int64 {
		if a-b < 0 {
			return 0
		}
		return a - b
	}
	return tokenDelta{sub(cur.Input, prev.Input), sub(cur.Cached, prev.Cached), sub(cur.CacheWrite, prev.CacheWrite), sub(cur.Output, prev.Output)}
}

type tokenDay struct {
	Input  int64 `json:"input"`
	Cached int64 `json:"cached"`
	Output int64 `json:"output"`
}

type tokenTally struct {
	path     string
	Days     map[string]map[string]*tokenDay `json:"days"` // 本機日期 → 帳戶 → 計數
	loaded   bool
	dirty    bool
	readOnly bool
	lastDay  string
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func dayKeys(today time.Time, n int) []string {
	out := make([]string, 0, n)
	y, m, d := today.Date()
	base := time.Date(y, m, d, 12, 0, 0, 0, today.Location())
	for i := 0; i < n; i++ {
		out = append(out, dayKey(base.AddDate(0, 0, -i)))
	}
	return out
}

// expiredDays：超過保留天數的日期鍵（比今天晚的不刪：使用者把時鐘往回調時，往後的資料留著）
func expiredDays(keys []string, today time.Time) []string {
	keep := map[string]bool{}
	for _, k := range dayKeys(today, tokenRetentionDays) {
		keep[k] = true
	}
	t := dayKey(today)
	var out []string
	for _, k := range keys {
		if !keep[k] && k < t {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (t *tokenTally) load() {
	if t.loaded {
		return
	}
	t.loaded = true
	if b, err := os.ReadFile(t.path); err == nil {
		if json.Unmarshal(b, t) != nil {
			t.readOnly = true
		}
	} else if !os.IsNotExist(err) {
		t.readOnly = true
	}
	if t.Days == nil {
		t.Days = map[string]map[string]*tokenDay{}
	}
}

func (t *tokenTally) add(accountID string, now time.Time, d tokenDelta) {
	if accountID == "" || (d.Input == 0 && d.Cached == 0 && d.Output == 0) {
		return
	}
	t.load()
	k := dayKey(now)
	day := t.Days[k]
	if day == nil {
		day = map[string]*tokenDay{}
		t.Days[k] = day
		keys := make([]string, 0, len(t.Days))
		for x := range t.Days {
			keys = append(keys, x)
		}
		for _, x := range expiredDays(keys, now) {
			delete(t.Days, x)
		}
	}
	c := day[accountID]
	if c == nil {
		c = &tokenDay{}
		day[accountID] = c
	}
	c.Input += max64(0, d.Input)
	c.Cached += max64(0, d.Cached)
	c.Output += max64(0, d.Output)
	t.dirty = true
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// sum：某帳戶從 now 往回 days 天（含今天）的合計
func (t *tokenTally) sum(accountID string, now time.Time, days int) tokenDay {
	t.load()
	var out tokenDay
	for _, k := range dayKeys(now, days) {
		if c := t.Days[k][accountID]; c != nil {
			out.Input += c.Input
			out.Cached += c.Cached
			out.Output += c.Output
		}
	}
	return out
}

func (t *tokenTally) flush() {
	if !t.dirty || !t.loaded {
		return
	}
	t.dirty = false
	if t.readOnly {
		return
	}
	writeJSON(t.path, t)
}

// tokensView：init 與 tokens 訊息的 Tokens
func (h *Hub) tokensView(now time.Time) map[string]any {
	t := h.tally()
	accounts := map[string]any{}
	for _, a := range h.accounts {
		accounts[a.ID] = map[string]any{
			"today": t.sum(a.ID, now, 1),
			"week":  t.sum(a.ID, now, 7),
			"month": t.sum(a.ID, now, 30),
		}
	}
	return map[string]any{"day": dayKey(now), "accounts": accounts}
}

func (h *Hub) tally() *tokenTally {
	r := h.ar()
	if r.tokens == nil {
		r.tokens = &tokenTally{path: filepath.Join(h.root, "usage-tokens.json")}
	}
	return r.tokens
}

// recordTokenUsage：會話回報的一筆 token 增量（各家 Provider 呼叫；持有 h.mu）。記進側欄的計數與每週 5% 分段紀錄，稍後送 tokens 給網頁
func (h *Hub) recordTokenUsage(accountID, model string, d tokenDelta) {
	if accountID == "" || d.zero() {
		return
	}
	h.tally().add(accountID, time.Now(), d)
	h.segments().addTokens(accountID, model, d)
	h.scheduleTokensPush()
}

// scheduleTokensPush：同一段時間的多筆合併成一次送出（最多每 200 毫秒一次）
func (h *Hub) scheduleTokensPush() {
	r := h.ar()
	if r.tokensPending {
		return
	}
	r.tokensPending = true
	time.AfterFunc(200*time.Millisecond, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.ar().tokensPending = false
		h.pushTokens()
	})
}

func (h *Hub) pushTokens() {
	h.broadcast(map[string]any{"type": "tokens", "tokens": h.tokensView(time.Now())})
}

func init() {
	registerInitField("tokens", func(h *Hub, v *View) any { return h.tokensView(time.Now()) })
	registerStartHook(func(h *Hub) {
		// 跨了本機日期就重送（30 秒內）；有變動就存檔
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for now := range t.C {
			h.mu.Lock()
			tl := h.tally()
			if d := dayKey(now); tl.lastDay != "" && tl.lastDay != d {
				h.pushTokens()
			}
			tl.lastDay = dayKey(now)
			tl.flush()
			h.segments().flush()
			h.mu.Unlock()
		}
	})
}
