package deployments

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

const testSecretKey = "0123456789abcdef0123456789abcdef"

var testActor = auth.Actor{ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-deployments"}

func testService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	return testServiceWith(t, Options{SecretKey: []byte(testSecretKey)})
}

// testServiceWith always applies the migration; CheckTimeout defaults to 2s.
func testServiceWith(t *testing.T, opts Options) (*Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if opts.CheckTimeout == 0 {
		opts.CheckTimeout = 2 * time.Second
	}
	return New(db, opts), db
}

func validInput(name, endpoint string) Input {
	return Input{Name: name, Endpoint: endpoint}
}

func auditRows(t *testing.T, db *sql.DB, objectID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT action||'|'||detail FROM auth_audit WHERE object_type='deployment_target' AND object_id=$1 ORDER BY created_at,id`, objectID)
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

func TestCreateValidationAndDefaults(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()

	invalid := []Input{
		validInput("ab", "https://example.com"),
		validInput(" padded", "https://example.com"),
		validInput("trailing ", "https://example.com"),
		{Name: "line\nbreak", Endpoint: "https://example.com"},
		{Name: "kind", Kind: "tcp", Endpoint: "https://example.com"},
		{Name: "environment", Environment: "production-ish", Endpoint: "https://example.com"},
		{Name: "no-endpoint", Endpoint: ""},
		{Name: "ftp", Endpoint: "ftp://example.com"},
		{Name: "userinfo", Endpoint: "https://user:pass@example.com"},
		{Name: "query", Endpoint: "https://example.com/api?token=1"},
		{Name: "fragment", Endpoint: "https://example.com/api#frag"},
		{Name: "healthPath", Endpoint: "https://example.com", HealthPath: "health"},
		{Name: "healthPath-space", Endpoint: "https://example.com", HealthPath: "/a b"},
		{Name: "status", Endpoint: "https://example.com", Status: "paused"},
		{Name: "secret-too-long", Endpoint: "https://example.com", Secret: strings.Repeat("s", 4097)},
		{Name: "secret-multiline", Endpoint: "https://example.com", Secret: "line1\nline2"},
	}
	for _, input := range invalid {
		if _, err := s.Create(ctx, testActor, input); !errors.Is(err, ErrValidation) {
			t.Fatalf("Create(%q) error = %v, want ErrValidation", input.Name, err)
		}
	}

	target, err := s.Create(ctx, testActor, validInput("web-prod", "https://example.com/"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind != KindHTTP || target.Environment != EnvironmentDevelopment || target.HealthPath != "/" ||
		target.Status != StatusActive || target.HasSecret || target.CreatedBy != testActor.ID || target.CreatedAt == 0 || target.UpdatedAt == 0 {
		t.Fatalf("unexpected defaults: %+v", target)
	}
	if target.LastCheckAt != 0 || target.LastCheckOK || target.LastCheckDetail != "" {
		t.Fatalf("unexpected check state: %+v", target)
	}
}

func TestCreateDuplicateNameIsCaseInsensitive(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, testActor, validInput("web-prod", "https://example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, testActor, validInput("WEB-PROD", "https://other.example.com")); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate error = %v, want ErrConflict", err)
	}
}

func TestUpdateAndRemove(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	target, err := s.Create(ctx, testActor, validInput("web", "https://example.com"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s.Update(ctx, testActor, target.ID, Patch{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty patch error = %v", err)
	}
	if _, err = s.Update(ctx, testActor, "missing-0000000000000001", Patch{Status: str(StatusDisabled)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown update error = %v", err)
	}

	updated, err := s.Update(ctx, testActor, target.ID, Patch{
		Name:        str("web-staging"),
		Environment: str(EnvironmentStaging),
		Endpoint:    str("https://staging.example.com/base"),
		HealthPath:  str("/healthz"),
		Status:      str(StatusDisabled),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "web-staging" || updated.Environment != EnvironmentStaging || updated.Endpoint != "https://staging.example.com/base" ||
		updated.HealthPath != "/healthz" || updated.Status != StatusDisabled || updated.UpdatedAt < target.UpdatedAt {
		t.Fatalf("updated target = %+v", updated)
	}
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Name: str("x")}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid merged name error = %v", err)
	}
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Endpoint: str("https://user:pw@example.com")}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid merged endpoint error = %v", err)
	}
	if _, err = s.Create(ctx, testActor, validInput("taken-name", "https://taken.example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Name: str("TAKEN-NAME")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename conflict error = %v", err)
	}

	if err = s.Remove(ctx, testActor, "missing-0000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown remove error = %v", err)
	}
	if err = s.Remove(ctx, testActor, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, testActor, target.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after remove error = %v", err)
	}
	if err = s.Remove(ctx, testActor, target.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeat remove error = %v", err)
	}

	rows := auditRows(t, db, target.ID)
	if len(rows) != 3 {
		t.Fatalf("audit rows = %v", rows)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row] = true
	}
	for _, want := range []string{
		ActionCreate + "|name=web",
		ActionUpdate + "|name=web-staging",
		ActionRemove + "|name=web-staging",
	} {
		if !seen[want] {
			t.Fatalf("missing audit row %q in %v", want, rows)
		}
	}
}

func str(value string) *string { return &value }

func TestGetRejectsMalformedID(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if _, err := s.Get(ctx, testActor, "short"); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed id error = %v", err)
	}
}

func TestListPagination(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	for _, name := range []string{"curie", "ada", "grace"} {
		if _, err := s.Create(ctx, testActor, validInput(name, "https://"+name+".example.com")); err != nil {
			t.Fatal(err)
		}
	}
	list, total, err := s.List(ctx, testActor, 2, 0)
	if err != nil || total != 3 || len(list) != 2 || list[0].Name != "ada" || list[1].Name != "curie" {
		t.Fatalf("list = %+v total=%d err=%v", list, total, err)
	}
	if _, _, err = s.List(ctx, testActor, 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad limit error = %v", err)
	}
	if _, _, err = s.List(ctx, testActor, 20, -1); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad offset error = %v", err)
	}
}

func TestSecretIsEncryptedAtRestAndNeverReturned(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	target, err := s.Create(ctx, testActor, Input{Name: "secret-lab", Endpoint: "https://example.com", Secret: "s3cret-token"})
	if err != nil {
		t.Fatal(err)
	}
	if !target.HasSecret {
		t.Fatal("hasSecret = false after create with secret")
	}
	data, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cret-token") {
		t.Fatalf("target JSON leaked the secret: %s", data)
	}

	var sealed []byte
	if err := db.QueryRow(`SELECT secret_cipher FROM deployment_targets WHERE id=$1`, target.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 {
		t.Fatal("secret_cipher is empty")
	}
	if strings.Contains(string(sealed), "s3cret-token") {
		t.Fatal("plaintext stored in secret_cipher")
	}
	plain, err := s.decryptSecret(target.ID, sealed)
	if err != nil || plain != "s3cret-token" {
		t.Fatalf("decrypt = %q err=%v", plain, err)
	}
	// The AAD binds the ciphertext to its target id.
	if _, err := s.decryptSecret("other-0000000000000001", sealed); err == nil {
		t.Fatal("ciphertext decrypted under a different target id")
	}
}

func TestSecretUpdateSemantics(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	target, err := s.Create(ctx, testActor, Input{Name: "secret-update", Endpoint: "https://example.com", Secret: "first-token"})
	if err != nil {
		t.Fatal(err)
	}
	// nil leaves the stored secret untouched.
	updated, err := s.Update(ctx, testActor, target.ID, Patch{Name: str("secret-update-2")})
	if err != nil || !updated.HasSecret {
		t.Fatalf("untouched secret = %+v err=%v", updated, err)
	}
	var sealed []byte
	if err := db.QueryRow(`SELECT secret_cipher FROM deployment_targets WHERE id=$1`, target.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := s.decryptSecret(target.ID, sealed); err != nil || plain != "first-token" {
		t.Fatalf("secret after name-only patch = %q err=%v", plain, err)
	}
	// A non-empty pointer replaces it.
	updated, err = s.Update(ctx, testActor, target.ID, Patch{Secret: str("second-token")})
	if err != nil || !updated.HasSecret {
		t.Fatalf("replace secret = %+v err=%v", updated, err)
	}
	if err := db.QueryRow(`SELECT secret_cipher FROM deployment_targets WHERE id=$1`, target.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := s.decryptSecret(target.ID, sealed); err != nil || plain != "second-token" {
		t.Fatalf("replaced secret = %q err=%v", plain, err)
	}
	// An empty string clears it.
	updated, err = s.Update(ctx, testActor, target.ID, Patch{Secret: str("")})
	if err != nil || updated.HasSecret {
		t.Fatalf("clear secret = %+v err=%v", updated, err)
	}
	var cipherLength int
	if err := db.QueryRow(`SELECT octet_length(secret_cipher) FROM deployment_targets WHERE id=$1`, target.ID).Scan(&cipherLength); err != nil {
		t.Fatal(err)
	}
	if cipherLength != 0 {
		t.Fatalf("secret_cipher not cleared: %d bytes", cipherLength)
	}
	// Invalid secret is rejected without touching state.
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Secret: str("bad\nsecret")}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid secret error = %v", err)
	}
}

func TestSecretStorageDisabled(t *testing.T) {
	s, _ := testServiceWith(t, Options{SecretKey: nil})
	ctx := context.Background()
	if _, err := s.Create(ctx, testActor, Input{Name: "no-secret", Endpoint: "https://example.com"}); err != nil {
		t.Fatalf("target without secret should work: %v", err)
	}
	_, err := s.Create(ctx, testActor, Input{Name: "with-secret", Endpoint: "https://example.com", Secret: "token"})
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "AEGIS_SECRET_KEY") {
		t.Fatalf("create with secret error = %v", err)
	}
	// A short key is not configured either.
	s2, _ := testServiceWith(t, Options{SecretKey: []byte("short")})
	if _, err := s2.Create(ctx, testActor, Input{Name: "short-key", Endpoint: "https://example.com", Secret: "token"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("short key error = %v", err)
	}
	// Update cannot add a secret when storage is disabled.
	target, err := s.Create(ctx, testActor, Input{Name: "add-later", Endpoint: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Secret: str("token")}); !errors.Is(err, ErrValidation) {
		t.Fatalf("update adding secret error = %v", err)
	}
}

// A stored credential must never travel over cleartext http to a non-loopback
// host; loopback stays allowed for local probes and tests.
func TestSecretRequiresTLSOutsideLoopback(t *testing.T) {
	s, _ := testServiceWith(t, Options{SecretKey: []byte(testSecretKey)})
	ctx := context.Background()
	if _, err := s.Create(ctx, testActor, Input{Name: "plain", Endpoint: "http://example.com", Secret: "token"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("http + secret error = %v", err)
	}
	if _, err := s.Create(ctx, testActor, Input{Name: "loopback", Endpoint: "http://127.0.0.1:8080", Secret: "token"}); err != nil {
		t.Fatalf("loopback http + secret should work: %v", err)
	}
	if _, err := s.Create(ctx, testActor, Input{Name: "tls", Endpoint: "https://example.com", Secret: "token"}); err != nil {
		t.Fatalf("https + secret should work: %v", err)
	}
	target, err := s.Create(ctx, testActor, Input{Name: "moves", Endpoint: "https://example.com", Secret: "token"})
	if err != nil {
		t.Fatal(err)
	}
	// Moving a target that already holds a secret onto cleartext http is refused
	// even though the patch does not touch the secret itself.
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Endpoint: str("http://example.com")}); !errors.Is(err, ErrValidation) {
		t.Fatalf("downgrade with stored secret error = %v", err)
	}
	// Switching to a loopback cleartext endpoint stays possible.
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Endpoint: str("http://127.0.0.1:8080")}); err != nil {
		t.Fatalf("loopback downgrade should work: %v", err)
	}
	// Adding a secret to a cleartext non-loopback target is refused too.
	plain, err := s.Create(ctx, testActor, Input{Name: "plain-upgrade", Endpoint: "http://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, testActor, plain.ID, Patch{Secret: str("token")}); !errors.Is(err, ErrValidation) {
		t.Fatalf("adding secret to http target error = %v", err)
	}
}

func TestCheckHealthyAndUnhealthy(t *testing.T) {
	var requests atomic.Int32
	var mu sync.Mutex
	var seenPath, seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenPath, seenAuth = r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	s, db := testService(t)
	ctx := context.Background()
	target, err := s.Create(ctx, testActor, Input{
		Name: "probe", Endpoint: upstream.URL + "/", HealthPath: "/healthz", Secret: "probe-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.Check(ctx, testActor, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.StatusCode != http.StatusOK || result.Detail != "ok" || result.CheckedAt == 0 || result.LatencyMS < 0 {
		t.Fatalf("healthy result = %+v", result)
	}
	mu.Lock()
	path, authHeader := seenPath, seenAuth
	mu.Unlock()
	if path != "/healthz" {
		t.Fatalf("probe path = %q", path)
	}
	if authHeader != "Bearer probe-token" {
		t.Fatalf("Authorization header = %q", authHeader)
	}
	checkTarget, err := s.Get(ctx, testActor, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if checkTarget.LastCheckAt != result.CheckedAt || !checkTarget.LastCheckOK || checkTarget.LastCheckDetail != "ok" {
		t.Fatalf("persisted check state = %+v", checkTarget)
	}

	result, err = s.Check(ctx, testActor, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.StatusCode != http.StatusInternalServerError || result.Detail != "http 500" {
		t.Fatalf("unhealthy result = %+v", result)
	}
	checkTarget, err = s.Get(ctx, testActor, target.ID)
	if err != nil || checkTarget.LastCheckOK || checkTarget.LastCheckDetail != "http 500" {
		t.Fatalf("persisted unhealthy state = %+v err=%v", checkTarget, err)
	}

	rows := auditRows(t, db, target.ID)
	var checks []string
	for _, row := range rows {
		if strings.HasPrefix(row, ActionCheck) {
			checks = append(checks, row)
		}
	}
	if len(checks) != 2 {
		t.Fatalf("check audit rows = %v", checks)
	}
	checkDetails := map[string]bool{}
	for _, row := range checks {
		checkDetails[row] = true
	}
	if !checkDetails[ActionCheck+"|status=200 ok"] || !checkDetails[ActionCheck+"|status=500 http 500"] {
		t.Fatalf("check audit rows = %v", checks)
	}
	for _, row := range rows {
		if strings.Contains(row, "probe-token") {
			t.Fatalf("audit leaked the secret: %q", row)
		}
	}
}

func TestCheckTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	defer close(release)

	s, _ := testServiceWith(t, Options{SecretKey: []byte(testSecretKey), CheckTimeout: 50 * time.Millisecond})
	target, err := s.Create(context.Background(), testActor, validInput("slow", upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Check(context.Background(), testActor, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.StatusCode != 0 || result.Detail != "timeout" {
		t.Fatalf("timeout result = %+v", result)
	}
}

func TestCheckNetworkErrorIsSanitized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := upstream.URL
	upstream.Close()

	s, _ := testServiceWith(t, Options{SecretKey: []byte(testSecretKey), CheckTimeout: 500 * time.Millisecond})
	target, err := s.Create(context.Background(), testActor, Input{Name: "down", Endpoint: endpoint, Secret: "hidden-token"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Check(context.Background(), testActor, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.StatusCode != 0 || !strings.HasPrefix(result.Detail, "network error:") {
		t.Fatalf("network result = %+v", result)
	}
	if strings.Contains(result.Detail, "hidden-token") || strings.Contains(result.Detail, "://") {
		t.Fatalf("unsanitized detail = %q", result.Detail)
	}
}

func TestCheckUnknownTarget(t *testing.T) {
	s, _ := testService(t)
	if _, err := s.Check(context.Background(), testActor, "missing-0000000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown check error = %v", err)
	}
}

func TestCheckNetworkErrorShortDetail(t *testing.T) {
	if got := sanitizeDetail(strings.Repeat("x", 500), "", ""); len(got) > 124 {
		t.Fatalf("detail not truncated: %d bytes", len(got))
	}
	if got := sanitizeDetail(fmt.Sprintf("dial %s: boom", "https://example.com"), "https://example.com", ""); strings.Contains(got, "https://example.com") {
		t.Fatalf("endpoint not redacted: %q", got)
	}
}
