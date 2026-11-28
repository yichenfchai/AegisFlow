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
	"sync"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/builds"
	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
	"github.com/yichenfchai/AegisFlow/backend/internal/knowledge"
	"github.com/yichenfchai/AegisFlow/backend/internal/policies"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/reviews"
	"github.com/yichenfchai/AegisFlow/backend/internal/scans"
	"github.com/yichenfchai/AegisFlow/backend/internal/secrets"
	"github.com/yichenfchai/AegisFlow/backend/internal/securityevents"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// isolationEnv is one full server wiring over a real migrated database holding
// two tenants. It is the phase-F2 isolation matrix: the same resource names,
// delivery ids and endpoints exist in both tenants, so every read below proves
// the tenant predicate, not a lucky unique constraint.
type isolationEnv struct {
	t          *testing.T
	h          http.Handler
	db         *sql.DB
	auth       *auth.Service
	builds     *builds.Service
	deploy     *deployments.Service
	review     *reviews.Service
	webhook    *webhookRecorder
	webhookURL string
	tenantA    tenants.Tenant
	tenantB    tenants.Tenant
	tokenA     string
	tokenB     string
	userAID    string
	userBID    string
	features   map[string]*tenantResources
}

// tenantResources holds the ids one tenant created through the HTTP API.
type tenantResources struct {
	ProjectID          string
	DevTargetID        string
	ProdTargetID       string
	BuildID            string
	DeploymentID       string
	AwaitingDeployID   string
	ApprovalID         string
	ReviewID           string
	ScanID             string
	ScanDelivery       string
	ExternalResultID   string
	SecurityEventID    string
	SecurityDelivery   string
	KnowledgeID        string
	SecretID           string
	ProjectName        string
	ReviewIDApprovedID string
}

const isolationPassword = "a-strong-password"
const isolationOtherPassword = "b-strong-password"
const isolationReviewDelivery = "review-delivery-shared-0001"

// webhookRecorder captures outbound approval/review notifications so the test can
// prove the payloads carry the owning tenant and never mix tenants.
type webhookRecorder struct {
	mu     sync.Mutex
	events []recordedWebhook
}

type recordedWebhook struct {
	Event   string
	Payload map[string]any
}

func (w *webhookRecorder) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	w.mu.Lock()
	w.events = append(w.events, recordedWebhook{Event: r.Header.Get("X-AegisFlow-Event"), Payload: payload})
	w.mu.Unlock()
	rw.WriteHeader(http.StatusNoContent)
}

func (w *webhookRecorder) snapshot() []recordedWebhook {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]recordedWebhook(nil), w.events...)
}

