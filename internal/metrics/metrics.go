// Package metrics aggregates stored runs and jobs into the numbers shown on the
// dashboard, workflow and repository pages.
package metrics

import (
	"sort"
	"time"

	"github.com/nklmilojevic/runwall/internal/cost"
	"github.com/nklmilojevic/runwall/internal/store"
)

type Period struct {
	Key   string
	Label string
	Dur   time.Duration
}

var Periods = []Period{
	{"24h", "Last 24 hours", 24 * time.Hour},
	{"7d", "Last 7 days", 7 * 24 * time.Hour},
	{"30d", "Last 30 days", 30 * 24 * time.Hour},
	{"90d", "Last 90 days", 90 * 24 * time.Hour},
}

// ParsePeriod returns the named period, defaulting to 30 days.
func ParsePeriod(key string) Period {
	for _, p := range Periods {
		if p.Key == key {
			return p
		}
	}
	return Periods[2]
}

type Outcome int

const (
	Pending Outcome = iota
	Success
	Failure
	Other
)

func OutcomeOf(status, conclusion string) Outcome {
	if status != "completed" {
		return Pending
	}
	switch conclusion {
	case "success":
		return Success
	case "failure", "timed_out", "startup_failure":
		return Failure
	}
	return Other
}

type Bucket struct {
	Start   time.Time
	Success int
	Failure int
	Other   int
}

type Counts struct {
	Runs    int
	Success int
	Failure int
	Other   int
	Running int
	Queued  int
}

func (c *Counts) add(m store.MetricRun) {
	c.Runs++
	switch OutcomeOf(m.Status, m.Conclusion) {
	case Success:
		c.Success++
	case Failure:
		c.Failure++
	case Other:
		c.Other++
	default:
		if store.IsQueued(m.Status) {
			c.Queued++
		} else {
			c.Running++
		}
	}
}

// SuccessRate is successes over successes plus failures; cancelled and skipped runs don't count.
func (c Counts) SuccessRate() float64 {
	if c.Success+c.Failure == 0 {
		return -1
	}
	return float64(c.Success) / float64(c.Success+c.Failure)
}

type Durations []time.Duration

func (d Durations) Avg() time.Duration {
	if len(d) == 0 {
		return 0
	}
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	return sum / time.Duration(len(d))
}

func (d Durations) P95() time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append(Durations(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s))*0.95+0.5) - 1
	return s[max(0, min(i, len(s)-1))]
}

func (d Durations) Sum() time.Duration {
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	return sum
}

type WorkflowKey struct{ RepoID, WorkflowID int64 }

type Workflow struct {
	Key          WorkflowKey
	Owner        string
	Repo         string
	Name         string
	Path         string
	Counts       Counts
	Durations    Durations
	Cost         cost.Estimate
	Last         store.MetricRun
	LastDefault  *store.MetricRun // latest finished run on the default branch
	ActiveRunIDs []int64
}

type Repo struct {
	ID        int64
	FullName  string
	Owner     string
	Counts    Counts
	Durations Durations
	Cost      cost.Estimate
	Workflows []*Workflow
}

type Summary struct {
	Period       Period
	Since        time.Time
	Counts       Counts
	Durations    Durations
	Cost         cost.Estimate
	Trend        []Bucket
	Workflows    []*Workflow // sorted by repo, then name
	Repos        map[int64]*Repo
	ActiveFlows  int
	CostsPartial bool // some finished runs have no job data yet
}

// Compute aggregates runs and jobs for a period. Runs must be ordered by start time.
func Compute(p Period, now time.Time, runs []store.MetricRun, jobs []store.CostJob, table cost.Table) Summary {
	since := now.Add(-p.Dur)
	s := Summary{Period: p, Since: since, Repos: map[int64]*Repo{}}
	s.Trend = buckets(p, since, now)
	flows := map[WorkflowKey]*Workflow{}

	jobsByRun := map[int64]bool{}
	for _, j := range jobs {
		jobsByRun[j.RunID] = true
		e := table.Job(j.Labels, j.RunnerName, j.Private, j.Completed.Sub(j.Started))
		s.Cost.Add(e)
		k := WorkflowKey{j.RepoID, j.WorkflowID}
		if w := flows[k]; w != nil {
			w.Cost.Add(e)
		} else {
			// Price jobs before their run is seen so both orderings work.
			flows[k] = &Workflow{Key: k, Cost: e}
		}
	}

	for _, m := range runs {
		k := WorkflowKey{m.RepoID, m.WorkflowID}
		w := flows[k]
		if w == nil {
			w = &Workflow{Key: k}
			flows[k] = w
		}
		w.Owner, w.Repo, w.Name, w.Path = m.Owner, m.RepoFullName, m.WorkflowName, m.WorkflowPath
		w.Counts.add(m)
		w.Last = m
		s.Counts.add(m)
		if m.Status != "completed" {
			w.ActiveRunIDs = append(w.ActiveRunIDs, m.ID)
		}
		if !m.End.IsZero() {
			d := m.End.Sub(m.Start)
			if d > 0 {
				w.Durations = append(w.Durations, d)
				s.Durations = append(s.Durations, d)
			}
			if m.HeadBranch == m.DefaultBranch {
				mm := m
				w.LastDefault = &mm
			}
			if !jobsByRun[m.ID] {
				s.CostsPartial = true
			}
		}
		if i := bucketIndex(p, since, m.Start, len(s.Trend)); i >= 0 {
			switch OutcomeOf(m.Status, m.Conclusion) {
			case Success:
				s.Trend[i].Success++
			case Failure:
				s.Trend[i].Failure++
			case Other:
				s.Trend[i].Other++
			}
		}
	}

	for _, w := range flows {
		if w.Name == "" {
			continue // jobs whose runs fell outside the period
		}
		s.Workflows = append(s.Workflows, w)
		if w.Counts.Runs > 0 {
			s.ActiveFlows++
		}
		r := s.Repos[w.Key.RepoID]
		if r == nil {
			r = &Repo{ID: w.Key.RepoID, FullName: w.Repo, Owner: w.Owner}
			s.Repos[w.Key.RepoID] = r
		}
		r.Counts.Runs += w.Counts.Runs
		r.Counts.Success += w.Counts.Success
		r.Counts.Failure += w.Counts.Failure
		r.Counts.Other += w.Counts.Other
		r.Counts.Running += w.Counts.Running
		r.Counts.Queued += w.Counts.Queued
		r.Durations = append(r.Durations, w.Durations...)
		r.Cost.Add(w.Cost)
		r.Workflows = append(r.Workflows, w)
	}
	sort.Slice(s.Workflows, func(i, j int) bool {
		a, b := s.Workflows[i], s.Workflows[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Name < b.Name
	})
	for _, r := range s.Repos {
		sort.Slice(r.Workflows, func(i, j int) bool { return r.Workflows[i].Name < r.Workflows[j].Name })
	}
	return s
}

// hourly buckets for short periods, daily otherwise
func bucketSize(p Period) time.Duration {
	if p.Dur <= 48*time.Hour {
		return time.Hour
	}
	return 24 * time.Hour
}

func buckets(p Period, since, now time.Time) []Bucket {
	size := bucketSize(p)
	start := since.UTC().Truncate(size)
	var out []Bucket
	for t := start; !t.After(now); t = t.Add(size) {
		out = append(out, Bucket{Start: t})
	}
	return out
}

func bucketIndex(p Period, since, t time.Time, n int) int {
	size := bucketSize(p)
	i := int(t.UTC().Truncate(size).Sub(since.UTC().Truncate(size)) / size)
	if i < 0 || i >= n {
		return -1
	}
	return i
}
