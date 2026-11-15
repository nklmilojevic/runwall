// Package pulls reads open pull requests from GitHub and merges them, always with the
// signed-in person's own token, so GitHub enforces that person's permissions.
package pulls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/nklmilojevic/runwall/internal/store"
)

// Merge methods, as the REST API names them.
const (
	MethodMerge  = "merge"
	MethodSquash = "squash"
	MethodRebase = "rebase"
)

func ValidMethod(m string) bool { return m == MethodMerge || m == MethodSquash || m == MethodRebase }

// PR is one open pull request and what GitHub says about merging it.
type PR struct {
	RepoID    int64
	Repo      string // full name
	Owner     string
	NodeID    string
	Number    int
	Title     string
	URL       string
	Author    string
	AuthorBot bool
	HeadRef   string
	BaseRef   string
	HeadSHA   string
	Draft     bool
	CreatedAt time.Time
	UpdatedAt time.Time
	Mergeable string // MERGEABLE, CONFLICTING or UNKNOWN
	State     string // mergeStateStatus: CLEAN, BLOCKED, BEHIND, DIRTY, DRAFT, HAS_HOOKS, UNSTABLE or UNKNOWN
	Review    string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, or empty when no review is required
	Checks    string // SUCCESS, FAILURE, ERROR, PENDING, EXPECTED, or empty when the head commit has none
	AutoMerge bool   // auto-merge is already enabled

	DefaultMethod    string   // the repo's preferred merge method for the viewer
	Methods          []string // the merge methods the repo allows
	AutoMergeAllowed bool
}

// Ready means GitHub would merge it now without any further condition.
func (p PR) Ready() bool {
	return !p.Draft && p.Mergeable == "MERGEABLE" && (p.State == "CLEAN" || p.State == "HAS_HOOKS")
}

// Selectable means a merge or auto-merge request for it could succeed.
func (p PR) Selectable() bool {
	return !p.Draft && p.Mergeable != "CONFLICTING" && p.State != "DIRTY"
}

// Target identifies a pull request to act on, as the page saw it.
type Target struct {
	RepoID        int64
	Number        int
	SHA           string // the head commit the viewer saw; GitHub refuses the merge if it moved
	NodeID        string
	DefaultMethod string
}

// Key encodes a target for a form value.
func (p PR) Key() string {
	return strconv.FormatInt(p.RepoID, 10) + ":" + strconv.Itoa(p.Number) + ":" + p.HeadSHA + ":" + p.NodeID + ":" + p.DefaultMethod
}

// ParseTarget reverses PR.Key.
func ParseTarget(s string) (Target, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 5 {
		return Target{}, errors.New("malformed pull request reference")
	}
	repoID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || repoID <= 0 {
		return Target{}, errors.New("malformed repository id")
	}
	num, err := strconv.Atoi(parts[1])
	if err != nil || num <= 0 {
		return Target{}, errors.New("malformed pull request number")
	}
	if !isHex(parts[2]) || len(parts[2]) < 7 {
		return Target{}, errors.New("malformed head commit")
	}
	if !ValidMethod(parts[4]) {
		parts[4] = MethodMerge
	}
	return Target{RepoID: repoID, Number: num, SHA: parts[2], NodeID: parts[3], DefaultMethod: parts[4]}, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// ErrPermission means the App lacks a permission GitHub needs for the request.
var ErrPermission = errors.New("the GitHub App lacks a permission for this request")

// Refused is a GraphQL request that GitHub answered with errors.
type Refused struct{ Message string }

func (r Refused) Error() string { return "github: " + r.Message }

// GitHub talks to the GitHub API. GraphQLURL is the GraphQL endpoint for the API base URL.
type GitHub struct {
	GraphQLURL string
}

// GraphQLURL derives the GraphQL endpoint from a REST API base URL:
// https://api.github.com → https://api.github.com/graphql, and
// https://ghe.example.com/api/v3 → https://ghe.example.com/api/graphql.
func GraphQLURL(apiURL string) string {
	apiURL = strings.TrimRight(apiURL, "/")
	if base, ok := strings.CutSuffix(apiURL, "/api/v3"); ok {
		return base + "/api/graphql"
	}
	return apiURL + "/graphql"
}

// reposPerQuery keeps each GraphQL query well under GitHub's node limits.
const reposPerQuery = 20

// prsPerRepo is how many open pull requests are read per repository, oldest first.
const prsPerRepo = 50

const prFields = `
	id number title url isDraft createdAt updatedAt headRefName baseRefName headRefOid
	mergeable mergeStateStatus reviewDecision
	autoMergeRequest { enabledAt }
	author { __typename login }
	commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }`

var repoFields = `
	viewerDefaultMergeMethod mergeCommitAllowed squashMergeAllowed rebaseMergeAllowed autoMergeAllowed
	pullRequests(states: OPEN, first: ` + strconv.Itoa(prsPerRepo) + `, orderBy: {field: CREATED_AT, direction: ASC}) { nodes {` + prFields + `} }`

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

func (g GitHub) graphql(ctx context.Context, gh *github.Client, query string, vars map[string]any, out any) ([]gqlError, error) {
	req, err := gh.NewRequest(ctx, "POST", g.GraphQLURL, map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}
	var resp gqlResponse
	if _, err := gh.Do(req, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) > 0 && string(resp.Data) != "null" && out != nil {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return resp.Errors, fmt.Errorf("decode GraphQL response: %w", err)
		}
	}
	return resp.Errors, nil
}

