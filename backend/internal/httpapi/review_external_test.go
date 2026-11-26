package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/reviews"
)

const reviewWebhookTestSecret = "0123456789abcdef"

type externalResultEnvelope struct {
	Data reviews.ExternalResult `json:"data"`
}

func parseExternalResultHTTP(t *testing.T, body []byte) reviews.ExternalResult {
	t.Helper()
	var envelope externalResultEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("external result payload: %s", string(body))
	}
	return envelope.Data
}

type externalResultListEnvelope struct {
	Data struct {
		Items []reviews.ExternalResult `json:"items"`
	} `json:"data"`
}

func parseExternalResultListHTTP(t *testing.T, body []byte) []reviews.ExternalResult {
	t.Helper()
	var envelope externalResultListEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("external result list payload: %s", string(body))
	}
	return envelope.Data.Items
}

// serveSignedExternalResult posts body to path with a valid timestamp, delivery
// and HMAC signature. mutate runs after signing, so a test can remove or corrupt
// one header while the others stay valid. A nil secret sends no signature
// headers at all.
func serveSignedExternalResult(t *testing.T, h http.Handler, path string, body []byte, secret []byte, timestamp int64, delivery string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if secret != nil {
		stamp := strconv.FormatInt(timestamp, 10)
		request.Header.Set("X-AegisFlow-Timestamp", stamp)
		request.Header.Set("X-AegisFlow-Delivery", delivery)
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(stamp))
		_, _ = mac.Write([]byte("."))
		_, _ = mac.Write(body)
		request.Header.Set("X-AegisFlow-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	if mutate != nil {
		mutate(request)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func TestReviewExternalResultHappyPathAndIdempotency(t *testing.T) {
	h, _, db := reviewTestServerWithOptions(t, Options{ReviewWebhookSecret: []byte(reviewWebhookTestSecret)})
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "external-happy", "public")
	review := createReviewHTTP(t, h, adminToken, projectID, "external result target")
	path := "/api/v1/reviews/" + review.ID + "/external-results"

	body := `{"source":"scanner","verdict":"failed","summary":"2 high findings","url":"https://example.com/scan/9"}`
	w := serveSignedExternalResult(t, h, path, []byte(body), []byte(reviewWebhookTestSecret), time.Now().Unix(), "delivery-happy-0001", nil)
	if w.Code != 201 {
		t.Fatalf("ingest: %d %s", w.Code, w.Body.String())
	}
	result := parseExternalResultHTTP(t, w.Body.Bytes())
	if result.ID == "" || result.ReviewID != review.ID || result.DeliveryID != "delivery-happy-0001" ||
		result.Source != "scanner" || result.Verdict != "failed" || result.Summary != "2 high findings" ||
		result.URL != "https://example.com/scan/9" || result.ReceivedAt == 0 {
		t.Fatalf("result = %+v", result)
	}

	// A retried delivery is idempotent: 200 and the original row, no duplicate.
	retryBody := `{"source":"scanner","verdict":"passed","summary":"a later attempt","url":""}`
	w = serveSignedExternalResult(t, h, path, []byte(retryBody), []byte(reviewWebhookTestSecret), time.Now().Unix(), "delivery-happy-0001", nil)
	if w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if retried := parseExternalResultHTTP(t, w.Body.Bytes()); retried.ID != result.ID || retried.Verdict != "failed" {
		t.Fatalf("retried result = %+v", retried)
	}
	var stored int
	if err := db.QueryRow(`SELECT COUNT(*) FROM review_external_results WHERE review_id=$1`, review.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored rows = %d, want 1", stored)
	}

	// The read route is a normal Bearer route: visible to the project's readers,
	// newest first, and it does not include the raw signature headers.
	w = request(h, "GET", path, "", adminToken)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	items := parseExternalResultListHTTP(t, w.Body.Bytes())
	if len(items) != 1 || items[0].ID != result.ID {
		t.Fatalf("list items = %+v", items)
	}
	if strings.Contains(w.Body.String(), "signature") || strings.Contains(w.Body.String(), reviewWebhookTestSecret) {
		t.Fatalf("list leaked signature material: %s", w.Body.String())
	}

	// External results are evidence only: the review stays pending.
	if w = request(h, "GET", "/api/v1/reviews/"+review.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("get review: %d %s", w.Code, w.Body.String())
	} else if got := parseReviewHTTP(t, w.Body.Bytes()); got.Status != "pending" {
		t.Fatalf("review status = %q after external result", got.Status)
	}
}

func TestReviewExternalResultNotConfigured(t *testing.T) {
	h, _, _ := reviewTestServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "external-disabled", "public")
	review := createReviewHTTP(t, h, adminToken, projectID, "unconfigured external result")
	w := serveSignedExternalResult(t, h, "/api/v1/reviews/"+review.ID+"/external-results",
		[]byte(`{"source":"scanner","verdict":"passed"}`), nil, time.Now().Unix(), "", nil)
	if w.Code != 503 || errorCode(t, w) != "dependencies_unavailable" {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "review results are not configured") {
		t.Fatalf("unconfigured message = %s", w.Body.String())
	}
}

