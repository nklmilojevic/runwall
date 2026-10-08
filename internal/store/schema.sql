CREATE TABLE IF NOT EXISTS installations (
    id           INTEGER PRIMARY KEY,
    account      TEXT NOT NULL,
    account_type TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS repos (
    id              INTEGER PRIMARY KEY,
    installation_id INTEGER NOT NULL,
    owner           TEXT NOT NULL,
    name            TEXT NOT NULL,
    full_name       TEXT NOT NULL,
    default_branch  TEXT NOT NULL DEFAULT '',
    archived        INTEGER NOT NULL DEFAULT 0,
    fork            INTEGER NOT NULL DEFAULT 0,
    last_seen_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS repos_installation ON repos (installation_id);

CREATE TABLE IF NOT EXISTS runs (
    id             INTEGER PRIMARY KEY,
    repo_id        INTEGER NOT NULL,
    workflow_id    INTEGER NOT NULL DEFAULT 0,
    workflow_name  TEXT NOT NULL DEFAULT '',
    workflow_path  TEXT NOT NULL DEFAULT '',
    run_number     INTEGER NOT NULL DEFAULT 0,
    run_attempt    INTEGER NOT NULL DEFAULT 1,
    event          TEXT NOT NULL DEFAULT '',
    head_branch    TEXT NOT NULL DEFAULT '',
    head_sha       TEXT NOT NULL DEFAULT '',
    title          TEXT NOT NULL DEFAULT '',
    commit_message TEXT NOT NULL DEFAULT '',
    actor_login    TEXT NOT NULL DEFAULT '',
    actor_is_bot   INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT '',
    conclusion     TEXT NOT NULL DEFAULT '',
    pr_number      INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL DEFAULT 0,
    run_started_at INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL DEFAULT 0,
    html_url       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_repo ON runs (repo_id);
CREATE INDEX IF NOT EXISTS runs_activity ON runs (max(created_at, run_started_at) DESC);
CREATE INDEX IF NOT EXISTS runs_status ON runs (status);

CREATE TABLE IF NOT EXISTS jobs (
    id           INTEGER PRIMARY KEY,
    run_id       INTEGER NOT NULL,
    run_attempt  INTEGER NOT NULL DEFAULT 1,
    name         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT '',
    conclusion   TEXT NOT NULL DEFAULT '',
    labels       TEXT NOT NULL DEFAULT '',
    runner_name  TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL DEFAULT 0,
    started_at   INTEGER NOT NULL DEFAULT 0,
    completed_at INTEGER NOT NULL DEFAULT 0,
    html_url     TEXT NOT NULL DEFAULT '',
    steps        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_run ON jobs (run_id, run_attempt);

CREATE TABLE IF NOT EXISTS deliveries (
    id          TEXT PRIMARY KEY,
    received_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS notifications (
    run_id      INTEGER NOT NULL,
    run_attempt INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    sent_at     INTEGER NOT NULL,
    PRIMARY KEY (run_id, run_attempt, kind)
);

CREATE TABLE IF NOT EXISTS users (
    id         INTEGER PRIMARY KEY,
    login      TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT ''
);

-- Repos a user can see, refreshed from GitHub every few minutes.
CREATE TABLE IF NOT EXISTS user_repos (
    user_id   INTEGER NOT NULL,
    repo_id   INTEGER NOT NULL,
    can_write INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, repo_id)
);

CREATE TABLE IF NOT EXISTS sessions (
    id_hash            TEXT PRIMARY KEY,
    user_id            INTEGER NOT NULL DEFAULT 0,
    kiosk_id           INTEGER NOT NULL DEFAULT 0,
    access_token       BLOB,
    refresh_token      BLOB,
    access_expires_at  INTEGER NOT NULL DEFAULT 0,
    refresh_expires_at INTEGER NOT NULL DEFAULT 0,
    csrf               TEXT NOT NULL,
    repos_checked_at   INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    last_seen_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL,
    name         TEXT NOT NULL,
    hash         TEXT NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS kiosk_links (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    hash       TEXT NOT NULL UNIQUE,
    owners     TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      INTEGER NOT NULL,
    login   TEXT NOT NULL,
    action  TEXT NOT NULL,
    repo    TEXT NOT NULL,
    target  INTEGER NOT NULL,
    result  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS repo_scores (
    repo_id     INTEGER PRIMARY KEY,
    score       INTEGER NOT NULL,
    tier        TEXT NOT NULL,
    breakdown   TEXT NOT NULL,
    computed_at INTEGER NOT NULL
);
