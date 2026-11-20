package agent

import (
	"context"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevinlincg/myriagent/internal/sysstat"
)

// 電腦狀態面板（C# Hub/AgentHub.Monitor.cs、SystemStatus/SystemMonitor.cs；web-protocol 的 monitor 與 deviceStats）：
// 網頁開著面板而且看得到時送 monitor {on:true}，中樞記下是哪些視窗；有任何一個時這台每秒取樣（打開時立刻取第一次），
// deviceStats 只送給開著面板的視窗。視窗關閉就不算開著，網頁收到 init 之後會再送一次 monitor。
// 這一版還沒有同步：只有這台（id 是 null、rttMs 是 null），hub 是 null。
// 電腦選單的 deviceControl（喇叭音量、靜音、立即關閉螢幕；C# AgentHub.DeviceControl.cs）也在這裡，只做這台的

const statsInterval = time.Second

type deviceSample struct {
	seq     int64
	takenAt time.Time
	stats   sysstat.Stats
}

// monitorState：一個中樞的面板狀態（views、latest 在 h.mu 底下；sampler 只在取樣的 goroutine 用）
type monitorState struct {
	views   map[*View]bool
	running bool
	latest  *deviceSample
	seq     int64
	sampler *sysstat.Sampler
	// 測試換成假的
	sample func() sysstat.Stats
}

var monitorStates sync.Map // *Hub → *monitorState

func (h *Hub) monitor() *monitorState {
	if s, ok := monitorStates.Load(h); ok {
		return s.(*monitorState)
	}
	s, _ := monitorStates.LoadOrStore(h, &monitorState{views: map[*View]bool{}})
	return s.(*monitorState)
}

func init() {
	registerCommand("monitor", func(h *Hub, v *View, c *Conv, msg map[string]any) {
		on, _ := msg["on"].(bool)
		h.setMonitor(v, on)
	})
	registerCommand("deviceControl", func(h *Hub, v *View, c *Conv, msg map[string]any) { h.deviceControl(v, msg) })
}

// setMonitor：monitor 指令（持有 h.mu）：開了就立刻送這個視窗一則（有資料的話），沒在取樣就開始
func (h *Hub) setMonitor(v *View, on bool) {
	m := h.monitor()
	if !on {
		delete(m.views, v)
		return
	}
	if m.views[v] {
		return
	}
	m.views[v] = true
	if m.latest != nil {
		h.sendTo(v, h.deviceStatsMsg(m.latest))
	}
	if !m.running {
		m.running = true
		go h.monitorLoop(m)
	}
}

// monitorLoop：每秒取樣一次，送給開著面板的視窗；沒有視窗開著（或都關了）就停
func (h *Hub) monitorLoop(m *monitorState) {
	for {
		sample := m.sample
		if sample == nil {
			if m.sampler == nil {
				m.sampler = sysstat.NewSampler()
			}
			sample = m.sampler.Sample
		}
		st := sample()
		h.mu.Lock()
		for v := range m.views {
			if v.closed {
				delete(m.views, v)
			}
		}
		if len(m.views) == 0 {
			m.running = false
			h.mu.Unlock()
			return
		}
		m.seq++
		m.latest = &deviceSample{seq: m.seq, takenAt: time.Now(), stats: st}
		msg := h.deviceStatsMsg(m.latest)
		for v := range m.views {
			h.sendTo(v, msg)
		}
		h.mu.Unlock()
		time.Sleep(statsInterval)
	}
}

// machineName：電腦名稱（同 .NET 的 Environment.MachineName：主機名稱的第一段）
func machineName() string {
	n, _ := os.Hostname()
	if i := strings.IndexByte(n, '.'); i > 0 {
		n = n[:i]
	}
	return n
}

