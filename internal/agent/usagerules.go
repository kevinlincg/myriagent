package agent

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 用量的純函式規則（照 C# 的 Usage/UsageRules.cs）：解析 Claude 官方用量端點、Claude 對話的 rate_limit_event、Codex 的 rateLimits、
// Antigravity 的 -p /usage、Grok 的 _x.ai/billing；合併、推算重置、嚴重度。
// 時間窗的 key：Claude 為 session／weekly／weekly:模型名，Codex 為 {limitId}:{分鐘數}，Antigravity 為 agy-{bucketId}，Grok 為 grok-credits。

const (
	usageSessionKey   = "session"
	usageWeeklyKey    = "weekly"
	agyUsageKeyPrefix = "agy-"
	grokCreditsKey    = "grok-credits"
)

// usageWindow：一個額度時間窗的使用率（ResetsAt 是 Unix 毫秒，0＝不知道）
type usageWindow struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Percent  float64 `json:"percent"`
	ResetsAt int64   `json:"resetsAt,omitempty"`
	Locked   bool    `json:"locked,omitempty"`
}

// usageSnapshot：帳戶的用量快照（FetchedAt 是 Unix 毫秒，0＝沒查到過）
type usageSnapshot struct {
	FetchedAt            int64         `json:"fetchedAt"`
	Source               string        `json:"source"`
	Windows              []usageWindow `json:"windows"`
	Credits              string        `json:"credits,omitempty"`
	Note                 string        `json:"note,omitempty"`
	Error                string        `json:"error,omitempty"`
	Stale                bool          `json:"stale"`
	ResetCredits         *int          `json:"resetCredits,omitempty"`
	ResetCreditExpiresAt int64         `json:"resetCreditExpiresAt,omitempty"`
}

func emptyUsage(errText string) *usageSnapshot {
	return &usageSnapshot{Source: "cache", Windows: []usageWindow{}, Error: errText, Stale: true}
}

// usageSeverity：<50 ok、50～80 warn、80～95 high、≥95 或已鎖定 max
func usageSeverity(percent float64, locked bool) string {
	switch {
	case locked || percent >= 95:
		return "max"
	case percent >= 80:
		return "high"
	case percent >= 50:
		return "warn"
	}
	return "ok"
}

func clampPercent(p float64) float64 {
	if math.IsNaN(p) {
		return 0
	}
	return math.Round(math.Max(0, math.Min(100, p))*10) / 10
}

// ---- JSON 小工具 ----

func jnum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func jstr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func jobj(m map[string]any, k string) map[string]any {
	o, _ := m[k].(map[string]any)
	return o
}

func jbool(m map[string]any, k string) (bool, bool) {
	b, ok := m[k].(bool)
	return b, ok
}

// parseUsageTime：數字（大於 1e10 的是毫秒，其他是秒）或 ISO 時間字串 → Unix 毫秒；看不懂回 0
func parseUsageTime(v any) int64 {
	if n, ok := jnum(v); ok {
		if n > 10_000_000_000 {
			return int64(n)
		}
		return int64(n) * 1000
	}
	if s, ok := v.(string); ok && s != "" {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UnixMilli()
			}
		}
	}
	return 0
}

// ---- Claude ----