func newIsolationEnv(t *testing.T) *isolationEnv {
	t.Helper()
	db := testutil.OpenPostgres(t)
	ctx := context.Background()
	authService := auth.New(db, strings.Repeat("k", 32), time.Minute, time.Hour)
	if err := authService.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	defaultTenant, err := authService.EnsureDefaultTenant(ctx, tenants.DefaultSlug)
	if err != nil {
		t.Fatal(err)
	}
	if err := authService.Bootstrap(ctx, "admin-a", isolationPassword); err != nil {
		t.Fatal(err)
	}
	otherTenant, err := tenants.Create(ctx, db, "other", "Other Org")
	if err != nil {
		t.Fatal(err)
	}
	otherAdmin, err := authService.CreateUser(ctx,
		auth.Actor{TenantID: otherTenant.ID, Username: "bootstrap-actor", Role: "admin", RequestID: "req-isolation"},
		"admin-b", isolationOtherPassword, "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	// A member of tenant B exists so cross-tenant user management can be probed.
	if _, err := authService.CreateUser(ctx,
		auth.Actor{TenantID: otherTenant.ID, Username: "bootstrap-actor", Role: "admin", RequestID: "req-isolation"},
		"member-b", isolationOtherPassword, "", "member"); err != nil {
		t.Fatal(err)
	}

	recorder := &webhookRecorder{}
	webhookServer := httptest.NewServer(recorder)
	t.Cleanup(webhookServer.Close)
	registry := projects.New(db)
	policyStore := policies.New(db, registry)
	reviewCenter := reviews.New(db, registry, reviews.Options{WebhookURL: webhookServer.URL})
	scanRegistry := scans.New(db)
	defenseEvents := securityevents.New(db)
	knowledgeBase := knowledge.New(db)
	secretStore := secrets.New(db, secrets.Options{SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	buildPipeline := builds.New(db, registry)
	deployService := deployments.New(db, deployments.Options{
		SecretKey:              []byte("0123456789abcdef0123456789abcdef"),
		Projects:               registry,
		PolicyGate:             policyStore,
		ApprovalEnvironments:   []string{"production"},
		ApprovalWebhookURL:     webhookServer.URL,
		ApprovalWebhookTimeout: 5 * time.Second,
	})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler := New(authService, logger, db.PingContext, []string{"http://localhost:5173"}, Options{
		Limiter:             newTestLimiter(),
		Projects:            registry,
		Builds:              buildPipeline,
		Deployments:         deployService,
		Reviews:             reviewCenter,
		Scans:               scanRegistry,
		SecurityEvents:      defenseEvents,
		Knowledge:           knowledgeBase,
		Secrets:             secretStore,
		Policies:            policyStore,
		ReviewWebhookSecret: []byte("0123456789abcdef"),
		IngestWebhookSecret: []byte("0123456789abcdef"),
		DefaultTenantID:     defaultTenant.ID,
	})
	env := &isolationEnv{
		t: t, h: handler, db: db, auth: authService, builds: buildPipeline, deploy: deployService, review: reviewCenter,
		webhook: recorder, webhookURL: webhookServer.URL,
		tenantA: *defaultTenant, tenantB: *otherTenant,
		userBID:  otherAdmin.ID,
		features: map[string]*tenantResources{},
	}
	if err := db.QueryRowContext(ctx, `SELECT id FROM auth_users WHERE tenant_id=$1 AND lower(username)='admin-a'`, defaultTenant.ID).Scan(&env.userAID); err != nil {
		t.Fatal(err)
	}
	env.tokenA = env.login("admin-a", isolationPassword, "default")
	env.tokenB = env.login("admin-b", isolationOtherPassword, "other")
	return env
}

// subtest returns a shallow copy of the environment that reports failures on the
// subtest instead of the parent test.
func (e *isolationEnv) subtest(t *testing.T) *isolationEnv {
	copied := *e
	copied.t = t
	return &copied
}

// login performs a password login and returns the access token.
func (e *isolationEnv) login(username, password, slug string) string {
	e.t.Helper()
	body := `{"username":"` + username + `","password":"` + password + ``
	if slug != "" {
		body += `","tenantSlug":"` + slug + `"`
	}
	body += `}`
	w := request(e.h, http.MethodPost, "/api/v1/auth/login", body, "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("login %s@%s = %d %s", username, slug, w.Code, w.Body.String())
	}
	return accessToken(e.t, w.Body.Bytes())
}

func accessToken(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data.AccessToken
}

// do performs one authenticated request and returns the recorder.
func (e *isolationEnv) do(token, method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return request(e.h, method, path, body, token)
}

// expect asserts the response status and returns the decoded data object.
func (e *isolationEnv) expect(token, method, path, body string, want int) map[string]any {
	e.t.Helper()
	w := e.do(token, method, path, body)
	if w.Code != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	if want == http.StatusNoContent || len(w.Body.Bytes()) == 0 {
		return nil
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		e.t.Fatalf("%s %s data: %v (%s)", method, path, err, w.Body.String())
	}
	return envelope.Data
}

func (e *isolationEnv) expect404(token, method, path, body string) {
	e.t.Helper()
	w := e.do(token, method, path, body)
	if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
		e.t.Fatalf("%s %s = %d, want 404/403 (no cross-tenant access): %s", method, path, w.Code, w.Body.String())
	}
	if w.Code == http.StatusForbidden {
		// Hidden resources must be reported as not found; a 403 would confirm the
		// resource exists in another tenant. Only the caller's own-tenant
		// role checks may answer 403.
		e.t.Fatalf("%s %s = 403, want 404 for a cross-tenant resource: %s", method, path, w.Body.String())
	}
}

// otherUserID returns the user id of the tenant the token does not belong to.
func otherUserID(env *isolationEnv, token string) string {
	if token == env.tokenA {
		return env.userBID
	}
	return env.userAID
}

func (e *isolationEnv) listItems(token, path string) ([]map[string]any, int) {
	e.t.Helper()
	w := e.do(token, http.MethodGet, path, "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
	}
	var envelope struct {
		Data struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		e.t.Fatal(err)
	}
	return envelope.Data.Items, envelope.Data.Total
}

// createData creates the same resources in tenant A and tenant B. Identical names
// and delivery ids are used on purpose: tenant-scoped uniqueness is part of the
// contract under test.
func (e *isolationEnv) createData() {
	e.t.Helper()
	for _, tenant := range []struct {
		key   string
		token string
		slug  string
	}{
		{"a", e.tokenA, "default"},
		{"b", e.tokenB, "other"},
	} {
		res := &tenantResources{ProjectName: "shared-project", ScanDelivery: "delivery-shared-0001", SecurityDelivery: "security-delivery-shared-0001"}
		project := e.expect(tenant.token, http.MethodPost, "/api/v1/projects", `{"name":"shared-project","buildCommand":"echo isolated-build","visibility":"private"}`, http.StatusCreated)
		res.ProjectID = project["id"].(string)
		// private visibility keeps the member overlay exercised too: the admin sees
		// its own tenant's private project, the other tenant's admin must not.
		e.expect(tenant.token, http.MethodPatch, "/api/v1/projects/"+res.ProjectID, `{"description":"tenant-owned"}`, http.StatusOK)

		devTarget := e.expect(tenant.token, http.MethodPost, "/api/v1/deployment-targets",
			`{"name":"shared-target","endpoint":"https://deploy.example.test","environment":"development"}`, http.StatusCreated)
		res.DevTargetID = devTarget["id"].(string)
		prodTarget := e.expect(tenant.token, http.MethodPost, "/api/v1/deployment-targets",
			`{"name":"shared-prod-target","endpoint":"https://prod.example.test","environment":"production"}`, http.StatusCreated)
		res.ProdTargetID = prodTarget["id"].(string)

		build := e.expect(tenant.token, http.MethodPost, "/api/v1/projects/"+res.ProjectID+"/builds", `{"branch":"main"}`, http.StatusCreated)
		res.BuildID = build["id"].(string)

		devDeployment := e.expect(tenant.token, http.MethodPost, "/api/v1/projects/"+res.ProjectID+"/deployments",
			`{"targetId":"`+res.DevTargetID+`","version":"v1"}`, http.StatusCreated)
		res.DeploymentID = devDeployment["id"].(string)
		prodDeployment := e.expect(tenant.token, http.MethodPost, "/api/v1/projects/"+res.ProjectID+"/deployments",
			`{"targetId":"`+res.ProdTargetID+`","version":"v2"}`, http.StatusCreated)
		res.AwaitingDeployID = prodDeployment["id"].(string)
		res.ApprovalID = prodDeployment["approvalId"].(string)
		if res.ApprovalID == "" {
			e.t.Fatal("production deployment did not await an approval")
		}

		review := e.expect(tenant.token, http.MethodPost, "/api/v1/projects/"+res.ProjectID+"/reviews",
			`{"subject":"shared change"}`, http.StatusCreated)
		res.ReviewID = review["id"].(string)

		// HMAC inbound evidence: scan (project id resolves the tenant), review
		// external result (review id resolves the tenant) and defense event (the
		// signed body names the tenant; absent means the default tenant).
		scanBody := []byte(`{"source":"scanner","tool":"trivy","verdict":"passed","summary":"isolated scan"}`)
		w := serveSignedExternalResult(e.t, e.h, "/api/v1/projects/"+res.ProjectID+"/scans", scanBody, []byte("0123456789abcdef"), time.Now().Unix(), res.ScanDelivery, nil)
		if w.Code != http.StatusCreated {
			e.t.Fatalf("scan ingest = %d %s", w.Code, w.Body.String())
		}
		res.ScanID = parseScanHTTP(e.t, w.Body.Bytes()).ID

		externalBody := []byte(`{"source":"ci","verdict":"passed","summary":"isolated evidence"}`)
		w = serveSignedExternalResult(e.t, e.h, "/api/v1/reviews/"+res.ReviewID+"/external-results", externalBody, []byte("0123456789abcdef"), time.Now().Unix(), isolationReviewDelivery, nil)
		if w.Code != http.StatusCreated {
			e.t.Fatalf("review external result = %d %s", w.Code, w.Body.String())
		}
		res.ExternalResultID = parseExternalResultHTTP(e.t, w.Body.Bytes()).ID

		eventBody := `{"source":"ids","severity":"high","eventType":"intrusion","summary":"isolated defense event"`
		if tenant.key == "b" {
			eventBody += `,"tenantSlug":"other"`
		}
		eventBody += `}`
		w = serveSignedExternalResult(e.t, e.h, "/api/v1/security-events", []byte(eventBody), []byte("0123456789abcdef"), time.Now().Unix(), res.SecurityDelivery, nil)
		if w.Code != http.StatusCreated {
			e.t.Fatalf("security event ingest = %d %s", w.Code, w.Body.String())
		}
		res.SecurityEventID = parseEventID(e.t, w.Body.Bytes())

		knowledgeDoc := e.expect(tenant.token, http.MethodPost, "/api/v1/knowledge", `{"title":"shared runbook","body":"secret-free body","tags":"ops"}`, http.StatusCreated)
		res.KnowledgeID = knowledgeDoc["id"].(string)
		secretMeta := e.expect(tenant.token, http.MethodPost, "/api/v1/secrets", `{"name":"shared-secret","value":"shared-value-01"}`, http.StatusCreated)
		res.SecretID = secretMeta["id"].(string)
		e.expect(tenant.token, http.MethodPut, "/api/v1/projects/"+res.ProjectID+"/policy",
			`{"requireReview":false,"requiredScanVerdict":"none"}`, http.StatusOK)
		e.features[tenant.key] = res
	}
}

func parseEventID(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Data securityevents.Event `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data.ID
}

// TestTenantIsolationEndToEnd is the cross-tenant 404/list/audit/uniqueness
// matrix. Every resource exists in both tenants; every read is made with the
// other tenant's administrator token and must be a 404.
func TestTenantIsolationEndToEnd(t *testing.T) {
	env := newIsolationEnv(t)
	env.createData()
	a, b := env.features["a"], env.features["b"]

	t.Run("cross tenant resource reads are 404", func(t *testing.T) {
		env := env.subtest(t)
		// Tenant A's admin against tenant B's resources and vice versa.
		for _, tc := range []struct{ owner, intruder string }{
			{env.tokenA, env.tokenB},
			{env.tokenB, env.tokenA},
		} {
			other := a
			ownerUserID := env.userAID
			if tc.owner == env.tokenB {
				other = b
				ownerUserID = env.userBID
			}
			for _, req := range []struct{ method, path, body string }{
				{http.MethodGet, "/api/v1/projects/" + other.ProjectID, ""},
				{http.MethodPatch, "/api/v1/projects/" + other.ProjectID, `{"description":"hijack"}`},
				{http.MethodGet, "/api/v1/projects/" + other.ProjectID + "/members", ""},
				{http.MethodPost, "/api/v1/projects/" + other.ProjectID + "/members", `{"userId":"` + env.userBID + `","role":"viewer"}`},
				{http.MethodPost, "/api/v1/projects/" + other.ProjectID + "/builds", `{"branch":"main"}`},
				{http.MethodGet, "/api/v1/projects/" + other.ProjectID + "/deployments", ""},
				{http.MethodPost, "/api/v1/projects/" + other.ProjectID + "/deployments", `{"targetId":"` + other.DevTargetID + `","version":"v9"}`},
				{http.MethodGet, "/api/v1/projects/" + other.ProjectID + "/reviews", ""},
				{http.MethodPost, "/api/v1/projects/" + other.ProjectID + "/reviews", `{"subject":"intrusion"}`},
				{http.MethodGet, "/api/v1/projects/" + other.ProjectID + "/policy", ""},
				{http.MethodPut, "/api/v1/projects/" + other.ProjectID + "/policy", `{"requireReview":true,"requiredScanVerdict":"passed"}`},
				{http.MethodGet, "/api/v1/projects/" + other.ProjectID + "/scans", ""},
				{http.MethodGet, "/api/v1/builds/" + other.BuildID, ""},
				{http.MethodGet, "/api/v1/builds/" + other.BuildID + "/logs", ""},
				{http.MethodGet, "/api/v1/builds/" + other.BuildID + "/events", ""},
				{http.MethodPost, "/api/v1/builds/" + other.BuildID + "/cancel", `{}`},
				{http.MethodPost, "/api/v1/builds/" + other.BuildID + "/retry", `{}`},
				{http.MethodGet, "/api/v1/deployment-targets/" + other.DevTargetID, ""},
				{http.MethodPatch, "/api/v1/deployment-targets/" + other.DevTargetID, `{"status":"disabled"}`},
				{http.MethodDelete, "/api/v1/deployment-targets/" + other.DevTargetID, ""},
				{http.MethodPost, "/api/v1/deployment-targets/" + other.DevTargetID + "/check", `{}`},
				{http.MethodGet, "/api/v1/deployments/" + other.DeploymentID, ""},
				{http.MethodGet, "/api/v1/deployments/" + other.DeploymentID + "/events", ""},
				{http.MethodPost, "/api/v1/deployments/" + other.DeploymentID + "/cancel", `{}`},
				{http.MethodPost, "/api/v1/deployments/" + other.DeploymentID + "/rollback", `{}`},
				{http.MethodGet, "/api/v1/reviews/" + other.ReviewID, ""},
				{http.MethodPost, "/api/v1/reviews/" + other.ReviewID + "/approve", `{}`},
				{http.MethodPost, "/api/v1/reviews/" + other.ReviewID + "/reject", `{"reason":"intrusion"}`},
				{http.MethodPost, "/api/v1/reviews/" + other.ReviewID + "/cancel", `{}`},
				{http.MethodGet, "/api/v1/reviews/" + other.ReviewID + "/external-results", ""},
				{http.MethodGet, "/api/v1/scans/" + other.ScanID, ""},
				{http.MethodGet, "/api/v1/security-events/" + other.SecurityEventID, ""},
				{http.MethodPost, "/api/v1/security-events/" + other.SecurityEventID + "/ack", `{}`},
				{http.MethodGet, "/api/v1/knowledge/" + other.KnowledgeID, ""},
				{http.MethodPut, "/api/v1/knowledge/" + other.KnowledgeID, `{"title":"hijack","body":"x"}`},
				{http.MethodDelete, "/api/v1/knowledge/" + other.KnowledgeID, ""},
				{http.MethodPut, "/api/v1/secrets/" + other.SecretID, `{"value":"hijacked-value-01"}`},
				{http.MethodDelete, "/api/v1/secrets/" + other.SecretID, ""},
				{http.MethodPost, "/api/v1/approvals/" + other.ApprovalID + "/approve", `{}`},
				{http.MethodPost, "/api/v1/approvals/" + other.ApprovalID + "/reject", `{"reason":"intrusion"}`},
				{http.MethodPatch, "/api/v1/users/" + ownerUserID, `{"role":"viewer"}`},
			} {
				env.expect404(tc.intruder, req.method, req.path, req.body)
			}
		}
	})

	t.Run("lists and filters never include the other tenant", func(t *testing.T) {
		env := env.subtest(t)
		for _, token := range []string{env.tokenA, env.tokenB} {
			other := b
			if token == env.tokenB {
				other = a
			}
			foreign := map[string]bool{}
			foreignUser := otherUserID(env, token)
			for _, id := range []string{other.ProjectID, other.BuildID, other.DeploymentID, other.AwaitingDeployID,
				other.ReviewID, other.ScanID, other.DevTargetID, other.ProdTargetID, other.KnowledgeID,
				other.SecretID, other.ApprovalID, other.SecurityEventID, foreignUser} {
				foreign[id] = true
			}
			for _, list := range []struct {
				path string
				want int
			}{
				{"/api/v1/projects?limit=100", 1},
				{"/api/v1/builds?limit=100", 1},
				{"/api/v1/deployments?limit=100", 2},
				{"/api/v1/reviews?limit=100", 1},
				{"/api/v1/scans?limit=100", 1},
				{"/api/v1/security-events?limit=100", 1},
				{"/api/v1/knowledge?limit=100", 1},
				{"/api/v1/secrets?limit=100", 1},
				{"/api/v1/deployment-targets?limit=100", 2},
				{"/api/v1/approvals?limit=100", 1},
			} {
				items, total := env.listItems(token, list.path)
				if total != list.want || len(items) != list.want {
					t.Fatalf("GET %s = %d/%d items, want %d", list.path, len(items), total, list.want)
				}
				for _, item := range items {
					for _, value := range item {
						if id, ok := value.(string); ok && foreign[id] {
							t.Fatalf("GET %s leaked cross-tenant id %q", list.path, id)
						}
					}
				}
			}
			// A filter naming the other tenant's project is empty, not an error.
			if items, total := env.listItems(token, "/api/v1/builds?projectId="+other.ProjectID); total != 0 || len(items) != 0 {
				t.Fatalf("cross-tenant build filter leaked %d rows", total)
			}
			if items, total := env.listItems(token, "/api/v1/reviews?projectId="+other.ProjectID); total != 0 || len(items) != 0 {
				t.Fatalf("cross-tenant review filter leaked %d rows", total)
			}
			if items, total := env.listItems(token, "/api/v1/scans?projectId="+other.ProjectID); total != 0 || len(items) != 0 {
				t.Fatalf("cross-tenant scan filter leaked %d rows", total)
			}
			// The same title exists in both tenants; the search is tenant scoped.
			if items, total := env.listItems(token, "/api/v1/knowledge?q=shared%20runbook"); total != 1 || len(items) != 1 {
				t.Fatalf("cross-tenant knowledge search = %d/%d", len(items), total)
			}
			// The deployment-target options projection is tenant scoped too.
			options, _ := env.listItems(token, "/api/v1/deployment-targets/options")
			if len(options) != 2 {
				t.Fatalf("target options = %d, want the two own-tenant targets", len(options))
			}
			for _, option := range options {
				id, _ := option["id"].(string)
				if id != a.DevTargetID && id != a.ProdTargetID && id != b.DevTargetID && id != b.ProdTargetID {
					t.Fatalf("target options leaked unknown target %q", id)
				}
				if token == env.tokenA && id != a.DevTargetID && id != a.ProdTargetID {
					t.Fatalf("tenant A target options leaked %q", id)
				}
				if token == env.tokenB && id != b.DevTargetID && id != b.ProdTargetID {
					t.Fatalf("tenant B target options leaked %q", id)
				}
			}
		}
	})

	t.Run("audit and CSV export are tenant scoped", func(t *testing.T) {
		env := env.subtest(t)
		items, _ := env.listItems(env.tokenA, "/api/v1/audit?limit=100")
		if len(items) == 0 {
			t.Fatal("tenant A has no audit rows")
		}
		for _, item := range items {
			if id, _ := item["objectId"].(string); id != "" {
				for _, foreign := range []string{b.ProjectID, b.BuildID, b.DeploymentID, b.AwaitingDeployID, b.ReviewID, b.ScanID, b.DevTargetID, b.ProdTargetID, b.KnowledgeID, b.SecretID, b.ApprovalID, b.SecurityEventID} {
					if id == foreign {
						t.Fatalf("audit list leaked cross-tenant object id %q", id)
					}
				}
			}
		}
		w := env.do(env.tokenA, http.MethodGet, "/api/v1/audit/export.csv", "")
		if w.Code != http.StatusOK {
			t.Fatalf("audit export = %d %s", w.Code, w.Body.String())
		}
		for _, foreign := range []string{b.ProjectID, b.BuildID, b.ReviewID, b.ScanID, b.KnowledgeID, b.SecretID, b.ApprovalID} {
			if strings.Contains(w.Body.String(), foreign) {
				t.Fatalf("audit CSV leaked cross-tenant id %q", foreign)
			}
		}
		// Tenant B cannot see tenant A's rows either.
		itemsB, _ := env.listItems(env.tokenB, "/api/v1/audit?limit=100")
		for _, item := range itemsB {
			if id, _ := item["objectId"].(string); id == a.ProjectID || id == a.BuildID || id == a.ReviewID {
				t.Fatalf("tenant B audit list leaked tenant A object %q", id)
			}
		}
	})

	t.Run("inbound HMAC evidence belongs to the resource tenant", func(t *testing.T) {
		env := env.subtest(t)
		rows, err := env.db.Query(`SELECT tenant_id,project_id FROM scans WHERE delivery_id=$1`, a.ScanDelivery)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		tenantsSeen := map[string]string{}
		for rows.Next() {
			var tenantID, projectID string
			if err := rows.Scan(&tenantID, &projectID); err != nil {
				t.Fatal(err)
			}
			tenantsSeen[tenantID] = projectID
		}
		if len(tenantsSeen) != 2 || tenantsSeen[env.tenantA.ID] != a.ProjectID || tenantsSeen[env.tenantB.ID] != b.ProjectID {
			t.Fatalf("scan tenant attribution = %+v", tenantsSeen)
		}
		// The same delivery id delivered twice to the same tenant is idempotent.
		w := serveSignedExternalResult(t, env.h, "/api/v1/projects/"+a.ProjectID+"/scans",
			[]byte(`{"source":"scanner","verdict":"failed","summary":"retry"}`), []byte("0123456789abcdef"), time.Now().Unix(), a.ScanDelivery, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("idempotent scan retry = %d %s", w.Code, w.Body.String())
		}

		// External review results: both tenants reused the same delivery id and
		// each row is bound to the review's tenant.
		extTenants := map[string]int{}
		extRows, err := env.db.Query(`SELECT tenant_id FROM review_external_results WHERE delivery_id=$1`, isolationReviewDelivery)
		if err != nil {
			t.Fatal(err)
		}
		defer extRows.Close()
		for extRows.Next() {
			var tenantID string
			if err := extRows.Scan(&tenantID); err != nil {
				t.Fatal(err)
			}
			extTenants[tenantID]++
		}
		if extTenants[env.tenantA.ID] != 1 || extTenants[env.tenantB.ID] != 1 {
			t.Fatalf("review external result attribution = %+v", extTenants)
		}
		listed := env.expect(env.tokenA, http.MethodGet, "/api/v1/reviews/"+a.ReviewID+"/external-results", "", http.StatusOK)
		items := listed["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("external results = %d", len(items))
		}

		// Defense events: the signed tenantSlug selects the owning tenant; the
		// same delivery id is an independent idempotency scope per tenant.
		eventTenants := map[string]int{}
		eventRows, err := env.db.Query(`SELECT tenant_id FROM security_events WHERE delivery_id=$1`, a.SecurityDelivery)
		if err != nil {
			t.Fatal(err)
		}
		defer eventRows.Close()
		for eventRows.Next() {
			var tenantID string
			if err := eventRows.Scan(&tenantID); err != nil {
				t.Fatal(err)
			}
			eventTenants[tenantID]++
		}
		if eventTenants[env.tenantA.ID] != 1 || eventTenants[env.tenantB.ID] != 1 {
			t.Fatalf("security event attribution = %+v", eventTenants)
		}

		// The exact same names, delivery ids and endpoints exist in both tenants:
		// tenant-scoped uniqueness, not an accident of the data.
		for _, table := range []struct{ name, query string }{
			{"projects", `SELECT COUNT(*) FROM projects WHERE lower(name)='shared-project'`},
			{"deployment_targets", `SELECT COUNT(*) FROM deployment_targets WHERE lower(name)='shared-target'`},
			{"secrets", `SELECT COUNT(*) FROM secrets WHERE name='shared-secret'`},
			{"knowledge_docs", `SELECT COUNT(*) FROM knowledge_docs WHERE title='shared runbook'`},
		} {
			var count int
			if err := env.db.QueryRow(table.query).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatalf("%s rows with the same tenant-scoped name = %d, want 2", table.name, count)
			}
		}
		w = serveSignedExternalResult(t, env.h, "/api/v1/security-events", []byte(`{"source":"ids","severity":"low","eventType":"probe","summary":"unknown tenant","tenantSlug":"missing"}`), []byte("0123456789abcdef"), time.Now().Unix(), "security-delivery-unknown-01", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("unknown tenant security event = %d, want 404: %s", w.Code, w.Body.String())
		}
	})

	t.Run("outbox rows carry the owning tenant", func(t *testing.T) {
		env := env.subtest(t)
		// Approval notifications: one requested event per tenant, each payload
		// names its own tenant (id + slug) and its own deployment.
		approvalRows, err := env.db.Query(`SELECT tenant_id,payload->'tenant'->>'id',payload->'tenant'->>'slug',payload->'deployment'->>'id' FROM approval_notifications WHERE event='approval.requested' ORDER BY tenant_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer approvalRows.Close()
		seen := 0
		for approvalRows.Next() {
			var tenantID, payloadTenantID, slug, deploymentID string
			if err := approvalRows.Scan(&tenantID, &payloadTenantID, &slug, &deploymentID); err != nil {
				t.Fatal(err)
			}
			seen++
			if payloadTenantID != tenantID {
				t.Fatalf("approval outbox tenant column %q != payload tenant %q", tenantID, payloadTenantID)
			}
			switch tenantID {
			case env.tenantA.ID:
				if slug != "default" || deploymentID != a.AwaitingDeployID {
					t.Fatalf("tenant A approval payload = %q %q", slug, deploymentID)
				}
			case env.tenantB.ID:
				if slug != "other" || deploymentID != b.AwaitingDeployID {
					t.Fatalf("tenant B approval payload = %q %q", slug, deploymentID)
				}
			default:
				t.Fatalf("approval outbox row of unknown tenant %q", tenantID)
			}
		}
		if seen != 2 {
			t.Fatalf("approval.requested outbox rows = %d, want 2", seen)
		}

		reviewRows, err := env.db.Query(`SELECT tenant_id,payload->'tenant'->>'id',payload->'project'->>'id' FROM review_notifications WHERE event='review.requested' ORDER BY tenant_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer reviewRows.Close()
		seen = 0
		for reviewRows.Next() {
			var tenantID, payloadTenantID, projectID string
			if err := reviewRows.Scan(&tenantID, &payloadTenantID, &projectID); err != nil {
				t.Fatal(err)
			}
			seen++
			if payloadTenantID != tenantID {
				t.Fatalf("review outbox tenant column %q != payload tenant %q", tenantID, payloadTenantID)
			}
			if projectID != a.ProjectID && projectID != b.ProjectID {
				t.Fatalf("review outbox project %q is not a known tenant project", projectID)
			}
		}
		if seen != 2 {
			t.Fatalf("review.requested outbox rows = %d, want 2", seen)
		}
	})
}

// TestTenantIsolationApprovalExpiryAuditTenant proves the worker expiry path
// attributes its system audit row to the owning tenant instead of a
// deployment-wide default.
func TestTenantIsolationApprovalExpiryAuditTenant(t *testing.T) {
	env := newIsolationEnv(t)
	env.createData()
	b := env.features["b"]
	if _, err := env.db.Exec(`UPDATE approvals SET expires_at=1 WHERE id=$1`, b.ApprovalID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.deploy.ExpireApprovals(context.Background(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	var tenantID string
	if err := env.db.QueryRow(`SELECT tenant_id FROM auth_audit WHERE action='approval.expire' AND object_id=$1`, b.ApprovalID).Scan(&tenantID); err != nil {
		t.Fatalf("approval.expire audit row: %v", err)
	}
	if tenantID != env.tenantB.ID {
		t.Fatalf("approval.expire audit tenant = %q, want %q", tenantID, env.tenantB.ID)
	}
	// Tenant A's admin cannot see it; tenant B's admin can.
	for _, tc := range []struct {
		token string
		want  bool
	}{{env.tokenA, false}, {env.tokenB, true}} {
		items, _ := env.listItems(tc.token, "/api/v1/audit?limit=100&objectType=approval")
		found := false
		for _, item := range items {
			if id, _ := item["objectId"].(string); id == b.ApprovalID {
				found = true
			}
		}
		if found != tc.want {
			t.Fatalf("approval expiry audit visibility = %v, want %v", found, tc.want)
		}
	}
}

// TestTenantIsolationBuildWorkerKeepsRowTenant runs the single deployment-wide
// build worker over queued builds of both tenants: each row is claimed with its
// own tenant, finalizes under that tenant, and its log events stay reachable only
// through the owning tenant's build.
func TestTenantIsolationBuildWorkerKeepsRowTenant(t *testing.T) {
	env := newIsolationEnv(t)
	env.createData()
	a, b := env.features["a"], env.features["b"]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.builds.RunWorker(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	deadline := time.Now().Add(30 * time.Second)
	for {
		var settled int
		if err := env.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE id IN ($1,$2) AND status IN ('succeeded','failed','cancelled','interrupted')`, a.BuildID, b.BuildID).Scan(&settled); err != nil {
			t.Fatal(err)
		}
		if settled == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not settle both tenants' builds")
		}
		time.Sleep(50 * time.Millisecond)
	}
	tenantsByBuild := map[string]string{}
	rows, err := env.db.Query(`SELECT id,tenant_id FROM builds WHERE id IN ($1,$2)`, a.BuildID, b.BuildID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, tenantID string
		if err := rows.Scan(&id, &tenantID); err != nil {
			t.Fatal(err)
		}
		tenantsByBuild[id] = tenantID
	}
	if tenantsByBuild[a.BuildID] != env.tenantA.ID || tenantsByBuild[b.BuildID] != env.tenantB.ID {
		t.Fatalf("build tenants after worker = %+v", tenantsByBuild)
	}
	// Log events were written for both builds and are only exposed through the
	// owning tenant's visibility predicate.
	var logTenants int
	if err := env.db.QueryRow(`SELECT COUNT(DISTINCT b.tenant_id) FROM build_log_events e JOIN builds b ON b.id=e.build_id WHERE e.build_id IN ($1,$2)`, a.BuildID, b.BuildID).Scan(&logTenants); err != nil {
		t.Fatal(err)
	}
	if logTenants != 2 {
		t.Fatalf("build log events span %d tenants, want 2", logTenants)
	}
	for _, tc := range []struct {
		token, ownBuild, otherBuild string
	}{
		{env.tokenA, a.BuildID, b.BuildID},
		{env.tokenB, b.BuildID, a.BuildID},
	} {
		if w := env.do(tc.token, http.MethodGet, "/api/v1/builds/"+tc.ownBuild+"/logs", ""); w.Code != http.StatusOK {
			t.Fatalf("own build logs = %d %s", w.Code, w.Body.String())
		}
		env.expect404(tc.token, http.MethodGet, "/api/v1/builds/"+tc.otherBuild+"/logs", "")
	}
}

