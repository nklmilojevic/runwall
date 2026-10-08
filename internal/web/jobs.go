package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/ghapp"
	"github.com/nklmilojevic/runwall/internal/joblog"
	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/syncer"
)

// jobs lazily renders a run's jobs inside an expanded feed row.
func (s *Server) jobs(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
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
	jobs, err := s.Store.ListJobs(ctx, []store.Run{run})
	if err != nil {
		s.fail(w, err)
		return
	}
	var fetchErr error
	if len(jobs[id]) == 0 {
		if fetchErr = s.Syncer.FetchJobsForRun(ctx, id); fetchErr == nil {
			if jobs, err = s.Store.ListJobs(ctx, []store.Run{run}); err != nil {
				s.fail(w, err)
				return
			}
		} else {
			s.Log.Error("fetch jobs", "run", id, "err", fetchErr)
		}
	}
	s.render(w, r, jobList(jobs[id], s.Now(), s.StuckThreshold, run.HTMLURL, fetchErr != nil, v.CanAct(ctx, run.RepoID)))
}

// shouldRefresh reports whether a job's steps are due to be re-read from GitHub.
func (s *Server) shouldRefresh(jobID int64, now time.Time) bool {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if s.refreshed == nil {
		s.refreshed = make(map[int64]time.Time)
	}
	if now.Sub(s.refreshed[jobID]) < stepPollInterval-time.Second {
		return false
	}
	s.refreshed[jobID] = now
	for id, t := range s.refreshed {
		if now.Sub(t) > time.Hour {
			delete(s.refreshed, id)
		}
	}
	return true
}

// jobBody re-renders an expanded job, refreshing its steps from GitHub while it runs.
func (s *Server) jobBody(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	job, run, ok := s.visibleJob(w, r, v)
	if !ok {
		return
	}
	ctx := r.Context()
	now := s.Now()
	if job.Status != "completed" && s.shouldRefresh(job.ID, now) {
		if err := s.Syncer.RefreshJob(ctx, job.ID); err != nil {
			s.Log.Error("refresh job", "job", job.ID, "err", err)
		} else if job, err = s.Store.GetJob(ctx, job.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.render(w, r, jobBody(job, now, v.CanAct(ctx, run.RepoID)))
}

func (s *Server) jobLog(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	job, _, ok := s.visibleJob(w, r, v)
	if !ok {
		return
	}
	ctx := r.Context()
	raw, err := s.Syncer.JobLog(ctx, job.ID)
	switch {
	case errors.Is(err, ghapp.ErrLogUnavailable):
		s.render(w, r, logNote(job, "GitHub hasn't published this log. Logs appear a few seconds after a job finishes, and expire after the repository's log retention period."))
	case err != nil:
		s.Log.Error("job log", "job", job.ID, "err", err)
		s.render(w, r, logNote(job, "Couldn't download this log from GitHub. Check the server log for details."))
	default:
		s.render(w, r, jobLogView(job, joblog.Parse(raw, 0)))
	}
}

func (s *Server) annotations(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	job, _, ok := s.visibleJob(w, r, v)
	if !ok {
		return
	}
	anns, err := s.Syncer.Annotations(r.Context(), job.ID)
	var perm syncer.ErrPermission
	switch {
	case errors.As(err, &perm):
		s.render(w, r, noteText("Annotations need the App's "+perm.Permission+" permission."))
	case err != nil:
		s.Log.Error("annotations", "job", job.ID, "err", err)
		s.render(w, r, noteText("Couldn't load annotations from GitHub."))
	default:
		s.render(w, r, annotationList(anns, s.repoURLFor(r, job)))
	}
}

// repoURLFor returns the GitHub URL of the job's repo at its commit, for linking file:line.
func (s *Server) repoURLFor(r *http.Request, job store.Job) string {
	run, err := s.Store.GetRun(r.Context(), job.RunID)
	if err != nil {
		return ""
	}
	repo, err := s.Store.GetRepo(r.Context(), run.RepoID)
	if err != nil {
		return ""
	}
	return "https://github.com/" + repo.FullName + "/blob/" + run.HeadSHA + "/"
}

func (s *Server) workflowFile(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	run, ok := s.visibleRun(w, r, v, id)
	if !ok {
		return
	}
	content, err := s.Syncer.WorkflowFile(r.Context(), run.ID)
	var perm syncer.ErrPermission
	switch {
	case errors.As(err, &perm):
		s.render(w, r, noteText("Showing the workflow file needs the App's "+perm.Permission+" permission. Open it on GitHub instead."))
	case err != nil:
		s.Log.Error("workflow file", "run", run.ID, "err", err)
		s.render(w, r, noteText("Couldn't load the workflow file: "+err.Error()))
	default:
		s.render(w, r, codeBlock(content))
	}
}