// parseClaudeUsage：GET /api/oauth/usage → 時間窗。優先用通用的 limits 清單（session／weekly_all／weekly_scoped＋模型名）；
// 沒有 limits 時退回 five_hour／seven_day／seven_day_opus／seven_day_sonnet 這幾個具名欄位（utilization 為 0～100）
func parseClaudeUsage(doc map[string]any) ([]usageWindow, string) {
	windows := []usageWindow{}
	if doc == nil {
		return windows, ""
	}
	if limits, ok := doc["limits"].([]any); ok {
		for _, x := range limits {
			l, _ := x.(map[string]any)
			if l == nil {
				continue
			}
			pct, ok := jnum(l["percent"])
			if !ok {
				continue
			}
			kind := jstr(l, "kind")
			model := jstr(jobj(jobj(l, "scope"), "model"), "display_name")
			key, label := kind, kind
			switch {
			case kind == "session":
				key, label = usageSessionKey, "5 小時"
			case kind == "weekly_all":
				key, label = usageWeeklyKey, "每週"
			case kind == "weekly_scoped" && strings.TrimSpace(model) != "":
				key, label = usageWeeklyKey+":"+model, "每週・"+model
			}
			sev := strings.ToLower(jstr(l, "severity"))
			locked := sev == "blocked" || sev == "exceeded"
			if !hasWindow(windows, key) {
				windows = append(windows, usageWindow{Key: key, Label: label, Percent: clampPercent(pct), ResetsAt: parseUsageTime(l["resets_at"]), Locked: locked})
			}
		}
	}
	if len(windows) == 0 {
		addNamed := func(w map[string]any, key, label string) {
			if u, ok := jnum(w["utilization"]); ok && w != nil {
				windows = append(windows, usageWindow{Key: key, Label: label, Percent: clampPercent(u), ResetsAt: parseUsageTime(w["resets_at"])})
			}
		}
		addNamed(jobj(doc, "five_hour"), usageSessionKey, "5 小時")
		addNamed(jobj(doc, "seven_day"), usageWeeklyKey, "每週")
		addNamed(jobj(doc, "seven_day_opus"), usageWeeklyKey+":Opus", "每週・Opus")
		addNamed(jobj(doc, "seven_day_sonnet"), usageWeeklyKey+":Sonnet", "每週・Sonnet")
	}
	note := ""
	if extra := jobj(doc, "extra_usage"); extra != nil {
		if on, _ := jbool(extra, "is_enabled"); on {
			used, uok := jnum(extra["used_credits"])
			limit, lok := jnum(extra["monthly_limit"])
			if uok && lok {
				note = fmt.Sprintf("已啟用額外用量（本月 %s／%s）", fmtNum2(used), fmtNum2(limit))
			} else {
				note = "已啟用額外用量"
			}
		}
	}
	return orderWindows(windows), note
}

// fmtNum2：最多兩位小數（同 C# 的 0.##）
func fmtNum2(f float64) string {
	return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64)
}

func hasWindow(ws []usageWindow, key string) bool {
	for _, w := range ws {
		if w.Key == key {
			return true
		}
	}
	return false
}

// isClaudeQuotaRejection：rate_limit_info 是訂閱額度被拒：status 是 rejected，而且帶 rateLimitType 或 resetsAt。
// 兩個都沒帶的是暫時性的 429（not your usage limit），不是額度用完
func isClaudeQuotaRejection(info map[string]any) bool {
	if info == nil || !strings.EqualFold(jstr(info, "status"), "rejected") {
		return false
	}
	if t, ok := info["rateLimitType"].(string); ok && strings.TrimSpace(t) != "" {
		return true
	}
	_, ok := jnum(info["resetsAt"])
	return ok
}

// parseClaudeRateLimitEvent：rate_limit_event.rate_limit_info → 時間窗更新（unifiedWindows 的 utilization 是 0～1、resetsAt 是秒）。
// 額度被拒時把 rateLimitType 對應的時間窗標為鎖定；被鎖的時間窗沒附使用率時補一個 100% 已鎖定的
func parseClaudeRateLimitEvent(info map[string]any) []usageWindow {
	out := []usageWindow{}
	if info == nil {
		return out
	}
	windows := jobj(info, "unifiedWindows")
	lockedKey, lockedLabel := "", ""
	if isClaudeQuotaRejection(info) {
		t, has := info["rateLimitType"].(string)
		switch {
		case !has || t == "five_hour":
			lockedKey, lockedLabel = usageSessionKey, "5 小時"
		case t == "seven_day":
			lockedKey, lockedLabel = usageWeeklyKey, "每週"
		case t == "seven_day_opus":
			lockedKey, lockedLabel = usageWeeklyKey+":Opus", "每週・Opus"
		case t == "seven_day_sonnet":
			lockedKey, lockedLabel = usageWeeklyKey+":Sonnet", "每週・Sonnet"
		}
	}
	add := func(w map[string]any, key, label string) {
		u, ok := jnum(w["utilization"])
		if w == nil || !ok {
			return
		}
		var resets int64
		if r, ok := jnum(w["resetsAt"]); ok {
			resets = int64(r) * 1000
		}
		out = append(out, usageWindow{Key: key, Label: label, Percent: clampPercent(u * 100), ResetsAt: resets, Locked: lockedKey == key})
	}
	add(jobj(windows, "five_hour"), usageSessionKey, "5 小時")
	add(jobj(windows, "seven_day"), usageWeeklyKey, "每週")
	if lockedKey != "" && !hasWindow(out, lockedKey) {
		var resets int64
		if r, ok := jnum(info["resetsAt"]); ok {
			resets = int64(r) * 1000
		}
		out = append(out, usageWindow{Key: lockedKey, Label: lockedLabel, Percent: 100, ResetsAt: resets, Locked: true})
	}
	return out
}