// TestTenantIsolationSuspendedTenant proves the F1 suspended-tenant contract
// still holds once business data isolation exists: login, refresh and existing
// access tokens are all rejected while the tenant is suspended.
func TestTenantIsolationSuspendedTenant(t *testing.T) {
	env := newIsolationEnv(t)
	// Capture a refresh cookie while tenant B is still active.
	login := request(env.h, http.MethodPost, "/api/v1/auth/login", `{"username":"admin-b","password":"`+isolationOtherPassword+`","tenantSlug":"other"}`, "")
	if login.Code != http.StatusOK {
		t.Fatalf("pre-suspension login = %d %s", login.Code, login.Body.String())
	}
	var refreshCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == refreshCookieName {
			refreshCookie = cookie
		}
	}
	if refreshCookie == nil {
		t.Fatal("login did not set a refresh cookie")
	}
	if _, err := env.db.Exec(`UPDATE tenants SET status='suspended' WHERE id=$1`, env.tenantB.ID); err != nil {
		t.Fatal(err)
	}
	// Login with the slug is rejected with the uniform 401.
	w := request(env.h, http.MethodPost, "/api/v1/auth/login", `{"username":"admin-b","password":"`+isolationOtherPassword+`","tenantSlug":"other"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("suspended tenant login = %d %s", w.Code, w.Body.String())
	}
	// Login without a slug is rejected too (the cross-tenant lookup finds the
	// suspended tenant).
	w = request(env.h, http.MethodPost, "/api/v1/auth/login", `{"username":"admin-b","password":"`+isolationOtherPassword+`"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("suspended tenant login without slug = %d %s", w.Code, w.Body.String())
	}
	// Refresh with a cookie obtained before the suspension is rejected and the
	// cookie is cleared.
	refreshRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", strings.NewReader(`{}`))
	refreshRequest.Header.Set("Content-Type", "application/json")
	refreshRequest.Header.Set("Origin", "http://localhost:5173")
	refreshRequest.AddCookie(refreshCookie)
	refreshRecorder := httptest.NewRecorder()
	env.h.ServeHTTP(refreshRecorder, refreshRequest)
	if refreshRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("suspended tenant refresh = %d %s", refreshRecorder.Code, refreshRecorder.Body.String())
	}
	var cleared bool
	for _, cookie := range refreshRecorder.Result().Cookies() {
		if cookie.Name == refreshCookieName && cookie.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("suspended tenant refresh did not clear the refresh cookie")
	}
	// An access token minted before the suspension stops working immediately.
	if w := env.do(env.tokenB, http.MethodGet, "/api/v1/auth/me", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("suspended tenant access token = %d %s", w.Code, w.Body.String())
	}
	// Tenant A is unaffected.
	if w := env.do(env.tokenA, http.MethodGet, "/api/v1/auth/me", ""); w.Code != http.StatusOK {
		t.Fatalf("default tenant access token = %d %s", w.Code, w.Body.String())
	}
}

