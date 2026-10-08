// Package scoring grades repositories gold/silver/bronze, using Snorlx's category
// weights. A category whose data the App can't read (missing permission) is left out
// and its weight is shared among the others, so a grade never counts an unknown as a failure.
package scoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/nklmilojevic/runwall/internal/ghapp"
	"github.com/nklmilojevic/runwall/internal/metrics"
	"github.com/nklmilojevic/runwall/internal/store"
)

const (
	Gold   = "gold"
	Silver = "silver"
	Bronze = "bronze"
	None   = "unranked"
)

// TierFor maps a 0–100 score to a tier.
func TierFor(score int) string {
	switch {
	case score >= 85:
		return Gold
	case score >= 70:
		return Silver
	case score >= 50:
		return Bronze
	}
	return None
}

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type Category struct {
	Name      string  `json:"name"`
	Weight    float64 `json:"weight"`
	Score     int     `json:"score"`
	Available bool    `json:"available"`
	Missing   string  `json:"missing,omitempty"` // permission needed when unavailable
	Checks    []Check `json:"checks"`
}

type Breakdown struct {
	Categories []Category `json:"categories"`
}

func ParseBreakdown(s string) Breakdown {
	var b Breakdown
	_ = json.Unmarshal([]byte(s), &b) // a corrupt breakdown renders as empty
	return b
}

type Scorer struct {
	GH      ghapp.Clients
	Store   *store.Store
	Log     *slog.Logger
	Now     func() time.Time
	Every   time.Duration // rescore interval, default 24h
	PerPass int           // repos scored per ScoreDue call, default 10
}

// ScoreDue grades repos whose grade is missing or older than Every.
func (s *Scorer) ScoreDue(ctx context.Context) error {
	every, perPass := s.Every, s.PerPass
	if every == 0 {
		every = 24 * time.Hour
	}
	if perPass == 0 {
		perPass = 10
	}
	repos, err := s.Store.ListRepos(ctx, 0)
	if err != nil {
		return err
	}
	scores, err := s.Store.ListScores(ctx)
	if err != nil {
		return err
	}
	now := s.Now()
	done := 0
	var errs []error
	for _, r := range repos {
		if r.Archived || r.InstallationID == 0 {
			continue
		}
		if sc, ok := scores[r.ID]; ok && now.Sub(sc.ComputedAt) < every {
			continue
		}
		if done >= perPass {
			break
		}
		done++
		if _, err := s.Score(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.FullName, err))
		}
	}
	return errors.Join(errs...)
}

// inputs are everything the categories are computed from.
type inputs struct {
	repo store.Repo

	tree      []string // file paths at the default branch head; nil if unreadable
	treeErr   error
	dependabo alertCount
	codeScan  alertCount
	secrets   alertCount
	protected *bool

	ciRuns       int
	ciSuccess    float64 // default-branch success rate, -1 if unknown
	hasTestJobs  bool
	communityPct int // GitHub's community profile health, -1 if unknown
}

type alertCount struct {
	available bool // false: the App can't read this kind of alert
	enabled   bool
	critical  int
	high      int
	medium    int
	low       int
}

// Score grades one repo now and stores the result.
func (s *Scorer) Score(ctx context.Context, r store.Repo) (store.RepoScore, error) {
	in, err := s.gather(ctx, r)
	if err != nil {
		return store.RepoScore{}, err
	}
	b := grade(in)
	score := Total(b)
	raw, _ := json.Marshal(b)
	rs := store.RepoScore{RepoID: r.ID, Score: score, Tier: TierFor(score), Breakdown: string(raw), ComputedAt: s.Now()}
	return rs, s.Store.SaveScore(ctx, rs)
}

func notAccessible(err error) bool {
	var ge *github.ErrorResponse
	return errors.As(err, &ge) && strings.Contains(strings.ToLower(ge.Message), "not accessible by integration")
}

