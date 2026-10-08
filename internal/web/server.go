package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/nklmilojevic/runwall/internal/auth"
	"github.com/nklmilojevic/runwall/internal/cost"
	"github.com/nklmilojevic/runwall/internal/ghapp"
	"github.com/nklmilojevic/runwall/internal/hub"
	"github.com/nklmilojevic/runwall/internal/metrics"
	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/syncer"
)

//go:embed static
var staticFS embed.FS

type Syncer interface {
	FetchJobsForRun(ctx context.Context, runID int64) error
	RefreshJob(ctx context.Context, jobID int64) error
	RefreshRun(ctx context.Context, runID int64) error
	JobLog(ctx context.Context, jobID int64) (string, error)
	Annotations(ctx context.Context, jobID int64) ([]syncer.Annotation, error)
	WorkflowFile(ctx context.Context, runID int64) (string, error)
	LastSync() (time.Time, error)
	Budget(ctx context.Context) (ghapp.Rate, bool)
	TriggerAll()
}

type Scorer interface {
	Score(ctx context.Context, r store.Repo) (store.RepoScore, error)
}

// stepPollInterval is how often an expanded, running job polls for step progress.
// Refreshes from GitHub are shared between viewers within this window.
const stepPollInterval = 5 * time.Second

type Server struct {
	Store          *store.Store
	Hub            *hub.Hub
	Syncer         Syncer
	Scorer         Scorer
	Auth           *auth.Auth // nil in demo mode
	Demo           bool
	Costs          cost.Table
	StuckThreshold time.Duration
	BaseURL        string
	Log            *slog.Logger
	Now            func() time.Time

	refreshMu sync.Mutex
	refreshed map[int64]time.Time
}

