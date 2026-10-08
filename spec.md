# Runwall — Spec

A live dashboard of GitHub Actions workflow runs across every repo in a handful of GitHub organizations. Each signed-in person sees the repos they have access to, can re-run or cancel where GitHub allows them to, and gets trends, cost estimates and repository grades. A kiosk link serves a wall display without signing in.

## Goals

- One live feed of workflow runs across all orgs, updating within seconds.
- Expand a run to see its jobs and their live status.
- Flag runs that have been queued too long.
- Notify on default-branch failures.
- Overview dashboard, Workflows, Repositories and Run detail pages (layout modelled on Snorlx, Catppuccin skin).
- Trends and metrics over a selectable period, plus list-price cost estimates.
- Re-run and cancel, authorised by the signed-in user's own GitHub access.
- Repository grades (gold/silver/bronze).
- An MCP endpoint so AI agents can query runs, failures and logs.

## Non-goals

- Approving deployments / environment protection rules.
- GitHub Deployments / Environments.
- Writing anything to repositories other than re-run/cancel.

## Scale

- Fewer than 5 orgs, fewer than 50 active repos.
- Single instance, single replica in all versions.

## Phases

| | v1 | v2 |
|---|---|---|
| Runs on | Laptop | Kubernetes (Talos), deployed via Flux |
| Webhook ingress | cloudflared named tunnel → localhost | Cluster ingress / cloudflared |
| Viewer access | Local only | Behind Cloudflare Access |
| Storage | SQLite file | SQLite on a PVC (single replica) |
| Failure notifications | macOS local notification | TBD (see Open Questions) |

## Stack

- Go, server-rendered with templ + htmx.
- Live updates pushed to the browser over SSE (htmx SSE extension).
- SQLite for storage, the same in v1 and v2.
- No JS build step.

## GitHub integration

### GitHub App

- One GitHub App, owned by the user's account, installed on every org. The user can approve installs on all target orgs.
- Permissions (expanded from the original read-only set):
  - **Actions: read & write**: write is used only through *user* tokens, for re-run and cancel.
  - **Checks: read**: annotations.
  - **Contents: read**: workflow YAML and repository scoring file checks.
  - **Administration: read**: branch protection, for scoring.
  - **Dependabot alerts: read**, **Code scanning alerts: read**, **Secret scanning alerts: read**: security score.
  - **Metadata: read**.
- Existing installations must accept the new permissions before the extra data appears. Until they do, the features that depend on them degrade gracefully.
- **User authorization** is enabled on the App (callback URL `https://<host>/auth/callback`). It provides the sign-in and the user tokens described under Authentication.
- Subscribed events: `workflow_run`, `workflow_job`, plus installation lifecycle events (`installation`, `installation_repositories`) so org and repo membership stays current.
- The set of orgs and repos comes from the App's installations. There is no static config list.
- Auth: App JWT → per-installation access tokens, cached and refreshed before they expire.
- Webhook URL: a stable hostname on the user's Cloudflare zone via a cloudflared named tunnel. The same URL can stay the same into v2.

### Webhook handling

- Verify `X-Hub-Signature-256` against the App's webhook secret. Reject anything unsigned or invalid.
- Deduplicate on `X-GitHub-Delivery`.
- Upsert by run ID / job ID, and never let an older event overwrite newer state (compare `updated_at`).
- Respond quickly, then do the processing asynchronously.

### Backfill and reconciliation

Webhooks are the fast path. Backfill covers gaps from laptop sleep, tunnel downtime, or restarts.

- On startup: for each installation and repo, list workflow runs updated since the last-seen timestamp for that repo, and upsert them.
- Periodically, every few minutes: the same reconcile pass. This also catches runs that are stuck in a non-terminal state locally.
- Fetch jobs for runs that are in progress or were updated recently.
- On first run with an empty DB: backfill a bounded window (default: last 7 days).

## Data model (sketch)

