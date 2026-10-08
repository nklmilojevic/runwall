// Package demo seeds a database with plausible runs so the UI can be viewed without a GitHub App.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/nklmilojevic/runwall/internal/ghapp"
	"github.com/nklmilojevic/runwall/internal/scoring"
	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/syncer"
)

// Syncer stands in for the GitHub reconciler.
type Syncer struct{ At time.Time }

func (Syncer) FetchJobsForRun(context.Context, int64) error { return nil }
func (Syncer) RefreshJob(context.Context, int64) error      { return nil }
func (Syncer) RefreshRun(context.Context, int64) error      { return nil }
func (Syncer) TriggerAll()                                  {}
func (Syncer) Budget(context.Context) (ghapp.Rate, bool)    { return ghapp.Rate{}, false }

func (Syncer) Annotations(context.Context, int64) ([]syncer.Annotation, error) {
	return []syncer.Annotation{
		{Level: "failure", Path: "internal/importer/import_test.go", Line: 88, Title: "TestImportBatch", Message: "expected 500 subscribers, got 499"},
		{Level: "warning", Path: ".github", Message: "Node.js 20 actions are deprecated. Update actions/setup-go to v5."},
	}, nil
}

func (Syncer) WorkflowFile(context.Context, int64) (string, error) {
	return `name: CI
on:
  push:
    branches: [main]
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test ./...
`, nil
}

func (Syncer) JobLog(context.Context, int64) (string, error) {
	var b strings.Builder
	b.WriteString("\ufeff2026-10-08T09:56:04.0000000Z ##[group]Run actions/checkout@v4\n")
	b.WriteString("2026-10-08T09:56:04.1000000Z Syncing repository: acme/api\n")
	b.WriteString("2026-10-08T09:56:05.0000000Z ##[endgroup]\n")
	b.WriteString("2026-10-08T09:56:05.1000000Z ##[group]Run go test ./...\n")
	b.WriteString("2026-10-08T09:56:05.2000000Z \x1b[36;1mgo test ./...\x1b[0m\n")
	b.WriteString("2026-10-08T09:56:05.3000000Z ##[endgroup]\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "2026-10-08T09:56:%02d.0000000Z ok  \tgithub.com/acme/api/internal/pkg%02d\t0.%03ds\n", 6+i%50, i, 100+i*7)
	}
	b.WriteString("2026-10-08T09:56:50.0000000Z --- FAIL: TestImportBatch (0.21s)\n")
	b.WriteString("2026-10-08T09:56:50.1000000Z     import_test.go:88: expected 500 subscribers, got 499\n")
	b.WriteString("2026-10-08T09:56:50.2000000Z FAIL\tgithub.com/acme/api/internal/importer\t1.204s\n")
	b.WriteString("2026-10-08T09:56:50.3000000Z ##[error]Process completed with exit code 1.\n")
	b.WriteString("2026-10-08T09:56:51.0000000Z ##[warning]Node.js 20 actions are deprecated.\n")
	return b.String(), nil
}
func (s Syncer) LastSync() (time.Time, error) { return s.At, nil }

