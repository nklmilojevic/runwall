package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedRepo(t *testing.T, s *Store, r Repo) {
	t.Helper()
	if r.Owner == "" {
		r.Owner = "acme"
	}
	if r.Name == "" {
		r.Name = "api"
	}
	r.FullName = r.Owner + "/" + r.Name
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	if err := s.UpsertRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func mustUpsertRun(t *testing.T, s *Store, r Run) bool {
	t.Helper()
	_, changed, err := s.UpsertRun(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func TestUpsertRunIgnoresStaleEvents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seedRepo(t, s, Repo{ID: 1, InstallationID: 9})

	completed := Run{ID: 100, RepoID: 1, RunAttempt: 1, Status: "completed", Conclusion: "success", HeadBranch: "main",
		CreatedAt: t0, UpdatedAt: t0.Add(2 * time.Minute)}
	if !mustUpsertRun(t, s, completed) {
		t.Fatal("first insert should change")
	}

	stale := completed
	stale.Status, stale.Conclusion, stale.UpdatedAt = "in_progress", "", t0.Add(time.Minute)
	if mustUpsertRun(t, s, stale) {
		t.Fatal("older in_progress event must not overwrite completed run")
	}

	sameTimeLowerRank := completed
	sameTimeLowerRank.Status, sameTimeLowerRank.Conclusion = "in_progress", ""
	if mustUpsertRun(t, s, sameTimeLowerRank) {
		t.Fatal("same updated_at with lower status rank must not overwrite")
	}

	if mustUpsertRun(t, s, completed) {
		t.Fatal("identical run should not report a change")
	}

	rerun := completed
	rerun.RunAttempt, rerun.Status, rerun.Conclusion, rerun.UpdatedAt = 2, "queued", "", t0.Add(10*time.Minute)
	if !mustUpsertRun(t, s, rerun) {
		t.Fatal("new attempt should overwrite")
	}
	oldAttempt := completed
	oldAttempt.UpdatedAt = t0.Add(time.Hour)
	if mustUpsertRun(t, s, oldAttempt) {
		t.Fatal("an older attempt must never overwrite a newer one")
	}

	got, err := s.GetRun(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunAttempt != 2 || got.Status != "queued" {
		t.Fatalf("got attempt %d status %q", got.RunAttempt, got.Status)
	}
}

func TestUpsertJobNeverRegresses(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := Job{ID: 5, RunID: 100, RunAttempt: 1, Name: "build", Status: "completed", Conclusion: "success", Labels: []string{"ubuntu-latest"}}
	if ch, err := s.UpsertJob(ctx, j); err != nil || !ch {
		t.Fatalf("insert: %v %v", ch, err)
	}
	back := j
	back.Status, back.Conclusion = "in_progress", ""
	if ch, _ := s.UpsertJob(ctx, back); ch {
		t.Fatal("job moved backwards")
	}
	jobs, err := s.ListJobs(ctx, []Run{{ID: 100, RunAttempt: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs[100]) != 1 || jobs[100][0].Status != "completed" || jobs[100][0].Labels[0] != "ubuntu-latest" {
		t.Fatalf("unexpected jobs %+v", jobs[100])
	}
	if jobs, _ := s.ListJobs(ctx, []Run{{ID: 100, RunAttempt: 2}}); len(jobs[100]) != 0 {
		t.Fatal("jobs from a previous attempt should be hidden")
	}
}

func TestFeedFiltersAndStuck(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seedRepo(t, s, Repo{ID: 1, Owner: "acme", Name: "api"})
	seedRepo(t, s, Repo{ID: 2, Owner: "acme", Name: "old", Archived: true})
	seedRepo(t, s, Repo{ID: 3, Owner: "other", Name: "fork", Fork: true})
	seedRepo(t, s, Repo{ID: 4, Owner: "other", Name: "web"})

	now := t0.Add(time.Hour)
	runs := []Run{
		{ID: 1, RepoID: 1, HeadBranch: "main", Event: "push", Status: "completed", Conclusion: "failure", CreatedAt: t0, UpdatedAt: t0},
		{ID: 2, RepoID: 1, HeadBranch: "feat", Event: "pull_request", Status: "queued", CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now},
		{ID: 3, RepoID: 2, HeadBranch: "main", Event: "push", Status: "completed", Conclusion: "success", CreatedAt: t0, UpdatedAt: t0},
		{ID: 4, RepoID: 3, HeadBranch: "main", Event: "push", Status: "completed", Conclusion: "success", CreatedAt: t0, UpdatedAt: t0},
		{ID: 5, RepoID: 4, HeadBranch: "main", Event: "push", ActorLogin: "dependabot[bot]", ActorIsBot: true, Status: "completed", Conclusion: "success", CreatedAt: t0, UpdatedAt: t0},
		{ID: 6, RepoID: 4, HeadBranch: "main", Event: "push", Status: "in_progress", CreatedAt: now.Add(-20 * time.Minute), RunStartedAt: now.Add(-20 * time.Minute), UpdatedAt: now},
		{ID: 7, RepoID: 4, HeadBranch: "main", Event: "push", Status: "queued", CreatedAt: now.Add(-time.Minute), UpdatedAt: now},
	}
	for _, r := range runs {
		r.RunAttempt = 1
		mustUpsertRun(t, s, r)
	}
	// Run 6 is in progress but one of its jobs has been waiting for a runner.
	if _, err := s.UpsertJob(ctx, Job{ID: 61, RunID: 6, RunAttempt: 1, Status: "queued", CreatedAt: now.Add(-15 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-5 * time.Minute)
	ids := func(f FeedFilter) []int64 {
		t.Helper()
		f.Scope = Scope{All: true}
		feed, err := s.Feed(ctx, f, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range feed {
			out = append(out, r.ID)
		}
		return out
	}
	assertIDs := func(name string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
		seen := map[int64]bool{}
		for _, id := range got {
			seen[id] = true
		}
		for _, id := range want {
			if !seen[id] {
				t.Fatalf("%s: got %v want %v", name, got, want)
			}
		}
	}

	assertIDs("default visibility", ids(FeedFilter{}), 1, 2, 6, 7)
	assertIDs("show everything", ids(FeedFilter{ShowArchived: true, ShowForks: true, ShowBots: true}), 1, 2, 3, 4, 5, 6, 7)
	assertIDs("default branch only", ids(FeedFilter{DefaultOnly: true}), 1, 6, 7)
	assertIDs("owner", ids(FeedFilter{Owner: "other"}), 6, 7)
	assertIDs("repo", ids(FeedFilter{Repo: "acme/api"}), 1, 2)
	assertIDs("event", ids(FeedFilter{Event: "pull_request"}), 2)
	assertIDs("active", ids(FeedFilter{Status: StatusActive}), 2, 6, 7)
	assertIDs("failure", ids(FeedFilter{Status: StatusFailure}), 1)
	assertIDs("stuck", ids(FeedFilter{Status: StatusStuck}), 2, 6)

	all := Scope{All: true}
	feed, _ := s.Feed(ctx, FeedFilter{Scope: all}, cutoff)
	if feed[0].ID != 7 {
		t.Fatalf("feed should be newest first, got %d first", feed[0].ID)
	}
	for _, r := range feed {
		if r.ID == 6 && (len(r.Jobs) != 1 || !r.Stuck) {
			t.Fatalf("run 6: jobs=%d stuck=%v", len(r.Jobs), r.Stuck)
		}
		if r.ID == 7 && r.Stuck {
			t.Fatal("run 7 has been queued under the threshold")
		}
	}

	sum, err := s.Summary(ctx, FeedFilter{Scope: all}, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Active != 3 || sum.Stuck != 2 || sum.FailingDefault != 1 {
		t.Fatalf("summary %+v", sum)
	}

	// A later green run on main clears the failing count.
	mustUpsertRun(t, s, Run{ID: 8, RepoID: 1, RunAttempt: 1, HeadBranch: "main", Event: "push", Status: "completed", Conclusion: "success", CreatedAt: t0.Add(time.Minute), UpdatedAt: t0.Add(time.Minute)})
	sum, _ = s.Summary(ctx, FeedFilter{Scope: all}, cutoff)
	if sum.FailingDefault != 0 {
		t.Fatalf("expected no failing workflows, got %d", sum.FailingDefault)
	}
}

func TestDeliveriesNotificationsAndPrune(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if ok, _ := s.RecordDelivery(ctx, "abc", t0); !ok {
		t.Fatal("first delivery should be new")
	}
	if ok, _ := s.RecordDelivery(ctx, "abc", t0); ok {
		t.Fatal("duplicate delivery should be rejected")
	}
	if ok, _ := s.MarkNotified(ctx, 1, 1, "failure", t0); !ok {
		t.Fatal("first notification should be new")
	}
	if ok, _ := s.MarkNotified(ctx, 1, 1, "failure", t0); ok {
		t.Fatal("duplicate notification")
	}
	if ok, _ := s.MarkNotified(ctx, 1, 2, "failure", t0); !ok {
		t.Fatal("a new attempt should notify again")
	}

	seedRepo(t, s, Repo{ID: 1})
	mustUpsertRun(t, s, Run{ID: 1, RepoID: 1, RunAttempt: 1, Status: "completed", UpdatedAt: t0})
	mustUpsertRun(t, s, Run{ID: 2, RepoID: 1, RunAttempt: 1, Status: "completed", UpdatedAt: t0.Add(48 * time.Hour)})
	mustUpsertRun(t, s, Run{ID: 3, RepoID: 1, RunAttempt: 1, Status: "in_progress", UpdatedAt: t0})
	s.UpsertJob(ctx, Job{ID: 1, RunID: 1, RunAttempt: 1, Status: "completed"})

	if err := s.Prune(ctx, t0.Add(24*time.Hour), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRun(ctx, 1); err == nil {
		t.Fatal("old completed run should be pruned")
	}
	if _, err := s.GetRun(ctx, 2); err != nil {
		t.Fatal("recent run should survive")
	}
	if _, err := s.GetRun(ctx, 3); err != nil {
		t.Fatal("unfinished runs are never pruned")
	}
	if ok, _ := s.RecordDelivery(ctx, "abc", t0); !ok {
		t.Fatal("old delivery should be pruned")
	}
}

func TestDeleteInstallationCascades(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	s.UpsertInstallation(ctx, Installation{ID: 9, Account: "acme"})
	seedRepo(t, s, Repo{ID: 1, InstallationID: 9})
	mustUpsertRun(t, s, Run{ID: 1, RepoID: 1, RunAttempt: 1, Status: "completed", UpdatedAt: t0})
	if err := s.DeleteInstallation(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if repos, _ := s.ListRepos(ctx, 0); len(repos) != 0 {
		t.Fatal("repos should be gone")
	}
	if _, err := s.GetRun(ctx, 1); err == nil {
		t.Fatal("runs should be gone")
	}
}

func TestUpsertRepoKeepsDefaultBranchFromPartialPayload(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seedRepo(t, s, Repo{ID: 1, InstallationID: 9, DefaultBranch: "trunk"})
	if err := s.UpsertRepo(ctx, Repo{ID: 1, Owner: "acme", Name: "api", FullName: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRepo(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.DefaultBranch != "trunk" || r.InstallationID != 9 {
		t.Fatalf("got %+v", r)
	}
}

func TestOpenMigratesOldDatabase(t *testing.T) {
	path := t.TempDir() + "/old.db"
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE jobs (id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL, run_attempt INTEGER NOT NULL DEFAULT 1,
		name TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', conclusion TEXT NOT NULL DEFAULT '', labels TEXT NOT NULL DEFAULT '',
		runner_name TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL DEFAULT 0,
		completed_at INTEGER NOT NULL DEFAULT 0, html_url TEXT NOT NULL DEFAULT '');
		INSERT INTO jobs (id, run_id, name) VALUES (1, 2, 'old');`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	j, err := s.GetJob(ctx, 1)
	if err != nil || j.Name != "old" {
		t.Fatalf("existing job unreadable after migration: %+v %v", j, err)
	}
	j.Steps = []Step{{Number: 1, Name: "Set up job", Status: "completed", StartedAt: t0}}
	if _, err := s.UpsertJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if j, _ = s.GetJob(ctx, 1); len(j.Steps) != 1 || !j.Steps[0].StartedAt.Equal(t0) {
		t.Fatalf("steps round-trip: %+v", j.Steps)
	}
}

func TestScopes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seedRepo(t, s, Repo{ID: 1, Owner: "acme", Name: "api"})
	seedRepo(t, s, Repo{ID: 2, Owner: "other", Name: "web"})
	mustUpsertRun(t, s, Run{ID: 1, RepoID: 1, RunAttempt: 1, Status: "completed", CreatedAt: t0, UpdatedAt: t0})
	mustUpsertRun(t, s, Run{ID: 2, RepoID: 2, RunAttempt: 1, Status: "completed", CreatedAt: t0, UpdatedAt: t0})
	s.SetUserRepos(ctx, 42, []UserRepo{{RepoID: 2, CanWrite: true}})

	count := func(sc Scope) int {
		feed, err := s.Feed(ctx, FeedFilter{Scope: sc}, t0)
		if err != nil {
			t.Fatal(err)
		}
		return len(feed)
	}
	if count(Scope{}) != 0 {
		t.Fatal("the zero scope must see nothing")
	}
	if count(Scope{All: true}) != 2 || count(Scope{UserID: 42}) != 1 || count(Scope{Owners: []string{"acme"}}) != 1 {
		t.Fatal("scope filtering")
	}
	if ok, _ := s.CanSeeRepo(ctx, Scope{UserID: 42}, 1); ok {
		t.Fatal("user 42 has no access to repo 1")
	}
	if w, _ := s.CanWrite(ctx, 42, 2); !w {
		t.Fatal("write access")
	}
	opts, _ := s.FilterOptions(ctx, Scope{UserID: 42})
	if len(opts.Owners) != 1 || opts.Owners[0] != "other" {
		t.Fatalf("filter options leak other repos: %+v", opts)
	}
	runs, _ := s.MetricRuns(ctx, Scope{Owners: []string{"acme"}}, t0.Add(-time.Hour))
	if len(runs) != 1 || runs[0].RepoID != 1 {
		t.Fatalf("metric runs scoped: %+v", runs)
	}
}