func TestReviewExternalResultAuthenticationFailures(t *testing.T) {
	h, _, _ := reviewTestServerWithOptions(t, Options{ReviewWebhookSecret: []byte(reviewWebhookTestSecret)})
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "external-auth", "public")
	review := createReviewHTTP(t, h, adminToken, projectID, "signed external result")
	path := "/api/v1/reviews/" + review.ID + "/external-results"
	body := []byte(`{"source":"scanner","verdict":"passed"}`)
	fixed := time.Now().Unix()
	wrongSecret := []byte("fedcba9876543210")

	for _, tc := range []struct {
		name      string
		secret    []byte
		timestamp int64
		delivery  string
		mutate    func(*http.Request)
	}{
		{"missing all headers", nil, 0, "", nil},
		{"missing timestamp", []byte(reviewWebhookTestSecret), fixed, "delivery-auth-0001", func(r *http.Request) { r.Header.Del("X-AegisFlow-Timestamp") }},
		{"missing signature", []byte(reviewWebhookTestSecret), fixed, "delivery-auth-0002", func(r *http.Request) { r.Header.Del("X-AegisFlow-Signature") }},
		{"missing delivery", []byte(reviewWebhookTestSecret), fixed, "delivery-auth-0003", func(r *http.Request) { r.Header.Del("X-AegisFlow-Delivery") }},
		{"wrong secret", wrongSecret, fixed, "delivery-auth-0004", nil},
		{"malformed signature", []byte(reviewWebhookTestSecret), fixed, "delivery-auth-0005", func(r *http.Request) {
			r.Header.Set("X-AegisFlow-Signature", "sha256=not-hex")
		}},
		{"signature without prefix", []byte(reviewWebhookTestSecret), fixed, "delivery-auth-0006", func(r *http.Request) {
			r.Header.Set("X-AegisFlow-Signature", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
		}},
		{"unparseable timestamp", []byte(reviewWebhookTestSecret), fixed, "delivery-auth-0007", func(r *http.Request) {
			r.Header.Set("X-AegisFlow-Timestamp", "not-a-number")
		}},
		{"stale timestamp", []byte(reviewWebhookTestSecret), fixed - 301, "delivery-auth-0008", nil},
		{"future timestamp", []byte(reviewWebhookTestSecret), fixed + 301, "delivery-auth-0009", nil},
		{"short delivery", []byte(reviewWebhookTestSecret), fixed, "short", nil},
		{"delivery with space", []byte(reviewWebhookTestSecret), fixed, "delivery auth 01", nil},
		{"delivery with slash", []byte(reviewWebhookTestSecret), fixed, "delivery/auth/01", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveSignedExternalResult(t, h, path, body, tc.secret, tc.timestamp, tc.delivery, tc.mutate)
			if w.Code != 401 || errorCode(t, w) != "unauthorized" {
				t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
			}
		})
	}

	// The timestamp window is inclusive: 300s in the past is still accepted.
	w := serveSignedExternalResult(t, h, path, body, []byte(reviewWebhookTestSecret), time.Now().Unix()-300, "delivery-auth-ok-1", nil)
	if w.Code != 201 {
		t.Fatalf("boundary timestamp: %d %s", w.Code, w.Body.String())
	}

	// Content type is enforced before the signature; a wrong type is 415.
	w = serveSignedExternalResult(t, h, path, body, []byte(reviewWebhookTestSecret), fixed, "delivery-auth-0010", func(r *http.Request) {
		r.Header.Set("Content-Type", "text/plain")
	})
	if w.Code != 415 || errorCode(t, w) != "unsupported_media_type" {
		t.Fatalf("wrong content type: %d %s", w.Code, w.Body.String())
	}

	// A correctly signed delivery for an unknown review is 404; a malformed id
	// is a validation error.
	w = serveSignedExternalResult(t, h, "/api/v1/reviews/missing-0000000000001/external-results", body,
		[]byte(reviewWebhookTestSecret), fixed, "delivery-auth-0011", nil)
	if w.Code != 404 {
		t.Fatalf("unknown review: %d %s", w.Code, w.Body.String())
	}
	w = serveSignedExternalResult(t, h, "/api/v1/reviews/not-a-valid-id/external-results", body,
		[]byte(reviewWebhookTestSecret), fixed, "delivery-auth-0012", nil)
	if w.Code != 400 || errorCode(t, w) != "validation_failed" {
		t.Fatalf("invalid review id: %d %s", w.Code, w.Body.String())
	}
}

