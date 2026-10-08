package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nklmilojevic/runwall/internal/metrics"

	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/syncer"
)

// state is the visual status shared by runs and jobs: ok, fail, cancel, skip, run, queue, wait, stuck.
type state string

func runState(r store.FeedRun) state {
	if r.Stuck {
		return "stuck"
	}
	return statusState(r.Status, r.Conclusion)
}

func jobState(j store.Job, now time.Time, threshold time.Duration) state {
	if store.IsQueued(j.Status) && !j.CreatedAt.IsZero() && now.Sub(j.CreatedAt) > threshold {
		return "stuck"
	}
	return statusState(j.Status, j.Conclusion)
}

func statusState(status, conclusion string) state {
	switch status {
	case "completed":
		switch conclusion {
		case "success":
			return "ok"
		case "failure", "timed_out", "startup_failure":
			return "fail"
		case "cancelled":
			return "cancel"
		case "action_required":
			return "wait"
		}
		return "skip"
	case "in_progress":
		return "run"
	case "waiting", "action_required":
		return "wait"
	}
	return "queue"
}

func (s state) glyph() string {
	switch s {
	case "ok":
		return "✓"
	case "fail":
		return "✕"
	case "cancel":
		return "⊘"
	case "skip":
		return "–"
	case "run":
		return "●"
	case "wait":
		return "◇"
	case "stuck":
		return "◌"
	}
	return "○"
}

