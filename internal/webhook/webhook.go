// Package webhook receives GitHub App deliveries: it verifies, de-duplicates and
// acknowledges them immediately, then processes them in order on a background worker.
package webhook

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/nklmilojevic/runwall/internal/ingest"
	"github.com/nklmilojevic/runwall/internal/store"
)

// Syncer is the part of the reconciler that installation events need.
type Syncer interface {
	TriggerInstallation(id int64)
}

type Handler struct {
	// Allowed, if set, limits which installation accounts are accepted. Events from
	// installations the dashboard doesn't know are then dropped too.
	Allowed func(account string) bool

	secret []byte
	store  *store.Store
	ing    *ingest.Ingester
	sync   Syncer
	log    *slog.Logger
	queue  chan delivery
}

type delivery struct {
	id    string
	kind  string
	event any
}

func New(secret []byte, st *store.Store, ing *ingest.Ingester, sync Syncer, log *slog.Logger) *Handler {
	return &Handler{secret: secret, store: st, ing: ing, sync: sync, log: log, queue: make(chan delivery, 1024)}
}

// Run processes queued deliveries until ctx is done.
func (h *Handler) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-h.queue:
			if err := h.process(ctx, d); err != nil {
				h.log.Error("webhook: process", "delivery", d.id, "event", d.kind, "err", err)
			}
		}
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	payload, err := github.ValidatePayload(r, h.secret)
	if err != nil {
		h.log.Warn("webhook: rejected", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	kind := github.WebHookType(r)
	id := github.DeliveryID(r)
	if id == "" {
		http.Error(w, "missing delivery id", http.StatusBadRequest)
		return
	}

	event, err := github.ParseWebHook(kind, payload)
	if err != nil {
		// Unknown or unsubscribed event types are acknowledged and ignored.
		h.log.Debug("webhook: ignored", "event", kind, "err", err)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	fresh, err := h.store.RecordDelivery(r.Context(), id, time.Now())
	if err != nil {
		h.log.Error("webhook: record delivery", "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !fresh {
		w.WriteHeader(http.StatusOK)
		return
	}

	select {
	case h.queue <- delivery{id: id, kind: kind, event: event}:
		w.WriteHeader(http.StatusAccepted)
	default:
		// The reconciler will pick the change up on its next pass.
		h.log.Error("webhook: queue full, dropping", "delivery", id, "event", kind)
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}
}

// known reports whether events for an installation should be processed.
func (h *Handler) known(ctx context.Context, installationID int64) bool {
	if h.Allowed == nil {
		return true
	}
	insts, err := h.store.ListInstallations(ctx)
	if err != nil {
		return false
	}
	for _, in := range insts {
		if in.ID == installationID {
			return true
		}
	}
	return false
}

func (h *Handler) process(ctx context.Context, d delivery) error {
	switch e := d.event.(type) {
	case *github.WorkflowRunEvent, *github.WorkflowJobEvent, *github.RepositoryEvent, *github.InstallationRepositoriesEvent:
		var id int64
		switch e := e.(type) {
		case *github.WorkflowRunEvent:
			id = e.GetInstallation().GetID()
		case *github.WorkflowJobEvent:
			id = e.GetInstallation().GetID()
		case *github.RepositoryEvent:
			id = e.GetInstallation().GetID()
		case *github.InstallationRepositoriesEvent:
			id = e.GetInstallation().GetID()
		}
		if !h.known(ctx, id) {
			h.log.Warn("webhook: ignoring event from unknown or disallowed installation", "installation", id, "event", d.kind)
			return nil
		}
	case *github.InstallationEvent:
		if h.Allowed != nil && !h.Allowed(e.GetInstallation().GetAccount().GetLogin()) {
			h.log.Warn("webhook: ignoring installation on disallowed account", "account", e.GetInstallation().GetAccount().GetLogin())
			return nil
		}
	}
	switch e := d.event.(type) {
	case *github.PingEvent:
		h.log.Info("webhook: ping", "zen", e.GetZen())

	case *github.WorkflowRunEvent:
		repo := ingest.RepoFromGitHub(e.GetRepo(), e.GetInstallation().GetID())
		if err := h.ing.Repo(ctx, repo); err != nil {
			return err
		}
		return h.ing.Run(ctx, ingest.RunFromGitHub(e.GetWorkflowRun(), repo.ID))

	case *github.WorkflowJobEvent:
		if err := h.ing.Repo(ctx, ingest.RepoFromGitHub(e.GetRepo(), e.GetInstallation().GetID())); err != nil {
			return err
		}
		return h.ing.Job(ctx, ingest.JobFromGitHub(e.GetWorkflowJob()))

	case *github.InstallationEvent:
		inst := e.GetInstallation()
		switch e.GetAction() {
		case "deleted", "suspend":
			h.log.Info("installation removed", "installation", inst.GetID(), "account", inst.GetAccount().GetLogin())
			return h.store.DeleteInstallation(ctx, inst.GetID())
		default:
			if err := h.store.UpsertInstallation(ctx, store.Installation{
				ID: inst.GetID(), Account: inst.GetAccount().GetLogin(), AccountType: inst.GetAccount().GetType(),
			}); err != nil {
				return err
			}
			h.sync.TriggerInstallation(inst.GetID())
		}

	case *github.InstallationRepositoriesEvent:
		var removed []int64
		for _, r := range e.RepositoriesRemoved {
			removed = append(removed, r.GetID())
		}
		if err := h.store.DeleteRepos(ctx, removed); err != nil {
			return err
		}
		if len(e.RepositoriesAdded) > 0 {
			h.sync.TriggerInstallation(e.GetInstallation().GetID())
		}

	case *github.RepositoryEvent:
		if e.GetAction() == "deleted" {
			return h.store.DeleteRepos(ctx, []int64{e.GetRepo().GetID()})
		}
		return h.ing.Repo(ctx, ingest.RepoFromGitHub(e.GetRepo(), e.GetInstallation().GetID()))
	}
	return nil
}
