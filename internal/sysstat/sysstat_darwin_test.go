//go:build darwin

package sysstat

import (
	"encoding/binary"
	"testing"
)

func TestParseIfList2AndWrap(t *testing.T) {
	msg := make([]byte, 120)
	binary.LittleEndian.PutUint16(msg[0:], 120)
	msg[3] = 0x12
	binary.LittleEndian.PutUint16(msg[12:], 4)
	binary.LittleEndian.PutUint64(msg[96:], 1000)
	binary.LittleEndian.PutUint64(msg[104:], 2000)
	other := make([]byte, 16)
	binary.LittleEndian.PutUint16(other[0:], 16)
	other[3] = 0x0e
	got := parseIfList2(append(other, msg...))
	if len(got) != 1 || got[0].index != 4 || got[0].ibytes != 1000 || got[0].obytes != 2000 {
		t.Fatalf("%+v", got)
	}
	if wrapDelta(1<<32-10, 5) != 15 || wrapDelta(5, 15) != 10 || wrapDelta(1<<40, 5) != 0 {
		t.Fatal("wrapDelta")
	}
}