func forbidden(errs []gqlError) bool {
	for _, e := range errs {
		if e.Type == "FORBIDDEN" || strings.Contains(e.Message, "not accessible by integration") {
			return true
		}
	}
	return false
}

func errorsText(errs []gqlError) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	return strings.Join(msgs, "; ")
}

type gqlRepo struct {
	ViewerDefaultMergeMethod string `json:"viewerDefaultMergeMethod"`
	MergeCommitAllowed       bool   `json:"mergeCommitAllowed"`
	SquashMergeAllowed       bool   `json:"squashMergeAllowed"`
	RebaseMergeAllowed       bool   `json:"rebaseMergeAllowed"`
	AutoMergeAllowed         bool   `json:"autoMergeAllowed"`
	PullRequests             struct {
		Nodes []*gqlPR `json:"nodes"`
	} `json:"pullRequests"`
}

type gqlPR struct {
	ID               string    `json:"id"`
	Number           int       `json:"number"`
	Title            string    `json:"title"`
	URL              string    `json:"url"`
	IsDraft          bool      `json:"isDraft"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	HeadRefName      string    `json:"headRefName"`
	BaseRefName      string    `json:"baseRefName"`
	HeadRefOid       string    `json:"headRefOid"`
	Mergeable        string    `json:"mergeable"`
	MergeStateStatus string    `json:"mergeStateStatus"`
	ReviewDecision   string    `json:"reviewDecision"`
	AutoMergeRequest *struct {
		EnabledAt time.Time `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	Author *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func (r gqlRepo) methods() []string {
	var out []string
	if r.SquashMergeAllowed {
		out = append(out, MethodSquash)
	}
	if r.MergeCommitAllowed {
		out = append(out, MethodMerge)
	}
	if r.RebaseMergeAllowed {
		out = append(out, MethodRebase)
	}
	return out
}

func convert(repo store.Repo, r gqlRepo, p gqlPR) PR {
	pr := PR{
		RepoID: repo.ID, Repo: repo.FullName, Owner: repo.Owner,
		NodeID: p.ID, Number: p.Number, Title: p.Title, URL: p.URL, Draft: p.IsDraft,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, HeadRef: p.HeadRefName, BaseRef: p.BaseRefName, HeadSHA: p.HeadRefOid,
		Mergeable: p.Mergeable, State: p.MergeStateStatus, Review: p.ReviewDecision, AutoMerge: p.AutoMergeRequest != nil,
		DefaultMethod: strings.ToLower(r.ViewerDefaultMergeMethod), Methods: r.methods(), AutoMergeAllowed: r.AutoMergeAllowed,
	}
	if p.Author != nil {
		pr.Author, pr.AuthorBot = p.Author.Login, p.Author.Typename == "Bot"
	}
	if n := p.Commits.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		pr.Checks = n[0].Commit.StatusCheckRollup.State
	}
	if !ValidMethod(pr.DefaultMethod) {
		pr.DefaultMethod = MethodMerge
		if len(pr.Methods) > 0 {
			pr.DefaultMethod = pr.Methods[0]
		}
	}
	return pr
}

