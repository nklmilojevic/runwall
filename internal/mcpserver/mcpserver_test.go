package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v93/github"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/cost"
	"github.com/nklmilojevic/runwall/internal/pulls"
	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/testutil"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type fakeLogs struct{}

func (fakeLogs) JobLog(context.Context, int64) (string, error) {
	return "2026-10-08T10:00:00Z ##[group]Run make\n2026-10-08T10:00:01Z ##[error]boom\n", nil
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

const (
	sha5 = "5555555555555555555555555555555555555555"
	sha6 = "6666666666666666666666666666666666666666"
)

// fakePulls serves open pull requests from memory and records merges.
type fakePulls struct {
	mu     sync.Mutex
	prs    []pulls.PR
	closed map[string]bool
	done   []string
}

func (f *fakePulls) List(_ context.Context, _ *github.Client, repos []store.Repo) ([]pulls.PR, error) {
	var out []pulls.PR
	for _, p := range f.prs {
		if slices.ContainsFunc(repos, func(r store.Repo) bool { return r.ID == p.RepoID }) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakePulls) Detail(_ context.Context, _ *github.Client, repo store.Repo, number int) (pulls.Detail, error) {
	for _, p := range f.prs {
		if p.RepoID == repo.ID && p.Number == number {
			status := "OPEN"
			if f.closed[p.Repo+"#"+strconv.Itoa(number)] {
				status = "MERGED"
			}
			return pulls.Detail{PR: p, Status: status}, nil
		}
	}
	return pulls.Detail{}, pulls.ErrNotFound
}

func (f *fakePulls) record(kind string, repo store.Repo, t pulls.Target, method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = append(f.done, kind+" "+repo.FullName+"#"+strconv.Itoa(t.Number)+" "+method+" "+t.SHA)
	return nil
}

func (f *fakePulls) Merge(_ context.Context, _ *github.Client, repo store.Repo, t pulls.Target, method string) error {
	return f.record("merge", repo, t, method)
}

func (f *fakePulls) EnableAutoMerge(_ context.Context, _ *github.Client, repo store.Repo, t pulls.Target, method string) error {
	return f.record("auto", repo, t, method)
}

type env struct {
	url   string
	token string
	st    *store.Store
	s     *Server
	prs   *fakePulls
}

func setup(t *testing.T) env {
	t.Helper()
	st := testutil.Store(t)
	ctx := context.Background()
	for _, r := range []store.Repo{{ID: 1, Owner: "acme", Name: "api", DefaultBranch: "main"}, {ID: 2, Owner: "secret", Name: "vault", DefaultBranch: "main"},
		{ID: 3, Owner: "acme", Name: "docs", DefaultBranch: "main"}} {
		r.FullName = r.Owner + "/" + r.Name
		st.UpsertRepo(ctx, r)
	}
	st.UpsertRun(ctx, store.Run{ID: 10, RepoID: 1, RunAttempt: 1, WorkflowID: 5, WorkflowName: "CI", HeadBranch: "main", Event: "push",
		Status: "completed", Conclusion: "failure", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-50 * time.Minute), HTMLURL: "https://github.com/acme/api/actions/runs/10"})
	st.UpsertRun(ctx, store.Run{ID: 20, RepoID: 2, RunAttempt: 1, WorkflowName: "Hidden", HeadBranch: "main", Status: "completed",
		Conclusion: "failure", CreatedAt: now.Add(-time.Hour), UpdatedAt: now})
	st.UpsertJob(ctx, store.Job{ID: 100, RunID: 10, RunAttempt: 1, Name: "build", Status: "completed", Conclusion: "failure"})
	st.UpsertJob(ctx, store.Job{ID: 200, RunID: 20, RunAttempt: 1, Name: "secret job", Status: "completed"})
	st.UpsertUser(ctx, store.User{ID: 42, Login: "nkl"})
	st.SetUserRepos(ctx, 42, []store.UserRepo{{RepoID: 1, CanWrite: true}, {RepoID: 3}})
	token := auth.RandomToken("gad_")
	st.CreateAPIToken(ctx, 42, "test", auth.Hash(token), now)

	a, err := auth.New(auth.Config{Key: auth.ParseKey("k")}, st, testutil.Logger())
	if err != nil {
		t.Fatal(err)
	}
	pr := func(repoID int64, repo string, number int, sha string, draft bool, state string) pulls.PR {
		return pulls.PR{RepoID: repoID, Repo: repo, Owner: strings.Split(repo, "/")[0], Number: number, Title: "PR " + strconv.Itoa(number),
			Author: "renovate", HeadSHA: sha, NodeID: "PR_" + strconv.Itoa(number), Draft: draft, Mergeable: "MERGEABLE", State: state,
			DefaultMethod: pulls.MethodSquash, Methods: []string{pulls.MethodSquash, pulls.MethodRebase}}
	}
	prs := &fakePulls{closed: map[string]bool{"acme/api#8": true}, prs: []pulls.PR{
		pr(1, "acme/api", 5, sha5, false, "CLEAN"),
		pr(1, "acme/api", 6, sha6, false, "BLOCKED"),
		pr(1, "acme/api", 7, sha6, true, "DRAFT"),
		pr(1, "acme/api", 8, sha6, false, "CLEAN"),
		pr(2, "secret/vault", 1, sha5, false, "CLEAN"),
		pr(3, "acme/docs", 2, sha5, false, "CLEAN"),
	}}
	s := &Server{Store: st, Auth: a, Logs: fakeLogs{}, Pulls: prs, Costs: cost.Default(), Log: testutil.Logger(), Now: func() time.Time { return now }, Version: "test",
		GitHub: func(context.Context, *auth.Viewer) (*github.Client, error) { return github.NewClient() }}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return env{url: srv.URL, token: token, st: st, s: s, prs: prs}
}

func connect(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestRequiresToken(t *testing.T) {
	e := setup(t)
	for _, h := range []string{"", "Bearer nope"} {
		req, _ := http.NewRequest(http.MethodPost, e.url, strings.NewReader(`{}`))
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("auth %q: %d", h, resp.StatusCode)
		}
	}
}

func TestTools(t *testing.T) {
	e := setup(t)
	cs := connect(t, e.url, e.token)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 8 {
		t.Fatalf("expected 8 tools, got %d", len(tools.Tools))
	}
	for _, tl := range tools.Tools {
		if tl.Name == "merge_pull_request" {
			if tl.Annotations == nil || tl.Annotations.ReadOnlyHint || tl.Annotations.DestructiveHint == nil || !*tl.Annotations.DestructiveHint {
				t.Errorf("%s must be marked destructive", tl.Name)
			}
			continue
		}
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s should be marked read-only", tl.Name)
		}
	}

	out, _ := call(t, cs, "list_runs", nil)
	if !strings.Contains(out, `"id": 10`) || strings.Contains(out, "Hidden") {
		t.Fatalf("list_runs must only include the user's repos: %s", out)
	}
	if out, _ := call(t, cs, "get_run", map[string]any{"run_id": 10}); !strings.Contains(out, `"Name": "build"`) {
		t.Fatalf("get_run: %s", out)
	}
	if out, isErr := call(t, cs, "get_run", map[string]any{"run_id": 20}); !isErr || strings.Contains(out, "secret") {
		t.Fatalf("get_run on a hidden repo must fail without leaking: %s", out)
	}
	if out, _ := call(t, cs, "get_job_log", map[string]any{"job_id": 100}); !strings.Contains(out, "ERROR: boom") {
		t.Fatalf("get_job_log: %s", out)
	}
	if _, isErr := call(t, cs, "get_job_log", map[string]any{"job_id": 200}); !isErr {
		t.Fatal("get_job_log on a hidden repo must fail")
	}
	if out, _ := call(t, cs, "failing_workflows", nil); !strings.Contains(out, `"workflow": "CI"`) || strings.Contains(out, "Hidden") {
		t.Fatalf("failing_workflows: %s", out)
	}
	if out, _ := call(t, cs, "workflow_stats", map[string]any{"period": "7d"}); !strings.Contains(out, `"period": "7d"`) || !strings.Contains(out, `"runs": 1`) {
		t.Fatalf("workflow_stats: %s", out)
	}
	if out, _ := call(t, cs, "repo_score", map[string]any{"repo": "acme/api"}); !strings.Contains(out, "not been graded") {
		t.Fatalf("repo_score: %s", out)
	}
	if _, isErr := call(t, cs, "repo_score", map[string]any{"repo": "secret/vault"}); !isErr {
		t.Fatal("repo_score on a hidden repo must fail")
	}
}

