package httpapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"

	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/oidc"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

const testOIDCClientID = "aegisflow-test"

// ---------------------------------------------------------------------------
// Fake OIDC provider: discovery + JWKS + authorize + token, RSA/RS256 only.
// It verifies the PKCE S256 challenge like a real provider, so the end-to-end
// test would fail if the server sent a wrong verifier or challenge.
// ---------------------------------------------------------------------------

type fakeOIDCIdentity struct {
	email             string
	preferredUsername string
	name              string
	emailVerified     bool
}

type fakeOIDCCode struct {
	nonce       string
	challenge   string
	redirectURI string
}

type fakeOIDCProvider struct {
	server *httptest.Server

	mu          sync.Mutex
	keys        map[string]*rsa.PrivateKey
	published   []string
	signKid     string
	identity    fakeOIDCIdentity
	mutate      func(map[string]any)
	tokenStatus int
	tokenBody   string
	// idTokenOverride, when non-empty, is returned verbatim as id_token so a
	// test can present a structurally invalid/unsigned token.
	idTokenOverride string
	codes           map[string]fakeOIDCCode
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	key := generateRSAKey(t)
	fake := &fakeOIDCProvider{
		keys:        map[string]*rsa.PrivateKey{"kid-1": key},
		published:   []string{"kid-1"},
		signKid:     "kid-1",
		identity:    fakeOIDCIdentity{email: "linked@example.com", preferredUsername: "linked-user", name: "Linked User", emailVerified: true},
		tokenStatus: http.StatusOK,
		codes:       map[string]fakeOIDCCode{},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeOIDCProvider) issuer() string { return f.server.URL }

func (f *fakeOIDCProvider) setIdentity(identity fakeOIDCIdentity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identity = identity
}

func (f *fakeOIDCProvider) setMutate(mutate func(map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutate = mutate
}

func (f *fakeOIDCProvider) setTokenStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenStatus = status
}

func (f *fakeOIDCProvider) setTokenBody(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenBody = body
}

func (f *fakeOIDCProvider) setIDTokenOverride(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idTokenOverride = token
}

// addSigningKey simulates key rotation: the new key becomes the active signing
// key and both keys stay published, so a server that never refreshes its JWKS
// would reject the new kid.
func (f *fakeOIDCProvider) addSigningKey(t *testing.T, kid string) {
	t.Helper()
	key := generateRSAKey(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[kid] = key
	f.published = append(f.published, kid)
	f.signKid = kid
}

// addUnpublishedKey signs tokens with a key the JWKS never advertises, which
// models an unknown kid that a correct verifier must reject.
func (f *fakeOIDCProvider) addUnpublishedKey(t *testing.T, kid string) {
	t.Helper()
	key := generateRSAKey(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[kid] = key
	f.signKid = kid
}

func (f *fakeOIDCProvider) codeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.codes)
}

func (f *fakeOIDCProvider) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		writeFakeJSON(w, map[string]any{
			"issuer":                 f.server.URL,
			"authorization_endpoint": f.server.URL + "/authorize",
			"token_endpoint":         f.server.URL + "/token",
			"jwks_uri":               f.server.URL + "/jwks",
		})
	case "/jwks":
		f.mu.Lock()
		keys := make([]any, 0, len(f.published))
		for _, kid := range f.published {
			key := f.keys[kid]
			if key == nil {
				continue
			}
			keys = append(keys, fakeJWK(&key.PublicKey, kid))
		}
		f.mu.Unlock()
		writeFakeJSON(w, map[string]any{"keys": keys})
	case "/authorize":
		query := r.URL.Query()
		code, err := oidc.RandomToken(16)
		if err != nil {
			http.Error(w, "cannot mint code", http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.codes[code] = fakeOIDCCode{
			nonce:       query.Get("nonce"),
			challenge:   query.Get("code_challenge"),
			redirectURI: query.Get("redirect_uri"),
		}
		f.mu.Unlock()
		redirect, err := url.Parse(query.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect", http.StatusBadRequest)
			return
		}
		values := redirect.Query()
		values.Set("code", code)
		values.Set("state", query.Get("state"))
		redirect.RawQuery = values.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	case "/token":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		code := r.PostFormValue("code")
		verifier := r.PostFormValue("code_verifier")
		f.mu.Lock()
		pending, ok := f.codes[code]
		delete(f.codes, code)
		status := f.tokenStatus
		body := f.tokenBody
		identity := f.identity
		mutate := f.mutate
		signKid := f.signKid
		key := f.keys[signKid]
		idTokenOverride := f.idTokenOverride
		f.mu.Unlock()
		if !ok || pending.challenge == "" || pending.challenge != oidc.PKCEChallenge(verifier) {
			http.Error(w, "provider says: invalid_grant secret-detail", http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			http.Error(w, body, status)
			return
		}
		if idTokenOverride != "" {
			writeFakeJSON(w, map[string]any{"id_token": idTokenOverride, "token_type": "Bearer"})
			return
		}
		if key == nil {
			http.Error(w, "missing signing key", http.StatusInternalServerError)
			return
		}
		now := time.Now()
		claims := map[string]any{
			"iss":                f.server.URL,
			"aud":                testOIDCClientID,
			"sub":                "external-subject",
			"email":              identity.email,
			"email_verified":     identity.emailVerified,
			"preferred_username": identity.preferredUsername,
			"name":               identity.name,
			"nonce":              pending.nonce,
			"iat":                now.Unix(),
			"exp":                now.Add(5 * time.Minute).Unix(),
		}
		if mutate != nil {
			mutate(claims)
		}
		token, err := signFakeIDToken(key, signKid, claims)
		if err != nil {
			http.Error(w, "cannot sign", http.StatusInternalServerError)
			return
		}
		writeFakeJSON(w, map[string]any{"id_token": token, "access_token": "unused", "token_type": "Bearer"})
	default:
		http.NotFound(w, r)
	}
}

func writeFakeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func generateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func fakeJWK(publicKey *rsa.PublicKey, kid string) map[string]any {
	encoder := base64.RawURLEncoding
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   encoder.EncodeToString(publicKey.N.Bytes()),
		"e":   encoder.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
	}
}

func signFakeIDToken(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	headerJSON, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoder := base64.RawURLEncoding
	unsigned := encoder.EncodeToString(headerJSON) + "." + encoder.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + encoder.EncodeToString(signature), nil
}

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// oidcTestLimiter is a deterministic test-only limiter with room for many
// legitimate flows inside one test. It is never a production fallback.
type oidcTestLimiter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newOIDCTestLimiter() *oidcTestLimiter { return &oidcTestLimiter{counts: map[string]int{}} }

func (l *oidcTestLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] >= 1000 {
		return false, 1500 * time.Millisecond, nil
	}
	l.counts[key]++
	return true, 0, nil
}