func status(resp *github.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func (s *Scorer) gather(ctx context.Context, r store.Repo) (inputs, error) {
	in := inputs{repo: r, ciSuccess: -1, communityPct: -1}
	gh := s.GH.Installation(r.InstallationID)

	if r.DefaultBranch != "" {
		tree, resp, err := gh.Git.GetTree(ctx, r.Owner, r.Name, r.DefaultBranch, true)
		switch {
		case err == nil:
			for _, e := range tree.Entries {
				in.tree = append(in.tree, e.GetPath())
			}
		case status(resp) == http.StatusConflict: // empty repository
			in.tree = []string{}
		default:
			in.treeErr = err
		}
	}

	in.dependabo = s.alerts(ctx, func() ([]string, *github.Response, error) {
		alerts, resp, err := gh.Dependabot.ListRepoAlerts(ctx, r.Owner, r.Name, &github.ListAlertsOptions{State: new("open"), ListCursorOptions: github.ListCursorOptions{PerPage: 100}})
		var sev []string
		for _, a := range alerts {
			sev = append(sev, a.GetSecurityAdvisory().GetSeverity())
		}
		return sev, resp, err
	})
	in.codeScan = s.alerts(ctx, func() ([]string, *github.Response, error) {
		alerts, resp, err := gh.CodeScanning.ListAlertsForRepo(ctx, r.Owner, r.Name, &github.AlertListOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}})
		var sev []string
		for _, a := range alerts {
			sev = append(sev, a.GetRule().GetSecuritySeverityLevel())
		}
		return sev, resp, err
	})
	in.secrets = s.alerts(ctx, func() ([]string, *github.Response, error) {
		alerts, resp, err := gh.SecretScanning.ListAlertsForRepo(ctx, r.Owner, r.Name, &github.SecretScanningAlertListOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}})
		sev := make([]string, len(alerts))
		for i := range alerts {
			sev[i] = "critical" // a leaked secret is always urgent
		}
		return sev, resp, err
	})

	if r.DefaultBranch != "" {
		_, resp, err := gh.Repositories.GetBranchProtection(ctx, r.Owner, r.Name, r.DefaultBranch)
		switch {
		case err == nil:
			in.protected = new(true)
		case notAccessible(err) || status(resp) == http.StatusForbidden:
			// Administration: read missing (or a plan without branch protection); fall back to rulesets below.
		case status(resp) == http.StatusNotFound:
			in.protected = new(false)
		}
		// Rulesets protect branches too and only need Metadata: read.
		req, err := gh.NewRequest(ctx, http.MethodGet, fmt.Sprintf("repos/%s/%s/rules/branches/%s", r.Owner, r.Name, r.DefaultBranch), nil)
		if err == nil {
			var rules []json.RawMessage
			if _, err := gh.Do(req, &rules); err == nil && len(rules) > 0 {
				in.protected = new(true)
			} else if err == nil && in.protected == nil {
				in.protected = new(false)
			}
		}
	}

	if !r.Private {
		if m, _, err := gh.Repositories.GetCommunityHealthMetrics(ctx, r.Owner, r.Name); err == nil {
			in.communityPct = m.GetHealthPercentage()
		}
	}

	runs, err := s.Store.MetricRuns(ctx, store.Scope{All: true}, s.Now().Add(-90*24*time.Hour))
	if err != nil {
		return in, err
	}
	var c metrics.Counts
	for _, m := range runs {
		if m.RepoID != r.ID {
			continue
		}
		in.ciRuns++
		if strings.Contains(strings.ToLower(m.WorkflowName), "test") || strings.Contains(strings.ToLower(m.WorkflowPath), "test") {
			in.hasTestJobs = true
		}
		if m.HeadBranch == m.DefaultBranch {
			switch metrics.OutcomeOf(m.Status, m.Conclusion) {
			case metrics.Success:
				c.Success++
			case metrics.Failure:
				c.Failure++
			}
		}
	}
	in.ciSuccess = c.SuccessRate()
	if !in.hasTestJobs {
		in.hasTestJobs = s.hasTestJob(ctx, r.ID)
	}
	return in, nil
}

