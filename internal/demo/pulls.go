package demo

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/nklmilojevic/runwall/internal/pulls"
	"github.com/nklmilojevic/runwall/internal/store"
)

// Pulls stands in for GitHub's pull requests. Merged ones stay gone until a restart.
type Pulls struct {
	At time.Time

	mu     sync.Mutex
	merged map[string]bool
	auto   map[string]bool
}

type demoPR struct {
	repo, author, title, head string
	bot, draft                bool
	mergeable, state          string
	review, checks            string
	age                       time.Duration
}

var demoPRs = []demoPR{
	{repo: "acme/api", author: "renovate", bot: true, title: "fix(deps): update module golang.org/x/net to v0.48.0", head: "renovate/golang.org-x-net-0.x", mergeable: "MERGEABLE", state: "CLEAN", checks: "SUCCESS", age: 26 * time.Hour},
	{repo: "acme/api", author: "renovate", bot: true, title: "chore(deps): lock file maintenance", head: "renovate/lock-file-maintenance", mergeable: "MERGEABLE", state: "CLEAN", checks: "SUCCESS", age: 7 * time.Hour},
	{repo: "acme/api", author: "monalisa", title: "Rework subscriber import batching", head: "feat/batch-import", mergeable: "MERGEABLE", state: "BLOCKED", review: "REVIEW_REQUIRED", checks: "SUCCESS", age: 3 * 24 * time.Hour},
	{repo: "acme/web-app", author: "renovate", bot: true, title: "ci(github-action): update actions/setup-node action to v5", head: "renovate/actions-setup-node-5.x", mergeable: "MERGEABLE", state: "CLEAN", checks: "SUCCESS", age: 2 * 24 * time.Hour},
	{repo: "acme/web-app", author: "dependabot", bot: true, title: "Bump vite from 6.3.1 to 6.4.0", head: "dependabot/npm_and_yarn/vite-6.4.0", mergeable: "CONFLICTING", state: "DIRTY", checks: "SUCCESS", age: 9 * 24 * time.Hour},
	{repo: "acme/web-app", author: "octocat", title: "Speed up e2e shard split", head: "perf/e2e-shards", mergeable: "MERGEABLE", state: "UNSTABLE", checks: "PENDING", age: 40 * time.Minute},
	{repo: "acme/clusters", author: "renovate", bot: true, title: "feat(container): update image ghcr.io/fluxcd/flux to v2.7.3", head: "renovate/flux-2.x", mergeable: "MERGEABLE", state: "CLEAN", checks: "SUCCESS", age: 5 * time.Hour},
	{repo: "acme/clusters", author: "hubot", title: "Add arm64 node pool", head: "feat/arm64-pool", mergeable: "MERGEABLE", state: "BLOCKED", review: "CHANGES_REQUESTED", checks: "FAILURE", age: 4 * 24 * time.Hour},
	{repo: "acme-labs/sdk-go", author: "renovate", bot: true, title: "fix(deps): update module github.com/google/go-github to v93", head: "renovate/github.com-google-go-github-93.x", mergeable: "MERGEABLE", state: "BEHIND", checks: "SUCCESS", age: 30 * time.Hour},
	{repo: "acme-labs/cli", author: "monalisa", title: "WIP: interactive login", head: "feat/login-tui", draft: true, mergeable: "MERGEABLE", state: "DRAFT", age: 6 * 24 * time.Hour},
	{repo: "octocat/dotfiles", author: "renovate", bot: true, title: "chore(deps): update flake.lock", head: "renovate/flake-lock", mergeable: "UNKNOWN", state: "UNKNOWN", checks: "SUCCESS", age: 15 * time.Minute},
}

func demoKey(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

func (p *Pulls) List(_ context.Context, _ *github.Client, repos []store.Repo) ([]pulls.PR, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	byName := map[string]store.Repo{}
	for _, r := range repos {
		byName[r.FullName] = r
	}
	var out []pulls.PR
	for i, d := range demoPRs {
		repo, ok := byName[d.repo]
		n := 40 + i*3
		if !ok || p.merged[demoKey(d.repo, n)] {
			continue
		}
		out = append(out, pulls.PR{
			RepoID: repo.ID, Repo: repo.FullName, Owner: repo.Owner, NodeID: fmt.Sprintf("PR_demo_%d", i), Number: n,
			Title: d.title, URL: "https://github.com/" + d.repo + "/pull/" + fmt.Sprint(n), Author: d.author, AuthorBot: d.bot,
			HeadRef: d.head, BaseRef: "main", HeadSHA: fmt.Sprintf("%040x", 0xc0ffee00+i), Draft: d.draft,
			CreatedAt: p.At.Add(-d.age), UpdatedAt: p.At.Add(-d.age / 2), Mergeable: d.mergeable, State: d.state, Review: d.review,
			Checks: d.checks, AutoMerge: p.auto[demoKey(d.repo, n)],
			DefaultMethod: pulls.MethodSquash, Methods: []string{pulls.MethodSquash, pulls.MethodRebase}, AutoMergeAllowed: true,
		})
	}
	return out, nil
}

func (p *Pulls) find(repo store.Repo, t pulls.Target) (pulls.PR, bool) {
	prs, _ := p.List(context.Background(), nil, []store.Repo{repo})
	for _, pr := range prs {
		if pr.Number == t.Number {
			return pr, true
		}
	}
	return pulls.PR{}, false
}

func (p *Pulls) Merge(_ context.Context, _ *github.Client, repo store.Repo, t pulls.Target, _ string) error {
	pr, ok := p.find(repo, t)
	switch {
	case !ok:
		return pulls.Refused{Message: "Pull request is not open"}
	case pr.Mergeable == "CONFLICTING":
		return pulls.Refused{Message: "Pull request is not mergeable"}
	case !pr.Ready():
		return pulls.Refused{Message: "Repository rule violations found: the base branch policy prohibits the merge"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.merged == nil {
		p.merged = map[string]bool{}
	}
	p.merged[demoKey(repo.FullName, t.Number)] = true
	return nil
}

func (p *Pulls) EnableAutoMerge(_ context.Context, _ *github.Client, repo store.Repo, t pulls.Target, _ string) error {
	pr, ok := p.find(repo, t)
	switch {
	case !ok:
		return pulls.Refused{Message: "Pull request is not open"}
	case pr.Ready():
		return pulls.Refused{Message: "Pull request is in clean status"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.auto == nil {
		p.auto = map[string]bool{}
	}
	p.auto[demoKey(repo.FullName, t.Number)] = true
	return nil
}
