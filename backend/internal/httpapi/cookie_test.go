package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

func cookieRequest(h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://localhost:5173")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func responseTokens(t *testing.T, w *httptest.ResponseRecorder) (string, *http.Cookie) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != 3 || result.Data["user"] == nil || result.Data["accessToken"] == nil || result.Data["expiresIn"] == nil {
		t.Fatalf("unsafe token shape: %s", w.Body.String())
	}
	var access string
	if err := json.Unmarshal(result.Data["accessToken"], &access); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != refreshCookieName {
		t.Fatalf("cookies: %+v", cookies)
	}
	if strings.Contains(w.Body.String(), cookies[0].Value) {
		t.Fatal("refresh leaked in JSON")
	}
	return access, cookies[0]
}

func TestCookieFlagsRememberRotationAndLogout(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, mode := range []string{"omitted", "false", "true"} {
			t.Run(fmt.Sprintf("secure=%v/remember=%s", secure, mode), func(t *testing.T) {
				_, service := testServer(t)
				h := New(service, slog.New(slog.NewJSONHandler(io.Discard, nil)), func(context.Context) error { return nil }, []string{"http://localhost:5173"}, Options{CookieSecure: secure, Limiter: newTestLimiter()})
				body := `{"username":"admin","password":"a-strong-password"`
				if mode != "omitted" {
					body += `,"remember":` + mode
				}
				body += `}`
				w := cookieRequest(h, "POST", "/api/v1/auth/login", body, nil)
				access, first := responseTokens(t, w)
				if !first.HttpOnly || first.Secure != secure || first.SameSite != http.SameSiteStrictMode || first.Path != refreshCookiePath || first.Domain != "" {
					t.Fatalf("unsafe flags: %+v", first)
				}
				if mode == "true" {
					if first.MaxAge <= 0 || first.Expires.IsZero() {
						t.Fatalf("missing persistence: %+v", first)
					}
				} else if first.MaxAge != 0 || !first.Expires.IsZero() {
					t.Fatalf("session cookie persisted: %+v", first)
				}
				w = cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, first)
				nextAccess, next := responseTokens(t, w)
				if next.Value == first.Value || next.Secure != secure || !next.HttpOnly || next.SameSite != first.SameSite || next.Path != first.Path || next.MaxAge > first.MaxAge || !next.Expires.Equal(first.Expires) {
					t.Fatalf("rotation changed deadline/policy: first=%+v next=%+v", first, next)
				}
				w = cookieRequest(h, "POST", "/api/v1/auth/logout", `{}`, next)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				deleted := w.Result().Cookies()[0]
				if deleted.MaxAge != -1 || deleted.Value != "" || deleted.Path != first.Path || !deleted.HttpOnly || deleted.Secure != secure || deleted.SameSite != http.SameSiteStrictMode {
					t.Fatalf("bad deletion: %+v", deleted)
				}
				for _, token := range []string{access, nextAccess} {
					if w := request(h, "GET", "/api/v1/auth/me", "", token); w.Code != 401 {
						t.Fatal("logout left access valid", w.Code)
					}
				}
				for _, cookie := range []*http.Cookie{next, nil, {Name: refreshCookieName, Value: "unknown"}} {
					if w := cookieRequest(h, "POST", "/api/v1/auth/logout", `{}`, cookie); w.Code != 200 || w.Result().Cookies()[0].MaxAge != -1 {
						t.Fatal("logout not idempotent", w.Code)
					}
				}
				if w := cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, next); w.Code != 401 {
					t.Fatal("revoked refresh accepted", w.Code)
				}
			})
		}
	}
}

func TestAuthCSRFAndCookieOnlyTransport(t *testing.T) {
	h, _ := testServer(t)
	for _, path := range []string{"login", "refresh", "logout"} {
		for _, origin := range []string{"", "null", "https://evil.example", "http://localhost:5173/", "http://localhost:5173.evil.example"} {
			r := httptest.NewRequest("POST", "/api/v1/auth/"+path, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 || len(w.Result().Cookies()) != 0 {
				t.Fatalf("%s Origin=%q: %d", path, origin, w.Code)
			}
		}
		r := httptest.NewRequest("POST", "/api/v1/auth/"+path, strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:5173")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("cross-site accepted", path, w.Code)
		}
	}
	w := cookieRequest(h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"a-strong-password"}`, nil)
	_, cookie := responseTokens(t, w)
	for _, path := range []string{"refresh", "logout"} {
		for _, body := range []string{`{"refreshToken":"` + cookie.Value + `"}`, `null`, `[]`, ``, `{} {}`} {
			if w := cookieRequest(h, "POST", "/api/v1/auth/"+path, body, cookie); w.Code != 400 {
				t.Fatalf("%s accepted %s: %d", path, body, w.Code)
			}
		}
	}
	w = cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, nil)
	if w.Code != 401 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("missing cookie not rejected", w.Code)
	}
	if w := cookieRequest(h, "GET", "/api/v1/auth/me", ``, cookie); w.Code != 401 {
		t.Fatal("cookie authorized Bearer route", w.Code)
	}
	if w := cookieRequest(h, "GET", "/api/v1/users", ``, cookie); w.Code != 401 {
		t.Fatal("cookie authorized users route", w.Code)
	}
	// Replay revokes the entire session and clears the invalid cookie.
	rotated := cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, cookie)
	access, next := responseTokens(t, rotated)
	if w := cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, cookie); w.Code != 401 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("replay accepted")
	}
	if w := request(h, "GET", "/api/v1/auth/me", ``, access); w.Code != 401 {
		t.Fatal("replay did not revoke access")
	}
	if w := cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, next); w.Code != 401 {
		t.Fatal("replay did not revoke refresh")
	}
}

func TestAuthRejectedRequestsDoNotMutateSession(t *testing.T) {
	h, _ := testServer(t)
	access, cookie := responseTokens(t, cookieRequest(h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"a-strong-password"}`, nil))
	for _, path := range []string{"refresh", "logout"} {
		r := httptest.NewRequest("POST", "/api/v1/auth/"+path, strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 || len(w.Result().Cookies()) != 0 {
			t.Fatal("CSRF rejection changed cookie", w.Code)
		}
	}
	if w := request(h, "GET", "/api/v1/auth/me", ``, access); w.Code != 200 {
		t.Fatal("CSRF rejection revoked session", w.Code)
	}
	responseTokens(t, cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, cookie))
	for _, value := range []string{"bad", strings.Repeat("a", 43)} {
		w := cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, &http.Cookie{Name: refreshCookieName, Value: value})
		if w.Code != 401 || w.Result().Cookies()[0].MaxAge != -1 {
			t.Fatal("invalid cookie accepted", w.Code)
		}
	}
	for _, body := range []string{`{"username":"admin","password":"a-strong-password","remember":null}`, `{"username":"admin","password":"a-strong-password","remember":"false"}`} {
		if w := cookieRequest(h, "POST", "/api/v1/auth/login", body, nil); w.Code != 400 {
			t.Fatal("invalid remember accepted", w.Code)
		}
	}
}

