// Package mcpserver exposes the dashboard's data as read-only MCP tools over streamable
// HTTP. Every call is authenticated with a personal API token and sees only the token
// owner's repos.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/cost"
	"github.com/nklmilojevic/runwall/internal/joblog"
	"github.com/nklmilojevic/runwall/internal/metrics"
	"github.com/nklmilojevic/runwall/internal/scoring"
	"github.com/nklmilojevic/runwall/internal/store"
)

type Logs interface {
	JobLog(ctx context.Context, jobID int64) (string, error)
}

type Server struct {
	Store   *store.Store
	Auth    *auth.Auth
	Logs    Logs
	Costs   cost.Table
	Log     *slog.Logger
	Now     func() time.Time
	Version string
}

// Handler serves MCP at the mounted path. It is stateless: each request builds a
// server bound to the caller's scope.
func (s *Server) Handler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.server(auth.FromContext(r.Context()))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dashboard"`)
			http.Error(w, "Create a personal API token in the dashboard's Settings page and send it as a Bearer token.", http.StatusUnauthorized)
			return
		}
		v, err := s.Auth.APIViewer(r.Context(), token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dashboard", error="invalid_token"`)
			http.Error(w, "Unknown or revoked API token.", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r.WithContext(auth.WithViewer(r.Context(), v)))
	})
}

func result(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

func textResult(s string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}, nil, nil
}

var errNotFound = errors.New("not found, or not visible to this token's user")

type ListRunsIn struct {
	Org         string `json:"org,omitempty" jsonschema:"organization or user login"`
	Repo        string `json:"repo,omitempty" jsonschema:"repository full name, e.g. acme/api"`
	Branch      string `json:"branch,omitempty"`
	Event       string `json:"event,omitempty" jsonschema:"push, pull_request, schedule, workflow_dispatch, ..."`
	Status      string `json:"status,omitempty" jsonschema:"active, stuck, failure, success or cancelled"`
	DefaultOnly bool   `json:"default_branch_only,omitempty"`
	IncludeBots bool   `json:"include_bots,omitempty"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max runs to return, default 25, max 100"`
}

type runOut struct {
	ID         int64     `json:"id"`
	Repo       string    `json:"repo"`
	Workflow   string    `json:"workflow"`
	Number     int       `json:"number"`
	Attempt    int       `json:"attempt"`
	Branch     string    `json:"branch"`
	PR         int       `json:"pull_request,omitempty"`
	Event      string    `json:"event"`
	Title      string    `json:"title"`
	Actor      string    `json:"actor"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion,omitempty"`
	Stuck      bool      `json:"stuck,omitempty"`
	Started    time.Time `json:"started"`
	Updated    time.Time `json:"updated"`
	URL        string    `json:"url"`
}

func toRunOut(r store.FeedRun) runOut {
	return runOut{ID: r.ID, Repo: r.Owner + "/" + r.RepoName, Workflow: r.WorkflowName, Number: r.RunNumber, Attempt: r.RunAttempt,
		Branch: r.HeadBranch, PR: r.PRNumber, Event: r.Event, Title: r.Title, Actor: r.ActorLogin, Status: r.Status,
		Conclusion: r.Conclusion, Stuck: r.Stuck, Started: r.QueuedSince(), Updated: r.UpdatedAt, URL: r.HTMLURL}
}

type RunIn struct {
	RunID int64 `json:"run_id"`
}

type JobLogIn struct {
	JobID int64 `json:"job_id"`
	Lines int   `json:"lines,omitempty" jsonschema:"how many trailing lines, default 200, max 1000"`
}

type PeriodIn struct {
	Period string `json:"period,omitempty" jsonschema:"24h, 7d, 30d or 90d (default 30d)"`
	Repo   string `json:"repo,omitempty" jsonschema:"limit to one repository full name"`
}

type RepoIn struct {
	Repo string `json:"repo" jsonschema:"repository full name, e.g. acme/api"`
}

