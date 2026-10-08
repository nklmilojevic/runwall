package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = ":memory:"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes and keeps :memory: databases coherent.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds columns introduced after a database was first created.
func migrate(db *sql.DB) error {
	added := []struct{ table, column, ddl string }{
		{"jobs", "steps", `ALTER TABLE jobs ADD COLUMN steps TEXT NOT NULL DEFAULT ''`},
		{"jobs", "check_run_id", `ALTER TABLE jobs ADD COLUMN check_run_id INTEGER NOT NULL DEFAULT 0`},
		{"repos", "private", `ALTER TABLE repos ADD COLUMN private INTEGER NOT NULL DEFAULT 0`},
		{"repos", "pushed_at", `ALTER TABLE repos ADD COLUMN pushed_at INTEGER NOT NULL DEFAULT 0`},
		{"repos", "open_issues", `ALTER TABLE repos ADD COLUMN open_issues INTEGER NOT NULL DEFAULT 0`},
		{"repos", "description", `ALTER TABLE repos ADD COLUMN description TEXT NOT NULL DEFAULT ''`},
		{"runs", "jobs_fetched", `ALTER TABLE runs ADD COLUMN jobs_fetched INTEGER NOT NULL DEFAULT 0`},
	}
	for _, a := range added {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, a.table, a.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(a.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

type Installation struct {
	ID          int64
	Account     string
	AccountType string
}

type Repo struct {
	ID             int64
	InstallationID int64
	Owner          string
	Name           string
	FullName       string
	DefaultBranch  string
	Archived       bool
	Fork           bool
	Private        bool
	Description    string
	OpenIssues     int
	PushedAt       time.Time
	LastSeenAt     time.Time
}

type Run struct {
	ID            int64
	RepoID        int64
	WorkflowID    int64
	WorkflowName  string
	WorkflowPath  string
	RunNumber     int
	RunAttempt    int
	Event         string
	HeadBranch    string
	HeadSHA       string
	Title         string
	CommitMessage string
	ActorLogin    string
	ActorIsBot    bool
	Status        string
	Conclusion    string
	PRNumber      int
	CreatedAt     time.Time
	RunStartedAt  time.Time
	UpdatedAt     time.Time
	HTMLURL       string
}

// QueuedSince is when the current attempt entered the queue.
func (r Run) QueuedSince() time.Time {
	if r.RunStartedAt.After(r.CreatedAt) {
		return r.RunStartedAt
	}
	return r.CreatedAt
}

type Job struct {
	ID          int64
	RunID       int64
	RunAttempt  int
	Name        string
	Status      string
	Conclusion  string
	Labels      []string
	RunnerName  string
	CreatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
	HTMLURL     string
	Steps       []Step
	CheckRunID  int64
}

type Step struct {
	Number      int       `json:"number"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion,omitempty"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

func encodeSteps(steps []Step) string {
	if len(steps) == 0 {
		return ""
	}
	b, _ := json.Marshal(steps)
	return string(b)
}

// IsQueued reports whether a run or job status means it is waiting for a runner.
func IsQueued(status string) bool {
	switch status {
	case "queued", "requested", "pending":
		return true
	}
	return false
}

func statusRank(status string) int {
	switch status {
	case "completed":
		return 3
	case "in_progress":
		return 2
	case "":
		return 0
	}
	return 1
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Installations

func (s *Store) UpsertInstallation(ctx context.Context, in Installation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO installations (id, account, account_type) VALUES (?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET account = excluded.account, account_type = excluded.account_type`,
		in.ID, in.Account, in.AccountType)
	return err
}

func (s *Store) ListInstallations(ctx context.Context) ([]Installation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account, account_type FROM installations ORDER BY account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		var in Installation
		if err := rows.Scan(&in.ID, &in.Account, &in.AccountType); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// DeleteInstallation removes an installation together with its repos, runs and jobs.
func (s *Store) DeleteInstallation(ctx context.Context, id int64) error {
	repos, err := s.ListRepos(ctx, id)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.ID)
	}
	if err := s.DeleteRepos(ctx, ids); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM installations WHERE id = ?`, id)
	return err
}

// Repos

const repoColumns = `id, installation_id, owner, name, full_name, default_branch, archived, fork, last_seen_at,
	private, description, open_issues, pushed_at`

func scanRepo(sc interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	var archived, fork, private int
	var seen, pushed int64
	err := sc.Scan(&r.ID, &r.InstallationID, &r.Owner, &r.Name, &r.FullName, &r.DefaultBranch, &archived, &fork, &seen,
		&private, &r.Description, &r.OpenIssues, &pushed)
	r.Archived, r.Fork, r.Private, r.LastSeenAt, r.PushedAt = archived == 1, fork == 1, private == 1, fromUnix(seen), fromUnix(pushed)
	return r, err
}

// UpsertRepo stores repository metadata. LastSeenAt is never touched here; see MarkRepoSeen.
// An empty DefaultBranch keeps the stored one, because some webhook payloads carry partial repos.
func (s *Store) UpsertRepo(ctx context.Context, r Repo) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repos (id, installation_id, owner, name, full_name, default_branch, archived, fork,
			private, description, open_issues, pushed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			private = excluded.private,
			description = excluded.description,
			open_issues = excluded.open_issues,
			pushed_at = max(repos.pushed_at, excluded.pushed_at),
			installation_id = CASE WHEN excluded.installation_id != 0 THEN excluded.installation_id ELSE repos.installation_id END,
			owner = excluded.owner,
			name = excluded.name,
			full_name = excluded.full_name,
			default_branch = CASE WHEN excluded.default_branch != '' THEN excluded.default_branch ELSE repos.default_branch END,
			archived = excluded.archived,
			fork = excluded.fork`,
		r.ID, r.InstallationID, r.Owner, r.Name, r.FullName, r.DefaultBranch, boolInt(r.Archived), boolInt(r.Fork),
		boolInt(r.Private), r.Description, r.OpenIssues, unix(r.PushedAt))
	return err
}

