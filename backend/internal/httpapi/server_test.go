package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

func testServer(t *testing.T) (http.Handler, *auth.Service) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	s := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(context.Background(), "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	return New(s, slog.New(slog.NewJSONHandler(io.Discard, nil)), db.PingContext, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter(), Projects: projects.New(db)}), s
}
func request(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://localhost:5173")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAuthenticationAndRBAC(t *testing.T) {
	h, s := testServer(t)
	if w := request(h, "GET", "/api/v1/auth/me", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := request(h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"a-strong-password"}`, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	admin := result.Data.AccessToken
	loginCookie := w.Result().Cookies()[0]
	if admin == "" {
		t.Fatal("missing token")
	}
	if w = request(h, "POST", "/api/v1/users", `{"username":"reader","password":"reader-password","displayName":"Reader","role":"viewer"}`, admin); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	reader, err := s.Login(context.Background(), "reader", "reader-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		method, path, body string
		want               int
	}{{"GET", "/api/v1/auth/me", "", 200}, {"GET", "/api/v1/users", "", 403}, {"POST", "/api/v1/users", `{}`, 403}} {
		w = request(h, route.method, route.path, route.body, reader.AccessToken)
		if w.Code != route.want {
			t.Fatalf("%s: %d %s", route.path, w.Code, w.Body.String())
		}
	}
	if w = request(h, "GET", "/api/v1/users", "", admin); w.Code != 200 || strings.Contains(w.Body.String(), "password") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = cookieRequest(h, "POST", "/api/v1/auth/logout", `{}`, loginCookie); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/auth/me", "", admin); w.Code != 401 {
		t.Fatal("logout did not revoke access", w.Code)
	}
}
func TestHTTPGuards(t *testing.T) {
	h, _ := testServer(t)
	for _, body := range []string{`{"username":"admin","password":"a-strong-password","extra":true}`, `{} {}`, `{"password":"` + strings.Repeat("a", 17000) + `"}`} {
		if w := request(h, "POST", "/api/v1/auth/login", body, ""); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{}`))
	r.Header.Set("Origin", "http://localhost:5173")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal(w.Code)
	}
	for _, origin := range []string{"http://localhost:5173", "https://evil.example"} {
		r = httptest.NewRequest("OPTIONS", "/api/v1/auth/login", nil)
		r.Header.Set("Origin", origin)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 204
		if strings.Contains(origin, "evil") {
			want = 403
		}
		if w.Code != want {
			t.Fatal(w.Code)
		}
		if w.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing request id")
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if w = request(h, "GET", path, "", ""); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
}
func TestLoginRateLimit(t *testing.T) {
	h, _ := testServer(t)
	for i := 0; i < 11; i++ {
		w := request(h, "POST", "/api/v1/auth/login", `{}`, "")
		if i == 10 && w.Code != 429 {
			t.Fatal(w.Code)
		}
	}
}
func TestGinFallbackAndRecovery(t *testing.T) {
	h, _ := testServer(t)
	router := h.(*gin.Engine)
	router.GET("/test-panic", func(c *gin.Context) { panic("sensitive panic detail") })
	for _, tc := range []struct {
		method, path, code string
		status             int
	}{
		{"GET", "/missing", "not_found", 404},
		{"GET", "/api/v1/auth/login", "not_found", 404},
		{"GET", "/healthz/", "not_found", 404},
		{"GET", "/test-panic", "internal_error", 500},
	} {
		w := request(h, tc.method, tc.path, "", "")
		var result struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err, w.Body.String())
		}
		if w.Code != tc.status || result.Error.Code != tc.code || result.RequestID == "" || result.RequestID != w.Header().Get("X-Request-ID") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "sensitive") {
			t.Fatal("panic details leaked")
		}
	}
}

func TestForwardedHeadersCannotBypassLoginLimit(t *testing.T) {
	h, _ := testServer(t)
	for i := 0; i < 11; i++ {
		r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:5173")
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
		r.Header.Set("X-Real-IP", fmt.Sprintf("203.0.113.%d", i+1))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i == 10 && w.Code != 429 {
			t.Fatalf("forwarded headers bypassed limit: %d", w.Code)
		}
	}
}
