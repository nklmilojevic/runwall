package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v93/github"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/demo"
	"github.com/nklmilojevic/runwall/internal/pulls"
	"github.com/nklmilojevic/runwall/internal/store"
)

type fakePulls struct {
	mu     sync.Mutex
	listed [][]string
	prs    []pulls.PR
	err    error
	calls  []string
	fail   map[int]error
}

func (f *fakePulls) List(_ context.Context, _ *github.Client, repos []store.Repo) ([]pulls.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, r := range repos {
		names = append(names, r.FullName)
	}
	f.listed = append(f.listed, names)
	return f.prs, f.err
}

func (f *fakePulls) record(kind string, repo store.Repo, t pulls.Target, method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s#%d %s %s", kind, repo.FullName, t.Number, method, t.SHA))
	return f.fail[t.Number]
}

func (f *fakePulls) Merge(_ context.Context, _ *github.Client, repo store.Repo, t pulls.Target, method string) error {
	return f.record("merge", repo, t, method)
}

func (f *fakePulls) EnableAutoMerge(_ context.Context, _ *github.Client, repo store.Repo, t pulls.Target, method string) error {
	return f.record("auto", repo, t, method)
}

// withPulls installs a fake pull request source and a stand-in for the viewer's GitHub client.
func withPulls(e env, fp *fakePulls) *fakePulls {
	e.s.Pulls = fp
	e.s.ghFor = func(context.Context, *auth.Viewer) (*github.Client, error) { return nil, nil }
	return fp
}

func postForm(t *testing.T, e env, path string, c *http.Cookie, form url.Values) (*http.Response, string) {
	t.Helper()
	return do(t, http.MethodPost, e.srv.URL+path, c, map[string]string{"Content-Type": "application/x-www-form-urlencoded", "HX-Request": "true"},
		strings.NewReader(form.Encode()))
}

func key(repoID int64, number int, method string) string {
	return pulls.PR{RepoID: repoID, Number: number, HeadSHA: "abcdef1234567", NodeID: "PR_x", DefaultMethod: method}.Key()
}

