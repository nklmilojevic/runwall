package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/testutil"
)

const secret = "s3cret"

type fakeSyncer struct {
	mu  sync.Mutex
	ids []int64
}

func (f *fakeSyncer) TriggerInstallation(id int64) {
	f.mu.Lock()
	f.ids = append(f.ids, id)
	f.mu.Unlock()
}

func (f *fakeSyncer) triggered() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.ids...)
}

func sign(body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func post(h http.Handler, event, delivery, body, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", signature)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const repoJSON = `"repository": {"id": 10, "name": "api", "full_name": "acme/api", "owner": {"login": "acme"}, "default_branch": "main"},
	"installation": {"id": 7}`

func runPayload(status, conclusion, updated string) string {
	return `{"action": "completed", "workflow_run": {
		"id": 500, "name": "CI", "path": ".github/workflows/ci.yml", "workflow_id": 3, "run_number": 42, "run_attempt": 1,
		"event": "push", "head_branch": "main", "head_sha": "abc", "display_title": "Fix the thing\nmore",
		"status": "` + status + `", "conclusion": "` + conclusion + `",
		"created_at": "2026-10-07T10:00:00Z", "run_started_at": "2026-10-07T10:00:00Z", "updated_at": "` + updated + `",
		"html_url": "https://github.com/acme/api/actions/runs/500",
		"actor": {"login": "nkl", "type": "User"}
	}, ` + repoJSON + `}`
}

func setup(t *testing.T) (*Handler, *testutil.RecordingNotifier, *fakeSyncer) {
	t.Helper()
	ing, notifier := testutil.Ingester(t, time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	fs := &fakeSyncer{}
	h := New([]byte(secret), ing.Store, ing, fs, testutil.Logger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)
	return h, notifier, fs
}

func TestRejectsBadSignature(t *testing.T) {
	h, _, _ := setup(t)
	body := runPayload("completed", "success", "2026-10-07T10:05:00Z")
	if rec := post(h, "workflow_run", "d1", body, "sha256=deadbeef"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: got %d", rec.Code)
	}
	if rec := post(h, "workflow_run", "d1", body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature: got %d", rec.Code)
	}
}

func TestWorkflowRunFailureStoresAndNotifiesOnce(t *testing.T) {
	h, notifier, _ := setup(t)
	ctx := context.Background()

	body := runPayload("completed", "failure", "2026-10-07T10:05:00Z")
	if rec := post(h, "workflow_run", "d1", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d", rec.Code)
	}
	testutil.Eventually(t, "run stored", func() bool {
		r, err := h.store.GetRun(ctx, 500)
		return err == nil && r.Conclusion == "failure"
	})
	r, _ := h.store.GetRun(ctx, 500)
	if r.Title != "Fix the thing" || r.RunNumber != 42 || r.ActorLogin != "nkl" {
		t.Fatalf("unexpected run %+v", r)
	}
	repo, err := h.store.GetRepo(ctx, 10)
	if err != nil || repo.InstallationID != 7 || repo.DefaultBranch != "main" {
		t.Fatalf("repo %+v err %v", repo, err)
	}
	testutil.Eventually(t, "notification", func() bool { return len(notifier.Sent()) == 1 })
	if n := notifier.Sent()[0]; n.URL != "https://github.com/acme/api/actions/runs/500" || !strings.Contains(n.Title, "acme/api") {
		t.Fatalf("notification %+v", n)
	}

	// Redelivery of the same delivery is acknowledged but not reprocessed.
	if rec := post(h, "workflow_run", "d1", body, sign(body)); rec.Code != http.StatusOK {
		t.Fatalf("duplicate: got %d", rec.Code)
	}
	// A distinct delivery for the same failure does not notify again.
	post(h, "workflow_run", "d2", body, sign(body))
	time.Sleep(50 * time.Millisecond)
	if got := len(notifier.Sent()); got != 1 {
		t.Fatalf("expected exactly one notification, got %d", got)
	}
}

func TestOutOfOrderEventsKeepNewestState(t *testing.T) {
	h, _, _ := setup(t)
	ctx := context.Background()
	done := runPayload("completed", "success", "2026-10-07T10:05:00Z")
	started := runPayload("in_progress", "", "2026-10-07T10:01:00Z")
	post(h, "workflow_run", "d1", done, sign(done))
	post(h, "workflow_run", "d2", started, sign(started))
	testutil.Eventually(t, "both processed", func() bool { return len(h.queue) == 0 })
	time.Sleep(20 * time.Millisecond)
	r, err := h.store.GetRun(ctx, 500)
	if err != nil || r.Status != "completed" {
		t.Fatalf("got %+v err %v", r, err)
	}
}

func TestWorkflowJobAndInstallationEvents(t *testing.T) {
	h, _, fs := setup(t)
	ctx := context.Background()

	job := `{"action": "queued", "workflow_job": {"id": 900, "run_id": 500, "run_attempt": 1, "name": "build",
		"status": "queued", "labels": ["self-hosted", "linux"], "created_at": "2026-10-07T10:00:00Z"}, ` + repoJSON + `}`
	if rec := post(h, "workflow_job", "j1", job, sign(job)); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d", rec.Code)
	}
	testutil.Eventually(t, "job stored", func() bool {
		jobs, err := h.store.ListJobs(ctx, []store.Run{{ID: 500, RunAttempt: 1}})
		return err == nil && len(jobs[500]) == 1 && jobs[500][0].Labels[0] == "self-hosted"
	})

	added := `{"action": "added", "repositories_added": [{"id": 11, "full_name": "acme/web"}], "repositories_removed": [{"id": 10}], "installation": {"id": 7}}`
	post(h, "installation_repositories", "i1", added, sign(added))
	testutil.Eventually(t, "sync triggered and repo removed", func() bool {
		r, _ := h.store.ListRepos(ctx, 7)
		return len(fs.triggered()) == 1 && fs.triggered()[0] == 7 && len(r) == 0
	})

	created := `{"action": "created", "installation": {"id": 8, "account": {"login": "other", "type": "Organization"}}}`
	post(h, "installation", "i2", created, sign(created))
	testutil.Eventually(t, "installation stored", func() bool {
		insts, _ := h.store.ListInstallations(ctx)
		return len(insts) == 1 && insts[0].Account == "other" && len(fs.triggered()) == 2
	})

	deleted := `{"action": "deleted", "installation": {"id": 8, "account": {"login": "other"}}}`
	post(h, "installation", "i3", deleted, sign(deleted))
	testutil.Eventually(t, "installation removed", func() bool {
		insts, _ := h.store.ListInstallations(ctx)
		return len(insts) == 0
	})
}

func TestUnknownEventIsAcknowledged(t *testing.T) {
	h, _, _ := setup(t)
	body := `{"zen": "hi"}`
	if rec := post(h, "some_future_event", "u1", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d", rec.Code)
	}
}
