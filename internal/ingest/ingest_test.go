package ingest

import (
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
)

func TestRunFromGitHub(t *testing.T) {
	ts := github.Timestamp{Time: time.Date(2026, 10, 7, 10, 0, 0, 500, time.FixedZone("CEST", 7200))}
	r := RunFromGitHub(&github.WorkflowRun{
		ID:              new(int64(1)),
		DisplayTitle:    new("  First line\nsecond"),
		HeadCommit:      &github.HeadCommit{Message: new("commit\nbody")},
		Actor:           &github.User{Login: new("someone")},
		TriggeringActor: &github.User{Login: new("renovate[bot]"), Type: new("Bot")},
		PullRequests:    []*github.PullRequest{{Number: new(12)}},
		CreatedAt:       &ts,
	}, 9)
	if r.Title != "First line" || r.CommitMessage != "commit" {
		t.Fatalf("titles: %q %q", r.Title, r.CommitMessage)
	}
	if r.ActorLogin != "renovate[bot]" || !r.ActorIsBot {
		t.Fatal("triggering actor should win and be detected as a bot")
	}
	if r.PRNumber != 12 || r.RepoID != 9 || r.RunAttempt != 1 {
		t.Fatalf("got %+v", r)
	}
	if r.CreatedAt.Location() != time.UTC || r.CreatedAt.Nanosecond() != 0 {
		t.Fatalf("timestamps should be UTC seconds: %v", r.CreatedAt)
	}
}

func TestIsBot(t *testing.T) {
	cases := map[*github.User]bool{
		{Login: new("dependabot[bot]")}:            true,
		{Login: new("ci-app"), Type: new("Bot")}:   true,
		{Login: new("octocat"), Type: new("User")}: false,
		nil: false,
	}
	for u, want := range cases {
		if got := IsBot(u); got != want {
			t.Errorf("IsBot(%v) = %v", u.GetLogin(), got)
		}
	}
}
