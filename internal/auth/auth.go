// Package auth signs people in with GitHub (the App's user-authorization flow), keeps
// their sessions, and works out which repos each viewer may see and act on.
//
// The App's installation token is still used for all background work. A user's own
// token is used only to discover their repo access and to perform run actions, so
// GitHub enforces that user's permissions.
package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/nklmilojevic/runwall/internal/store"
)

const (
	sessionCookie = "session"
	oauthCookie   = "oauth"
	// how often a viewer's repo access is re-read from GitHub
	accessTTL = 10 * time.Minute
	// sessions idle longer than this are dropped
	idleTTL = 30 * 24 * time.Hour
)

type Config struct {
	ClientID      string
	ClientSecret  string
	BaseURL       string // the dashboard's external URL, e.g. http://127.0.0.1:8080
	WebURL        string // https://github.com
	APIURL        string // https://api.github.com
	Key           []byte // 32 bytes, encrypts tokens at rest and the OAuth cookie
	AllowedUsers  []string
	AllowedOwners []string // installation accounts whose repos count; empty means all
	Admins        []string
}

type Auth struct {
	cfg   Config
	store *store.Store
	log   *slog.Logger
	http  *http.Client
	now   func() time.Time
	aead  cipher.AEAD
}

func New(cfg Config, st *store.Store, log *slog.Logger) (*Auth, error) {
	if len(cfg.Key) != 32 {
		return nil, errors.New("session key must be 32 bytes")
	}
	block, err := aes.NewCipher(cfg.Key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.WebURL = strings.TrimRight(cfg.WebURL, "/")
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	return &Auth{cfg: cfg, store: st, log: log, http: &http.Client{Timeout: 20 * time.Second}, now: time.Now, aead: aead}, nil
}

// ParseKey accepts a 64-character hex key, or derives 32 bytes from any other string.
func ParseKey(s string) []byte {
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b
	}
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// Hash is how session IDs and API/kiosk tokens are stored. They are 256-bit random
// values, so an unsalted hash is enough to make a database leak useless.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RandomToken returns a URL-safe random string with the given prefix.
func RandomToken(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func (a *Auth) seal(plain string) []byte {
	if plain == "" {
		return nil
	}
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return a.aead.Seal(nonce, nonce, []byte(plain), nil)
}

func (a *Auth) open(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	n := a.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("ciphertext too short")
	}
	plain, err := a.aead.Open(nil, sealed[:n], sealed[n:], nil)
	return string(plain), err
}

func (a *Auth) secureCookies() bool { return strings.HasPrefix(a.cfg.BaseURL, "https://") }

func (a *Auth) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.secureCookies(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(maxAge.Seconds()),
	})
}

func (a *Auth) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: a.secureCookies(), MaxAge: -1})
}

// OAuth flow

type oauthState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Next     string `json:"n"`
}

// safeNext only allows local paths, so the login flow can't be used as an open redirect.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// Start redirects to GitHub's authorization page.
func (a *Auth) Start(w http.ResponseWriter, r *http.Request) {
	st := oauthState{State: RandomToken(""), Verifier: RandomToken(""), Next: safeNext(r.URL.Query().Get("next"))}
	b, _ := json.Marshal(st)
	a.setCookie(w, oauthCookie, base64.RawURLEncoding.EncodeToString(a.seal(string(b))), 10*time.Minute)
	challenge := sha256.Sum256([]byte(st.Verifier))
	q := url.Values{
		"client_id":             {a.cfg.ClientID},
		"redirect_uri":          {a.cfg.BaseURL + "/auth/callback"},
		"state":                 {st.State},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, a.cfg.WebURL+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
}

func (a *Auth) tokenRequest(ctx context.Context, params url.Values) (tokenResponse, error) {
	params.Set("client_id", a.cfg.ClientID)
	params.Set("client_secret", a.cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.WebURL+"/login/oauth/access_token?"+params.Encode(), nil)
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return tr, fmt.Errorf("token response (%s): %w", resp.Status, err)
	}
	if tr.Error != "" || tr.AccessToken == "" {
		return tr, fmt.Errorf("github: %s %s", tr.Error, tr.ErrorDescription)
	}
	return tr, nil
}

