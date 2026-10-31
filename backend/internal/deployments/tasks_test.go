package deployments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// taskService builds a service with a short deploy timeout so failure paths do
// not slow the suite down.
func taskService(t *testing.T, opts Options) (*Service, *sql.DB, *projects.Service) {
	t.Helper()
	if len(opts.SecretKey) == 0 {
		opts.SecretKey = []byte(testSecretKey)
	}
	if opts.DeployTimeout == 0 {
		opts.DeployTimeout = 5 * time.Second
	}
	s, db := testServiceWith(t, opts)
	return s, db, projects.New(db)
}

func taskProject(t *testing.T, registry *projects.Service, name string) projects.Project {
	t.Helper()
	project, err := registry.Create(context.Background(), testActor, projects.Input{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return *project
}

func taskTarget(t *testing.T, s *Service, name, endpoint string) Target {
	t.Helper()
	target, err := s.Create(context.Background(), testActor, Input{Name: name, Endpoint: endpoint, Environment: EnvironmentProduction})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// startDeployWorker runs the loop directly; lease behaviour is covered by the
// lease test, so execution tests do not contend for it.
func startDeployWorker(t *testing.T, s *Service) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go s.run(ctx)
	t.Cleanup(cancel)
	return cancel
}

func waitDeploymentStatus(t *testing.T, s *Service, id string, want ...string) Deployment {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		deployment, err := s.GetDeployment(context.Background(), testActor, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range want {
			if deployment.Status == status {
				return deployment
			}
		}
		if deployment.Status == DeploySucceeded || deployment.Status == DeployFailed || deployment.Status == DeployCancelled || deployment.Status == DeployInterrupted || deployment.Status == DeployRejected {
			t.Fatalf("deployment settled in %s, want %v", deployment.Status, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %v, status=%s", want, deployment.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func deploymentEvents(t *testing.T, s *Service, id string) []DeploymentEvent {
	t.Helper()
	events, err := s.DeploymentEvents(context.Background(), testActor, id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func deploymentAudit(t *testing.T, db *sql.DB, deploymentID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT action||'|'||detail FROM auth_audit WHERE object_type='deployment' AND object_id=$1 ORDER BY created_at,id`, deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func seedDeployment(t *testing.T, db *sql.DB, projectID, targetID string, number int64, status, version string, createdAt int64) string {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO deployments(id,project_id,target_id,target_name,target_endpoint,target_environment,deployment_number,status,version,triggered_by,created_at)
	 VALUES($1,$2,$3,'snapshot','https://snapshot.example.com','production',$4,$5,$6,$7,$8)`,
		id, projectID, targetID, number, status, version, testActor.ID, createdAt); err != nil {
		t.Fatal(err)
	}
	// Keep the allocator consistent with directly seeded rows.
	if _, err = db.Exec(`UPDATE projects SET deploy_count=GREATEST(deploy_count,$2) WHERE id=$1`, projectID, number); err != nil {
		t.Fatal(err)
	}
	return id
}

func waitForEventText(t *testing.T, s *Service, id, substr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if strings.Contains(joinEvents(deploymentEvents(t, s, id)), substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("event %q not seen; got %s", substr, joinEvents(deploymentEvents(t, s, id)))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestCreateDeploymentValidationSnapshotAndNumbering(t *testing.T) {
	s, db, registry := taskService(t, Options{})
	ctx := context.Background()
	project := taskProject(t, registry, "deploy-numbering")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := taskTarget(t, s, "prod-target", upstream.URL)
	disabled, err := s.Update(ctx, testActor, target.ID, Patch{Status: str(StatusDisabled)})
	if err != nil || disabled.Status != StatusDisabled {
		t.Fatalf("disable target = %+v err=%v", disabled, err)
	}
	// A second active target for the happy path.
	target = taskTarget(t, s, "prod-target-2", upstream.URL)

	buildID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO builds(id,project_id,build_number,status,branch,command,triggered_by,created_at)
	 VALUES($1,$2,1,'succeeded','main','echo ok',$3,$4)`, buildID, project.ID, testActor.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	failedBuildID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO builds(id,project_id,build_number,status,branch,command,triggered_by,created_at)
	 VALUES($1,$2,2,'failed','main','echo ok',$3,$4)`, failedBuildID, project.ID, testActor.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	other := taskProject(t, registry, "deploy-other")

	invalid := []struct {
		projectID string
		input     DeployInput
	}{
		{"short", DeployInput{TargetID: target.ID, Version: "v1"}},
		{project.ID, DeployInput{TargetID: "short", Version: "v1"}},
		{project.ID, DeployInput{TargetID: target.ID, Version: "v 1"}},
		{project.ID, DeployInput{TargetID: target.ID, Version: strings.Repeat("v", 65)}},
		{project.ID, DeployInput{TargetID: target.ID, Version: "v1", BuildID: "short"}},
		{project.ID, DeployInput{TargetID: target.ID, Version: "v1", BuildID: buildID + "x"}},
		{project.ID, DeployInput{TargetID: target.ID, Version: "v1", BuildID: failedBuildID}},
		{other.ID, DeployInput{TargetID: target.ID, Version: "v1", BuildID: buildID}},
	}
	for _, tc := range invalid {
		if _, err := s.CreateDeployment(ctx, testActor, tc.projectID, tc.input); !errors.Is(err, ErrValidation) {
			t.Fatalf("CreateDeployment(%q,%+v) = %v, want ErrValidation", tc.projectID, tc.input, err)
		}
	}
	if _, err := s.CreateDeployment(ctx, testActor, "missing-0000000000000001", DeployInput{TargetID: target.ID, Version: "v1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project error = %v", err)
	}
	if _, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: "missing-0000000000000001", Version: "v1"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown target error = %v", err)
	}
	if _, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: disabled.ID, Version: "v1"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("disabled target error = %v", err)
	}

	first, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1.2.3", BuildID: buildID})
	if err != nil {
		t.Fatal(err)
	}
	if first.DeploymentNumber != 1 || first.Status != DeployQueued || first.Version != "v1.2.3" || first.BuildID != buildID ||
		first.TargetID != target.ID || first.TargetName != target.Name || first.TargetEndpoint != target.Endpoint ||
		first.TargetEnvironment != EnvironmentProduction || first.TriggeredBy != testActor.ID || first.CreatedAt == 0 {
		t.Fatalf("first deployment = %+v", first)
	}
	// Snapshot stays fixed when the target changes afterwards.
	if _, err := s.Update(ctx, testActor, target.ID, Patch{Name: str("renamed-target"), Endpoint: str("https://changed.example.com")}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.GetDeployment(ctx, testActor, first.ID)
	if err != nil || snapshot.TargetName != target.Name || snapshot.TargetEndpoint != target.Endpoint {
		t.Fatalf("snapshot changed: %+v err=%v", snapshot, err)
	}
	second, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v2"})
	if err != nil || second.DeploymentNumber != 2 {
		t.Fatalf("second deployment = %+v err=%v", second, err)
	}

	list, total, err := s.ListDeployments(ctx, testActor, project.ID, "", 10, 0)
	if err != nil || total != 2 || len(list) != 2 || list[0].ID != second.ID {
		t.Fatalf("list = %+v total=%d err=%v", list, total, err)
	}
	if _, total, err = s.ListDeployments(ctx, testActor, project.ID, DeployQueued, 10, 0); err != nil || total != 2 {
		t.Fatalf("status filter total=%d err=%v", total, err)
	}
	if _, _, err = s.ListDeployments(ctx, testActor, project.ID, "bogus", 10, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad status error = %v", err)
	}
	if _, _, err = s.ListDeployments(ctx, testActor, "short", "", 10, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad project error = %v", err)
	}
	if _, _, err = s.ListDeployments(ctx, testActor, "", "", 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad bounds error = %v", err)
	}
	if _, err = s.GetDeployment(ctx, testActor, "short"); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed get error = %v", err)
	}
	if _, err = s.GetDeployment(ctx, testActor, "missing-0000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown get error = %v", err)
	}

	rows := deploymentAudit(t, db, first.ID)
	if len(rows) != 1 || rows[0] != ActionDeployCreate+"|target="+target.Name+" version=v1.2.3 approval=none" {
		t.Fatalf("create audit = %v", rows)
	}
}

func TestDeployWorkerSuccessPayloadEventsAndAudit(t *testing.T) {
	var mu sync.Mutex
	var requests []deployPayload
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/deploy" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer deploy-token" {
			t.Errorf("authorization = %q", got)
		}
		var payload deployPayload
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("payload: %v", err)
		}
		mu.Lock()
		requests = append(requests, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "line one\nline two\n")
	}))
	defer upstream.Close()

	s, db, registry := taskService(t, Options{})
	project := taskProject(t, registry, "deploy-worker")
	target, err := s.Create(context.Background(), testActor, Input{Name: "secret-target", Endpoint: upstream.URL, Environment: EnvironmentStaging, Secret: "deploy-token"})
	if err != nil {
		t.Fatal(err)
	}
	startDeployWorker(t, s)

	deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v9.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	deployment = waitDeploymentStatus(t, s, deployment.ID, DeploySucceeded)
	waitForEventText(t, s, deployment.ID, "system:deployment succeeded")
	if deployment.StartedAt == 0 || deployment.FinishedAt == 0 || deployment.Detail != "" {
		t.Fatalf("succeeded deployment = %+v", deployment)
	}
	mu.Lock()
	if len(requests) != 1 {
		t.Fatalf("requests = %+v", requests)
	}
	payload := requests[0]
	mu.Unlock()
	if payload.DeploymentID != deployment.ID || payload.Project != project.Name || payload.Version != "v9.1.0" ||
		payload.Environment != EnvironmentStaging || payload.Rollback || payload.RollbackVersion != "" {
		t.Fatalf("payload = %+v", payload)
	}

	events := deploymentEvents(t, s, deployment.ID)
	var joined []string
	for _, event := range events {
		joined = append(joined, event.Stream+":"+event.Message)
	}
	text := strings.Join(joined, "\n")
	for _, want := range []string{"system:deployment #1 started", "response:line one", "response:line two", "system:deployment succeeded"} {
		if !strings.Contains(text, want) {
			t.Fatalf("events missing %q: %s", want, text)
		}
	}
	if len(events) < 4 {
		t.Fatalf("expected lifecycle and response events, got %d", len(events))
	}
	// Cursor paging: after the first event only later ids are returned.
	afterFirst, err := s.DeploymentEvents(context.Background(), testActor, deployment.ID, events[0].ID, 1000)
	if err != nil || len(afterFirst) != len(events)-1 {
		t.Fatalf("after first = %d events, err=%v", len(afterFirst), err)
	}
	if _, err = s.DeploymentEvents(context.Background(), testActor, deployment.ID, -1, 10); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative cursor error = %v", err)
	}
	if _, err = s.DeploymentEvents(context.Background(), testActor, deployment.ID, 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad limit error = %v", err)
	}
	if _, err = s.DeploymentEvents(context.Background(), testActor, "missing-0000000000000001", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown events error = %v", err)
	}

	rows := deploymentAudit(t, db, deployment.ID)
	if len(rows) != 1 || !strings.HasPrefix(rows[0], ActionDeployCreate+"|") {
		t.Fatalf("worker wrote unexpected audit rows: %v", rows)
	}
}

