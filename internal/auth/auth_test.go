package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nklmilojevic/runwall/internal/store"
	"github.com/nklmilojevic/runwall/internal/testutil"
)

type fakeGitHub struct {
	t         *testing.T
	mu        sync.Mutex
	challenge string
	tokens    int
	refreshes int
	login     string
	push      bool
	expiresIn int64
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		if q.Get("client_id") != "cid" || q.Get("client_secret") != "csecret" {
			write(w, map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		switch q.Get("grant_type") {
		case "refresh_token":
			if q.Get("refresh_token") != "ghr_1" {
				write(w, map[string]string{"error": "bad_refresh_token"})
				return
			}
			f.refreshes++
			write(w, map[string]any{"access_token": "ghu_2", "expires_in": 28800, "refresh_token": "ghr_2", "refresh_token_expires_in": 15897600})
		default:
			sum := sha256.Sum256([]byte(q.Get("code_verifier")))
			if q.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				write(w, map[string]string{"error": "bad_verification_code"})
				return
			}
			f.tokens++
			write(w, map[string]any{"access_token": "ghu_1", "expires_in": f.expiresIn, "refresh_token": "ghr_1", "refresh_token_expires_in": 15897600})
		}
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if a := r.Header.Get("Authorization"); a != "Bearer ghu_1" && a != "Bearer ghu_2" {
				http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /user", auth(func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"id": 42, "login": f.login, "name": "Nikola", "avatar_url": "https://avatars/x"})
	}))
	mux.HandleFunc("GET /user/installations", auth(func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"total_count": 2, "installations": []map[string]any{
			{"id": 7, "account": map[string]any{"login": "acme"}},
			{"id": 8, "account": map[string]any{"login": "stranger"}},
		}})
	}))
	mux.HandleFunc("GET /user/installations/{id}/repositories", auth(func(w http.ResponseWriter, r *http.Request) {
		repos := []map[string]any{}
		switch r.PathValue("id") {
		case "7":
			repos = append(repos,
				map[string]any{"id": 10, "full_name": "acme/api", "permissions": map[string]bool{"pull": true, "push": f.push}},
				map[string]any{"id": 11, "full_name": "acme/web", "permissions": map[string]bool{"pull": true}},
				map[string]any{"id": 99, "full_name": "acme/unknown-to-dashboard", "permissions": map[string]bool{"admin": true}})
		case "8":
			repos = append(repos, map[string]any{"id": 20, "full_name": "stranger/x", "permissions": map[string]bool{"admin": true}})
		}
		write(w, map[string]any{"total_count": len(repos), "repositories": repos})
	}))
	return mux
}

func setup(t *testing.T, mutate func(*Config)) (*Auth, *fakeGitHub, *store.Store) {
	t.Helper()
	f := &fakeGitHub{t: t, login: "nkl", push: true, expiresIn: 28800}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	st := testutil.Store(t)
	ctx := context.Background()
	for _, r := range []store.Repo{{ID: 10, Owner: "acme", Name: "api"}, {ID: 11, Owner: "acme", Name: "web"}, {ID: 20, Owner: "stranger", Name: "x"}} {
		r.FullName = r.Owner + "/" + r.Name
		st.UpsertRepo(ctx, r)
	}
	cfg := Config{ClientID: "cid", ClientSecret: "csecret", BaseURL: "http://dash.local", WebURL: srv.URL, APIURL: srv.URL,
		Key: ParseKey(strings.Repeat("ab", 32)), AllowedOwners: []string{"acme"}, Admins: []string{"NKL"}}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := New(cfg, st, testutil.Logger())
	if err != nil {
		t.Fatal(err)
	}
	return a, f, st
}

// signIn runs the browser side of the flow and returns the session cookie.
func signIn(t *testing.T, a *Auth, f *fakeGitHub, next string) (*http.Cookie, string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.Start(rec, httptest.NewRequest(http.MethodGet, "/auth/start?next="+url.QueryEscape(next), nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("client_id") != "cid" || q.Get("redirect_uri") != "http://dash.local/auth/callback" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL %s", loc)
	}
	f.challenge = q.Get("code_challenge")
	var oauth *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthCookie {
			oauth = c
		}
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?code=the-code&state="+q.Get("state"), nil)
	cb.AddCookie(oauth)
	rec = httptest.NewRecorder()
	dest, err := a.Callback(rec, cb)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge > 0 {
			return c, dest, err
		}
	}
	return nil, dest, err
}

func load(a *Auth, c *http.Cookie) *Viewer {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if c != nil {
		r.AddCookie(c)
	}
	return a.Load(httptest.NewRecorder(), r)
}

