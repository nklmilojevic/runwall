// Package testutil holds fakes shared by tests.
package testutil

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/nklmilojevic/runwall/internal/hub"
	"github.com/nklmilojevic/runwall/internal/ingest"
	"github.com/nklmilojevic/runwall/internal/notify"
	"github.com/nklmilojevic/runwall/internal/store"
)

type RecordingNotifier struct {
	mu   sync.Mutex
	sent []notify.Notification
}

func (r *RecordingNotifier) Notify(_ context.Context, n notify.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, n)
	return nil
}

func (r *RecordingNotifier) Sent() []notify.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Notification(nil), r.sent...)
}

func Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func Store(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func Ingester(t *testing.T, since time.Time) (*ingest.Ingester, *RecordingNotifier) {
	t.Helper()
	n := &RecordingNotifier{}
	return &ingest.Ingester{Store: Store(t), Hub: hub.New(), Notifier: n, Log: Logger(), Since: since}, n
}

// Eventually polls cond until it holds or the deadline passes.
func Eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