func (s *Store) MarkRepoSeen(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET last_seen_at = ? WHERE id = ?`, unix(at), id)
	return err
}

func (s *Store) GetRepo(ctx context.Context, id int64) (Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE id = ?`, id))
}

// ListRepos returns repos for one installation, or all repos when installationID is 0.
func (s *Store) ListRepos(ctx context.Context, installationID int64) ([]Repo, error) {
	q := `SELECT ` + repoColumns + ` FROM repos`
	var args []any
	if installationID != 0 {
		q += ` WHERE installation_id = ?`
		args = append(args, installationID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY full_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRepos(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE run_id IN (SELECT id FROM runs WHERE repo_id = ?)`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE repo_id = ?`, id); err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM user_repos WHERE repo_id = ?`, `DELETE FROM repo_scores WHERE repo_id = ?`, `DELETE FROM repos WHERE id = ?`} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Runs

const runColumns = `r.id, r.repo_id, r.workflow_id, r.workflow_name, r.workflow_path, r.run_number, r.run_attempt,
	r.event, r.head_branch, r.head_sha, r.title, r.commit_message, r.actor_login, r.actor_is_bot,
	r.status, r.conclusion, r.pr_number, r.created_at, r.run_started_at, r.updated_at, r.html_url`

func runDest(r *Run, bot *int, created, started, updated *int64) []any {
	return []any{&r.ID, &r.RepoID, &r.WorkflowID, &r.WorkflowName, &r.WorkflowPath, &r.RunNumber, &r.RunAttempt,
		&r.Event, &r.HeadBranch, &r.HeadSHA, &r.Title, &r.CommitMessage, &r.ActorLogin, bot,
		&r.Status, &r.Conclusion, &r.PRNumber, created, started, updated, &r.HTMLURL}
}

func scanRun(sc interface{ Scan(...any) error }, extra ...any) (Run, error) {
	var r Run
	var bot int
	var created, started, updated int64
	err := sc.Scan(append(runDest(&r, &bot, &created, &started, &updated), extra...)...)
	r.ActorIsBot = bot == 1
	r.CreatedAt, r.RunStartedAt, r.UpdatedAt = fromUnix(created), fromUnix(started), fromUnix(updated)
	return r, err
}

func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs r WHERE r.id = ?`, id))
}