// ---- Codex ----

// codexBuckets：rateLimitsByLimitId 逐桶；沒有就用 rateLimits；rateLimits/updated 直接給一個桶的快照
func codexBuckets(r map[string]any) []map[string]any {
	var out []map[string]any
	if byID := jobj(r, "rateLimitsByLimitId"); len(byID) > 0 {
		keys := make([]string, 0, len(byID))
		for k := range byID {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if b, ok := byID[k].(map[string]any); ok {
				out = append(out, b)
			}
		}
	} else if single := jobj(r, "rateLimits"); single != nil {
		out = append(out, single)
	} else if r["primary"] != nil || r["secondary"] != nil {
		out = append(out, r)
	}
	return out
}

// describeUsageWindow：時間窗長度 → 中文（300 分＝5 小時、10080 分＝每週）
func describeUsageWindow(minutes int64, has bool) string {
	switch {
	case !has:
		return "額度"
	case minutes == 10080:
		return "每週"
	case minutes == 1440:
		return "每日"
	case minutes >= 60 && minutes%60 == 0:
		return fmt.Sprintf("%d 小時", minutes/60)
	}
	return fmt.Sprintf("%d 分鐘", minutes)
}

// parseCodexRateLimits：account/rateLimits/read 的結果（或 rateLimits/updated 的 rateLimits）→ 時間窗與點數餘額。
// 已達上限：用滿的時間窗；桶回報 rateLimitReachedType 卻沒有時間窗用滿時整個桶都算；ordinaryUsageAllowed=false 時全部都算
func parseCodexRateLimits(r map[string]any) ([]usageWindow, string) {
	windows := []usageWindow{}
	if r == nil {
		return windows, ""
	}
	allowed, hasAllowed := jbool(r, "ordinaryUsageAllowed")
	blocked := hasAllowed && !allowed
	credits := ""
	for _, b := range codexBuckets(r) {
		limitID := jstr(b, "limitId")
		if limitID == "" {
			limitID = "codex"
		}
		limitName := jstr(b, "limitName")
		reached := jstr(b, "rateLimitReachedType") != ""
		var bucket []usageWindow
		for _, slot := range []string{"primary", "secondary"} {
			w := jobj(b, slot)
			used, ok := jnum(w["usedPercent"])
			if w == nil || !ok {
				continue
			}
			mins, hasMins := jnum(w["windowDurationMins"])
			label := describeUsageWindow(int64(mins), hasMins)
			if strings.TrimSpace(limitName) != "" {
				label += "・" + limitName
			}
			suffix := slot
			if hasMins {
				suffix = strconv.FormatInt(int64(mins), 10)
			}
			var resets int64
			if r, ok := jnum(w["resetsAt"]); ok {
				resets = int64(r) * 1000
			}
			bucket = append(bucket, usageWindow{Key: limitID + ":" + suffix, Label: label, Percent: clampPercent(used), ResetsAt: resets, Locked: blocked || (reached && used >= 100)})
		}
		if reached {
			any := false
			for _, w := range bucket {
				any = any || w.Locked
			}
			if !any {
				for i := range bucket {
					bucket[i].Locked = true
				}
			}
		}
		windows = append(windows, bucket...)
		if c := jobj(b, "credits"); credits == "" && c != nil {
			unlimited, _ := jbool(c, "unlimited")
			has, _ := jbool(c, "hasCredits")
			if !unlimited && has {
				if v, err := strconv.ParseFloat(jstr(c, "balance"), 64); err == nil {
					credits = formatThousands2(v)
				}
			}
		}
	}
	return orderWindows(windows), credits
}

