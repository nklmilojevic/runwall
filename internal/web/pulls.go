package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/pulls"
	"github.com/nklmilojevic/runwall/internal/store"
)

// PullSource reads and merges pull requests. gh is the viewer's own client, nil in demo mode.
type PullSource interface {
	List(ctx context.Context, gh *github.Client, repos []store.Repo) ([]pulls.PR, error)
	Merge(ctx context.Context, gh *github.Client, repo store.Repo, t pulls.Target, method string) error
	EnableAutoMerge(ctx context.Context, gh *github.Client, repo store.Repo, t pulls.Target, method string) error
}

// pullCacheTTL keeps filter changes from re-reading GitHub. Refresh and merges bypass it.
const pullCacheTTL = time.Minute

// maxBulk caps one merge request, so a single click can't run for many minutes.
const maxBulk = 100

type pullCache struct {
	mu      sync.Mutex
	entries map[int64]pullEntry
}

type pullEntry struct {
	at  time.Time
	prs []pulls.PR
	err error
}

func (c *pullCache) get(user int64, now time.Time) (pullEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[user]
	return e, ok && now.Sub(e.at) < pullCacheTTL
}

func (c *pullCache) put(user int64, e pullEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[int64]pullEntry{}
	}
	c.entries[user] = e
}

func (c *pullCache) drop(user int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, user)
}

var errSignIn = errors.New("GitHub sign-in expired")

// viewerGitHub returns the viewer's own GitHub client, or nil in demo mode.
func (s *Server) viewerGitHub(ctx context.Context, v *auth.Viewer) (*github.Client, error) {
	switch {
	case s.Demo:
		return nil, nil
	case s.ghFor != nil:
		return s.ghFor(ctx, v)
	}
	return v.GitHub(ctx)
}

// openPulls returns the open pull requests in the repos the viewer can push to.
func (s *Server) openPulls(ctx context.Context, v *auth.Viewer, refresh bool) (pullEntry, error) {
	now := s.Now()
	if e, ok := s.prCache.get(v.User.ID, now); ok && !refresh {
		return e, nil
	}
	repos, err := s.Store.VisibleRepos(ctx, v.Scope)
	if err != nil {
		return pullEntry{}, err
	}
	writable := map[int64]bool{}
	if !v.Demo {
		if writable, err = s.Store.WritableRepos(ctx, v.User.ID); err != nil {
			return pullEntry{}, err
		}
	}
	repos = slices.DeleteFunc(repos, func(r store.Repo) bool { return r.Archived || (!v.Demo && !writable[r.ID]) })

	gh, err := s.viewerGitHub(ctx, v)
	if err != nil {
		return pullEntry{}, errSignIn
	}
	prs, lerr := s.Pulls.List(ctx, gh, repos)
	if lerr != nil {
		s.Log.Warn("list pull requests", "login", v.User.Login, "err", lerr)
	}
	slices.SortFunc(prs, func(a, b pulls.PR) int {
		return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number))
	})
	e := pullEntry{at: now, prs: prs, err: lerr}
	s.prCache.put(v.User.ID, e)
	return e, nil
}

type pullFilter struct {
	Owner, Repo, Author string
	ReadyOnly, Drafts   bool
}

func parsePullFilter(q url.Values) pullFilter {
	return pullFilter{Owner: q.Get("org"), Repo: q.Get("repo"), Author: q.Get("author"), ReadyOnly: q.Get("ready") == "1", Drafts: q.Get("drafts") == "1"}
}

func (f pullFilter) keep(p pulls.PR) bool {
	return (f.Owner == "" || p.Owner == f.Owner) && (f.Repo == "" || p.Repo == f.Repo) && (f.Author == "" || p.Author == f.Author) &&
		(!f.ReadyOnly || p.Ready()) && (f.Drafts || !p.Draft)
}

func (f pullFilter) active() bool { return f != pullFilter{} }

type pullsView struct {
	shell
	Filter  pullFilter
	Owners  []string
	Repos   []string
	Authors []string
	Bots    map[string]bool
	PRs     []pulls.PR
	Total   int
	At      time.Time
	Note    string
}

func pullNote(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, pulls.ErrPermission):
		return "GitHub refused some requests because the GitHub App lacks a permission. Give the App \"Pull requests: read\" and \"Contents: read and write\", then accept the new permissions on each installation."
	}
	return "Couldn't read the pull requests of some repositories. The list can be incomplete. Check the server log for details."
}

func (s *Server) pullRequests(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	if v.Kiosk != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	e, err := s.openPulls(ctx, v, r.URL.Query().Get("refresh") == "1")
	if errors.Is(err, errSignIn) {
		login := "/login?next=" + url.QueryEscape("/pulls")
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", login)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, login, http.StatusFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	pv := pullsView{shell: s.shell(r, v, "pulls", "Pull requests"), Filter: parsePullFilter(r.URL.Query()),
		Total: len(e.prs), At: e.at, Note: pullNote(e.err), Bots: map[string]bool{}}
	for _, p := range e.prs {
		pv.Owners = append(pv.Owners, p.Owner)
		pv.Repos = append(pv.Repos, p.Repo)
		pv.Authors = append(pv.Authors, p.Author)
		pv.Bots[p.Author] = p.AuthorBot
		if pv.Filter.keep(p) {
			pv.PRs = append(pv.PRs, p)
		}
	}
	for _, l := range []*[]string{&pv.Owners, &pv.Repos, &pv.Authors} {
		slices.Sort(*l)
		*l = slices.Compact(*l)
	}
	if isHTMX(r) && r.Header.Get("HX-Target") == "pulls" {
		s.render(w, r, pullRows(pv))
		return
	}
	s.render(w, r, pullsPage(pv))
}

