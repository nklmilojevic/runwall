package joblog

import (
	"strings"
	"testing"
)

func TestParseLog(t *testing.T) {
	raw := "\ufeff2026-10-08T09:56:04.1234567Z ##[group]Run cargo test\r\n" +
		"2026-10-08T09:56:04.2Z \x1b[36;1mcargo test\x1b[0m\n" +
		"2026-10-08T09:56:05Z ##[endgroup]\n" +
		"2026-10-08T09:56:06Z [command]/usr/bin/git status\n" +
		"2026-10-08T09:56:07Z test result: FAILED\n" +
		"2026-10-08T09:56:08Z ##[error]Process completed with exit code 101.\n" +
		"2026-10-08T09:56:09Z ##[warning]Node 16 is deprecated\n"
	v := Parse(raw, 0)
	want := []Line{
		{"group", "Run cargo test"},
		{"", "cargo test"},
		{"command", "/usr/bin/git status"},
		{"", "test result: FAILED"},
		{"error", "Process completed with exit code 101."},
		{"warning", "Node 16 is deprecated"},
	}
	if len(v.Lines) != len(want) {
		t.Fatalf("got %d lines: %+v", len(v.Lines), v.Lines)
	}
	for i := range want {
		if v.Lines[i] != want[i] {
			t.Errorf("line %d: got %+v want %+v", i, v.Lines[i], want[i])
		}
	}
	if v.FirstError != 4 || v.Total != 6 {
		t.Fatalf("first error %d total %d", v.FirstError, v.Total)
	}
}

func TestParseLogKeepsTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < Tail+50; i++ {
		b.WriteString("2026-10-08T09:56:04Z line\n")
	}
	v := Parse(b.String(), 0)
	if len(v.Lines) != Tail || v.Total != Tail+50 {
		t.Fatalf("kept %d of %d", len(v.Lines), v.Total)
	}
}
