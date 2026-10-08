package store

import (
	"context"
	"strings"
	"time"
)

type StatusFilter string

const (
	StatusAll       StatusFilter = ""
	StatusActive    StatusFilter = "active"
	StatusStuck     StatusFilter = "stuck"
	StatusFailure   StatusFilter = "failure"
	StatusSuccess   StatusFilter = "success"
	StatusCancelled StatusFilter = "cancelled"
)

// Scope limits what a viewer can see. The zero value sees nothing.
type Scope struct {
	All    bool      // demo mode and internal callers
	UserID int64     // a signed-in user: repos in user_repos
	Owners []string  // a kiosk link: repos owned by these accounts
	Only   Selection // narrows any of the above
}

// Selection keeps what matches every non-empty list. Values within a list are alternatives.
type Selection struct {
	Owners []string
	Repos  []string // full names
	Topics []string
	Actors []string // who triggered a run; applies to runs, not repos
}

func (f Selection) Empty() bool {
	return len(f.Owners) == 0 && len(f.Repos) == 0 && len(f.Topics) == 0 && len(f.Actors) == 0
}

// runClause is clause plus the parts of the selection that apply to runs, aliased as r.
func (sc Scope) runClause() (string, []any) {
	cl, args := sc.clause()
	if len(sc.Only.Actors) > 0 {
		cl += " AND r.actor_login IN (" + placeholders(len(sc.Only.Actors)) + ")"
		args = appendStrings(args, sc.Only.Actors)
	}
	return cl, args
}

// clause returns a SQL condition on the repos table aliased as p. It ignores Only.Actors.
func (sc Scope) clause() (string, []any) {
	cl, args := sc.base()
	if len(sc.Only.Owners) > 0 {
		cl += " AND p.owner IN (" + placeholders(len(sc.Only.Owners)) + ")"
		args = appendStrings(args, sc.Only.Owners)
	}
	if len(sc.Only.Repos) > 0 {
		cl += " AND p.full_name IN (" + placeholders(len(sc.Only.Repos)) + ")"
		args = appendStrings(args, sc.Only.Repos)
	}
	if len(sc.Only.Topics) > 0 {
		match := strings.TrimSuffix(strings.Repeat("instr(',' || p.topics || ',', ',' || ? || ',') > 0 OR ", len(sc.Only.Topics)), " OR ")
		cl += " AND (" + match + ")"
		args = appendStrings(args, sc.Only.Topics)
	}
	return cl, args
}