func Seed(ctx context.Context, st *store.Store, now time.Time) error {
	rng := rand.New(rand.NewPCG(7, 7))
	type repo struct {
		owner, name string
		workflows   []string
	}
	repos := []repo{
		{"acme", "api", []string{"CI", "Deploy", "CodeQL"}},
		{"acme", "web-app", []string{"CI", "E2E", "Deploy"}},
		{"acme", "clusters", []string{"Flux diff", "Kubeconform"}},
		{"acme-labs", "sdk-go", []string{"Test", "Release"}},
		{"acme-labs", "cli", []string{"CI", "GoReleaser"}},
		{"octocat", "dotfiles", []string{"Build hosts", "Cache push"}},
	}
	titles := []string{
		"Bump golang.org/x/net to 0.48.0", "Fix race in webhook dedupe", "Add retry around cache push",
		"Rework subscriber import batching", "Pin node to 22.11", "Drop legacy session store", "Speed up e2e shard split",
		"Update flake.lock", "Add arm64 node pool", "Tidy up release notes template",
	}
	actors := []string{"octocat", "monalisa", "hubot", "dependabot[bot]", "renovate[bot]"}
	branches := []string{"feat/batch-import", "fix/dedupe", "chore/deps", "release/2.14"}

	descriptions := []string{"Public REST API and background workers", "Customer-facing web app", "Flux manifests for the clusters",
		"Go SDK for the public API", "Command-line client", "Machine and editor configuration"}
	for ri, r := range repos {
		inst := int64(100 + ri/2)
		if err := st.UpsertInstallation(ctx, store.Installation{ID: inst, Account: r.owner, AccountType: "Organization"}); err != nil {
			return err
		}
		if err := st.UpsertRepo(ctx, store.Repo{ID: int64(ri + 1), InstallationID: inst, Owner: r.owner, Name: r.name,
			FullName: r.owner + "/" + r.name, DefaultBranch: "main", Private: ri%2 == 0, Description: descriptions[ri],
			PushedAt: now.Add(-time.Duration(ri*9) * 24 * time.Hour), OpenIssues: ri * 7}); err != nil {
			return err
		}
		if err := seedScore(ctx, st, int64(ri+1), ri, now); err != nil {
			return err
		}
	}

	id := int64(1000)
	for i := 0; i < 180; i++ {
		ri := rng.IntN(len(repos))
		r := repos[ri]
		age := time.Duration(i)*7*time.Minute + time.Duration(rng.IntN(240))*time.Second
		if i >= 12 {
			age = time.Duration(i-11)*4*time.Hour + time.Duration(rng.IntN(7200))*time.Second
		}
		created := now.Add(-age)
		dur := time.Duration(30+rng.IntN(600)) * time.Second
		if r.workflows[0] == "Build hosts" {
			dur *= 4
		}
		run := store.Run{
			ID: id, RepoID: int64(ri + 1), WorkflowID: int64(ri*10 + rng.IntN(len(r.workflows))), RunNumber: 4000 - i,
			RunAttempt: 1, Event: "push", HeadBranch: "main", HeadSHA: fmt.Sprintf("%016x%016x%08x", rng.Uint64(), rng.Uint64(), rng.Uint32()),
			Title: titles[rng.IntN(len(titles))], ActorLogin: actors[rng.IntN(len(actors))],
			CreatedAt: created, RunStartedAt: created, HTMLURL: fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", r.owner, r.name, id),
		}
		run.WorkflowName = r.workflows[int(run.WorkflowID)%len(r.workflows)]
		run.ActorIsBot = run.ActorLogin[len(run.ActorLogin)-1] == ']'
		if rng.IntN(3) == 0 {
			run.Event, run.HeadBranch, run.PRNumber = "pull_request", branches[rng.IntN(len(branches))], 200+rng.IntN(300)
		}
		switch i {
		case 0, 2:
			run.Status = "in_progress"
		case 1:
			run.Status = "queued"
		case 3:
			run.Status, run.RunStartedAt = "queued", now.Add(-9*time.Minute)
			run.CreatedAt = run.RunStartedAt
		case 4:
			run.Status = "in_progress"
		default:
			run.Status = "completed"
			switch n := rng.IntN(20); {
			case n < 3:
				run.Conclusion = "failure"
			case n < 4:
				run.Conclusion = "cancelled"
			case n < 5:
				run.Conclusion = "skipped"
			default:
				run.Conclusion = "success"
			}
			if i == 9 {
				run.RunAttempt = 2
			}
		}
		run.UpdatedAt = created.Add(dur)
		if run.Status != "completed" {
			run.UpdatedAt = now
		}
		if _, _, err := st.UpsertRun(ctx, run); err != nil {
			return err
		}

		jobs := []string{"lint", "test (ubuntu-latest)", "test (macos-14)", "build"}
		for ji, name := range jobs[:2+rng.IntN(3)] {
			j := store.Job{ID: id*10 + int64(ji), RunID: id, RunAttempt: run.RunAttempt, Name: name,
				Labels:     []string{[]string{"ubuntu-latest", "ubuntu-latest", "ubuntu-24.04-arm", "macos-15", "windows-2025"}[(ri+ji)%5]},
				RunnerName: fmt.Sprintf("GitHub Actions %d", rng.IntN(900)),
				CreatedAt:  created, StartedAt: created.Add(5 * time.Second),
				HTMLURL: run.HTMLURL + fmt.Sprintf("/job/%d", id*10+int64(ji))}
			switch {
			case run.Status == "completed":
				j.Status, j.Conclusion, j.CompletedAt = "completed", "success", created.Add(dur/time.Duration(ji+1))
				if run.Conclusion != "success" && ji == 1 {
					j.Conclusion = run.Conclusion
				}
			case i == 4 && ji == 1:
				j.Status, j.StartedAt, j.RunnerName = "queued", time.Time{}, ""
				j.Labels, j.CreatedAt = []string{"self-hosted", "linux", "arm64"}, now.Add(-12*time.Minute)
			case store.IsQueued(run.Status):
				j.Status, j.StartedAt, j.RunnerName = "queued", time.Time{}, ""
			default:
				j.Status = "in_progress"
				if ji == 0 {
					j.Status, j.Conclusion, j.CompletedAt = "completed", "success", created.Add(40*time.Second)
				}
			}
			j.Steps = demoSteps(j)
			if _, err := st.UpsertJob(ctx, j); err != nil {
				return err
			}
		}
		id++
	}
	return nil
}

