package schedules

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// OriginEntry 排程是誰建立或改過的（來源有證據的才記）
type OriginEntry struct {
	// Source myriagent＝Myriagent 對話的工具指令；transcript＝外部 CLI 的對話紀錄
	Source         string    `json:"Source"`
	Verb           string    `json:"Verb"`
	At             time.Time `json:"At"`
	ConversationID string    `json:"ConversationId,omitempty"`
	Title          string    `json:"Title,omitempty"`
	Provider       string    `json:"Provider,omitempty"`
	TranscriptPath string    `json:"TranscriptPath,omitempty"`
	AccountID      string    `json:"AccountId,omitempty"`
	SessionID      string    `json:"SessionId,omitempty"`
	Cwd            string    `json:"Cwd,omitempty"`
}

// ChangeEntry 一筆變更紀錄（建立、修改、刪除、啟用、停用；看得出是哪個對話做的就連過去）
type ChangeEntry struct {
	Path           string    `json:"Path"`
	At             time.Time `json:"At"`
	Kind           string    `json:"Kind"`
	ConversationID string    `json:"ConversationId,omitempty"`
	Title          string    `json:"Title,omitempty"`
	Provider       string    `json:"Provider,omitempty"`
}

// StateFile <資料根目錄>/schedules/state.json：來源、變更紀錄、定義的指紋（偵測變更，app 重開也接得上）、管理對話的連結、使用者的選擇、自動診斷。
// 欄位名稱同 C# 版；只加欄位。路徑比對一律不分大小寫（同 C#）
type StateFile struct {
	// Baseline 已經有第一次掃描的指紋（之前的掃描只建基準，不把每個排程都記成「建立」）
	Baseline       bool                   `json:"Baseline"`
	Origins        map[string]OriginEntry `json:"Origins"`
	Changes        []ChangeEntry          `json:"Changes"`
	Fingerprints   map[string]string      `json:"Fingerprints"`
	EnabledStates  map[string]bool        `json:"EnabledStates"`
	Links          map[string]string      `json:"Links"`
	Ignored        []string               `json:"Ignored"`
	Adopted        []string               `json:"Adopted"`
	NoAutoDiagnose []string               `json:"NoAutoDiagnose"`
	AccountID      string                 `json:"AccountId,omitempty"`
	Diagnoses      []DiagnoseRecord       `json:"Diagnoses"`
	DiagnoseBlocks map[string]string      `json:"DiagnoseBlocks"`
}

const (
	// ChangesPerTask 每個排程留幾筆變更
	ChangesPerTask = 50
	// ChangesTotal 全部留幾筆變更
	ChangesTotal = 1000
)

// Store 排程的本機紀錄（<資料根目錄>/schedules/）：state.json＋runs.jsonl（Myriagent 觀察到的執行紀錄，只附加，載入時每個排程留最近 200 筆、
// 檔案多出太多就重寫）。載入在背景（第一次掃描前），之後在中樞的鎖裡讀寫
type Store struct {
	dir      string
	State    *StateFile
	runs     map[string][]RunRecord // 鍵是小寫的路徑
	appended int
}

// LoadStore 讀進來；讀不到的檔當成空的
func LoadStore(dir string) *Store {
	st := &StateFile{}
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		if json.Unmarshal(b, st) != nil {
			// 壞掉的檔改名留著
			_ = os.Rename(filepath.Join(dir, "state.json"), filepath.Join(dir, "state.json.bad-"+time.Now().Format("20060102-150405")))
			st = &StateFile{}
		}
	}
	st.normalize()
	s := &Store{dir: dir, State: st, runs: map[string][]RunRecord{}}
	s.loadRuns()
	return s
}

// normalize 補上沒有的集合，路徑鍵的大小寫不同時以第一個為準
func (st *StateFile) normalize() {
	if st.Origins == nil {
		st.Origins = map[string]OriginEntry{}
	}
	if st.Fingerprints == nil {
		st.Fingerprints = map[string]string{}
	}
	if st.EnabledStates == nil {
		st.EnabledStates = map[string]bool{}
	}
	if st.Links == nil {
		st.Links = map[string]string{}
	}
	if st.DiagnoseBlocks == nil {
		st.DiagnoseBlocks = map[string]string{}
	}
	if st.Changes == nil {
		st.Changes = []ChangeEntry{}
	}
	if st.Ignored == nil {
		st.Ignored = []string{}
	}
	if st.Adopted == nil {
		st.Adopted = []string{}
	}
	if st.NoAutoDiagnose == nil {
		st.NoAutoDiagnose = []string{}
	}
	if st.Diagnoses == nil {
		st.Diagnoses = []DiagnoseRecord{}
	}
}

// FindKey 字典裡不分大小寫對得上的鍵
func FindKey[V any](m map[string]V, path string) (string, bool) {
	if _, ok := m[path]; ok {
		return path, true
	}
	for k := range m {
		if strings.EqualFold(k, path) {
			return k, true
		}
	}
	return "", false
}

