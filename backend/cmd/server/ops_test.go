package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealProcessLifecycle is the P1 operational acceptance: it builds the
// real server binary, starts it against real PostgreSQL and Redis, verifies
// readiness, logs in, terminates it with the OS termination signal, requires a
// clean exit, restarts it, and proves the session and project data survive.
// It skips without AEGIS_OPS_DATABASE_URL/AEGIS_OPS_REDIS_URL; a skip is not
// an acceptance pass.
func TestRealProcessLifecycle(t *testing.T) {
	databaseURL := os.Getenv("AEGIS_OPS_DATABASE_URL")
	redisURL := os.Getenv("AEGIS_OPS_REDIS_URL")
	if databaseURL == "" || redisURL == "" {
		t.Skip("real process acceptance skipped: set AEGIS_OPS_DATABASE_URL and AEGIS_OPS_REDIS_URL")
	}
	const (
		origin   = "http://ops.aegisflow.test"
		username = "ops-admin"
		password = "Ops-admin-password-2026"
	)
	// One secret for both processes: persistence across restarts requires it.
	const jwtSecret = "ops-acceptance-secret-at-least-32-bytes"
	bin := buildServer(t)

	first := startServer(t, bin, databaseURL, redisURL, jwtSecret, origin)
	waitReady(t, first)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	var login struct {
		Data struct {
			AccessToken string `json:"accessToken"`
			User        struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"data"`
	}
	loginBody := fmt.Sprintf(`{"username":%q,"password":%q,"remember":true}`, username, password)
	postJSON(t, client, origin, first.baseURL+"/api/v1/auth/login", loginBody, "", &login)
	if login.Data.AccessToken == "" || login.Data.User.Username != username {
		t.Fatalf("login payload: %+v", login.Data)
	}
	me := getJSON(t, client, origin, first.baseURL+"/api/v1/auth/me", login.Data.AccessToken)
	if !strings.Contains(me, username) {
		t.Fatalf("me after login: %s", me)
	}
	// Seed a project before shutdown.
	projectName := fmt.Sprintf("ops-project-%d", time.Now().UnixNano())
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	projectBody := fmt.Sprintf(`{"name":%q,"repoUrl":"https://github.com/example/ops.git","defaultBranch":"main"}`, projectName)
	postJSON(t, client, origin, first.baseURL+"/api/v1/projects", projectBody, login.Data.AccessToken, &created)
	if created.Data.ID == "" {
		t.Fatal("project create returned no id")
	}

	stopGracefully(t, first)

	second := startServer(t, bin, databaseURL, redisURL, jwtSecret, origin)
	waitReady(t, second)
	// The access token, refresh cookie and project data all survive restart.
	me = getJSON(t, client, origin, second.baseURL+"/api/v1/auth/me", login.Data.AccessToken)
	if !strings.Contains(me, username) {
		t.Fatalf("access token lost after restart: %s", me)
	}
	var refreshed struct {
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	postJSON(t, client, origin, second.baseURL+"/api/v1/auth/refresh", "{}", "", &refreshed)
	if refreshed.Data.AccessToken == "" {
		t.Fatal("refresh cookie lost after restart")
	}
	// Read the project itself instead of scanning a page of the shared list: an
	// ops database may be reused across runs (backend/README documents that),
	// and after enough runs the new project sorts past the first page. The list
	// endpoint's pagination has its own coverage; this test owns process
	// lifecycle, so it asserts the row survived the restart, not page position.
	detail := getJSON(t, client, origin, second.baseURL+"/api/v1/projects/"+created.Data.ID, login.Data.AccessToken)
	if !strings.Contains(detail, projectName) {
		t.Fatalf("project lost after restart: %s", detail)
	}
	stopGracefully(t, second)
}

func buildServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aegis-server-ops")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build server binary: %v\n%s", err, out)
	}
	return bin
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type serverProcess struct {
	cmd     *exec.Cmd
	logs    *syncBuffer
	baseURL string
}

func startServer(t *testing.T, bin, databaseURL, redisURL, jwtSecret, origin string) *serverProcess {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"AEGIS_HTTP_ADDR=127.0.0.1:0",
		"AEGIS_DATABASE_URL="+databaseURL,
		"AEGIS_REDIS_URL="+redisURL,
		// A fresh namespace per run keeps repeated local acceptances from
		// sharing the login rate-limit window.
		"AEGIS_REDIS_NAMESPACE=ops-"+hexSuffix(),
		"AEGIS_JWT_SECRET="+jwtSecret,
		"AEGIS_BOOTSTRAP_USERNAME=ops-admin",
		"AEGIS_BOOTSTRAP_PASSWORD=Ops-admin-password-2026",
		"AEGIS_COOKIE_SECURE=false",
		"AEGIS_ALLOWED_ORIGINS="+origin,
		"GIN_MODE=release",
	)
	cmd.SysProcAttr = procAttr()
	logs := &syncBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	p := &serverProcess{cmd: cmd, logs: logs}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	// The listener binds port 0; the startup log reports the real address.
	deadline := time.Now().Add(15 * time.Second)
	for {
		line := ""
		for _, candidate := range strings.Split(logs.String(), "\n") {
			if strings.Contains(candidate, "server listening") {
				line = candidate
			}
		}
		if line != "" {
			var record struct {
				Address string `json:"address"`
			}
			if err := json.Unmarshal([]byte(line), &record); err == nil && record.Address != "" && !strings.HasSuffix(record.Address, ":0") {
				p.baseURL = "http://" + record.Address
				return p
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not log its address; output:\n%s", logs.String())
		}
		if cmd.ProcessState != nil {
			t.Fatalf("server exited during startup (%v); output:\n%s", cmd.ProcessState, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitReady(t *testing.T, p *serverProcess) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(p.baseURL + "/readyz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became ready; output:\n%s", p.logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func stopGracefully(t *testing.T, p *serverProcess) {
	t.Helper()
	start := time.Now()
	if err := terminate(p.cmd); err != nil {
		t.Fatalf("send termination signal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server did not exit cleanly: %v; output:\n%s", err, tail(p.logs.String()))
		}
		if elapsed := time.Since(start); elapsed > 15*time.Second {
			t.Fatalf("shutdown took %s", elapsed)
		}
	case <-time.After(30 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatalf("graceful shutdown timed out; output:\n%s", tail(p.logs.String()))
	}
}

func tail(log string) string {
	if len(log) > 4000 {
		return log[len(log)-4000:]
	}
	return log
}

func hexSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fixed"
	}
	return hex.EncodeToString(b[:])
}

func postJSON(t *testing.T, client *http.Client, origin, url, body, token string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: %d %s", url, resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("POST %s payload: %v (%s)", url, err, data)
	}
}

func getJSON(t *testing.T, client *http.Client, origin, url, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, data)
	}
	return string(data)
}
