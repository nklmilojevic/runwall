package metrics

import (
	"math"
	"testing"
	"time"

	"github.com/nklmilojevic/runwall/internal/cost"
	"github.com/nklmilojevic/runwall/internal/store"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func TestCompute(t *testing.T) {
	run := func(id, wf int64, status, conclusion, branch string, ago, dur time.Duration) store.MetricRun {
		m := store.MetricRun{ID: id, RepoID: 1, WorkflowID: wf, RepoFullName: "acme/api", Owner: "acme", DefaultBranch: "main",
			WorkflowName: map[int64]string{1: "CI", 2: "Deploy"}[wf], HeadBranch: branch, Status: status, Conclusion: conclusion,
			Start: now.Add(-ago)}
		if status == "completed" {
			m.End = m.Start.Add(dur)
		}
		return m
	}
	runs := []store.MetricRun{
		run(1, 1, "completed", "success", "main", 72*time.Hour, 2*time.Minute),
		run(2, 1, "completed", "failure", "feat", 48*time.Hour, 4*time.Minute),
		run(3, 1, "completed", "success", "main", 24*time.Hour, 6*time.Minute),
		run(4, 2, "completed", "cancelled", "main", 3*time.Hour, time.Minute),
		run(5, 2, "in_progress", "", "main", time.Minute, 0),
		run(6, 2, "queued", "", "main", 30*time.Second, 0),
	}
	jobs := []store.CostJob{
		{RunID: 1, RepoID: 1, WorkflowID: 1, Private: true, Labels: []string{"ubuntu-latest"}, Started: now.Add(-72 * time.Hour), Completed: now.Add(-72*time.Hour + 90*time.Second)},
		{RunID: 3, RepoID: 1, WorkflowID: 1, Private: true, Labels: []string{"macos-15"}, Started: now.Add(-24 * time.Hour), Completed: now.Add(-24*time.Hour + 3*time.Minute)},
	}
	s := Compute(ParsePeriod("7d"), now, runs, jobs, cost.Default())

	c := s.Counts
	if c.Runs != 6 || c.Success != 2 || c.Failure != 1 || c.Other != 1 || c.Running != 1 || c.Queued != 1 {
		t.Fatalf("counts %+v", c)
	}
	if math.Abs(c.SuccessRate()-2.0/3) > 1e-9 {
		t.Fatalf("success rate %v", c.SuccessRate())
	}
	if s.Durations.Sum() != 13*time.Minute {
		t.Fatalf("pipeline time %v", s.Durations.Sum())
	}
	// 2 min linux at $0.006 + 3 min macOS at $0.062
	if s.Cost.Minutes != 5 || math.Abs(s.Cost.USD-(0.012+0.186)) > 1e-9 {
		t.Fatalf("cost %+v", s.Cost)
	}
	if !s.CostsPartial {
		t.Fatal("runs 2 and 4 have no jobs, so costs are partial")
	}

	if len(s.Workflows) != 2 || s.ActiveFlows != 2 {
		t.Fatalf("workflows %d active %d", len(s.Workflows), s.ActiveFlows)
	}
	ci := s.Workflows[0]
	if ci.Name != "CI" || ci.LastDefault == nil || ci.LastDefault.ID != 3 || ci.Durations.P95() != 6*time.Minute || ci.Durations.Avg() != 4*time.Minute {
		t.Fatalf("CI workflow %+v", ci)
	}
	deploy := s.Workflows[1]
	if len(deploy.ActiveRunIDs) != 2 || deploy.Cost.USD != 0 {
		t.Fatalf("deploy %+v", deploy)
	}
	if r := s.Repos[1]; r == nil || r.Counts.Runs != 6 || len(r.Workflows) != 2 {
		t.Fatalf("repo rollup %+v", r)
	}

	if len(s.Trend) != 8 {
		t.Fatalf("7 days plus today = 8 daily buckets, got %d", len(s.Trend))
	}
	var succ, fail int
	for _, b := range s.Trend {
		succ += b.Success
		fail += b.Failure
	}
	if succ != 2 || fail != 1 {
		t.Fatalf("trend totals %d/%d", succ, fail)
	}
	if hourly := Compute(ParsePeriod("24h"), now, runs, nil, cost.Default()); len(hourly.Trend) != 25 {
		t.Fatalf("24h should use hourly buckets, got %d", len(hourly.Trend))
	}
}

func TestParsePeriodDefault(t *testing.T) {
	if ParsePeriod("nonsense").Key != "30d" || ParsePeriod("90d").Dur != 90*24*time.Hour {
		t.Fatal("period parsing")
	}
}
