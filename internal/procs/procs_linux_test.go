//go:build linux

package procs

import "testing"

func TestParseStat(t *testing.T) {
	name, rest, ok := ParseStat("123 (my (odd) prog) S 1 123 123 0 -1 4194560 100 0 0 0 7 3 0 0 20 0 1 0 4567 1000 10 18446744073709551615")
	if !ok || name != "my (odd) prog" || rest[0] != "S" || rest[1] != "1" || rest[11] != "7" || rest[19] != "4567" {
		t.Fatalf("name %q rest %v", name, rest)
	}
	if _, _, ok := ParseStat("123 (x) S 1"); ok {
		t.Fatal("欄位不夠應該失敗")
	}
}