// UpsertRun stores a run unless the stored copy is newer. It returns the previous
// copy (nil if the run was unknown) and whether anything was written.
func (s *Store) UpsertRun(ctx context.Context, r Run) (prev *Run, changed bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	old, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs r WHERE r.id = ?`, r.ID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, false, err
	default:
		prev = &old
		if !runIsNewer(r, old) {
			return prev, false, nil
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO runs (id, repo_id, workflow_id, workflow_name, workflow_path, run_number, run_attempt,
			event, head_branch, head_sha, title, commit_message, actor_login, actor_is_bot,
			status, conclusion, pr_number, created_at, run_started_at, updated_at, html_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RepoID, r.WorkflowID, r.WorkflowName, r.WorkflowPath, r.RunNumber, r.RunAttempt,
		r.Event, r.HeadBranch, r.HeadSHA, r.Title, r.CommitMessage, r.ActorLogin, boolInt(r.ActorIsBot),
		r.Status, r.Conclusion, r.PRNumber, unix(r.CreatedAt), unix(r.RunStartedAt), unix(r.UpdatedAt), r.HTMLURL)
	if err != nil {
		return prev, false, err
	}
	return prev, true, tx.Commit()
}

func runIsNewer(n, old Run) bool {
	if n.RunAttempt != old.RunAttempt {
		return n.RunAttempt > old.RunAttempt
	}
	if !n.UpdatedAt.Equal(old.UpdatedAt) {
		return n.UpdatedAt.After(old.UpdatedAt)
	}
	if statusRank(n.Status) != statusRank(old.Status) {
		return statusRank(n.Status) > statusRank(old.Status)
	}
	return normalizeRun(n) != normalizeRun(old)
}

func normalizeRun(r Run) Run {
	r.CreatedAt, r.RunStartedAt, r.UpdatedAt = fromUnix(unix(r.CreatedAt)), fromUnix(unix(r.RunStartedAt)), fromUnix(unix(r.UpdatedAt))
	return r
}

// MarkJobsFetched records that a run's jobs have been loaded from GitHub.
func (s *Store) MarkJobsFetched(ctx context.Context, runID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET jobs_fetched = 1 WHERE id = ?`, runID)
	return err
}

// RunsMissingJobs returns finished runs in a repo, newest first, whose jobs were never fetched.
// Their jobs are needed for cost estimates.
func (s *Store) RunsMissingJobs(ctx context.Context, repoID int64, since time.Time, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id FROM runs r
		WHERE r.repo_id = ? AND r.status = 'completed' AND r.jobs_fetched = 0 AND r.updated_at >= ?
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.run_id = r.id AND j.run_attempt = r.run_attempt AND j.status = 'completed')
		ORDER BY r.updated_at DESC LIMIT ?`, repoID, unix(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListActiveRuns returns runs not yet completed, for reconciliation.
func (s *Store) ListActiveRuns(ctx context.Context, repoID int64) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs r WHERE r.repo_id = ? AND r.status != 'completed'`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Jobs

const jobColumns = `id, run_id, run_attempt, name, status, conclusion, labels, runner_name, created_at, started_at, completed_at, html_url, steps, check_run_id`

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var labels, steps string
	var created, started, completed int64
	err := sc.Scan(&j.ID, &j.RunID, &j.RunAttempt, &j.Name, &j.Status, &j.Conclusion, &labels, &j.RunnerName,
		&created, &started, &completed, &j.HTMLURL, &steps, &j.CheckRunID)
	if labels != "" {
		j.Labels = strings.Split(labels, ",")
	}
	if steps != "" && err == nil {
		err = json.Unmarshal([]byte(steps), &j.Steps)
	}
	j.CreatedAt, j.StartedAt, j.CompletedAt = fromUnix(created), fromUnix(started), fromUnix(completed)
	return j, err
}

