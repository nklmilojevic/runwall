package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaultsInDemoMode(t *testing.T) {
	c, err := Load(env(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != "127.0.0.1:8080" || c.StuckThreshold != 5*time.Minute || c.Backfill != 7*24*time.Hour || c.Retention != 90*24*time.Hour || c.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected defaults %+v", c)
	}
}

func TestRequiresGitHubSettings(t *testing.T) {
	_, err := Load(env(map[string]string{"STUCK_THRESHOLD": "soon"}), true)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_WEBHOOK_SECRET", "STUCK_THRESHOLD", "GITHUB_CLIENT_ID", "SESSION_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestLoadsGitHubSettings(t *testing.T) {
	c, err := Load(env(map[string]string{
		"GITHUB_APP_ID": "123", "GITHUB_APP_PRIVATE_KEY": "pem", "GITHUB_WEBHOOK_SECRET": "s", "STUCK_THRESHOLD": "10m",
		"GITHUB_CLIENT_ID": "cid", "GITHUB_CLIENT_SECRET": "cs", "SESSION_KEY": "k", "LISTEN_ADDR": ":8080",
		"ALLOWED_ACCOUNTS": "acme, octocat ,", "BASE_URL": "https://wall.example.com/",
	}), true)
	if err != nil {
		t.Fatal(err)
	}
	if c.AppID != 123 || string(c.PrivateKey) != "pem" || string(c.WebhookSecret) != "s" || c.StuckThreshold != 10*time.Minute {
		t.Fatalf("got %+v", c)
	}
	if len(c.AllowedAccounts) != 2 || c.AllowedAccounts[1] != "octocat" || c.BaseURL != "https://wall.example.com" {
		t.Fatalf("lists/base url: %+v", c)
	}
	if defaultBaseURL(":8080") != "http://localhost:8080" {
		t.Fatal("listen on all interfaces should default to localhost")
	}
}
