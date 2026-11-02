package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateAndTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "x.log")
	l, err := Open(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := l.Write([]byte(strings.Repeat(string(rune('a'+i)), 29) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	cur, _ := os.ReadFile(p)
	old, _ := os.ReadFile(p + ".1")
	if len(cur) > 100 || len(old) > 100 || len(old) == 0 {
		t.Fatalf("rotation: cur=%d old=%d", len(cur), len(old))
	}
	tail, err := Tail(p, 70)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(tail, strings.Repeat("j", 29)+"\n") || strings.Count(tail, "\n") != 2 {
		t.Fatalf("tail = %q", tail)
	}
	if _, err := Tail(filepath.Join(t.TempDir(), "none.log"), 10); err == nil {
		t.Fatal("expected an error for a missing log")
	}
}
