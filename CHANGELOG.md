# Changelog

## v0.2.0 - 2026-11-15

This release lets an AI agent list and merge pull requests through MCP. It also updates the `templ` library.

### New features

- Each API token can now get its own GitHub authorization. Use the **Authorize** button on the Settings page. The token owner then controls what the token can merge.
- The new `list_pull_requests` MCP tool lists the open pull requests in the repositories where the token owner can push.
- The new `merge_pull_request` MCP tool merges a pull request, or enables auto-merge, as the token owner. Each merge goes to the audit log.

### Fixes

- The `templ` dependency is now at version v0.3.1118. This version includes parser fixes and a security update.

## v0.1.0 - 2026-11-15

This is the first release of Runwall. It gives you a live dashboard for GitHub Actions runs across your organizations.

### New features

- Runwall is a live dashboard for GitHub Actions. It has a webhook run feed, GitHub sign-in, re-run and cancel with your own token, run detail pages, trends, cost estimates, kiosk links, and a read-only MCP endpoint. It ships as a single Go binary with SQLite and a multi-arch image.
- The dashboard has multi-select pickers for org, repo, and topic. The selection stays in the URL, and Runwall remembers the last selection in a cookie. Runwall also syncs repository topics from GitHub.
- The dashboard and the runs page can filter runs by the user who triggered them. The run page links the pull request, branch, commit, and user to GitHub or to their runs.
- The run page shows the jobs of the workflow as a dependency graph. The graph includes jobs that did not start yet.
- When you pick orgs in the dashboard filter, the repo, topic, and user pickers show only the items of those orgs.
- A new Pull requests page (`/pulls`) lists the open pull requests that you can push to. You can merge many pull requests or enable auto-merge in one step. The page shows kritika scores, checks, and workflow runs for the head commit.
- Each pull request has its own page at `/pulls/{repo}/{number}`. The page has merge controls and live workflow runs.

### Fixes

- Dashboard charts now update in place on a live refresh. Before, the charts went blank and played their animation again.
- The syncer now waits for the reconcile interval after each pass ends. The default `RECONCILE_INTERVAL` is now `10m`, and the log shows how many repos each pass handled.
- Cancel now works for queued runs that have no jobs. Before, the cancel failed with an error.
- The sidebar brand block now has the same height as the top bar.

### Other changes

- The README is now in Simplified Technical English.
- The Makefile is replaced with a `justfile`, and a Nix dev shell is added.
- The App now needs the `Contents: write`, `Pull requests: read and write`, and `Commit statuses: read` permissions for the Pull requests page.
- The file `env.example` replaces the 1Password-specific `secrets.op`. The command `just run` reads `.env`.
