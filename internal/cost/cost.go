// Package cost estimates GitHub Actions spend from job durations and runner types,
// using list prices. It is an estimate: plan allowances and discounts are ignored.
package cost

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// Runner kinds priced by the built-in table.
const (
	Linux1      = "linux-1"
	Linux       = "linux-2"
	LinuxArm    = "linux-arm64-2"
	Windows     = "windows-2"
	WindowsArm  = "windows-arm64-2"
	MacOS       = "macos"
	MacOSLarge  = "macos-12"
	MacOSXLarge = "macos-m2pro-5"
	SelfHosted  = "self-hosted"
	Custom      = "custom" // a hosted larger runner with a custom label not in the table
)

type Table struct {
	AsOf   string             `json:"as_of"`
	Rates  map[string]float64 `json:"rates"`  // USD per minute by runner kind
	Labels map[string]float64 `json:"labels"` // USD per minute for custom runner labels
}

// Default holds GitHub's list prices for GitHub-hosted runners, checked on the AsOf date
// against https://docs.github.com/en/billing/reference/actions-runner-pricing.
func Default() Table {
	return Table{
		AsOf: "2026-10-08",
		Rates: map[string]float64{
			Linux1:      0.002,
			Linux:       0.006,
			LinuxArm:    0.005,
			Windows:     0.010,
			WindowsArm:  0.010,
			MacOS:       0.062,
			MacOSLarge:  0.077,
			MacOSXLarge: 0.102,
			Custom:      0.006,
		},
		Labels: map[string]float64{},
	}
}

// Load reads overrides from a JSON file and merges them over the defaults.
func Load(path string) (Table, error) {
	t := Default()
	if path == "" {
		return t, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	var o Table
	if err := json.Unmarshal(b, &o); err != nil {
		return t, fmt.Errorf("%s: %w", path, err)
	}
	if o.AsOf != "" {
		t.AsOf = o.AsOf
	}
	for k, v := range o.Rates {
		t.Rates[k] = v
	}
	for k, v := range o.Labels {
		t.Labels[strings.ToLower(k)] = v
	}
	return t, nil
}

// Kind classifies a job's runner from its labels and runner name.
func Kind(labels []string, runnerName string) string {
	for _, l := range labels {
		if strings.EqualFold(l, "self-hosted") {
			return SelfHosted
		}
	}
	if runnerName != "" && !strings.HasPrefix(runnerName, "GitHub Actions") && !strings.HasPrefix(runnerName, "Hosted Agent") {
		return SelfHosted
	}
	for _, l := range labels {
		l = strings.ToLower(l)
		switch {
		case l == "ubuntu-slim":
			return Linux1
		case strings.HasPrefix(l, "ubuntu-"):
			if strings.Contains(l, "-arm") {
				return LinuxArm
			}
			return Linux
		case strings.HasPrefix(l, "windows-"):
			if strings.Contains(l, "arm") {
				return WindowsArm
			}
			return Windows
		case strings.HasPrefix(l, "macos-"):
			switch {
			case strings.HasSuffix(l, "-xlarge"):
				return MacOSXLarge
			case strings.HasSuffix(l, "-large"):
				return MacOSLarge
			}
			return MacOS
		}
	}
	return Custom
}

// standard runners are free in public repositories; larger ones are not.
func standard(kind string) bool {
	switch kind {
	case Linux1, Linux, LinuxArm, Windows, WindowsArm, MacOS:
		return true
	}
	return false
}

type Estimate struct {
	Minutes int     // billable minutes (each job rounded up)
	USD     float64 // estimated list-price cost
}

func (e *Estimate) Add(o Estimate) {
	e.Minutes += o.Minutes
	e.USD += o.USD
}

// Job prices one job.
func (t Table) Job(labels []string, runnerName string, private bool, d time.Duration) Estimate {
	if d <= 0 {
		return Estimate{}
	}
	minutes := int(math.Ceil(d.Minutes()))
	kind := Kind(labels, runnerName)
	if kind == SelfHosted {
		return Estimate{}
	}
	if !private && standard(kind) {
		return Estimate{Minutes: minutes}
	}
	rate := t.Rates[kind]
	if kind == Custom {
		for _, l := range labels {
			if r, ok := t.Labels[strings.ToLower(l)]; ok {
				rate = r
				break
			}
		}
	}
	return Estimate{Minutes: minutes, USD: float64(minutes) * rate}
}
