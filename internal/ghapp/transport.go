package ghapp

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Rate is the primary rate-limit budget GitHub last reported for an installation.
type Rate struct {
	Limit     int
	Remaining int
	Reset     time.Time
	Seen      time.Time
}

// Healthy reports whether more than the given fraction of the budget is left.
// An unknown budget counts as healthy.
func (r Rate) Healthy(fraction float64) bool {
	if r.Limit == 0 || time.Now().After(r.Reset) {
		return true
	}
	return float64(r.Remaining) > fraction*float64(r.Limit)
}

// Cond carries an ETag into a GET request and the response's ETag back out.
// A 304 answer means nothing changed, and GitHub doesn't count it against the rate limit.
type Cond struct {
	ETag    string // sent as If-None-Match when set
	NewETag string // filled from the response
}

type condKey struct{}

// Conditional attaches c to the request context; the transport applies it.
func Conditional(ctx context.Context, c *Cond) context.Context {
	return context.WithValue(ctx, condKey{}, c)
}

// budgets tracks rate limits per installation.
type budgets struct {
	mu sync.Mutex
	m  map[int64]Rate
}

func (b *budgets) set(id int64, r Rate) {
	b.mu.Lock()
	b.m[id] = r
	b.mu.Unlock()
}

func (b *budgets) get(id int64) (Rate, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.m[id]
	return r, ok
}

// transport throttles requests (GitHub's secondary limits allow 900 points a minute),
// applies conditional requests and records the rate-limit headers.
type transport struct {
	next    http.RoundTripper
	limiter *rate.Limiter
	budgets *budgets
	id      int64 // installation ID, 0 for App-level calls
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.limiter.Wait(req.Context()); err != nil {
		return nil, err
	}
	c, _ := req.Context().Value(condKey{}).(*Cond)
	if c != nil && c.ETag != "" && req.Method == http.MethodGet {
		req = req.Clone(req.Context())
		req.Header.Set("If-None-Match", c.ETag)
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if c != nil {
		c.NewETag = resp.Header.Get("ETag")
	}
	if t.id != 0 {
		if limit, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit")); err == nil && limit > 0 {
			remaining, _ := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
			reset, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
			t.budgets.set(t.id, Rate{Limit: limit, Remaining: remaining, Reset: time.Unix(reset, 0), Seen: time.Now()})
		}
	}
	return resp, nil
}

// Transport wraps next with conditional-request support and no throttling; for clients
// built outside App, such as in tests.
func Transport(next http.RoundTripper) http.RoundTripper {
	return &transport{next: next, limiter: rate.NewLimiter(rate.Inf, 0), budgets: &budgets{m: map[int64]Rate{}}}
}
