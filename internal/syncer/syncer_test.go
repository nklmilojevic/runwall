package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/nklmilojevic/runwall/internal/ghapp"
	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/testutil"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

type fakeGitHub struct {
	mu          sync.Mutex
	created     []string // "created" query values seen per runs listing
	notModified int      // conditional requests answered with 304
	runs        []map[string]any
	single      map[int64]map[string]any
	jobs        map[int64][]map[string]any
	repos       []map[string]any
	requests    []string
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		write(w, []map[string]any{{"id": 7, "account": map[string]any{"login": "acme", "type": "Organization"}}})
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"total_count": len(f.repos), "repositories": f.repos})
	})
	mux.HandleFunc("GET /repos/acme/{repo}/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.created = append(f.created, r.PathValue("repo")+" "+r.URL.Query().Get("created")+" "+r.URL.Query().Get("per_page"))
		etag := `"` + r.PathValue("repo") + `-v1"`
		if r.Header.Get("If-None-Match") == etag {
			f.notModified++
			f.mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.mu.Unlock()
		w.Header().Set("ETag", etag)
		if r.PathValue("repo") != "api" {
			write(w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
			return
		}
		write(w, map[string]any{"total_count": len(f.runs), "workflow_runs": f.runs})
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmt.Sscan(r.PathValue("id"), &id)
		run, ok := f.single[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		write(w, run)
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/{id}/jobs", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmt.Sscan(r.PathValue("id"), &id)
		if r.URL.Query().Get("filter") != "latest" {
			http.Error(w, "expected filter=latest", 400)
			return
		}
		write(w, map[string]any{"total_count": len(f.jobs[id]), "jobs": f.jobs[id]})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
}

type clients struct{ c *github.Client }

func (c clients) App() *github.Client               { return c.c }
func (c clients) Installation(int64) *github.Client { return c.c }

func run(id int64, status, conclusion string, updated time.Time) map[string]any {
	return map[string]any{
		"id": id, "name": "CI", "workflow_id": 3, "run_number": id, "run_attempt": 1, "event": "push",
		"head_branch": "main", "status": status, "conclusion": conclusion,
		"created_at": updated.Add(-time.Minute), "updated_at": updated,
		"html_url": fmt.Sprintf("https://github.com/acme/api/actions/runs/%d", id),
		"actor":    map[string]any{"login": "nkl", "type": "User"},
	}
}

func newSyncer(t *testing.T, f *fakeGitHub) (*Syncer, *store.Store, *testutil.RecordingNotifier) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	c, err := github.NewClient(github.WithURLs(&base, nil), github.WithTransport(ghapp.Transport(http.DefaultTransport)))
	if err != nil {
		t.Fatal(err)
	}
	ing, notifier := testutil.Ingester(t, now.Add(-10*time.Minute))
	s := New(clients{c}, ing.Store, ing, Config{Interval: time.Minute, Backfill: 7 * 24 * time.Hour, Retention: 30 * 24 * time.Hour}, testutil.Logger())
	s.now = func() time.Time { return now }
	return s, ing.Store, notifier
}

func TestSyncAllBackfillsAndReconciles(t *testing.T) {
	f := &fakeGitHub{
		repos: []map[string]any{
			{"id": 10, "name": "api", "full_name": "acme/api", "owner": map[string]any{"login": "acme"}, "default_branch": "main"},
			{"id": 11, "name": "legacy", "full_name": "acme/legacy", "owner": map[string]any{"login": "acme"}, "default_branch": "main", "archived": true},
		},
		runs: []map[string]any{
			run(1, "completed", "failure", now.Add(-time.Minute)), // fresh failure: notify
			run(2, "completed", "failure", now.Add(-2*time.Hour)), // old failure from backfill: quiet
			run(3, "in_progress", "", now.Add(-30*time.Second)),   // active: fetch jobs
		},
		single: map[int64]map[string]any{
			// Known locally as in progress, created before the listing window, finished while we were away.
			99: run(99, "completed", "success", now.Add(-5*time.Hour)),
		},
		jobs: map[int64][]map[string]any{
			3:  {{"id": 30, "run_id": 3, "run_attempt": 1, "name": "test", "status": "queued", "created_at": now.Add(-time.Minute)}},
			99: {{"id": 990, "run_id": 99, "run_attempt": 1, "name": "build", "status": "completed", "conclusion": "success"}},
		},
	}
	s, st, notifier := newSyncer(t, f)
	ctx := context.Background()

	// A stale local run (98) that GitHub no longer knows about, and one (99) that finished.
	st.UpsertRepo(ctx, store.Repo{ID: 10, InstallationID: 7, Owner: "acme", Name: "api", FullName: "acme/api", DefaultBranch: "main"})
	for _, id := range []int64{98, 99} {
		st.UpsertRun(ctx, store.Run{ID: id, RepoID: 10, RunAttempt: 1, Status: "in_progress", HeadBranch: "main",
			CreatedAt: now.Add(-6 * time.Hour), UpdatedAt: now.Add(-6 * time.Hour)})
	}
	// A repo that has been removed from the installation.
	st.UpsertRepo(ctx, store.Repo{ID: 12, InstallationID: 7, Owner: "acme", Name: "gone", FullName: "acme/gone"})

	if err := s.SyncAll(ctx); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[int64]string{1: "failure", 2: "failure", 99: "success", 98: "cancelled"} {
		r, err := st.GetRun(ctx, id)
		if err != nil || r.Conclusion != want {
			t.Fatalf("run %d: got %+v err %v, want conclusion %s", id, r, err, want)
		}
	}
	jobs, _ := st.ListJobs(ctx, []store.Run{{ID: 3, RunAttempt: 1}, {ID: 99, RunAttempt: 1}})
	if len(jobs[3]) != 1 || len(jobs[99]) != 1 {
		t.Fatalf("jobs %+v", jobs)
	}

	repos, _ := st.ListRepos(ctx, 7)
	if len(repos) != 2 {
		t.Fatalf("expected api and legacy, got %+v", repos)
	}
	for _, r := range repos {
		if r.FullName == "acme/api" && !r.LastSeenAt.Equal(now) {
			t.Fatalf("last seen not recorded: %v", r.LastSeenAt)
		}
	}

	f.mu.Lock()
	created := append([]string(nil), f.created...)
	f.mu.Unlock()
	if len(created) != 1 || created[0] != "api >=2026-09-30T12:00:00+00:00 100" {
		t.Fatalf("first pass should backfill 7 days for non-archived repos only, got %v", created)
	}

	testutil.Eventually(t, "notification", func() bool { return len(notifier.Sent()) == 1 })
	if !strings.HasSuffix(notifier.Sent()[0].URL, "/runs/1") {
		t.Fatalf("wrong run notified: %+v", notifier.Sent()[0])
	}

	// Later passes read the newest page with a stable URL, so unchanged repos answer 304.
	stats, err := s.syncAllStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Repos != 2 || stats.Reconciled != 1 {
		t.Fatalf("pass stats should count both repos and reconcile only the unarchived one: %+v", stats)
	}
	f.mu.Lock()
	last := f.created[len(f.created)-1]
	f.mu.Unlock()
	if last != "api  50" {
		t.Fatalf("incremental listing should be the conditional first page: %q", last)
	}
	if err := s.SyncAll(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	notModified := f.notModified
	f.mu.Unlock()
	if notModified != 1 {
		t.Fatalf("third pass should get a 304 for the unchanged repo, got %d", notModified)
	}
	time.Sleep(20 * time.Millisecond)
	if len(notifier.Sent()) != 1 {
		t.Fatalf("expected no further notifications, got %d", len(notifier.Sent()))
	}
}

func TestFetchJobsForRun(t *testing.T) {
	f := &fakeGitHub{jobs: map[int64][]map[string]any{
		5: {{"id": 50, "run_id": 5, "run_attempt": 2, "name": "lint", "status": "completed", "conclusion": "success"}},
	}}
	s, st, _ := newSyncer(t, f)
	ctx := context.Background()
	st.UpsertRepo(ctx, store.Repo{ID: 10, InstallationID: 7, Owner: "acme", Name: "api", FullName: "acme/api"})
	st.UpsertRun(ctx, store.Run{ID: 5, RepoID: 10, RunAttempt: 2, Status: "completed", UpdatedAt: now})
	if err := s.FetchJobsForRun(ctx, 5); err != nil {
		t.Fatal(err)
	}
	jobs, _ := st.ListJobs(ctx, []store.Run{{ID: 5, RunAttempt: 2}})
	if len(jobs[5]) != 1 || jobs[5][0].Name != "lint" {
		t.Fatalf("jobs %+v", jobs)
	}
}

func TestRefreshJobAndLogs(t *testing.T) {
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("GET /repos/acme/api/actions/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": 70, "run_id": 5, "run_attempt": 1, "name": "build", "status": "in_progress",
			"steps": []map[string]any{
				{"number": 1, "name": "Set up job", "status": "completed", "conclusion": "success"},
				{"number": 2, "name": "Compile", "status": "in_progress", "started_at": now.Add(-time.Minute)},
			},
		})
	})
	mux.HandleFunc("GET /repos/acme/api/actions/jobs/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srvURL+"/blob/"+r.PathValue("id"), http.StatusFound)
	})
	mux.HandleFunc("GET /blob/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("GitHub credentials must not be sent to the log storage host")
		}
		if r.PathValue("id") == "70" {
			// What GitHub's storage returns while a job is still running.
			http.Error(w, "BlobNotFound", http.StatusNotFound)
			return
		}
		w.Write([]byte("2026-10-08T09:56:04Z hello\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL

	base := srv.URL + "/"
	c, err := github.NewClient(github.WithURLs(&base, nil), github.WithAuthToken("installation-token"))
	if err != nil {
		t.Fatal(err)
	}
	ing, _ := testutil.Ingester(t, now)
	s := New(clients{c}, ing.Store, ing, Config{Interval: time.Minute}, testutil.Logger())
	ctx := context.Background()
	st := ing.Store
	st.UpsertRepo(ctx, store.Repo{ID: 10, InstallationID: 7, Owner: "acme", Name: "api", FullName: "acme/api"})
	st.UpsertRun(ctx, store.Run{ID: 5, RepoID: 10, RunAttempt: 1, Status: "in_progress", UpdatedAt: now})
	st.UpsertJob(ctx, store.Job{ID: 70, RunID: 5, RunAttempt: 1, Status: "in_progress"})
	st.UpsertJob(ctx, store.Job{ID: 71, RunID: 5, RunAttempt: 1, Status: "completed"})

	if err := s.RefreshJob(ctx, 70); err != nil {
		t.Fatal(err)
	}
	j, _ := st.GetJob(ctx, 70)
	if len(j.Steps) != 2 || j.Steps[1].Name != "Compile" || j.Steps[1].Status != "in_progress" || j.Steps[1].StartedAt.IsZero() {
		t.Fatalf("steps not stored: %+v", j.Steps)
	}

	if _, err := s.JobLog(ctx, 70); !errors.Is(err, ghapp.ErrLogUnavailable) {
		t.Fatalf("running job: want ErrLogUnavailable, got %v", err)
	}
	log, err := s.JobLog(ctx, 71)
	if err != nil || !strings.Contains(log, "hello") {
		t.Fatalf("log %q err %v", log, err)
	}
	if _, err := s.JobLog(ctx, 999); err == nil {
		t.Fatal("unknown job should fail")
	}
}

func TestReconcileBeforeJobBackfill(t *testing.T) {
	f := &fakeGitHub{
		repos: []map[string]any{
			{"id": 10, "name": "api", "full_name": "acme/api", "owner": map[string]any{"login": "acme"}, "default_branch": "main"},
			{"id": 11, "name": "web", "full_name": "acme/web", "owner": map[string]any{"login": "acme"}, "default_branch": "main"},
		},
		jobs: map[int64][]map[string]any{
			50: {{"id": 500, "run_id": 50, "run_attempt": 1, "name": "build", "status": "completed", "conclusion": "success"}},
		},
	}
	s, st, _ := newSyncer(t, f)
	ctx := context.Background()
	// A finished run from before jobs were tracked: its jobs come from the backfill phase.
	st.UpsertRepo(ctx, store.Repo{ID: 10, InstallationID: 7, Owner: "acme", Name: "api", FullName: "acme/api"})
	st.UpsertRun(ctx, store.Run{ID: 50, RepoID: 10, RunAttempt: 1, Status: "completed", Conclusion: "success",
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)})

	if err := s.SyncAll(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	reqs := append([]string(nil), f.requests...)
	f.mu.Unlock()
	lastListing, firstJobs := -1, -1
	for i, r := range reqs {
		if strings.HasSuffix(r, "/actions/runs") {
			lastListing = i
		}
		if strings.HasSuffix(r, "/runs/50/jobs") && firstJobs < 0 {
			firstJobs = i
		}
	}
	if firstJobs < 0 || lastListing < 0 || firstJobs < lastListing {
		t.Fatalf("every repo should be reconciled before jobs are backfilled: %v", reqs)
	}
	if jobs, _ := st.ListJobs(ctx, []store.Run{{ID: 50, RunAttempt: 1}}); len(jobs[50]) != 1 {
		t.Fatal("backfill should load the missing jobs")
	}
}

func TestQuietReposAreCheckedLessOften(t *testing.T) {
	s, _, _ := newSyncer(t, &fakeGitHub{})
	n := now
	cases := []struct {
		name string
		r    store.Repo
		a    store.Activity
		want bool
	}{
		{"never synced", store.Repo{}, store.Activity{}, true},
		{"has running work", store.Repo{LastSeenAt: n.Add(-time.Minute)}, store.Activity{Active: 1}, true},
		{"recent run", store.Repo{LastSeenAt: n.Add(-time.Minute)}, store.Activity{LastRun: n.Add(-2 * time.Hour)}, true},
		{"recent push", store.Repo{LastSeenAt: n.Add(-time.Minute), PushedAt: n.Add(-time.Hour)}, store.Activity{}, true},
		{"quiet, checked recently", store.Repo{LastSeenAt: n.Add(-10 * time.Minute)}, store.Activity{LastRun: n.Add(-72 * time.Hour)}, false},
		{"quiet, due", store.Repo{LastSeenAt: n.Add(-61 * time.Minute)}, store.Activity{LastRun: n.Add(-72 * time.Hour)}, true},
	}
	for _, c := range cases {
		if got := s.due(c.r, c.a, n); got != c.want {
			t.Errorf("%s: due = %v", c.name, got)
		}
	}
}

type budgetClients struct {
	clients
	rate ghapp.Rate
}

func (b budgetClients) Budget(int64) (ghapp.Rate, bool) { return b.rate, true }

func TestBudgetGuards(t *testing.T) {
	f := &fakeGitHub{
		repos: []map[string]any{{"id": 10, "name": "api", "full_name": "acme/api", "owner": map[string]any{"login": "acme"}, "default_branch": "main", "pushed_at": now.Add(-time.Hour)}},
		jobs:  map[int64][]map[string]any{50: {{"id": 500, "run_id": 50, "run_attempt": 1, "name": "build", "status": "completed"}}},
	}
	s, st, _ := newSyncer(t, f)
	ctx := context.Background()
	st.UpsertRepo(ctx, store.Repo{ID: 10, InstallationID: 7, Owner: "acme", Name: "api", FullName: "acme/api"})
	st.UpsertRun(ctx, store.Run{ID: 50, RepoID: 10, RunAttempt: 1, Status: "completed", CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)})
	c := s.gh.(clients)

	// Low budget: runs are still reconciled, but the job backfill waits.
	s.gh = budgetClients{c, ghapp.Rate{Limit: 5000, Remaining: 500, Reset: time.Now().Add(time.Hour)}}
	if err := s.SyncAll(ctx); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := st.ListJobs(ctx, []store.Run{{ID: 50, RunAttempt: 1}}); len(jobs[50]) != 0 {
		t.Fatal("job backfill should pause when the budget is low")
	}

	// Nearly exhausted: the pass stops.
	s.gh = budgetClients{c, ghapp.Rate{Limit: 5000, Remaining: 20, Reset: time.Now().Add(time.Hour)}}
	if err := s.SyncAll(ctx); !errors.Is(err, errBudget) {
		t.Fatalf("expected the pass to stop on an exhausted budget, got %v", err)
	}

	// Healthy again: the backfill catches up.
	s.gh = budgetClients{c, ghapp.Rate{Limit: 5000, Remaining: 4900, Reset: time.Now().Add(time.Hour)}}
	if err := s.SyncAll(ctx); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := st.ListJobs(ctx, []store.Run{{ID: 50, RunAttempt: 1}}); len(jobs[50]) != 1 {
		t.Fatal("job backfill should resume")
	}
	if r, ok := s.Budget(ctx); !ok || r.Remaining != 4900 {
		t.Fatalf("budget for the UI: %+v %v", r, ok)
	}
}