// Routes registers the UI. The webhook and MCP endpoints are mounted separately.
func (s *Server) Routes(mux *http.ServeMux) {
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticCache(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("GET /auth/start", s.authStart)
	mux.HandleFunc("GET /auth/callback", s.authCallback)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /kiosk/{token}", s.kiosk)

	mux.HandleFunc("GET /{$}", s.protect(s.dashboard))
	mux.HandleFunc("GET /runs", s.protect(s.runs))
	mux.HandleFunc("GET /runs/{id}", s.protect(s.runDetail))
	mux.HandleFunc("GET /runs/{id}/jobs", s.protect(s.jobs))
	mux.HandleFunc("GET /runs/{id}/workflow-file", s.protect(s.workflowFile))
	mux.HandleFunc("GET /jobs/{id}/body", s.protect(s.jobBody))
	mux.HandleFunc("GET /jobs/{id}/log", s.protect(s.jobLog))
	mux.HandleFunc("GET /jobs/{id}/annotations", s.protect(s.annotations))
	mux.HandleFunc("GET /workflows", s.protect(s.workflows))
	mux.HandleFunc("GET /workflows/{repo}/{workflow}", s.protect(s.workflowDetail))
	mux.HandleFunc("GET /repos", s.protect(s.repos))
	mux.HandleFunc("GET /repos/{id}", s.protect(s.repoDetail))
	mux.HandleFunc("GET /settings", s.protect(s.settings))
	mux.HandleFunc("GET /events", s.protect(s.events))

	mux.HandleFunc("POST /sync", s.mutate(s.sync))
	mux.HandleFunc("POST /runs/{id}/rerun", s.mutate(s.runAction(actionRerun)))
	mux.HandleFunc("POST /runs/{id}/rerun-failed", s.mutate(s.runAction(actionRerunFailed)))
	mux.HandleFunc("POST /runs/{id}/cancel", s.mutate(s.runAction(actionCancel)))
	mux.HandleFunc("POST /jobs/{id}/rerun", s.mutate(s.jobRerun))
	mux.HandleFunc("POST /repos/{id}/score", s.mutate(s.rescore))
	mux.HandleFunc("POST /settings/tokens", s.mutate(s.createToken))
	mux.HandleFunc("POST /settings/tokens/{id}/delete", s.mutate(s.deleteToken))
	mux.HandleFunc("POST /settings/kiosk", s.mutate(s.createKiosk))
	mux.HandleFunc("POST /settings/kiosk/{id}/delete", s.mutate(s.deleteKiosk))
}

// assetVersions holds a short content hash per static file, computed once at startup.
var assetVersions = func() map[string]string {
	out := map[string]string{}
	fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[strings.TrimPrefix(path, "static/")] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	return out
}()

// asset returns a static file's URL with its content hash, so a changed file always gets a new URL.
func asset(name string) string {
	if v, ok := assetVersions[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

// staticCache lets browsers keep versioned assets forever; unversioned requests revalidate.
func staticCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

// Viewers and guards

type handler func(w http.ResponseWriter, r *http.Request, v *auth.Viewer)

func (s *Server) viewer(w http.ResponseWriter, r *http.Request) *auth.Viewer {
	if s.Demo {
		return auth.DemoViewer()
	}
	if s.Auth == nil {
		return nil
	}
	return s.Auth.Load(w, r)
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// protect requires a signed-in user or a kiosk session.
func (s *Server) protect(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := s.viewer(w, r)
		if v == nil {
			login := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
			switch {
			case isHTMX(r):
				w.Header().Set("HX-Redirect", login)
				w.WriteHeader(http.StatusUnauthorized)
			case r.Method == http.MethodGet && r.URL.Path != "/events":
				http.Redirect(w, r, login, http.StatusFound)
			default:
				http.Error(w, "Sign in to continue.", http.StatusUnauthorized)
			}
			return
		}
		h(w, r.WithContext(auth.WithViewer(r.Context(), v)), v)
	}
}

// mutate guards state-changing requests: a signed-in user (not a kiosk) and a valid CSRF token.
func (s *Server) mutate(h handler) http.HandlerFunc {
	return s.protect(func(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
		if v.Kiosk != nil {
			http.Error(w, "Kiosk links are read-only.", http.StatusForbidden)
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			token = r.PostFormValue("csrf")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(v.CSRF)) != 1 {
			http.Error(w, "This form expired. Reload the page and try again.", http.StatusForbidden)
			return
		}
		h(w, r, v)
	})
}

// Shared page context

type shell struct {
	V        *auth.Viewer
	Active   string
	Title    string
	Period   metrics.Period
	Now      time.Time
	LastSync time.Time
	SyncErr  error
	Budget   ghapp.Rate
	HasRate  bool
	Demo     bool
}

func periodOf(r *http.Request) metrics.Period {
	if p := r.URL.Query().Get("period"); p != "" {
		return metrics.ParsePeriod(p)
	}
	if c, err := r.Cookie("period"); err == nil {
		return metrics.ParsePeriod(c.Value)
	}
	return metrics.ParsePeriod("")
}

func (s *Server) shell(r *http.Request, v *auth.Viewer, active, title string) shell {
	sh := shell{V: v, Active: active, Title: title, Period: periodOf(r), Now: s.Now(), Demo: s.Demo}
	sh.LastSync, sh.SyncErr = s.Syncer.LastSync()
	sh.Budget, sh.HasRate = s.Syncer.Budget(r.Context())
	return sh
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "HX-Request, HX-Target")
	if err := c.Render(r.Context(), w); err != nil {
		s.Log.Error("render", "path", r.URL.Path, "err", err)
	}
}

// toast asks the page to show a message after an htmx request.
func toast(w http.ResponseWriter, kind, msg string) {
	b, _ := json.Marshal(map[string]any{"toast": map[string]string{"kind": kind, "message": msg}})
	w.Header().Set("HX-Trigger", string(b))
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

// visibleRun loads a run the viewer may see, or writes a 404.
func (s *Server) visibleRun(w http.ResponseWriter, r *http.Request, v *auth.Viewer, id int64) (store.Run, bool) {
	run, err := s.Store.GetRun(r.Context(), id)
	if err == nil {
		if ok, _ := s.Store.CanSeeRepo(r.Context(), v.Scope, run.RepoID); ok {
			return run, true
		}
	}
	http.NotFound(w, r)
	return store.Run{}, false
}

func (s *Server) visibleJob(w http.ResponseWriter, r *http.Request, v *auth.Viewer) (store.Job, store.Run, bool) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return store.Job{}, store.Run{}, false
	}
	job, err := s.Store.GetJob(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return job, store.Run{}, false
	}
	run, ok := s.visibleRun(w, r, v, job.RunID)
	return job, run, ok
}

// events streams a tick whenever a run the viewer can see changes; pages re-fetch their own view.
func (s *Server) events(w http.ResponseWriter, r *http.Request, v *auth.Viewer) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := s.Hub.Subscribe()
	defer unsubscribe()
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	ctx := r.Context()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-ch:
			if !ok {
				return
			}
			if run, err := s.Store.GetRun(ctx, id); err == nil {
				if visible, _ := s.Store.CanSeeRepo(ctx, v.Scope, run.RepoID); !visible {
					continue
				}
			}
			fmt.Fprintf(w, "event: update\ndata: %d\n\n", id)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("request", "err", err)
	http.Error(w, "Something went wrong reading the local database. Check the server log for details.", http.StatusInternalServerError)
}