// ErrNotAllowed means GitHub authenticated the user but they may not use this dashboard.
var ErrNotAllowed = errors.New("not allowed")

// Callback finishes the OAuth flow, creates a session and redirects to where the user started.
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) (string, error) {
	c, err := r.Cookie(oauthCookie)
	if err != nil {
		return "", errors.New("sign-in expired; start again")
	}
	a.clearCookie(w, oauthCookie)
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return "", errors.New("bad sign-in state")
	}
	plain, err := a.open(raw)
	if err != nil {
		return "", errors.New("bad sign-in state")
	}
	var st oauthState
	if err := json.Unmarshal([]byte(plain), &st); err != nil || st.State == "" || r.URL.Query().Get("state") != st.State {
		return "", errors.New("sign-in state mismatch; start again")
	}
	if e := r.URL.Query().Get("error"); e != "" {
		return "", fmt.Errorf("GitHub declined the sign-in: %s", r.URL.Query().Get("error_description"))
	}

	ctx := r.Context()
	tr, err := a.tokenRequest(ctx, url.Values{
		"code":          {r.URL.Query().Get("code")},
		"redirect_uri":  {a.cfg.BaseURL + "/auth/callback"},
		"code_verifier": {st.Verifier},
	})
	if err != nil {
		return "", err
	}
	gh, err := a.client(tr.AccessToken)
	if err != nil {
		return "", err
	}
	u, _, err := gh.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("read GitHub user: %w", err)
	}
	user := store.User{ID: u.GetID(), Login: u.GetLogin(), Name: u.GetName(), AvatarURL: u.GetAvatarURL()}
	if err := a.store.UpsertUser(ctx, user); err != nil {
		return "", err
	}
	n, err := a.syncAccess(ctx, gh, user.ID)
	if err != nil {
		return "", err
	}
	if !a.allowed(user.Login, n) {
		a.log.Warn("sign-in refused", "login", user.Login, "repos", n)
		return "", ErrNotAllowed
	}

	now := a.now()
	sid := RandomToken("")
	se := store.Session{
		IDHash: Hash(sid), UserID: user.ID, CSRF: RandomToken(""),
		AccessToken: a.seal(tr.AccessToken), RefreshToken: a.seal(tr.RefreshToken),
		ReposCheckedAt: now, CreatedAt: now, LastSeenAt: now,
	}
	if tr.ExpiresIn > 0 {
		se.AccessExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	if tr.RefreshTokenExpiresIn > 0 {
		se.RefreshExpiresAt = now.Add(time.Duration(tr.RefreshTokenExpiresIn) * time.Second)
	}
	if err := a.store.SaveSession(ctx, se); err != nil {
		return "", err
	}
	a.setCookie(w, sessionCookie, sid, idleTTL)
	a.log.Info("signed in", "login", user.Login, "repos", n)
	return st.Next, nil
}

func (a *Auth) allowed(login string, repos int) bool {
	if len(a.cfg.AllowedUsers) > 0 {
		return containsFold(a.cfg.AllowedUsers, login)
	}
	return repos > 0
}

func containsFold(list []string, v string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.EqualFold(s, v) })
}

func (a *Auth) client(token string) (*github.Client, error) {
	base := a.cfg.APIURL + "/"
	return github.NewClient(github.WithAuthToken(token), github.WithURLs(&base, nil))
}

