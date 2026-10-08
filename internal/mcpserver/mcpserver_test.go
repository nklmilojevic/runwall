package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/cost"
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

func setup(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	st := testutil.Store(t)
	ctx := context.Background()
	for _, r := range []store.Repo{{ID: 1, Owner: "acme", Name: "api", DefaultBranch: "main"}, {ID: 2, Owner: "secret", Name: "vault", DefaultBranch: "main"}} {
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
	st.SetUserRepos(ctx, 42, []store.UserRepo{{RepoID: 1}})
	token := auth.RandomToken("gad_")
	st.CreateAPIToken(ctx, 42, "test", auth.Hash(token), now)

	a, err := auth.New(auth.Config{Key: auth.ParseKey("k")}, st, testutil.Logger())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Auth: a, Logs: fakeLogs{}, Costs: cost.Default(), Log: testutil.Logger(), Now: func() time.Time { return now }, Version: "test"}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, token
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
	srv, _ := setup(t)
	for _, h := range []string{"", "Bearer nope"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
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
	srv, token := setup(t)
	cs := connect(t, srv.URL, token)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(tools.Tools))
	}
	for _, tl := range tools.Tools {
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
