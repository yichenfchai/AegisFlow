package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/secrets"
)

type secretEnvelope struct {
	Data secrets.Metadata `json:"data"`
}

type secretPageEnvelope struct {
	Data struct {
		Items  []secrets.Metadata `json:"items"`
		Total  int                `json:"total"`
		Limit  int                `json:"limit"`
		Offset int                `json:"offset"`
	} `json:"data"`
}

const (
	httpSecretValue = "super-secret-value-123"
	httpSecretKey   = "0123456789abcdef0123456789abcdef"
)

func parseSecret(t *testing.T, body []byte) secrets.Metadata {
	t.Helper()
	var envelope secretEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("secret payload: %s", string(body))
	}
	return envelope.Data
}

func TestSecretsHTTPWriteOnlyLifecycle(t *testing.T) {
	h, db := knowledgeSecretsTestServer(t, []byte(httpSecretKey))
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")

	w := request(h, "GET", "/api/v1/secrets?limit=20&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("empty list: %d %s", w.Code, w.Body.String())
	}

	w = request(h, "POST", "/api/v1/secrets", `{"name":"prod-db-password","value":"`+httpSecretValue+`"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), httpSecretValue) || strings.Contains(w.Body.String(), "ciphertext") {
		t.Fatalf("create response leaked secret material: %s", w.Body.String())
	}
	meta := parseSecret(t, w.Body.Bytes())
	if meta.ID == "" || meta.Name != "prod-db-password" || meta.CreatedByName != "admin" || meta.CreatedAt == 0 {
		t.Fatalf("metadata = %+v", meta)
	}
	var sealed []byte
	var keyVersion int
	if err := db.QueryRow(`SELECT ciphertext,key_version FROM secrets WHERE id=$1`, meta.ID).Scan(&sealed, &keyVersion); err != nil {
		t.Fatal(err)
	}
	if keyVersion != 1 || len(sealed) <= 12 || strings.Contains(string(sealed), httpSecretValue) {
		t.Fatalf("stored ciphertext = %x version=%d", sealed, keyVersion)
	}

	// List metadata only; the value and ciphertext never appear.
	w = request(h, "GET", "/api/v1/secrets", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), httpSecretValue) || strings.Contains(w.Body.String(), "ciphertext") {
		t.Fatalf("list leaked secret material: %s", w.Body.String())
	}
	var page secretPageEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Data.Total != 1 || page.Data.Items[0].ID != meta.ID || page.Data.Items[0].Name != "prod-db-password" {
		t.Fatalf("page = %+v", page.Data)
	}

	// Rotation returns only metadata and bumps the stored rotation counter.
	w = request(h, "PUT", "/api/v1/secrets/"+meta.ID, `{"value":"rotated-secret-value-456"}`, adminToken)
	if w.Code != 200 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "rotated-secret-value-456") {
		t.Fatalf("rotate response leaked the new value: %s", w.Body.String())
	}
	var rotatedVersion int
	if err := db.QueryRow(`SELECT key_version FROM secrets WHERE id=$1`, meta.ID).Scan(&rotatedVersion); err != nil {
		t.Fatal(err)
	}
	if rotatedVersion != 2 {
		t.Fatalf("key_version = %d", rotatedVersion)
	}

	// Duplicate name 409; missing id 404; bad payload 400.
	if w = request(h, "POST", "/api/v1/secrets", `{"name":"prod-db-password","value":"other"}`, adminToken); w.Code != 409 || errorCode(t, w) != "conflict" {
		t.Fatalf("duplicate name: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "PUT", "/api/v1/secrets/missing-0000000000001", `{"value":"x"}`, adminToken); w.Code != 404 {
		t.Fatalf("rotate missing: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`{"name":"","value":"x"}`, `{"name":"ok","value":""}`, `{"name":"ok"}`} {
		if w = request(h, "POST", "/api/v1/secrets", body, adminToken); w.Code != 400 {
			t.Fatalf("invalid create %s: %d %s", body, w.Code, w.Body.String())
		}
	}

	// Audit rows never contain a value.
	rows, err := db.Query(`SELECT action,detail FROM auth_audit WHERE object_type='secret' ORDER BY created_at,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			t.Fatal(err)
		}
		seen[action] = true
		if strings.Contains(detail, httpSecretValue) || strings.Contains(detail, "rotated-secret-value") {
			t.Fatalf("audit detail leaked a value: %q", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen[secrets.ActionCreate] || !seen[secrets.ActionRotate] {
		t.Fatalf("audit actions = %+v", seen)
	}

	// Non-admins get 403 from the route guard (no metadata is exposed).
	createUserHTTP(t, h, adminToken, "secret-member", "member-password-01", "member")
	memberToken, _, _ := loginHTTP(t, h, "secret-member", "member-password-01")
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/secrets", ""},
		{"POST", "/api/v1/secrets", `{"name":"nope","value":"x"}`},
		{"PUT", "/api/v1/secrets/" + meta.ID, `{"value":"x"}`},
		{"DELETE", "/api/v1/secrets/" + meta.ID, ""},
	} {
		if w = request(h, tc.method, tc.path, tc.body, memberToken); w.Code != 403 {
			t.Fatalf("member %s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// Delete removes the row; the secret is gone from the list.
	if w = request(h, "DELETE", "/api/v1/secrets/"+meta.ID, "", adminToken); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/secrets", "", adminToken); w.Code != 200 {
		t.Fatalf("list after delete: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Data.Total != 0 {
		t.Fatalf("list after delete = %+v", page.Data)
	}
}

func TestSecretsHTTPNotConfiguredAndUnwired(t *testing.T) {
	// A wired store without AEGIS_SECRET_KEY keeps metadata reads available and
	// answers 503 with the documented message on create/rotate.
	h, _ := knowledgeSecretsTestServer(t, nil)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	if w := request(h, "GET", "/api/v1/secrets", "", adminToken); w.Code != 200 {
		t.Fatalf("list without key: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/secrets", `{"name":"blocked","value":"x"}`},
		{"PUT", "/api/v1/secrets/blocked-000000000001", `{"value":"x"}`},
	} {
		w := request(h, tc.method, tc.path, tc.body, adminToken)
		if w.Code != 503 || errorCode(t, w) != "secrets_not_configured" {
			t.Fatalf("%s %s without key: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "secrets are not configured") {
			t.Fatalf("503 message = %s", w.Body.String())
		}
	}

	// A store that was not wired at all fails closed with the shared dependency
	// envelope.
	unwired, _ := testServer(t)
	unwiredToken, _, _ := loginHTTP(t, unwired, "admin", "a-strong-password")
	if w := request(unwired, "GET", "/api/v1/secrets", "", unwiredToken); w.Code != 503 || errorCode(t, w) != "dependencies_unavailable" {
		t.Fatalf("unwired list: %d %s", w.Code, w.Body.String())
	}
	if w := request(unwired, "POST", "/api/v1/secrets", `{"name":"x","value":"y"}`, unwiredToken); w.Code != 503 || errorCode(t, w) != "dependencies_unavailable" {
		t.Fatalf("unwired create: %d %s", w.Code, w.Body.String())
	}
}
