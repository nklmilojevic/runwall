package scoring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/testutil"
)

func cat(b Breakdown, name string) Category {
	for _, c := range b.Categories {
		if c.Name == name {
			return c
		}
	}
	return Category{}
}

func TestGradeHealthyRepo(t *testing.T) {
	in := inputs{
		repo: store.Repo{Description: "A thing", PushedAt: time.Now().Add(-24 * time.Hour), OpenIssues: 3},
		tree: []string{"README.md", "LICENSE", "CONTRIBUTING.md", ".github/SECURITY.md", "CODE_OF_CONDUCT.md",
			"docs/intro.md", "internal/x_test.go", ".golangci.yml", ".editorconfig"},
		dependabo: alertCount{available: true, enabled: true},
		codeScan:  alertCount{available: true, enabled: true},
		secrets:   alertCount{available: true, enabled: true},
		protected: github.Ptr(true), ciRuns: 40, ciSuccess: 0.97, hasTestJobs: true, communityPct: -1,
	}
	b := grade(in)
	if got := Total(b); got != 100 || TierFor(got) != Gold {
		t.Fatalf("healthy repo scored %d: %+v", got, b)
	}
}

func TestGradeWithoutContentsPermission(t *testing.T) {
	in := inputs{
		repo:      store.Repo{PushedAt: time.Now().Add(-200 * 24 * time.Hour), OpenIssues: 300},
		tree:      nil, // Contents: read missing
		dependabo: alertCount{available: true, enabled: true, critical: 1, high: 2},
		codeScan:  alertCount{available: true}, // turned off
		secrets:   alertCount{},                // no permission
		ciRuns:    5, ciSuccess: 0.5, communityPct: -1,
	}
	b := grade(in)
	if c := cat(b, "Code quality"); c.Available || c.Missing != "Contents: read" {
		t.Fatalf("code quality should be skipped and say why: %+v", c)
	}
	if c := cat(b, "Community"); c.Available {
		t.Fatal("community can't be checked without the tree or a public profile")
	}
	sec := cat(b, "Security")
	if !sec.Available || sec.Score != 23 { // dependabot 100-25-30=45, code scanning off=0 → 22.5 rounds to 23
		t.Fatalf("security %+v", sec)
	}
	got := Total(b)
	if got >= 50 {
		t.Fatalf("neglected repo should be unranked, got %d", got)
	}
	// Unavailable categories are excluded, not counted as zero.
	var sum, w float64
	for _, c := range b.Categories {
		if c.Available {
			sum += float64(c.Score) * c.Weight
			w += c.Weight
		}
	}
	if got != clamp(sum/w) {
		t.Fatal("weights should be redistributed over available categories")
	}
}

type clients struct{ c *github.Client }

func (c clients) App() *github.Client               { return c.c }
func (c clients) Installation(int64) *github.Client { return c.c }

func TestScoreAgainstFakeGitHub(t *testing.T) {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	notAccessible := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		write(w, map[string]string{"message": "Resource not accessible by integration"})
	}
	mux.HandleFunc("GET /repos/acme/api/git/trees/main", notAccessible)
	mux.HandleFunc("GET /repos/acme/api/dependabot/alerts", func(w http.ResponseWriter, r *http.Request) {
		write(w, []map[string]any{{"number": 1, "state": "open", "security_advisory": map[string]string{"severity": "high"}}})
	})
	mux.HandleFunc("GET /repos/acme/api/code-scanning/alerts", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		write(w, map[string]string{"message": "no analysis found"})
	})
	mux.HandleFunc("GET /repos/acme/api/secret-scanning/alerts", notAccessible)
	mux.HandleFunc("GET /repos/acme/api/branches/main/protection", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		write(w, map[string]string{"message": "Branch not protected"})
	})
	mux.HandleFunc("GET /repos/acme/api/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		write(w, []map[string]string{{"type": "pull_request"}})
	})
	mux.HandleFunc("GET /repos/acme/api/community/profile", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"health_percentage": 71})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	c, _ := github.NewClient(github.WithURLs(&base, nil))

	st := testutil.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	repo := store.Repo{ID: 1, InstallationID: 7, Owner: "acme", Name: "api", FullName: "acme/api", DefaultBranch: "main", PushedAt: now}
	st.UpsertRepo(ctx, repo)
	st.UpsertRun(ctx, store.Run{ID: 1, RepoID: 1, RunAttempt: 1, WorkflowName: "Test", HeadBranch: "main", Status: "completed", Conclusion: "success", CreatedAt: now.Add(-time.Hour), UpdatedAt: now})

	sc := &Scorer{GH: clients{c}, Store: st, Log: testutil.Logger(), Now: func() time.Time { return now }}
	if err := sc.ScoreDue(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetScore(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	b := ParseBreakdown(got.Breakdown)
	if c := cat(b, "CI/CD"); c.Score != 100 {
		t.Fatalf("ruleset protection plus a green default branch: %+v", c)
	}
	if c := cat(b, "Community"); !c.Available || c.Score != 71 {
		t.Fatalf("public repo falls back to the community profile: %+v", c)
	}
	if c := cat(b, "Security"); c.Score != 43 { // dependabot 85, code scanning off 0 → 42.5 rounds to 43
		t.Fatalf("security %+v", c)
	}

	// Graded repos aren't rescored until the interval passes.
	st.SaveScore(ctx, store.RepoScore{RepoID: 1, Score: 1, Tier: None, Breakdown: "{}", ComputedAt: now})
	sc.ScoreDue(ctx)
	if again, _ := st.GetScore(ctx, 1); again.Score != 1 {
		t.Fatal("fresh grades should not be recomputed")
	}
}
