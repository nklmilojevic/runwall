// Package ingest turns GitHub API and webhook objects into stored rows, then fans out
// the side effects (live updates, failure notifications). Webhooks and the reconciler
// both go through it, so the two paths behave identically.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/nklmilojevic/runwall/internal/hub"
	"github.com/nklmilojevic/runwall/internal/notify"
	"github.com/nklmilojevic/runwall/internal/store"
)

type Ingester struct {
	Store    *store.Store
	Hub      *hub.Hub
	Notifier notify.Notifier
	Log      *slog.Logger
	// Runs last updated before Since never notify, so a backfill after downtime stays quiet.
	Since time.Time
}

func (i *Ingester) Repo(ctx context.Context, r store.Repo) error {
	return i.Store.UpsertRepo(ctx, r)
}

func (i *Ingester) Run(ctx context.Context, run store.Run) error {
	prev, changed, err := i.Store.UpsertRun(ctx, run)
	if err != nil || !changed {
		return err
	}
	i.Hub.Publish(run.ID)
	i.maybeNotify(ctx, prev, run)
	return nil
}

func (i *Ingester) Job(ctx context.Context, job store.Job) error {
	changed, err := i.Store.UpsertJob(ctx, job)
	if err != nil || !changed {
		return err
	}
	i.Hub.Publish(job.RunID)
	return nil
}

func IsFailure(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

func (i *Ingester) maybeNotify(ctx context.Context, prev *store.Run, run store.Run) {
	if run.Status != "completed" || !IsFailure(run.Conclusion) || run.UpdatedAt.Before(i.Since) {
		return
	}
	if prev != nil && prev.RunAttempt == run.RunAttempt && prev.Status == "completed" && IsFailure(prev.Conclusion) {
		return
	}
	repo, err := i.Store.GetRepo(ctx, run.RepoID)
	if err != nil {
		i.Log.Error("notify: load repo", "repo_id", run.RepoID, "err", err)
		return
	}
	if repo.DefaultBranch == "" || run.HeadBranch != repo.DefaultBranch {
		return
	}
	fresh, err := i.Store.MarkNotified(ctx, run.ID, run.RunAttempt, "failure", time.Now())
	if err != nil || !fresh {
		if err != nil {
			i.Log.Error("notify: record", "run_id", run.ID, "err", err)
		}
		return
	}
	msg := run.Title
	if msg == "" {
		msg = run.CommitMessage
	}
	n := notify.Notification{
		Title:    "✗ " + repo.FullName,
		Subtitle: fmt.Sprintf("%s · %s · %s", run.WorkflowName, run.HeadBranch, run.Conclusion),
		Message:  msg,
		URL:      run.HTMLURL,
		Group:    fmt.Sprintf("run-%d", run.ID),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := i.Notifier.Notify(ctx, n); err != nil {
			i.Log.Error("notify", "run_id", run.ID, "err", err)
		}
	}()
}

// Conversions

func ts(t *github.Timestamp) time.Time {
	if t == nil || t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Second)
}

func RepoFromGitHub(r *github.Repository, installationID int64) store.Repo {
	owner := r.GetOwner().GetLogin()
	full := r.GetFullName()
	if owner == "" {
		owner, _, _ = strings.Cut(full, "/")
	}
	return store.Repo{
		ID:             r.GetID(),
		InstallationID: installationID,
		Owner:          owner,
		Name:           r.GetName(),
		FullName:       full,
		DefaultBranch:  r.GetDefaultBranch(),
		Archived:       r.GetArchived(),
		Fork:           r.GetFork(),
		Private:        r.GetPrivate(),
		Description:    r.GetDescription(),
		OpenIssues:     r.GetOpenIssuesCount(),
		PushedAt:       ts(r.PushedAt),
	}
}

func IsBot(u *github.User) bool {
	return u.GetType() == "Bot" || strings.HasSuffix(u.GetLogin(), "[bot]")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

func RunFromGitHub(r *github.WorkflowRun, repoID int64) store.Run {
	actor := r.GetTriggeringActor()
	if actor == nil {
		actor = r.GetActor()
	}
	var pr int
	if len(r.PullRequests) > 0 {
		pr = r.PullRequests[0].GetNumber()
	}
	attempt := r.GetRunAttempt()
	if attempt == 0 {
		attempt = 1
	}
	return store.Run{
		ID:            r.GetID(),
		RepoID:        repoID,
		WorkflowID:    r.GetWorkflowID(),
		WorkflowName:  r.GetName(),
		WorkflowPath:  r.GetPath(),
		RunNumber:     r.GetRunNumber(),
		RunAttempt:    attempt,
		Event:         r.GetEvent(),
		HeadBranch:    r.GetHeadBranch(),
		HeadSHA:       r.GetHeadSHA(),
		Title:         firstLine(r.GetDisplayTitle()),
		CommitMessage: firstLine(r.GetHeadCommit().GetMessage()),
		ActorLogin:    actor.GetLogin(),
		ActorIsBot:    IsBot(actor),
		Status:        r.GetStatus(),
		Conclusion:    r.GetConclusion(),
		PRNumber:      pr,
		CreatedAt:     ts(r.CreatedAt),
		RunStartedAt:  ts(r.RunStartedAt),
		UpdatedAt:     ts(r.UpdatedAt),
		HTMLURL:       r.GetHTMLURL(),
	}
}

func JobFromGitHub(j *github.WorkflowJob) store.Job {
	attempt := int(j.GetRunAttempt())
	if attempt == 0 {
		attempt = 1
	}
	var steps []store.Step
	for _, st := range j.Steps {
		steps = append(steps, store.Step{
			Number:      int(st.GetNumber()),
			Name:        st.GetName(),
			Status:      st.GetStatus(),
			Conclusion:  st.GetConclusion(),
			StartedAt:   ts(st.StartedAt),
			CompletedAt: ts(st.CompletedAt),
		})
	}
	return store.Job{
		ID:          j.GetID(),
		RunID:       j.GetRunID(),
		RunAttempt:  attempt,
		Name:        j.GetName(),
		Status:      j.GetStatus(),
		Conclusion:  j.GetConclusion(),
		Labels:      j.Labels,
		RunnerName:  j.GetRunnerName(),
		CreatedAt:   ts(j.CreatedAt),
		StartedAt:   ts(j.StartedAt),
		CompletedAt: ts(j.CompletedAt),
		HTMLURL:     j.GetHTMLURL(),
		Steps:       steps,
		CheckRunID:  lastID(j.GetCheckRunURL()),
	}
}

// lastID parses the trailing numeric ID of an API URL such as .../check-runs/123.
func lastID(u string) int64 {
	i := strings.LastIndexByte(u, '/')
	if i < 0 {
		return 0
	}
	id, err := strconv.ParseInt(u[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return id
}
