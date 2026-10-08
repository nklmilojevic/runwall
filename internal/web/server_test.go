package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/cost"
	"github.com/nklmilojevic/runwall/internal/demo"
	"github.com/nklmilojevic/runwall/internal/ghapp"
	"github.com/nklmilojevic/runwall/internal/hub"
	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/syncer"
	"github.com/nklmilojevic/runwall/internal/testutil"
)

var now = time.Now().UTC().Truncate(time.Second)

type fakeSyncer struct {
	fetched   []int64
	refreshed []int64
	triggered int
	err       error
	log       string
	logErr    error
	annErr    error
	st        *store.Store
}

func (f *fakeSyncer) FetchJobsForRun(ctx context.Context, id int64) error {
	f.fetched = append(f.fetched, id)
	if f.err != nil {
		return f.err
	}
	_, err := f.st.UpsertJob(ctx, store.Job{ID: 1, RunID: id, RunAttempt: 1, Name: "fetched-on-demand", Status: "completed", Conclusion: "success"})
	return err
}

func (f *fakeSyncer) RefreshJob(ctx context.Context, id int64) error {
	f.refreshed = append(f.refreshed, id)
	j, err := f.st.GetJob(ctx, id)
	if err != nil {
		return err
	}
	j.Steps = append(j.Steps, store.Step{Number: len(j.Steps) + 1, Name: "polled step", Status: "in_progress", StartedAt: now.Add(-3 * time.Second)})
	_, err = f.st.UpsertJob(ctx, j)
	return err
}

func (f *fakeSyncer) RefreshRun(context.Context, int64) error       { return nil }
func (f *fakeSyncer) JobLog(context.Context, int64) (string, error) { return f.log, f.logErr }
func (f *fakeSyncer) LastSync() (time.Time, error)                  { return now.Add(-2 * time.Minute), nil }
func (f *fakeSyncer) TriggerAll()                                   { f.triggered++ }
func (f *fakeSyncer) Budget(context.Context) (ghapp.Rate, bool) {
	return ghapp.Rate{Limit: 12500, Remaining: 1200, Reset: now.Add(20 * time.Minute)}, true
}
func (f *fakeSyncer) WorkflowFile(context.Context, int64) (string, error) {
	return "", syncer.ErrPermission{Permission: "Contents: read"}
}
func (f *fakeSyncer) Annotations(context.Context, int64) ([]syncer.Annotation, error) {
	return []syncer.Annotation{{Level: "failure", Path: "main.go", Line: 3, Message: "<boom>"}}, f.annErr
}

type env struct {
	srv  *httptest.Server
	s    *Server
	sync *fakeSyncer
}

// newDemo serves seeded demo data where everyone is the all-seeing demo user.
func newDemo(t *testing.T) env {
	t.Helper()
	st := testutil.Store(t)
	if err := demo.Seed(context.Background(), st, now); err != nil {
		t.Fatal(err)
	}
	fs := &fakeSyncer{st: st}
	s := &Server{Store: st, Hub: hub.New(), Syncer: fs, Demo: true, Costs: cost.Default(), StuckThreshold: 5 * time.Minute,
		BaseURL: "http://dash.local", Log: testutil.Logger(), Now: func() time.Time { return now }}
	return serve(t, s, fs)
}

func serve(t *testing.T, s *Server, fs *fakeSyncer) env {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return env{srv, s, fs}
}

