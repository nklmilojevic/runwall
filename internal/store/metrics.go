package store

import (
	"context"
	"strings"
	"time"
)

// MetricRun is the slice of a run that metrics are computed from.
type MetricRun struct {
	ID            int64
	RepoID        int64
	WorkflowID    int64
	Owner         string
	RepoFullName  string
	DefaultBranch string
	WorkflowName  string
	WorkflowPath  string
	HeadBranch    string
	Event         string
	Status        string
	Conclusion    string
	RunAttempt    int
	Start         time.Time
	End           time.Time // zero while unfinished
	HTMLURL       string
}

// MetricRuns returns runs that started within the scope since the given time.
func (s *Store) MetricRuns(ctx context.Context, sc Scope, since time.Time) ([]MetricRun, error) {
	cl, args := sc.clause()
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.repo_id, r.workflow_id, p.owner, p.full_name, p.default_branch, r.workflow_name, r.workflow_path,
			r.head_branch, r.event, r.status, r.conclusion, r.run_attempt,
			max(r.created_at, r.run_started_at), CASE WHEN r.status = 'completed' THEN r.updated_at ELSE 0 END, r.html_url
		FROM runs r JOIN repos p ON p.id = r.repo_id
		WHERE max(r.created_at, r.run_started_at) >= ? AND `+cl+`
		ORDER BY max(r.created_at, r.run_started_at)`, append([]any{unix(since)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricRun
	for rows.Next() {
		var m MetricRun
		var start, end int64
		if err := rows.Scan(&m.ID, &m.RepoID, &m.WorkflowID, &m.Owner, &m.RepoFullName, &m.DefaultBranch, &m.WorkflowName,
			&m.WorkflowPath, &m.HeadBranch, &m.Event, &m.Status, &m.Conclusion, &m.RunAttempt, &start, &end, &m.HTMLURL); err != nil {
			return nil, err
		}
		m.Start, m.End = fromUnix(start), fromUnix(end)
		out = append(out, m)
	}
	return out, rows.Err()
}

// CostJob is a finished job with what's needed to price it.
type CostJob struct {
	RunID      int64
	RepoID     int64
	WorkflowID int64
	Private    bool
	Labels     []string
	RunnerName string
	Started    time.Time
	Completed  time.Time
}

// CostJobs returns finished jobs in the scope that completed since the given time, across all attempts.
func (s *Store) CostJobs(ctx context.Context, sc Scope, since time.Time) ([]CostJob, error) {
	cl, args := sc.clause()
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.run_id, r.repo_id, r.workflow_id, p.private, j.labels, j.runner_name, j.started_at, j.completed_at
		FROM jobs j JOIN runs r ON r.id = j.run_id JOIN repos p ON p.id = r.repo_id
		WHERE j.status = 'completed' AND j.started_at > 0 AND j.completed_at >= ? AND `+cl,
		append([]any{unix(since)}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostJob
	for rows.Next() {
		var c CostJob
		var private int
		var labels string
		var started, completed int64
		if err := rows.Scan(&c.RunID, &c.RepoID, &c.WorkflowID, &private, &labels, &c.RunnerName, &started, &completed); err != nil {
			return nil, err
		}
		c.Private = private == 1
		if labels != "" {
			c.Labels = strings.Split(labels, ",")
		}
		c.Started, c.Completed = fromUnix(started), fromUnix(completed)
		out = append(out, c)
	}
	return out, rows.Err()
}

// VisibleRepos lists repos in the scope.
func (s *Store) VisibleRepos(ctx context.Context, sc Scope) ([]Repo, error) {
	cl, args := sc.clause()
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixed(repoColumns, "p.")+` FROM repos p WHERE `+cl+` ORDER BY p.full_name COLLATE NOCASE`, args...)
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

func prefixed(columns, prefix string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