func demoSteps(j store.Job) []store.Step {
	names := []string{"Set up job", "Checkout", "Set up Go", "Run tests", "Post Checkout", "Complete job"}
	if store.IsQueued(j.Status) {
		return nil
	}
	var steps []store.Step
	t := j.StartedAt
	for i, name := range names {
		st := store.Step{Number: i + 1, Name: name}
		switch {
		case j.Status == "completed":
			st.Status, st.Conclusion = "completed", "success"
			if name == "Run tests" && j.Conclusion != "success" {
				st.Conclusion = j.Conclusion
			}
			if i > 3 && j.Conclusion == "failure" {
				st.Conclusion = "skipped"
			}
			st.StartedAt, st.CompletedAt = t, t.Add(time.Duration(5+i*9)*time.Second)
			t = st.CompletedAt
		case i < 3:
			st.Status, st.Conclusion = "completed", "success"
			st.StartedAt, st.CompletedAt = t, t.Add(time.Duration(4+i*6)*time.Second)
			t = st.CompletedAt
		case i == 3:
			st.Status, st.StartedAt = "in_progress", t
		default:
			st.Status = "pending"
		}
		steps = append(steps, st)
	}
	return steps
}

func seedScore(ctx context.Context, st *store.Store, repoID int64, i int, now time.Time) error {
	scores := []int{92, 74, 58, 81, 44, 66}
	v := scores[i%len(scores)]
	b := scoring.Breakdown{Categories: []scoring.Category{
		{Name: "Security", Weight: 25, Score: min(100, v+5), Available: true, Checks: []scoring.Check{
			{Name: "Dependabot alerts", OK: v > 60, Detail: map[bool]string{true: "no open alerts", false: "3 open (0 critical, 1 high)"}[v > 60]},
			{Name: "Code scanning", OK: true, Detail: "no open alerts"},
			{Name: "Secret scanning", OK: true, Detail: "no open alerts"}}},
		{Name: "Testing", Weight: 20, Score: v, Available: true, Checks: []scoring.Check{
			{Name: "Test files", OK: true, Detail: "internal/api/handler_test.go"}, {Name: "Tests run in CI", OK: v > 50}}},
		{Name: "CI/CD", Weight: 15, Score: min(100, v+8), Available: true, Checks: []scoring.Check{
			{Name: "Uses GitHub Actions", OK: true, Detail: "41 runs in 90 days"}, {Name: "Default branch passing", OK: v > 70, Detail: "88% success"},
			{Name: "Default branch protected", OK: v > 55}}},
		{Name: "Documentation", Weight: 15, Score: max(0, v-10), Available: true, Checks: []scoring.Check{
			{Name: "Description", OK: true}, {Name: "README", OK: true, Detail: "README.md"}, {Name: "Docs beyond the README", OK: v > 75, Detail: "2 markdown files"}}},
		{Name: "Code quality", Weight: 10, Available: false, Missing: "Contents: read"},
		{Name: "Maintenance", Weight: 10, Score: max(0, v-4), Available: true, Checks: []scoring.Check{
			{Name: "Recent activity", OK: true, Detail: "last push " + now.Add(-time.Duration(i*9)*24*time.Hour).Format("2006-01-02")},
			{Name: "Manageable issue backlog", OK: i < 4, Detail: fmt.Sprintf("%d open issues and PRs", i*7)}}},
		{Name: "Community", Weight: 5, Score: v - 20, Available: true, Checks: []scoring.Check{
			{Name: "GitHub community profile", OK: false, Detail: fmt.Sprintf("%d%% complete", v-20)}}},
	}}
	raw, _ := json.Marshal(b)
	total := scoring.Total(b)
	return st.SaveScore(ctx, store.RepoScore{RepoID: repoID, Score: total, Tier: scoring.TierFor(total), Breakdown: string(raw), ComputedAt: now.Add(-3 * time.Hour)})
}