// TestTenantIsolationCrossTenantUniqueness drives the schema-level checks through
// the HTTP API: identical names are accepted in two tenants but a duplicate
// inside one tenant is a 409.
func TestTenantIsolationCrossTenantUniqueness(t *testing.T) {
	env := newIsolationEnv(t)
	env.createData()
	// Same project name in the same tenant is a conflict; cross-tenant was
	// already accepted by createData.
	w := env.do(env.tokenA, http.MethodPost, "/api/v1/projects", `{"name":"SHARED-PROJECT"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate project name inside a tenant = %d %s", w.Code, w.Body.String())
	}
	w = env.do(env.tokenA, http.MethodPost, "/api/v1/deployment-targets", `{"name":"shared-target","endpoint":"https://other.example.test"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate target name inside a tenant = %d %s", w.Code, w.Body.String())
	}
	w = env.do(env.tokenA, http.MethodPost, "/api/v1/secrets", `{"name":"shared-secret","value":"another-value-01"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate secret name inside a tenant = %d %s", w.Code, w.Body.String())
	}
}

// TestTenantIsolationNotifierDeliversTenantPayload runs both outboxes' notifiers
// and proves the delivered JSON carries the owning tenant in every event.
func TestTenantIsolationNotifierDeliversTenantPayload(t *testing.T) {
	env := newIsolationEnv(t)
	env.createData()
	a, b := env.features["a"], env.features["b"]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	go env.deploy.RunNotifier(ctx, logger)
	go env.review.RunNotifier(ctx, logger)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if len(env.webhook.snapshot()) >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifiers delivered %d events, want 4: %+v", len(env.webhook.snapshot()), env.webhook.snapshot())
		}
		time.Sleep(50 * time.Millisecond)
	}
	expectedDeployment := map[string]string{env.tenantA.ID: a.AwaitingDeployID, env.tenantB.ID: b.AwaitingDeployID}
	expectedProject := map[string]string{env.tenantA.ID: a.ProjectID, env.tenantB.ID: b.ProjectID}
	seenEvents := map[string]int{}
	for _, event := range env.webhook.snapshot() {
		tenant, ok := event.Payload["tenant"].(map[string]any)
		if !ok {
			t.Fatalf("%s payload has no tenant block", event.Event)
		}
		tenantID, _ := tenant["id"].(string)
		slug, _ := tenant["slug"].(string)
		if tenantID != env.tenantA.ID && tenantID != env.tenantB.ID {
			t.Fatalf("%s delivered to unknown tenant %q", event.Event, tenantID)
		}
		wantSlug := "default"
		if tenantID == env.tenantB.ID {
			wantSlug = "other"
		}
		if slug != wantSlug {
			t.Fatalf("%s tenant slug = %q, want %q", event.Event, slug, wantSlug)
		}
		seenEvents[event.Event]++
		switch {
		case strings.HasPrefix(event.Event, "approval."):
			deployment, _ := event.Payload["deployment"].(map[string]any)
			if id, _ := deployment["id"].(string); id != expectedDeployment[tenantID] {
				t.Fatalf("%s carried deployment %q for tenant %q", event.Event, id, tenantID)
			}
		case strings.HasPrefix(event.Event, "review."):
			project, _ := event.Payload["project"].(map[string]any)
			if id, _ := project["id"].(string); id != expectedProject[tenantID] {
				t.Fatalf("%s carried project %q for tenant %q", event.Event, id, tenantID)
			}
			review, _ := event.Payload["review"].(map[string]any)
			if reviewTenant := review["id"]; reviewTenant == nil {
				t.Fatalf("%s payload has no review block", event.Event)
			}
		default:
			t.Fatalf("unexpected event %q", event.Event)
		}
	}
	if seenEvents["approval.requested"] != 2 || seenEvents["review.requested"] != 2 {
		t.Fatalf("delivered events = %+v", seenEvents)
	}
	// Delivery marks the rows delivered; nothing is left pending.
	var pending int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM approval_notifications WHERE status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending approval notifications = %d", pending)
	}
}