// UpsertJob stores a job unless that would move it backwards in its lifecycle.
func (s *Store) UpsertJob(ctx context.Context, j Job) (changed bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	old, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, j.ID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	default:
		if statusRank(j.Status) < statusRank(old.Status) {
			return false, nil
		}
		if jobEqual(j, old) {
			return false, nil
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO jobs (`+jobColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.RunID, j.RunAttempt, j.Name, j.Status, j.Conclusion, strings.Join(j.Labels, ","), j.RunnerName,
		unix(j.CreatedAt), unix(j.StartedAt), unix(j.CompletedAt), j.HTMLURL, encodeSteps(j.Steps), j.CheckRunID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func jobEqual(a, b Job) bool {
	return a.RunID == b.RunID && a.RunAttempt == b.RunAttempt && a.Name == b.Name && a.Status == b.Status &&
		a.Conclusion == b.Conclusion && strings.Join(a.Labels, ",") == strings.Join(b.Labels, ",") &&
		a.RunnerName == b.RunnerName && a.CreatedAt.Equal(b.CreatedAt) && a.StartedAt.Equal(b.StartedAt) &&
		a.CompletedAt.Equal(b.CompletedAt) && a.HTMLURL == b.HTMLURL && encodeSteps(a.Steps) == encodeSteps(b.Steps) && a.CheckRunID == b.CheckRunID
}

func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
}

// ListJobs returns the jobs of the given runs, keyed by run ID, limited to each run's current attempt.
func (s *Store) ListJobs(ctx context.Context, runs []Run) (map[int64][]Job, error) {
	out := make(map[int64][]Job, len(runs))
	if len(runs) == 0 {
		return out, nil
	}
	attempt := make(map[int64]int, len(runs))
	placeholders := make([]string, len(runs))
	args := make([]any, len(runs))
	for i, r := range runs {
		attempt[r.ID] = r.RunAttempt
		placeholders[i] = "?"
		args[i] = r.ID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE run_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY CASE WHEN started_at = 0 THEN created_at ELSE started_at END, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		if j.RunAttempt == attempt[j.RunID] {
			out[j.RunID] = append(out[j.RunID], j)
		}
	}
	return out, rows.Err()
}

// Deliveries and notifications

// RecordDelivery returns false if the delivery ID was already seen.
func (s *Store) RecordDelivery(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO deliveries (id, received_at) VALUES (?, ?)`, id, unix(at))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkNotified returns false if this notification was already sent.
func (s *Store) MarkNotified(ctx context.Context, runID int64, attempt int, kind string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO notifications (run_id, run_attempt, kind, sent_at) VALUES (?, ?, ?, ?)`,
		runID, attempt, kind, unix(at))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Prune drops runs (and their jobs) with no activity since runsBefore, and deliveries older than deliveriesBefore.
func (s *Store) Prune(ctx context.Context, runsBefore, deliveriesBefore time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cut := unix(runsBefore)
	stmts := []struct {
		q   string
		arg int64
	}{
		{`DELETE FROM jobs WHERE run_id IN (SELECT id FROM runs WHERE updated_at < ? AND status = 'completed')`, cut},
		{`DELETE FROM notifications WHERE run_id IN (SELECT id FROM runs WHERE updated_at < ? AND status = 'completed')`, cut},
		{`DELETE FROM runs WHERE updated_at < ? AND status = 'completed'`, cut},
		{`DELETE FROM jobs WHERE run_id NOT IN (SELECT id FROM runs) AND created_at < ?`, cut},
		{`DELETE FROM deliveries WHERE received_at < ?`, unix(deliveriesBefore)},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.q, st.arg); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Activity summarises a repo's recent Actions use, to decide how often to reconcile it.
type Activity struct {
	LastRun time.Time // most recent run update
	Active  int       // runs not yet completed
}

func (s *Store) RepoActivity(ctx context.Context) (map[int64]Activity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id, max(updated_at), sum(status != 'completed') FROM runs GROUP BY repo_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Activity{}
	for rows.Next() {
		var id, last int64
		var active int
		if err := rows.Scan(&id, &last, &active); err != nil {
			return nil, err
		}
		out[id] = Activity{LastRun: fromUnix(last), Active: active}
	}
	return out, rows.Err()
}

// RunMissingJobs is a finished run whose jobs were never fetched.
type RunMissingJobs struct {
	RunID  int64
	RepoID int64
}

// RunsMissingJobsFor returns, newest first, finished runs in an installation's repos
// whose jobs were never fetched.
func (s *Store) RunsMissingJobsFor(ctx context.Context, installationID int64, since time.Time, limit int) ([]RunMissingJobs, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.repo_id FROM runs r JOIN repos p ON p.id = r.repo_id
		WHERE p.installation_id = ? AND p.archived = 0 AND r.status = 'completed' AND r.jobs_fetched = 0 AND r.updated_at >= ?
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.run_id = r.id AND j.run_attempt = r.run_attempt AND j.status = 'completed')
		ORDER BY r.updated_at DESC LIMIT ?`, installationID, unix(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunMissingJobs
	for rows.Next() {
		var m RunMissingJobs
		if err := rows.Scan(&m.RunID, &m.RepoID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
