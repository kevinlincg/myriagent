package agent

import (
	"testing"
	"time"
)

func TestActivityRules(t *testing.T) {
	a, b, c := procStamp{1, 100}, procStamp{2, 200}, procStamp{3, 3500}
	prev := map[procStamp]uint64{a: 1000, b: 500}
	cur := map[procStamp]uint64{a: 1500, b: 400, c: 300}
	if treeDelta(prev, cur, false) != 0 {
		t.Fatal("沒取過基準應該是 0")
	}
	if d := treeDelta(prev, cur, true); d != 500+0+300 {
		t.Fatalf("treeDelta %d", d)
	}
	// 新程序 d 在 prevAt 之前就啟動（活了 4000，這段 1000）：攤 1/4；c 在這段之內啟動：整個算
	d := procStamp{4, 0}
	cur2 := map[procStamp]uint64{a: 1500, c: 300, d: 800}
	if got := treeCPUDelta(prev, cur2, true, 3000, 4000); got != 500+300+200 {
		t.Fatalf("treeCPUDelta %d", got)
	}
	if activityCPUPercent(int64(250*time.Millisecond), 250*time.Millisecond, 4) != 25 || activityCPUPercent(0, time.Second, 4) != 0 {
		t.Fatal("activityCPUPercent")
	}
	if activityIntervalMs(true) != 250 || activityIntervalMs(false) != 500 {
		t.Fatal("interval")
	}
	zero := 0.0
	if renderActivity([]activityItem{{ID: "x"}}, &[2]int64{0, 0}, 250) != nil {
		t.Fatal("沒有動靜不送")
	}
	m := renderActivity([]activityItem{{ID: "x"}, {ID: "y", CPU: &zero}}, &[2]int64{0, 0}, 0)
	if m == nil || len(m["conversations"].([]activityItem)) != 1 || m["net"] != nil || m["ms"] != 1 {
		t.Fatalf("render %v", m)
	}
}

// 中樞：看得到的執行中對話，量得到 CLI 程序（假 CLI）的 CPU 與記憶體
func TestActivityMeasuresRunningConversation(t *testing.T) {
	h, tv := newTestHub(t)
	id := tv.newDraft()
	tv.send(map[string]any{"cmd": "visible", "conversationIds": []any{id}})
	tv.send(map[string]any{"cmd": "send", "conversationId": id, "text": "慢一點"})
	tv.wait("執行中", func(m map[string]any) bool {
		hd, _ := m["header"].(map[string]any)
		return m["type"] == "header" && hd["running"] == true
	})
	a := newActivityState()
	t.Cleanup(func() { close(a.stop) })
	a.loop(h)
	tv.wait("activity 帶 CPU 與記憶體", func(m map[string]any) bool {
		if m["type"] != "activity" {
			return false
		}
		for _, x := range m["conversations"].([]any) {
			c := x.(map[string]any)
			if c["id"] == id && c["cpu"] != nil && c["mem"].(float64) > 0 {
				return true
			}
		}
		return false
	})
	tv.send(map[string]any{"cmd": "stop", "conversationId": id})
}