func (s *Scorer) hasTestJob(ctx context.Context, repoID int64) bool {
	runs, err := s.Store.Feed(ctx, store.FeedFilter{Scope: store.Scope{All: true}, ShowArchived: true, ShowForks: true, ShowBots: true, Limit: 50}, s.Now())
	if err != nil {
		return false
	}
	for _, r := range runs {
		if r.RepoID != repoID {
			continue
		}
		for _, j := range r.Jobs {
			if strings.Contains(strings.ToLower(j.Name), "test") {
				return true
			}
		}
	}
	return false
}

func (s *Scorer) alerts(_ context.Context, list func() ([]string, *github.Response, error)) alertCount {
	sev, resp, err := list()
	switch {
	case err == nil:
		a := alertCount{available: true, enabled: true}
		for _, v := range sev {
			switch strings.ToLower(v) {
			case "critical":
				a.critical++
			case "high", "error":
				a.high++
			case "medium", "moderate", "warning":
				a.medium++
			default:
				a.low++
			}
		}
		return a
	case notAccessible(err):
		return alertCount{}
	case status(resp) == http.StatusForbidden || status(resp) == http.StatusNotFound:
		// The feature is turned off for this repo (or no analysis has run yet).
		return alertCount{available: true}
	}
	return alertCount{}
}

// Grading

func clamp(v float64) int { return int(math.Round(math.Max(0, math.Min(100, v)))) }

func hasFile(tree []string, match func(p string) bool) (string, bool) {
	for _, p := range tree {
		if match(p) {
			return p, true
		}
	}
	return "", false
}

func base(p string) string { return strings.ToLower(path.Base(p)) }

func isTestPath(p string) bool {
	l := strings.ToLower(p)
	b := base(p)
	return strings.HasSuffix(b, "_test.go") || strings.HasPrefix(b, "test_") && strings.HasSuffix(b, ".py") ||
		strings.Contains(b, ".test.") || strings.Contains(b, ".spec.") || strings.HasSuffix(b, "_spec.rb") ||
		strings.HasPrefix(l, "test/") || strings.HasPrefix(l, "tests/") || strings.Contains(l, "/tests/") ||
		strings.Contains(l, "/__tests__/") || strings.HasPrefix(l, "spec/") || strings.Contains(l, "/src/test/")
}

var lintConfigs = []string{
	".golangci.yml", ".golangci.yaml", ".golangci.toml", ".eslintrc", ".eslintrc.js", ".eslintrc.json", ".eslintrc.cjs",
	".eslintrc.yml", "eslint.config.js", "eslint.config.mjs", "eslint.config.ts", "biome.json", "biome.jsonc", ".prettierrc",
	".prettierrc.json", "prettier.config.js", "ruff.toml", ".ruff.toml", ".flake8", ".pylintrc", "rustfmt.toml", ".rustfmt.toml",
	"clippy.toml", ".rubocop.yml", ".swiftlint.yml", "deno.json", ".stylelintrc", "treefmt.toml", ".markdownlint.json",
}

