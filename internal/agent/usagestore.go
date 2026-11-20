package agent

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 用量歷史（照 C# 的 Usage/UsageHistoryStore.cs）：每個帳戶一個檔 usage/{帳戶}.json，內容是「時間窗 key → [毫秒時間, 百分比]…」。
// 使用率有變化（0.5 以上）、或距上一點超過 30 分鐘才記一點；保留 14 天；畫面取最近 7 天，每小時一點（該小時最後一個值）最多 168 點。
// 最後一份快照也一併存檔（下次開 app 先顯示上次的值）。只在 h.mu 底下讀寫。

const (
	usageRetentionMs   = 14 * 24 * 3600 * 1000
	usageViewSpanMs    = 7 * 24 * 3600 * 1000
	usageMinSampleMs   = 30 * 60 * 1000
	usageMaxViewPoints = 168
)

type accountHistory struct {
	Points map[string][][2]float64 `json:"points"`
	Last   *usageSnapshot          `json:"last,omitempty"`
}

type usageHistory struct {
	dir      string
	cache    map[string]*accountHistory
	dirty    map[string]bool
	readOnly map[string]bool // 存在卻讀不懂（被占用、壞了）：這次執行不寫回，不能用空的歷史蓋掉原檔
}

func newUsageHistory(dir string) *usageHistory {
	return &usageHistory{dir: dir, cache: map[string]*accountHistory{}, dirty: map[string]bool{}, readOnly: map[string]bool{}}
}

func (u *usageHistory) path(id string) string { return filepath.Join(u.dir, id+".json") }

func (u *usageHistory) get(id string) *accountHistory {
	if h := u.cache[id]; h != nil {
		return h
	}
	h := &accountHistory{}
	if b, err := os.ReadFile(u.path(id)); err == nil {
		if json.Unmarshal(b, h) != nil {
			h = &accountHistory{}
			u.readOnly[id] = true
		}
	} else if !os.IsNotExist(err) {
		u.readOnly[id] = true
	}
	if h.Points == nil {
		h.Points = map[string][][2]float64{}
	}
	u.cache[id] = h
	return h
}

func (u *usageHistory) lastSnapshot(id string) *usageSnapshot { return u.get(id).Last }

// record 記錄一份快照：更新最後快照，並為有變化的時間窗加一點
func (u *usageHistory) record(id string, s *usageSnapshot, pointMs int64) {
	h := u.get(id)
	h.Last = s
	for _, w := range s.Windows {
		addHistoryPoint(h.Points, w.Key, pointMs, w.Percent)
	}
	cutoff := float64(pointMs - usageRetentionMs)
	for k, list := range h.Points {
		i := 0
		for i < len(list) && list[i][0] < cutoff {
			i++
		}
		h.Points[k] = list[i:]
	}
	u.dirty[id] = true
}

// addHistoryPoint：時間要嚴格遞增；使用率變化 0.5 以上或距上一點 30 分鐘以上才記
func addHistoryPoint(points map[string][][2]float64, key string, ms int64, percent float64) bool {
	list := points[key]
	if n := len(list); n > 0 {
		last := list[n-1]
		if float64(ms) <= last[0] {
			return false
		}
		changed := math.Abs(last[1]-percent) >= 0.5
		stale := float64(ms)-last[0] >= usageMinSampleMs
		if !changed && !stale {
			return false
		}
	}
	points[key] = append(list, [2]float64{float64(ms), percent})
	return true
}

// view：畫面用的走勢點：最近 7 天，每小時一點（該小時最後一個值）
func (u *usageHistory) view(id, key string, nowMs int64) [][2]float64 {
	return downsampleHistory(u.get(id).Points[key], nowMs)
}

func downsampleHistory(points [][2]float64, nowMs int64) [][2]float64 {
	from := float64(nowMs - usageViewSpanMs)
	buckets := map[int64][2]float64{}
	for _, p := range points {
		if p[0] < from {
			continue
		}
		buckets[int64(p[0]/3_600_000)] = p
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([][2]float64, 0, len(keys))
	for _, k := range keys {
		out = append(out, buckets[k])
	}
	if len(out) > usageMaxViewPoints {
		out = out[len(out)-usageMaxViewPoints:]
	}
	return out
}

// keys：這個帳戶記錄過的時間窗 key
func (u *usageHistory) keys(id string) []string {
	h := u.get(id)
	seen := map[string]bool{}
	var out []string
	for k := range h.Points {
		seen[k] = true
		out = append(out, k)
	}
	if h.Last != nil {
		for _, w := range h.Last.Windows {
			if !seen[w.Key] {
				out = append(out, w.Key)
			}
		}
	}
	return out
}

// accountIDs：有用量歷史檔的帳戶 Id（帳戶清單遺失時用來找回本機預設帳戶原本的 Id）
func (u *usageHistory) accountIDs() []string {
	var out []string
	for _, n := range listNames(u.dir) {
		if strings.HasSuffix(n, ".json") {
			out = append(out, strings.TrimSuffix(n, ".json"))
		}
	}
	return out
}

func (u *usageHistory) remove(id string) {
	delete(u.cache, id)
	delete(u.dirty, id)
	delete(u.readOnly, id)
	_ = os.Remove(u.path(id))
}

// flush 把有變動的帳戶寫回磁碟；讀不懂的不寫
func (u *usageHistory) flush() {
	for id := range u.dirty {
		delete(u.dirty, id)
		if u.readOnly[id] || u.cache[id] == nil {
			continue
		}
		writeJSON(u.path(id), u.cache[id])
	}
}
