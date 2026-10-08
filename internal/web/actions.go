package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/store"
)

type runActionKind string

const (
	actionRerun       runActionKind = "rerun"
	actionRerunFailed runActionKind = "rerun-failed"
	actionCancel      runActionKind = "cancel"
)

var actionDone = map[runActionKind]string{
	actionRerun:       "Re-run requested for all jobs.",
	actionRerunFailed: "Re-run requested for the failed jobs.",
	actionCancel:      "Cancellation requested.",
}

// cancelRun asks GitHub to cancel a run. A run whose jobs were never created can sit
// queued forever, and GitHub answers a normal cancel for it with 409 Conflict. Only then,
// and only if GitHub confirms the run has no jobs, it force-cancels: force-cancel skips
// always() steps, and with no jobs there are none to skip.
func cancelRun(ctx context.Context, gh *github.Client, owner, repo string, id int64) (forced bool, err error) {
	_, err = gh.Actions.CancelWorkflowRunByID(ctx, owner, repo, id)
	var ge *github.ErrorResponse
	if !errors.As(err, &ge) || ge.Response.StatusCode != http.StatusConflict {
		return false, accepted(err)
	}
	jobs, _, jerr := gh.Actions.ListWorkflowJobs(ctx, owner, repo, id, &github.ListWorkflowJobsOptions{ListOptions: github.ListOptions{PerPage: 1}})
	if jerr != nil || jobs.GetTotalCount() > 0 {
		return false, err
	}
	req, rerr := gh.NewRequest(ctx, http.MethodPost, fmt.Sprintf("repos/%v/%v/actions/runs/%v/force-cancel", owner, repo, id), nil)
	if rerr != nil {
		return false, rerr
	}
	_, err = gh.Do(req, nil)
	return true, accepted(err)
}

// accepted treats 202 Accepted, which go-github reports as an error, as success.
func accepted(err error) error {
	var a *github.AcceptedError
	if errors.As(err, &a) {
		return nil
	}
	return err
}

// githubMessage turns a GitHub API error into something worth showing.
func githubMessage(err error) string {
	var ge *github.ErrorResponse
	if errors.As(err, &ge) {
		switch ge.Response.StatusCode {
		case http.StatusForbidden, http.StatusNotFound:
			return "GitHub refused: you need write access to this repository (" + ge.Message + ")."
		case http.StatusConflict, http.StatusUnprocessableEntity:
			return "GitHub refused: " + ge.Message + "."
		}
		return "GitHub error: " + ge.Message
	}
	return "Couldn't reach GitHub. Check the server log for details."
}

func (s *Server) audit(ctx context.Context, v *auth.Viewer, action, repo string, target int64, err error) {
	result := "ok"
	if err != nil {
		result = err.Error()
	}
	if aerr := s.Store.Audit(ctx, store.AuditEntry{At: s.Now(), Login: v.User.Login, Action: action, Repo: repo, Target: target, Result: result}); aerr != nil {
		s.Log.Error("audit", "err", aerr)
	}
	s.Log.Info("run action", "login", v.User.Login, "action", action, "repo", repo, "target", target, "result", result)
}

// refreshSoon re-reads a run in the background so the page shows the new state quickly.
func (s *Server) refreshSoon(runID int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		time.Sleep(2 * time.Second)
		if err := s.Syncer.RefreshRun(ctx, runID); err != nil {
			s.Log.Error("refresh after action", "run", runID, "err", err)
		}
	}()
}