func grade(in inputs) Breakdown {
	tree, treeOK := in.tree, in.tree != nil
	const contents = "Contents: read"
	var b Breakdown

	// Security
	sec := Category{Name: "Security", Weight: 25}
	kinds := []struct {
		name string
		a    alertCount
		perm string
	}{
		{"Dependabot alerts", in.dependabo, "Dependabot alerts: read"},
		{"Code scanning", in.codeScan, "Code scanning alerts: read"},
		{"Secret scanning", in.secrets, "Secret scanning alerts: read"},
	}
	secScore, secParts := 0.0, 0
	var missing []string
	for _, k := range kinds {
		if !k.a.available {
			missing = append(missing, k.perm)
			continue
		}
		secParts++
		if !k.a.enabled {
			sec.Checks = append(sec.Checks, Check{Name: k.name, Detail: "not enabled"})
			continue
		}
		v := 100 - 25*float64(k.a.critical) - 15*float64(k.a.high) - 5*float64(k.a.medium) - 2*float64(k.a.low)
		open := k.a.critical + k.a.high + k.a.medium + k.a.low
		detail := "no open alerts"
		if open > 0 {
			detail = fmt.Sprintf("%d open (%d critical, %d high)", open, k.a.critical, k.a.high)
		}
		sec.Checks = append(sec.Checks, Check{Name: k.name, OK: open == 0, Detail: detail})
		secScore += float64(clamp(v))
	}
	if secParts > 0 {
		sec.Available, sec.Score = true, clamp(secScore/float64(secParts))
	}
	if len(missing) > 0 && secParts == 0 {
		sec.Missing = strings.Join(missing, ", ")
	}
	b.Categories = append(b.Categories, sec)

	// Testing
	test := Category{Name: "Testing", Weight: 20, Available: true}
	tv := 0.0
	if treeOK {
		p, ok := hasFile(tree, isTestPath)
		test.Checks = append(test.Checks, Check{Name: "Test files", OK: ok, Detail: p})
		if ok {
			tv += 60
		}
		test.Checks = append(test.Checks, Check{Name: "Tests run in CI", OK: in.hasTestJobs})
		if in.hasTestJobs {
			tv += 40
		}
	} else {
		test.Checks = append(test.Checks, Check{Name: "Tests run in CI", OK: in.hasTestJobs, Detail: "test files not checked: needs " + contents})
		if in.hasTestJobs {
			tv = 100
		}
	}
	test.Score = clamp(tv)
	b.Categories = append(b.Categories, test)

	// CI/CD
	ci := Category{Name: "CI/CD", Weight: 15, Available: true}
	cv := 0.0
	ci.Checks = append(ci.Checks, Check{Name: "Uses GitHub Actions", OK: in.ciRuns > 0, Detail: fmt.Sprintf("%d runs in 90 days", in.ciRuns)})
	if in.ciRuns > 0 {
		cv += 30
	}
	if in.ciSuccess >= 0 {
		ci.Checks = append(ci.Checks, Check{Name: "Default branch passing", OK: in.ciSuccess >= 0.9, Detail: fmt.Sprintf("%.0f%% success", in.ciSuccess*100)})
		cv += 40 * math.Max(0, math.Min(1, (in.ciSuccess-0.5)/0.4))
	}
	if in.protected != nil {
		ci.Checks = append(ci.Checks, Check{Name: "Default branch protected", OK: *in.protected})
		if *in.protected {
			cv += 30
		}
	}
	ci.Score = clamp(cv)
	b.Categories = append(b.Categories, ci)

	// Documentation
	doc := Category{Name: "Documentation", Weight: 15, Available: true}
	dv := 0.0
	doc.Checks = append(doc.Checks, Check{Name: "Description", OK: in.repo.Description != ""})
	if in.repo.Description != "" {
		dv += 20
	}
	if treeOK {
		p, ok := hasFile(tree, func(p string) bool { return !strings.Contains(p, "/") && strings.HasPrefix(base(p), "readme") })
		doc.Checks = append(doc.Checks, Check{Name: "README", OK: ok, Detail: p})
		if ok {
			dv += 50
		}
		_, docsDir := hasFile(tree, func(p string) bool { return strings.HasPrefix(strings.ToLower(p), "docs/") })
		md := 0
		for _, p := range tree {
			if strings.HasSuffix(strings.ToLower(p), ".md") {
				md++
			}
		}
		doc.Checks = append(doc.Checks, Check{Name: "Docs beyond the README", OK: docsDir || md >= 3, Detail: fmt.Sprintf("%d markdown files", md)})
		if docsDir || md >= 3 {
			dv += 30
		}
		doc.Score = clamp(dv)
	} else {
		doc.Checks = append(doc.Checks, Check{Name: "README and docs", Detail: "not checked: needs " + contents})
		doc.Score = clamp(dv / 20 * 100)
	}
	b.Categories = append(b.Categories, doc)

	// Code quality
	cq := Category{Name: "Code quality", Weight: 10}
	if treeOK {
		cq.Available = true
		p, ok := hasFile(tree, func(p string) bool {
			b := base(p)
			for _, c := range lintConfigs {
				if b == c {
					return true
				}
			}
			return strings.HasPrefix(strings.ToLower(p), ".github/workflows/") && strings.Contains(b, "lint")
		})
		cq.Checks = append(cq.Checks, Check{Name: "Linter or formatter configured", OK: ok, Detail: p})
		q := 0.0
		if ok {
			q += 70
		}
		p2, ok2 := hasFile(tree, func(p string) bool { b := base(p); return b == ".editorconfig" || b == ".pre-commit-config.yaml" })
		cq.Checks = append(cq.Checks, Check{Name: "Editor config or pre-commit hooks", OK: ok2, Detail: p2})
		if ok2 {
			q += 30
		}
		cq.Score = clamp(q)
	} else {
		cq.Missing = contents
	}
	b.Categories = append(b.Categories, cq)

	// Maintenance
	mt := Category{Name: "Maintenance", Weight: 10, Available: !in.repo.PushedAt.IsZero()}
	if mt.Available {
		age := time.Since(in.repo.PushedAt)
		recent := 60 * math.Max(0, math.Min(1, (90*24*time.Hour-age).Hours()/(60*24)))
		mt.Checks = append(mt.Checks, Check{Name: "Recent activity", OK: age < 30*24*time.Hour, Detail: "last push " + in.repo.PushedAt.Format("2006-01-02")})
		issues := 40 * math.Max(0, math.Min(1, (200-float64(in.repo.OpenIssues))/180))
		mt.Checks = append(mt.Checks, Check{Name: "Manageable issue backlog", OK: in.repo.OpenIssues <= 20, Detail: fmt.Sprintf("%d open issues and PRs", in.repo.OpenIssues)})
		mt.Score = clamp(recent + issues)
	}
	b.Categories = append(b.Categories, mt)

	// Community
	cm := Category{Name: "Community", Weight: 5}
	switch {
	case treeOK:
		cm.Available = true
		v := 0.0
		for _, f := range []struct {
			name, prefix string
			points       float64
		}{{"License", "license", 40}, {"Contributing guide", "contributing", 20}, {"Code of conduct", "code_of_conduct", 20}, {"Security policy", "security", 20}} {
			p, ok := hasFile(tree, func(p string) bool {
				dir := strings.ToLower(path.Dir(p))
				return (dir == "." || dir == ".github" || dir == "docs") && strings.HasPrefix(base(p), f.prefix)
			})
			cm.Checks = append(cm.Checks, Check{Name: f.name, OK: ok, Detail: p})
			if ok {
				v += f.points
			}
		}
		cm.Score = clamp(v)
	case in.communityPct >= 0:
		cm.Available, cm.Score = true, clamp(float64(in.communityPct))
		cm.Checks = append(cm.Checks, Check{Name: "GitHub community profile", OK: in.communityPct >= 80, Detail: fmt.Sprintf("%d%% complete", in.communityPct)})
	default:
		cm.Missing = contents
	}
	b.Categories = append(b.Categories, cm)
	return b
}

// Total is the weighted average over the categories that could be checked.
func Total(b Breakdown) int {
	var sum, weight float64
	for _, c := range b.Categories {
		if c.Available {
			sum += float64(c.Score) * c.Weight
			weight += c.Weight
		}
	}
	if weight == 0 {
		return 0
	}
	return clamp(sum / weight)
}
