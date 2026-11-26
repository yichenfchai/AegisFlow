package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/reviews"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// reviewTestServer wires the review center next to the real auth service and
// the project registry, mirroring cmd/server.
func reviewTestServer(t *testing.T) (http.Handler, *auth.Service, *sql.DB) {
	t.Helper()
	return reviewTestServerWithOptions(t, Options{})
}

// reviewTestServerWithOptions is reviewTestServer with explicit options (for
// example ReviewWebhookSecret for the signed external-result route).
func reviewTestServerWithOptions(t *testing.T, options Options) (http.Handler, *auth.Service, *sql.DB) {
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
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if options.Limiter == nil {
		options.Limiter = newTestLimiter()
	}
	if options.Projects == nil {
		options.Projects = registry
	}
	if options.Reviews == nil {
		options.Reviews = reviews.New(db, registry)
	}
	return New(s, logger, db.PingContext, []string{"http://localhost:5173"}, options), s, db
}

type reviewEnvelope struct {
	Data reviews.Review `json:"data"`
}

func parseReviewHTTP(t *testing.T, body []byte) reviews.Review {
	t.Helper()
	var envelope reviewEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("review payload: %s", string(body))
	}
	return envelope.Data
}

type reviewPageEnvelope struct {
	Data struct {
		Items  []reviews.Review `json:"items"`
		Total  int              `json:"total"`
		Limit  int              `json:"limit"`
		Offset int              `json:"offset"`
	} `json:"data"`
}

func parseReviewPageHTTP(t *testing.T, body []byte) reviewPageEnvelope {
	t.Helper()
	var envelope reviewPageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("review page payload: %s", string(body))
	}
	return envelope
}

func createProjectHTTP(t *testing.T, h http.Handler, token, name, visibility string) string {
	t.Helper()
	body := `{"name":"` + name + `","visibility":"` + visibility + `"}`
	w := request(h, "POST", "/api/v1/projects", body, token)
	if w.Code != 201 {
		t.Fatalf("create project: %d %s", w.Code, w.Body.String())
	}
	var payload struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.Data.ID == "" {
		t.Fatalf("project payload: %s", w.Body.String())
	}
	return payload.Data.ID
}

func createReviewHTTP(t *testing.T, h http.Handler, token, projectID, subject string) reviews.Review {
	t.Helper()
	w := request(h, "POST", "/api/v1/projects/"+projectID+"/reviews",
		`{"subject":"`+subject+`","description":"details","version":"v1.0.0","externalUrl":"https://example.com/pr/1"}`, token)
	if w.Code != 201 {
		t.Fatalf("create review: %d %s", w.Code, w.Body.String())
	}
	return parseReviewHTTP(t, w.Body.Bytes())
}

