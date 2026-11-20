//go:build darwin

package procs

import (
	"reflect"
	"testing"
)

func TestParseProcArgs2(t *testing.T) {
	data := append([]byte{2, 0, 0, 0}, []byte("/usr/bin/node\x00\x00\x00node\x00server.js\x00A=1\x00MYRIAGENT_AGENT_CONVERSATION=c1\x00\x00\x00")...)
	args, env := ParseProcArgs2(data)
	if !reflect.DeepEqual(args, []string{"node", "server.js"}) || !reflect.DeepEqual(env, []string{"A=1", "MYRIAGENT_AGENT_CONVERSATION=c1"}) {
		t.Fatalf("args %v env %v", args, env)
	}
	// 系統不給讀環境變數：參數之後就沒有了
	_, env = ParseProcArgs2(append([]byte{1, 0, 0, 0}, []byte("/bin/sleep\x00sleep\x00")...))
	if len(env) != 0 {
		t.Fatalf("env 應該是空的：%v", env)
	}
}
