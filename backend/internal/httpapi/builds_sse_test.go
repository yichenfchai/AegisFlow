package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// slowEchoCommand prints after a delay so the test proves the stream survives
// the server-wide write timeout and delivers output written later.
func slowEchoCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 3 127.0.0.1 >nul & echo late-sse-ok"
	}
	return "sleep 1; echo late-sse-ok"
}

func TestBuildEventsStream(t *testing.T) {
	h, _ := testServer(t)
	server := httptest.NewUnstartedServer(h)
	// A long-lived stream must outlive the server-wide write deadline.
	server.Config.WriteTimeout = 200 * time.Millisecond
	server.Start()
	defer server.Close()

	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createBuildableProject(t, h, adminToken, "sse-build", slowEchoCommand())
	buildID := triggerBuild(t, h, adminToken, projectID, `{}`)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/builds/"+buildID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("content type = %q", contentType)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var event string
	var logs strings.Builder
	var sawSucceeded, sawDone bool
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if event == "logs" {
				logs.WriteString(data)
			}
			if event == "status" && strings.Contains(data, `"status":"succeeded"`) {
				sawSucceeded = true
			}
		case line == "" && event == "done":
			sawDone = true
		}
		if sawDone {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !sawSucceeded {
		t.Fatal("stream never reported the succeeded status")
	}
	if !sawDone {
		t.Fatal("stream did not end with a done event")
	}
	if !strings.Contains(logs.String(), "late-sse-ok") {
		t.Fatalf("streamed logs lack the process output: %s", logs.String())
	}
}

func TestBuildEventsRejectsInvalidRequests(t *testing.T) {
	h, _ := testServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createBuildableProject(t, h, adminToken, "sse-errors", "echo x")
	buildID := triggerBuild(t, h, adminToken, projectID, `{}`)

	if w := request(h, "GET", "/api/v1/builds/"+buildID+"/events?after=-1", "", adminToken); w.Code != 400 {
		t.Fatalf("negative cursor: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/api/v1/builds/"+buildID+"/events?after=abc", "", adminToken); w.Code != 400 {
		t.Fatalf("non-numeric cursor: %d", w.Code)
	}
	if w := request(h, "GET", "/api/v1/builds/missing-0000000000000001/events", "", adminToken); w.Code != 404 {
		t.Fatalf("unknown build: %d", w.Code)
	}
	if w := request(h, "GET", "/api/v1/builds/"+buildID+"/events", "", ""); w.Code != 401 {
		t.Fatalf("unauthenticated stream: %d", w.Code)
	}
}

// slowShutdownCommand keeps a build running so the stream is still live when
// the test cancels the shutdown context.
func slowShutdownCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 10 127.0.0.1 >nul"
	}
	return "sleep 9"
}

// TestBuildEventsStreamStopsOnShutdown is the P1-09 regression: http.Server
// Shutdown does not cancel in-flight request contexts, so a live SSE stream
// would hold the 10s drain and turn a graceful stop into a non-zero exit. The
// stream must watch the server shutdown context, write a done frame and return.
func TestBuildEventsStreamStopsOnShutdown(t *testing.T) {
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	h, _ := testServerWithOptions(t, Options{ShutdownContext: shutdownCtx})
	server := httptest.NewUnstartedServer(h)
	// The normal server-wide write deadline must not kill the stream early.
	server.Config.WriteTimeout = 200 * time.Millisecond
	server.Start()
	defer server.Close()

	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createBuildableProject(t, h, adminToken, "sse-shutdown", slowShutdownCommand())
	buildID := triggerBuild(t, h, adminToken, projectID, `{}`)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/builds/"+buildID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	// Consume the opening status frame so the handler is inside its select loop.
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if line == "" && event == "status" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	shutdown()
	type streamResult struct {
		err      error
		doneData string
	}
	finished := make(chan streamResult, 1)
	go func() {
		var result streamResult
		var current string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				current = strings.TrimPrefix(line, "event: ")
			}
			if line == "" && current == "done" {
				break
			}
			if strings.HasPrefix(line, "data: ") && current == "done" {
				result.doneData = strings.TrimPrefix(line, "data: ")
			}
		}
		result.err = scanner.Err()
		finished <- result
	}()
	select {
	case result := <-finished:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if !strings.Contains(result.doneData, `"reason":"shutdown"`) {
			t.Fatalf("done frame data = %q, want a shutdown reason", result.doneData)
		}
	case <-time.After(time.Second):
		t.Fatal("stream handler did not return within 1s of the shutdown signal")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("stream ended after %v, want under 1s", elapsed)
	}
}

// TestBuildEventsStreamConnectionLimit is the P2-14 regression: every live
// stream holds a goroutine that queries PostgreSQL twice per 500ms tick, so an
// unbounded stream count lets slow clients exhaust the process and its database
// pool. With the cap set to one, a second concurrent stream must be rejected
// fast with the shared 503 not_ready envelope, and a released slot must be
// reusable by a later stream.
func TestBuildEventsStreamConnectionLimit(t *testing.T) {
	previousLimit := sseMaxConnections
	sseMaxConnections = 1
	t.Cleanup(func() { sseMaxConnections = previousLimit })

	h, _ := testServer(t)
	server := httptest.NewUnstartedServer(h)
	server.Start()
	defer server.Close()

	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createBuildableProject(t, h, adminToken, "sse-limit", slowShutdownCommand())
	buildID := triggerBuild(t, h, adminToken, projectID, `{}`)
	streamURL := server.URL + "/api/v1/builds/" + buildID + "/events"
	open := func() *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, streamURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	first := open()
	if first.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(first.Body)
		first.Body.Close()
		t.Fatalf("first stream = %d %s, want 200", first.StatusCode, body)
	}
	// The 200 response is committed only after the handler reserved a slot, so
	// the cap is already occupied when the second request arrives.
	second := open()
	defer second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second concurrent stream = %d, want 503", second.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(second.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "not_ready" {
		t.Fatalf("capacity error code = %q, want not_ready", envelope.Error.Code)
	}

	// Once the client disconnects, the handler releases the slot and a new
	// stream is accepted again.
	first.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp := open()
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			break
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if time.Now().After(deadline) {
			t.Fatalf("stream slot was not released after the first client disconnected")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
