package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v93/github"

	"github.com/nklmilojevic/runwall/internal/store"
)

func client(t *testing.T, mux *http.ServeMux) (*github.Client, GitHub) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	gh, err := github.NewClient(github.WithURLs(&base, nil))
	if err != nil {
		t.Fatal(err)
	}
	return gh, GitHub{GraphQLURL: GraphQLURL(srv.URL)}
}

func TestGraphQLURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.github.com":          "https://api.github.com/graphql",
		"https://api.github.com/":         "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3":  "https://ghe.example.com/api/graphql",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com/api/graphql",
	} {
		if got := GraphQLURL(in); got != want {
			t.Errorf("GraphQLURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTargetRoundTrip(t *testing.T) {
	pr := PR{RepoID: 12, Number: 34, HeadSHA: "abc1234def", NodeID: "PR_kwDOAbc", DefaultMethod: MethodSquash}
	got, err := ParseTarget(pr.Key())
	if err != nil {
		t.Fatal(err)
	}
	want := Target{RepoID: 12, Number: 34, SHA: "abc1234def", NodeID: "PR_kwDOAbc", DefaultMethod: MethodSquash}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, bad := range []string{"", "1:2:abc1234", "x:2:abc1234:PR:merge", "1:0:abc1234:PR:merge", "1:2:nothex!:PR:merge", "1:2:ab:PR:merge"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) accepted", bad)
		}
	}
	if tg, _ := ParseTarget("1:2:abc1234:PR:octopus"); tg.DefaultMethod != MethodMerge {
		t.Errorf("unknown method kept: %q", tg.DefaultMethod)
	}
}

func TestReadyAndSelectable(t *testing.T) {
	cases := []struct {
		pr                PR
		ready, selectable bool
	}{
		{PR{Mergeable: "MERGEABLE", State: "CLEAN"}, true, true},
		{PR{Mergeable: "MERGEABLE", State: "HAS_HOOKS"}, true, true},
		{PR{Mergeable: "MERGEABLE", State: "BLOCKED"}, false, true},
		{PR{Mergeable: "UNKNOWN", State: "UNKNOWN"}, false, true},
		{PR{Mergeable: "CONFLICTING", State: "DIRTY"}, false, false},
		{PR{Mergeable: "MERGEABLE", State: "CLEAN", Draft: true}, false, false},
	}
	for _, c := range cases {
		if c.pr.Ready() != c.ready || c.pr.Selectable() != c.selectable {
			t.Errorf("%+v: ready=%v selectable=%v", c.pr, c.pr.Ready(), c.pr.Selectable())
		}
	}
}

const repoJSON = `{
	"viewerDefaultMergeMethod": "SQUASH", "mergeCommitAllowed": false, "squashMergeAllowed": true, "rebaseMergeAllowed": true, "autoMergeAllowed": true,
	"pullRequests": {"nodes": [{
		"id": "PR_1", "number": 7, "title": "Update module x", "url": "https://github.com/acme/api/pull/7", "isDraft": false,
		"createdAt": "2026-11-01T10:00:00Z", "updatedAt": "2026-11-02T10:00:00Z",
		"headRefName": "renovate/x", "baseRefName": "main", "headRefOid": "0123456789abcdef",
		"mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": null, "autoMergeRequest": null,
		"author": {"__typename": "Bot", "login": "renovate"},
		"commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "SUCCESS"}}}]},
		"comments": {"nodes": [
			{"author": {"login": "renovate"}, "body": "Confidence Score: 1/5", "url": "u1"},
			{"author": {"login": "kritikal-github"}, "body": "<!-- kritika:pr-7 -->\n### Confidence Score: 5/5\n<sub>Last reviewed commit: [fix](https://github.com/acme/api/commit/0123456789abcdef0123456789abcdef01234567)</sub>", "url": "https://github.com/acme/api/pull/7#issuecomment-1"}
		]}
	}]}
}`