- `installations`: id, account login, account type
- `repos`: id, installation_id, full_name, default_branch, archived, fork, last_seen_at
- `runs`: id, repo_id, workflow name/path, run_number, run_attempt, event, head_branch, head_sha, commit message, actor login and type, status, conclusion, created_at, run_started_at, updated_at, html_url, pull request number(s)
- `jobs`: id, run_id, name, status, conclusion, runner labels, started_at, completed_at, html_url
- `deliveries`: delivery id, received_at (used for dedupe, and pruned)
- `notifications`: run id and kind, so a notification never fires twice

## Authentication & access

- **Sign in with GitHub** through the App's user authorization flow. All pages require a session except `/kiosk/<token>`, `/webhook` and `/healthz`.
- **Two kinds of token**:
  - The **installation token** is still used for everything the server does on its own: webhooks, backfill and reconcile, step polling, logs, annotations, workflow files and scoring.
  - The **user token** is used only to determine visibility and to perform run actions. GitHub caps it at whichever is lower, the App's permissions or the user's own access, so a re-run fails on GitHub's side when the user lacks write access.
- **Visibility**: on sign-in, and then every ~10 minutes, list the repos the user can access in each installation (`GET /user/installations/{installation_id}/repositories`). Cache them per session. Every page, fragment, SSE stream, chart and MCP call is filtered to that set.
- **Sessions**: server-side session rows in SQLite behind an HttpOnly, Secure, SameSite=Lax cookie. User tokens are encrypted at rest with a key from 1Password and refreshed with their refresh token when they expire. Signing out deletes the session.
- **Kiosk link**: an admin-configured secret URL (`/kiosk/<token>`) that shows a fixed set of orgs, read-only, with no sign-in. It never shows action buttons or logs from repos outside its set. Tokens are stored hashed and can be revoked.
- **Run actions** (users with write access only; the buttons are shown only when the user's repo permission is write or higher):
  - Re-run failed jobs: `POST …/runs/{id}/rerun-failed-jobs`
  - Re-run all jobs: `POST …/runs/{id}/rerun`
  - Re-run a single job: `POST …/jobs/{id}/rerun`
  - Cancel a run: `POST …/runs/{id}/cancel`, with a confirmation step
  - Each action is recorded in an `audit_log` table (who, what, when, result).
- **CSRF**: actions are POSTs carrying a per-session token, checked server-side.

## UI

### Layout

- Modelled on Snorlx, skinned with Catppuccin (Mocha by default, Latte via the toggle). Monospace for data such as SHAs, durations and logs.
- A left sidebar with Dashboard, Runs, Workflows, Repositories and Settings. It collapses, and becomes a drawer on mobile.
- A top bar with the live-connection indicator, a period selector (24h / 7d / **30d** / 90d, default 30d), the theme toggle, the **Sync** button and the user menu.
- Charts use **Chart.js**, vendored as a static file (no build step) and themed from CSS variables. They re-render on live updates.

### Dashboard (landing page)

- KPI cards: workflows (and how many are active), runs in the period (succeeded/failed), pending (running/queued), pipeline time in the period, and estimated cost in the period.
- **Active pipelines** as cards with an animated striped bottom edge, colour-coded running or queued, with stuck ones in peach.
- **Run trends**: a daily line/area chart of successes vs failures.
- **Run distribution**: a doughnut of success, failure and other.
- **Recent runs**, linking to the Runs page.
- The pulse strip from v1 stays at the top of the Runs page.

### Runs

The v1 live feed with its filters (below), unchanged in behaviour. Run rows link to the Run detail page and can still be expanded inline.

### Workflows

- Every workflow across the visible repos: last status, success rate, average and p95 duration, run count, and estimated cost over the period.
- Each workflow has a detail page with a trend chart and its runs.

### Repositories

- Cards or rows per repo: grade badge, latest default-branch status per workflow, success rate, and cost.
- A detail page with the score breakdown, a **Refresh grade** button, workflows and recent runs.

### Run detail

- Header: status, workflow, repo, branch/PR, commit, actor, attempt, durations, and action buttons.
- Jobs, with steps and logs as in v1 (live steps while running, inline log once finished).
- **Annotations**: the check-run annotations for each job (level, file:line, message), linked to the file on GitHub.
- **Workflow file**: the YAML at the run's head SHA, shown read-only.

### Settings

- Personal API tokens for MCP: create (shown once), list and revoke.
- Kiosk links (for the admin, i.e. the App owner): create and revoke.
- Session/sign-out.

### Live feed (Runs page)

- A chronological stream of runs across all orgs, newest activity first. Rows update in place as their status changes.
- Each row shows: org/repo, workflow name, branch (or PR #), event, actor, commit message (truncated), status/conclusion, duration or elapsed time, and a link to GitHub.
- Expanding a row shows its jobs with live status, plus links to the job logs on GitHub.
- Expanding a job shows its steps:
  - **Running job**: steps refresh every 5 s while the job is expanded. Workflow-job webhooks don't report step progress, so this polls `GET /actions/jobs/{id}`, and refreshes are shared between viewers. There is a link to the live log on GitHub, because the public API has no live log text: the log endpoint returns `BlobNotFound` until the job finishes.
  - **Finished job**: "Show log" loads the last 1,000 lines inline. Timestamps and colours are stripped, group, error and warning markers are highlighted, and the view opens at the first error. Logs are fetched on demand and never stored.
- Expanded runs and jobs, loaded logs, and the log scroll position all survive live refreshes.
- Theme: dark mode, Catppuccin palette.

### Filters

- By org, repo, branch, event, and status/conclusion.
- Default branch only vs. all branches and PRs.
- Hidden by default, with a toggle to show:
  - Archived repos
  - Forks
  - Bot-triggered runs (actor type `Bot`, e.g. Dependabot or Renovate)
- Filter state lives in the URL query string, so a filtered view can be shared.

### Stuck detection

- A run or job that has been `queued` for more than **5 minutes** is highlighted as stuck. The threshold is global and configurable.
- A periodic check re-evaluates queued items, because no webhook fires when "nothing happens."

## Metrics, costs and scoring

### Metrics

- Computed from stored runs and jobs over the selected period: run counts, success rate, average/p95 duration, total pipeline time, queue time and re-run rate.
- Retention is raised to **90 days** so the 90d period works.

### Cost estimates

- Billable minutes are job durations rounded up to whole minutes per job, multiplied by the per-minute list price for the runner's OS and size.
- Public repos and self-hosted runners cost $0.
- Prices come from a built-in table, overridable by a config file. The UI labels them "estimated, list price as of <date>". Plan allowances aren't subtracted.
- Shown per workflow, per repo, and as a dashboard KPI.

### Repository scoring

- Grade tiers are gold, silver and bronze, using Snorlx's weights: Security 25%, Testing 20%, CI/CD 15%, Documentation 15%, Code quality 10%, Maintenance 10%, Community 5%.
- Inputs:
  - Security: open Dependabot, code-scanning and secret-scanning alerts.
  - Testing: test files and directories present, and test jobs in CI.
  - CI/CD: CI success rate, flaky re-run rate, and default-branch protection.
  - Documentation: README and docs.
  - Code quality: linters and formatters configured.
  - Maintenance: recent commits and open stale issues/PRs.
  - Community: GitHub's community profile.
- Recomputed daily per repo, plus on demand with **Refresh grade**. Results are stored with their breakdown so the page explains the grade.

## MCP

- A streamable-HTTP MCP endpoint at `/mcp`, served by the same binary.
- Auth is a bearer **personal API token** created in Settings. Calls act as that user and see only that user's repos. Tokens are stored hashed and can be revoked.
- Tools (read-only): `list_runs` (filters as on the Runs page), `get_run` (jobs, steps, annotations), `get_job_log` (tail, around first error), `failing_workflows`, `workflow_stats`, `repo_score`.
- Run actions are not exposed over MCP.

## Notifications

- Trigger: a run on the repo's **default branch** completes with conclusion `failure`. PR runs and other branches stay quiet.
- v1: a macOS local notification from the running process. It includes the repo, workflow and branch, and clicking it opens the run.
- Fire at most once per run attempt. Don't notify for runs that only arrived through backfill and are older than the current session start, so a startup doesn't produce a flood.

## Security

- The installation token is never used for writes. Writes happen only with the signed-in user's token, so GitHub enforces that user's permissions.
- The webhook secret and App private key come from 1Password (`op run` locally, External Secrets in k8s). They are never committed.
- v1 listens on localhost for the UI. Only the webhook path is exposed through the tunnel.
- v2: the UI sits behind Cloudflare Access, and the webhook path is exempt from Access but signature-verified.
- Visibility is per user (see Authentication & access). The kiosk link shows only its configured orgs.
- New secrets in 1Password: the App client ID and secret, the session encryption key, and the kiosk/API token pepper.

## Decisions & Tradeoffs

| Decision | Chosen | Rejected / why |
|---|---|---|
| Build vs. adopt | Build | Snorlx (young project, not the desired UX); GitactionBoard (single owner) |
| Audience | Per-user views via GitHub sign-in, plus a kiosk link for wall displays | Shared open wall (original v1 choice): replaced so private repos are visible only to people with access |
| Run actions | Re-run (failed / all / single job) and cancel, using the user's token | Installation token for writes: would bypass per-user permissions |
| App permissions | Expanded for annotations, workflow YAML and full scoring | Checks-only: would limit scoring to CI health |
| Charts | Chart.js, vendored | Server-rendered SVG: less interactive; uPlot: no doughnut |
| MCP | HTTP `/mcp` with personal tokens | Stdio: laptop-only; single admin token: bypasses per-user access |
| Delivery | All features built together | Phased delivery |
| Event source | GitHub App webhooks + backfill | Polling only: not live; PAT + org webhooks: manual config per org |
| Local webhooks | cloudflared named tunnel | smee.io: third-party relay of private payloads |
| Stack | Go + templ + htmx | SvelteKit: wanted a single binary with no JS build |
| Storage | SQLite in both v1 and v2 | SQLite→CNPG: two dialects for no real gain at this scale. This locks v2 to a single replica. |
| Primary view | Live feed of runs | Repo status grid (could be added later) |

## Open Questions

- **Making the App public**: GitHub's docs say only members of the owning account can authorize a private App, and it can only be installed on that account. Before adding orgs or teammates, make it public and set `ALLOWED_ACCOUNTS` (installation allow-list, already implemented) and optionally `ALLOWED_USERS`.
- **Kiosk link vs Cloudflare Access (v2)**: `/kiosk/*` needs an Access bypass, or a service token on the wall device.
- **Price table**: verify GitHub's current per-minute list prices (Linux, Windows, macOS, larger runners, arm64) when building the built-in table.
- **Contents: read** is not yet granted on the App. Until it is, the workflow YAML view and the file-based scoring checks (tests, docs, linters, community files) show as "not checked".
- **v2 notifications**: macOS notifications don't work from the cluster. Slack, ntfy, or something else? (The notifier is already an interface; `NOTIFIER=log` is the k8s default until this is decided.)
- **Stuck threshold for self-hosted runners**: is one global 5-minute threshold enough, or will it be noisy?
- **Webhook path exemption in CF Access**: confirm that the bypass policy for the webhook route is acceptable to whoever owns the Cloudflare zone.

## Resolved during implementation

- **Admins**: `ADMIN_USERS` (comma-separated logins); defaults to the GitHub App's owner. Admins manage kiosk links and see the action audit log.
- **Grade tiers**: gold ≥ 85, silver ≥ 70, bronze ≥ 50, otherwise unranked.
- **Cost prices**: built-in table checked against GitHub's pricing page on 2026-10-08 (Linux 2-core $0.006/min, Windows $0.010, macOS $0.062, larger runners per the page); override with `COST_RATES_FILE`.

- **History retention**: 30 days by default (`RETENTION`), pruned on every reconcile pass. Unfinished runs are never pruned.
- **Re-runs**: a new `run_attempt` replaces the row in place (same run ID), shows a ↻N marker, and moves to the top of the feed. Jobs shown are the current attempt's only. A failed re-run notifies again.
- **Jobs for backfilled runs**: fetched during sync only for unfinished runs; for older runs they are fetched from GitHub the first time the row is expanded, then stored.
