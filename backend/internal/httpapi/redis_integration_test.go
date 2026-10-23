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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/ratelimit"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// Two network servers, independent Redis clients and auth services, one database
// and quota. A browser cookie jar crosses instances for refresh and revocation.
func TestRedisTwoHTTPInstancesShareQuotaAndSessions(t *testing.T) {
	raw := os.Getenv("AEGIS_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("AEGIS_TEST_REDIS_URL is not set")
	}
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	db := testutil.OpenPostgres(t)
	services := []*auth.Service{auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour), auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)}
	if err := services[0].Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := services[0].Bootstrap(context.Background(), "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	namespace := fmt.Sprintf("aegis:test:http:%d", time.Now().UnixNano())
	var servers []*httptest.Server
	for _, service := range services {
		client := redis.NewClient(opts)
		t.Cleanup(func() { client.Close() })
		if err := client.Ping(context.Background()).Err(); err != nil {
			t.Fatal(err)
		}
		h := New(service, slog.New(slog.NewTextHandler(io.Discard, nil)), db.PingContext, []string{"http://localhost:5173"}, Options{Limiter: ratelimit.New(client, namespace)})
		server := httptest.NewServer(h)
		t.Cleanup(server.Close)
		servers = append(servers, server)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	call := func(index int, method, path, body, access string, want int) []byte {
		t.Helper()
		r, err := http.NewRequest(method, servers[index].URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", "http://localhost:5173")
		r.Header.Set("Content-Type", "application/json")
		if access != "" {
			r.Header.Set("Authorization", "Bearer "+access)
		}
		response, err := browser.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("instance %d %s: %d %s", index, path, response.StatusCode, data)
		}
		if strings.Contains(string(data), "refreshToken") {
			t.Fatal("refresh token leaked in JSON")
		}
		return data
	}
	token := func(data []byte) string {
		t.Helper()
		var result struct {
			Data struct {
				AccessToken string `json:"accessToken"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.Data.AccessToken == "" {
			t.Fatal("missing access token")
		}
		return result.Data.AccessToken
	}
	initial := token(call(0, "POST", "/api/v1/auth/login", `{"username":"admin","password":"a-strong-password","remember":true}`, "", 200))
	call(1, "GET", "/api/v1/auth/me", "", initial, 200)
	rotated := token(call(1, "POST", "/api/v1/auth/refresh", `{}`, "", 200))
	call(0, "GET", "/api/v1/auth/me", "", rotated, 200)
	// First login consumed one attempt; malformed requests consume the other nine.
	for i := 0; i < 9; i++ {
		call(i%2, "POST", "/api/v1/auth/login", `{`, "", 400)
	}
	for i := 0; i < 2; i++ {
		call(i, "POST", "/api/v1/auth/login", `{`, "", 429)
	}
	// Login quota never prevents revocation on another instance.
	call(1, "POST", "/api/v1/auth/logout", `{}`, "", 200)
	call(0, "GET", "/api/v1/auth/me", "", rotated, 401)
	call(0, "GET", "/api/v1/auth/me", "", initial, 401)
	call(0, "POST", "/api/v1/auth/refresh", `{}`, "", 401)
}