func TestSignInFlow(t *testing.T) {
	a, f, st := setup(t, nil)
	ctx := context.Background()
	c, dest, err := signIn(t, a, f, "/runs?org=acme")
	if err != nil {
		t.Fatal(err)
	}
	if dest != "/runs?org=acme" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("dest %q cookie %+v", dest, c)
	}

	v := load(a, c)
	if v == nil || v.User.Login != "nkl" || !v.Admin || v.Scope.UserID != 42 {
		t.Fatalf("viewer %+v", v)
	}
	// Only repos the dashboard knows about, from allowed installations.
	for id, want := range map[int64]bool{10: true, 11: true, 20: false} {
		if ok, _ := st.CanSeeRepo(ctx, v.Scope, id); ok != want {
			t.Errorf("repo %d visible = %v", id, ok)
		}
	}
	if !v.CanAct(ctx, 10) || v.CanAct(ctx, 11) {
		t.Fatal("write access should follow the user's push permission")
	}

	// Stored tokens are encrypted.
	se, _ := st.GetSession(ctx, Hash(c.Value))
	if strings.Contains(string(se.AccessToken), "ghu_") || strings.Contains(string(se.RefreshToken), "ghr_") {
		t.Fatal("tokens must not be stored in plain text")
	}

	a.Logout(httptest.NewRecorder(), func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		r.AddCookie(c)
		return r
	}())
	if load(a, c) != nil {
		t.Fatal("session should be gone after logout")
	}
}

func TestCallbackRejectsForgedState(t *testing.T) {
	a, _, _ := setup(t, nil)
	rec := httptest.NewRecorder()
	a.Start(rec, httptest.NewRequest(http.MethodGet, "/auth/start", nil))
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?code=the-code&state=forged", nil)
	cb.AddCookie(rec.Result().Cookies()[0])
	if _, err := a.Callback(httptest.NewRecorder(), cb); err == nil {
		t.Fatal("state mismatch must fail")
	}
	if _, err := a.Callback(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=y", nil)); err == nil {
		t.Fatal("missing state cookie must fail")
	}
}

func TestAllowLists(t *testing.T) {
	a, f, _ := setup(t, func(c *Config) { c.AllowedUsers = []string{"someone-else"} })
	if _, _, err := signIn(t, a, f, "/"); err != ErrNotAllowed {
		t.Fatalf("user outside ALLOWED_USERS: %v", err)
	}

	// Without a user list, access to at least one repo is required.
	a, f, _ = setup(t, func(c *Config) { c.AllowedOwners = []string{"nobody"} })
	if _, _, err := signIn(t, a, f, "/"); err != ErrNotAllowed {
		t.Fatalf("user with no repos: %v", err)
	}
}

func TestTokenRefreshAndAccessRecheck(t *testing.T) {
	a, f, st := setup(t, nil)
	ctx := context.Background()
	c, _, err := signIn(t, a, f, "/")
	if err != nil {
		t.Fatal(err)
	}
	// Nine hours later the access token has expired and repo access is due a recheck.
	a.now = func() time.Time { return time.Now().Add(9 * time.Hour) }
	f.push = false
	v := load(a, c)
	if v == nil {
		t.Fatal("session should survive via refresh token")
	}
	if f.refreshes != 1 {
		t.Fatalf("expected one refresh, got %d", f.refreshes)
	}
	if v.CanAct(ctx, 10) {
		t.Fatal("revoked push access should be picked up on recheck")
	}
	se, _ := st.GetSession(ctx, Hash(c.Value))
	if tok, _ := a.open(se.AccessToken); tok != "ghu_2" {
		t.Fatalf("refreshed token not stored, got %q", tok)
	}
}

func TestKioskAndAPITokens(t *testing.T) {
	a, f, st := setup(t, nil)
	ctx := context.Background()
	token := RandomToken("kiosk_")
	st.CreateKioskLink(ctx, "Office TV", Hash(token), []string{"acme"}, "nkl", time.Now())

	rec := httptest.NewRecorder()
	if err := a.OpenKiosk(rec, httptest.NewRequest(http.MethodGet, "/kiosk/x", nil), token); err != nil {
		t.Fatal(err)
	}
	v := load(a, rec.Result().Cookies()[0])
	if v == nil || v.Kiosk == nil || v.SignedIn() || v.CanAct(ctx, 10) {
		t.Fatalf("kiosk viewer %+v", v)
	}
	if ok, _ := st.CanSeeRepo(ctx, v.Scope, 20); ok {
		t.Fatal("kiosk sees only its owners")
	}
	if err := a.OpenKiosk(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "wrong"); err == nil {
		t.Fatal("unknown kiosk token")
	}
	links, _ := st.ListKioskLinks(ctx)
	st.DeleteKioskLink(ctx, links[0].ID)
	if load(a, rec.Result().Cookies()[0]) != nil {
		t.Fatal("revoking a kiosk link ends its sessions")
	}

	if _, _, err := signIn(t, a, f, "/"); err != nil {
		t.Fatal(err)
	}
	api := RandomToken("gad_")
	st.CreateAPIToken(ctx, 42, "laptop", Hash(api), time.Now())
	av, err := a.APIViewer(ctx, api)
	if err != nil || av.User.Login != "nkl" || av.Scope.UserID != 42 {
		t.Fatalf("api viewer %+v %v", av, err)
	}
	if _, err := a.APIViewer(ctx, "gad_wrong"); err == nil {
		t.Fatal("unknown API token")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/runs": "/runs", "https://evil.com": "/", "//evil.com": "/", "/\\evil.com": "/", "": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
}
