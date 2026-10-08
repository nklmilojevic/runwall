package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type User struct {
	ID        int64
	Login     string
	Name      string
	AvatarURL string
}

func (s *Store) UpsertUser(ctx context.Context, u User) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, login, name, avatar_url) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET login = excluded.login, name = excluded.name, avatar_url = excluded.avatar_url`,
		u.ID, u.Login, u.Name, u.AvatarURL)
	return err
}

func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT id, login, name, avatar_url FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Login, &u.Name, &u.AvatarURL)
	return u, err
}

// UserRepo is one repo a user can access, and whether they can push to it.
type UserRepo struct {
	RepoID   int64
	CanWrite bool
}

// SetUserRepos replaces the set of repos a user can see.
func (s *Store) SetUserRepos(ctx context.Context, userID int64, repos []UserRepo) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_repos WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO user_repos (user_id, repo_id, can_write) VALUES (?, ?, ?)`,
			userID, r.RepoID, boolInt(r.CanWrite)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CanWrite(ctx context.Context, userID, repoID int64) (bool, error) {
	var w int
	err := s.db.QueryRowContext(ctx, `SELECT can_write FROM user_repos WHERE user_id = ? AND repo_id = ?`, userID, repoID).Scan(&w)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return w == 1, err
}

// WritableRepos returns the IDs of repos the user can push to.
func (s *Store) WritableRepos(ctx context.Context, userID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id FROM user_repos WHERE user_id = ? AND can_write = 1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Sessions

type Session struct {
	IDHash           string
	UserID           int64
	KioskID          int64
	AccessToken      []byte // encrypted
	RefreshToken     []byte // encrypted
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	CSRF             string
	ReposCheckedAt   time.Time
	CreatedAt        time.Time
	LastSeenAt       time.Time
}

const sessionColumns = `id_hash, user_id, kiosk_id, access_token, refresh_token, access_expires_at, refresh_expires_at,
	csrf, repos_checked_at, created_at, last_seen_at`

func (s *Store) SaveSession(ctx context.Context, se Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		se.IDHash, se.UserID, se.KioskID, se.AccessToken, se.RefreshToken, unix(se.AccessExpiresAt), unix(se.RefreshExpiresAt),
		se.CSRF, unix(se.ReposCheckedAt), unix(se.CreatedAt), unix(se.LastSeenAt))
	return err
}

func (s *Store) GetSession(ctx context.Context, idHash string) (Session, error) {
	var se Session
	var ae, re, rc, ca, ls int64
	err := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id_hash = ?`, idHash).Scan(
		&se.IDHash, &se.UserID, &se.KioskID, &se.AccessToken, &se.RefreshToken, &ae, &re, &se.CSRF, &rc, &ca, &ls)
	se.AccessExpiresAt, se.RefreshExpiresAt, se.ReposCheckedAt = fromUnix(ae), fromUnix(re), fromUnix(rc)
	se.CreatedAt, se.LastSeenAt = fromUnix(ca), fromUnix(ls)
	return se, err
}

func (s *Store) TouchSession(ctx context.Context, idHash string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?`, unix(at), idHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// PruneSessions removes sessions idle since before the cutoff.
func (s *Store) PruneSessions(ctx context.Context, idleBefore time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE last_seen_at < ?`, unix(idleBefore))
	return err
}

// API tokens

type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name, hash string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens (user_id, name, hash, created_at) VALUES (?, ?, ?, ?)`,
		userID, name, hash, unix(at))
	return err
}

func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, name, created_at, last_used_at FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var ca, lu int64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &ca, &lu); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsedAt = fromUnix(ca), fromUnix(lu)
		out = append(out, t)
	}
	return out, rows.Err()
}

// UseAPIToken resolves a token hash to its user and records the use.
func (s *Store) UseAPIToken(ctx context.Context, hash string, at time.Time) (int64, error) {
	var id, userID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id, user_id FROM api_tokens WHERE hash = ?`, hash).Scan(&id, &userID); err != nil {
		return 0, err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, unix(at), id)
	return userID, err
}

func (s *Store) DeleteAPIToken(ctx context.Context, userID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

// Kiosk links

type KioskLink struct {
	ID        int64
	Name      string
	Owners    []string
	CreatedBy string
	CreatedAt time.Time
}

func (s *Store) CreateKioskLink(ctx context.Context, name, hash string, owners []string, by string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kiosk_links (name, hash, owners, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, hash, strings.Join(owners, ","), by, unix(at))
	return err
}

func scanKiosk(sc interface{ Scan(...any) error }) (KioskLink, error) {
	var k KioskLink
	var owners string
	var ca int64
	err := sc.Scan(&k.ID, &k.Name, &owners, &k.CreatedBy, &ca)
	if owners != "" {
		k.Owners = strings.Split(owners, ",")
	}
	k.CreatedAt = fromUnix(ca)
	return k, err
}

func (s *Store) KioskLinkByHash(ctx context.Context, hash string) (KioskLink, error) {
	return scanKiosk(s.db.QueryRowContext(ctx, `SELECT id, name, owners, created_by, created_at FROM kiosk_links WHERE hash = ?`, hash))
}

func (s *Store) GetKioskLink(ctx context.Context, id int64) (KioskLink, error) {
	return scanKiosk(s.db.QueryRowContext(ctx, `SELECT id, name, owners, created_by, created_at FROM kiosk_links WHERE id = ?`, id))
}

func (s *Store) ListKioskLinks(ctx context.Context) ([]KioskLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, owners, created_by, created_at FROM kiosk_links ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KioskLink
	for rows.Next() {
		k, err := scanKiosk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteKioskLink revokes a link and ends every session opened with it.
func (s *Store) DeleteKioskLink(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE kiosk_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM kiosk_links WHERE id = ?`, id)
	return err
}

// Audit log

type AuditEntry struct {
	At     time.Time
	Login  string
	Action string
	Repo   string
	Target int64
	Result string
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (at, login, action, repo, target, result) VALUES (?, ?, ?, ?, ?, ?)`,
		unix(e.At), e.Login, e.Action, e.Repo, e.Target, e.Result)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, login, action, repo, target, result FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&at, &e.Login, &e.Action, &e.Repo, &e.Target, &e.Result); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Repo scores

type RepoScore struct {
	RepoID     int64
	Score      int
	Tier       string
	Breakdown  string // JSON, owned by the scoring package
	ComputedAt time.Time
}

func (s *Store) SaveScore(ctx context.Context, sc RepoScore) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO repo_scores (repo_id, score, tier, breakdown, computed_at) VALUES (?, ?, ?, ?, ?)`,
		sc.RepoID, sc.Score, sc.Tier, sc.Breakdown, unix(sc.ComputedAt))
	return err
}

func (s *Store) GetScore(ctx context.Context, repoID int64) (RepoScore, error) {
	var sc RepoScore
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT repo_id, score, tier, breakdown, computed_at FROM repo_scores WHERE repo_id = ?`, repoID).
		Scan(&sc.RepoID, &sc.Score, &sc.Tier, &sc.Breakdown, &at)
	sc.ComputedAt = fromUnix(at)
	return sc, err
}

func (s *Store) ListScores(ctx context.Context) (map[int64]RepoScore, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id, score, tier, breakdown, computed_at FROM repo_scores`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]RepoScore{}
	for rows.Next() {
		var sc RepoScore
		var at int64
		if err := rows.Scan(&sc.RepoID, &sc.Score, &sc.Tier, &sc.Breakdown, &at); err != nil {
			return nil, err
		}
		sc.ComputedAt = fromUnix(at)
		out[sc.RepoID] = sc
	}
	return out, rows.Err()
}