func (s state) label() string {
	switch s {
	case "ok":
		return "Succeeded"
	case "fail":
		return "Failed"
	case "cancel":
		return "Cancelled"
	case "skip":
		return "Skipped"
	case "run":
		return "Running"
	case "wait":
		return "Waiting for approval"
	case "stuck":
		return "Stuck in queue"
	}
	return "Queued"
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// humanDuration is for prose, e.g. "5 min" or "90 s".
func humanDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return d.String()
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// runTiming describes how long a run has taken, or how long it has been waiting.
func runTiming(r store.FeedRun, now time.Time) string {
	start := r.RunStartedAt
	if start.IsZero() {
		start = r.CreatedAt
	}
	switch {
	case r.Status == "completed":
		return fmtDuration(r.UpdatedAt.Sub(start))
	case store.IsQueued(r.Status):
		return "queued " + fmtDuration(now.Sub(r.QueuedSince()))
	}
	return fmtDuration(now.Sub(start))
}

func jobTiming(j store.Job, now time.Time) string {
	switch {
	case !j.CompletedAt.IsZero() && !j.StartedAt.IsZero():
		return fmtDuration(j.CompletedAt.Sub(j.StartedAt))
	case !j.StartedAt.IsZero():
		return fmtDuration(now.Sub(j.StartedAt))
	case !j.CreatedAt.IsZero():
		return "queued " + fmtDuration(now.Sub(j.CreatedAt))
	}
	return ""
}

func runSeconds(r store.FeedRun, now time.Time) float64 {
	start := r.RunStartedAt
	if start.IsZero() {
		start = r.CreatedAt
	}
	end := now
	if r.Status == "completed" {
		end = r.UpdatedAt
	}
	return math.Max(end.Sub(start).Seconds(), 1)
}

// pulseHeight maps a run's duration onto a bar height, on a log scale so a two-hour
// deploy doesn't flatten every thirty-second lint job. 15s ≈ 25%, 2h ≈ 100%.
func pulseHeight(r store.FeedRun, now time.Time) string {
	lo, hi := math.Log(15), math.Log(2*3600)
	v := (math.Log(runSeconds(r, now)) - lo) / (hi - lo)
	v = math.Min(math.Max(v, 0), 1)
	return strconv.Itoa(int(25 + v*75))
}

func branchLabel(r store.FeedRun) string {
	if r.PRNumber != 0 {
		return "#" + strconv.Itoa(r.PRNumber)
	}
	return r.HeadBranch
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func headline(r store.FeedRun) string {
	if r.Title != "" {
		return r.Title
	}
	if r.CommitMessage != "" {
		return r.CommitMessage
	}
	return r.WorkflowName
}

// query builds a filter URL from the current filter with one field overridden.
func (v pageView) query(key, value string) string {
	q := v.Query
	if q == nil {
		q = url.Values{}
	}
	out := url.Values{}
	for k, vs := range q {
		out[k] = vs
	}
	if value == "" {
		out.Del(key)
	} else {
		out.Set(key, value)
	}
	if enc := out.Encode(); enc != "" {
		return "/runs?" + enc
	}
	return "/runs"
}

func annotationHref(blobURL string, a syncer.Annotation) string {
	if blobURL == "" {
		return ""
	}
	return blobURL + a.Path + "#L" + strconv.Itoa(a.Line)
}

func itoa(n int) string { return strconv.Itoa(n) }

func stepTiming(st store.Step, now time.Time) string {
	switch {
	case !st.CompletedAt.IsZero() && !st.StartedAt.IsZero():
		return fmtDuration(st.CompletedAt.Sub(st.StartedAt))
	case !st.StartedAt.IsZero() && st.Status == "in_progress":
		return fmtDuration(now.Sub(st.StartedAt))
	}
	return ""
}

// shortDur formats a duration compactly for KPI cards, e.g. "1h 43m" or "4m".
func shortDur(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d < time.Minute:
		return "0m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours()/24), int(d.Hours())%24)
}

func money(usd float64) string {
	switch {
	case usd == 0:
		return "$0"
	case usd < 10:
		return fmt.Sprintf("$%.2f", usd)
	}
	return fmt.Sprintf("$%.0f", usd)
}

func pct(rate float64) string {
	if rate < 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", rate*100)
}

func rateClass(rate float64) string {
	switch {
	case rate < 0:
		return "rate-none"
	case rate >= 0.9:
		return "rate-good"
	case rate >= 0.7:
		return "rate-ok"
	}
	return "rate-bad"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

type chartSpec struct {
	Kind   string        `json:"kind"`
	Labels []string      `json:"labels"`
	Series []chartSeries `json:"series"`
}

type chartSeries struct {
	Name  string `json:"name"`
	Color string `json:"color"` // CSS variable name, resolved in the browser
	Data  []int  `json:"data"`
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// trendChart turns trend buckets into a Chart.js line chart spec.
func trendChart(sum metrics.Summary) string {
	spec := chartSpec{Kind: "trend"}
	ok, fail := chartSeries{Name: "Succeeded", Color: "--green"}, chartSeries{Name: "Failed", Color: "--red"}
	hourly := sum.Period.Dur <= 48*time.Hour
	for _, b := range sum.Trend {
		label := b.Start.Local().Format("Jan 2")
		if hourly {
			label = b.Start.Local().Format("15:04")
		}
		spec.Labels = append(spec.Labels, label)
		ok.Data = append(ok.Data, b.Success)
		fail.Data = append(fail.Data, b.Failure)
	}
	spec.Series = []chartSeries{ok, fail}
	return toJSON(spec)
}

func distributionChart(c metrics.Counts) string {
	return toJSON(chartSpec{Kind: "doughnut", Labels: []string{"Succeeded", "Failed", "Other"}, Series: []chartSeries{
		{Name: "Succeeded", Color: "--green", Data: []int{c.Success}},
		{Name: "Failed", Color: "--red", Data: []int{c.Failure}},
		{Name: "Other", Color: "--overlay0", Data: []int{c.Other}},
	}})
}

func durationChart(runs []store.FeedRun, now time.Time) string {
	spec := chartSpec{Kind: "bars"}
	s := chartSeries{Name: "Duration (min)", Color: "--blue"}
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		if r.Status != "completed" {
			continue
		}
		spec.Labels = append(spec.Labels, "#"+strconv.Itoa(r.RunNumber))
		s.Data = append(s.Data, int(math.Round(runSeconds(r, now)/60)))
	}
	spec.Series = []chartSeries{s}
	return toJSON(spec)
}

func tierTitle(t string) string {
	if t == "" {
		return ""
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

func workflowHref(w *metrics.Workflow) string {
	return fmt.Sprintf("/workflows/%d/%d", w.Key.RepoID, w.Key.WorkflowID)
}

func runHref(id int64) string { return "/runs/" + strconv.FormatInt(id, 10) }

func repoHref(id int64) string { return "/repos/" + strconv.FormatInt(id, 10) }

// thousands formats 12500 as "12,500".
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