// newAuthed serves real sessions: user 42 can see repo 1 (write) and repo 2 (read), not repo 3.
func newAuthed(t *testing.T) (env, *http.Cookie, *http.Cookie) {
	t.Helper()
	st := testutil.Store(t)
	ctx := context.Background()
	for _, r := range []store.Repo{{ID: 1, Owner: "acme", Name: "api"}, {ID: 2, Owner: "acme", Name: "web"}, {ID: 3, Owner: "secret", Name: "vault"}} {
		r.FullName, r.DefaultBranch, r.InstallationID = r.Owner+"/"+r.Name, "main", 7
		st.UpsertRepo(ctx, r)
	}
	st.UpsertInstallation(ctx, store.Installation{ID: 7, Account: "acme"})
	for _, r := range []store.Run{
		{ID: 100, RepoID: 1, WorkflowID: 5, WorkflowName: "CI", HeadBranch: "main", Status: "completed", Conclusion: "failure"},
		{ID: 200, RepoID: 2, WorkflowID: 6, WorkflowName: "Deploy", HeadBranch: "main", Status: "completed", Conclusion: "success"},
		{ID: 300, RepoID: 3, WorkflowID: 7, WorkflowName: "Hidden", HeadBranch: "main", Status: "completed", Conclusion: "failure"},
	} {
		r.RunAttempt, r.CreatedAt, r.UpdatedAt, r.Title = 1, now.Add(-time.Hour), now.Add(-50*time.Minute), "title "+r.WorkflowName
		st.UpsertRun(ctx, r)
	}
	st.UpsertJob(ctx, store.Job{ID: 3000, RunID: 300, RunAttempt: 1, Name: "secret job", Status: "completed"})
	st.UpsertUser(ctx, store.User{ID: 42, Login: "nkl"})
	st.SetUserRepos(ctx, 42, []store.UserRepo{{RepoID: 1, CanWrite: true}, {RepoID: 2}})

	a, err := auth.New(auth.Config{Key: auth.ParseKey("k"), Admins: []string{"nkl"}, BaseURL: "http://dash.local"}, st, testutil.Logger())
	if err != nil {
		t.Fatal(err)
	}
	session := func(se store.Session) *http.Cookie {
		sid := auth.RandomToken("")
		se.IDHash, se.CreatedAt, se.LastSeenAt, se.ReposCheckedAt = auth.Hash(sid), time.Now(), time.Now(), time.Now()
		if err := st.SaveSession(ctx, se); err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: "session", Value: sid}
	}
	user := session(store.Session{UserID: 42, CSRF: "csrf-user"})
	st.CreateKioskLink(ctx, "TV", auth.Hash("kiosk-token"), []string{"acme"}, "nkl", now)
	links, _ := st.ListKioskLinks(ctx)
	kiosk := session(store.Session{KioskID: links[0].ID, CSRF: "csrf-kiosk"})

	fs := &fakeSyncer{st: st}
	s := &Server{Store: st, Hub: hub.New(), Syncer: fs, Auth: a, Costs: cost.Default(), StuckThreshold: 5 * time.Minute,
		BaseURL: "http://dash.local", Log: testutil.Logger(), Now: func() time.Time { return now }}
	return serve(t, s, fs), user, kiosk
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func do(t *testing.T, method, url string, c *http.Cookie, headers map[string]string, body io.Reader) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	if c != nil {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func get(t *testing.T, url string, c *http.Cookie, headers map[string]string) (int, string) {
	t.Helper()
	resp, body := do(t, http.MethodGet, url, c, headers, nil)
	return resp.StatusCode, body
}

func TestDemoPagesRender(t *testing.T) {
	e := newDemo(t)
	pages := map[string][]string{
		"/":            {"Active workflows", "Active pipelines", "Run trends", "Run distribution", "Recent runs", `data-chart=`, "Estimated cost"},
		"/runs":        {"API budget 1,200 / 12,500", "budget-low", `id="pulse"`, `id="filters"`, `class="run `, `hx-get="/runs"`},
		"/runs/1005":   {"Jobs", "Workflow file", "Re-run all jobs", `id="run-detail"`},
		"/workflows":   {"Success rate", "p95", "Est. cost"},
		"/repos":       {"repo-card", "tier-"},
		"/repos/1":     {"Refresh grade", "Security", "Code quality", "needs Contents: read"},
		"/settings":    {"API tokens for MCP", "Kiosk links", "Recent run actions", "http://dash.local/mcp"},
		"/workflows/1": nil, // not a route: 404
	}
	for path, wants := range pages {
		code, body := get(t, e.srv.URL+path, nil, nil)
		if wants == nil {
			if code != http.StatusNotFound {
				t.Errorf("%s: %d", path, code)
			}
			continue
		}
		if code != 200 {
			t.Errorf("%s: status %d", path, code)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", path, w)
			}
		}
		if !strings.Contains(body, `&#34;X-CSRF-Token&#34;: &#34;demo&#34;`) && !strings.Contains(body, `"X-CSRF-Token": "demo"`) {
			t.Errorf("%s: htmx requests must carry the CSRF token", path)
		}
	}
}

