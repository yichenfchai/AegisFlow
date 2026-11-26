package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

func TestSafeRedirectTo(t *testing.T) {
	accepted := []string{"/", "/builds", "/builds/abc?tab=logs#latest", "/%E4%B8%AD%E6%96%87"}
	for _, value := range accepted {
		if got := SafeRedirectTo(value); got != value {
			t.Fatalf("SafeRedirectTo(%q) = %q, want unchanged", value, got)
		}
	}
	rejected := []string{
		"", "https://evil.example", "//evil.example", "/\\evil.example", `/\evil.example`,
		"https:/evil.example", "javascript:alert(1)", "/builds\n", "/%2F%2Fevil.example",
		"/%5Cevil.example", "/" + strings.Repeat("a", 2050),
	}
	for _, value := range rejected {
		if got := SafeRedirectTo(value); got != "/" {
			t.Fatalf("SafeRedirectTo(%q) = %q, want /", value, got)
		}
	}
}

func TestPKCEAndStateHash(t *testing.T) {
	verifier, err := RandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier) != 43 {
		t.Fatalf("PKCE verifier length = %d, want 43", len(verifier))
	}
	challenge := PKCEChallenge(verifier)
	if len(challenge) != 43 || challenge != PKCEChallenge(verifier) {
		t.Fatalf("unstable or malformed PKCE challenge %q", challenge)
	}
	state, err := RandomToken(32)
	if err != nil {
		t.Fatal(err)
	}
	hash := StateHash(state)
	if len(hash) != 64 || hash != StateHash(state) || strings.Contains(hash, state) {
		t.Fatalf("state hash %q is not a stable SHA-256 hex digest", hash)
	}
	if StateHash(state+"x") == hash {
		t.Fatal("state hash does not depend on the state")
	}
}

func TestStateStoreIsOneTimeUse(t *testing.T) {
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	store := &StateStore{db: db}
	ctx := context.Background()
	now := time.Now().Unix()
	state := StateHash("state-value")
	if err := store.Create(ctx, state, "nonce", "verifier", "/builds", now+600, now); err != nil {
		t.Fatal(err)
	}
	consumed, err := store.Consume(ctx, state, now)
	if err != nil {
		t.Fatal(err)
	}
	if consumed.Nonce != "nonce" || consumed.CodeVerifier != "verifier" || consumed.RedirectTo != "/builds" {
		t.Fatalf("consumed state = %+v", consumed)
	}
	// Replay: the row is already consumed and can never yield a second login.
	if _, err = store.Consume(ctx, state, now); err != ErrStateNotFound {
		t.Fatalf("replayed state error = %v", err)
	}
	// An expired row is rejected exactly like a consumed one.
	expired := StateHash("expired")
	if err = store.Create(ctx, expired, "nonce", "verifier", "/", now-1, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Consume(ctx, expired, now); err != ErrStateNotFound {
		t.Fatalf("expired state error = %v", err)
	}
	// Create opportunistically deletes expired rows (including the one above).
	var remaining int
	if err = db.QueryRow(`SELECT COUNT(*) FROM oidc_login_states WHERE expires_at < $1`, now).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expired state rows remain: %d", remaining)
	}
}

