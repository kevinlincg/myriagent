package schedulecli

import (
	"bytes"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	a := Parse([]string{"add", "--weekdays", "09:00", "--new", "--label", "週報", "--", "寫", "週報"})
	if a.Error != "" || a.WhenKind != "weekdays" || a.WhenValue != "09:00" || !a.New || *a.Label != "週報" || *a.Text != "寫 週報" {
		t.Fatalf("add：%+v", a)
	}
	req := Request(a)
	when := req["when"].(map[string]any)
	if req["op"] != "add" || when["kind"] != "weekdays" || req["new"] != true || req["text"] != "寫 週報" {
		t.Fatalf("要求：%v", req)
	}
	for args, want := range map[string]string{
		"add --daily 09:00 --every 5m -- x": "時間只能給一個",
		"add -- x":                          "add 要給時間",
		"add --daily 09:00":                 "add 要給文字",
		"remove":                            "remove 要給 id",
		"frob":                              "不認得的操作",
		"list --bogus":                      "不認得的參數",
		"add --daily":                       "後面要接時間",
	} {
		if got := Parse(strings.Fields(args)); !strings.Contains(got.Error, want) {
			t.Errorf("%q：%q", args, got.Error)
		}
	}
	if !Parse(nil).Help || !Parse([]string{"--help"}).Help {
		t.Fatal("說明")
	}
}

func TestPrintAndExitCodes(t *testing.T) {
	var out, errb bytes.Buffer
	if Print(map[string]any{"t": "error", "code": "token", "message": "權杖對不上"}, &out, &errb, false) != 3 {
		t.Fatal("權杖是 3")
	}
	if Print(map[string]any{"t": "timed", "ok": false, "message": "不行"}, &out, &errb, false) != 1 || !strings.Contains(errb.String(), "不行") {
		t.Fatal("被拒絕是 1")
	}
	out.Reset()
	code := Print(map[string]any{"t": "timed", "ok": true, "items": []any{
		map[string]any{"id": "tm_1", "label": "a", "when": "每天 09:00", "next": "2026/11/21 09:00", "enabled": true, "mine": true, "conversation": "對話"},
		map[string]any{"id": "tm_2", "label": "b", "when": "每小時", "enabled": false, "newConversation": true},
	}}, &out, &errb, false)
	want := "* tm_1  a｜每天 09:00，下一次 2026/11/21 09:00｜送進「對話」\n  tm_2  b｜每小時｜每次開新對話（停用）\n"
	if code != 0 || out.String() != want {
		t.Fatalf("清單：%q", out.String())
	}
	if Main([]string{"add"}, &out, &errb) != 2 {
		t.Fatal("參數不對是 2")
	}
	t.Setenv("MYRIAGENT_AGENT_URL", "")
	if Main([]string{"list"}, &out, &errb) != 3 {
		t.Fatal("不在對話裡是 3")
	}
}
