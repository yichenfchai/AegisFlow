package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
	"github.com/yichenfchai/AegisFlow/backend/internal/policies"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/reviews"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// policyTestServer wires the policy store and the deployment service with the
// policy gate injected, exactly like cmd/server, so the HTTP tests exercise the
// real composition of policy writes and deployment gates. The approval
// environment set is empty: the gate matrix here is about the project policy.
func policyTestServer(t *testing.T) (http.Handler, *sql.DB, *deployments.Service) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	s := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(context.Background(), "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	policyStore := policies.New(db, registry)
	deploymentStore := deployments.New(db, deployments.Options{
		SecretKey:            bytes.Repeat([]byte{9}, 32),
		Projects:             registry,
		CheckTimeout:         2 * time.Second,
		DeployTimeout:        5 * time.Second,
		ApprovalEnvironments: []string{},
		PolicyGate:           policyStore,
	})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(s, logger, db.PingContext, []string{"http://localhost:5173"}, Options{
		Limiter:     newTestLimiter(),
		Projects:    registry,
		Deployments: deploymentStore,
		Reviews:     reviews.New(db, registry),
		Policies:    policyStore,
	}), db, deploymentStore
}

type policyEnvelope struct {
	Data policies.Policy `json:"data"`
}

func parseProjectPolicyHTTP(t *testing.T, body []byte) policies.Policy {
	t.Helper()
	var envelope policyEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("policy payload: %s", string(body))
	}
	return envelope.Data
}

func putPolicyHTTP(h http.Handler, token, projectID, body string) *httptest.ResponseRecorder {
	return request(h, "PUT", "/api/v1/projects/"+projectID+"/policy", body, token)
}