// formatThousands2：千分位、最多兩位小數（同 C# 的 #,0.##）
func formatThousands2(v float64) string {
	s := fmtNum2(v)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var b strings.Builder
	for i, r := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String() + frac
	}
	return b.String() + frac
}

// hasCodexRateLimits：看得懂的 Codex 用量回應（至少有一個額度桶）
func hasCodexRateLimits(r map[string]any) bool { return r != nil && len(codexBuckets(r)) > 0 }

const maxResetCredits = 1000

// parseCodexResetCredits：rateLimitResetCredits → 可用的免費重置次數與最早到期的時間（毫秒）。沒有這個欄位回 nil
func parseCodexResetCredits(r map[string]any) (*int, int64) {
	summary := jobj(r, "rateLimitResetCredits")
	n, ok := jnum(summary["availableCount"])
	if summary == nil || !ok || n != math.Trunc(n) {
		return nil, 0
	}
	count := int(math.Max(0, math.Min(maxResetCredits, n)))
	var next int64
	if rows, ok := summary["credits"].([]any); ok && count > 0 {
		for _, x := range rows {
			row, _ := x.(map[string]any)
			if row == nil {
				continue
			}
			if st := jstr(row, "status"); st == "redeemed" || st == "redeeming" {
				continue
			}
			sec, ok := jnum(row["expiresAt"])
			if !ok || sec <= 0 || sec > 253402300799 || sec != math.Trunc(sec) {
				continue
			}
			if at := int64(sec) * 1000; next == 0 || at < next {
				next = at
			}
		}
	}
	return &count, next
}

// describeCodexBlock：Codex 額度被擋的原因（畫面上的說明）；沒被擋回空字串
func describeCodexBlock(r map[string]any) string {
	if r == nil {
		return ""
	}
	var reasons []string
	seen := map[string]bool{}
	for _, b := range codexBuckets(r) {
		t := jstr(b, "rateLimitReachedType")
		if t == "" {
			continue
		}
		reason := "已達用量上限"
		switch t {
		case "workspace_owner_credits_depleted":
			reason = "工作區的點數已用完"
		case "workspace_member_credits_depleted":
			reason = "你在工作區的點數額度已用完"
		case "workspace_owner_usage_limit_reached":
			reason = "工作區已達用量上限"
		case "workspace_member_usage_limit_reached":
			reason = "已達工作區管理員設定的用量上限"
		}
		if n := strings.TrimSpace(jstr(b, "limitName")); n != "" {
			reason = n + "：" + reason
		}
		if !seen[reason] {
			seen[reason] = true
			reasons = append(reasons, reason)
		}
	}
	if allowed, ok := jbool(r, "ordinaryUsageAllowed"); len(reasons) == 0 && ok && !allowed {
		reasons = append(reasons, "Codex 回報目前不能使用方案內含的額度")
	}
	return strings.Join(reasons, "；")
}

// ---- Antigravity ----