func TestReviewExternalResultBodyValidation(t *testing.T) {
	h, _, _ := reviewTestServerWithOptions(t, Options{ReviewWebhookSecret: []byte(reviewWebhookTestSecret)})
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createProjectHTTP(t, h, adminToken, "external-bodies", "public")
	review := createReviewHTTP(t, h, adminToken, projectID, "external body validation")
	path := "/api/v1/reviews/" + review.ID + "/external-results"
	delivery := 0
	for _, tc := range []struct {
		name string
		body string
	}{
		{"unknown field", `{"source":"scanner","verdict":"passed","extra":1}`},
		{"null field", `{"source":null,"verdict":"passed"}`},
		{"array", `[]`},
		{"null body", `null`},
		{"two documents", `{"source":"scanner","verdict":"passed"} {}`},
		{"empty source", `{"source":"","verdict":"passed"}`},
		{"source too long", `{"source":"` + strings.Repeat("s", 65) + `","verdict":"passed"}`},
		{"source multiline", `{"source":"scanner\nline","verdict":"passed"}`},
		{"verdict unknown", `{"source":"scanner","verdict":"unknown"}`},
		{"verdict uppercase", `{"source":"scanner","verdict":"PASSED"}`},
		{"summary too long", `{"source":"scanner","verdict":"passed","summary":"` + strings.Repeat("x", 501) + `"}`},
		{"url not http", `{"source":"scanner","verdict":"passed","url":"ftp://example.com/x"}`},
		{"url userinfo", `{"source":"scanner","verdict":"passed","url":"https://user:pass@example.com/x"}`},
		{"url fragment", `{"source":"scanner","verdict":"passed","url":"https://example.com/x#frag"}`},
		{"malformed json", `{"source":`},
		{"oversized body", `{"source":"scanner","verdict":"passed","summary":"` + strings.Repeat("x", 17<<10) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivery++
			w := serveSignedExternalResult(t, h, path, []byte(tc.body), []byte(reviewWebhookTestSecret),
				time.Now().Unix(), "delivery-body-"+strconv.Itoa(delivery), nil)
			if w.Code != 400 {
				t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
			}
		})
	}

	// Summary control characters are removed while newlines are preserved; the
	// minimal valid body only needs source and verdict.
	valid := `{"source":"scanner","verdict":"warning","summary":"line one\r\nline\u0000two\n\n"}`
	w := serveSignedExternalResult(t, h, path, []byte(valid), []byte(reviewWebhookTestSecret),
		time.Now().Unix(), "delivery-body-ok-1", nil)
	if w.Code != 201 {
		t.Fatalf("valid body: %d %s", w.Code, w.Body.String())
	}
	if result := parseExternalResultHTTP(t, w.Body.Bytes()); result.Summary != "line one\nlinetwo" || result.URL != "" {
		t.Fatalf("sanitized result = %+v", result)
	}
}

func TestReviewExternalResultReadVisibility(t *testing.T) {
	h, _, _ := reviewTestServerWithOptions(t, Options{ReviewWebhookSecret: []byte(reviewWebhookTestSecret)})
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "external-outsider", "member-password-01", "member")
	outsiderToken, _, _ := loginHTTP(t, h, "external-outsider", "member-password-01")
	publicID := createProjectHTTP(t, h, adminToken, "external-read-public", "public")
	privateID := createProjectHTTP(t, h, adminToken, "external-read-private", "private")
	publicReview := createReviewHTTP(t, h, adminToken, publicID, "public external review")
	privateReview := createReviewHTTP(t, h, adminToken, privateID, "private external review")

	// HMAC ingestion has no project visibility concept: the trusted signer may
	// attach evidence to any existing review id.
	w := serveSignedExternalResult(t, h, "/api/v1/reviews/"+privateReview.ID+"/external-results",
		[]byte(`{"source":"scanner","verdict":"passed"}`), []byte(reviewWebhookTestSecret),
		time.Now().Unix(), "delivery-read-0001", nil)
	if w.Code != 201 {
		t.Fatalf("private ingest: %d %s", w.Code, w.Body.String())
	}

	// Reading results follows review visibility: invisible project is 404.
	for _, path := range []string{
		"/api/v1/reviews/" + privateReview.ID + "/external-results",
		"/api/v1/reviews/missing-0000000000001/external-results",
	} {
		if w = request(h, "GET", path, "", outsiderToken); w.Code != 404 || errorCode(t, w) != "not_found" {
			t.Fatalf("outsider GET %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w = request(h, "GET", "/api/v1/reviews/"+privateReview.ID+"/external-results", "", adminToken); w.Code != 200 {
		t.Fatalf("admin private list: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/reviews/"+publicReview.ID+"/external-results", "", outsiderToken); w.Code != 200 {
		t.Fatalf("outsider public list: %d %s", w.Code, w.Body.String())
	} else if items := parseExternalResultListHTTP(t, w.Body.Bytes()); len(items) != 0 {
		t.Fatalf("public review list = %+v", items)
	}
	if w = request(h, "GET", "/api/v1/reviews/"+publicReview.ID+"/external-results", "", ""); w.Code != 401 {
		t.Fatalf("unauthenticated list: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/reviews/not-a-valid-id/external-results", "", adminToken); w.Code != 400 {
		t.Fatalf("invalid id list: %d %s", w.Code, w.Body.String())
	}
}