func TestPullRequestTools(t *testing.T) {
	e := setup(t)
	cs := connect(t, e.url, e.token)

	out, isErr := call(t, cs, "list_pull_requests", nil)
	if isErr || !strings.Contains(out, `"number": 5`) || !strings.Contains(out, `"head_sha": "`+sha5+`"`) {
		t.Fatalf("list_pull_requests: %s", out)
	}
	for _, leak := range []string{"secret/vault", "acme/docs", `"number": 7`} {
		if strings.Contains(out, leak) {
			t.Fatalf("list_pull_requests must skip hidden and read-only repos and drafts (%s): %s", leak, out)
		}
	}
	if out, _ := call(t, cs, "list_pull_requests", map[string]any{"include_drafts": true}); !strings.Contains(out, `"number": 7`) {
		t.Fatalf("include_drafts: %s", out)
	}
	if out, _ := call(t, cs, "list_pull_requests", map[string]any{"ready_only": true}); strings.Contains(out, `"number": 6`) || !strings.Contains(out, `"number": 5`) {
		t.Fatalf("ready_only: %s", out)
	}

	refused := map[string]struct {
		args map[string]any
		want string
	}{
		"hidden repo":    {map[string]any{"repo": "secret/vault", "number": 1}, "not found"},
		"read-only repo": {map[string]any{"repo": "acme/docs", "number": 2}, "write access"},
		"unknown PR":     {map[string]any{"repo": "acme/api", "number": 99}, "not found"},
		"already merged": {map[string]any{"repo": "acme/api", "number": 8}, "merged"},
		"head moved":     {map[string]any{"repo": "acme/api", "number": 5, "head_sha": sha6[:12]}, "has moved"},
		"bad method":     {map[string]any{"repo": "acme/api", "number": 5, "method": "fast-forward"}, "merge, squash or rebase"},
		"bad sha":        {map[string]any{"repo": "acme/api", "number": 5, "head_sha": "xyz"}, "head_sha"},
	}
	for name, c := range refused {
		out, isErr := call(t, cs, "merge_pull_request", c.args)
		if !isErr || !strings.Contains(out, c.want) || strings.Contains(out, "secret") {
			t.Errorf("%s: want an error containing %q, got %v %s", name, c.want, isErr, out)
		}
	}
	if len(e.prs.done) != 0 {
		t.Fatalf("refused merges must not reach GitHub: %v", e.prs.done)
	}

	if out, isErr := call(t, cs, "merge_pull_request", map[string]any{"repo": "ACME/api", "number": 5, "head_sha": sha5[:7]}); isErr || !strings.Contains(out, `"merged": true`) {
		t.Fatalf("merge: %s", out)
	}
	if out, isErr := call(t, cs, "merge_pull_request", map[string]any{"repo": "acme/api", "number": 6, "auto": true, "method": "rebase"}); isErr || !strings.Contains(out, `"auto_merge_enabled": true`) {
		t.Fatalf("auto-merge: %s", out)
	}
	want := []string{"merge acme/api#5 squash " + sha5, "auto acme/api#6 rebase " + sha6}
	if !slices.Equal(e.prs.done, want) {
		t.Fatalf("GitHub calls %v, want %v", e.prs.done, want)
	}
	audit, _ := e.st.ListAudit(context.Background(), 10)
	if len(audit) != 2 || audit[0].Action != "mcp-auto-merge-pr" || audit[1].Action != "mcp-merge-pr" || audit[1].Login != "nkl" || audit[1].Result != "ok" {
		t.Fatalf("audit log: %+v", audit)
	}
}

func TestMergeNeedsAuthorizedToken(t *testing.T) {
	e := setup(t)
	e.s.GitHub = nil // use the API token's own grant, which this token doesn't have
	cs := connect(t, e.url, e.token)
	out, isErr := call(t, cs, "merge_pull_request", map[string]any{"repo": "acme/api", "number": 5})
	if !isErr || !strings.Contains(out, "Settings page") {
		t.Fatalf("merge without a grant: %v %s", isErr, out)
	}
	if len(e.prs.done) != 0 {
		t.Fatal("nothing must be merged")
	}
}