// parseAntigravityUsage：agy -p /usage --output-format json 的 command.data → 時間窗與附註。看不懂（沒有 groups）回 ok=false
func parseAntigravityUsage(data map[string]any) ([]usageWindow, string, bool) {
	groups, ok := data["groups"].([]any)
	if !ok {
		return nil, "", false
	}
	windows := []usageWindow{}
	var notes []string
	sawEnabled := false
	for gi, gx := range groups {
		g, _ := gx.(map[string]any)
		if g == nil {
			continue
		}
		groupLabel := simplifyAgyGroup(jstr(g, "name"))
		buckets, ok := g["buckets"].([]any)
		if !ok {
			continue
		}
		for bi, bx := range buckets {
			b, _ := bx.(map[string]any)
			if b == nil {
				continue
			}
			if d, _ := jbool(b, "disabled"); d {
				continue
			}
			sawEnabled = true
			rawWindow := jstr(b, "window")
			key := normalizeAgyKey(jstr(b, "id"), gi+1, bi+1, rawWindow)
			if hasWindow(windows, key) {
				continue
			}
			label := describeAgyWindow(rawWindow, jstr(b, "name"))
			if groupLabel != "" {
				label += "・" + groupLabel
			}
			resets := parseUsageTime(b["reset_time"])
			if f, ok := jnum(b["remaining_fraction"]); ok {
				if math.IsInf(f, 0) || math.IsNaN(f) {
					continue
				}
				used := clampPercent((1 - math.Max(0, math.Min(1, f))) * 100)
				windows = append(windows, usageWindow{Key: key, Label: label, Percent: used, ResetsAt: resets, Locked: f <= 0 || used >= 100})
			} else if amt, ok := jnum(b["remaining_amount"]); ok {
				if amt <= 0 {
					windows = append(windows, usageWindow{Key: key, Label: label, Percent: 100, ResetsAt: resets, Locked: true})
				} else {
					notes = append(notes, fmt.Sprintf("%s 剩餘 %s 次", label, formatThousands2(math.Trunc(amt))))
				}
			} else if strings.TrimSpace(jstr(b, "id")) != "" && (strings.TrimSpace(rawWindow) != "" || resets != 0) {
				// remaining_fraction 是 Go 的 omitempty：用滿剩餘 0 時欄位會省略
				windows = append(windows, usageWindow{Key: key, Label: label, Percent: 100, ResetsAt: resets, Locked: true})
			}
		}
	}
	if sawEnabled && len(windows) == 0 && len(notes) == 0 {
		return nil, "", false
	}
	return orderWindows(windows), strings.Join(notes, "；"), true
}

func normalizeAgyKey(rawID string, gi, bi int, rawWindow string) string {
	source := strings.TrimSpace(rawID)
	if source == "" {
		w := rawWindow
		if strings.TrimSpace(w) == "" {
			w = fmt.Sprintf("b%d", bi)
		}
		source = fmt.Sprintf("g%d-%s", gi, w)
	}
	if len(source) >= len(agyUsageKeyPrefix) && strings.EqualFold(source[:len(agyUsageKeyPrefix)], agyUsageKeyPrefix) {
		source = source[len(agyUsageKeyPrefix):]
	}
	var b strings.Builder
	for _, r := range source {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			if s := b.String(); len(s) > 0 && s[len(s)-1] != '-' {
				b.WriteByte('-')
			}
		}
	}
	body := strings.TrimRight(b.String(), "-")
	if body == "" {
		body = fmt.Sprintf("g%d-b%d", gi, bi)
	}
	if max := 32 - len(agyUsageKeyPrefix); len(body) > max {
		body = strings.TrimRight(body[:max], "-")
	}
	return agyUsageKeyPrefix + body
}

func simplifyAgyGroup(name string) string {
	t := strings.TrimSpace(name)
	switch {
	case t == "":
		return ""
	case strings.EqualFold(t, "Gemini Models"):
		return "Gemini"
	case strings.EqualFold(t, "Claude and GPT models"):
		return "Claude/GPT"
	}
	if len(t) > 7 && strings.EqualFold(t[len(t)-7:], " models") {
		t = strings.TrimSpace(t[:len(t)-7])
	}
	return t
}

