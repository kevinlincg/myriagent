package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func jsonMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUsageSeverity(t *testing.T) {
	for _, c := range []struct {
		p      float64
		locked bool
		want   string
	}{{0, false, "ok"}, {49.9, false, "ok"}, {50, false, "warn"}, {80, false, "high"}, {95, false, "max"}, {10, true, "max"}} {
		if got := usageSeverity(c.p, c.locked); got != c.want {
			t.Errorf("%v %v：%s", c.p, c.locked, got)
		}
	}
}

func TestParseClaudeUsageLimits(t *testing.T) {
	doc := jsonMap(t, `{"limits":[
		{"kind":"weekly_all","percent":41.26,"resets_at":"2026-11-25T10:00:00Z"},
		{"kind":"session","percent":12,"resets_at":1795000000},
		{"kind":"weekly_scoped","percent":100,"severity":"blocked","scope":{"model":{"display_name":"Opus"}}},
		{"kind":"weekly_all","percent":99}
	],"extra_usage":{"is_enabled":true,"used_credits":1.5,"monthly_limit":20}}`)
	ws, note := parseClaudeUsage(doc)
	if len(ws) != 3 || ws[0].Key != "session" || ws[1].Key != "weekly" || ws[2].Key != "weekly:Opus" {
		t.Fatalf("時間窗：%+v", ws)
	}
	if ws[1].Percent != 41.3 || ws[0].ResetsAt != 1795000000000 || !ws[2].Locked || ws[2].Label != "每週・Opus" {
		t.Fatalf("內容：%+v", ws)
	}
	if ws[1].ResetsAt != time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatal(ws[1].ResetsAt)
	}
	if note != "已啟用額外用量（本月 1.5／20）" {
		t.Fatal(note)
	}
	// 沒有 limits：退回具名欄位
	ws, _ = parseClaudeUsage(jsonMap(t, `{"five_hour":{"utilization":5},"seven_day_sonnet":{"utilization":120}}`))
	if len(ws) != 2 || ws[0].Key != "session" || ws[1].Key != "weekly:Sonnet" || ws[1].Percent != 100 {
		t.Fatalf("具名欄位：%+v", ws)
	}
}

func TestParseClaudeRateLimitEvent(t *testing.T) {
	ws := parseClaudeRateLimitEvent(jsonMap(t, `{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.25,"resetsAt":1795000000},"seven_day":{"utilization":0.5}}}`))
	if len(ws) != 2 || ws[0].Percent != 25 || ws[0].ResetsAt != 1795000000000 || ws[1].Percent != 50 || ws[0].Locked {
		t.Fatalf("%+v", ws)
	}
	// 每週・Opus 被拒、沒有附使用率：補一個 100% 已鎖定的
	ws = parseClaudeRateLimitEvent(jsonMap(t, `{"status":"rejected","rateLimitType":"seven_day_opus","resetsAt":1795000000,"unifiedWindows":{"five_hour":{"utilization":0.1}}}`))
	if len(ws) != 2 || ws[1].Key != "weekly:Opus" || !ws[1].Locked || ws[1].Percent != 100 || ws[0].Locked {
		t.Fatalf("%+v", ws)
	}
	// 沒帶類型與重置時間的 429 是暫時性的，不鎖
	ws = parseClaudeRateLimitEvent(jsonMap(t, `{"status":"rejected","unifiedWindows":{"five_hour":{"utilization":0.1}}}`))
	if len(ws) != 1 || ws[0].Locked {
		t.Fatalf("%+v", ws)
	}
}

