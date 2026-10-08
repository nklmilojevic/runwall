package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr     string
	DBPath         string
	AppID          int64
	PrivateKey     []byte
	WebhookSecret  []byte
	APIURL         string
	StuckThreshold time.Duration
	Backfill       time.Duration
	Reconcile      time.Duration
	Retention      time.Duration
	ColdInterval   time.Duration
	Concurrency    int
	JobBackfill    int
	Notifier       string
	LogLevel       string

	// Sign-in (GitHub App user authorization)
	ClientID     string
	ClientSecret string
	SessionKey   string
	BaseURL      string // external URL of the dashboard, used for the OAuth callback and links
	WebURL       string

	AllowedUsers    []string // if set, only these GitHub logins may sign in
	AllowedAccounts []string // if set, only installations on these accounts are used
	Admins          []string // defaults to the App's owner
	CostRatesFile   string
}

func list(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Load reads configuration from the environment. Secrets are expected to be injected,
// e.g. with `op run`, never written to disk.
func Load(getenv func(string) string, needGitHub bool) (Config, error) {
	defaultNotifier := "log"
	if runtime.GOOS == "darwin" {
		defaultNotifier = "macos"
	}
	c := Config{
		ListenAddr: or(getenv("LISTEN_ADDR"), "127.0.0.1:8080"),
		DBPath:     or(getenv("DB_PATH"), "runwall.db"),
		APIURL:     or(getenv("GITHUB_API_URL"), "https://api.github.com"),
		Notifier:   or(getenv("NOTIFIER"), defaultNotifier),
		LogLevel:   or(getenv("LOG_LEVEL"), "info"),
		WebURL:     or(getenv("GITHUB_WEB_URL"), "https://github.com"),

		ClientID:        getenv("GITHUB_CLIENT_ID"),
		ClientSecret:    getenv("GITHUB_CLIENT_SECRET"),
		SessionKey:      getenv("SESSION_KEY"),
		AllowedUsers:    list(getenv("ALLOWED_USERS")),
		AllowedAccounts: list(getenv("ALLOWED_ACCOUNTS")),
		Admins:          list(getenv("ADMIN_USERS")),
		CostRatesFile:   getenv("COST_RATES_FILE"),
	}
	c.BaseURL = strings.TrimRight(or(getenv("BASE_URL"), defaultBaseURL(c.ListenAddr)), "/")
	var errs []error
	dur := func(key string, def time.Duration) time.Duration {
		v := getenv(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: want a positive duration like 5m, got %q", key, v))
			return def
		}
		return d
	}
	c.StuckThreshold = dur("STUCK_THRESHOLD", 5*time.Minute)
	c.Backfill = dur("BACKFILL_WINDOW", 7*24*time.Hour)
	c.Reconcile = dur("RECONCILE_INTERVAL", 3*time.Minute)
	c.Retention = dur("RETENTION", 90*24*time.Hour)
	c.ColdInterval = dur("COLD_INTERVAL", time.Hour)
	num := func(key string, def int) int {
		v := getenv(key)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s: want a non-negative number, got %q", key, v))
			return def
		}
		return n
	}
	c.Concurrency = num("SYNC_CONCURRENCY", 4)
	c.JobBackfill = num("JOB_BACKFILL", 200)

	if !needGitHub {
		return c, errors.Join(errs...)
	}

	if v := getenv("GITHUB_APP_ID"); v == "" {
		errs = append(errs, errors.New("GITHUB_APP_ID is required"))
	} else if id, err := strconv.ParseInt(v, 10, 64); err != nil {
		errs = append(errs, fmt.Errorf("GITHUB_APP_ID: %w", err))
	} else {
		c.AppID = id
	}

	switch key, file := getenv("GITHUB_APP_PRIVATE_KEY"), getenv("GITHUB_APP_PRIVATE_KEY_FILE"); {
	case key != "":
		c.PrivateKey = []byte(key)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			errs = append(errs, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: %w", err))
		}
		c.PrivateKey = b
	default:
		errs = append(errs, errors.New("GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE is required"))
	}

	for key, v := range map[string]string{"GITHUB_CLIENT_ID": c.ClientID, "GITHUB_CLIENT_SECRET": c.ClientSecret, "SESSION_KEY": c.SessionKey} {
		if v == "" {
			errs = append(errs, errors.New(key+" is required for GitHub sign-in"))
		}
	}

	if v := getenv("GITHUB_WEBHOOK_SECRET"); v == "" {
		errs = append(errs, errors.New("GITHUB_WEBHOOK_SECRET is required"))
	} else {
		c.WebhookSecret = []byte(v)
	}
	return c, errors.Join(errs...)
}

// defaultBaseURL derives the dashboard's URL from its listen address.
func defaultBaseURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