func (s *Server) runAction(kind runActionKind) handler {
	return func(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
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
		if !v.CanAct(ctx, run.RepoID) {
			toast(w, "error", "You need write access to this repository to do that.")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		repo, err := s.Store.GetRepo(ctx, run.RepoID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if s.Demo {
			toast(w, "ok", actionDone[kind]+" (demo mode: nothing was sent to GitHub)")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		gh, err := v.GitHub(ctx)
		if err != nil {
			w.Header().Set("HX-Redirect", "/login?next="+r.Referer())
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		action, done := string(kind), actionDone[kind]
		switch kind {
		case actionRerun:
			_, err = gh.Actions.RerunWorkflowByID(ctx, repo.Owner, repo.Name, id)
		case actionRerunFailed:
			_, err = gh.Actions.RerunFailedJobsByID(ctx, repo.Owner, repo.Name, id)
		case actionCancel:
			var forced bool
			if forced, err = cancelRun(ctx, gh, repo.Owner, repo.Name, id); forced {
				action, done = "force-cancel", "Cancellation forced: GitHub never created jobs for this run."
			}
		}
		s.audit(ctx, v, action, repo.FullName, id, err)
		if err != nil {
			toast(w, "error", githubMessage(err))
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		s.refreshSoon(id)
		toast(w, "ok", done)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) jobRerun(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	job, run, ok := s.visibleJob(w, r, v)
	if !ok {
		return
	}
	ctx := r.Context()
	if !v.CanAct(ctx, run.RepoID) {
		toast(w, "error", "You need write access to this repository to do that.")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	repo, err := s.Store.GetRepo(ctx, run.RepoID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.Demo {
		toast(w, "ok", "Re-run requested for "+job.Name+" (demo mode: nothing was sent to GitHub)")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	gh, err := v.GitHub(ctx)
	if err != nil {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	_, err = gh.Actions.RerunJobByID(ctx, repo.Owner, repo.Name, job.ID)
	s.audit(ctx, v, "rerun-job", repo.FullName, job.ID, err)
	if err != nil {
		toast(w, "error", githubMessage(err))
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	s.refreshSoon(run.ID)
	toast(w, "ok", "Re-run requested for "+job.Name+".")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	s.Syncer.TriggerAll()
	toast(w, "ok", "Syncing with GitHub. New runs appear as they're found.")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rescore(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
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
	if s.Scorer == nil {
		toast(w, "error", "Grading isn't available in demo mode.")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	sc, err := s.Scorer.Score(ctx, repo)
	if err != nil {
		s.Log.Error("rescore", "repo", repo.FullName, "err", err)
		toast(w, "error", "Couldn't grade "+repo.FullName+": "+githubMessage(err))
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	toast(w, "ok", repo.FullName+" graded "+strings.ToUpper(sc.Tier[:1])+sc.Tier[1:]+" ("+itoa(sc.Score)+"/100).")
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// Settings forms are plain POSTs that re-render the page.

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	ctx := r.Context()
	if v.User.ID == 0 {
		http.Error(w, "Sign in to create tokens.", http.StatusForbidden)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "MCP token"
	}
	token := auth.RandomToken("gad_")
	if err := s.Store.CreateAPIToken(ctx, v.User.ID, name, auth.Hash(token), s.Now()); err != nil {
		s.fail(w, err)
		return
	}
	sv, err := s.settingsView(ctx, r, v)
	if err != nil {
		s.fail(w, err)
		return
	}
	sv.NewToken = token
	s.render(w, r, settingsPage(sv))
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	if id, ok := pathID(r); ok {
		if err := s.Store.DeleteAPIToken(r.Context(), v.User.ID, id); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) createKiosk(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	if !v.Admin {
		http.Error(w, "Only admins can create kiosk links.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Couldn't read the form.", http.StatusBadRequest)
		return
	}
	insts, err := s.Store.ListInstallations(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	var owners []string
	for _, o := range r.PostForm["owners"] {
		if slices.ContainsFunc(insts, func(in store.Installation) bool { return in.Account == o }) {
			owners = append(owners, o)
		}
	}
	sv, err := s.settingsView(ctx, r, v)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(owners) == 0 {
		sv.NewKioskURL = ""
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, r, settingsPage(sv))
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "Wall display"
	}
	token := auth.RandomToken("kiosk_")
	if err := s.Store.CreateKioskLink(ctx, name, auth.Hash(token), owners, v.User.Login, s.Now()); err != nil {
		s.fail(w, err)
		return
	}
	if sv, err = s.settingsView(ctx, r, v); err != nil {
		s.fail(w, err)
		return
	}
	sv.NewKioskURL = s.BaseURL + "/kiosk/" + token
	s.render(w, r, settingsPage(sv))
}

func (s *Server) deleteKiosk(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	if !v.Admin {
		http.Error(w, "Only admins can revoke kiosk links.", http.StatusForbidden)
		return
	}
	if id, ok := pathID(r); ok {
		if err := s.Store.DeleteKioskLink(r.Context(), id); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