// TestProjectPolicyHTTP covers the policy API contract: defaults, visibility,
// the admin-or-maintainer write matrix, strict JSON, enum validation and the
// transactional policy.update audit row.
func TestProjectPolicyHTTP(t *testing.T) {
	h, db, _ := policyTestServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "policy-maint", "policy-maint-pass-1", "member")
	createUserHTTP(t, h, adminToken, "policy-plain", "policy-plain-pass-1", "member")
	createUserHTTP(t, h, adminToken, "policy-viewer", "policy-viewer-pass-1", "viewer")
	maintToken, maintID, _ := loginHTTP(t, h, "policy-maint", "policy-maint-pass-1")
	plainToken, _, _ := loginHTTP(t, h, "policy-plain", "policy-plain-pass-1")
	viewerToken, _, _ := loginHTTP(t, h, "policy-viewer", "policy-viewer-pass-1")

	projectID := createProjectHTTP(t, h, adminToken, "policy-http", "public")
	privateID := createProjectHTTP(t, h, adminToken, "policy-http-private", "private")

	// Defaults: every visible role reads the documented gate-off policy.
	w := request(h, "GET", "/api/v1/projects/"+projectID+"/policy", "", viewerToken)
	if w.Code != 200 {
		t.Fatalf("default policy get: %d %s", w.Code, w.Body.String())
	}
	defaultPolicy := parseProjectPolicyHTTP(t, w.Body.Bytes())
	if defaultPolicy.ProjectID != projectID || defaultPolicy.RequireReview || defaultPolicy.RequiredScanVerdict != policies.RequiredScanVerdictNone ||
		defaultPolicy.UpdatedBy != "" || defaultPolicy.UpdatedByName != "" || defaultPolicy.UpdatedAt != 0 {
		t.Fatalf("default policy = %+v", defaultPolicy)
	}
	if w = request(h, "GET", "/api/v1/projects/short/policy", "", adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("malformed project id: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/projects/missing-000000000000001/policy", "", adminToken); w.Code != 404 {
		t.Fatalf("unknown project: %d %s", w.Code, w.Body.String())
	}
	// A private project the member cannot see is 404, not 403: writes cannot
	// probe project existence either.
	if w = request(h, "GET", "/api/v1/projects/"+privateID+"/policy", "", plainToken); w.Code != 404 {
		t.Fatalf("invisible project get: %d %s", w.Code, w.Body.String())
	}
	if w = putPolicyHTTP(h, plainToken, privateID, `{"requireReview":true,"requiredScanVerdict":"passed"}`); w.Code != 404 {
		t.Fatalf("invisible project put: %d %s", w.Code, w.Body.String())
	}

	// Only an admin or a project maintainer may write: a viewer and a plain
	// member of the visible public project are both 403.
	for name, token := range map[string]string{"viewer": viewerToken, "member": plainToken} {
		if w = putPolicyHTTP(h, token, projectID, `{"requireReview":true,"requiredScanVerdict":"passed"}`); w.Code != 403 || errorCode(t, w) != "forbidden" {
			t.Fatalf("%s put: %d %s", name, w.Code, w.Body.String())
		}
	}
	// A rejected write leaves no policy row and no audit row.
	if countPolicyAuditRows(t, db, projectID) != 0 {
		t.Fatal("rejected policy writes must not be audited")
	}

	// Promote the maintainer, then let them store the policy.
	addProjectMemberHTTP(t, h, adminToken, projectID, maintID, "maintainer")
	w = putPolicyHTTP(h, maintToken, projectID, `{"requireReview":true,"requiredScanVerdict":"passed"}`)
	if w.Code != 200 {
		t.Fatalf("maintainer put: %d %s", w.Code, w.Body.String())
	}
	updated := parseProjectPolicyHTTP(t, w.Body.Bytes())
	if !updated.RequireReview || updated.RequiredScanVerdict != policies.RequiredScanVerdictPassed ||
		updated.UpdatedBy != maintID || updated.UpdatedByName != "policy-maint" || updated.UpdatedAt == 0 {
		t.Fatalf("stored policy = %+v", updated)
	}
	// Every visible role reads the stored row; the audit detail is limited to the
	// project id and the two gate values.
	for name, token := range map[string]string{"viewer": viewerToken, "member": plainToken, "admin": adminToken} {
		w = request(h, "GET", "/api/v1/projects/"+projectID+"/policy", "", token)
		if w.Code != 200 {
			t.Fatalf("%s policy get: %d %s", name, w.Code, w.Body.String())
		}
		if got := parseProjectPolicyHTTP(t, w.Body.Bytes()); got != updated {
			t.Fatalf("%s policy = %+v, want %+v", name, got, updated)
		}
	}
	var detail string
	if err := db.QueryRow(`SELECT detail FROM auth_audit WHERE action='policy.update' AND object_type='policy' AND object_id=$1`, projectID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	wantDetail := fmt.Sprintf("projectId=%s requireReview=true requiredScanVerdict=passed", projectID)
	if detail != wantDetail {
		t.Fatalf("audit detail = %q, want %q", detail, wantDetail)
	}

	// Strict JSON: unknown fields, null values and a missing field are
	// invalid_request; a bad enum is validation_failed; a non-JSON content type
	// is 415.
	for name, body := range map[string]string{
		"unknown field": `{"requireReview":true,"requiredScanVerdict":"passed","extra":1}`,
		"null field":    `{"requireReview":null,"requiredScanVerdict":"passed"}`,
		"missing field": `{"requireReview":true}`,
	} {
		if w = putPolicyHTTP(h, adminToken, projectID, body); w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w = putPolicyHTTP(h, adminToken, projectID, `{"requireReview":true,"requiredScanVerdict":"failed"}`); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("bad enum: %d %s", w.Code, w.Body.String())
	}
	rawRequest := httptest.NewRequest("PUT", "/api/v1/projects/"+projectID+"/policy", strings.NewReader(`{"requireReview":true,"requiredScanVerdict":"none"}`))
	rawRequest.Header.Set("Content-Type", "text/plain")
	rawRequest.Header.Set("Origin", "http://localhost:5173")
	rawRequest.Header.Set("Authorization", "Bearer "+adminToken)
	raw := httptest.NewRecorder()
	h.ServeHTTP(raw, rawRequest)
	if raw.Code != 415 || errorCode(t, raw) != "unsupported_media_type" {
		t.Fatalf("non-JSON content type: %d %s", raw.Code, raw.Body.String())
	}

	// An admin may write a private project's policy; the minimum body is
	// accepted and returns the replaced values.
	w = putPolicyHTTP(h, adminToken, privateID, `{"requireReview":false,"requiredScanVerdict":"none"}`)
	if w.Code != 200 {
		t.Fatalf("admin private put: %d %s", w.Code, w.Body.String())
	}
	if got := parseProjectPolicyHTTP(t, w.Body.Bytes()); got.RequireReview || got.RequiredScanVerdict != policies.RequiredScanVerdictNone || got.UpdatedByName != "admin" {
		t.Fatalf("admin private policy = %+v", got)
	}
}

// addProjectMemberHTTP adds one existing user to a project through the real
// endpoint, keeping the policy tests focused on the policy contract.
func addProjectMemberHTTP(t *testing.T, h http.Handler, adminToken, projectID, userID, role string) {
	t.Helper()
	body := fmt.Sprintf(`{"userId":%q,"role":%q}`, userID, role)
	if w := request(h, "POST", "/api/v1/projects/"+projectID+"/members", body, adminToken); w.Code != 201 {
		t.Fatalf("add project member: %d %s", w.Code, w.Body.String())
	}
}

func countPolicyAuditRows(t *testing.T, db *sql.DB, projectID string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE action='policy.update' AND object_type='policy' AND object_id=$1`, projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestProjectPolicyDeploymentGateHTTP drives the documented gate matrix through
// the real endpoints with the real policy store: default compatibility, the
// policy review gate on a non-gated environment, the latest-scan gate, and the
// same two gates on rollback.
func TestProjectPolicyDeploymentGateHTTP(t *testing.T) {
	h, db, _ := policyTestServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "policy-gate-http", "public")
	targetID := createDeploymentTargetHTTP(t, h, adminToken, "policy-gate-staging", "staging", upstream.URL, "/deploy")

	// No policy: the deployment is accepted exactly as before.
	w := deploymentCreateHTTP(t, h, adminToken, projectID, targetID, "v0", "")
	if w.Code != 201 {
		t.Fatalf("default create: %d %s", w.Code, w.Body.String())
	}
	first := parseDeployment(t, w)

	// requireReview applies even though staging is not in the environment gate.
	if w = putPolicyHTTP(h, adminToken, projectID, `{"requireReview":true,"requiredScanVerdict":"none"}`); w.Code != 200 {
		t.Fatalf("set review policy: %d %s", w.Code, w.Body.String())
	}
	if w = deploymentCreateHTTP(t, h, adminToken, projectID, targetID, "v1", ""); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("missing review under policy: %d %s", w.Code, w.Body.String())
	}
	review := createReviewHTTP(t, h, adminToken, projectID, "policy gate review")
	approveReviewHTTP(t, h, adminToken, review.ID)
	w = deploymentCreateHTTP(t, h, adminToken, projectID, targetID, "v1", review.ID)
	if w.Code != 201 {
		t.Fatalf("review under policy: %d %s", w.Code, w.Body.String())
	}
	gated := parseDeployment(t, w)
	if gated.ReviewID != review.ID {
		t.Fatalf("gated deployment reviewId = %q, want %q", gated.ReviewID, review.ID)
	}
	var detail string
	if err := db.QueryRow(`SELECT detail FROM auth_audit WHERE action='deployment.create' AND object_type='deployment' AND object_id=$1`, gated.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "policy="+projectID) {
		t.Fatalf("deployment audit detail = %q, want policy context", detail)
	}

	// requiredScanVerdict=passed: no scan and a non-passed latest scan are 400;
	// a newer passed scan lets the deployment through.
	if w = putPolicyHTTP(h, adminToken, projectID, `{"requireReview":false,"requiredScanVerdict":"passed"}`); w.Code != 200 {
		t.Fatalf("set scan policy: %d %s", w.Code, w.Body.String())
	}
	if w = deploymentCreateHTTP(t, h, adminToken, projectID, targetID, "v2", ""); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("no scan under policy: %d %s", w.Code, w.Body.String())
	}
	seedHTTPPolicyScan(t, db, projectID, "failed", time.Now().Unix()-10)
	if w = deploymentCreateHTTP(t, h, adminToken, projectID, targetID, "v2", ""); w.Code != 400 {
		t.Fatalf("failed latest scan under policy: %d %s", w.Code, w.Body.String())
	}
	seedHTTPPolicyScan(t, db, projectID, "passed", time.Now().Unix())
	if w = deploymentCreateHTTP(t, h, adminToken, projectID, targetID, "v2", ""); w.Code != 201 {
		t.Fatalf("passed latest scan under policy: %d %s", w.Code, w.Body.String())
	}

	// Rollback passes the same gates. Make the first deployment a succeeded
	// source and seed one earlier succeeded version to roll back to.
	if _, err := db.Exec(`UPDATE deployments SET status='succeeded' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	seedHTTPPolicyRollbackSource(t, db, projectID, targetID, 0, "v0-base")
	setReviewPolicy := func() {
		if w := putPolicyHTTP(h, adminToken, projectID, `{"requireReview":true,"requiredScanVerdict":"none"}`); w.Code != 200 {
			t.Fatalf("reset review policy: %d %s", w.Code, w.Body.String())
		}
	}
	setReviewPolicy()
	rollbackNoReview := request(h, "POST", "/api/v1/deployments/"+first.ID+"/rollback", `{}`, adminToken)
	if rollbackNoReview.Code != 400 || errorCode(t, rollbackNoReview) != "validation_failed" {
		t.Fatalf("rollback without review under policy: %d %s", rollbackNoReview.Code, rollbackNoReview.Body.String())
	}
	rollbackWithReview := request(h, "POST", "/api/v1/deployments/"+first.ID+"/rollback", `{"reviewId":"`+review.ID+`"}`, adminToken)
	if rollbackWithReview.Code != 201 {
		t.Fatalf("rollback with review under policy: %d %s", rollbackWithReview.Code, rollbackWithReview.Body.String())
	}
	rolledBack := parseDeployment(t, rollbackWithReview)
	if rolledBack.Version != "v0-base" || rolledBack.ReviewID != review.ID || rolledBack.RollbackOf != first.ID {
		t.Fatalf("policy rollback = %+v", rolledBack)
	}

	// Scan gate on rollback: a failed scan inserted now is the latest one, so the
	// rollback is blocked until a newer passed scan lands.
	if w = putPolicyHTTP(h, adminToken, projectID, `{"requireReview":false,"requiredScanVerdict":"passed"}`); w.Code != 200 {
		t.Fatalf("set scan policy again: %d %s", w.Code, w.Body.String())
	}
	seedHTTPPolicyScan(t, db, projectID, "failed", time.Now().Unix()+100)
	if w = request(h, "POST", "/api/v1/deployments/"+first.ID+"/rollback", `{}`, adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("rollback with failed latest scan: %d %s", w.Code, w.Body.String())
	}
	seedHTTPPolicyScan(t, db, projectID, "passed", time.Now().Unix()+101)
	if w = request(h, "POST", "/api/v1/deployments/"+first.ID+"/rollback", `{}`, adminToken); w.Code != 201 {
		t.Fatalf("rollback with passed latest scan: %d %s", w.Code, w.Body.String())
	}
}

// seedHTTPPolicyScan inserts one scan row directly; scan ingestion is covered by
// its own signed-webhook tests, and the policy gate only reads the stored row.
func seedHTTPPolicyScan(t *testing.T, db *sql.DB, projectID, verdict string, receivedAt int64) {
	t.Helper()
	id := seedHTTPID(t)
	if _, err := db.Exec(`INSERT INTO scans(id,project_id,delivery_id,source,tool,version,verdict,summary,received_at)
	 VALUES($1,$2,$3,'test-source','trivy','0.55.0',$4,'',$5)`, id, projectID, "delivery-"+id, verdict, receivedAt); err != nil {
		t.Fatal(err)
	}
}

// seedHTTPPolicyRollbackSource inserts an earlier succeeded deployment row so a
// rollback has a version to pick.
func seedHTTPPolicyRollbackSource(t *testing.T, db *sql.DB, projectID, targetID string, number int64, version string) string {
	t.Helper()
	id := seedHTTPID(t)
	if _, err := db.Exec(`INSERT INTO deployments(id,project_id,target_id,target_name,target_endpoint,target_environment,deployment_number,status,version,triggered_by,created_at)
	 VALUES($1,$2,$3,'snapshot','https://snapshot.example.com','staging',$4,'succeeded',$5,$6,$7)`,
		id, projectID, targetID, number, version, "admin", time.Now().Unix()-60); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE projects SET deploy_count=GREATEST(deploy_count,$2) WHERE id=$1`, projectID, number); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedHTTPID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
