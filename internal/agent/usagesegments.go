package agent

import (
	"bufio"
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 每週額度每 5% 的 token 分段紀錄（照 C# 的 Usage/UsageSegmentRules.cs、UsageSegmentStore.cs、Hub/AgentHub.UsageSegments.cs）：
// 用來比較不同帳戶、不同模型「每 1% 的每週額度能用多少 token」是不是同一個基準。
//   - 只看每週的額度窗（Claude 的 weekly／weekly:模型、Codex 10080 分鐘的、名稱以「每週」開頭的）
//   - 每存下一份用量快照就看有沒有跨過下一個 5% 的邊界；跨過就把這一段各模型用掉的 token 結成一筆，附加到 usage-segments/records.jsonl
//   - 換週（重置時間往後超過一天、或使用率掉了超過 3 個百分點）時進行中的那段丟掉
//   - 只算這台 app 經手的對話：其他電腦、終端機的 CLI 用掉的額度也會讓使用率上升，那幾段每 5% 的 token 會偏少

const (
	segmentStep        = 5.0
	segmentResetDrop   = 3.0
	segmentResetShift  = 24 * 3600 * 1000 // 毫秒
	segmentUnknown     = "（未知的模型）"
	segmentMixed       = "（混用）"
	segmentDominant    = 0.9
	segmentRecentLimit = 200
)

type segmentTokens struct {
	Input      int64 `json:"input"`
	Cached     int64 `json:"cached"`
	CacheWrite int64 `json:"cacheWrite"`
	Output     int64 `json:"output"`
}

func (t *segmentTokens) addAll(o segmentTokens) {
	t.Input += o.Input
	t.Cached += o.Cached
	t.CacheWrite += o.CacheWrite
	t.Output += o.Output
}

// segmentOpen：一個帳戶的一個每週額度窗目前在記的這一段（時間是 Unix 毫秒）
type segmentOpen struct {
	AccountID    string                    `json:"accountId"`
	WindowKey    string                    `json:"windowKey"`
	WindowLabel  string                    `json:"windowLabel"`
	Bucket       float64                   `json:"bucket"`
	StartPercent float64                   `json:"startPercent"`
	StartedAt    int64                     `json:"startedAt"`
	Complete     bool                      `json:"complete"`
	ResetsAt     int64                     `json:"resetsAt,omitempty"`
	LastPercent  float64                   `json:"lastPercent"`
	LastSeenAt   int64                     `json:"lastSeenAt"`
	Models       map[string]*segmentTokens `json:"models"`
}

// segmentRecord：結好的一段（records.jsonl 一行一筆；欄位只加不改）
type segmentRecord struct {
	ID           string                    `json:"id"`
	AccountID    string                    `json:"accountId"`
	AccountLabel string                    `json:"accountLabel"`
	Provider     string                    `json:"provider"`
	Email        string                    `json:"email,omitempty"`
	WindowKey    string                    `json:"windowKey"`
	WindowLabel  string                    `json:"windowLabel"`
	FromBucket   float64                   `json:"fromBucket"`
	ToBucket     float64                   `json:"toBucket"`
	StartPercent float64                   `json:"startPercent"`
	EndPercent   float64                   `json:"endPercent"`
	StartedAt    int64                     `json:"startedAt"`
	EndedAt      int64                     `json:"endedAt"`
	ResetsAt     int64                     `json:"resetsAt,omitempty"`
	Complete     bool                      `json:"complete"`
	Models       map[string]*segmentTokens `json:"models"`
}

// isWeeklyWindow：每週的額度窗
func isWeeklyWindow(w usageWindow) bool {
	return w.Key == usageWeeklyKey || strings.HasPrefix(w.Key, usageWeeklyKey+":") || strings.HasSuffix(w.Key, ":10080") || strings.HasPrefix(w.Label, "每週")
}

// segmentBucket：使用率所在那一段的起點邊界：0～4.99→0、5～9.99→5…，100→100
func segmentBucket(p float64) float64 {
	if math.IsNaN(p) || math.IsInf(p, 0) {
		p = 0
	}
	p = math.Max(0, math.Min(100, p))
	return math.Floor(p/segmentStep+1e-9) * segmentStep
}

func segmentKey(accountID, windowKey string) string { return accountID + "|" + windowKey }

// segmentNewWeek：換週：重置時間往後移超過一天，或使用率比上次看到的低超過 3 個百分點
func segmentNewWeek(o *segmentOpen, w usageWindow, percent float64) bool {
	if w.ResetsAt != 0 && o.ResetsAt != 0 && w.ResetsAt-o.ResetsAt > segmentResetShift {
		return true
	}
	return percent < o.LastPercent-segmentResetDrop
}

func startSegment(accountID string, w usageWindow, percent float64, now int64, complete bool, resetsAt int64) *segmentOpen {
	return &segmentOpen{AccountID: accountID, WindowKey: w.Key, WindowLabel: w.Label, Bucket: segmentBucket(percent), StartPercent: percent,
		StartedAt: now, Complete: complete, ResetsAt: resetsAt, LastPercent: percent, LastSeenAt: now, Models: map[string]*segmentTokens{}}
}

// observeSegment：看到一個每週額度窗的使用率：回傳之後要留著的那一段，以及結好的一筆（沒有跨過邊界是 nil）。
// open 是 nil（第一次看到）時從這裡開始記：使用率是 0 才算完整的一段
func observeSegment(open *segmentOpen, accountID string, w usageWindow, now int64) (*segmentOpen, *segmentRecord) {
	percent := w.Percent
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		percent = 0
	}
	percent = math.Max(0, math.Min(100, percent))
	if open == nil || segmentNewWeek(open, w, percent) {
		return startSegment(accountID, w, percent, now, percent <= 0, w.ResetsAt), nil
	}
	if b := segmentBucket(percent); b > open.Bucket {
		resets := w.ResetsAt
		if resets == 0 {
			resets = open.ResetsAt
		}
		label := open.WindowLabel
		if label == "" {
			label = w.Label
		}
		models := map[string]*segmentTokens{}
		for k, v := range open.Models {
			c := *v
			models[k] = &c
		}
		rec := &segmentRecord{AccountID: accountID, WindowKey: open.WindowKey, WindowLabel: label, FromBucket: open.Bucket, ToBucket: b,
			StartPercent: open.StartPercent, EndPercent: percent, StartedAt: open.StartedAt, EndedAt: now, ResetsAt: resets,
			Complete: open.Complete, Models: models}
		return startSegment(accountID, w, percent, now, true, resets), rec
	}
	open.LastPercent, open.LastSeenAt = percent, now
	if w.ResetsAt != 0 {
		open.ResetsAt = w.ResetsAt
	}
	if w.Label != "" {
		open.WindowLabel = w.Label
	}
	return open, nil
}

