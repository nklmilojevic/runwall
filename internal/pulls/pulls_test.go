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
		"commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "SUCCESS"}}}]}
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
		p.DefaultMethod != MethodSquash || strings.Join(p.Methods, ",") != "squash,rebase" || !p.Ready() || p.HeadSHA != "0123456789abcdef" {
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