func TestListReadsReposAndKeepsPartialResults(t *testing.T) {
	var mu sync.Mutex
	var queries []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		queries = append(queries, body.Variables)
		mu.Unlock()
		// r0 is readable; r1 is refused, as when the App lacks Pull requests: read on it.
		io.WriteString(w, `{"data": {"r0": `+repoJSON+`, "r1": null},
			"errors": [{"type": "FORBIDDEN", "path": ["r1"], "message": "Resource not accessible by integration"}]}`)
	})
	gh, g := client(t, mux)
	repos := []store.Repo{{ID: 1, Owner: "acme", Name: "api", FullName: "acme/api"}, {ID: 2, Owner: "acme", Name: "web", FullName: "acme/web"}}

	prs, err := g.List(context.Background(), gh, repos)
	if !errors.Is(err, ErrPermission) {
		t.Fatalf("err = %v, want ErrPermission", err)
	}
	if len(queries) != 1 || queries[0]["o1"] != "acme" || queries[0]["n1"] != "web" {
		t.Fatalf("queries = %v", queries)
	}
	if len(prs) != 1 {
		t.Fatalf("prs = %+v", prs)
	}
	p := prs[0]
	if p.RepoID != 1 || p.Repo != "acme/api" || p.Number != 7 || !p.AuthorBot || p.Author != "renovate" || p.Checks != "SUCCESS" ||
		p.DefaultMethod != MethodSquash || strings.Join(p.Methods, ",") != "squash,rebase" || !p.Ready() || p.HeadSHA != "0123456789abcdef" ||
		p.Kritika == nil || p.Kritika.Score != 5 || p.Kritika.URL != "https://github.com/acme/api/pull/7#issuecomment-1" || p.KritikaCurrent() {
		t.Fatalf("pr = %+v", p)
	}
}

func TestListSplitsLargeRepoSets(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		io.WriteString(w, `{"data": {}}`)
	})
	gh, g := client(t, mux)
	repos := make([]store.Repo, reposPerQuery*2+1)
	for i := range repos {
		repos[i] = store.Repo{ID: int64(i + 1), Owner: "acme", Name: "r"}
	}
	if _, err := g.List(context.Background(), gh, repos); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestMergeSendsHeadAndMethod(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repos/acme/api/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"merged": true}`)
	})
	gh, g := client(t, mux)
	err := g.Merge(context.Background(), gh, store.Repo{Owner: "acme", Name: "api"}, Target{Number: 7, SHA: "0123456789abcdef"}, MethodSquash)
	if err != nil {
		t.Fatal(err)
	}
	if got["sha"] != "0123456789abcdef" || got["merge_method"] != "squash" {
		t.Fatalf("body = %v", got)
	}
}

func TestEnableAutoMerge(t *testing.T) {
	var vars map[string]any
	answer := `{"data": {"enablePullRequestAutoMerge": {"clientMutationId": null}}}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		vars = body.Variables
		io.WriteString(w, answer)
	})
	gh, g := client(t, mux)
	t1 := Target{Number: 7, SHA: "0123456789abcdef", NodeID: "PR_1"}
	if err := g.EnableAutoMerge(context.Background(), gh, store.Repo{}, t1, MethodRebase); err != nil {
		t.Fatal(err)
	}
	if vars["id"] != "PR_1" || vars["method"] != "REBASE" || vars["sha"] != "0123456789abcdef" {
		t.Fatalf("vars = %v", vars)
	}

	answer = `{"data": null, "errors": [{"type": "UNPROCESSABLE", "message": "Auto merge is not allowed for this repository"}]}`
	err := g.EnableAutoMerge(context.Background(), gh, store.Repo{}, t1, MethodRebase)
	if err == nil || !strings.Contains(err.Error(), "Auto merge is not allowed") || errors.Is(err, ErrPermission) {
		t.Fatalf("err = %v", err)
	}

	answer = `{"data": null, "errors": [{"type": "FORBIDDEN", "message": "Resource not accessible by integration"}]}`
	if err := g.EnableAutoMerge(context.Background(), gh, store.Repo{}, t1, MethodRebase); !errors.Is(err, ErrPermission) {
		t.Fatalf("err = %v, want ErrPermission", err)
	}
}

func TestParseKritika(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	body := "<!-- kritika:pr-3 -->\nConfidence Score: 3/5\n...\nConfidence Score: 4/5\n" +
		"Last reviewed commit: [old](https://github.com/a/b/commit/" + strings.Repeat("9", 40) + ")\n" +
		"Last reviewed commit: [new](https://ghe.example.com/a/b/commit/" + head + ")"
	k := parseKritika("kritikal-github", body, "u")
	if k == nil || k.Score != 4 || k.ReviewedSHA != head {
		t.Fatalf("got %+v", k)
	}
	if parseKritika("someone", body, "u") != nil {
		t.Error("accepted a comment from another author")
	}
	if parseKritika("kritikal-github", "Confidence Score: 5/5", "u") != nil {
		t.Error("accepted a kritika comment without the summary marker")
	}
	if parseKritika("kritikal-github", "<!-- kritika:pr-3 --> Reviewing…", "u") != nil {
		t.Error("accepted a summary without a score")
	}
	pr := PR{HeadSHA: head, Kritika: &Kritika{Score: 5, ReviewedSHA: head}}
	if !pr.KritikaPass() {
		t.Error("5/5 on head should pass")
	}
	pr.HeadSHA = strings.Repeat("1", 40)
	if pr.KritikaPass() || pr.KritikaCurrent() {
		t.Error("a review of an older commit should not pass")
	}
}