func (sc Scope) base() (string, []any) {
	switch {
	case sc.All:
		return "1 = 1", nil
	case sc.UserID != 0:
		return "p.id IN (SELECT repo_id FROM user_repos WHERE user_id = ?)", []any{sc.UserID}
	case len(sc.Owners) > 0:
		return "p.owner IN (" + placeholders(len(sc.Owners)) + ")", appendStrings(nil, sc.Owners)
	}
	return "0 = 1", nil
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

func appendStrings(args []any, vs []string) []any {
	for _, v := range vs {
		args = append(args, v)
	}
	return args
}

// CanSeeRepo reports whether the scope includes a repo.
func (s *Store) CanSeeRepo(ctx context.Context, sc Scope, repoID int64) (bool, error) {
	cl, args := sc.clause()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM repos p WHERE p.id = ? AND `+cl, append([]any{repoID}, args...)...).Scan(&n)
	return n == 1, err
}

type FeedFilter struct {
	Scope        Scope
	Owner        string
	Repo         string // full name
	RepoID       int64
	WorkflowID   int64
	Branch       string
	Event        string
	Actor        string
	Status       StatusFilter
	DefaultOnly  bool
	ShowArchived bool
	ShowForks    bool
	ShowBots     bool
	Limit        int
}

type FeedRun struct {
	Run
	Owner         string
	RepoName      string
	DefaultBranch string
	Stuck         bool
	Jobs          []Job
}

type Summary struct {
	Active         int
	Stuck          int
	FailingDefault int
}

// stuckExpr is true when the run's current attempt, or one of its jobs, has been queued since before the cutoff.
// It expects two cutoff arguments.
const stuckExpr = `(
	(r.status IN ('queued', 'requested', 'pending') AND max(r.created_at, r.run_started_at) < ?)
	OR (r.status != 'completed' AND EXISTS (
		SELECT 1 FROM jobs j WHERE j.run_id = r.id AND j.run_attempt = r.run_attempt
			AND j.status IN ('queued', 'requested', 'pending') AND j.created_at > 0 AND j.created_at < ?))
)`

func visibilityClause(f FeedFilter) (string, []any) {
	cl, args := f.Scope.runClause()
	where := []string{cl}
	if !f.ShowArchived {
		where = append(where, `p.archived = 0`)
	}
	if !f.ShowForks {
		where = append(where, `p.fork = 0`)
	}
	if !f.ShowBots {
		where = append(where, `r.actor_is_bot = 0`)
	}
	if f.Owner != "" {
		where = append(where, `p.owner = ?`)
		args = append(args, f.Owner)
	}
	if f.Repo != "" {
		where = append(where, `p.full_name = ?`)
		args = append(args, f.Repo)
	}
	if f.Actor != "" {
		where = append(where, `r.actor_login = ?`)
		args = append(args, f.Actor)
	}
	return strings.Join(where, " AND "), args
}

func (s *Store) Feed(ctx context.Context, f FeedFilter, stuckCutoff time.Time) ([]FeedRun, error) {
	cut := unix(stuckCutoff)
	vis, args := visibilityClause(f)
	where := []string{"1 = 1"}
	if vis != "" {
		where = append(where, vis)
	}
	if f.Branch != "" {
		where = append(where, `r.head_branch = ?`)
		args = append(args, f.Branch)
	}
	if f.Event != "" {
		where = append(where, `r.event = ?`)
		args = append(args, f.Event)
	}
	if f.RepoID != 0 {
		where = append(where, `r.repo_id = ?`)
		args = append(args, f.RepoID)
	}
	if f.WorkflowID != 0 {
		where = append(where, `r.workflow_id = ?`)
		args = append(args, f.WorkflowID)
	}
	if f.DefaultOnly {
		where = append(where, `r.head_branch = p.default_branch`)
	}
	switch f.Status {
	case StatusActive:
		where = append(where, `r.status != 'completed'`)
	case StatusStuck:
		where = append(where, stuckExpr)
		args = append(args, cut, cut)
	case StatusFailure:
		where = append(where, `r.status = 'completed' AND r.conclusion IN ('failure', 'timed_out', 'startup_failure')`)
	case StatusSuccess:
		where = append(where, `r.status = 'completed' AND r.conclusion = 'success'`)
	case StatusCancelled:
		where = append(where, `r.status = 'completed' AND r.conclusion = 'cancelled'`)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	q := `SELECT ` + runColumns + `, p.owner, p.name, p.default_branch, ` + stuckExpr + `
		FROM runs r JOIN repos p ON p.id = r.repo_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY max(r.created_at, r.run_started_at) DESC, r.id DESC
		LIMIT ?`
	allArgs := append([]any{cut, cut}, args...)
	allArgs = append(allArgs, limit)

	rows, err := s.db.QueryContext(ctx, q, allArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedRun
	var runs []Run
	for rows.Next() {
		var fr FeedRun
		var stuck int
		run, err := scanRun(rows, &fr.Owner, &fr.RepoName, &fr.DefaultBranch, &stuck)
		if err != nil {
			return nil, err
		}
		fr.Run, fr.Stuck = run, stuck == 1
		out = append(out, fr)
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	jobs, err := s.ListJobs(ctx, runs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Jobs = jobs[out[i].ID]
	}
	return out, nil
}

// Summary counts active and stuck runs, and workflows whose latest completed default-branch run failed.
func (s *Store) Summary(ctx context.Context, f FeedFilter, stuckCutoff time.Time) (Summary, error) {
	cut := unix(stuckCutoff)
	vis, args := visibilityClause(f)
	if vis == "" {
		vis = "1 = 1"
	}
	var sum Summary
	err := s.db.QueryRowContext(ctx, `
		SELECT
			coalesce(sum(r.status != 'completed'), 0),
			coalesce(sum(`+stuckExpr+`), 0)
		FROM runs r JOIN repos p ON p.id = r.repo_id WHERE `+vis,
		append([]any{cut, cut}, args...)...).Scan(&sum.Active, &sum.Stuck)
	if err != nil {
		return sum, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM runs r JOIN repos p ON p.id = r.repo_id
		WHERE `+vis+`
			AND r.head_branch = p.default_branch
			AND r.status = 'completed' AND r.conclusion IN ('failure', 'timed_out', 'startup_failure')
			AND r.id = (
				SELECT r2.id FROM runs r2
				WHERE r2.repo_id = r.repo_id AND r2.workflow_id = r.workflow_id
					AND r2.head_branch = r.head_branch AND r2.status = 'completed'
					AND r2.conclusion NOT IN ('cancelled', 'skipped')
				ORDER BY max(r2.created_at, r2.run_started_at) DESC, r2.id DESC LIMIT 1)`,
		args...).Scan(&sum.FailingDefault)
	return sum, err
}

type FilterOptions struct {
	Owners []string
	Repos  []string
	Events []string
	Actors []string
}

func (s *Store) FilterOptions(ctx context.Context, sc Scope) (FilterOptions, error) {
	var fo FilterOptions
	cl, args := sc.clause()
	queries := []struct {
		q   string
		dst *[]string
	}{
		{`SELECT DISTINCT p.owner FROM repos p WHERE ` + cl + ` ORDER BY p.owner COLLATE NOCASE`, &fo.Owners},
		{`SELECT p.full_name FROM repos p WHERE p.archived = 0 AND ` + cl + ` ORDER BY p.full_name COLLATE NOCASE`, &fo.Repos},
		{`SELECT DISTINCT r.event FROM runs r JOIN repos p ON p.id = r.repo_id WHERE r.event != '' AND ` + cl + ` ORDER BY r.event`, &fo.Events},
		{`SELECT DISTINCT r.actor_login FROM runs r JOIN repos p ON p.id = r.repo_id WHERE r.actor_login != '' AND ` + cl + ` ORDER BY r.actor_login COLLATE NOCASE`, &fo.Actors},
	}
	for _, q := range queries {
		rows, err := s.db.QueryContext(ctx, q.q, args...)
		if err != nil {
			return fo, err
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return fo, err
			}
			*q.dst = append(*q.dst, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fo, err
		}
	}
	return fo, nil
}

// ActorOwners maps each user who triggered a run in the scope to the accounts owning those repos.
func (s *Store) ActorOwners(ctx context.Context, sc Scope) (map[string][]string, error) {
	cl, args := sc.clause()
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT r.actor_login, p.owner FROM runs r JOIN repos p ON p.id = r.repo_id
		WHERE r.actor_login != '' AND `+cl+` ORDER BY p.owner`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var actor, owner string
		if err := rows.Scan(&actor, &owner); err != nil {
			return nil, err
		}
		out[actor] = append(out[actor], owner)
	}
	return out, rows.Err()
}