func (s *Server) server(v *auth.Viewer) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "runwall", Version: s.Version}, nil)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(srv, &mcp.Tool{Name: "list_runs", Annotations: readOnly,
		Description: "List recent GitHub Actions workflow runs across the user's repositories, newest first, with the same filters as the dashboard's Runs page."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ListRunsIn) (*mcp.CallToolResult, any, error) {
			limit := in.Limit
			if limit <= 0 {
				limit = 25
			}
			feed, err := s.Store.Feed(ctx, store.FeedFilter{Scope: v.Scope, Owner: in.Org, Repo: in.Repo, Branch: in.Branch, Event: in.Event,
				Status: store.StatusFilter(in.Status), DefaultOnly: in.DefaultOnly, ShowBots: in.IncludeBots, Limit: min(limit, 100)},
				s.Now().Add(-5*time.Minute))
			if err != nil {
				return nil, nil, err
			}
			out := make([]runOut, 0, len(feed))
			for _, r := range feed {
				out = append(out, toRunOut(r))
			}
			return result(out)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_run", Annotations: readOnly,
		Description: "Get one workflow run with its jobs and their steps."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, any, error) {
			run, err := s.Store.GetRun(ctx, in.RunID)
			if err != nil {
				return nil, nil, errNotFound
			}
			if ok, _ := s.Store.CanSeeRepo(ctx, v.Scope, run.RepoID); !ok {
				return nil, nil, errNotFound
			}
			repo, _ := s.Store.GetRepo(ctx, run.RepoID)
			jobs, err := s.Store.ListJobs(ctx, []store.Run{run})
			if err != nil {
				return nil, nil, err
			}
			return result(map[string]any{
				"run":  toRunOut(store.FeedRun{Run: run, Owner: repo.Owner, RepoName: repo.Name}),
				"jobs": jobs[run.ID],
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_job_log", Annotations: readOnly,
		Description: "Get the tail of a finished job's log (timestamps and colours stripped, errors marked). GitHub publishes logs only after a job finishes."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in JobLogIn) (*mcp.CallToolResult, any, error) {
			job, err := s.Store.GetJob(ctx, in.JobID)
			if err != nil {
				return nil, nil, errNotFound
			}
			run, err := s.Store.GetRun(ctx, job.RunID)
			if err != nil {
				return nil, nil, errNotFound
			}
			if ok, _ := s.Store.CanSeeRepo(ctx, v.Scope, run.RepoID); !ok {
				return nil, nil, errNotFound
			}
			raw, err := s.Logs.JobLog(ctx, in.JobID)
			if err != nil {
				return nil, nil, fmt.Errorf("log unavailable: %w", err)
			}
			lines := in.Lines
			if lines <= 0 {
				lines = 200
			}
			lv := joblog.Parse(raw, min(lines, joblog.Tail))
			return textResult(fmt.Sprintf("# %s (job %d), last %d of %d lines\n%s", job.Name, job.ID, len(lv.Lines), lv.Total, lv.Text()))
		})

	summary := func(ctx context.Context, in PeriodIn) (metrics.Summary, error) {
		p := metrics.ParsePeriod(in.Period)
		since := s.Now().Add(-p.Dur)
		runs, err := s.Store.MetricRuns(ctx, v.Scope, since)
		if err != nil {
			return metrics.Summary{}, err
		}
		jobs, err := s.Store.CostJobs(ctx, v.Scope, since)
		if err != nil {
			return metrics.Summary{}, err
		}
		return metrics.Compute(p, s.Now(), runs, jobs, s.Costs), nil
	}

	mcp.AddTool(srv, &mcp.Tool{Name: "failing_workflows", Annotations: readOnly,
		Description: "List workflows whose latest finished run on the default branch failed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in PeriodIn) (*mcp.CallToolResult, any, error) {
			sum, err := summary(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			type failing struct {
				Repo     string    `json:"repo"`
				Workflow string    `json:"workflow"`
				RunID    int64     `json:"run_id"`
				Since    time.Time `json:"failed_at"`
				URL      string    `json:"url"`
			}
			var out []failing
			for _, w := range sum.Workflows {
				if in.Repo != "" && w.Repo != in.Repo {
					continue
				}
				if d := w.LastDefault; d != nil && metrics.OutcomeOf(d.Status, d.Conclusion) == metrics.Failure {
					out = append(out, failing{w.Repo, w.Name, d.ID, d.End, d.HTMLURL})
				}
			}
			return result(out)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "workflow_stats", Annotations: readOnly,
		Description: "Per-workflow run counts, success rate, average and p95 duration, and estimated cost over a period."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in PeriodIn) (*mcp.CallToolResult, any, error) {
			sum, err := summary(ctx, in)
			if err != nil {
				return nil, nil, err
			}
			type stat struct {
				Repo        string  `json:"repo"`
				Workflow    string  `json:"workflow"`
				Runs        int     `json:"runs"`
				SuccessRate float64 `json:"success_rate"`
				AvgSeconds  int     `json:"avg_seconds"`
				P95Seconds  int     `json:"p95_seconds"`
				Minutes     int     `json:"billable_minutes"`
				CostUSD     float64 `json:"estimated_cost_usd"`
			}
			var out []stat
			for _, w := range sum.Workflows {
				if in.Repo != "" && w.Repo != in.Repo {
					continue
				}
				out = append(out, stat{w.Repo, w.Name, w.Counts.Runs, w.Counts.SuccessRate(), int(w.Durations.Avg().Seconds()),
					int(w.Durations.P95().Seconds()), w.Cost.Minutes, w.Cost.USD})
			}
			return result(map[string]any{"period": sum.Period.Key, "prices_as_of": s.Costs.AsOf, "costs_partial": sum.CostsPartial, "workflows": out})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "repo_score", Annotations: readOnly,
		Description: "Get a repository's grade (gold/silver/bronze) and the checks behind it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RepoIn) (*mcp.CallToolResult, any, error) {
			repos, err := s.Store.VisibleRepos(ctx, v.Scope)
			if err != nil {
				return nil, nil, err
			}
			for _, r := range repos {
				if !strings.EqualFold(r.FullName, in.Repo) {
					continue
				}
				sc, err := s.Store.GetScore(ctx, r.ID)
				if err != nil {
					return textResult(in.Repo + " has not been graded yet.")
				}
				return result(map[string]any{"repo": r.FullName, "score": sc.Score, "tier": sc.Tier, "computed_at": sc.ComputedAt,
					"categories": scoring.ParseBreakdown(sc.Breakdown).Categories})
			}
			return nil, nil, errNotFound
		})

	return srv
}
