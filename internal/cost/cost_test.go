package cost

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKind(t *testing.T) {
	cases := []struct {
		labels []string
		runner string
		want   string
	}{
		{[]string{"ubuntu-latest"}, "GitHub Actions 12", Linux},
		{[]string{"ubuntu-24.04-arm"}, "GitHub Actions 3", LinuxArm},
		{[]string{"ubuntu-slim"}, "", Linux1},
		{[]string{"windows-2025"}, "", Windows},
		{[]string{"windows-11-arm"}, "", WindowsArm},
		{[]string{"macos-15"}, "", MacOS},
		{[]string{"macos-15-large"}, "", MacOSLarge},
		{[]string{"macos-15-xlarge"}, "", MacOSXLarge},
		{[]string{"self-hosted", "linux"}, "", SelfHosted},
		{[]string{"linux"}, "my-box-01", SelfHosted},
		{[]string{"big-linux-16"}, "GitHub Actions 1000001", Custom},
	}
	for _, c := range cases {
		if got := Kind(c.labels, c.runner); got != c.want {
			t.Errorf("Kind(%v, %q) = %s, want %s", c.labels, c.runner, got, c.want)
		}
	}
}

func TestJob(t *testing.T) {
	tb := Default()
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

	e := tb.Job([]string{"ubuntu-latest"}, "", true, 61*time.Second)
	if e.Minutes != 2 || !near(e.USD, 0.012) {
		t.Fatalf("private linux, 61s: %+v", e)
	}
	if e := tb.Job([]string{"ubuntu-latest"}, "", false, 10*time.Minute); e.USD != 0 || e.Minutes != 10 {
		t.Fatalf("public standard runners are free: %+v", e)
	}
	if e := tb.Job([]string{"macos-15-xlarge"}, "", false, 10*time.Minute); !near(e.USD, 1.02) {
		t.Fatalf("public larger runners are billed: %+v", e)
	}
	if e := tb.Job([]string{"self-hosted"}, "", true, time.Hour); e != (Estimate{}) {
		t.Fatalf("self-hosted is free: %+v", e)
	}
}

func TestLoadOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	os.WriteFile(path, []byte(`{"as_of": "2027-01-01", "rates": {"linux-2": 0.01}, "labels": {"Big-Linux-16": 0.042}}`), 0o600)
	tb, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if tb.AsOf != "2027-01-01" || tb.Rates[Linux] != 0.01 || tb.Rates[MacOS] != 0.062 {
		t.Fatalf("merge: %+v", tb)
	}
	if e := tb.Job([]string{"big-linux-16"}, "GitHub Actions 1", true, time.Minute); math.Abs(e.USD-0.042) > 1e-9 {
		t.Fatalf("custom label rate: %+v", e)
	}
}
