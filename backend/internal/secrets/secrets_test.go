package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// testSecretKey is a fixed 32-byte AES key used only by this test binary.
var testSecretKey = []byte("0123456789abcdef0123456789abcdef")

var (
	adminActor  = auth.Actor{ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-admin"}
	memberActor = auth.Actor{ID: "member-000000000000001", Username: "mia", Role: "member", RequestID: "req-member"}
)

const testSecretValue = "super-secret-value-123"

func testService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []auth.Actor{adminActor, memberActor} {
		if _, err := db.Exec(`INSERT INTO auth_users(id,username,password_hash,display_name,role)
		 VALUES($1,$2,'unused',$2,$3)`, actor.ID, actor.Username, actor.Role); err != nil {
			t.Fatal(err)
		}
	}
	return New(db, Options{SecretKey: testSecretKey}), db
}

// readCiphertext returns the stored ciphertext and rotation counter of one row.
func readCiphertext(t *testing.T, db *sql.DB, id string) ([]byte, int) {
	t.Helper()
	var sealed []byte
	var keyVersion int
	if err := db.QueryRow(`SELECT ciphertext,key_version FROM secrets WHERE id=$1`, id).Scan(&sealed, &keyVersion); err != nil {
		t.Fatal(err)
	}
	return sealed, keyVersion
}

// decryptForTest opens a stored ciphertext with the documented layout and AAD,
// proving the round trip without adding a read path to the production service.
func decryptForTest(t *testing.T, id string, sealed []byte) string {
	t.Helper()
	block, err := aes.NewCipher(testSecretKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) <= aead.NonceSize() {
		t.Fatalf("ciphertext too short: %d", len(sealed))
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], secretAAD(id))
	if err != nil {
		t.Fatalf("decrypt %s: %v", id, err)
	}
	return string(plain)
}

