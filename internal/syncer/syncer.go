// Package syncer backfills and reconciles state from the GitHub API, covering any
// webhooks missed while the app was asleep, offline or unreachable.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/nklmilojevic/runwall/internal/ghapp"
	"github.com/nklmilojevic/runwall/internal/ingest"
	"github.com/nklmilojevic/runwall/internal/store"
)

type Config struct {
	Interval  time.Duration // how often to reconcile
	Backfill  time.Duration // how far back to look for a repo seen for the first time
	Retention time.Duration // how long to keep finished runs
	MaxPages  int           // per repo and pass
	// JobBackfill is how many finished runs per installation and pass get their jobs
	// fetched, so cost estimates cover runs that arrived before jobs were tracked.
	JobBackfill int
	// HotWindow: repos with a push or run this recent are reconciled every pass.
	HotWindow time.Duration
	// ColdInterval: how often quieter repos are reconciled.
	ColdInterval time.Duration
	// Concurrency is how many repos are synced at once (GitHub asks for few concurrent requests).
	Concurrency int
	// Scorer, if set, grades repos after each full sync.
	Scorer Scorer
	// AllowedAccounts, if set, limits which installations are synced. Installations on
	// other accounts (possible once the App is public) are ignored and their data removed.
	AllowedAccounts []string
}

// Allowed reports whether an installation account may be used.
func (c Config) Allowed(account string) bool {
	if len(c.AllowedAccounts) == 0 {
		return true
	}
	for _, a := range c.AllowedAccounts {
		if strings.EqualFold(a, account) {
			return true
		}
	}
	return false
}

// Scorer grades repositories; see the scoring package.
type Scorer interface {
	ScoreDue(ctx context.Context) error
}

type Syncer struct {
	gh    ghapp.Clients
	store *store.Store
	ing   *ingest.Ingester
	cfg   Config
	log   *slog.Logger
	now   func() time.Time

	logClient *http.Client
	trigger   chan int64
	full      chan struct{}

	etagMu sync.Mutex
	etags  [3]map[int64]string

	mu      sync.Mutex
	lastAt  time.Time
	lastErr error
}

// LastSync reports when the last full sync finished and whether it failed.
func (s *Syncer) LastSync() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAt, s.lastErr
}

// Overlap re-lists runs created shortly before the last pass, in case the previous
// pass raced with a run being created.
const overlap = time.Hour

func New(gh ghapp.Clients, st *store.Store, ing *ingest.Ingester, cfg Config, log *slog.Logger) *Syncer {
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 10
	}
	if cfg.JobBackfill == 0 {
		cfg.JobBackfill = 200
	}
	if cfg.HotWindow == 0 {
		cfg.HotWindow = 24 * time.Hour
	}
	if cfg.ColdInterval == 0 {
		cfg.ColdInterval = time.Hour
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	s := &Syncer{gh: gh, store: st, ing: ing, cfg: cfg, log: log, now: time.Now,
		logClient: &http.Client{Timeout: 30 * time.Second}, trigger: make(chan int64, 32), full: make(chan struct{}, 1)}
	for i := range s.etags {
		s.etags[i] = map[int64]string{}
	}
	return s
}

// TriggerInstallation schedules a sync of one installation (e.g. after repos were added to it).
func (s *Syncer) TriggerInstallation(id int64) {
	select {
	case s.trigger <- id:
	default:
	}
}

// TriggerAll schedules a full sync now (the UI's Sync button). Repeated calls while one
// is pending collapse into a single sync.
func (s *Syncer) TriggerAll() {
	select {
	case s.full <- struct{}{}:
	default:
	}
}

// Run does a full sync immediately and then every Interval, plus any triggered installation syncs.
func (s *Syncer) Run(ctx context.Context) {
	s.syncAll(ctx)
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncAll(ctx)
		case <-s.full:
			s.syncAll(ctx)
			t.Reset(s.cfg.Interval)
		case id := <-s.trigger:
			if err := s.SyncInstallation(ctx, id); err != nil {
				s.log.Error("sync installation", "installation", id, "err", err)
			}
		}
	}
}

func (s *Syncer) syncAll(ctx context.Context) {
	start := s.now()
	err := s.SyncAll(ctx)
	s.mu.Lock()
	s.lastAt, s.lastErr = s.now(), err
	s.mu.Unlock()
	if err != nil {
		s.log.Error("sync", "err", err)
		return
	}
	s.log.Info("sync complete", "took", s.now().Sub(start).Round(time.Millisecond))
	if s.cfg.Scorer != nil && s.allHealthy(ctx, lowBudget) {
		if err := s.cfg.Scorer.ScoreDue(ctx); err != nil {
			s.log.Error("score repos", "err", err)
		}
	}
}