// syncAccess stores the repos the user can see across the App's installations, and returns how many.
func (a *Auth) syncAccess(ctx context.Context, gh *github.Client, userID int64) (int, error) {
	known, err := a.store.ListRepos(ctx, 0)
	if err != nil {
		return 0, err
	}
	local := make(map[int64]bool, len(known))
	for _, r := range known {
		local[r.ID] = true
	}

	var access []store.UserRepo
	opts := &github.ListOptions{PerPage: 100}
	for {
		insts, resp, err := gh.Apps.ListUserInstallations(ctx, opts)
		if err != nil {
			return 0, fmt.Errorf("list installations for user: %w", err)
		}
		for _, inst := range insts {
			if len(a.cfg.AllowedOwners) > 0 && !containsFold(a.cfg.AllowedOwners, inst.GetAccount().GetLogin()) {
				continue
			}
			ropts := &github.ListOptions{PerPage: 100}
			for {
				page, rresp, err := gh.Apps.ListUserRepos(ctx, inst.GetID(), ropts)
				if err != nil {
					return 0, fmt.Errorf("list repos of installation %d for user: %w", inst.GetID(), err)
				}
				for _, repo := range page.Repositories {
					if !local[repo.GetID()] {
						continue
					}
					p := repo.GetPermissions()
					access = append(access, store.UserRepo{RepoID: repo.GetID(), CanWrite: p.GetPush() || p.GetMaintain() || p.GetAdmin()})
				}
				if rresp.NextPage == 0 {
					break
				}
				ropts.Page = rresp.NextPage
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return len(access), a.store.SetUserRepos(ctx, userID, access)
}

// Viewers

// Viewer is who is looking at the page.
type Viewer struct {
	User    store.User
	Kiosk   *store.KioskLink
	Scope   store.Scope
	Admin   bool
	CSRF    string
	Demo    bool
	session store.Session
	auth    *Auth
}

func (v *Viewer) SignedIn() bool { return v.User.ID != 0 || v.Demo }

// CanAct reports whether the viewer may use run actions on a repo.
func (v *Viewer) CanAct(ctx context.Context, repoID int64) bool {
	switch {
	case v.Demo:
		return true
	case v.Kiosk != nil || v.User.ID == 0:
		return false
	}
	ok, err := v.auth.store.CanWrite(ctx, v.User.ID, repoID)
	return err == nil && ok
}

// GitHub returns a client authenticated as the viewer, refreshing the token if needed.
func (v *Viewer) GitHub(ctx context.Context) (*github.Client, error) {
	if v.User.ID == 0 || v.auth == nil {
		return nil, errors.New("not signed in")
	}
	token, err := v.auth.accessToken(ctx, &v.session)
	if err != nil {
		return nil, err
	}
	return v.auth.client(token)
}

type viewerKey struct{}

func WithViewer(ctx context.Context, v *Viewer) context.Context {
	return context.WithValue(ctx, viewerKey{}, v)
}

func FromContext(ctx context.Context) *Viewer {
	v, _ := ctx.Value(viewerKey{}).(*Viewer)
	return v
}

// DemoViewer sees everything; used when the dashboard runs without GitHub.
func DemoViewer() *Viewer {
	return &Viewer{User: store.User{ID: 1, Login: "demo", Name: "Demo"}, Scope: store.Scope{All: true}, Admin: true, CSRF: "demo", Demo: true}
}

// accessToken returns a valid user token, refreshing it when it is about to expire.
func (a *Auth) accessToken(ctx context.Context, se *store.Session) (string, error) {
	now := a.now()
	if se.AccessExpiresAt.IsZero() || now.Before(se.AccessExpiresAt.Add(-5*time.Minute)) {
		return a.open(se.AccessToken)
	}
	refresh, err := a.open(se.RefreshToken)
	if err != nil || refresh == "" || (!se.RefreshExpiresAt.IsZero() && now.After(se.RefreshExpiresAt)) {
		return "", errors.New("GitHub sign-in expired")
	}
	tr, err := a.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if err != nil {
		return "", fmt.Errorf("refresh token: %w", err)
	}
	se.AccessToken, se.RefreshToken = a.seal(tr.AccessToken), a.seal(tr.RefreshToken)
	se.AccessExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.RefreshTokenExpiresIn > 0 {
		se.RefreshExpiresAt = now.Add(time.Duration(tr.RefreshTokenExpiresIn) * time.Second)
	}
	return tr.AccessToken, a.store.SaveSession(ctx, *se)
}

// Load resolves the request's session. It returns nil when there is no valid session.
func (a *Auth) Load(w http.ResponseWriter, r *http.Request) *Viewer {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	ctx := r.Context()
	se, err := a.store.GetSession(ctx, Hash(c.Value))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			a.log.Error("load session", "err", err)
		}
		a.clearCookie(w, sessionCookie)
		return nil
	}
	now := a.now()
	if now.Sub(se.LastSeenAt) > idleTTL {
		a.dropSession(ctx, se.IDHash)
		a.clearCookie(w, sessionCookie)
		return nil
	}
	v := &Viewer{CSRF: se.CSRF, session: se, auth: a}

	if se.KioskID != 0 {
		k, err := a.store.GetKioskLink(ctx, se.KioskID)
		if err != nil {
			a.dropSession(ctx, se.IDHash)
			a.clearCookie(w, sessionCookie)
			return nil
		}
		v.Kiosk, v.Scope = &k, store.Scope{Owners: k.Owners}
		a.touch(ctx, &se, now)
		return v
	}

	u, err := a.store.GetUser(ctx, se.UserID)
	if err != nil {
		a.dropSession(ctx, se.IDHash)
		a.clearCookie(w, sessionCookie)
		return nil
	}
	v.User, v.Scope, v.Admin = u, store.Scope{UserID: u.ID}, containsFold(a.cfg.Admins, u.Login)

	if now.Sub(se.ReposCheckedAt) > accessTTL {
		gh, err := v.GitHub(ctx)
		if err != nil {
			a.log.Info("session ended", "login", u.Login, "reason", err)
			a.dropSession(ctx, se.IDHash)
			a.clearCookie(w, sessionCookie)
			return nil
		}
		n, err := a.syncAccess(ctx, gh, u.ID)
		switch {
		case err != nil:
			// Keep the last known access rather than locking people out during a GitHub hiccup.
			a.log.Error("refresh repo access", "login", u.Login, "err", err)
		case !a.allowed(u.Login, n):
			a.dropSession(ctx, se.IDHash)
			a.clearCookie(w, sessionCookie)
			return nil
		}
		v.session.ReposCheckedAt = now
		v.session.LastSeenAt = now
		if err := a.store.SaveSession(ctx, v.session); err != nil {
			a.log.Error("save session", "err", err)
		}
		return v
	}
	a.touch(ctx, &v.session, now)
	return v
}

func (a *Auth) touch(ctx context.Context, se *store.Session, now time.Time) {
	if now.Sub(se.LastSeenAt) > time.Minute {
		se.LastSeenAt = now
		if err := a.store.TouchSession(ctx, se.IDHash, now); err != nil {
			a.log.Error("touch session", "err", err)
		}
	}
}

func (a *Auth) dropSession(ctx context.Context, idHash string) {
	if err := a.store.DeleteSession(ctx, idHash); err != nil {
		a.log.Error("delete session", "err", err)
	}
}

// Logout ends the session.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.dropSession(r.Context(), Hash(c.Value))
	}
	a.clearCookie(w, sessionCookie)
}