func TestCreateRotateDeleteEncryptsAtRest(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	meta, err := s.Create(ctx, adminActor, "db-password", testSecretValue)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID == "" || meta.Name != "db-password" || meta.CreatedBy != adminActor.ID ||
		meta.CreatedByName != "root" || meta.CreatedAt == 0 || meta.UpdatedAt != meta.CreatedAt {
		t.Fatalf("metadata = %+v", meta)
	}

	sealed, version := readCiphertext(t, db, meta.ID)
	if version != 1 || len(sealed) <= 12 || bytes.Contains(sealed, []byte(testSecretValue)) {
		t.Fatalf("stored ciphertext = %x version=%d", sealed, version)
	}
	if got := decryptForTest(t, meta.ID, sealed); got != testSecretValue {
		t.Fatalf("decrypted = %q", got)
	}

	// The list projection carries metadata only; neither the value nor the
	// ciphertext can appear in a JSON response.
	list, total, err := s.List(ctx, adminActor, ListFilter{Limit: 20})
	if err != nil || total != 1 || len(list) != 1 || list[0].ID != meta.ID {
		t.Fatalf("list: %v total=%d list=%+v", err, total, list)
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), testSecretValue) || strings.Contains(string(encoded), "ciphertext") {
		t.Fatalf("list leaked secret material: %s", encoded)
	}

	// A rotation replaces the ciphertext, bumps key_version and returns only
	// metadata.
	rotated, err := s.Rotate(ctx, adminActor, meta.ID, "rotated-secret-value-456")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ID != meta.ID || rotated.Name != "db-password" || rotated.CreatedAt != meta.CreatedAt ||
		rotated.CreatedBy != meta.CreatedBy {
		t.Fatalf("rotated metadata = %+v", rotated)
	}
	rotatedCipher, rotatedVersion := readCiphertext(t, db, meta.ID)
	if rotatedVersion != 2 {
		t.Fatalf("key_version after rotation = %d", rotatedVersion)
	}
	if bytes.Equal(rotatedCipher, sealed) {
		t.Fatal("rotation reused the previous ciphertext")
	}
	if got := decryptForTest(t, meta.ID, rotatedCipher); got != "rotated-secret-value-456" {
		t.Fatalf("rotated value = %q", got)
	}
	if bytes.Contains(rotatedCipher, []byte("rotated-secret-value-456")) {
		t.Fatal("rotated ciphertext contains plaintext")
	}

	// Two equal values under different names never produce equal ciphertexts.
	dupA, err := s.Create(ctx, adminActor, "same-a", "same-value")
	if err != nil {
		t.Fatal(err)
	}
	dupB, err := s.Create(ctx, adminActor, "same-b", "same-value")
	if err != nil {
		t.Fatal(err)
	}
	cipherA, _ := readCiphertext(t, db, dupA.ID)
	cipherB, _ := readCiphertext(t, db, dupB.ID)
	if bytes.Equal(cipherA, cipherB) {
		t.Fatal("equal values produced equal ciphertexts")
	}

	// Ciphertext is bound to its row id: moving it to another secret makes it
	// undecryptable with that row's AAD (the rotation counter is untouched).
	if _, err = db.Exec(`UPDATE secrets SET ciphertext=$2 WHERE id=$1`, dupB.ID, cipherA); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(testSecretKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	moved, _ := readCiphertext(t, db, dupB.ID)
	if _, openErr := aead.Open(nil, moved[:aead.NonceSize()], moved[aead.NonceSize():], secretAAD(dupB.ID)); openErr == nil {
		t.Fatal("ciphertext opened under a different secret id")
	}

	// Audit rows carry id/name only and never a value.
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
		if strings.Contains(detail, testSecretValue) || strings.Contains(detail, "rotated-secret-value") || strings.Contains(detail, "same-value") {
			t.Fatalf("audit detail leaked a value: %q", detail)
		}
		if !strings.Contains(detail, "id=") || !strings.Contains(detail, "name=") {
			t.Fatalf("audit detail = %q", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen[ActionCreate] || !seen[ActionRotate] {
		t.Fatalf("audit actions = %+v", seen)
	}

	// Delete discards the row and its ciphertext.
	if err = s.Delete(ctx, adminActor, meta.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE id=$1`, meta.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("secret row survived delete: %d", count)
	}
	if list, _, err = s.List(ctx, adminActor, ListFilter{Limit: 20}); err != nil || len(list) != 2 {
		t.Fatalf("list after delete: %v len=%d", err, len(list))
	}
}

func TestValidationConflictsPermissionsAndPagination(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, adminActor, "unique-name", testSecretValue); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, adminActor, "unique-name", "other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}

	invalid := []struct{ name, value string }{
		{"", "v"},
		{"   ", "v"},
		{strings.Repeat("n", maxNameBytes+1), "v"},
		{"a\nb", "v"},
		{"ok", ""},
		{"ok", strings.Repeat("v", maxValueBytes+1)},
	}
	for _, tc := range invalid {
		if _, err := s.Create(ctx, adminActor, tc.name, tc.value); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid create %q/%d: %v", tc.name, len(tc.value), err)
		}
	}
	meta, err := s.Create(ctx, adminActor, strings.Repeat("n", maxNameBytes), strings.Repeat("v", maxValueBytes))
	if err != nil {
		t.Fatalf("boundary create: %v", err)
	}

	// Unknown/malformed ids and payload failures on rotation.
	if _, err := s.Rotate(ctx, adminActor, "short", "v"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid id: %v", err)
	}
	if _, err := s.Rotate(ctx, adminActor, "missing-0000000000001", "v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := s.Rotate(ctx, adminActor, meta.ID, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty rotation value: %v", err)
	}
	if err := s.Delete(ctx, adminActor, "missing-0000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v", err)
	}

	// Non-admin callers are refused by the service, not only by the routes.
	for _, action := range []func() error{
		func() error { _, err := s.Create(ctx, memberActor, "member-secret", "v"); return err },
		func() error { _, _, err := s.List(ctx, memberActor, ListFilter{Limit: 20}); return err },
		func() error { _, err := s.Rotate(ctx, memberActor, meta.ID, "v"); return err },
		func() error { return s.Delete(ctx, memberActor, meta.ID) },
	} {
		if err := action(); !errors.Is(err, ErrForbidden) {
			t.Fatalf("member access: %v", err)
		}
	}

	// Pagination bounds and page shape.
	for _, filter := range []ListFilter{{Limit: 0}, {Limit: 101}, {Limit: 20, Offset: -1}} {
		if _, _, err := s.List(ctx, adminActor, filter); !errors.Is(err, ErrValidation) {
			t.Fatalf("filter %+v: %v", filter, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO secrets(id,name,ciphertext,key_version,created_by,created_at,updated_at)
		VALUES('pagination-0000000001','page-a','',1,$1,9000000001,9000000001)`, adminActor.ID); err != nil {
		t.Fatalf("seed pagination row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO secrets(id,name,ciphertext,key_version,created_by,created_at,updated_at)
		VALUES('pagination-0000000002','page-b','',1,$1,9000000002,9000000002)`, adminActor.ID); err != nil {
		t.Fatal(err)
	}
	page, total, err := s.List(ctx, adminActor, ListFilter{Limit: 1, Offset: 0})
	if err != nil || total != 4 || len(page) != 1 || page[0].Name != "page-b" {
		t.Fatalf("page: %v total=%d page=%+v", err, total, page)
	}
}

func TestNotConfiguredKeepsMetadataAvailable(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	meta, err := s.Create(ctx, adminActor, "existing", testSecretValue)
	if err != nil {
		t.Fatal(err)
	}
	unconfigured := New(db, Options{})
	if _, err := unconfigured.Create(ctx, adminActor, "blocked", "value"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("create without key: %v", err)
	}
	if _, err := unconfigured.Rotate(ctx, adminActor, meta.ID, "value"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("rotate without key: %v", err)
	}
	// Metadata reads and deletion do not need the encryption key.
	if list, total, err := unconfigured.List(ctx, adminActor, ListFilter{Limit: 20}); err != nil || total != 1 || list[0].Name != "existing" {
		t.Fatalf("list without key: %v total=%d list=%+v", err, total, list)
	}
	if err := unconfigured.Delete(ctx, adminActor, meta.ID); err != nil {
		t.Fatalf("delete without key: %v", err)
	}
}
