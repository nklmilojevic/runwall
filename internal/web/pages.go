package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/metrics"
	"github.com/nklmilojevic/runwall/internal/scoring"
	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/wfgraph"
)

// Sign-in

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if v := s.viewer(w, r); v != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.render(w, r, loginPage(r.URL.Query().Get("next"), r.URL.Query().Get("error"), s.Auth != nil))
}

func (s *Server) authStart(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.Auth.Start(w, r)
}

func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		http.NotFound(w, r)
		return
	}
	dest, err := s.Auth.Callback(w, r)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, auth.ErrNotAllowed) {
			msg = "Your GitHub account doesn't have access to any repository on this dashboard. Ask an admin to add you."
		} else {
			s.Log.Warn("sign-in failed", "err", err)
		}
		http.Redirect(w, r, "/login?error="+url.QueryEscape(msg), http.StatusFound)
		return
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.Auth != nil {
		s.Auth.Logout(w, r)
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) kiosk(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := s.Auth.OpenKiosk(w, r, r.PathValue("token")); err != nil {
		http.Error(w, "This kiosk link is invalid or has been revoked.", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// Dashboard

type dashView struct {
	shell
	Filter   store.Selection
	Options  dashOptions
	Sum      metrics.Summary
	Active   []store.FeedRun
	Recent   []store.FeedRun
	Costs    costInfo
	Threshld time.Duration
}

type dashOptions struct {
	Owners, Repos, Topics, Actors []string
}

type costInfo struct {
	AsOf    string
	Partial bool
}

func (s *Server) summary(ctx context.Context, sc store.Scope, p metrics.Period, now time.Time) (metrics.Summary, error) {
	since := now.Add(-p.Dur)
	runs, err := s.Store.MetricRuns(ctx, sc, since)
	if err != nil {
		return metrics.Summary{}, err
	}
	jobs, err := s.Store.CostJobs(ctx, sc, since)
	if err != nil {
		return metrics.Summary{}, err
	}
	return metrics.Compute(p, now, runs, jobs, s.Costs), nil
}

// dashCookie remembers the last dashboard filter, so a plain visit to / opens it again.
const dashCookie = "dash"

var dashKeys = []string{"org", "repo", "topic", "actor"}

func parseDashFilter(q url.Values) store.Selection {
	clean := func(vs []string) []string {
		vs = slices.DeleteFunc(slices.Clone(vs), func(v string) bool { return v == "" })
		slices.Sort(vs)
		return slices.Compact(vs)
	}
	return store.Selection{Owners: clean(q["org"]), Repos: clean(q["repo"]), Topics: clean(q["topic"]), Actors: clean(q["actor"])}
}

// dashQuery encodes a filter as the dashboard's query string, without the leading "?".
func dashQuery(f store.Selection) string {
	q := url.Values{}
	for i, vs := range [][]string{f.Owners, f.Repos, f.Topics, f.Actors} {
		for _, v := range vs {
			q.Add(dashKeys[i], v)
		}
	}
	return q.Encode()
}

func dashURL(f store.Selection) string {
	if f.Empty() {
		return "/"
	}
	return "/?" + dashQuery(f)
}

// dashFilter reads the filter from the URL, or from the cookie when the URL has none.
// A URL with filter keys, or with f=1 for an explicitly empty filter, replaces the cookie.
func dashFilter(w http.ResponseWriter, r *http.Request) (f store.Selection, fromCookie bool) {
	q := r.URL.Query()
	explicit := q.Has("f") || slices.ContainsFunc(dashKeys, q.Has)
	if !explicit {
		if c, err := r.Cookie(dashCookie); err == nil {
			if raw, err := url.QueryUnescape(c.Value); err == nil {
				if saved, err := url.ParseQuery(raw); err == nil {
					return parseDashFilter(saved), true
				}
			}
		}
		return f, false
	}
	f = parseDashFilter(q)
	want := url.QueryEscape(dashQuery(f))
	if c, err := r.Cookie(dashCookie); err == nil && c.Value == want {
		return f, false
	}
	c := &http.Cookie{Name: dashCookie, Value: want, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if f.Empty() {
		c.Value, c.MaxAge = "", -1
	}
	http.SetCookie(w, c)
	return f, false
}

// dashOptionsFor lists what the viewer can filter by. Selected values stay listed even when no
// longer visible, so they can be cleared.
func (s *Server) dashOptionsFor(ctx context.Context, sc store.Scope, f store.Selection) (dashOptions, error) {
	repos, err := s.Store.VisibleRepos(ctx, sc)
	if err != nil {
		return dashOptions{}, err
	}
	fo, err := s.Store.FilterOptions(ctx, sc)
	if err != nil {
		return dashOptions{}, err
	}
	o := dashOptions{Owners: f.Owners, Repos: f.Repos, Topics: f.Topics, Actors: append(slices.Clone(f.Actors), fo.Actors...)}
	for _, r := range repos {
		if r.Archived {
			continue
		}
		o.Owners = append(o.Owners, r.Owner)
		o.Repos = append(o.Repos, r.FullName)
		o.Topics = append(o.Topics, r.Topics...)
	}
	for _, vs := range []*[]string{&o.Owners, &o.Repos, &o.Topics, &o.Actors} {
		*vs = slices.Clone(*vs)
		slices.SortFunc(*vs, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
		*vs = slices.Compact(*vs)
	}
	return o, nil
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	ctx := r.Context()
	filter, fromCookie := dashFilter(w, r)
	htmx := isHTMX(r) && r.Header.Get("HX-Target") == "dash"
	if fromCookie && !isHTMX(r) && !filter.Empty() {
		http.Redirect(w, r, dashURL(filter), http.StatusFound)
		return
	}
	if !htmx && filter.Empty() && r.URL.Query().Has("f") {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	d := dashView{shell: s.shell(r, v, "dashboard", "Dashboard"), Filter: filter, Threshld: s.StuckThreshold}
	sc := v.Scope
	sc.Only = filter
	var err error
	if d.Options, err = s.dashOptionsFor(ctx, v.Scope, filter); err != nil {
		s.fail(w, err)
		return
	}
	if d.Sum, err = s.summary(ctx, sc, d.Period, d.Now); err != nil {
		s.fail(w, err)
		return
	}
	cutoff := d.Now.Add(-s.StuckThreshold)
	if d.Active, err = s.Store.Feed(ctx, store.FeedFilter{Scope: sc, Status: store.StatusActive, ShowBots: true, Limit: 12}, cutoff); err != nil {
		s.fail(w, err)
		return
	}
	if d.Recent, err = s.Store.Feed(ctx, store.FeedFilter{Scope: sc, ShowBots: true, Limit: 10}, cutoff); err != nil {
		s.fail(w, err)
		return
	}
	d.Costs = costInfo{AsOf: s.Costs.AsOf, Partial: d.Sum.CostsPartial}
	if htmx {
		w.Header().Set("HX-Replace-Url", dashURL(filter))
		s.render(w, r, dashBody(d))
		return
	}
	s.render(w, r, dashboardPage(d))
}

// Runs feed

type pageView struct {
	shell
	Filter        store.FeedFilter
	Query         url.Values
	Feed          []store.FeedRun
	Summary       store.Summary
	Options       store.FilterOptions
	Installations int
	Threshold     time.Duration
	Writable      map[int64]bool
}

func (v pageView) filtered() bool { return len(v.Query) > 0 }

func parseFilter(q url.Values) store.FeedFilter {
	on := func(k string) bool { return q.Get(k) == "1" }
	limit, _ := strconv.Atoi(q.Get("limit"))
	return store.FeedFilter{
		Owner:        q.Get("org"),
		Repo:         q.Get("repo"),
		Branch:       q.Get("branch"),
		Event:        q.Get("event"),
		Actor:        q.Get("actor"),
		Status:       store.StatusFilter(q.Get("status")),
		DefaultOnly:  on("default"),
		ShowArchived: on("archived"),
		ShowForks:    on("forks"),
		ShowBots:     on("bots") || q.Get("actor") != "", // picking a bot means showing it
		Limit:        limit,
	}
}

// cleanQuery drops empty values so pushed URLs stay short.
func cleanQuery(q url.Values) url.Values {
	out := url.Values{}
	for k, vs := range q {
		if len(vs) > 0 && vs[0] != "" && k != "period" {
			out.Set(k, vs[0])
		}
	}
	return out
}

func (s *Server) runsView(ctx context.Context, r *http.Request, v *auth.Viewer) (pageView, error) {
	pv := pageView{shell: s.shell(r, v, "runs", "Runs"), Threshold: s.StuckThreshold}
	pv.Query = cleanQuery(r.URL.Query())
	pv.Filter = parseFilter(pv.Query)
	pv.Filter.Scope = v.Scope
	cutoff := pv.Now.Add(-s.StuckThreshold)
	var err error
	if pv.Feed, err = s.Store.Feed(ctx, pv.Filter, cutoff); err != nil {
		return pv, err
	}
	if pv.Summary, err = s.Store.Summary(ctx, pv.Filter, cutoff); err != nil {
		return pv, err
	}
	if pv.Options, err = s.Store.FilterOptions(ctx, v.Scope); err != nil {
		return pv, err
	}
	insts, err := s.Store.ListInstallations(ctx)
	if err != nil {
		return pv, err
	}
	pv.Installations = len(insts)
	pv.Writable = s.writable(ctx, v, pv.Feed)
	return pv, nil
}

// writable reports, per repo, whether the viewer may use run actions there.
func (s *Server) writable(ctx context.Context, v *auth.Viewer, feed []store.FeedRun) map[int64]bool {
	if v.Demo {
		out := map[int64]bool{}
		for _, r := range feed {
			out[r.RepoID] = true
		}
		return out
	}
	if v.User.ID == 0 || v.Kiosk != nil {
		return nil
	}
	w, err := s.Store.WritableRepos(ctx, v.User.ID)
	if err != nil {
		s.Log.Error("writable repos", "err", err)
	}
	return w
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	pv, err := s.runsView(r.Context(), r, v)
	if err != nil {
		s.fail(w, err)
		return
	}
	if isHTMX(r) && r.Header.Get("HX-Target") == "feed" {
		s.render(w, r, feedFragment(pv))
		return
	}
	s.render(w, r, runsPage(pv))
}

// Run detail

type runView struct {
	shell
	Graph     *wfgraph.Graph
	Run       store.FeedRun
	Repo      store.Repo
	Jobs      []store.Job
	JobsErr   bool
	CanAct    bool
	Threshold time.Duration
}

func (s *Server) runDetail(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	run, ok := s.visibleRun(w, r, v, id)
	if !ok {
		return
	}
	ctx := r.Context()
	repo, err := s.Store.GetRepo(ctx, run.RepoID)
	if err != nil {
		s.fail(w, err)
		return
	}
	rv := runView{shell: s.shell(r, v, "runs", run.WorkflowName+" #"+strconv.Itoa(run.RunNumber)), Repo: repo, Threshold: s.StuckThreshold}
	cutoff := rv.Now.Add(-s.StuckThreshold)
	feed, err := s.Store.Feed(ctx, store.FeedFilter{Scope: v.Scope, RepoID: run.RepoID, WorkflowID: run.WorkflowID,
		ShowArchived: true, ShowForks: true, ShowBots: true, Limit: 500}, cutoff)
	if err != nil {
		s.fail(w, err)
		return
	}
	rv.Run = store.FeedRun{Run: run, Owner: repo.Owner, RepoName: repo.Name, DefaultBranch: repo.DefaultBranch}
	for _, f := range feed {
		if f.ID == run.ID {
			rv.Run = f
		}
	}
	jobs, err := s.Store.ListJobs(ctx, []store.Run{run})
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(jobs[id]) == 0 && !s.Demo {
		if err := s.Syncer.FetchJobsForRun(ctx, id); err != nil {
			s.Log.Error("fetch jobs", "run", id, "err", err)
			rv.JobsErr = true
		} else if jobs, err = s.Store.ListJobs(ctx, []store.Run{run}); err != nil {
			s.fail(w, err)
			return
		}
	}
	rv.Jobs = jobs[id]
	rv.Graph = s.runGraph(ctx, rv)
	rv.CanAct = v.CanAct(ctx, run.RepoID)
	if isHTMX(r) && r.Header.Get("HX-Target") == "run-detail" {
		s.render(w, r, runDetailBody(rv))
		return
	}
	s.render(w, r, runDetailPage(rv))
}

// Workflows

type workflowsView struct {
	shell
	Sum   metrics.Summary
	Costs costInfo
}

func (s *Server) workflows(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	wv := workflowsView{shell: s.shell(r, v, "workflows", "Workflows")}
	var err error
	if wv.Sum, err = s.summary(r.Context(), v.Scope, wv.Period, wv.Now); err != nil {
		s.fail(w, err)
		return
	}
	wv.Costs = costInfo{AsOf: s.Costs.AsOf, Partial: wv.Sum.CostsPartial}
	s.render(w, r, workflowsPage(wv))
}

type workflowView struct {
	shell
	Flow  *metrics.Workflow
	Sum   metrics.Summary
	Runs  []store.FeedRun
	Costs costInfo
}

func (s *Server) workflowDetail(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	repoID, err1 := strconv.ParseInt(r.PathValue("repo"), 10, 64)
	flowID, err2 := strconv.ParseInt(r.PathValue("workflow"), 10, 64)
	ctx := r.Context()
	if err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return
	}
	if ok, _ := s.Store.CanSeeRepo(ctx, v.Scope, repoID); !ok {
		http.NotFound(w, r)
		return
	}
	wv := workflowView{shell: s.shell(r, v, "workflows", "Workflow")}
	since := wv.Now.Add(-wv.Period.Dur)
	runs, err := s.Store.MetricRuns(ctx, v.Scope, since)
	if err != nil {
		s.fail(w, err)
		return
	}
	runs = slices.DeleteFunc(runs, func(m store.MetricRun) bool { return m.RepoID != repoID || m.WorkflowID != flowID })
	jobs, err := s.Store.CostJobs(ctx, v.Scope, since)
	if err != nil {
		s.fail(w, err)
		return
	}
	jobs = slices.DeleteFunc(jobs, func(j store.CostJob) bool { return j.RepoID != repoID || j.WorkflowID != flowID })
	wv.Sum = metrics.Compute(wv.Period, wv.Now, runs, jobs, s.Costs)
	for _, f := range wv.Sum.Workflows {
		wv.Flow = f
	}
	if wv.Runs, err = s.Store.Feed(ctx, store.FeedFilter{Scope: v.Scope, RepoID: repoID, WorkflowID: flowID, ShowBots: true, ShowArchived: true,
		ShowForks: true, Limit: 50}, wv.Now.Add(-s.StuckThreshold)); err != nil {
		s.fail(w, err)
		return
	}
	if wv.Flow == nil && len(wv.Runs) > 0 {
		f := wv.Runs[0]
		wv.Flow = &metrics.Workflow{Key: metrics.WorkflowKey{RepoID: repoID, WorkflowID: flowID}, Name: f.WorkflowName, Path: f.WorkflowPath, Repo: f.Owner + "/" + f.RepoName}
	}
	if wv.Flow == nil {
		http.NotFound(w, r)
		return
	}
	wv.Title = wv.Flow.Name
	wv.Costs = costInfo{AsOf: s.Costs.AsOf, Partial: wv.Sum.CostsPartial}
	s.render(w, r, workflowDetailPage(wv))
}

// Repositories

type repoCard struct {
	Repo  store.Repo
	Stats *metrics.Repo
	Score *store.RepoScore
}

type reposView struct {
	shell
	Cards []repoCard
	Costs costInfo
}

func (s *Server) repos(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	ctx := r.Context()
	rv := reposView{shell: s.shell(r, v, "repos", "Repositories")}
	sum, err := s.summary(ctx, v.Scope, rv.Period, rv.Now)
	if err != nil {
		s.fail(w, err)
		return
	}
	repos, err := s.Store.VisibleRepos(ctx, v.Scope)
	if err != nil {
		s.fail(w, err)
		return
	}
	scores, err := s.Store.ListScores(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, repo := range repos {
		if repo.Archived {
			continue
		}
		c := repoCard{Repo: repo, Stats: sum.Repos[repo.ID]}
		if sc, ok := scores[repo.ID]; ok {
			c.Score = &sc
		}
		rv.Cards = append(rv.Cards, c)
	}
	// Most active first, then by name.
	slices.SortStableFunc(rv.Cards, func(a, b repoCard) int {
		return runsOf(b) - runsOf(a)
	})
	rv.Costs = costInfo{AsOf: s.Costs.AsOf, Partial: sum.CostsPartial}
	s.render(w, r, reposPage(rv))
}

func runsOf(c repoCard) int {
	if c.Stats == nil {
		return 0
	}
	return c.Stats.Counts.Runs
}

type repoView struct {
	shell
	Card      repoCard
	Breakdown scoring.Breakdown
	Runs      []store.FeedRun
	Costs     costInfo
}

func (s *Server) repoDetail(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	id, ok := pathID(r)
	ctx := r.Context()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if visible, _ := s.Store.CanSeeRepo(ctx, v.Scope, id); !visible {
		http.NotFound(w, r)
		return
	}
	repo, err := s.Store.GetRepo(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rv := repoView{shell: s.shell(r, v, "repos", repo.FullName)}
	sum, err := s.summary(ctx, v.Scope, rv.Period, rv.Now)
	if err != nil {
		s.fail(w, err)
		return
	}
	rv.Card = repoCard{Repo: repo, Stats: sum.Repos[id]}
	if sc, err := s.Store.GetScore(ctx, id); err == nil {
		rv.Card.Score = &sc
		rv.Breakdown = scoring.ParseBreakdown(sc.Breakdown)
	}
	if rv.Runs, err = s.Store.Feed(ctx, store.FeedFilter{Scope: v.Scope, RepoID: id, ShowBots: true, ShowArchived: true, ShowForks: true, Limit: 25},
		rv.Now.Add(-s.StuckThreshold)); err != nil {
		s.fail(w, err)
		return
	}
	rv.Costs = costInfo{AsOf: s.Costs.AsOf, Partial: sum.CostsPartial}
	s.render(w, r, repoDetailPage(rv))
}

// Settings

type settingsView struct {
	shell
	Tokens      []store.APIToken
	NewToken    string
	Kiosks      []store.KioskLink
	NewKioskURL string
	Owners      []string
	Audit       []store.AuditEntry
	MCPURL      string
	BaseURL     string
}

func (s *Server) settingsView(ctx context.Context, r *http.Request, v *auth.Viewer) (settingsView, error) {
	sv := settingsView{shell: s.shell(r, v, "settings", "Settings"), MCPURL: s.BaseURL + "/mcp", BaseURL: s.BaseURL}
	var err error
	if sv.Tokens, err = s.Store.ListAPITokens(ctx, v.User.ID); err != nil {
		return sv, err
	}
	if v.Admin {
		if sv.Kiosks, err = s.Store.ListKioskLinks(ctx); err != nil {
			return sv, err
		}
		insts, err := s.Store.ListInstallations(ctx)
		if err != nil {
			return sv, err
		}
		for _, in := range insts {
			sv.Owners = append(sv.Owners, in.Account)
		}
		if sv.Audit, err = s.Store.ListAudit(ctx, 50); err != nil {
			return sv, err
		}
	}
	return sv, nil
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	if v.Kiosk != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	sv, err := s.settingsView(r.Context(), r, v)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, settingsPage(sv))
}