func TestDeployWorkerFailureTimeoutAndDisabledTarget(t *testing.T) {
	t.Run("http failure", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "deployment rejected\n")
		}))
		defer upstream.Close()
		s, _, registry := taskService(t, Options{})
		project := taskProject(t, registry, "deploy-failure")
		target := taskTarget(t, s, "failing", upstream.URL)
		startDeployWorker(t, s)
		deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		deployment = waitDeploymentStatus(t, s, deployment.ID, DeployFailed)
		if deployment.Detail != "http 500" {
			t.Fatalf("detail = %q", deployment.Detail)
		}
		waitForEventText(t, s, deployment.ID, "deployment failed: http 500")
		if text := joinEvents(deploymentEvents(t, s, deployment.ID)); !strings.Contains(text, "deployment failed: http 500") {
			t.Fatalf("events = %s", text)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		// The handler is bounded: httptest.Server.Close waits for it, and peer
		// cancellation is not surfaced to a handler on every platform.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
		}))
		defer upstream.Close()
		s, _, registry := taskService(t, Options{DeployTimeout: 100 * time.Millisecond})
		project := taskProject(t, registry, "deploy-timeout")
		target := taskTarget(t, s, "slow", upstream.URL)
		startDeployWorker(t, s)
		deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		deployment = waitDeploymentStatus(t, s, deployment.ID, DeployFailed)
		if deployment.Detail != "timeout" {
			t.Fatalf("detail = %q", deployment.Detail)
		}
	})

	t.Run("target disabled after enqueue", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer upstream.Close()
		s, _, registry := taskService(t, Options{})
		project := taskProject(t, registry, "deploy-disabled")
		target := taskTarget(t, s, "to-disable", upstream.URL)
		deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Update(context.Background(), testActor, target.ID, Patch{Status: str(StatusDisabled)}); err != nil {
			t.Fatal(err)
		}
		startDeployWorker(t, s)
		deployment = waitDeploymentStatus(t, s, deployment.ID, DeployFailed)
		if deployment.Detail != "deployment target is disabled" {
			t.Fatalf("detail = %q", deployment.Detail)
		}
	})
}