func oidcTestServer(t *testing.T, idp *fakeOIDCProvider, autoProvision bool, label string, options Options) (http.Handler, *auth.Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	service := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	ctx := context.Background()
	if err := service.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.Bootstrap(ctx, "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if options.Limiter == nil {
		options.Limiter = newOIDCTestLimiter()
	}
	sso, err := oidc.New(ctx, db, service, oidc.Options{
		Issuer:           idp.issuer(),
		ClientID:         testOIDCClientID,
		RedirectURL:      "http://localhost:5173/api/v1/auth/oidc/callback",
		Scopes:           []string{"openid", "profile", "email"},
		AutoProvision:    autoProvision,
		Label:            label,
		DiscoveryTimeout: 2 * time.Second,
		Logger:           logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	options.OIDC = sso
	return New(service, logger, db.PingContext, []string{"http://localhost:5173"}, options), service, db
}

func oidcGet(h http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func responseCookie(recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

type oidcFlow struct {
	stateCookie  *http.Cookie
	state        string
	authorizeURL string
}

func startOIDCFlow(t *testing.T, h http.Handler, redirectTo string) oidcFlow {
	t.Helper()
	path := "/api/v1/auth/oidc/start"
	if redirectTo != "" {
		path += "?redirectTo=" + url.QueryEscape(redirectTo)
	}
	recorder := oidcGet(h, path)
	if recorder.Code != http.StatusFound {
		t.Fatalf("start status = %d %s", recorder.Code, recorder.Body.String())
	}
	cookie := responseCookie(recorder, oidcStateCookieName)
	if cookie == nil {
		t.Fatal("start did not set the state cookie")
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := location.Query()
	for _, key := range []string{"state", "nonce", "code_challenge", "code_challenge_method", "client_id", "redirect_uri", "scope", "response_type"} {
		if query.Get(key) == "" {
			t.Fatalf("authorization URL misses %s: %s", key, location)
		}
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("response_type") != "code" {
		t.Fatalf("authorization URL params = %s", location)
	}
	return oidcFlow{stateCookie: cookie, state: query.Get("state"), authorizeURL: location.String()}
}

// authorize at the fake provider returns the code it registered for the flow.
func (f *fakeOIDCProvider) authorize(t *testing.T, flow oidcFlow) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(flow.authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("provider authorize status = %d", response.StatusCode)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("state") != flow.state {
		t.Fatal("provider did not echo the state")
	}
	return location.Query().Get("code")
}

func oidcCallback(h http.Handler, code, state string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

// runOIDCFlow performs start -> provider authorize -> callback and returns the
// callback response.
func runOIDCFlow(t *testing.T, h http.Handler, idp *fakeOIDCProvider, redirectTo string) *httptest.ResponseRecorder {
	t.Helper()
	flow := startOIDCFlow(t, h, redirectTo)
	code := idp.authorize(t, flow)
	return oidcCallback(h, code, flow.state, flow.stateCookie)
}

func assertSSOError(t *testing.T, recorder *httptest.ResponseRecorder, code string) {
	t.Helper()
	if recorder.Code != http.StatusFound {
		t.Fatalf("callback status = %d %s", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "/login?sso_error="+code {
		t.Fatalf("callback redirect = %q, want sso_error=%s", location, code)
	}
	// The one-time state cookie is cleared on every callback path.
	cookie := responseCookie(recorder, oidcStateCookieName)
	if cookie == nil || cookie.MaxAge != -1 || cookie.Value != "" {
		t.Fatalf("state cookie not cleared: %+v", cookie)
	}
}

func assertSSOSuccess(t *testing.T, recorder *httptest.ResponseRecorder, redirectTo string) *http.Cookie {
	t.Helper()
	if recorder.Code != http.StatusFound {
		t.Fatalf("callback status = %d %s", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != redirectTo {
		t.Fatalf("callback redirect = %q, want %q", location, redirectTo)
	}
	if cookie := responseCookie(recorder, oidcStateCookieName); cookie == nil || cookie.MaxAge != -1 {
		t.Fatalf("state cookie not cleared: %+v", cookie)
	}
	session := responseCookie(recorder, refreshCookieName)
	if session == nil || session.Value == "" {
		t.Fatal("callback did not set the refresh cookie")
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteStrictMode || session.Path != refreshCookiePath {
		t.Fatalf("unsafe refresh cookie: %+v", session)
	}
	return session
}

func refreshWithCookie(t *testing.T, h http.Handler, cookie *http.Cookie) auth.Tokens {
	t.Helper()
	recorder := cookieRequest(h, http.MethodPost, "/api/v1/auth/refresh", `{}`, cookie)
	if recorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Data
}

func linkedMember(t *testing.T, service *auth.Service, db *sql.DB, username, email string) *auth.User {
	t.Helper()
	user, err := service.CreateUser(context.Background(), auth.Actor{TenantID: tenants.DefaultID}, username, "a-strong-password", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET email=$1 WHERE id=$2`, email, user.ID); err != nil {
		t.Fatal(err)
	}
	return user
}

func auditCount(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE `+where, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// ---------------------------------------------------------------------------
// Configuration surface
// ---------------------------------------------------------------------------

func TestOIDCUnconfiguredIsFullyDisabled(t *testing.T) {
	h, _ := testServer(t)
	recorder := oidcGet(h, "/api/v1/auth/oidc/config")
	if recorder.Code != http.StatusOK {
		t.Fatalf("config status = %d", recorder.Code)
	}
	var result struct {
		Data oidcConfigData `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Enabled || result.Data.Label != "" {
		t.Fatalf("disabled config = %+v", result.Data)
	}
	for _, path := range []string{"/api/v1/auth/oidc/start", "/api/v1/auth/oidc/callback"} {
		recorder = oidcGet(h, path)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s when disabled = %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

type oidcConfigData struct {
	Enabled bool   `json:"enabled"`
	Label   string `json:"label"`
}

func TestOIDCConfigProbeAndStateCookie(t *testing.T) {
	for _, secure := range []bool{false, true} {
		idp := newFakeOIDCProvider(t)
		h, _, _ := oidcTestServer(t, idp, false, "公司单点登录", Options{CookieSecure: secure})

		recorder := oidcGet(h, "/api/v1/auth/oidc/config")
		if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), idp.issuer()) || strings.Contains(recorder.Body.String(), testOIDCClientID) {
			t.Fatalf("config response = %d %s", recorder.Code, recorder.Body.String())
		}
		var result struct {
			Data oidcConfigData `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Data.Enabled || result.Data.Label != "公司单点登录" {
			t.Fatalf("enabled config = %+v", result.Data)
		}

		startRecorder := oidcGet(h, "/api/v1/auth/oidc/start?redirectTo=%2Fbuilds")
		if startRecorder.Code != http.StatusFound {
			t.Fatalf("start status = %d %s", startRecorder.Code, startRecorder.Body.String())
		}
		cookie := responseCookie(startRecorder, oidcStateCookieName)
		if cookie == nil {
			t.Fatal("missing state cookie")
		}
		if !cookie.HttpOnly || cookie.Secure != secure || cookie.SameSite != http.SameSiteLaxMode ||
			cookie.Path != oidcStateCookiePath || cookie.MaxAge != oidcStateCookieMaxAge || cookie.Domain != "" {
			t.Fatalf("unsafe state cookie: %+v", cookie)
		}
		if location, err := url.Parse(startRecorder.Header().Get("Location")); err != nil || location.Query().Get("redirect_uri") != "http://localhost:5173/api/v1/auth/oidc/callback" {
			t.Fatalf("authorization redirect = %q", startRecorder.Header().Get("Location"))
		}
	}
}

// TestOIDCRejectsOpenRedirectsWithLinkedUser is the real open-redirect check:
// a successful SSO login for a linked account must never be sent off-origin.
func TestOIDCRejectsOpenRedirectsWithLinkedUser(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, service, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	linkedMember(t, service, db, "redirect-user", "redirect@example.com")
	idp.setIdentity(fakeOIDCIdentity{email: "redirect@example.com", preferredUsername: "redirect", name: "Redirect", emailVerified: true})
	for _, tc := range []struct{ redirectTo, want string }{
		{"https://evil.example/steal", "/"},
		{"//evil.example/steal", "/"},
		{"/\\evil.example", "/"},
		{"/%2F%2Fevil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"/builds?tab=logs", "/builds?tab=logs"},
		{"/", "/"},
	} {
		t.Run(tc.redirectTo, func(t *testing.T) {
			recorder := runOIDCFlow(t, h, idp, tc.redirectTo)
			assertSSOSuccess(t, recorder, tc.want)
		})
	}
}

// ---------------------------------------------------------------------------
// State handling
// ---------------------------------------------------------------------------

func TestOIDCStateCookieAndReplayProtection(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, service, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	linkedMember(t, service, db, "state-user", "state@example.com")
	idp.setIdentity(fakeOIDCIdentity{email: "state@example.com", preferredUsername: "state", name: "State", emailVerified: true})

	t.Run("missing cookie", func(t *testing.T) {
		flow := startOIDCFlow(t, h, "/")
		code := idp.authorize(t, flow)
		assertSSOError(t, oidcCallback(h, code, flow.state, nil), "state_expired")
	})
	t.Run("mismatched cookie", func(t *testing.T) {
		flow := startOIDCFlow(t, h, "/")
		code := idp.authorize(t, flow)
		wrong := &http.Cookie{Name: oidcStateCookieName, Value: "another-state-value"}
		assertSSOError(t, oidcCallback(h, code, flow.state, wrong), "state_expired")
	})
	t.Run("unknown state", func(t *testing.T) {
		flow := startOIDCFlow(t, h, "/")
		assertSSOError(t, oidcCallback(h, "any-code", "unknown-state-value", flow.stateCookie), "state_expired")
	})
	t.Run("replayed state after success", func(t *testing.T) {
		flow := startOIDCFlow(t, h, "/")
		code := idp.authorize(t, flow)
		assertSSOSuccess(t, oidcCallback(h, code, flow.state, flow.stateCookie), "/")
		// The same code/state/cookie cannot mint a second session: the row is
		// consumed and the cookie was cleared by the first callback.
		assertSSOError(t, oidcCallback(h, code, flow.state, flow.stateCookie), "state_expired")
	})
	t.Run("expired state row", func(t *testing.T) {
		flow := startOIDCFlow(t, h, "/")
		code := idp.authorize(t, flow)
		if _, err := db.Exec(`UPDATE oidc_login_states SET expires_at=$1 WHERE state_hash=$2`, time.Now().Add(-time.Minute).Unix(), oidc.StateHash(flow.state)); err != nil {
			t.Fatal(err)
		}
		assertSSOError(t, oidcCallback(h, code, flow.state, flow.stateCookie), "state_expired")
	})
	t.Run("consumed rows are not reusable", func(t *testing.T) {
		var consumed int
		if err := db.QueryRow(`SELECT COUNT(*) FROM oidc_login_states WHERE consumed_at IS NOT NULL`).Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if consumed == 0 {
			t.Fatal("no consumed state rows recorded")
		}
	})
}

// ---------------------------------------------------------------------------
// Account linkage and auto-provisioning
// ---------------------------------------------------------------------------

func TestOIDCLinkedAccountLogin(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, service, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	member := linkedMember(t, service, db, "linked-login", "Linked@Example.com")
	idp.setIdentity(fakeOIDCIdentity{email: "linked@example.COM", preferredUsername: "ignored", name: "Linked", emailVerified: true})

	recorder := runOIDCFlow(t, h, idp, "/builds")
	session := assertSSOSuccess(t, recorder, "/builds")
	if session.Secure || session.MaxAge != 0 {
		t.Fatalf("SSO session cookie should be a non-persistent, explicit-secure cookie: %+v", session)
	}
	tokens := refreshWithCookie(t, h, session)
	if tokens.User.ID != member.ID || tokens.User.Role != "member" || tokens.AccessToken == "" {
		t.Fatalf("SSO session user = %+v", tokens.User)
	}
	if auditCount(t, db, `action=$1 AND actor_id=$2 AND result=$3 AND detail=$4`, auth.ActionLogin, member.ID, auth.ResultSuccess, "method=oidc") != 1 {
		t.Fatal("missing method=oidc success audit row")
	}
	if auditCount(t, db, `action=$1 AND actor_id=$2 AND result=$3`, auth.ActionLogin, member.ID, auth.ResultFailure) != 0 {
		t.Fatal("unexpected failure audit row on success")
	}
}

func TestOIDCDisabledAccountIsRefused(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, service, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	member := linkedMember(t, service, db, "disabled-sso", "disabled@example.com")
	if err := service.SetUserDisabled(context.Background(), auth.Actor{TenantID: tenants.DefaultID}, member.ID, true); err != nil {
		t.Fatal(err)
	}
	idp.setIdentity(fakeOIDCIdentity{email: "disabled@example.com", preferredUsername: "disabled", name: "Disabled", emailVerified: true})
	recorder := runOIDCFlow(t, h, idp, "/")
	assertSSOError(t, recorder, "account_disabled")
	if auditCount(t, db, `action=$1 AND actor_id=$2 AND result=$3 AND detail=$4`, auth.ActionLogin, member.ID, auth.ResultFailure, "method=oidc code=account_disabled") != 1 {
		t.Fatal("missing account_disabled failure audit row")
	}
	var sessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_sessions WHERE user_id=$1`, member.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("disabled account got %d session(s)", sessions)
	}
}

func TestOIDCUnknownAccountWithoutProvisioning(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, _, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	idp.setIdentity(fakeOIDCIdentity{email: "stranger@example.com", preferredUsername: "stranger", name: "Stranger", emailVerified: true})
	recorder := runOIDCFlow(t, h, idp, "/")
	assertSSOError(t, recorder, "account_not_found")
	var users int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_users WHERE email=$1`, "stranger@example.com").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Fatal("account created while auto-provision is disabled")
	}
	if auditCount(t, db, `action=$1 AND result=$2 AND detail=$3`, auth.ActionLogin, auth.ResultFailure, "method=oidc code=account_not_found") != 1 {
		t.Fatal("missing account_not_found failure audit row")
	}
}

func TestOIDCAutoProvisionCreatesMemberOnly(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, _, db := oidcTestServer(t, idp, true, "企业单点登录", Options{})
	idp.setIdentity(fakeOIDCIdentity{email: "new-sso@example.com", preferredUsername: "new-sso", name: "New SSO", emailVerified: true})

	recorder := runOIDCFlow(t, h, idp, "/")
	session := assertSSOSuccess(t, recorder, "/")
	tokens := refreshWithCookie(t, h, session)
	if tokens.User.Username != "new-sso" || tokens.User.Role != "member" || tokens.User.DisplayName != "New SSO" {
		t.Fatalf("provisioned user = %+v", tokens.User)
	}
	var email, role string
	var disabled int
	if err := db.QueryRow(`SELECT email,role,disabled FROM auth_users WHERE id=$1`, tokens.User.ID).Scan(&email, &role, &disabled); err != nil {
		t.Fatal(err)
	}
	if email != "new-sso@example.com" || role != "member" || disabled != 0 {
		t.Fatalf("provisioned row = email %q role %q disabled %d", email, role, disabled)
	}
	if auditCount(t, db, `action=$1 AND object_id=$2 AND result=$3 AND detail=$4`, auth.ActionUserCreate, tokens.User.ID, auth.ResultSuccess, "method=oidc") != 1 {
		t.Fatal("missing auto-provision user.create audit row")
	}
	if auditCount(t, db, `action=$1 AND actor_id=$2 AND result=$3 AND detail=$4`, auth.ActionLogin, tokens.User.ID, auth.ResultSuccess, "method=oidc") != 1 {
		t.Fatal("missing auto-provision login audit row")
	}
}

// TestOIDCAutoProvisionNeverCreatesAdmin keeps the privilege boundary: a
// preferred_username that collides with the bootstrap admin gets a suffixed
// member account, and the existing administrator row is untouched.
func TestOIDCAutoProvisionNeverCreatesAdmin(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, _, db := oidcTestServer(t, idp, true, "企业单点登录", Options{})
	idp.setIdentity(fakeOIDCIdentity{email: "admin-collision@example.com", preferredUsername: "admin", name: "Admin Collision", emailVerified: true})
	recorder := runOIDCFlow(t, h, idp, "/")
	session := assertSSOSuccess(t, recorder, "/")
	tokens := refreshWithCookie(t, h, session)
	if tokens.User.Role != "member" || tokens.User.Username == "admin" || !strings.HasPrefix(tokens.User.Username, "admin-") {
		t.Fatalf("collision provisioned %+v", tokens.User)
	}
	var adminRole, adminEmail string
	if err := db.QueryRow(`SELECT role,email FROM auth_users WHERE username='admin'`).Scan(&adminRole, &adminEmail); err != nil {
		t.Fatal(err)
	}
	if adminRole != "admin" || adminEmail != "" {
		t.Fatalf("bootstrap admin changed: role %q email %q", adminRole, adminEmail)
	}
}

// TestOIDCLinksExistingAdminAccountByVerifiedEmail documents the linking policy:
// SSO authenticates the already-existing account, so its role is preserved.
// Auto-provisioning is the only creation path and it always creates a member.
func TestOIDCLinksExistingAdminAccountByVerifiedEmail(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, _, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	if _, err := db.Exec(`UPDATE auth_users SET email='boss@example.com' WHERE username='admin'`); err != nil {
		t.Fatal(err)
	}
	idp.setIdentity(fakeOIDCIdentity{email: "boss@example.com", preferredUsername: "boss", name: "Boss", emailVerified: true})
	recorder := runOIDCFlow(t, h, idp, "/")
	session := assertSSOSuccess(t, recorder, "/")
	if tokens := refreshWithCookie(t, h, session); tokens.User.Role != "admin" {
		t.Fatalf("linked admin role = %q", tokens.User.Role)
	}
}

// ---------------------------------------------------------------------------
// ID token verification
// ---------------------------------------------------------------------------

func TestOIDCIDTokenValidation(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, service, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	linkedMember(t, service, db, "token-user", "token@example.com")
	baseIdentity := fakeOIDCIdentity{email: "token@example.com", preferredUsername: "token", name: "Token", emailVerified: true}

	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string // empty means the login must succeed
	}{
		{name: "valid", mutate: nil},
		{name: "wrong audience", mutate: func(claims map[string]any) { claims["aud"] = "another-client" }, wantErr: "invalid_token"},
		{name: "wrong issuer", mutate: func(claims map[string]any) { claims["iss"] = "https://evil.example" }, wantErr: "invalid_token"},
		{name: "expired beyond leeway", mutate: func(claims map[string]any) { claims["exp"] = time.Now().Add(-120 * time.Second).Unix() }, wantErr: "invalid_token"},
		{name: "not yet valid", mutate: func(claims map[string]any) { claims["nbf"] = time.Now().Add(120 * time.Second).Unix() }, wantErr: "invalid_token"},
		{name: "issued in the future beyond leeway", mutate: func(claims map[string]any) { claims["iat"] = time.Now().Add(120 * time.Second).Unix() }, wantErr: "invalid_token"},
		{name: "expired within clock skew", mutate: func(claims map[string]any) { claims["exp"] = time.Now().Add(-30 * time.Second).Unix() }},
		{name: "issued in the future within clock skew", mutate: func(claims map[string]any) { claims["iat"] = time.Now().Add(30 * time.Second).Unix() }},
		{name: "nonce mismatch", mutate: func(claims map[string]any) { claims["nonce"] = "another-nonce" }, wantErr: "invalid_token"},
		{name: "azp mismatch", mutate: func(claims map[string]any) { claims["azp"] = "another-client" }, wantErr: "invalid_token"},
		{name: "email not verified", mutate: func(claims map[string]any) { claims["email_verified"] = false }, wantErr: "email_unverified"},
		{name: "email missing", mutate: func(claims map[string]any) { delete(claims, "email") }, wantErr: "email_unverified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp.setIdentity(baseIdentity)
			idp.setMutate(tc.mutate)
			recorder := runOIDCFlow(t, h, idp, "/")
			if tc.wantErr == "" {
				assertSSOSuccess(t, recorder, "/")
				return
			}
			assertSSOError(t, recorder, tc.wantErr)
		})
	}
}

func TestOIDCInvalidTokenForms(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, service, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	linkedMember(t, service, db, "forms-user", "forms@example.com")
	idp.setIdentity(fakeOIDCIdentity{email: "forms@example.com", preferredUsername: "forms", name: "Forms", emailVerified: true})

	t.Run("unsigned garbage", func(t *testing.T) {
		idp.setIDTokenOverride("not-a-jwt")
		recorder := runOIDCFlow(t, h, idp, "/")
		assertSSOError(t, recorder, "invalid_token")
	})
	t.Run("unknown signing key", func(t *testing.T) {
		idp.setIDTokenOverride("")
		idp.setTokenBody("")
		idp.setMutate(nil)
		idp.addUnpublishedKey(t, "kid-not-published")
		recorder := runOIDCFlow(t, h, idp, "/")
		assertSSOError(t, recorder, "invalid_token")
	})
	t.Run("token endpoint failure", func(t *testing.T) {
		idp.setTokenStatus(http.StatusInternalServerError)
		idp.setTokenBody("provider secret error detail")
		recorder := runOIDCFlow(t, h, idp, "/")
		assertSSOError(t, recorder, "token_exchange_failed")
		if strings.Contains(recorder.Body.String(), "secret") {
			t.Fatal("provider error body leaked into the response")
		}
	})
}

func TestOIDCJWKSKidRotation(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, service, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	linkedMember(t, service, db, "rotation-user", "rotation@example.com")
	idp.setIdentity(fakeOIDCIdentity{email: "rotation@example.com", preferredUsername: "rotation", name: "Rotation", emailVerified: true})

	// Warm the JWKS cache with kid-1.
	assertSSOSuccess(t, runOIDCFlow(t, h, idp, "/"), "/")
	// Rotate: the next token is signed by kid-2 and the JWKS cache must refresh
	// on the unknown kid instead of failing every login until a restart.
	idp.addSigningKey(t, "kid-2")
	assertSSOSuccess(t, runOIDCFlow(t, h, idp, "/"), "/")
}

// ---------------------------------------------------------------------------
// Rate limiting and failure audit
// ---------------------------------------------------------------------------

func TestOIDCRateLimitIsSharedAndFailClosed(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	limiter := newTestLimiter() // deterministic 10/minute test quota
	h, _, _ := oidcTestServer(t, idp, false, "企业单点登录", Options{Limiter: limiter})
	for attempt := 0; attempt < 11; attempt++ {
		recorder := oidcGet(h, "/api/v1/auth/oidc/start")
		switch {
		case attempt < 10 && recorder.Code != http.StatusFound:
			t.Fatalf("start %d status = %d", attempt, recorder.Code)
		case attempt == 10:
			if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
				t.Fatalf("rate limited start = %d %v", recorder.Code, recorder.Header())
			}
		}
	}
	// A missing limiter is a hard failure, never a local counter fallback.
	h2, _, _ := oidcTestServer(t, idp, false, "企业单点登录", Options{Limiter: errorTestLimiter{}})
	if recorder := oidcGet(h2, "/api/v1/auth/oidc/start"); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("limiter failure status = %d %s", recorder.Code, recorder.Body.String())
	}

}

func TestOIDCFailuresAreAuditedWithoutProviderText(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, _, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	idp.setIdentity(fakeOIDCIdentity{email: "audit@example.com", preferredUsername: "audit", name: "Audit", emailVerified: true})
	recorder := runOIDCFlow(t, h, idp, "/")
	assertSSOError(t, recorder, "account_not_found")
	rows, err := db.Query(`SELECT detail FROM auth_audit WHERE action=$1 AND result=$2`, auth.ActionLogin, auth.ResultFailure)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var detail string
		if err = rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		count++
		if !strings.HasPrefix(detail, "method=oidc code=") || strings.Contains(strings.ToLower(detail), "secret") || strings.Contains(detail, "@") {
			t.Fatalf("unsafe failure audit detail %q", detail)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no failure audit row")
	}
}

// TestOIDCProviderDiscoveryFailureFailsStartup pins the startup rule: a broken
// discovery document must make New fail instead of leaving a half-enabled flow.
func TestOIDCProviderDiscoveryFailureFailsStartup(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, map[string]any{"issuer": "https://other.example"})
	}))
	defer broken.Close()
	db := testutil.OpenPostgres(t)
	service := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := service.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := oidc.New(context.Background(), db, service, oidc.Options{
		Issuer: broken.URL, ClientID: testOIDCClientID,
		RedirectURL: "http://localhost:5173/api/v1/auth/oidc/callback",
		Scopes:      []string{"openid"}, DiscoveryTimeout: 2 * time.Second,
	}); err == nil {
		t.Fatal("broken discovery accepted at startup")
	}
}

// TestOIDCCallbackAcceptsCrossSiteTopLevelNavigation covers the navigation
// exception every real OIDC provider relies on: the browser comes back from the
// IdP with a cross-site GET navigation, which the global CSRF middleware must
// allow for exactly the callback path (CSRF defence there is the one-time state
// cookie + state row). All other cross-site requests stay denied.
func TestOIDCCallbackAcceptsCrossSiteTopLevelNavigation(t *testing.T) {
	idp := newFakeOIDCProvider(t)
	h, service, db := oidcTestServer(t, idp, false, "企业单点登录", Options{})
	linkedMember(t, service, db, "cross-site-user", "cross-site@example.com")
	idp.setIdentity(fakeOIDCIdentity{email: "cross-site@example.com", preferredUsername: "cross", name: "Cross", emailVerified: true})

	flow := startOIDCFlow(t, h, "/")
	code := idp.authorize(t, flow)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(flow.state), nil)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.AddCookie(flow.stateCookie)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	assertSSOSuccess(t, recorder, "/")

	// The exception is scoped to the GET callback: other cross-site requests
	// (including the OIDC probe) are still rejected before any handler runs.
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/auth/me"},
		{http.MethodGet, "/api/v1/auth/oidc/config"},
		{http.MethodGet, "/api/v1/auth/oidc/start"},
		{http.MethodPost, "/api/v1/auth/login"},
	} {
		request := httptest.NewRequest(probe.method, probe.path, nil)
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		request.Header.Set("Origin", "http://localhost:5173")
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "origin_denied") {
			t.Fatalf("cross-site %s %s = %d %s, want 403 origin_denied", probe.method, probe.path, recorder.Code, recorder.Body.String())
		}
	}
}