func reviewAuditCountHTTP(t *testing.T, db *sql.DB, action string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE object_type='review' AND action=$1`, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestReviewRoutesRBACAndLifecycle(t *testing.T) {
	h, _, db := reviewTestServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "review-maint", "maintainer-pass-1", "member")
	createUserHTTP(t, h, adminToken, "review-member", "member-password-01", "member")
	createUserHTTP(t, h, adminToken, "review-viewer", "viewer-password-1", "viewer")
	maintToken, maintID, _ := loginHTTP(t, h, "review-maint", "maintainer-pass-1")
	memberToken, memberID, _ := loginHTTP(t, h, "review-member", "member-password-01")
	viewerToken, _, _ := loginHTTP(t, h, "review-viewer", "viewer-password-1")

	projectID := createProjectHTTP(t, h, adminToken, "review-http-public", "public")
	if w := request(h, "POST", "/api/v1/projects/"+projectID+"/members", `{"userId":"`+maintID+`","role":"maintainer"}`, adminToken); w.Code != 201 {
		t.Fatalf("add maintainer: %d %s", w.Code, w.Body.String())
	}

	// Every review endpoint requires authentication.
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/v1/projects/" + projectID + "/reviews", ""},
		{"POST", "/api/v1/projects/" + projectID + "/reviews", `{"subject":"unauthenticated"}`},
		{"GET", "/api/v1/reviews/missing-0000000000001", ""},
		{"POST", "/api/v1/reviews/missing-0000000000001/approve", `{}`},
		{"POST", "/api/v1/reviews/missing-0000000000001/reject", `{"reason":"no"}`},
		{"POST", "/api/v1/reviews/missing-0000000000001/cancel", `{}`},
	} {
		if w := request(h, route.method, route.path, route.body, ""); w.Code != 401 || errorCode(t, w) != "unauthorized" {
			t.Fatalf("unauthenticated %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}

	// A public-project member may request a review; a viewer may not write.
	if w := request(h, "POST", "/api/v1/projects/"+projectID+"/reviews", `{"subject":"viewer request"}`, viewerToken); w.Code != 403 || errorCode(t, w) != "forbidden" {
		t.Fatalf("viewer create: %d %s", w.Code, w.Body.String())
	}
	first := createReviewHTTP(t, h, memberToken, projectID, "first change request")
	if first.Number != 1 || first.Status != "pending" || first.RequestedBy != memberID || first.RequestedByName != "review-member" {
		t.Fatalf("created review = %+v", first)
	}
	if first.DecidedBy != nil || first.DecidedAt != nil || first.Reason != nil {
		t.Fatalf("pending review must omit decision fields: %+v", first)
	}

	// Reads are available to anyone who can see the project.
	w := request(h, "GET", "/api/v1/projects/"+projectID+"/reviews", "", viewerToken)
	if w.Code != 200 {
		t.Fatalf("viewer list: %d %s", w.Code, w.Body.String())
	}
	if page := parseReviewPageHTTP(t, w.Body.Bytes()); page.Data.Total != 1 || len(page.Data.Items) != 1 || page.Data.Items[0].ID != first.ID {
		t.Fatalf("viewer list page = %+v", page.Data)
	}
	if w = request(h, "GET", "/api/v1/reviews/"+first.ID, "", viewerToken); w.Code != 200 || parseReviewHTTP(t, w.Body.Bytes()).ID != first.ID {
		t.Fatalf("viewer get: %d %s", w.Code, w.Body.String())
	}

	// A plain member cannot approve; the maintainer can, and repetition is
	// idempotent with no duplicate audit row.
	if w = request(h, "POST", "/api/v1/reviews/"+first.ID+"/approve", `{"reason":"not a maintainer"}`, memberToken); w.Code != 403 || errorCode(t, w) != "forbidden" {
		t.Fatalf("member approve: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "POST", "/api/v1/reviews/"+first.ID+"/approve", `{"reason":"looks good"}`, maintToken)
	if w.Code != 200 {
		t.Fatalf("maintainer approve: %d %s", w.Code, w.Body.String())
	}
	approved := parseReviewHTTP(t, w.Body.Bytes())
	if approved.Status != "approved" || approved.DecidedBy == nil || *approved.DecidedBy != maintID || approved.Reason == nil || *approved.Reason != "looks good" {
		t.Fatalf("approved review = %+v", approved)
	}
	if w = request(h, "POST", "/api/v1/reviews/"+first.ID+"/approve", `{"reason":"again"}`, adminToken); w.Code != 200 {
		t.Fatalf("idempotent approve: %d %s", w.Code, w.Body.String())
	}
	if got := reviewAuditCountHTTP(t, db, "review.approve"); got != 1 {
		t.Fatalf("approve audit rows = %d, want 1", got)
	}
	if w = request(h, "POST", "/api/v1/reviews/"+first.ID+"/reject", `{"reason":"late"}`, maintToken); w.Code != 409 || errorCode(t, w) != "conflict" {
		t.Fatalf("reject approved: %d %s", w.Code, w.Body.String())
	}

	// The requester may cancel their own pending request; repetition is a no-op.
	second := createReviewHTTP(t, h, memberToken, projectID, "second change request")
	if second.Number != 2 {
		t.Fatalf("second number = %d", second.Number)
	}
	if w = request(h, "POST", "/api/v1/reviews/"+second.ID+"/cancel", `{}`, memberToken); w.Code != 200 {
		t.Fatalf("requester cancel: %d %s", w.Code, w.Body.String())
	}
	if cancelled := parseReviewHTTP(t, w.Body.Bytes()); cancelled.Status != "cancelled" || cancelled.DecidedBy == nil || *cancelled.DecidedBy != memberID {
		t.Fatalf("cancelled review = %+v", cancelled)
	}
	if w = request(h, "POST", "/api/v1/reviews/"+second.ID+"/cancel", `{}`, maintToken); w.Code != 200 {
		t.Fatalf("idempotent cancel: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/reviews/"+second.ID+"/approve", `{}`, maintToken); w.Code != 409 {
		t.Fatalf("approve cancelled: %d %s", w.Code, w.Body.String())
	}
	if got := reviewAuditCountHTTP(t, db, "review.cancel"); got != 1 {
		t.Fatalf("cancel audit rows = %d, want 1", got)
	}

	// Rejection requires a reason and is idempotent as well.
	third := createReviewHTTP(t, h, memberToken, projectID, "third change request")
	if w = request(h, "POST", "/api/v1/reviews/"+third.ID+"/reject", `{}`, maintToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("reject without reason: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/reviews/"+third.ID+"/reject", `{"reason":"needs tests"}`, maintToken); w.Code != 200 {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/reviews/"+third.ID+"/reject", `{"reason":"needs tests"}`, maintToken); w.Code != 200 {
		t.Fatalf("idempotent reject: %d %s", w.Code, w.Body.String())
	}
	if got := reviewAuditCountHTTP(t, db, "review.reject"); got != 1 {
		t.Fatalf("reject audit rows = %d, want 1", got)
	}

	// List filters and pagination are validated server-side.
	if w = request(h, "GET", "/api/v1/projects/"+projectID+"/reviews?status=approved", "", viewerToken); w.Code != 200 {
		t.Fatalf("status filter: %d", w.Code)
	} else if page := parseReviewPageHTTP(t, w.Body.Bytes()); page.Data.Total != 1 || page.Data.Items[0].ID != first.ID {
		t.Fatalf("status filter page = %+v", page.Data)
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?offset=-1"} {
		if w = request(h, "GET", "/api/v1/projects/"+projectID+"/reviews"+query, "", viewerToken); w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Fatalf("invalid list query %q: %d %s", query, w.Code, w.Body.String())
		}
	}
	if w = request(h, "GET", "/api/v1/projects/"+projectID+"/reviews?status=deleted", "", viewerToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("invalid status filter: %d %s", w.Code, w.Body.String())
	}

	// The audit trail carries review actions and never the review body.
	var detail string
	if err := db.QueryRow(`SELECT detail FROM auth_audit WHERE action='review.request' AND object_id=$1`, first.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "number=1") {
		t.Fatalf("request audit detail = %q", detail)
	}
	for _, forbidden := range []string{"first change request", "details", "v1.0.0", "example.com"} {
		var leaked int
		if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE object_type='review' AND detail LIKE '%'||$1||'%'`, forbidden).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Fatalf("audit detail leaked %q", forbidden)
		}
	}
}

