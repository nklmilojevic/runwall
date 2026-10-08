package ghapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestTransport(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	b := &budgets{m: map[int64]Rate{}}
	tr := &transport{next: http.DefaultTransport, limiter: rate.NewLimiter(rate.Inf, 0), budgets: b, id: 7}
	client := &http.Client{Transport: tr}

	cond := &Cond{}
	req, _ := http.NewRequestWithContext(Conditional(context.Background(), cond), http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || cond.NewETag != `"abc"` {
		t.Fatalf("first request: %d %q", resp.StatusCode, cond.NewETag)
	}

	cond = &Cond{ETag: `"abc"`}
	req, _ = http.NewRequestWithContext(Conditional(context.Background(), cond), http.MethodGet, srv.URL, nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional request should get 304, got %d", resp.StatusCode)
	}

	r, ok := b.get(7)
	if !ok || r.Limit != 5000 || r.Remaining != 4321 || r.Reset.Unix() != reset {
		t.Fatalf("budget not recorded: %+v", r)
	}
	if !r.Healthy(0.2) || r.Healthy(0.9) {
		t.Fatal("healthy thresholds")
	}
	if !(Rate{Limit: 5000, Remaining: 0, Reset: time.Now().Add(-time.Minute)}).Healthy(0.5) {
		t.Fatal("a budget past its reset time counts as healthy")
	}
}
