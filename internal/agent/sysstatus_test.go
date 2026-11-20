package agent

import (
	"testing"
	"time"

	"github.com/kevinlincg/myriagent/internal/sysstat"
)

type fakeControls struct {
	vol   int
	muted bool
}

func (f *fakeControls) getAudio() (int, bool, bool) { return f.vol, f.muted, true }
func (f *fakeControls) setAudio(v *int, m *bool) (int, bool, bool) {
	if v != nil {
		f.vol = *v
	}
	if m != nil {
		f.muted = *m
	}
	return f.vol, f.muted, true
}
func (f *fakeControls) screenOff() bool { return true }

func TestMonitorSendsDeviceStats(t *testing.T) {
	h, tv := newTestHub(t)
	tv.send(map[string]any{"cmd": "ready", "mode": "main"})
	h.mu.Lock()
	h.monitor().sample = func() sysstat.Stats {
		return sysstat.Stats{CPU: 12.5, MemUsed: 1, MemTotal: 2, Cores: 4, Disks: []sysstat.Disk{{Name: "/", Used: 1, Total: 2}}}
	}
	h.mu.Unlock()
	// 沒開面板：不送
	time.Sleep(100 * time.Millisecond)
	tv.send(map[string]any{"cmd": "monitor", "on": true})
	m := tv.wait("deviceStats", func(m map[string]any) bool { return m["type"] == "deviceStats" })
	d := m["devices"].([]any)[0].(map[string]any)
	if d["self"] != true || d["cpu"] != 12.5 || d["cores"] != float64(4) || d["seq"] != float64(1) || len(d["disks"].([]any)) != 1 || m["hub"] != nil {
		t.Fatalf("deviceStats 不對：%v", m)
	}
	tv.wait("每秒再送", func(m map[string]any) bool {
		if m["type"] != "deviceStats" {
			return false
		}
		return m["devices"].([]any)[0].(map[string]any)["seq"] == float64(2)
	})
	tv.send(map[string]any{"cmd": "monitor", "on": false})
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.mu.Lock()
		running := h.monitor().running
		h.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("關掉面板之後還在取樣")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 再打開：照樣送（seq 接著遞增）
	tv.send(map[string]any{"cmd": "monitor", "on": true})
	tv.wait("再打開", func(m map[string]any) bool {
		if m["type"] != "deviceStats" {
			return false
		}
		return m["devices"].([]any)[0].(map[string]any)["seq"].(float64) >= 3
	})
}

func TestDeviceControl(t *testing.T) {
	h, tv := newTestHub(t)
	_ = h
	old := localDeviceControls
	localDeviceControls = &fakeControls{vol: 30}
	t.Cleanup(func() { localDeviceControls = old })
	tv.send(map[string]any{"cmd": "deviceControl", "requestId": "dc-1", "action": "audioSet", "volume": "101"})
	tv.wait("音量不合格", func(m map[string]any) bool {
		return m["type"] == "deviceControlState" && m["requestId"] == "dc-1" && m["error"] == "音量要是 0～100"
	})
	tv.send(map[string]any{"cmd": "deviceControl", "requestId": "dc-2", "action": "audioSet", "volume": "35", "mute": true})
	tv.wait("設定音量", func(m map[string]any) bool {
		return m["type"] == "deviceControlState" && m["requestId"] == "dc-2" && m["volume"] == float64(35) && m["muted"] == true && m["error"] == nil
	})
	tv.send(map[string]any{"cmd": "deviceControl", "requestId": "dc-3", "action": "nope"})
	tv.wait("不認得的操作", func(m map[string]any) bool {
		return m["requestId"] == "dc-3" && m["action"] == "audioGet" && m["error"] == "不認得的操作"
	})
	if v, m, ok := parseMacVolume("35,false\n"); !ok || v != 35 || m {
		t.Fatal("parseMacVolume")
	}
	if _, _, ok := parseMacVolume("missing value,false"); ok {
		t.Fatal("沒有輸出裝置")
	}
	if v, m, ok := parsePactl("Volume: front-left: 26214 /  40% / -23.88 dB", "Mute: yes"); !ok || v != 40 || !m {
		t.Fatal("parsePactl")
	}
}