func describeAgyWindow(window, bucketName string) string {
	w := strings.ToLower(strings.TrimSpace(window))
	switch w {
	case "5h":
		return "5 小時"
	case "weekly", "7d", "1w":
		return "每週"
	case "daily", "24h", "1d":
		return "每日"
	case "monthly", "30d", "1m":
		return "每月"
	case "hourly":
		return "1 小時"
	}
	if len(w) > 1 && w[len(w)-1] == 'h' {
		if h, err := strconv.ParseInt(w[:len(w)-1], 10, 64); err == nil && h > 0 {
			if h == 24 {
				return "每日"
			}
			return fmt.Sprintf("%d 小時", h)
		}
	}
	n := strings.ToLower(bucketName)
	switch {
	case strings.Contains(n, "five hour") || strings.Contains(n, "5-hour"):
		return "5 小時"
	case strings.Contains(n, "weekly"):
		return "每週"
	case strings.Contains(n, "daily"):
		return "每日"
	case strings.Contains(n, "monthly"):
		return "每月"
	}
	return "額度"
}

// ---- Grok ----

func centValue(v any) int64 {
	if o, ok := v.(map[string]any); ok {
		if n, ok := jnum(o["val"]); ok {
			return int64(n)
		}
		return 0
	}
	if n, ok := jnum(v); ok {
		return int64(n)
	}
	return 0
}

func formatCents(c int64) string { return "$" + formatThousandsFixed2(float64(c)/100) }

func formatThousandsFixed2(v float64) string {
	s := formatThousands2(math.Trunc(v))
	return s + "." + fmt.Sprintf("%02d", int64(math.Round(math.Abs(v-math.Trunc(v))*100)))
}

// parseGrokBilling：_x.ai/billing → 一個時間窗與附註。看不懂（沒有 config）回 ok=false
func parseGrokBilling(billing map[string]any) ([]usageWindow, string, bool) {
	config := jobj(billing, "config")
	if config == nil {
		return nil, "", false
	}
	var percent float64
	if p, ok := jnum(config["creditUsagePercent"]); ok {
		percent = p
	} else if limit := centValue(config["monthlyLimit"]); limit > 0 {
		percent = float64(centValue(config["used"])) * 100 / float64(limit)
	}
	period := jobj(config, "currentPeriod")
	resets := parseUsageTime(period["end"])
	if resets == 0 {
		resets = parseUsageTime(config["billingPeriodEnd"])
	}
	label := "額度"
	switch jstr(period, "type") {
	case "USAGE_PERIOD_TYPE_WEEKLY":
		label = "每週"
	case "USAGE_PERIOD_TYPE_MONTHLY":
		label = "每月"
	case "USAGE_PERIOD_TYPE_DAILY":
		label = "每日"
	}
	capCents, usedCents, prepaid := centValue(config["onDemandCap"]), centValue(config["onDemandUsed"]), centValue(config["prepaidBalance"])
	w := usageWindow{Key: grokCreditsKey, Label: label, Percent: clampPercent(percent), ResetsAt: resets,
		Locked: percent >= 100 && (capCents <= 0 || usedCents >= capCents) && prepaid <= 0}
	var notes []string
	if capCents > 0 {
		notes = append(notes, "隨用隨付已用 "+formatCents(usedCents)+"／上限 "+formatCents(capCents))
	}
	if prepaid > 0 {
		notes = append(notes, "預購點數餘額 "+formatCents(prepaid))
	}
	return []usageWindow{w}, strings.Join(notes, "；"), true
}

// ---- 合併、重置、排序 ----

// mergeUsageWindows：稀疏更新合併：同 key 取代數值（名稱維持、沒帶重置時間沿用舊值）、新 key 加入、其他保留
func mergeUsageWindows(existing, updates []usageWindow) []usageWindow {
	out := append([]usageWindow{}, existing...)
	for _, u := range updates {
		found := false
		for i := range out {
			if out[i].Key == u.Key {
				old := out[i]
				u.Label = old.Label
				if u.ResetsAt == 0 {
					u.ResetsAt = old.ResetsAt
				}
				out[i] = u
				found = true
				break
			}
		}
		if !found {
			out = append(out, u)
		}
	}
	return orderWindows(out)
}