func TestParseCodexRateLimits(t *testing.T) {
	r := jsonMap(t, `{"rateLimitsByLimitId":{
		"codex":{"limitId":"codex","primary":{"usedPercent":30,"windowDurationMins":300,"resetsAt":1795000000},"secondary":{"usedPercent":70,"windowDurationMins":10080},
			"credits":{"hasCredits":true,"unlimited":false,"balance":"12345.678"}},
		"gpt":{"limitId":"gpt","limitName":"GPT-6","rateLimitReachedType":"workspace_owner_credits_depleted","primary":{"usedPercent":40,"windowDurationMins":10080}}},
		"rateLimitResetCredits":{"availableCount":2,"credits":[{"status":"available","expiresAt":1795000000},{"status":"redeemed","expiresAt":1700000000},{"expiresAt":1796000000}]}}`)
	ws, credits := parseCodexRateLimits(r)
	keys := []string{}
	for _, w := range ws {
		keys = append(keys, w.Key)
	}
	if strings.Join(keys, ",") != "codex:300,codex:10080,gpt:10080" {
		t.Fatalf("排序：%v", keys)
	}
	if credits != "12,345.68" {
		t.Fatal(credits)
	}
	if ws[2].Label != "每週・GPT-6" || !ws[2].Locked || ws[0].Locked {
		t.Fatalf("桶回報已達上限卻沒有時間窗用滿：整個桶都算：%+v", ws)
	}
	if n, exp := parseCodexResetCredits(r); n == nil || *n != 2 || exp != 1795000000000 {
		t.Fatalf("免費重置：%v %d", n, exp)
	}
	if d := describeCodexBlock(r); d != "GPT-6：工作區的點數已用完" {
		t.Fatal(d)
	}
	// ordinaryUsageAllowed=false：全部鎖定
	ws, _ = parseCodexRateLimits(jsonMap(t, `{"ordinaryUsageAllowed":false,"rateLimits":{"primary":{"usedPercent":1,"windowDurationMins":60}}}`))
	if len(ws) != 1 || !ws[0].Locked || ws[0].Label != "1 小時" {
		t.Fatalf("%+v", ws)
	}
	if !hasCodexRateLimits(jsonMap(t, `{"rateLimits":{}}`)) || hasCodexRateLimits(jsonMap(t, `{}`)) {
		t.Fatal("hasCodexRateLimits")
	}
}

func TestParseAntigravityUsage(t *testing.T) {
	data := jsonMap(t, `{"groups":[
		{"name":"Claude and GPT models","buckets":[{"id":"3p-weekly","window":"weekly","remaining_fraction":0.25},{"id":"3p-5h","window":"5h"}]},
		{"name":"Gemini Models","buckets":[{"id":"agy-gemini-5h","window":"5h","remaining_fraction":1,"reset_time":"2026-11-21T00:00:00Z"},
			{"id":"gemini-count","name":"Daily","remaining_amount":42},{"id":"off","disabled":true}]}]}`)
	ws, note, ok := parseAntigravityUsage(data)
	if !ok {
		t.Fatal("看不懂")
	}
	keys := []string{}
	for _, w := range ws {
		keys = append(keys, w.Key)
	}
	if strings.Join(keys, ",") != "agy-gemini-5h,agy-3p-5h,agy-3p-weekly" {
		t.Fatalf("%v", keys)
	}
	if ws[0].Percent != 0 || ws[1].Percent != 100 || !ws[1].Locked || ws[2].Percent != 75 || ws[2].Label != "每週・Claude/GPT" {
		t.Fatalf("%+v", ws)
	}
	if note != "每日・Gemini 剩餘 42 次" {
		t.Fatal(note)
	}
	if _, _, ok := parseAntigravityUsage(jsonMap(t, `{"x":1}`)); ok {
		t.Fatal("沒有 groups 應該看不懂")
	}
}

func TestParseGrokBilling(t *testing.T) {
	ws, note, ok := parseGrokBilling(jsonMap(t, `{"subscription_tier":"SuperGrok","config":{"creditUsagePercent":100,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-11-27T00:00:00Z"},
		"onDemandCap":{"val":5000},"onDemandUsed":{"val":1234},"prepaidBalance":{"val":0}}}`))
	if !ok || len(ws) != 1 || ws[0].Key != grokCreditsKey || ws[0].Label != "每週" || ws[0].Percent != 100 || ws[0].Locked {
		t.Fatalf("%+v", ws)
	}
	if note != "隨用隨付已用 $12.34／上限 $50.00" {
		t.Fatal(note)
	}
	ws, _, _ = parseGrokBilling(jsonMap(t, `{"config":{"monthlyLimit":{"val":1000},"used":{"val":1000}}}`))
	if ws[0].Percent != 100 || !ws[0].Locked || ws[0].Label != "額度" {
		t.Fatalf("舊回應以 used／monthlyLimit 推算：%+v", ws)
	}
	if _, _, ok := parseGrokBilling(jsonMap(t, `{}`)); ok {
		t.Fatal("沒有 config 應該看不懂")
	}
}