// deviceStatsMsg：deviceStats（web-protocol 第一節）：這台一筆（DeviceStatsView）
func (h *Hub) deviceStatsMsg(s *deviceSample) map[string]any {
	st := s.stats
	disks := st.Disks
	if disks == nil {
		disks = []sysstat.Disk{}
	}
	view := map[string]any{
		"id": nil, "self": true, "name": nilIfEmpty(machineName()), "seq": s.seq, "ageMs": max(0, time.Since(s.takenAt).Milliseconds()),
		"cpu": st.CPU, "cores": st.Cores, "cpuName": st.CPUName, "memUsed": st.MemUsed, "memTotal": st.MemTotal,
		"disks": disks, "netRx": st.NetRx, "netTx": st.NetTx, "rttMs": nil, "uptimeSec": st.UptimeSec,
		"gpus": []any{}, "cpuTemp": nil, "cpuTempSource": nil,
	}
	return map[string]any{"type": "deviceStats", "devices": []any{view}, "hub": nil}
}

// ---- 電腦選單：喇叭音量、靜音、關閉螢幕 ----

const (
	ctlAudioGet  = "audioGet"
	ctlAudioSet  = "audioSet"
	ctlScreenOff = "screenOff"
)

// parseVolume：音量的字串（0～100 的整數，前後不能有空白）；不合格回 -1
func parseVolume(s string) int {
	if len(s) == 0 || len(s) > 3 {
		return -1
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return -1
		}
	}
	v, _ := strconv.Atoi(s)
	if v > 100 {
		return -1
	}
	return v
}

// deviceControlProblem：參數不合格時回原因：audioSet 至少要有音量或靜音其中一個、音量要是 0～100
func deviceControlProblem(action string, volume *string, mute *bool) string {
	switch action {
	case ctlAudioGet, ctlScreenOff:
		return ""
	case ctlAudioSet:
		if volume == nil && mute == nil {
			return "要設定音量或靜音"
		}
		if volume != nil && parseVolume(*volume) < 0 {
			return "音量要是 0～100"
		}
		return ""
	}
	return "不認得的操作"
}

func deviceControlState(requestID any, action string, volume *int, muted *bool, errText string) map[string]any {
	m := map[string]any{"type": "deviceControlState", "requestId": requestID, "action": action, "volume": nil, "muted": nil, "error": nil}
	if volume != nil {
		m["volume"] = *volume
	}
	if muted != nil {
		m["muted"] = *muted
	}
	if errText != "" {
		if r := []rune(errText); len(r) > 300 {
			errText = string(r[:300])
		}
		m["error"] = errText
	}
	return m
}

// deviceControls：調音量與關螢幕的實作（測試換成假的）
type deviceControls interface {
	getAudio() (int, bool, bool)
	setAudio(volume *int, mute *bool) (int, bool, bool)
	screenOff() bool
}

var localDeviceControls deviceControls = osDeviceControls{}

// deviceControl：deviceControl 指令（持有 h.mu）。其他電腦（deviceId 不是空的）要等同步；這台的在背景做，結果只回這個視窗
func (h *Hub) deviceControl(v *View, msg map[string]any) {
	requestID := msg["requestId"]
	if _, ok := requestID.(string); !ok {
		requestID = nil
	}
	action := str(msg, "action")
	var volume *string
	if s, ok := msg["volume"].(string); ok {
		volume = &s
	}
	var mute *bool
	if b, ok := msg["mute"].(bool); ok {
		mute = &b
	}
	shown := action
	if shown != ctlAudioGet && shown != ctlAudioSet && shown != ctlScreenOff {
		shown = ctlAudioGet
	}
	if str(msg, "deviceId") != "" {
		h.sendTo(v, deviceControlState(requestID, shown, nil, nil, "這一版還不能操作其他電腦"))
		return
	}
	if p := deviceControlProblem(action, volume, mute); p != "" {
		h.sendTo(v, deviceControlState(requestID, shown, nil, nil, p))
		return
	}
	ctl := localDeviceControls
	go func() {
		var res map[string]any
		switch action {
		case ctlScreenOff:
			ok := ctl.screenOff()
			errText := ""
			if !ok {
				errText = "關不了這台的螢幕"
			}
			log.Printf("電腦選單：關閉這台的螢幕（%v）", ok)
			res = deviceControlState(requestID, action, nil, nil, errText)
		default:
			var vol int
			var muted, ok bool
			if action == ctlAudioSet {
				var want *int
				if volume != nil {
					n := parseVolume(*volume)
					want = &n
				}
				vol, muted, ok = ctl.setAudio(want, mute)
			} else {
				vol, muted, ok = ctl.getAudio()
			}
			if ok {
				res = deviceControlState(requestID, action, &vol, &muted, "")
			} else {
				res = deviceControlState(requestID, action, nil, nil, "這台沒有可以調整的播放裝置（喇叭或耳機）")
			}
		}
		h.mu.Lock()
		h.sendTo(v, res)
		h.mu.Unlock()
	}()
}