func addSegmentTokens(o *segmentOpen, model string, d tokenDelta) {
	name := strings.TrimSpace(model)
	if name == "" || strings.EqualFold(name, "default") {
		name = segmentUnknown
	}
	if o.Models == nil {
		o.Models = map[string]*segmentTokens{}
	}
	t := o.Models[name]
	if t == nil {
		t = &segmentTokens{}
		o.Models[name] = t
	}
	t.Input += max64(0, d.Input)
	t.Cached += max64(0, d.Cached)
	t.CacheWrite += max64(0, d.CacheWrite)
	t.Output += max64(0, d.Output)
}

func totalSegmentTokens(models map[string]*segmentTokens) segmentTokens {
	var t segmentTokens
	for _, m := range models {
		t.addAll(*m)
	}
	return t
}

// dominantModel：輸入＋輸出佔這一段九成以上的那個模型；沒有就是混用；沒有 token 的是未知的模型
func dominantModel(models map[string]*segmentTokens) string {
	names := make([]string, 0, len(models))
	for k := range models {
		names = append(names, k)
	}
	sort.Strings(names)
	var total, bestW int64 = 0, -1
	best := ""
	for _, n := range names {
		w := models[n].Input + models[n].Output
		total += w
		if w > bestW {
			best, bestW = n, w
		}
	}
	if total <= 0 || best == "" {
		return segmentUnknown
	}
	if float64(bestW) >= float64(total)*segmentDominant {
		return best
	}
	return segmentMixed
}