// OpenKiosk turns a kiosk link token into a read-only session.
func (a *Auth) OpenKiosk(w http.ResponseWriter, r *http.Request, token string) error {
	ctx := r.Context()
	k, err := a.store.KioskLinkByHash(ctx, Hash(token))
	if err != nil {
		return err
	}
	now := a.now()
	sid := RandomToken("")
	if err := a.store.SaveSession(ctx, store.Session{IDHash: Hash(sid), KioskID: k.ID, CSRF: RandomToken(""), CreatedAt: now, LastSeenAt: now}); err != nil {
		return err
	}
	a.setCookie(w, sessionCookie, sid, idleTTL)
	return nil
}

// APIViewer resolves a personal API token (for MCP) to a viewer.
func (a *Auth) APIViewer(ctx context.Context, token string) (*Viewer, error) {
	userID, err := a.store.UseAPIToken(ctx, Hash(token), a.now())
	if err != nil {
		return nil, err
	}
	u, err := a.store.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &Viewer{User: u, Scope: store.Scope{UserID: u.ID}, Admin: containsFold(a.cfg.Admins, u.Login), auth: a}, nil
}

// PruneSessions drops idle sessions.
func (a *Auth) PruneSessions(ctx context.Context) error {
	return a.store.PruneSessions(ctx, a.now().Add(-idleTTL))
}