type mergeResult struct {
	Label string // owner/repo#number
	Title string
	URL   string
	OK    bool
	Msg   string
}

type mergeJob struct {
	idx    int
	repo   store.Repo
	target pulls.Target
	method string
}

func pullErrMessage(err error) string {
	var refused pulls.Refused
	var ge *github.ErrorResponse
	switch {
	case errors.Is(err, pulls.ErrPermission):
		return "The GitHub App lacks a permission for this. Check the App permissions in the README."
	case errors.As(err, &refused):
		return "GitHub refused: " + refused.Message + "."
	case errors.As(err, &ge) && ge.Response.StatusCode == http.StatusMethodNotAllowed:
		return "GitHub refused: " + ge.Message + "."
	}
	return githubMessage(err)
}

// mergePulls merges (or enables auto-merge on) the selected pull requests. Pull requests in
// the same repository merge one after another, because each merge moves the base branch.
func (s *Server) mergePulls(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Couldn't read the form.", http.StatusBadRequest)
		return
	}
	auto := r.PostForm.Get("action") == "auto"
	method := r.PostForm.Get("method")
	keys := slices.Compact(slices.Sorted(slices.Values(r.PostForm["pr"])))
	switch {
	case len(keys) == 0:
		toast(w, "error", "Select at least one pull request.")
		w.WriteHeader(http.StatusBadRequest)
		return
	case len(keys) > maxBulk:
		toast(w, "error", "Select at most "+strconv.Itoa(maxBulk)+" pull requests at a time.")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	known := map[string]pulls.PR{}
	if e, ok := s.prCache.get(v.User.ID, s.Now()); ok {
		for _, p := range e.prs {
			known[strconv.FormatInt(p.RepoID, 10)+"#"+strconv.Itoa(p.Number)] = p
		}
	}
	results := make([]mergeResult, len(keys))
	byRepo := map[int64][]mergeJob{}
	for i, k := range keys {
		t, err := pulls.ParseTarget(k)
		if err != nil {
			results[i] = mergeResult{Label: k, Msg: "This isn't a pull request reference."}
			continue
		}
		res := &results[i]
		res.Label = "#" + strconv.Itoa(t.Number)
		repo, err := s.Store.GetRepo(ctx, t.RepoID)
		visible, _ := s.Store.CanSeeRepo(ctx, v.Scope, t.RepoID)
		if err != nil || !visible {
			res.Msg = "Repository not found."
			continue
		}
		res.Label = repo.FullName + res.Label
		if p, ok := known[strconv.FormatInt(t.RepoID, 10)+"#"+strconv.Itoa(t.Number)]; ok {
			res.Title, res.URL = p.Title, p.URL
		}
		if !v.CanAct(ctx, t.RepoID) {
			res.Msg = "You need write access to this repository."
			continue
		}
		m := method
		if !pulls.ValidMethod(m) {
			m = t.DefaultMethod
		}
		byRepo[repo.ID] = append(byRepo[repo.ID], mergeJob{idx: i, repo: repo, target: t, method: m})
	}

	var gh *github.Client
	if len(byRepo) > 0 {
		var err error
		if gh, err = s.viewerGitHub(ctx, v); err != nil {
			w.Header().Set("HX-Redirect", "/login?next="+url.QueryEscape("/pulls"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	// Finish what was started even if the browser goes away.
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	action := "merge-pr"
	if auto {
		action = "auto-merge-pr"
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, jobs := range byRepo {
		slices.SortFunc(jobs, func(a, b mergeJob) int { return cmp.Compare(a.target.Number, b.target.Number) })
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			for _, j := range jobs {
				var err error
				if auto {
					err = s.Pulls.EnableAutoMerge(work, gh, j.repo, j.target, j.method)
				} else {
					err = s.Pulls.Merge(work, gh, j.repo, j.target, j.method)
				}
				s.audit(work, v, action, j.repo.FullName, int64(j.target.Number), err)
				res := &results[j.idx]
				res.OK = err == nil
				if err != nil {
					res.Msg = pullErrMessage(err)
				}
			}
		})
	}
	wg.Wait()
	s.prCache.drop(v.User.ID)

	ok := 0
	for _, res := range results {
		if res.OK {
			ok++
		}
	}
	verb := "Merged"
	if auto {
		verb = "Enabled auto-merge on"
	}
	summary := verb + " " + strconv.Itoa(ok) + " of " + plural(len(results), "pull request", "pull requests") + "."
	if s.Demo {
		summary += " (demo mode: nothing was sent to GitHub)"
	}
	kind := "ok"
	if ok < len(results) {
		kind = "error"
	}
	b, _ := json.Marshal(map[string]any{"toast": map[string]string{"kind": kind, "message": summary}, "pulls-changed": true})
	w.Header().Set("HX-Trigger", string(b))
	s.render(w, r, mergeResults(summary, results))
}