func TestDeployWorkerCancelAndRecovery(t *testing.T) {
	cancelSeen := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			once.Do(func() { close(cancelSeen) })
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)

	s, db, registry := taskService(t, Options{})
	project := taskProject(t, registry, "deploy-cancel")
	target := taskTarget(t, s, "cancellable", upstream.URL)
	startDeployWorker(t, s)

	deployment, err := s.CreateDeployment(context.Background(), testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	waitDeploymentStatus(t, s, deployment.ID, DeployRunning)
	cancelled, err := s.CancelDeployment(context.Background(), testActor, deployment.ID)
	if err != nil || cancelled.Status != DeployCancelled {
		t.Fatalf("cancel = %+v err=%v", cancelled, err)
	}
	select {
	case <-cancelSeen:
	case <-time.After(2 * time.Second):
		// The status and audit assertions below are authoritative; peer
		// cancellation delivery to the handler is platform dependent.
	}
	waitDeploymentStatus(t, s, deployment.ID, DeployCancelled)

	// Idempotent repeat writes no second audit row.
	again, err := s.CancelDeployment(context.Background(), testActor, deployment.ID)
	if err != nil || again.Status != DeployCancelled {
		t.Fatalf("repeat cancel = %+v err=%v", again, err)
	}
	rows := deploymentAudit(t, db, deployment.ID)
	cancels := 0
	for _, row := range rows {
		if strings.HasPrefix(row, ActionDeployCancel+"|from=running") {
			cancels++
		}
	}
	if cancels != 1 || len(rows) != 2 {
		t.Fatalf("audit rows = %v", rows)
	}

	// Restart recovery turns a leftover running row into interrupted.
	running := seedDeployment(t, db, project.ID, target.ID, 2, DeployRunning, "v2", time.Now().Unix())
	recovered, err := s.RecoverInterrupted(context.Background())
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	interrupted, err := s.GetDeployment(context.Background(), testActor, running)
	if err != nil || interrupted.Status != DeployInterrupted || interrupted.Detail != "interrupted by server restart" || interrupted.FinishedAt == 0 {
		t.Fatalf("interrupted = %+v err=%v", interrupted, err)
	}
	// A terminal deployment cannot be cancelled.
	if _, err = s.CancelDeployment(context.Background(), testActor, running); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel interrupted error = %v", err)
	}
	if recovered, err = s.RecoverInterrupted(context.Background()); err != nil || recovered != 0 {
		t.Fatalf("second recovery = %d err=%v", recovered, err)
	}
}