func TestDemoPullsPageAndFilters(t *testing.T) {
	e := newDemo(t)
	e.s.Pulls = &demo.Pulls{At: now}

	code, body := get(t, e.srv.URL+"/pulls", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`href="/pulls"`, "Pull requests", `class="pr s-ok"`, "Ready to merge", "Conflicts", "Needs review",
		`data-ready="1"`, "renovate (bot)", "Merge selected", "Enable auto-merge", `hx-post="/pulls/merge"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "WIP: interactive login") {
		t.Error("drafts are listed without the drafts toggle")
	}

	frag := map[string]string{"HX-Request": "true", "HX-Target": "pulls"}
	_, body = get(t, e.srv.URL+"/pulls?author=monalisa&drafts=1", nil, frag)
	if strings.Contains(body, "<html") || !strings.Contains(body, "Rework subscriber import batching") ||
		!strings.Contains(body, "WIP: interactive login") || strings.Contains(body, "golang.org/x/net") {
		t.Errorf("author filter fragment wrong:\n%s", body)
	}
	_, body = get(t, e.srv.URL+"/pulls?ready=1", nil, frag)
	if strings.Contains(body, "Rework subscriber import batching") || !strings.Contains(body, "golang.org/x/net") {
		t.Errorf("ready filter wrong:\n%s", body)
	}
}

func TestDemoMergeRemovesMergedPulls(t *testing.T) {
	e := newDemo(t)
	e.s.Pulls = &demo.Pulls{At: now}
	_, body := get(t, e.srv.URL+"/pulls", nil, nil)
	if !strings.Contains(body, "golang.org/x/net") {
		t.Fatal("demo PR missing")
	}
	prs, _ := e.s.Pulls.List(context.Background(), nil, []store.Repo{{ID: 1, Owner: "acme", Name: "api", FullName: "acme/api"}})
	var ready, blocked pulls.PR
	for _, p := range prs {
		switch {
		case p.Ready():
			ready = p
		case p.State == "BLOCKED":
			blocked = p
		}
	}

	resp, body := postForm(t, e, "/pulls/merge", nil, url.Values{"csrf": {"demo"}, "action": {"merge"}, "method": {"default"}, "pr": {ready.Key(), blocked.Key()}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	trig := resp.Header.Get("HX-Trigger")
	if !strings.Contains(trig, "pulls-changed") || !strings.Contains(trig, `"kind":"error"`) {
		t.Errorf("HX-Trigger = %s", trig)
	}
	if !strings.Contains(body, "Merged 1 of 2 pull requests.") || !strings.Contains(body, "base branch policy prohibits the merge") {
		t.Errorf("results:\n%s", body)
	}
	_, body = get(t, e.srv.URL+"/pulls", nil, nil)
	if strings.Contains(body, ready.Title) || !strings.Contains(body, blocked.Title) {
		t.Error("merged PR still listed, or the refused one vanished")
	}
}

func TestPullsListOnlyWritableRepos(t *testing.T) {
	e, user, kiosk := newAuthed(t)
	fp := &fakePulls{prs: []pulls.PR{{RepoID: 1, Repo: "acme/api", Owner: "acme", Number: 5, Title: "Bump x", URL: "https://github.com/acme/api/pull/5",
		HeadSHA: "abcdef1234567", Mergeable: "MERGEABLE", State: "CLEAN", DefaultMethod: "squash"}}}
	withPulls(e, fp)

	code, body := get(t, e.srv.URL+"/pulls", user, nil)
	if code != http.StatusOK || !strings.Contains(body, "Bump x") {
		t.Fatalf("status %d", code)
	}
	if len(fp.listed) != 1 || strings.Join(fp.listed[0], ",") != "acme/api" {
		t.Fatalf("listed %v, want only the writable repo", fp.listed)
	}
	// A second visit within the cache window doesn't ask GitHub again; refresh does.
	get(t, e.srv.URL+"/pulls", user, nil)
	get(t, e.srv.URL+"/pulls?refresh=1", user, nil)
	if len(fp.listed) != 2 {
		t.Fatalf("GitHub read %d times, want 2", len(fp.listed))
	}
	if code, _ := get(t, e.srv.URL+"/pulls", kiosk, nil); code != http.StatusNotFound {
		t.Errorf("kiosk: %d", code)
	}
	if code, _ := get(t, e.srv.URL+"/pulls", nil, nil); code != http.StatusFound {
		t.Errorf("anonymous: %d", code)
	}
}

func TestPullsPermissionNote(t *testing.T) {
	e, user, _ := newAuthed(t)
	withPulls(e, &fakePulls{err: fmt.Errorf("%w: Resource not accessible by integration", pulls.ErrPermission)})
	_, body := get(t, e.srv.URL+"/pulls", user, nil)
	if !strings.Contains(body, "lacks a permission") || !strings.Contains(body, "No open pull requests.") {
		t.Errorf("page:\n%s", body)
	}
}

func TestMergePullsChecksAccessAndOrdersPerRepo(t *testing.T) {
	e, user, kiosk := newAuthed(t)
	fp := withPulls(e, &fakePulls{fail: map[int]error{9: pulls.Refused{Message: "Head branch was modified"}}})
	form := url.Values{"csrf": {"csrf-user"}, "action": {"merge"}, "method": {"default"},
		"pr": {key(1, 9, "squash"), key(2, 6, "merge"), key(3, 7, "merge"), key(1, 5, "rebase"), "garbage"}}

	resp, body := postForm(t, e, "/pulls/merge", user, form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	want := []string{"merge acme/api#5 rebase abcdef1234567", "merge acme/api#9 squash abcdef1234567"}
	if strings.Join(fp.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", fp.calls, want)
	}
	for _, w := range []string{"Merged 1 of 5 pull requests.", "acme/web#6", "You need write access to this repository.",
		"Repository not found.", "GitHub refused: Head branch was modified.", "This isn&#39;t a pull request reference."} {
		if !strings.Contains(body, w) {
			t.Errorf("results lack %q:\n%s", w, body)
		}
	}
	if strings.Contains(body, "secret/vault") {
		t.Error("results name a repo the viewer can't see")
	}
	audit, _ := e.s.Store.ListAudit(context.Background(), 10)
	if len(audit) != 2 || audit[0].Action != "merge-pr" {
		t.Errorf("audit = %+v", audit)
	}

	fp.calls = nil
	form.Set("action", "auto")
	form.Set("method", "squash")
	form["pr"] = []string{key(1, 5, "rebase")}
	if resp, _ := postForm(t, e, "/pulls/merge", user, form); resp.StatusCode != http.StatusOK || len(fp.calls) != 1 || fp.calls[0] != "auto acme/api#5 squash abcdef1234567" {
		t.Fatalf("auto: %d %q", resp.StatusCode, fp.calls)
	}

	if resp, _ := postForm(t, e, "/pulls/merge", user, url.Values{"csrf": {"csrf-user"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty selection: %d", resp.StatusCode)
	}
	form.Set("csrf", "wrong")
	if resp, _ := postForm(t, e, "/pulls/merge", user, form); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad csrf: %d", resp.StatusCode)
	}
	form.Set("csrf", "csrf-kiosk")
	if resp, _ := postForm(t, e, "/pulls/merge", kiosk, form); resp.StatusCode != http.StatusForbidden {
		t.Errorf("kiosk: %d", resp.StatusCode)
	}
	if len(fp.calls) != 1 {
		t.Errorf("refused requests reached GitHub: %q", fp.calls)
	}
}

func TestPullErrMessage(t *testing.T) {
	if m := pullErrMessage(fmt.Errorf("x: %w", pulls.ErrPermission)); !strings.Contains(m, "lacks a permission") {
		t.Error(m)
	}
	ge := &github.ErrorResponse{Response: &http.Response{StatusCode: http.StatusMethodNotAllowed}, Message: "Pull Request is not mergeable"}
	if m := pullErrMessage(ge); m != "GitHub refused: Pull Request is not mergeable." {
		t.Error(m)
	}
	if m := pullErrMessage(errors.New("dial tcp: timeout")); !strings.Contains(m, "Couldn't reach GitHub") {
		t.Error(m)
	}
}

func TestPullsExpiredSignIn(t *testing.T) {
	e, user, _ := newAuthed(t)
	fp := withPulls(e, &fakePulls{})
	e.s.ghFor = func(context.Context, *auth.Viewer) (*github.Client, error) { return nil, errors.New("expired") }
	resp, _ := do(t, http.MethodGet, e.srv.URL+"/pulls", user, nil, nil)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=%2Fpulls") {
		t.Errorf("page: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = postForm(t, e, "/pulls/merge", user, url.Values{"csrf": {"csrf-user"}, "action": {"merge"}, "pr": {key(1, 5, "squash")}})
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("HX-Redirect") == "" || len(fp.calls) != 0 {
		t.Errorf("merge: %d %q %q", resp.StatusCode, resp.Header.Get("HX-Redirect"), fp.calls)
	}
}