func TestReviewRoutesCrossProjectAndStrictJSON(t *testing.T) {
	h, _, _ := reviewTestServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "review-outsider", "member-password-01", "member")
	outsiderToken, _, _ := loginHTTP(t, h, "review-outsider", "member-password-01")

	publicID := createProjectHTTP(t, h, adminToken, "strict-public", "public")
	privateID := createProjectHTTP(t, h, adminToken, "strict-private", "private")
	privateReview := createReviewHTTP(t, h, adminToken, privateID, "private review")
	publicReview := createReviewHTTP(t, h, adminToken, publicID, "public review")

	// A non-member cannot see, list, decide or cancel a private project's review.
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/v1/reviews/" + privateReview.ID, ""},
		{"POST", "/api/v1/reviews/" + privateReview.ID + "/approve", `{}`},
		{"POST", "/api/v1/reviews/" + privateReview.ID + "/reject", `{"reason":"no"}`},
		{"POST", "/api/v1/reviews/" + privateReview.ID + "/cancel", `{}`},
		{"GET", "/api/v1/projects/" + privateID + "/reviews", ""},
	} {
		if w := request(h, route.method, route.path, route.body, outsiderToken); w.Code != 404 || errorCode(t, w) != "not_found" {
			t.Fatalf("invisible project %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	if w := request(h, "POST", "/api/v1/projects/"+privateID+"/reviews", `{"subject":"outsider request"}`, outsiderToken); w.Code != 404 {
		t.Fatalf("invisible create: %d %s", w.Code, w.Body.String())
	}

	// Strict JSON: unknown fields, nulls, arrays, extra documents and missing
	// bodies are all rejected with the shared envelope.
	createPath := "/api/v1/projects/" + publicID + "/reviews"
	for _, body := range []string{
		`{"subject":"valid subject","unknown":true}`,
		`{"subject":null}`,
		`{"subject":["not","a","string"]}`,
		`[]`,
		`null`,
		`{} {}`,
		`{"subject":"ab"}`,
		`{"subject":"` + strings.Repeat("s", 201) + `"}`,
		`{"subject":"valid subject","externalUrl":"ftp://example.com/x"}`,
		`{"subject":"valid subject","externalUrl":"https://user:pass@example.com/x"}`,
		`{"subject":"valid subject","externalUrl":"https://example.com/x#frag"}`,
	} {
		if w := request(h, "POST", createPath, body, adminToken); w.Code != 400 {
			t.Fatalf("create body %q: %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, route := range []struct{ path, body string }{
		{"/api/v1/reviews/" + publicReview.ID + "/approve", `{"unknown":1}`},
		{"/api/v1/reviews/" + publicReview.ID + "/approve", `{"reason":null}`},
		{"/api/v1/reviews/" + publicReview.ID + "/reject", `{"reason":"x","extra":1}`},
		{"/api/v1/reviews/" + publicReview.ID + "/cancel", `{"reason":"not allowed"}`},
		{"/api/v1/reviews/" + publicReview.ID + "/cancel", `[]`},
	} {
		if w := request(h, "POST", route.path, route.body, adminToken); w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Fatalf("strict JSON %s %q: %d %s", route.path, route.body, w.Code, w.Body.String())
		}
	}
	// A wrong Content-Type is a 415, not a decoding failure.
	raw := httptest.NewRequest("POST", createPath, strings.NewReader(`{"subject":"valid subject"}`))
	raw.Header.Set("Origin", "http://localhost:5173")
	raw.Header.Set("Authorization", "Bearer "+adminToken)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, raw)
	if recorder.Code != 415 || errorCode(t, recorder) != "unsupported_media_type" {
		t.Fatalf("missing content type: %d %s", recorder.Code, recorder.Body.String())
	}

	// A non-requester member of a visible project may not cancel someone else's
	// review; decisions stay maintainer/admin-only.
	if w := request(h, "POST", "/api/v1/reviews/"+publicReview.ID+"/cancel", `{}`, outsiderToken); w.Code != 403 || errorCode(t, w) != "forbidden" {
		t.Fatalf("non-requester cancel: %d %s", w.Code, w.Body.String())
	}

	// Invalid ids are validation errors, unknown ids are not found.
	w := request(h, "GET", "/api/v1/reviews/not-a-valid-id", "", adminToken)
	if w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("invalid review id: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/reviews/missing-0000000000001", "", adminToken); w.Code != 404 {
		t.Fatalf("missing review: %d %s", w.Code, w.Body.String())
	}

	// The admin may decide the private review; the public one is left pending for
	// the idempotency checks above to observe.
	if w = request(h, "POST", "/api/v1/reviews/"+privateReview.ID+"/approve", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("admin private approve: %d %s", w.Code, w.Body.String())
	}
}
