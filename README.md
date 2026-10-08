# Runwall

A live wall of GitHub Actions runs across every repository in your organizations: what's running, what's stuck, what's red on main, how long it all takes and what it costs.

![Dashboard](docs/screenshots/dashboard.png)

Runwall is one Go binary with SQLite. Webhooks deliver updates in about a second, a rate-limit-aware reconciler covers anything missed, and everyone signs in with GitHub and sees only the repositories they can access.

## Features

- **Live run feed** across all orgs, with filters (org, repo, branch, event, status, bots, forks) kept in the URL, and a pulse strip of recent runs where bar height is duration.
- **Run detail**: jobs with live step progress, full logs once a job finishes (opened at the first error), check annotations with `file:line` links, and the workflow file that ran.
- **Re-run and cancel**: re-run failed jobs, all jobs or a single job, or cancel a run. These use the signed-in person's own GitHub token, so GitHub enforces their write access. Every action is audited.
- **Dashboard**: active pipelines, success/failure trends, run distribution, pipeline time and estimated cost over 24h, 7d, 30d or 90d.
- **Workflows**: success rate, average and p95 duration, and cost per workflow.
- **Repositories**: gold, silver or bronze grades from security alerts, tests, CI health, docs, code quality, maintenance and community files.
- **Stuck detection**: anything queued longer than a threshold is highlighted.
- **Kiosk links**: a read-only wall display for chosen accounts, without signing in.
- **MCP endpoint** so AI agents (Claude Code, Cursor, …) can ask about runs, failures, logs, stats and grades with a personal token.
- **Light and dark themes** (Catppuccin), usable on mobile.

<table>
  <tr>
    <td><img src="docs/screenshots/runs.png" alt="Runs"></td>
    <td><img src="docs/screenshots/workflows.png" alt="Workflows"></td>
  </tr>
  <tr>
    <td colspan="2"><img src="docs/screenshots/repository.png" alt="Repository grade"></td>
  </tr>
</table>

## Try it

Demo mode serves sample data and doesn't contact GitHub:

```sh
docker run --rm -p 8080:8080 ghcr.io/nklmilojevic/runwall --demo
# or, from a checkout:
go run ./cmd/runwall --demo
```

Open http://localhost:8080.

## Setup

### 1. Create a GitHub App

Replace `HOST` with the address people will use to reach Runwall (for example `runwall.example.com`), then open:

```
https://github.com/settings/apps/new?name=Runwall&url=https://HOST&public=false&webhook_active=true&webhook_url=https://HOST/webhook&callback_urls[]=https://HOST/auth/callback&actions=write&administration=read&checks=read&contents=read&metadata=read&vulnerability_alerts=read&security_events=read&secret_scanning_alerts=read&events[]=workflow_run&events[]=workflow_job&events[]=repository
```

To create the App under an organization, use `https://github.com/organizations/ORG/settings/apps/new?…` with the same parameters.

On the form:
- Set a **webhook secret**, for example from `openssl rand -hex 32`.
- After creating the App, note the **App ID** and **Client ID**, generate a **client secret**, and generate a **private key**.
- Install the App on each account whose runs you want to see.

**Public or private App.** A private App can only be installed on the account that owns it, and only members of that account can sign in. To cover several organizations, make the App public and set `ALLOWED_ACCOUNTS`. Runwall then ignores installations on any other account.

**What the permissions are for:**

| Permission | Used for |
|---|---|
| Actions: read & write | Runs, jobs and logs. Write is used only through a signed-in person's token, for re-run and cancel. |
| Checks: read | Annotations |
| Contents: read | The workflow file view, and test/docs/lint checks in grades |
| Administration: read | Branch protection in grades |
| Dependabot alerts, Code scanning alerts, Secret scanning alerts: read | The security part of grades |
| Metadata: read | Repository list |

Everything except Actions and Metadata is optional. Without it, the related features show "not checked" instead of failing.

### 2. Run it

Runwall reads its secrets from the environment:

| Variable | |
|---|---|
| `GITHUB_APP_ID` | The App ID |
| `GITHUB_APP_PRIVATE_KEY_FILE` | Path to the App's private key (`.pem`). Alternatively, put the PEM contents in `GITHUB_APP_PRIVATE_KEY`. |
| `GITHUB_WEBHOOK_SECRET` | The webhook secret |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | For signing in with GitHub |
| `SESSION_KEY` | `openssl rand -hex 32`; encrypts sessions and stored tokens |
| `BASE_URL` | `https://HOST`; must match the App's callback URL |

Keep them in a secret manager and inject them at start. [`secrets.op`](secrets.op) shows the pattern with 1Password's `op run`.

**Docker Compose:** [`deploy/docker-compose.yml`](deploy/docker-compose.yml) runs Runwall with a persistent volume, plus an optional Cloudflare Tunnel.