func TestSecureNotInferredFromForwardingHeaders(t *testing.T) {
	h, _ := testServer(t)
	r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"a-strong-password"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://localhost:5173")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Forwarded", `proto=https;host=example.com`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	_, cookie := responseTokens(t, w)
	if cookie.Secure {
		t.Fatal("untrusted headers changed Secure configuration")
	}
}

func TestMemberAndViewerCannotManageUsers(t *testing.T) {
	h, service := testServer(t)
	for _, role := range []string{"member", "viewer"} {
		if _, err := service.CreateUser(context.Background(), "user-"+role, "a-strong-password", "", role); err != nil {
			t.Fatal(err)
		}
		tokens, err := service.Login(context.Background(), "user-"+role, "a-strong-password")
		if err != nil {
			t.Fatal(err)
		}
		if w := request(h, "GET", "/api/v1/auth/me", ``, tokens.AccessToken); w.Code != 200 {
			t.Fatal(role, w.Code)
		}
		for _, method := range []string{"GET", "POST"} {
			if w := request(h, method, "/api/v1/users", `{}`, tokens.AccessToken); w.Code != 403 {
				t.Fatal(role, method, w.Code)
			}
		}
	}
}

func TestLogoutStorageFailurePreservesRetryCookie(t *testing.T) {
	db := testutil.OpenPostgres(t)
	service := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := service.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Bootstrap(context.Background(), "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	h := New(service, slog.New(slog.NewJSONHandler(io.Discard, nil)), db.PingContext, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter()})
	_, cookie := responseTokens(t, cookieRequest(h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"a-strong-password"}`, nil))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	w := cookieRequest(h, "POST", "/api/v1/auth/logout", `{}`, cookie)
	if w.Code != 500 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("storage failure deleted retry credential: %d %s %+v", w.Code, w.Body.String(), w.Result().Cookies())
	}
	// There is still no storage dependency when no session is supplied.
	w = cookieRequest(h, "POST", "/api/v1/auth/logout", `{}`, nil)
	if w.Code != 200 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("missing-cookie logout failed", w.Code)
	}
}

func TestCredentialedCORS(t *testing.T) {
	h, _ := testServer(t)
	for _, method := range []string{"GET", "OPTIONS"} {
		r := httptest.NewRequest(method, "/api/v1/auth/me", nil)
		r.Header.Set("Origin", "http://localhost:5173")
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "Content-Type")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" || w.Header().Get("Access-Control-Allow-Credentials") != "true" {
			t.Fatal("missing credentialed CORS", w.Header())
		}
		vary := strings.Join(w.Header().Values("Vary"), ",")
		if !strings.Contains(vary, "Origin") || !strings.Contains(vary, "Sec-Fetch-Site") || (method == "OPTIONS" && (!strings.Contains(vary, "Access-Control-Request-Method") || !strings.Contains(vary, "Access-Control-Request-Headers"))) {
			t.Fatal("missing Vary", vary)
		}
	}
}

// Exercise a real net/http server and browser-style cookie jar, not just handlers.
func TestBrowserCookieJarFlow(t *testing.T) {
	h, _ := testServer(t)
	server := httptest.NewServer(h)
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	post := func(path, body string) (*http.Response, []byte) {
		t.Helper()
		r, err := http.NewRequest("POST", server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", "http://localhost:5173")
		r.Header.Set("Content-Type", "application/json")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, data
	}
	for _, step := range []struct {
		path, body string
		status     int
	}{
		{"/api/v1/auth/login", `{"username":"admin","password":"a-strong-password","remember":true}`, 200},
		{"/api/v1/auth/refresh", `{}`, 200}, {"/api/v1/auth/logout", `{}`, 200}, {"/api/v1/auth/logout", `{}`, 200}, {"/api/v1/auth/refresh", `{}`, 401},
	} {
		response, data := post(step.path, step.body)
		if response.StatusCode != step.status {
			t.Fatalf("%s: %d %s", step.path, response.StatusCode, data)
		}
		if strings.Contains(string(data), "refreshToken") {
			t.Fatal("JSON refresh credential leaked")
		}
	}
}
