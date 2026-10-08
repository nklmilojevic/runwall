package ingest

import (
	"testing"
	"time"

	"github.com/google/go-github/v92/github"
)

func TestRunFromGitHub(t *testing.T) {
	ts := github.Timestamp{Time: time.Date(2026, 10, 7, 10, 0, 0, 500, time.FixedZone("CEST", 7200))}
	r := RunFromGitHub(&github.WorkflowRun{
		ID:              github.Ptr(int64(1)),
		DisplayTitle:    github.Ptr("  First line\nsecond"),
		HeadCommit:      &github.HeadCommit{Message: github.Ptr("commit\nbody")},
		Actor:           &github.User{Login: github.Ptr("someone")},
		TriggeringActor: &github.User{Login: github.Ptr("renovate[bot]"), Type: github.Ptr("Bot")},
		PullRequests:    []*github.PullRequest{{Number: github.Ptr(12)}},
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
		{Login: github.Ptr("dependabot[bot]")}:                   true,
		{Login: github.Ptr("ci-app"), Type: github.Ptr("Bot")}:   true,
		{Login: github.Ptr("octocat"), Type: github.Ptr("User")}: false,
		nil: false,
	}
	for u, want := range cases {
		if got := IsBot(u); got != want {
			t.Errorf("IsBot(%v) = %v", u.GetLogin(), got)
		}
	}
}