// List reads the open pull requests of the given repos. It returns what it could read
// together with an error when some repos failed; ErrPermission is wrapped when GitHub
// refused because the App lacks a permission.
func (g GitHub) List(ctx context.Context, gh *github.Client, repos []store.Repo) ([]PR, error) {
	var chunks [][]store.Repo
	for i := 0; i < len(repos); i += reposPerQuery {
		chunks = append(chunks, repos[i:min(i+reposPerQuery, len(repos))])
	}
	var (
		mu    sync.Mutex
		out   []PR
		errs  []error
		perms bool
		wg    sync.WaitGroup
		sem   = make(chan struct{}, 4)
	)
	for _, chunk := range chunks {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			prs, gerrs, err := g.listChunk(ctx, gh, chunk)
			mu.Lock()
			defer mu.Unlock()
			out = append(out, prs...)
			if err != nil {
				errs = append(errs, err)
			}
			if len(gerrs) > 0 {
				perms = perms || forbidden(gerrs)
				errs = append(errs, errors.New(errorsText(gerrs)))
			}
		})
	}
	wg.Wait()
	err := errors.Join(errs...)
	if perms {
		err = fmt.Errorf("%w: %w", ErrPermission, err)
	}
	return out, err
}

func (g GitHub) listChunk(ctx context.Context, gh *github.Client, repos []store.Repo) ([]PR, []gqlError, error) {
	var q strings.Builder
	q.WriteString("query(")
	vars := map[string]any{}
	for i, r := range repos {
		if i > 0 {
			q.WriteString(", ")
		}
		fmt.Fprintf(&q, "$o%d: String!, $n%d: String!", i, i)
		vars[fmt.Sprintf("o%d", i)], vars[fmt.Sprintf("n%d", i)] = r.Owner, r.Name
	}
	q.WriteString(") {")
	for i := range repos {
		fmt.Fprintf(&q, "\n r%d: repository(owner: $o%d, name: $n%d) {%s}", i, i, i, repoFields)
	}
	q.WriteString("\n}")

	data := map[string]*gqlRepo{}
	gerrs, err := g.graphql(ctx, gh, q.String(), vars, &data)
	if err != nil {
		return nil, gerrs, err
	}
	var out []PR
	for i, repo := range repos {
		r := data["r"+strconv.Itoa(i)]
		if r == nil {
			continue
		}
		for _, p := range r.PullRequests.Nodes {
			if p != nil {
				out = append(out, convert(repo, *r, *p))
			}
		}
	}
	return out, gerrs, nil
}

// Merge merges a pull request now. GitHub refuses it if the head moved since the viewer saw it.
func (g GitHub) Merge(ctx context.Context, gh *github.Client, repo store.Repo, t Target, method string) error {
	_, _, err := gh.PullRequests.Merge(ctx, repo.Owner, repo.Name, t.Number, "", &github.PullRequestOptions{SHA: t.SHA, MergeMethod: method})
	return err
}

// EnableAutoMerge asks GitHub to merge the pull request once its requirements are met.
func (g GitHub) EnableAutoMerge(ctx context.Context, gh *github.Client, _ store.Repo, t Target, method string) error {
	const m = `mutation($id: ID!, $method: PullRequestMergeMethod!, $sha: GitObjectID) {
		enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method, expectedHeadOid: $sha}) { clientMutationId }
	}`
	gerrs, err := g.graphql(ctx, gh, m, map[string]any{"id": t.NodeID, "method": strings.ToUpper(method), "sha": t.SHA}, nil)
	if err != nil {
		return err
	}
	if len(gerrs) > 0 {
		if forbidden(gerrs) {
			return fmt.Errorf("%w: %s", ErrPermission, errorsText(gerrs))
		}
		return Refused{Message: errorsText(gerrs)}
	}
	return nil
}