func TestRollbackSelectsEarlierVersionAndAudits(t *testing.T) {
	s, db, registry := taskService(t, Options{})
	ctx := context.Background()
	project := taskProject(t, registry, "deploy-rollback")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := taskTarget(t, s, "rollback-target", upstream.URL)

	base := time.Now().Unix()
	first := seedDeployment(t, db, project.ID, target.ID, 1, DeploySucceeded, "v1", base-30)
	seedDeployment(t, db, project.ID, target.ID, 2, DeploySucceeded, "v2", base-20)
	source := seedDeployment(t, db, project.ID, target.ID, 3, DeployFailed, "v3", base-10)

	rollback, err := s.RollbackDeployment(ctx, testActor, source)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Status != DeployQueued || rollback.Version != "v2" || rollback.RollbackOf != source ||
		rollback.DeploymentNumber != 4 || rollback.TargetID != target.ID || rollback.BuildID != "" {
		t.Fatalf("rollback = %+v", rollback)
	}
	rows := deploymentAudit(t, db, rollback.ID)
	if len(rows) != 1 || rows[0] != ActionDeployRollback+"|rollback_of="+source+" version=v2" {
		t.Fatalf("rollback audit = %v", rows)
	}
	// A second rollback from the same source creates another row (not idempotent).
	second, err := s.RollbackDeployment(ctx, testActor, source)
	if err != nil || second.ID == rollback.ID || second.DeploymentNumber != 5 || second.Version != "v2" {
		t.Fatalf("second rollback = %+v err=%v", second, err)
	}
	// The earliest succeeded deployment has no earlier success.
	if _, err = s.RollbackDeployment(ctx, testActor, first); !errors.Is(err, ErrValidation) {
		t.Fatalf("no earlier success error = %v", err)
	}
	// A queued source is not terminal.
	queued := seedDeployment(t, db, project.ID, target.ID, 6, DeployQueued, "v4", base)
	if _, err = s.RollbackDeployment(ctx, testActor, queued); !errors.Is(err, ErrConflict) {
		t.Fatalf("queued rollback error = %v", err)
	}
	if _, err = s.RollbackDeployment(ctx, testActor, "missing-0000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown rollback error = %v", err)
	}
	if _, err = s.RollbackDeployment(ctx, testActor, "short"); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed rollback error = %v", err)
	}
}