// osDeviceControls：macOS 用系統的 AppleScript（和選單列的音量是同一個設定）與 pmset displaysleepnow；
// Linux 用 PulseAudio／PipeWire 的 pactl 與 xset dpms（沒有這些工具時回失敗）
type osDeviceControls struct{}

func runTool(name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

func (osDeviceControls) getAudio() (int, bool, bool) {
	if runtime.GOOS == "darwin" {
		out, ok := runTool("/usr/bin/osascript", "-e", "set s to get volume settings", "-e", `return ((output volume of s) as text) & "," & ((output muted of s) as text)`)
		if !ok {
			return 0, false, false
		}
		return parseMacVolume(out)
	}
	vol, ok1 := runTool("pactl", "get-sink-volume", "@DEFAULT_SINK@")
	mute, ok2 := runTool("pactl", "get-sink-mute", "@DEFAULT_SINK@")
	if !ok1 || !ok2 {
		return 0, false, false
	}
	return parsePactl(vol, mute)
}

func (c osDeviceControls) setAudio(volume *int, mute *bool) (int, bool, bool) {
	if runtime.GOOS == "darwin" {
		if volume != nil {
			runTool("/usr/bin/osascript", "-e", "set volume output volume "+strconv.Itoa(*volume))
		}
		if mute != nil {
			runTool("/usr/bin/osascript", "-e", "set volume output muted "+strconv.FormatBool(*mute))
		}
	} else {
		if volume != nil {
			runTool("pactl", "set-sink-volume", "@DEFAULT_SINK@", strconv.Itoa(*volume)+"%")
		}
		if mute != nil {
			m := "0"
			if *mute {
				m = "1"
			}
			runTool("pactl", "set-sink-mute", "@DEFAULT_SINK@", m)
		}
	}
	return c.getAudio()
}

func (osDeviceControls) screenOff() bool {
	if runtime.GOOS == "darwin" {
		_, ok := runTool("/usr/bin/pmset", "displaysleepnow")
		return ok
	}
	_, ok := runTool("xset", "dpms", "force", "off")
	return ok
}

// parseMacVolume：「音量,是否靜音」；沒有可以調音量的輸出裝置時音量是 missing value，回 false
func parseMacVolume(out string) (int, bool, bool) {
	parts := strings.Split(strings.TrimSpace(out), ",")
	if len(parts) != 2 {
		return 0, false, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, false, false
	}
	return min(100, max(0, v)), strings.EqualFold(strings.TrimSpace(parts[1]), "true"), true
}

var pactlPercent = regexp.MustCompile(`(\d+)%`)

// parsePactl：pactl get-sink-volume 的第一個百分比、get-sink-mute 的 yes／no
func parsePactl(vol, mute string) (int, bool, bool) {
	m := pactlPercent.FindStringSubmatch(vol)
	if m == nil {
		return 0, false, false
	}
	v, _ := strconv.Atoi(m[1])
	return min(100, max(0, v)), strings.Contains(strings.ToLower(mute), "yes"), true
}