func TestDetail(t *testing.T) {
	answer := `{"data": {"repository": {
		"viewerDefaultMergeMethod": "MERGE", "mergeCommitAllowed": true, "squashMergeAllowed": false, "rebaseMergeAllowed": false, "autoMergeAllowed": false,
		"pullRequest": {
			"id": "PR_9", "number": 9, "title": "Add thing", "url": "https://github.com/acme/api/pull/9", "isDraft": false,
			"createdAt": "2026-11-01T10:00:00Z", "updatedAt": "2026-11-02T10:00:00Z",
			"headRefName": "feat/x", "baseRefName": "main", "headRefOid": "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
			"mergeable": "MERGEABLE", "mergeStateStatus": "BLOCKED", "reviewDecision": "REVIEW_REQUIRED", "autoMergeRequest": null,
			"author": {"__typename": "User", "login": "monalisa"},
			"commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "PENDING"}}}]},
			"comments": {"nodes": []},
			"state": "OPEN", "additions": 10, "deletions": 2, "changedFiles": 3,
			"reviewThreads": {"nodes": [
				{"isResolved": false, "isOutdated": false, "path": "a.go", "line": 4, "comments": {"nodes": [{"author": {"login": "kritikal-github"}, "url": "t1", "bodyText": "Handle   the\nerror."}]}},
				{"isResolved": true, "isOutdated": false, "path": "b.go", "line": 1, "comments": {"nodes": [{"author": {"login": "kritikal-github"}, "url": "t2", "bodyText": "fixed"}]}},
				{"isResolved": false, "isOutdated": true, "path": "c.go", "line": null, "comments": {"nodes": [{"author": {"login": "octocat"}, "url": "t3", "bodyText": "human"}]}}
			]},
			"checks": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [
				{"__typename": "CheckRun", "name": "test", "status": "COMPLETED", "conclusion": "FAILURE", "detailsUrl": "https://ci/test", "url": "https://github.com/x", "checkSuite": {"workflowRun": {"databaseId": 555}}},
				{"__typename": "CheckRun", "name": "external", "status": "IN_PROGRESS", "conclusion": null, "detailsUrl": null, "url": "https://github.com/y", "checkSuite": {"workflowRun": null}},
				{"__typename": "StatusContext", "context": "ci/jenkins", "state": "PENDING", "targetUrl": "https://jenkins", "description": "Build running"}
			]}}}}]}
		}
	}}}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, answer) })
	gh, g := client(t, mux)
	repo := store.Repo{ID: 1, Owner: "acme", Name: "api", FullName: "acme/api"}

	d, err := g.Detail(context.Background(), gh, repo, 9)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "OPEN" || d.Additions != 10 || d.ChangedFiles != 3 || d.State != "BLOCKED" || d.DefaultMethod != MethodMerge {
		t.Fatalf("detail = %+v", d)
	}
	wantChecks := []Check{
		{Name: "test", Status: "completed", Conclusion: "failure", URL: "https://ci/test", RunID: 555},
		{Name: "external", Status: "in_progress", URL: "https://github.com/y"},
		{Name: "ci/jenkins", Status: "in_progress", Description: "Build running", URL: "https://jenkins"},
	}
	if len(d.CheckRuns) != len(wantChecks) {
		t.Fatalf("checks = %+v", d.CheckRuns)
	}
	for i, c := range wantChecks {
		if d.CheckRuns[i] != c {
			t.Errorf("check %d = %+v, want %+v", i, d.CheckRuns[i], c)
		}
	}
	if len(d.Findings) != 1 || d.Findings[0] != (Finding{Path: "a.go", Line: 4, URL: "t1", Text: "Handle the error."}) {
		t.Errorf("findings = %+v", d.Findings)
	}

	answer = `{"data": {"repository": {"pullRequest": null}}, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a PullRequest"}]}`
	if _, err := g.Detail(context.Background(), gh, repo, 9); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	answer = `{"data": {"repository": null}, "errors": [{"type": "FORBIDDEN", "message": "Resource not accessible by integration"}]}`
	if _, err := g.Detail(context.Background(), gh, repo, 9); !errors.Is(err, ErrPermission) {
		t.Errorf("err = %v, want ErrPermission", err)
	}
}