func per5(tokens int64, percent float64) int64 {
	if percent <= 0 {
		return 0
	}
	return int64(math.Round(float64(tokens) * segmentStep / percent))
}

type segmentSummary struct {
	AccountID, AccountLabel, Provider, WindowKey, WindowLabel, Model string
	Records                                                          int
	Percent                                                          float64
	Tokens                                                           segmentTokens
	MinPer5, MaxPer5                                                 float64
	LastAt                                                           int64
}

// summarizeSegments：只算完整的、使用率有前進的；依帳戶 × 額度窗 × 主要模型分組，每 5%＝合計 × 5 ÷ 實際前進的百分點
func summarizeSegments(records []*segmentRecord) []*segmentSummary {
	groups := map[[3]string]*segmentSummary{}
	for _, r := range records {
		span := r.EndPercent - r.StartPercent
		if !r.Complete || !(span > 0) {
			continue
		}
		model := dominantModel(r.Models)
		key := [3]string{r.AccountID, r.WindowKey, model}
		tok := totalSegmentTokens(r.Models)
		p5 := float64(tok.Input+tok.Output) * segmentStep / span
		g := groups[key]
		if g == nil {
			g = &segmentSummary{AccountID: r.AccountID, WindowKey: r.WindowKey, Model: model, MinPer5: p5, MaxPer5: p5}
			groups[key] = g
		}
		if r.EndedAt >= g.LastAt {
			g.LastAt, g.AccountLabel, g.Provider, g.WindowLabel = r.EndedAt, r.AccountLabel, r.Provider, r.WindowLabel
		}
		g.Records++
		g.Percent += span
		g.Tokens.addAll(tok)
		g.MinPer5 = math.Min(g.MinPer5, p5)
		g.MaxPer5 = math.Max(g.MaxPer5, p5)
	}
	out := make([]*segmentSummary, 0, len(groups))
	for _, g := range groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.AccountLabel != b.AccountLabel {
			return a.AccountLabel < b.AccountLabel
		}
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.WindowKey != b.WindowKey {
			return a.WindowKey < b.WindowKey
		}
		return a.Model < b.Model
	})
	return out
}

// ---- 檔案 ----

type segmentStore struct {
	dir      string
	open     map[string]*segmentOpen
	loaded   bool
	readOnly bool
	dirty    bool
	records  []*segmentRecord
	recLoad  bool
	views    map[*View]bool // 要過分段紀錄的視窗：結了新的一筆時主動重送
}

func (s *segmentStore) recordsPath() string { return filepath.Join(s.dir, "records.jsonl") }
func (s *segmentStore) openPath() string    { return filepath.Join(s.dir, "open.json") }

func (s *segmentStore) segs() map[string]*segmentOpen {
	if !s.loaded {
		s.loaded = true
		var f struct {
			Segments map[string]*segmentOpen `json:"segments"`
		}
		if b, err := os.ReadFile(s.openPath()); err == nil {
			if json.Unmarshal(b, &f) != nil {
				s.readOnly = true
			}
		} else if !os.IsNotExist(err) {
			s.readOnly = true
		}
		s.open = f.Segments
		if s.open == nil {
			s.open = map[string]*segmentOpen{}
		}
	}
	return s.open
}

func (s *segmentStore) allRecords() []*segmentRecord {
	if s.recLoad {
		return s.records
	}
	s.recLoad = true
	f, err := os.Open(s.recordsPath())
	if err != nil {
		return s.records
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r segmentRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			s.records = append(s.records, &r)
		}
	}
	return s.records
}

