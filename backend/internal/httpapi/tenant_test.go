package httpapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

const httpOtherTenantID = "tenant_other_0000000000000001"
const httpTenantSecret = "tenant-http-test-secret-at-least-32-bytes"

// tenantHTTPServer builds the real engine over a migrated database whose
// default tenant exists, plus the database handle so a test can seed the second
// tenant that phase F1 deliberately has no API for.
func tenantHTTPServer(t *testing.T) (http.Handler, *auth.Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	ctx := context.Background()
	service := auth.New(db, httpTenantSecret, time.Minute, time.Hour)
	if err := service.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureDefaultTenant(ctx, tenants.DefaultSlug); err != nil {
		t.Fatal(err)
	}
	if err := service.Bootstrap(ctx, "admin", "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(service, logger, db.PingContext, []string{"http://localhost:5173"}, Options{Limiter: newTestLimiter()}), service, db
}

func insertHTTPTenant(t *testing.T, db *sql.DB, id, slug string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tenants(id,slug,name,status,created_at,updated_at) VALUES($1,$2,$2,'active',1,1)`, id, slug); err != nil {
		t.Fatal(err)
	}
}

func decodeTokenPayload(t *testing.T, accessToken string) map[string]any {
	t.Helper()
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed access token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// TestHTTPTenantMetadataOnAuthResponses proves the login, /auth/me and account
// listing/creation payloads all carry the same tenant object, and that a user's
// tenant metadata never leaks another tenant's identity.
func TestHTTPTenantMetadataOnAuthResponses(t *testing.T) {
	h, service, db := tenantHTTPServer(t)
	insertHTTPTenant(t, db, httpOtherTenantID, "other")
	ctx := context.Background()
	otherActor := auth.Actor{ID: "actor-other-00000000001", TenantID: httpOtherTenantID, Username: "other-admin", RequestID: "req-http-tenant"}
	if _, err := service.CreateUser(ctx, otherActor, "other-admin", "other-password-01", "", "admin"); err != nil {
		t.Fatal(err)
	}
	wantDefault := auth.TenantInfo{ID: tenants.DefaultID, Slug: tenants.DefaultSlug, Name: "Default"}
	wantOther := auth.TenantInfo{ID: httpOtherTenantID, Slug: "other", Name: "other"}
	assertTenant := func(label string, got *auth.TenantInfo, want auth.TenantInfo) {
		t.Helper()
		if got == nil || *got != want {
			t.Fatalf("%s tenant = %+v, want %+v", label, got, want)
		}
	}

	// Password login without a slug returns the default tenant metadata.
	w := request(h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"a-strong-password"}`, "")
	if w.Code != 200 {
		t.Fatalf("default login = %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"tenant":{`) {
		t.Fatalf("login payload carries no tenant object: %s", w.Body.String())
	}
	var loginResult struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &loginResult); err != nil {
		t.Fatal(err)
	}
	assertTenant("default login", loginResult.Data.User.Tenant, wantDefault)

	// /auth/me uses the same user shape.
	me := request(h, "GET", "/api/v1/auth/me", "", loginResult.Data.AccessToken)
	if me.Code != 200 {
		t.Fatalf("me = %d %s", me.Code, me.Body.String())
	}
	var meResult struct {
		Data auth.User `json:"data"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &meResult); err != nil {
		t.Fatal(err)
	}
	assertTenant("me", meResult.Data.Tenant, wantDefault)

	// A tenant-scoped login and its token carry that tenant's metadata.
	w = request(h, "POST", "/api/v1/auth/login", `{"username":"other-admin","password":"other-password-01","tenantSlug":"other"}`, "")
	if w.Code != 200 {
		t.Fatalf("other login = %d %s", w.Code, w.Body.String())
	}
	var otherLogin struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &otherLogin); err != nil {
		t.Fatal(err)
	}
	assertTenant("other login", otherLogin.Data.User.Tenant, wantOther)
	otherMe := request(h, "GET", "/api/v1/auth/me", "", otherLogin.Data.AccessToken)
	if otherMe.Code != 200 {
		t.Fatalf("other me = %d %s", otherMe.Code, otherMe.Body.String())
	}
	var otherMeResult struct {
		Data auth.User `json:"data"`
	}
	if err := json.Unmarshal(otherMe.Body.Bytes(), &otherMeResult); err != nil {
		t.Fatal(err)
	}
	assertTenant("other me", otherMeResult.Data.Tenant, wantOther)

	// Administrator account payloads stay inside the caller's tenant: the
	// listing and the creation response never carry the other tenant's object.
	created := request(h, "POST", "/api/v1/users", `{"username":"meta-member","password":"meta-password-01","role":"member"}`, loginResult.Data.AccessToken)
	if created.Code != 201 {
		t.Fatalf("create user = %d %s", created.Code, created.Body.String())
	}
	var createdResult struct {
		Data auth.User `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdResult); err != nil {
		t.Fatal(err)
	}
	assertTenant("created user", createdResult.Data.Tenant, wantDefault)

	listed := request(h, "GET", "/api/v1/users?limit=100&offset=0", "", loginResult.Data.AccessToken)
	if listed.Code != 200 {
		t.Fatalf("list users = %d %s", listed.Code, listed.Body.String())
	}
	var page struct {
		Data struct {
			Items []auth.User `json:"items"`
			Total int         `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Data.Total != 2 || len(page.Data.Items) != 2 {
		t.Fatalf("default user listing = %d/%d", page.Data.Total, len(page.Data.Items))
	}
	for _, user := range page.Data.Items {
		assertTenant("listed user", user.Tenant, wantDefault)
	}
}

func TestHTTPLoginTenantSlug(t *testing.T) {
	h, _, _ := tenantHTTPServer(t)
	loginAs := func(body string) *httptest.ResponseRecorder {
		return request(h, "POST", "/api/v1/auth/login", body, "")
	}

	// Omitted slug: single-tenant deployments keep working exactly as before.
	if w := loginAs(`{"username":"admin","password":"a-strong-password"}`); w.Code != 200 {
		t.Fatalf("login without slug = %d %s", w.Code, w.Body.String())
	}
	// Explicit slug resolves the same tenant and the token records it.
	w := loginAs(`{"username":"admin","password":"a-strong-password","tenantSlug":"default"}`)
	if w.Code != 200 {
		t.Fatalf("login with slug = %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if payload := decodeTokenPayload(t, result.Data.AccessToken); payload["tid"] != tenants.DefaultID {
		t.Fatalf("JWT tid = %v, want %q", payload["tid"], tenants.DefaultID)
	}

	// Unknown tenant and invalid slug are distinguishable: 401 vs 400.
	if w = loginAs(`{"username":"admin","password":"a-strong-password","tenantSlug":"missing"}`); w.Code != 401 || errorCode(t, w) != "unauthorized" {
		t.Fatalf("unknown tenant login = %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{
		`{"username":"admin","password":"a-strong-password","tenantSlug":""}`,
		`{"username":"admin","password":"a-strong-password","tenantSlug":"Bad"} `,
		`{"username":"admin","password":"a-strong-password","tenantSlug":"-acme"}`,
		`{"username":"admin","password":"a-strong-password","tenantSlug":"` + strings.Repeat("a", 33) + `"}`,
	} {
		if w = loginAs(body); w.Code != 400 || errorCode(t, w) != "validation_failed" {
			t.Fatalf("invalid slug %s = %d %s", body, w.Code, w.Body.String())
		}
	}
	// Strict JSON still rejects null tenantSlug and unknown fields.
	for _, body := range []string{
		`{"username":"admin","password":"a-strong-password","tenantSlug":null}`,
		`{"username":"admin","password":"a-strong-password","tenantSlug":"default","extra":true}`,
		`{"username":"admin","password":"a-strong-password","tenantSlug":123}`,
	} {
		if w = loginAs(body); w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Fatalf("strict JSON %s = %d %s", body, w.Code, w.Body.String())
		}
	}
}

func TestHTTPLoginTenantRequired(t *testing.T) {
	h, service, db := tenantHTTPServer(t)
	ctx := context.Background()
	insertHTTPTenant(t, db, httpOtherTenantID, "other")
	// The same username exists in both tenants; their passwords are independent.
	actor := auth.Actor{ID: "actor-other-00000000001", TenantID: httpOtherTenantID, Username: "other-admin", RequestID: "req-http-tenant"}
	if _, err := service.CreateUser(ctx, actor, "dual-user", "other-password-01", "", "member"); err != nil {
		t.Fatal(err)
	}
	defaultActor := auth.Actor{ID: "actor-default-000000001", TenantID: tenants.DefaultID, Username: "admin", RequestID: "req-http-tenant"}
	if _, err := service.CreateUser(ctx, defaultActor, "dual-user", "default-password-01", "", "member"); err != nil {
		t.Fatal(err)
	}

	// Without a slug the duplicate identifier cannot be resolved: 400 with the
	// documented tenant_required code, not an authentication failure.
	w := request(h, "POST", "/api/v1/auth/login", `{"username":"dual-user","password":"default-password-01"}`, "")
	if w.Code != 400 || errorCode(t, w) != "tenant_required" {
		t.Fatalf("ambiguous login = %d %s", w.Code, w.Body.String())
	}
	var ambiguous struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ambiguous); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ambiguous.Error.Message, "tenantSlug is required") {
		t.Fatalf("tenant_required message = %q", ambiguous.Error.Message)
	}
	// Retrying with either slug selects that tenant's account.
	for _, tc := range []struct{ slug, password string }{
		{"default", "default-password-01"},
		{"other", "other-password-01"},
	} {
		body := `{"username":"dual-user","password":"` + tc.password + `","tenantSlug":"` + tc.slug + `"}`
		w = request(h, "POST", "/api/v1/auth/login", body, "")
		if w.Code != 200 {
			t.Fatalf("login with slug %s = %d %s", tc.slug, w.Code, w.Body.String())
		}
	}
	// Wrong password with a valid slug stays the uniform 401.
	body := `{"username":"dual-user","password":"wrong-password-01","tenantSlug":"default"}`
	if w = request(h, "POST", "/api/v1/auth/login", body, ""); w.Code != 401 || errorCode(t, w) != "unauthorized" {
		t.Fatalf("wrong password with slug = %d %s", w.Code, w.Body.String())
	}
}

func TestHTTPCrossTenantUserManagementIs404(t *testing.T) {
	h, service, db := tenantHTTPServer(t)
	ctx := context.Background()
	insertHTTPTenant(t, db, httpOtherTenantID, "other")
	actor := auth.Actor{ID: "actor-other-00000000001", TenantID: httpOtherTenantID, Username: "other-admin", RequestID: "req-http-tenant"}
	otherUser, err := service.CreateUser(ctx, actor, "other-member", "other-password-01", "", "member")
	if err != nil {
		t.Fatal(err)
	}
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")

	// Cross-tenant targets are 404 for every administrator mutation.
	for _, route := range []struct{ method, path, body string }{
		{"PATCH", "/api/v1/users/" + otherUser.ID, `{"role":"viewer"}`},
		{"PATCH", "/api/v1/users/" + otherUser.ID, `{"disabled":true}`},
		{"POST", "/api/v1/users/" + otherUser.ID + "/password-reset", `{"password":"reset-password-0001"}`},
	} {
		if w := request(h, route.method, route.path, route.body, adminToken); w.Code != 404 || errorCode(t, w) != "not_found" {
			t.Fatalf("%s %s = %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	// The listing only contains the caller's tenant.
	w := request(h, "GET", "/api/v1/users?limit=100&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("list users = %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data struct {
			Items []auth.User `json:"items"`
			Total int         `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Data.Total != 1 || len(page.Data.Items) != 1 || page.Data.Items[0].Username != "admin" {
		t.Fatalf("cross-tenant user listing = %+v", page.Data)
	}
	for _, user := range page.Data.Items {
		if user.ID == otherUser.ID {
			t.Fatal("listing leaked a user from another tenant")
		}
	}
}

func TestHTTPAuditListIsTenantScoped(t *testing.T) {
	h, _, db := tenantHTTPServer(t)
	// The admin login writes exactly one default-tenant auth.login audit row.
	loginHTTP(t, h, "admin", "a-strong-password")
	insertHTTPTenant(t, db, httpOtherTenantID, "other")
	if _, err := db.Exec(`INSERT INTO auth_audit(id,tenant_id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
		VALUES('other-audit-00000001',$1,1,'other-actor','other-user','user.create','user','other-object','success','','req-other')`, httpOtherTenantID); err != nil {
		t.Fatal(err)
	}
	// A second default-tenant row so the page has two tenants' data to filter.
	if _, err := db.Exec(`INSERT INTO auth_audit(id,tenant_id,created_at,actor_id,actor_username,action,object_type,object_id,result,detail,request_id)
		VALUES('default-audit-000001',$1,2,'admin-id','admin','user.create','user','default-object','success','','req-default')`, tenants.DefaultID); err != nil {
		t.Fatal(err)
	}
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	w := request(h, "GET", "/api/v1/audit?limit=100&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("audit list = %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data struct {
			Items []auth.AuditEntry `json:"items"`
			Total int               `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var wantDefaultTotal int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE tenant_id=$1`, tenants.DefaultID).Scan(&wantDefaultTotal); err != nil {
		t.Fatal(err)
	}
	if wantDefaultTotal < 2 || page.Data.Total != wantDefaultTotal || len(page.Data.Items) != wantDefaultTotal {
		t.Fatalf("default tenant audit total = %d len = %d, want %d", page.Data.Total, len(page.Data.Items), wantDefaultTotal)
	}
	for _, entry := range page.Data.Items {
		if entry.ID == "other-audit-00000001" {
			t.Fatal("audit list leaked a row from another tenant")
		}
	}
	// The CSV export uses the same tenant scope.
	w = request(h, "GET", "/api/v1/audit/export.csv", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("audit export = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "other-audit") || strings.Contains(w.Body.String(), "other-object") {
		t.Fatalf("audit export leaked another tenant's rows: %s", w.Body.String())
	}
}

// TestHTTPAccessTokenWithoutTenantClaimRejected forges correctly signed JWTs
// to prove the middleware rejects a token whose tid is missing or does not match
// the session: only the signed, database-backed combination authenticates.
func TestHTTPAccessTokenWithoutTenantClaimRejected(t *testing.T) {
	h, _, db := tenantHTTPServer(t)
	insertHTTPTenant(t, db, httpOtherTenantID, "other")
	w := request(h, "POST", "/api/v1/auth/login", `{"username":"admin","password":"a-strong-password"}`, "")
	if w.Code != 200 {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	payload := decodeTokenPayload(t, result.Data.AccessToken)
	now := time.Now()
	baseClaims := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss": "aegisflow",
			"aud": "aegisflow-api",
			"sub": payload["sub"],
			"sid": payload["sid"],
			"iat": now.Unix(),
			"exp": now.Add(time.Minute).Unix(),
		}
	}
	sign := func(claims jwt.MapClaims) string {
		t.Helper()
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(httpTenantSecret))
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}
	// Sanity: the same claims plus the real tid authenticate.
	validClaims := baseClaims()
	validClaims["tid"] = tenants.DefaultID
	if got := request(h, "GET", "/api/v1/auth/me", "", sign(validClaims)); got.Code != 200 {
		t.Fatalf("forged-but-valid token rejected: %d %s", got.Code, got.Body.String())
	}
	// Missing tid is unauthenticated.
	if got := request(h, "GET", "/api/v1/auth/me", "", sign(baseClaims())); got.Code != 401 {
		t.Fatalf("token without tid accepted: %d %s", got.Code, got.Body.String())
	}
	// A syntactically valid but mismatched tenant is unauthenticated.
	wrongClaims := baseClaims()
	wrongClaims["tid"] = httpOtherTenantID
	if got := request(h, "GET", "/api/v1/auth/me", "", sign(wrongClaims)); got.Code != 401 {
		t.Fatalf("token with another tenant's tid accepted: %d %s", got.Code, got.Body.String())
	}
	// Admin routes are equally protected.
	if got := request(h, "GET", "/api/v1/users", "", sign(baseClaims())); got.Code != 401 {
		t.Fatalf("token without tid reached an admin route: %d", got.Code)
	}
}