**Kubernetes:** [`deploy/k8s`](deploy/k8s) is a Kustomize base: a single-replica Deployment with a PersistentVolumeClaim, a Service, an ExternalSecret for the secrets, and a ConfigMap for settings.

**Binary:** `go build ./cmd/runwall`, or `make run`, which injects secrets with `op run --env-file=secrets.op`.

Run exactly one replica: the state lives in a SQLite file.

### 3. Expose the webhook

GitHub must reach `https://HOST/webhook`. Everything else can stay private. With Cloudflare, put the UI behind Cloudflare Access and bypass Access for `/webhook`; GitHub's signature on every delivery is verified. [`deploy/cloudflared/config.example.yml`](deploy/cloudflared/config.example.yml) is a tunnel that exposes only the webhook path, for running on a laptop.

Webhook deliveries that GitHub couldn't make while Runwall was down are recovered by the reconciler within a minute or two of starting.

### 4. Use it

- **Signing in:** people sign in with GitHub. Who can sign in is controlled by `ALLOWED_USERS` if set. Otherwise anyone with access to at least one repository in an allowed installation can.
- **Admins** (`ADMIN_USERS`, default: the App's owner) create kiosk links and see the action audit log in Settings.
- **MCP:** create a personal token in Settings. The page shows the exact command, for example:

  ```sh
  claude mcp add --transport http runwall https://HOST/mcp --header "Authorization: Bearer <token>"
  ```

  The tools are read-only: `list_runs`, `get_run`, `get_job_log`, `failing_workflows`, `workflow_stats` and `repo_score`.

## Configuration

| Variable | Default | |
|---|---|---|
| `BASE_URL` | `http://` + `LISTEN_ADDR` | Public URL; used for the OAuth callback, kiosk and MCP links |
| `LISTEN_ADDR` | `127.0.0.1:8080` (`:8080` in the image) | |
| `DB_PATH` | `runwall.db` (`/data/runwall.db` in the image) | SQLite file |
| `ALLOWED_ACCOUNTS` | all | Comma-separated installation accounts to use; others are ignored |
| `ALLOWED_USERS` | anyone with repo access | Comma-separated GitHub logins allowed to sign in |
| `ADMIN_USERS` | the App's owner | Comma-separated GitHub logins |
| `STUCK_THRESHOLD` | `5m` | Queued longer than this counts as stuck |
| `BACKFILL_WINDOW` | `168h` | History loaded for a newly seen repository |
| `RECONCILE_INTERVAL` | `3m` | How often busy repositories are reconciled |
| `COLD_INTERVAL` | `1h` | How often quiet repositories (no push or run in 24h) are reconciled |
| `SYNC_CONCURRENCY` | `4` | Repositories reconciled at once |
| `JOB_BACKFILL` | `200` | Older runs per installation and pass whose jobs are loaded for cost estimates |
| `RETENTION` | `2160h` (90 days) | How long finished runs are kept |
| `COST_RATES_FILE` | built in | JSON overrides for per-minute prices (`{"as_of": "…", "rates": {"linux-2": 0.006}, "labels": {"my-runner": 0.02}}`) |
| `NOTIFIER` | `macos` on macOS, else `log` | Failure notifications for default-branch runs: `macos`, `log` or `none` |
| `GITHUB_API_URL`, `GITHUB_WEB_URL` | github.com | For GitHub Enterprise Server |
| `LOG_LEVEL` | `info` | |

## How it works

- **Webhooks** (`workflow_run`, `workflow_job`, `repository`) are verified, de-duplicated and applied in order. An older event never overwrites newer state.
- **The reconciler** lists each repository's newest runs with conditional requests. Unchanged repositories answer `304 Not Modified`, which doesn't count against GitHub's rate limit. Busy repositories are checked every pass and quiet ones hourly. It watches the installation's remaining rate limit: below 20%, cost backfill and grading pause; below 3%, it waits for the reset. The footer shows the current budget.
- **Live updates** reach the browser over server-sent events. Each viewer only gets events for repositories they can see.
- **Logs** come from GitHub on demand. GitHub only publishes a job's log once the job finishes, so running jobs show live step progress and link to GitHub's live log.
- **Costs** are list-price estimates: job minutes are rounded up per job and priced by runner type. Standard runners on public repositories and self-hosted runners count as free. Plan allowances aren't subtracted.

## Development

```sh
make test     # go test with generated templates
make demo     # sample data on http://127.0.0.1:8080
make run      # real GitHub App, secrets from 1Password (secrets.op)
make tunnel   # cloudflared tunnel exposing only /webhook
```

The stack is Go, [templ](https://templ.guide), [htmx](https://htmx.org) with server-sent events, [Chart.js](https://www.chartjs.org) and SQLite ([modernc.org/sqlite](https://modernc.org/sqlite), no CGO). Generated `*_templ.go` files are committed; run `go tool templ generate` after editing `.templ` files.

## License

[AGPL-3.0](LICENSE)