func TestRollbackExecutesWithRollbackPayload(t *testing.T) {
	var mu sync.Mutex
	var payloads []deployPayload
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload deployPayload
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		payloads = append(payloads, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	s, db, registry := taskService(t, Options{})
	ctx := context.Background()
	project := taskProject(t, registry, "deploy-rollback-run")
	target := taskTarget(t, s, "rollback-run-target", upstream.URL)
	base := time.Now().Unix()
	seedDeployment(t, db, project.ID, target.ID, 1, DeploySucceeded, "v1", base-30)
	source := seedDeployment(t, db, project.ID, target.ID, 2, DeploySucceeded, "v2", base-20)
	startDeployWorker(t, s)

	rollback, err := s.RollbackDeployment(ctx, testActor, source)
	if err != nil {
		t.Fatal(err)
	}
	waitDeploymentStatus(t, s, rollback.ID, DeploySucceeded)
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 1 {
		t.Fatalf("payloads = %+v", payloads)
	}
	if got := payloads[0]; !got.Rollback || got.RollbackVersion != "v1" || got.Version != "v1" || got.DeploymentID != rollback.ID {
		t.Fatalf("rollback payload = %+v", got)
	}
}

func TestDeploymentTargetCannotBeRemovedWithHistory(t *testing.T) {
	s, _, registry := taskService(t, Options{})
	ctx := context.Background()
	project := taskProject(t, registry, "deploy-remove")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target := taskTarget(t, s, "referenced", upstream.URL)
	if _, err := s.CreateDeployment(ctx, testActor, project.ID, DeployInput{TargetID: target.ID, Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, testActor, target.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("remove referenced target error = %v", err)
	}
	if _, err := s.GetDeployment(ctx, testActor, "missing-0000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deployment error = %v", err)
	}
}

func TestDeploymentWorkerLeaseIsExclusive(t *testing.T) {
	s1, db := testService(t)
	ctx := context.Background()
	s2 := New(testutil.ClonePostgres(t, db), Options{SecretKey: []byte(testSecretKey)})
	lease, held, err := s1.AcquireWorkerLease(ctx)
	if err != nil || !held {
		t.Fatalf("first acquire held=%v err=%v", held, err)
	}
	if other, held, err := s2.AcquireWorkerLease(ctx); err != nil || held {
		if other != nil {
			other.Release()
		}
		t.Fatalf("second acquire held=%v err=%v", held, err)
	}
	lease.Release()
	next, held, err := s2.AcquireWorkerLease(ctx)
	if err != nil || !held {
		t.Fatalf("acquire after release held=%v err=%v", held, err)
	}
	next.Release()
}

func joinEvents(events []DeploymentEvent) string {
	var parts []string
	for _, event := range events {
		parts = append(parts, event.Stream+":"+event.Message)
	}
	return strings.Join(parts, "\n")
}