// Lookup 不分大小寫取值
func Lookup[V any](m map[string]V, path string) (V, bool) {
	if k, ok := FindKey(m, path); ok {
		return m[k], true
	}
	var zero V
	return zero, false
}

// Put 不分大小寫設值（已經有同一個路徑的換掉）
func Put[V any](m map[string]V, path string, v V) {
	if k, ok := FindKey(m, path); ok {
		delete(m, k)
	}
	m[path] = v
}

// Remove 不分大小寫刪除
func Remove[V any](m map[string]V, path string) {
	if k, ok := FindKey(m, path); ok {
		delete(m, k)
	}
}

// ContainsFold 清單裡有沒有這個路徑（不分大小寫）
func ContainsFold(list []string, path string) bool {
	for _, p := range list {
		if strings.EqualFold(p, path) {
			return true
		}
	}
	return false
}

// Toggle 清單裡先拿掉這個路徑，add 時再加回去
func Toggle(list []string, path string, add bool) []string {
	out := []string{}
	for _, p := range list {
		if !strings.EqualFold(p, path) {
			out = append(out, p)
		}
	}
	if add {
		out = append(out, path)
	}
	return out
}

func (s *Store) runsPath() string { return filepath.Join(s.dir, "runs.jsonl") }

func (s *Store) loadRuns() {
	f, err := os.Open(s.runsPath())
	if err != nil {
		return
	}
	lines := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		lines++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var r RunRecord
		// 寫到一半的那一行（當機）略過
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Path != "" {
			s.addToMemory(r)
		}
	}
	f.Close()
	total := 0
	for _, l := range s.runs {
		total += len(l)
	}
	if lines > total+2000 {
		s.compact()
	}
}

func (s *Store) addToMemory(r RunRecord) {
	k := strings.ToLower(r.Path)
	list := append(s.runs[k], r)
	if len(list) > KeepRunsPerTask {
		list = append([]RunRecord{}, list[len(list)-KeepRunsPerTask:]...)
	}
	s.runs[k] = list
}

// RunsOf 這個排程的執行紀錄（舊的在前）
func (s *Store) RunsOf(path string) []RunRecord { return s.runs[strings.ToLower(path)] }

// LastRunOf 最後一筆
func (s *Store) LastRunOf(path string) *RunRecord {
	list := s.runs[strings.ToLower(path)]
	if len(list) == 0 {
		return nil
	}
	r := list[len(list)-1]
	return &r
}

// AppendRun 加一筆（同時附加到 runs.jsonl）
func (s *Store) AppendRun(r RunRecord) {
	s.addToMemory(r)
	_ = os.MkdirAll(s.dir, 0o700)
	if b, err := json.Marshal(r); err == nil {
		if f, err := os.OpenFile(s.runsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(append(b, '\n'))
			f.Close()
		} else {
			log.Printf("排程：寫執行紀錄失敗：%v", err)
		}
	}
	s.appended++
	if s.appended > 5000 {
		s.compact()
	}
}

// compact 重寫成記憶體裡留著的（每個排程最近 200 筆）
func (s *Store) compact() {
	s.appended = 0
	var all []RunRecord
	for _, l := range s.runs {
		all = append(all, l...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ObservedAt.Before(all[j].ObservedAt) })
	_ = os.MkdirAll(s.dir, 0o700)
	tmp := s.runsPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		log.Printf("排程：整理執行紀錄失敗：%v", err)
		return
	}
	w := bufio.NewWriter(f)
	for _, r := range all {
		b, _ := json.Marshal(r)
		_, _ = w.Write(append(b, '\n'))
	}
	_ = w.Flush()
	f.Close()
	_ = os.Rename(tmp, s.runsPath())
}

// AddChange 加一筆變更紀錄（每個排程留 50 筆、全部留 1000 筆）
func (s *Store) AddChange(c ChangeEntry) {
	st := s.State
	st.Changes = append(st.Changes, c)
	var mine []int
	for i, x := range st.Changes {
		if strings.EqualFold(x.Path, c.Path) {
			mine = append(mine, i)
		}
	}
	if len(mine) > ChangesPerTask {
		drop := map[int]bool{}
		for _, i := range mine[:len(mine)-ChangesPerTask] {
			drop[i] = true
		}
		var kept []ChangeEntry
		for i, x := range st.Changes {
			if !drop[i] {
				kept = append(kept, x)
			}
		}
		st.Changes = kept
	}
	if len(st.Changes) > ChangesTotal {
		st.Changes = append([]ChangeEntry{}, st.Changes[len(st.Changes)-ChangesTotal:]...)
	}
}

// Save 存 state.json（暫存檔換名）
func (s *Store) Save() {
	b, err := json.MarshalIndent(s.State, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(s.dir, 0o700)
	tmp := filepath.Join(s.dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("排程：存 state.json 失敗：%v", err)
		return
	}
	_ = os.Rename(tmp, filepath.Join(s.dir, "state.json"))
}
