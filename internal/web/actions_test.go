package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/google/go-github/v92/github"
)

func TestCancelRunForcesOnlyJoblessConflicts(t *testing.T) {
	cases := []struct {
		name       string
		cancel     int // status GitHub answers the normal cancel with
		jobs       int
		wantForced bool
		wantErr    bool
		wantCalls  []string
	}{
		{"accepted", http.StatusAccepted, 0, false, false, []string{"cancel"}},
		{"ghost run", http.StatusConflict, 0, true, false, []string{"cancel", "jobs", "force-cancel"}},
		{"conflict with jobs", http.StatusConflict, 2, false, true, []string{"cancel", "jobs"}},
		{"forbidden", http.StatusForbidden, 0, false, true, []string{"cancel"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			mux := http.NewServeMux()
			record := func(name string, status int, body string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					calls = append(calls, name)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					w.Write([]byte(body))
				}
			}
			mux.HandleFunc("POST /repos/acme/api/actions/runs/7/cancel", record("cancel", c.cancel, `{"message":"Cannot cancel a workflow run that is completed."}`))
			mux.HandleFunc("GET /repos/acme/api/actions/runs/7/jobs", record("jobs", http.StatusOK, `{"total_count":`+strconv.Itoa(c.jobs)+`,"jobs":[]}`))
			mux.HandleFunc("POST /repos/acme/api/actions/runs/7/force-cancel", record("force-cancel", http.StatusAccepted, `{}`))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			base := srv.URL + "/"
			gh, err := github.NewClient(github.WithURLs(&base, nil))
			if err != nil {
				t.Fatal(err)
			}

			forced, err := cancelRun(context.Background(), gh, "acme", "api", 7)
			if forced != c.wantForced || (err != nil) != c.wantErr {
				t.Fatalf("forced=%v err=%v", forced, err)
			}
			if !slices.Equal(calls, c.wantCalls) {
				t.Fatalf("calls %v, want %v", calls, c.wantCalls)
			}
		})
	}
}
