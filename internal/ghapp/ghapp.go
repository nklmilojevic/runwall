// Package ghapp builds GitHub API clients that authenticate as the App or as one of its installations.
package ghapp

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v92/github"
	"golang.org/x/time/rate"
)

// ErrLogUnavailable means GitHub has no log for a job: it is still running, finished
// moments ago, or the log has expired.
var ErrLogUnavailable = errors.New("log not available")

type Clients interface {
	App() *github.Client
	Installation(id int64) *github.Client
}

// Budgeter is implemented by clients that know each installation's rate-limit budget.
type Budgeter interface {
	Budget(installationID int64) (Rate, bool)
}

// requestsPerSecond keeps well inside GitHub's secondary limit of 900 points a minute.
const requestsPerSecond = 10

type App struct {
	apps    *ghinstallation.AppsTransport
	baseURL string
	app     *github.Client
	limiter *rate.Limiter
	budgets *budgets

	mu    sync.Mutex
	insts map[int64]*github.Client
}

// New creates clients for the App. baseURL is the API root, e.g. https://api.github.com.
func New(appID int64, privateKey []byte, baseURL string) (*App, error) {
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, privateKey)
	if err != nil {
		return nil, err
	}
	baseURL = strings.TrimRight(baseURL, "/")
	atr.BaseURL = baseURL
	a := &App{apps: atr, baseURL: baseURL, insts: make(map[int64]*github.Client),
		limiter: rate.NewLimiter(requestsPerSecond, requestsPerSecond), budgets: &budgets{m: map[int64]Rate{}}}
	a.app, err = a.client(&transport{next: atr, limiter: a.limiter, budgets: a.budgets})
	return a, err
}

// Budget returns the rate-limit budget GitHub last reported for an installation.
func (a *App) Budget(installationID int64) (Rate, bool) { return a.budgets.get(installationID) }

func (a *App) client(tr http.RoundTripper) (*github.Client, error) {
	base := a.baseURL + "/"
	return github.NewClient(github.WithTransport(tr), github.WithURLs(&base, nil))
}

func (a *App) App() *github.Client { return a.app }

// Installation returns a client whose tokens are cached and refreshed by ghinstallation.
func (a *App) Installation(id int64) *github.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.insts[id]; ok {
		return c
	}
	tr := &transport{next: ghinstallation.NewFromAppsTransport(a.apps, id), limiter: a.limiter, budgets: a.budgets, id: id}
	c, _ := a.client(tr) // baseURL already parsed successfully in New
	a.insts[id] = c
	return c
}