func TestMergeAndResets(t *testing.T) {
	old := []usageWindow{{Key: "session", Label: "5 小時", Percent: 10, ResetsAt: 1000}, {Key: "weekly", Label: "每週", Percent: 20, ResetsAt: 5000}}
	merged := mergeUsageWindows(old, []usageWindow{{Key: "session", Label: "x", Percent: 30}, {Key: "weekly:Opus", Label: "每週・Opus", Percent: 1}})
	if len(merged) != 3 || merged[0].Label != "5 小時" || merged[0].Percent != 30 || merged[0].ResetsAt != 1000 || merged[2].Key != "weekly:Opus" {
		t.Fatalf("%+v", merged)
	}
	s := &usageSnapshot{Source: "api", Windows: old}
	r := applyDueResets(s, 2000)
	if r == nil || r.Windows[0].Percent != 0 || r.Windows[0].ResetsAt != 0 || r.Windows[1].Percent != 20 || !r.Stale || r.Source != "cache" {
		t.Fatalf("%+v", r)
	}
	if applyDueResets(s, 500) != nil {
		t.Fatal("還沒到重置時間")
	}
	a := &usageSnapshot{Source: "session", Windows: old}
	b := &usageSnapshot{Source: "session", Windows: append([]usageWindow{}, old...), FetchedAt: 99}
	if !sameUsageForView(a, b) {
		t.Fatal("取得時間不算")
	}
	b.Windows[0].Percent = 11
	if sameUsageForView(a, b) {
		t.Fatal("使用率變了")
	}
	if g := guessProviderFromUsageKeys([]string{"codex:300", "codex:10080"}); g != "Codex" {
		t.Fatal(g)
	}
	if g := guessProviderFromUsageKeys([]string{"session", "codex:300"}); g != "" {
		t.Fatal(g)
	}
}

func TestUsageHistoryPoints(t *testing.T) {
	pts := map[string][][2]float64{}
	const min = 60_000
	if !addHistoryPoint(pts, "k", 1000, 10) {
		t.Fatal("第一點")
	}
	if addHistoryPoint(pts, "k", 1000+5*min, 10.2) {
		t.Fatal("沒變、不到 30 分鐘不記")
	}
	if !addHistoryPoint(pts, "k", 1000+6*min, 11) {
		t.Fatal("變了 0.5 以上要記")
	}
	if addHistoryPoint(pts, "k", 1000, 50) {
		t.Fatal("時間要嚴格遞增")
	}
	if !addHistoryPoint(pts, "k", 1000+40*min, 11) {
		t.Fatal("超過 30 分鐘要記")
	}
	now := int64(10 * 24 * 3600 * 1000)
	var many [][2]float64
	for i := 0; i < 300; i++ {
		many = append(many, [2]float64{float64(now - int64(i)*1800_000), float64(i)})
	}
	view := downsampleHistory(many, now)
	if len(view) > usageMaxViewPoints || view[len(view)-1][0] != float64(now) {
		t.Fatalf("取樣：%d 點，最後 %v", len(view), view[len(view)-1])
	}
	for i := 1; i < len(view); i++ {
		if view[i][0] <= view[i-1][0] {
			t.Fatal("時間要遞增")
		}
	}

	dir := t.TempDir()
	h := newUsageHistory(dir)
	s := &usageSnapshot{FetchedAt: now, Source: "api", Windows: []usageWindow{{Key: "weekly", Label: "每週", Percent: 33}}}
	h.record("a1", s, now)
	h.flush()
	h2 := newUsageHistory(dir)
	if last := h2.lastSnapshot("a1"); last == nil || last.Windows[0].Percent != 33 {
		t.Fatalf("重新載入：%+v", last)
	}
	if v := h2.view("a1", "weekly", now); len(v) != 1 {
		t.Fatal(v)
	}
	if ids := h2.accountIDs(); len(ids) != 1 || ids[0] != "a1" {
		t.Fatal(ids)
	}
	h2.remove("a1")
	if len(newUsageHistory(dir).accountIDs()) != 0 {
		t.Fatal("移除帳戶要刪掉歷史檔")
	}
}