func (s *segmentStore) appendRecord(r *segmentRecord) {
	s.allRecords()
	s.records = append(s.records, r)
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(s.recordsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("用量分段紀錄寫入失敗：%v", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Sync()
}

// observe：看到這個帳戶的一份用量快照：每個每週額度窗各走一次 observeSegment，結好的寫進 records.jsonl 並回傳
func (s *segmentStore) observe(a *Account, snap *usageSnapshot, now int64) []*segmentRecord {
	var closed []*segmentRecord
	if snap.Error != "" {
		return nil
	}
	segs := s.segs()
	for _, w := range snap.Windows {
		if !isWeeklyWindow(w) {
			continue
		}
		key := segmentKey(a.ID, w.Key)
		next, rec := observeSegment(segs[key], a.ID, w, now)
		segs[key] = next
		s.dirty = true
		if rec != nil {
			rec.ID, rec.AccountLabel, rec.Provider, rec.Email = newID(""), a.Label, a.Provider, a.email
			s.appendRecord(rec)
			closed = append(closed, rec)
		}
	}
	return closed
}

// addTokens：一筆增量加到這個帳戶每一個正在記的每週額度窗（還沒看過使用率的帳戶沒有段落，不記）
func (s *segmentStore) addTokens(accountID, model string, d tokenDelta) {
	if accountID == "" || d.zero() {
		return
	}
	for _, o := range s.segs() {
		if o.AccountID == accountID {
			addSegmentTokens(o, model, d)
			s.dirty = true
		}
	}
}

func (s *segmentStore) removeAccount(id string) {
	for k, o := range s.segs() {
		if o.AccountID == id {
			delete(s.open, k)
			s.dirty = true
		}
	}
}

func (s *segmentStore) flush() {
	if !s.dirty || !s.loaded {
		return
	}
	s.dirty = false
	if s.readOnly {
		return
	}
	writeJSON(s.openPath(), map[string]any{"segments": s.open})
}

func (h *Hub) segments() *segmentStore {
	r := h.ar()
	if r.segs == nil {
		r.segs = &segmentStore{dir: filepath.Join(h.root, "usage-segments"), views: map[*View]bool{}}
	}
	return r.segs
}

// ---- 畫面 ----

func tokensMap(t segmentTokens) map[string]any {
	return map[string]any{"input": t.Input, "cached": t.Cached, "cacheWrite": t.CacheWrite, "output": t.Output}
}

func modelsList(models map[string]*segmentTokens) []any {
	names := make([]string, 0, len(models))
	for k := range models {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := models[names[i]], models[names[j]]
		if a.Input+a.Output != b.Input+b.Output {
			return a.Input+a.Output > b.Input+b.Output
		}
		return names[i] < names[j]
	})
	out := []any{}
	for _, n := range names {
		m := tokensMap(*models[n])
		m["model"] = n
		out = append(out, m)
	}
	return out
}

// usageSegmentsView：UsageSegmentsView（彙總、正在記的各段、最近的紀錄）
func (h *Hub) usageSegmentsView() map[string]any {
	s := h.segments()
	records := s.allRecords()
	labels := map[string]*Account{}
	order := map[string]int{}
	for i, a := range h.accounts {
		labels[a.ID] = a
		order[a.ID] = i
	}
	summary := []any{}
	for _, g := range summarizeSegments(records) {
		label := g.AccountLabel
		if a := labels[g.AccountID]; a != nil {
			label = a.Label
		}
		summary = append(summary, map[string]any{
			"accountId": g.AccountID, "accountLabel": label, "provider": g.Provider, "windowKey": g.WindowKey, "windowLabel": g.WindowLabel,
			"model": g.Model, "records": g.Records, "percent": math.Round(g.Percent*100) / 100, "tokens": tokensMap(g.Tokens),
			"per5": map[string]any{"input": per5(g.Tokens.Input, g.Percent), "cached": per5(g.Tokens.Cached, g.Percent),
				"cacheWrite": per5(g.Tokens.CacheWrite, g.Percent), "output": per5(g.Tokens.Output, g.Percent)},
			"minPer5": int64(math.Round(g.MinPer5)), "maxPer5": int64(math.Round(g.MaxPer5)), "lastAt": g.LastAt,
		})
	}
	var opens []*segmentOpen
	for _, o := range s.segs() {
		if labels[o.AccountID] != nil {
			opens = append(opens, o)
		}
	}
	sort.Slice(opens, func(i, j int) bool {
		if order[opens[i].AccountID] != order[opens[j].AccountID] {
			return order[opens[i].AccountID] < order[opens[j].AccountID]
		}
		return opens[i].WindowKey < opens[j].WindowKey
	})
	open := []any{}
	for _, o := range opens {
		a := labels[o.AccountID]
		open = append(open, map[string]any{
			"accountId": o.AccountID, "accountLabel": a.Label, "provider": a.Provider, "windowKey": o.WindowKey, "windowLabel": o.WindowLabel,
			"bucket": o.Bucket, "startPercent": o.StartPercent, "lastPercent": o.LastPercent, "startedAt": o.StartedAt, "lastSeenAt": o.LastSeenAt,
			"complete": o.Complete, "models": modelsList(o.Models),
		})
	}
	recent := []any{}
	for i := len(records) - 1; i >= 0 && len(recent) < segmentRecentLimit; i-- {
		r := records[i]
		label := r.AccountLabel
		if a := labels[r.AccountID]; a != nil {
			label = a.Label
		}
		recent = append(recent, map[string]any{
			"id": r.ID, "accountId": r.AccountID, "accountLabel": label, "provider": r.Provider, "windowKey": r.WindowKey, "windowLabel": r.WindowLabel,
			"fromBucket": r.FromBucket, "toBucket": r.ToBucket, "startPercent": r.StartPercent, "endPercent": r.EndPercent,
			"startedAt": r.StartedAt, "endedAt": r.EndedAt, "complete": r.Complete, "model": dominantModel(r.Models), "models": modelsList(r.Models),
		})
	}
	return map[string]any{"path": s.recordsPath(), "total": len(records), "summary": summary, "open": open, "records": recent}
}

func init() {
	usageObserverHooks = append(usageObserverHooks, func(h *Hub, a *Account, snap *usageSnapshot) {
		closed := h.segments().observe(a, snap, nowMs())
		if len(closed) == 0 {
			return
		}
		for _, r := range closed {
			t := totalSegmentTokens(r.Models)
			note := ""
			if !r.Complete {
				note = "（不完整的一段）"
			}
			log.Printf("每週額度 %.0f%%→%.0f%%：%s %s（%.1f%%→%.1f%%）輸入 %d、輸出 %d%s", r.FromBucket, r.ToBucket, a.Label, r.WindowLabel,
				r.StartPercent, r.EndPercent, t.Input, t.Output, note)
		}
		s := h.segments()
		for v := range s.views {
			if v.closed {
				delete(s.views, v)
				continue
			}
			h.sendTo(v, map[string]any{"type": "usageSegments", "usageSegments": h.usageSegmentsView()})
		}
	})
	accountRemovedHooks = append(accountRemovedHooks, func(h *Hub, a *Account) { h.segments().removeAccount(a.ID) })
	// 只有主視窗：切到儀表板與在儀表板時每分鐘送；之後結了新的一筆也主動送給它
	registerCommand("usageSegments", func(h *Hub, v *View, _ *Conv, msg map[string]any) {
		if v.mode != "main" {
			return
		}
		h.segments().views[v] = true
		h.sendTo(v, map[string]any{"type": "usageSegments", "usageSegments": h.usageSegmentsView()})
	})
}