func (s *Syncer) allHealthy(ctx context.Context, fraction float64) bool {
	insts, err := s.store.ListInstallations(ctx)
	if err != nil {
		return false
	}
	for _, in := range insts {
		if !s.healthy(in.ID, fraction) {
			return false
		}
	}
	return true
}

// Budget reports the tightest rate-limit budget across installations, for the UI.
func (s *Syncer) Budget(ctx context.Context) (ghapp.Rate, bool) {
	b, ok := s.gh.(ghapp.Budgeter)
	if !ok {
		return ghapp.Rate{}, false
	}
	insts, err := s.store.ListInstallations(ctx)
	if err != nil {
		return ghapp.Rate{}, false
	}
	var worst ghapp.Rate
	found := false
	for _, in := range insts {
		r, ok := b.Budget(in.ID)
		if !ok || r.Limit == 0 {
			continue
		}
		if !found || float64(r.Remaining)/float64(r.Limit) < float64(worst.Remaining)/float64(worst.Limit) {
			worst, found = r, true
		}
	}
	return worst, found
}

// SyncAll refreshes installations, their repos and runs, then prunes old data.
func (s *Syncer) SyncAll(ctx context.Context) error {
	var remote []*github.Installation
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := s.gh.App().Apps.ListInstallations(ctx, opts)
		if err != nil {
			return fmt.Errorf("list installations: %w", err)
		}
		remote = append(remote, page...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	seen := make(map[int64]bool, len(remote))
	for _, inst := range remote {
		seen[inst.GetID()] = true
		if inst.SuspendedAt != nil || !s.cfg.Allowed(inst.GetAccount().GetLogin()) {
			seen[inst.GetID()] = false
			continue
		}
		if err := s.store.UpsertInstallation(ctx, store.Installation{
			ID: inst.GetID(), Account: inst.GetAccount().GetLogin(), AccountType: inst.GetAccount().GetType(),
		}); err != nil {
			return err
		}
	}
	local, err := s.store.ListInstallations(ctx)
	if err != nil {
		return err
	}
	for _, inst := range local {
		if !seen[inst.ID] {
			s.log.Info("installation gone, removing", "installation", inst.ID, "account", inst.Account)
			if err := s.store.DeleteInstallation(ctx, inst.ID); err != nil {
				return err
			}
		}
	}

	var errs []error
	for _, inst := range remote {
		if !seen[inst.GetID()] {
			continue
		}
		if err := s.SyncInstallation(ctx, inst.GetID()); err != nil {
			errs = append(errs, fmt.Errorf("installation %d (%s): %w", inst.GetID(), inst.GetAccount().GetLogin(), err))
		}
	}

	now := s.now()
	if err := s.store.Prune(ctx, now.Add(-s.cfg.Retention), now.Add(-72*time.Hour)); err != nil {
		errs = append(errs, fmt.Errorf("prune: %w", err))
	}
	return errors.Join(errs...)
}

// SyncInstallation refreshes the repo list of one installation and reconciles each repo's runs.
func (s *Syncer) SyncInstallation(ctx context.Context, id int64) error {
	gh := s.gh.Installation(id)
	var remote []*github.Repository
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := gh.Apps.ListRepos(ctx, opts)
		if err != nil {
			return fmt.Errorf("list repos: %w", err)
		}
		remote = append(remote, page.Repositories...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	seen := make(map[int64]bool, len(remote))
	for _, r := range remote {
		seen[r.GetID()] = true
		if err := s.ing.Repo(ctx, ingest.RepoFromGitHub(r, id)); err != nil {
			return err
		}
	}
	local, err := s.store.ListRepos(ctx, id)
	if err != nil {
		return err
	}
	var gone []int64
	for _, r := range local {
		if !seen[r.ID] {
			gone = append(gone, r.ID)
		}
	}
	if err := s.store.DeleteRepos(ctx, gone); err != nil {
		return err
	}

	// Phase 1: bring runs up to date. Busy repos every pass, quiet ones less often.
	activity, err := s.store.RepoActivity(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	var due []store.Repo
	for _, r := range local {
		if seen[r.ID] && !r.Archived && s.due(r, activity[r.ID], now) {
			due = append(due, r)
		}
	}
	if err := forEach(ctx, s, id, due, repoName, func(ctx context.Context, r store.Repo) error { return s.SyncRepo(ctx, gh, r) }); err != nil {
		return err
	}

	// Phase 2: load jobs for older runs (for cost estimates), only while the budget is healthy.
	if !s.healthy(id, lowBudget) {
		s.log.Info("API budget low; skipping job backfill this pass", "installation", id)
		return nil
	}
	return s.backfillJobs(ctx, id, gh, local)
}

// lowBudget is the share of the hourly rate limit kept for live work: below it, job
// backfill and grading pause. criticalBudget stops even reconciling.
const (
	lowBudget      = 0.2
	criticalBudget = 0.03
)

// healthy reports whether an installation has more than fraction of its rate limit left.
func (s *Syncer) healthy(installationID int64, fraction float64) bool {
	b, ok := s.gh.(ghapp.Budgeter)
	if !ok {
		return true
	}
	r, ok := b.Budget(installationID)
	return !ok || r.Healthy(fraction)
}

// due decides whether a repo is reconciled in this pass. Repos with recent pushes or
// runs are checked every pass; the rest once per ColdInterval. Webhooks still deliver
// every update live; this only bounds how stale a missed event can get.
func (s *Syncer) due(r store.Repo, a store.Activity, now time.Time) bool {
	switch {
	case r.LastSeenAt.IsZero(), a.Active > 0:
		return true
	case now.Sub(a.LastRun) < s.cfg.HotWindow, now.Sub(r.PushedAt) < s.cfg.HotWindow:
		return true
	}
	return now.Sub(r.LastSeenAt) >= s.cfg.ColdInterval
}

// errBudget stops a pass when the rate limit is nearly exhausted.
var errBudget = errors.New("GitHub API budget nearly exhausted; pausing until it resets")

func repoName(r store.Repo) string { return r.FullName }

// forEach runs fn over items with bounded concurrency. It stops early when GitHub
// rate-limits us or the budget runs out, and otherwise collects per-item errors.
func forEach[T any](ctx context.Context, s *Syncer, installationID int64, items []T, name func(T) string, fn func(context.Context, T) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		errs  []error
		fatal error
		wg    sync.WaitGroup
		sem   = make(chan struct{}, s.cfg.Concurrency)
	)
	for _, item := range items {
		if !s.healthy(installationID, criticalBudget) {
			mu.Lock()
			fatal = errBudget
			mu.Unlock()
			break
		}
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(item T) {
			defer func() { <-sem; wg.Done() }()
			err := fn(ctx, item)
			if err == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			var rl *github.RateLimitError
			var ab *github.AbuseRateLimitError
			switch {
			case errors.As(err, &rl):
				if fatal == nil {
					fatal = fmt.Errorf("rate limited until %s: %w", rl.Rate.Reset.Format(time.Kitchen), err)
				}
				cancel()
			case errors.As(err, &ab):
				if fatal == nil {
					fatal = fmt.Errorf("secondary rate limit hit; backing off: %w", err)
				}
				cancel()
			case ctx.Err() != nil:
			default:
				errs = append(errs, fmt.Errorf("%s: %w", name(item), err))
			}
		}(item)
	}
	wg.Wait()
	if fatal != nil {
		return fatal
	}
	return errors.Join(errs...)
}

// SyncRepo brings a repo's runs up to date: new or changed runs from the run list,
// plus any run still unfinished locally that the list didn't cover.
func (s *Syncer) SyncRepo(ctx context.Context, gh *github.Client, r store.Repo) error {
	passStart := s.now()
	listed := make(map[int64]bool)

	if r.LastSeenAt.IsZero() {
		// First sight of this repo: backfill a window of history.
		opts := &github.ListWorkflowRunsOptions{
			Created:     ">=" + passStart.Add(-s.cfg.Backfill).UTC().Format("2006-01-02T15:04:05-07:00"),
			ListOptions: github.ListOptions{PerPage: 100},
		}
		for page := 0; page < s.cfg.MaxPages; page++ {
			runs, resp, err := gh.Actions.ListRepositoryWorkflowRuns(ctx, r.Owner, r.Name, opts)
			if err != nil {
				return fmt.Errorf("list runs: %w", err)
			}
			if err := s.ingestRuns(ctx, gh, r, runs.WorkflowRuns, listed); err != nil {
				return err
			}
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	} else if err := s.listRecent(ctx, gh, r, listed); err != nil {
		return err
	}

	// Runs we still think are running but the list didn't include (long runs, or ones
	// whose completion webhook was missed). Recently updated ones are left to webhooks.
	active, err := s.store.ListActiveRuns(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, run := range active {
		if listed[run.ID] || passStart.Sub(run.UpdatedAt) < staleAfter {
			continue
		}
		cond := &ghapp.Cond{ETag: s.etag(etagRun, run.ID)}
		wr, resp, err := gh.Actions.GetWorkflowRunByID(ghapp.Conditional(ctx, cond), r.Owner, r.Name, run.ID)
		switch {
		case resp != nil && resp.StatusCode == http.StatusNotModified:
			continue
		case resp != nil && resp.StatusCode == http.StatusNotFound:
			// Deleted on GitHub; mark it finished so it stops looking active.
			run.Status, run.Conclusion, run.UpdatedAt = "completed", "cancelled", passStart.UTC().Truncate(time.Second)
			if err := s.ing.Run(ctx, run); err != nil {
				return err
			}
			continue
		case err != nil:
			return fmt.Errorf("get run %d: %w", run.ID, err)
		}
		fresh := ingest.RunFromGitHub(wr, r.ID)
		if err := s.ing.Run(ctx, fresh); err != nil {
			return err
		}
		if err := s.FetchJobs(ctx, gh, r, run.ID); err != nil {
			return err
		}
		s.setETag(etagRun, run.ID, cond.NewETag, fresh.Status != "completed")
	}

	return s.store.MarkRepoSeen(ctx, r.ID, passStart)
}

// staleAfter is how long an unfinished run may go without a webhook before it is re-checked.
const staleAfter = 10 * time.Minute

// recentPage is the size of the conditional first page of a repo's run list. Its URL
// never changes, so GitHub answers 304 (free) when nothing in the repo changed.
const recentPage = 50

// listRecent reads a repo's newest runs with a conditional request, and pages further
// back only while the page is entirely newer than the previous pass.
func (s *Syncer) listRecent(ctx context.Context, gh *github.Client, r store.Repo, listed map[int64]bool) error {
	since := r.LastSeenAt.Add(-overlap)
	cond := &ghapp.Cond{ETag: s.etag(etagList, r.ID)}
	opts := &github.ListWorkflowRunsOptions{ListOptions: github.ListOptions{PerPage: recentPage}}
	runs, resp, err := gh.Actions.ListRepositoryWorkflowRuns(ghapp.Conditional(ctx, cond), r.Owner, r.Name, opts)
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	for page := 1; ; page++ {
		if err := s.ingestRuns(ctx, gh, r, runs.WorkflowRuns, listed); err != nil {
			return err
		}
		n := len(runs.WorkflowRuns)
		if n == 0 || resp.NextPage == 0 || page >= s.cfg.MaxPages || runs.WorkflowRuns[n-1].GetCreatedAt().Before(since) {
			break
		}
		opts.Page = resp.NextPage
		if runs, resp, err = gh.Actions.ListRepositoryWorkflowRuns(ctx, r.Owner, r.Name, opts); err != nil {
			return fmt.Errorf("list runs: %w", err)
		}
	}
	// Only remember the ETag once everything it covers is stored.
	s.setETag(etagList, r.ID, cond.NewETag, true)
	return nil
}

func (s *Syncer) ingestRuns(ctx context.Context, gh *github.Client, r store.Repo, runs []*github.WorkflowRun, listed map[int64]bool) error {
	for _, wr := range runs {
		run := ingest.RunFromGitHub(wr, r.ID)
		listed[run.ID] = true
		if err := s.ing.Run(ctx, run); err != nil {
			return err
		}
		if run.Status != "completed" {
			if err := s.FetchJobs(ctx, gh, r, run.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// backfillJobs loads jobs for up to JobBackfill finished runs of an installation,
// newest first, so cost estimates cover runs that arrived before jobs were tracked.
func (s *Syncer) backfillJobs(ctx context.Context, installationID int64, gh *github.Client, repos []store.Repo) error {
	if s.cfg.JobBackfill <= 0 {
		return nil
	}
	missing, err := s.store.RunsMissingJobsFor(ctx, installationID, s.now().Add(-s.cfg.Retention), s.cfg.JobBackfill)
	if err != nil || len(missing) == 0 {
		return err
	}
	byID := make(map[int64]store.Repo, len(repos))
	for _, r := range repos {
		byID[r.ID] = r
	}
	name := func(m store.RunMissingJobs) string { return fmt.Sprintf("%s run %d", byID[m.RepoID].FullName, m.RunID) }
	return forEach(ctx, s, installationID, missing, name, func(ctx context.Context, m store.RunMissingJobs) error {
		r, ok := byID[m.RepoID]
		if !ok || !s.healthy(installationID, lowBudget) {
			return nil
		}
		return s.FetchJobs(ctx, gh, r, m.RunID)
	})
}

// FetchJobs loads the jobs of a run's latest attempt. The first page is requested
// conditionally, so polling an unchanged running job costs nothing.
func (s *Syncer) FetchJobs(ctx context.Context, gh *github.Client, r store.Repo, runID int64) error {
	opts := &github.ListWorkflowJobsOptions{Filter: "latest", ListOptions: github.ListOptions{PerPage: 100}}
	cond := &ghapp.Cond{ETag: s.etag(etagJobs, runID)}
	jobs, resp, err := gh.Actions.ListWorkflowJobs(ghapp.Conditional(ctx, cond), r.Owner, r.Name, runID, opts)
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return nil
	}
	finished := true
	for {
		if err != nil {
			return fmt.Errorf("list jobs for run %d: %w", runID, err)
		}
		for _, j := range jobs.Jobs {
			if j.GetStatus() != "completed" {
				finished = false
			}
			if err := s.ing.Job(ctx, ingest.JobFromGitHub(j)); err != nil {
				return err
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
		jobs, resp, err = gh.Actions.ListWorkflowJobs(ctx, r.Owner, r.Name, runID, opts)
	}
	s.setETag(etagJobs, runID, cond.NewETag, !finished)
	return s.store.MarkJobsFetched(ctx, runID)
}

// ETags of the last successfully processed responses, kept in memory: after a restart
// the first pass simply makes full requests again.
type etagKind int

const (
	etagList etagKind = iota // per repo: first page of the run list
	etagRun                  // per unfinished run
	etagJobs                 // per unfinished run's job list
)

func (s *Syncer) etag(kind etagKind, id int64) string {
	s.etagMu.Lock()
	defer s.etagMu.Unlock()
	return s.etags[kind][id]
}

// setETag stores an ETag, or forgets it when keep is false (finished runs are never polled again).
func (s *Syncer) setETag(kind etagKind, id int64, etag string, keep bool) {
	s.etagMu.Lock()
	defer s.etagMu.Unlock()
	if !keep || etag == "" {
		delete(s.etags[kind], id)
		return
	}
	s.etags[kind][id] = etag
}

// FetchJobsForRun loads jobs on demand, e.g. when a run is expanded in the UI and none are stored yet.
func (s *Syncer) FetchJobsForRun(ctx context.Context, runID int64) error {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	r, err := s.store.GetRepo(ctx, run.RepoID)
	if err != nil {
		return err
	}
	if r.InstallationID == 0 {
		return fmt.Errorf("repo %s has no installation", r.FullName)
	}
	return s.FetchJobs(ctx, s.gh.Installation(r.InstallationID), r, runID)
}

func (s *Syncer) jobRepo(ctx context.Context, jobID int64) (store.Job, store.Repo, error) {
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		return job, store.Repo{}, fmt.Errorf("job %d: %w", jobID, err)
	}
	run, err := s.store.GetRun(ctx, job.RunID)
	if err != nil {
		return job, store.Repo{}, fmt.Errorf("run %d of job %d: %w", job.RunID, jobID, err)
	}
	r, err := s.store.GetRepo(ctx, run.RepoID)
	if err != nil {
		return job, r, err
	}
	if r.InstallationID == 0 {
		return job, r, fmt.Errorf("repo %s has no installation", r.FullName)
	}
	return job, r, nil
}

// RefreshJob re-reads one job (including its steps) from GitHub. Workflow job webhooks
// only fire when a job is queued, starts or finishes, so step progress has to be polled.
func (s *Syncer) RefreshJob(ctx context.Context, jobID int64) error {
	_, r, err := s.jobRepo(ctx, jobID)
	if err != nil {
		return err
	}
	j, _, err := s.gh.Installation(r.InstallationID).Actions.GetWorkflowJobByID(ctx, r.Owner, r.Name, jobID)
	if err != nil {
		return fmt.Errorf("get job %d: %w", jobID, err)
	}
	return s.ing.Job(ctx, ingest.JobFromGitHub(j))
}

const maxLogBytes = 32 << 20

// JobLog downloads a job's plain-text log. GitHub only publishes it once the job has finished.
func (s *Syncer) JobLog(ctx context.Context, jobID int64) (string, error) {
	_, r, err := s.jobRepo(ctx, jobID)
	if err != nil {
		return "", err
	}
	u, resp, err := s.gh.Installation(r.InstallationID).Actions.GetWorkflowJobLogs(ctx, r.Owner, r.Name, jobID, 1)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
			return "", ghapp.ErrLogUnavailable
		}
		return "", fmt.Errorf("log url for job %d: %w", jobID, err)
	}
	// The signed download URL needs no GitHub credentials and expires after a minute.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	dl, err := s.logClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download log for job %d: %w", jobID, err)
	}
	defer dl.Body.Close()
	switch {
	case dl.StatusCode == http.StatusNotFound:
		return "", ghapp.ErrLogUnavailable
	case dl.StatusCode != http.StatusOK:
		return "", fmt.Errorf("download log for job %d: %s", jobID, dl.Status)
	}
	b, err := io.ReadAll(io.LimitReader(dl.Body, maxLogBytes))
	return string(b), err
}

// RefreshRun re-reads one run and its jobs, e.g. right after a re-run or cancel.
func (s *Syncer) RefreshRun(ctx context.Context, runID int64) error {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	r, err := s.store.GetRepo(ctx, run.RepoID)
	if err != nil {
		return err
	}
	gh := s.gh.Installation(r.InstallationID)
	wr, _, err := gh.Actions.GetWorkflowRunByID(ctx, r.Owner, r.Name, runID)
	if err != nil {
		return fmt.Errorf("get run %d: %w", runID, err)
	}
	if err := s.ing.Run(ctx, ingest.RunFromGitHub(wr, r.ID)); err != nil {
		return err
	}
	return s.FetchJobs(ctx, gh, r, runID)
}

type Annotation struct {
	Level   string // notice, warning, failure
	Path    string
	Line    int
	EndLine int
	Title   string
	Message string
}

// Annotations lists a job's check-run annotations (needs Checks: read).
func (s *Syncer) Annotations(ctx context.Context, jobID int64) ([]Annotation, error) {
	job, r, err := s.jobRepo(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.CheckRunID == 0 {
		return nil, nil
	}
	var out []Annotation
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := s.gh.Installation(r.InstallationID).Checks.ListCheckRunAnnotations(ctx, r.Owner, r.Name, job.CheckRunID, opts)
		if err != nil {
			return nil, permissionError(resp, err, "Checks: read")
		}
		for _, a := range page {
			out = append(out, Annotation{Level: a.GetAnnotationLevel(), Path: a.GetPath(), Line: a.GetStartLine(),
				EndLine: a.GetEndLine(), Title: a.GetTitle(), Message: a.GetMessage()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// ErrPermission means the App lacks a permission; the message names it.
type ErrPermission struct{ Permission string }

func (e ErrPermission) Error() string {
	return "the GitHub App needs the " + e.Permission + " permission"
}

func permissionError(resp *github.Response, err error, permission string) error {
	if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
		var ghErr *github.ErrorResponse
		if errors.As(err, &ghErr) && strings.Contains(strings.ToLower(ghErr.Message), "not accessible by integration") {
			return ErrPermission{permission}
		}
		if resp.StatusCode == http.StatusForbidden {
			return ErrPermission{permission}
		}
	}
	return err
}

// WorkflowFile returns the workflow YAML a run executed, at its head commit (needs Contents: read).
func (s *Syncer) WorkflowFile(ctx context.Context, runID int64) (string, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	r, err := s.store.GetRepo(ctx, run.RepoID)
	if err != nil {
		return "", err
	}
	path, _, _ := strings.Cut(run.WorkflowPath, "@")
	if path == "" || strings.HasPrefix(path, "dynamic/") {
		return "", fmt.Errorf("this run has no workflow file (%s)", run.WorkflowPath)
	}
	f, _, resp, err := s.gh.Installation(r.InstallationID).Repositories.GetContents(ctx, r.Owner, r.Name, path,
		&github.RepositoryContentGetOptions{Ref: run.HeadSHA})
	if err != nil {
		return "", permissionError(resp, err, "Contents: read")
	}
	if f == nil {
		return "", fmt.Errorf("%s is not a file", path)
	}
	return f.GetContent()
}
