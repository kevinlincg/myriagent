package agent

import (
	"strings"
	"testing"
)

func TestRenderLaunchAgentEscapes(t *testing.T) {
	out, err := renderService("darwin", serviceSpec{Label: launchLabel, Exe: "/Applications/A&B.app/Contents/MacOS/myriagent", Path: "/usr/bin:/bin", Log: "/tmp/x.log", Args: []string{"--port", "47800"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>/Applications/A&amp;B.app/Contents/MacOS/myriagent</string>", "<string>agent</string>", "<string>--port</string>", "<key>KeepAlive</key><true/>", "<key>PATH</key><string>/usr/bin:/bin</string>"} {
		if !strings.Contains(out, want) {
			t.Errorf("plist 少了 %q", want)
		}
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	out, err := renderService("linux", serviceSpec{Exe: "/opt/myriagent/myriagent", Path: "/usr/bin", Args: []string{"--port", "47800"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ExecStart=/opt/myriagent/myriagent agent --port 47800") || !strings.Contains(out, "Restart=always") {
		t.Fatalf("unit：%s", out)
	}
	if _, err := renderService("linux", serviceSpec{Exe: "/bad\npath"}); err == nil {
		t.Fatal("路徑有換行應該拒絕")
	}
}