func discoveryServer(t *testing.T, document func(issuer string) (int, string)) (*httptest.Server, Options) {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != discoveryPath {
			http.NotFound(w, r)
			return
		}
		status, body := document(server.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, Options{
		Issuer:           server.URL,
		ClientID:         "aegisflow",
		RedirectURL:      "http://127.0.0.1:8080/api/v1/auth/oidc/callback",
		Scopes:           []string{"openid", "profile", "email"},
		DiscoveryTimeout: 2 * time.Second,
	}
}

func validDiscoveryDocument(issuer string) map[string]string {
	return map[string]string{
		"issuer":                 issuer,
		"authorization_endpoint": issuer + "/authorize",
		"token_endpoint":         issuer + "/token",
		"jwks_uri":               issuer + "/jwks",
	}
}

func encodeDocument(t *testing.T, document map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDiscoveryValidation(t *testing.T) {
	t.Run("valid loopback provider", func(t *testing.T) {
		_, options := discoveryServer(t, func(issuer string) (int, string) {
			return http.StatusOK, encodeDocument(t, validDiscoveryDocument(issuer))
		})
		provider, err := newProvider(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		authorizationURL := provider.AuthorizationURL("state-value", "nonce-value", "challenge-value")
		for _, fragment := range []string{"response_type=code", "client_id=aegisflow", "state=state-value", "nonce=nonce-value", "code_challenge=challenge-value", "code_challenge_method=S256", "scope=openid+profile+email"} {
			if !strings.Contains(authorizationURL, fragment) {
				t.Fatalf("authorization URL %q misses %q", authorizationURL, fragment)
			}
		}
	})
	t.Run("issuer mismatch", func(t *testing.T) {
		_, options := discoveryServer(t, func(string) (int, string) {
			return http.StatusOK, encodeDocument(t, validDiscoveryDocument("https://other.example"))
		})
		if _, err := newProvider(context.Background(), options); err == nil || !strings.Contains(err.Error(), "issuer") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing endpoint", func(t *testing.T) {
		_, options := discoveryServer(t, func(issuer string) (int, string) {
			document := validDiscoveryDocument(issuer)
			delete(document, "jwks_uri")
			return http.StatusOK, encodeDocument(t, document)
		})
		if _, err := newProvider(context.Background(), options); err == nil || !strings.Contains(err.Error(), "jwks_uri") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("plain http endpoint outside loopback", func(t *testing.T) {
		_, options := discoveryServer(t, func(issuer string) (int, string) {
			document := validDiscoveryDocument(issuer)
			document["token_endpoint"] = "http://idp.example/token"
			return http.StatusOK, encodeDocument(t, document)
		})
		if _, err := newProvider(context.Background(), options); err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("endpoint with userinfo", func(t *testing.T) {
		_, options := discoveryServer(t, func(issuer string) (int, string) {
			document := validDiscoveryDocument(issuer)
			document["authorization_endpoint"] = issuer + "/authorize?user=a"
			return http.StatusOK, encodeDocument(t, document)
		})
		if _, err := newProvider(context.Background(), options); err == nil {
			t.Fatal("query-bearing authorization endpoint accepted")
		}
	})
	t.Run("oversized discovery document", func(t *testing.T) {
		_, options := discoveryServer(t, func(issuer string) (int, string) {
			return http.StatusOK, `{"padding":"` + strings.Repeat("a", maxProviderResponse) + `"}`
		})
		if _, err := newProvider(context.Background(), options); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("provider error status", func(t *testing.T) {
		_, options := discoveryServer(t, func(string) (int, string) {
			return http.StatusInternalServerError, `{}`
		})
		if _, err := newProvider(context.Background(), options); err == nil {
			t.Fatal("500 discovery accepted")
		}
	})
	t.Run("missing issuer", func(t *testing.T) {
		if _, err := newProvider(context.Background(), Options{ClientID: "x", RedirectURL: "https://example.com/cb", DiscoveryTimeout: time.Second}); err == nil {
			t.Fatal("provider without issuer accepted")
		}
	})
}

// TestValidateProviderURLHTTPRule pins the endpoint safety rule shared by all
// discovery URLs: https only, except loopback names/addresses for local tests.
func TestValidateProviderURLHTTPRule(t *testing.T) {
	for _, raw := range []string{
		"https://idp.example/token",
		"http://127.0.0.1:9000/token",
		"http://localhost:9000/token",
		"http://[::1]:9000/token",
	} {
		if _, err := validateProviderURL("token_endpoint", raw); err != nil {
			t.Fatalf("validateProviderURL(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://idp.example/token",
		"https://user:pass@idp.example/token",
		"https://idp.example/token#fragment",
		"https://idp.example/token?query=1",
		"ftp://idp.example/token",
		"",
	} {
		if _, err := validateProviderURL("token_endpoint", raw); err == nil {
			t.Fatalf("validateProviderURL(%q) accepted", raw)
		}
	}
}