func TestLiveFragments(t *testing.T) {
	e := newDemo(t)
	_, body := get(t, e.srv.URL+"/runs?bots=1&org=acme", nil, map[string]string{"HX-Request": "true", "HX-Target": "feed"})
	if strings.Contains(body, "<html") || strings.Count(body, `hx-swap-oob="true"`) != 2 {
		t.Errorf("feed fragment should be rows plus summary and pulse")
	}
	if strings.Contains(body, "acme-labs/") || !strings.Contains(body, "acme/") {
		t.Error("org filter not applied")
	}
	_, body = get(t, e.srv.URL+"/", nil, map[string]string{"HX-Request": "true", "HX-Target": "dash"})
	if strings.Contains(body, "<html") || !strings.Contains(body, "Active pipelines") {
		t.Error("dashboard fragment")
	}
}

func TestSignInRequired(t *testing.T) {
	e, _, _ := newAuthed(t)
	resp, _ := do(t, http.MethodGet, e.srv.URL+"/runs?org=acme", nil, nil, nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login?next="+url.QueryEscape("/runs?org=acme") {
		t.Fatalf("anonymous page view: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = do(t, http.MethodGet, e.srv.URL+"/runs", nil, map[string]string{"HX-Request": "true"}, nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("HX-Redirect"), "/login") {
		t.Fatalf("anonymous htmx request: %d", resp.StatusCode)
	}
	for _, p := range []string{"/events", "/runs/100/jobs"} {
		if code, _ := get(t, e.srv.URL+p, nil, map[string]string{"HX-Request": "true"}); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, body := get(t, e.srv.URL+"/login", nil, nil); code != 200 || !strings.Contains(body, "Sign in with GitHub") {
		t.Fatal("login page")
	}
	if code, _ := get(t, e.srv.URL+"/healthz", nil, nil); code != 200 {
		t.Fatal("healthz must stay public")
	}
}

func TestPerUserVisibility(t *testing.T) {
	e, user, kiosk := newAuthed(t)
	_, body := get(t, e.srv.URL+"/runs?bots=1", user, nil)
	if !strings.Contains(body, "title CI") || !strings.Contains(body, "title Deploy") || strings.Contains(body, "Hidden") {
		t.Fatal("feed must show only the user's repos")
	}
	_, body = get(t, e.srv.URL+"/", user, nil)
	if strings.Contains(body, "Hidden") || strings.Contains(body, "secret/vault") {
		t.Fatal("dashboard leaks a hidden repo")
	}
	for _, p := range []string{"/runs/300", "/runs/300/jobs", "/jobs/3000/log", "/jobs/3000/body", "/repos/3", "/workflows/3/7", "/runs/300/workflow-file"} {
		if code, _ := get(t, e.srv.URL+p, user, nil); code != http.StatusNotFound {
			t.Errorf("%s: want 404 for a hidden repo, got %d", p, code)
		}
	}
	_, body = get(t, e.srv.URL+"/repos", user, nil)
	if strings.Contains(body, "vault") {
		t.Fatal("repositories page leaks a hidden repo")
	}

	// Write access decides whether action buttons appear.
	if _, body = get(t, e.srv.URL+"/runs/100", user, nil); !strings.Contains(body, "Re-run failed jobs") {
		t.Error("writer should see re-run buttons")
	}
	if _, body = get(t, e.srv.URL+"/runs/200", user, nil); strings.Contains(body, "Re-run all jobs") || !strings.Contains(body, "need write access") {
		t.Error("reader should not see re-run buttons")
	}

	// The kiosk sees its owners, read-only, and no settings.
	if _, body = get(t, e.srv.URL+"/runs/100", kiosk, nil); strings.Contains(body, "Re-run") || strings.Contains(body, `href="/settings"`) {
		t.Error("kiosk must be read-only")
	}
	if resp, _ := do(t, http.MethodGet, e.srv.URL+"/settings", kiosk, nil, nil); resp.StatusCode != http.StatusFound {
		t.Error("kiosk can't open settings")
	}
}

func TestMutationsNeedCSRFAndWriteAccess(t *testing.T) {
	e, user, kiosk := newAuthed(t)
	post := func(path string, c *http.Cookie, csrf string) *http.Response {
		h := map[string]string{"HX-Request": "true"}
		if csrf != "" {
			h["X-CSRF-Token"] = csrf
		}
		resp, _ := do(t, http.MethodPost, e.srv.URL+path, c, h, nil)
		return resp
	}
	if r := post("/sync", user, ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: %d", r.StatusCode)
	}
	if r := post("/sync", user, "wrong"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("bad CSRF: %d", r.StatusCode)
	}
	if r := post("/sync", user, "csrf-user"); r.StatusCode != http.StatusNoContent || !strings.Contains(r.Header.Get("HX-Trigger"), "toast") || e.sync.triggered != 1 {
		t.Fatalf("sync: %d", r.StatusCode)
	}
	if r := post("/sync", kiosk, "csrf-kiosk"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("kiosk mutation: %d", r.StatusCode)
	}
	if r := post("/runs/200/rerun", user, "csrf-user"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("re-run without write access: %d", r.StatusCode)
	}
	if r := post("/runs/300/cancel", user, "csrf-user"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("action on a hidden repo: %d", r.StatusCode)
	}
}

func TestDemoActionsAndTokens(t *testing.T) {
	e := newDemo(t)
	resp, _ := do(t, http.MethodPost, e.srv.URL+"/runs/1005/rerun-failed", nil, map[string]string{"X-CSRF-Token": "demo"}, nil)
	if resp.StatusCode != http.StatusNoContent || !strings.Contains(resp.Header.Get("HX-Trigger"), "demo mode") {
		t.Fatalf("demo re-run: %d %s", resp.StatusCode, resp.Header.Get("HX-Trigger"))
	}

	form := url.Values{"csrf": {"demo"}, "name": {"laptop"}}
	resp, body := do(t, http.MethodPost, e.srv.URL+"/settings/tokens", nil, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, strings.NewReader(form.Encode()))
	if resp.StatusCode != 200 || !strings.Contains(body, "gad_") || !strings.Contains(body, "claude mcp add --transport http runwall http://dash.local/mcp") {
		t.Fatalf("token creation: %d", resp.StatusCode)
	}
	tokens, _ := e.s.Store.ListAPITokens(context.Background(), 1)
	if len(tokens) != 1 || tokens[0].Name != "laptop" {
		t.Fatalf("token stored: %+v", tokens)
	}

	form = url.Values{"csrf": {"demo"}, "name": {"Office"}, "owners": {"acme", "not-an-installation"}}
	resp, body = do(t, http.MethodPost, e.srv.URL+"/settings/kiosk", nil, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, strings.NewReader(form.Encode()))
	if resp.StatusCode != 200 || !strings.Contains(body, "http://dash.local/kiosk/kiosk_") {
		t.Fatalf("kiosk creation: %d", resp.StatusCode)
	}
	links, _ := e.s.Store.ListKioskLinks(context.Background())
	if len(links) != 1 || len(links[0].Owners) != 1 || links[0].Owners[0] != "acme" {
		t.Fatalf("kiosk owners must be existing installations: %+v", links)
	}
}

func TestJobEndpoints(t *testing.T) {
	e := newDemo(t)
	ctx := context.Background()
	st := e.s.Store

	st.UpsertJob(ctx, store.Job{ID: 77, RunID: 1000, RunAttempt: 1, Name: "build", Status: "in_progress", HTMLURL: "https://github.com/x/job/77"})
	_, body := get(t, e.srv.URL+"/jobs/77/body", nil, nil)
	for _, want := range []string{`hx-get="/jobs/77/body"`, "every 5s", "polled step", "Watch live log on GitHub"} {
		if !strings.Contains(body, want) {
			t.Errorf("running job body missing %q", want)
		}
	}
	get(t, e.srv.URL+"/jobs/77/body", nil, nil)
	if len(e.sync.refreshed) != 1 {
		t.Fatalf("refreshes within the poll window should be shared, got %d", len(e.sync.refreshed))
	}

	st.UpsertJob(ctx, store.Job{ID: 78, RunID: 1000, RunAttempt: 1, Name: "lint", Status: "completed", Conclusion: "failure", CheckRunID: 9})
	_, body = get(t, e.srv.URL+"/jobs/78/body", nil, nil)
	for _, want := range []string{`hx-get="/jobs/78/log"`, `hx-post="/jobs/78/rerun"`, `hx-get="/jobs/78/annotations"`} {
		if !strings.Contains(body, want) {
			t.Errorf("finished job body missing %q", want)
		}
	}

	e.sync.log = "2026-10-08T09:56:04Z ##[group]Run make\n2026-10-08T09:56:06Z ##[error]exit 2\n"
	if _, body = get(t, e.srv.URL+"/jobs/78/log", nil, nil); !strings.Contains(body, `class="l l-error"`) {
		t.Error("log view")
	}
	e.sync.logErr = ghapp.ErrLogUnavailable
	if _, body = get(t, e.srv.URL+"/jobs/78/log", nil, nil); !strings.Contains(body, "published this log") {
		t.Error("unavailable log note")
	}
	if _, body = get(t, e.srv.URL+"/jobs/78/annotations", nil, nil); !strings.Contains(body, "main.go:3") || !strings.Contains(body, "&lt;boom&gt;") {
		t.Errorf("annotations: %s", body)
	}
	e.sync.annErr = errors.New("down")
	if _, body = get(t, e.srv.URL+"/jobs/78/annotations", nil, nil); !strings.Contains(body, "Couldn") {
		t.Error("annotation error note")
	}
	if _, body = get(t, e.srv.URL+"/runs/1000/workflow-file", nil, nil); !strings.Contains(body, "Contents: read") {
		t.Errorf("workflow file permission note: %s", body)
	}
}

func TestEventsAreFilteredPerViewer(t *testing.T) {
	e, user, _ := newAuthed(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/events", nil)
	req.AddCookie(user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	r.ReadString('\n')
	r.ReadString('\n')

	e.s.Hub.Publish(300) // hidden repo
	e.s.Hub.Publish(100) // visible
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	data, _ := r.ReadString('\n')
	if strings.TrimSpace(line) != "event: update" || strings.TrimSpace(data) != "data: 100" {
		t.Fatalf("expected only the visible run, got %q %q", line, data)
	}
}

func TestStaticAssetsAreVersioned(t *testing.T) {
	e := newDemo(t)
	_, page := get(t, e.srv.URL+"/", nil, nil)
	for _, name := range []string{"app.css", "app.js", "theme.js", "htmx.min.js", "sse.js", "chart.umd.min.js"} {
		u := asset(name)
		if !strings.Contains(u, "?v=") || !strings.Contains(page, u) {
			t.Errorf("%s: page should reference versioned URL %s", name, u)
		}
		resp, _ := do(t, http.MethodGet, e.srv.URL+u, nil, nil, nil)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: status %d cache %q", u, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
}