// sameUsageForView：兩份快照在畫面上看起來一樣（取得時間不算）：一樣就不必通知
func sameUsageForView(a, b *usageSnapshot) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Source != b.Source || a.Stale != b.Stale || a.Credits != b.Credits || a.Note != b.Note || a.Error != b.Error ||
		a.ResetCreditExpiresAt != b.ResetCreditExpiresAt || (a.ResetCredits == nil) != (b.ResetCredits == nil) ||
		(a.ResetCredits != nil && *a.ResetCredits != *b.ResetCredits) || len(a.Windows) != len(b.Windows) {
		return false
	}
	for i := range a.Windows {
		if a.Windows[i] != b.Windows[i] {
			return false
		}
	}
	return true
}

// applyUsageResets：快取資料推算重置：重置時間已過的時間窗使用率歸零、解除鎖定（下一次的重置時間未知）
func applyUsageResets(ws []usageWindow, nowMs int64) []usageWindow {
	out := make([]usageWindow, len(ws))
	for i, w := range ws {
		if w.ResetsAt != 0 && w.ResetsAt <= nowMs {
			w.Percent, w.ResetsAt, w.Locked = 0, 0, false
		}
		out[i] = w
	}
	return out
}

// applyDueResets：目前顯示的快照有時間窗已到重置時間：推算、標成過時的快取；沒有到期的回 nil
func applyDueResets(s *usageSnapshot, nowMs int64) *usageSnapshot {
	if s == nil {
		return nil
	}
	due := false
	for _, w := range s.Windows {
		if w.ResetsAt != 0 && w.ResetsAt <= nowMs {
			due = true
		}
	}
	if !due {
		return nil
	}
	n := *s
	n.Windows = applyUsageResets(s.Windows, nowMs)
	n.Stale, n.Source = true, "cache"
	return &n
}

// orderWindows：5 小時／每週在前（Antigravity 為 Gemini 在前、同群組 5 小時在每週前），其他依 key
func orderWindows(ws []usageWindow) []usageWindow {
	out := append([]usageWindow{}, ws...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := windowRank(out[i].Key), windowRank(out[j].Key)
		if ri != rj {
			return ri < rj
		}
		mi, mj := windowMinutes(out[i].Key), windowMinutes(out[j].Key)
		if mi != mj {
			return mi < mj
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func windowRank(key string) int {
	switch {
	case key == usageSessionKey:
		return 0
	case key == usageWeeklyKey:
		return 1
	case strings.HasPrefix(key, usageWeeklyKey+":"):
		return 2
	case strings.HasPrefix(key, agyUsageKeyPrefix+"gemini-"):
		return 3
	case strings.HasPrefix(key, agyUsageKeyPrefix):
		return 4
	}
	return 5
}

func windowMinutes(key string) int64 {
	if strings.HasPrefix(key, agyUsageKeyPrefix) {
		suffix := key[strings.LastIndexByte(key, '-')+1:]
		switch suffix {
		case "weekly":
			return 10080
		case "daily":
			return 1440
		case "monthly":
			return 43200
		}
		if len(suffix) > 1 && suffix[len(suffix)-1] == 'h' {
			if h, err := strconv.ParseInt(suffix[:len(suffix)-1], 10, 64); err == nil {
				return h * 60
			}
		}
		return math.MaxInt64
	}
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		if m, err := strconv.ParseInt(key[i+1:], 10, 64); err == nil {
			return m
		}
	}
	return 0
}

// guessProviderFromUsageKeys：從用量時間窗的 key 判斷是哪種代理（帳戶清單遺失時找回舊版本機預設帳戶的 Id 用）；不確定回空字串
func guessProviderFromUsageKeys(keys []string) string {
	found := map[string]bool{}
	for _, k := range keys {
		switch {
		case k == usageSessionKey || k == usageWeeklyKey || strings.HasPrefix(k, usageWeeklyKey+":"):
			found["Claude"] = true
		case k == grokCreditsKey:
			found["Grok"] = true
		case strings.HasPrefix(k, agyUsageKeyPrefix):
			found["Antigravity"] = true
		case strings.Contains(k, ":"):
			found["Codex"] = true
		}
	}
	if len(found) != 1 {
		return ""
	}
	for p := range found {
		return p
	}
	return ""
}
