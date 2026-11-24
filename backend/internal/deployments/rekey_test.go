package deployments

import (
	"bytes"
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// seedRekeyTarget creates one target with its own credential through the
// service that owns key, so the row is sealed with that key.
func seedRekeyTarget(t *testing.T, db *sql.DB, key []byte, name string) Target {
	t.Helper()
	service := New(testutil.ClonePostgres(t, db), Options{SecretKey: key})
	target, err := service.Create(context.Background(), testActor, Input{Name: name, Endpoint: "https://example.com", Secret: name + "-secret"})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func updatedAt(t *testing.T, db *sql.DB, targetID string) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(`SELECT updated_at FROM deployment_targets WHERE id=$1`, targetID).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRekeySecretsDryRunApplyAndIdempotency(t *testing.T) {
	oldKey := []byte("0123456789abcdef0123456789abcdef")
	newKey := []byte("fedcba9876543210fedcba9876543210")
	strangerKey := []byte("aaaabbbbccccddddeeeeffff00001111")

	// The schema is created by the first service; every other service below
	// uses an independent pool on the same schema.
	_, db := testServiceWith(t, Options{SecretKey: oldKey})
	ctx := context.Background()
	rotating := seedRekeyTarget(t, db, oldKey, "rekey-old")
	currentRow := seedRekeyTarget(t, db, newKey, "rekey-current")
	stranger := seedRekeyTarget(t, db, strangerKey, "rekey-stranger")

	oldCipher := readCipher(t, db, rotating.ID)
	oldUpdatedAt := updatedAt(t, db, rotating.ID)

	rotation := New(testutil.ClonePostgres(t, db), Options{SecretKey: newKey, PreviousSecretKey: oldKey})

	// A dry run reports the work without touching a single row.
	report, err := rotation.RekeySecrets(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 3 || report.AlreadyCurrent != 1 || report.Reencrypted != 1 || report.Failed != 1 {
		t.Fatalf("dry run report = %+v", report)
	}
	if !bytes.Equal(readCipher(t, db, rotating.ID), oldCipher) {
		t.Fatal("dry run rewrote a ciphertext")
	}

	// Apply re-seals only the row that still depends on the old key.
	report, err = rotation.RekeySecrets(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 3 || report.AlreadyCurrent != 1 || report.Reencrypted != 1 || report.Failed != 1 {
		t.Fatalf("apply report = %+v", report)
	}
	if bytes.Equal(readCipher(t, db, rotating.ID), oldCipher) {
		t.Fatal("apply left the old ciphertext in place")
	}
	if got := updatedAt(t, db, rotating.ID); got != oldUpdatedAt {
		t.Fatalf("updated_at changed from %d to %d; a rekey is not a configuration edit", oldUpdatedAt, got)
	}
	// The re-encrypted row no longer needs the previous key, while the row
	// sealed with the stranger key still needs a key this service never had.
	strict := New(testutil.ClonePostgres(t, db), Options{SecretKey: newKey})
	if plain, err := strict.decryptSecret(rotating.ID, readCipher(t, db, rotating.ID)); err != nil || plain != "rekey-old-secret" {
		t.Fatalf("re-encrypted row = %q err=%v", plain, err)
	}
	if _, err := rotation.decryptSecret(stranger.ID, readCipher(t, db, stranger.ID)); err == nil {
		t.Fatal("a foreign key was decrypted")
	}
	if plain, err := rotation.decryptSecret(currentRow.ID, readCipher(t, db, currentRow.ID)); err != nil || plain != "rekey-current-secret" {
		t.Fatalf("already-current row = %q err=%v", plain, err)
	}

	// Running again is a no-op apart from the row nobody can open.
	report, err = rotation.RekeySecrets(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 3 || report.AlreadyCurrent != 2 || report.Reencrypted != 0 || report.Failed != 1 {
		t.Fatalf("second apply report = %+v", report)
	}
}

func TestRekeySecretsRequiresBothKeys(t *testing.T) {
	_, db := testServiceWith(t, Options{SecretKey: []byte("0123456789abcdef0123456789abcdef")})
	ctx := context.Background()

	if _, err := New(testutil.ClonePostgres(t, db), Options{SecretKey: []byte("0123456789abcdef0123456789abcdef")}).RekeySecrets(ctx, true); err == nil {
		t.Fatal("rekey without a previous key was allowed")
	}
	if _, err := New(testutil.ClonePostgres(t, db), Options{PreviousSecretKey: []byte("0123456789abcdef0123456789abcdef")}).RekeySecrets(ctx, true); err == nil {
		t.Fatal("rekey without a current key was allowed")
	}
}

// TestRekeyRowCompareAndSwapLeavesConcurrentChange regression: the rekey pass
// used to UPDATE each row unconditionally after taking a full snapshot, so a
// credential replaced or cleared by the online API between the snapshot and the
// write was silently overwritten with the stale plaintext (or resurrected). The
// per-row write is now a compare-and-swap on the snapshot ciphertext.
func TestRekeyRowCompareAndSwapLeavesConcurrentChange(t *testing.T) {
	oldKey := []byte("0123456789abcdef0123456789abcdef")
	newKey := []byte("fedcba9876543210fedcba9876543210")
	ctx := context.Background()

	_, db := testServiceWith(t, Options{SecretKey: oldKey})
	replaced := seedRekeyTarget(t, db, oldKey, "rekey-replaced")
	cleared := seedRekeyTarget(t, db, oldKey, "rekey-cleared")
	untouched := seedRekeyTarget(t, db, oldKey, "rekey-untouched")

	// During a rotation the API already seals writes with the current key.
	rotation := New(testutil.ClonePostgres(t, db), Options{SecretKey: newKey, PreviousSecretKey: oldKey})
	api := New(testutil.ClonePostgres(t, db), Options{SecretKey: newKey, PreviousSecretKey: oldKey})

	// The pass reads its snapshot first; the concurrent API writes then land.
	replacedSnapshot := readCipher(t, db, replaced.ID)
	clearedSnapshot := readCipher(t, db, cleared.ID)
	untouchedSnapshot := readCipher(t, db, untouched.ID)
	if _, err := api.Update(ctx, testActor, replaced.ID, Patch{Secret: str("concurrent-replacement")}); err != nil {
		t.Fatal(err)
	}
	replacedCipher := readCipher(t, db, replaced.ID)
	if _, err := api.Update(ctx, testActor, cleared.ID, Patch{Secret: str("")}); err != nil {
		t.Fatal(err)
	}

	if reencrypted, err := rotation.reencryptTargetRow(ctx, replaced.ID, replacedSnapshot, false); err != nil {
		t.Fatal(err)
	} else if reencrypted {
		t.Fatal("rekey overwrote a ciphertext replaced after the snapshot")
	}
	if !bytes.Equal(readCipher(t, db, replaced.ID), replacedCipher) {
		t.Fatal("concurrent credential replacement was overwritten with the snapshot")
	}
	if plain, err := rotation.decryptSecret(replaced.ID, readCipher(t, db, replaced.ID)); err != nil || plain != "concurrent-replacement" {
		t.Fatalf("stored credential = %q err=%v", plain, err)
	}

	if reencrypted, err := rotation.reencryptTargetRow(ctx, cleared.ID, clearedSnapshot, false); err != nil {
		t.Fatal(err)
	} else if reencrypted {
		t.Fatal("rekey raced with a credential clear")
	}
	if cipher := readCipher(t, db, cleared.ID); len(cipher) != 0 {
		t.Fatalf("cleared credential was resurrected with %d bytes", len(cipher))
	}

	// An unchanged row is still re-encrypted, so the compare-and-swap does not
	// skip the normal rotation work.
	if reencrypted, err := rotation.reencryptTargetRow(ctx, untouched.ID, untouchedSnapshot, false); err != nil {
		t.Fatal(err)
	} else if !reencrypted {
		t.Fatal("rekey refused an unchanged row")
	}
	if plain, err := rotation.decryptSecret(untouched.ID, readCipher(t, db, untouched.ID)); err != nil || plain != "rekey-untouched-secret" {
		t.Fatalf("re-encrypted credential = %q err=%v", plain, err)
	}
}

// TestRekeyReportCountsConcurrentSkip checks the report wiring: a row that
// changes between the snapshot and the write is counted once as
// SkippedConcurrent, not as reencrypted and not as failed.
func TestRekeyReportCountsConcurrentSkip(t *testing.T) {
	oldKey := []byte("0123456789abcdef0123456789abcdef")
	newKey := []byte("fedcba9876543210fedcba9876543210")
	ctx := context.Background()

	_, db := testServiceWith(t, Options{SecretKey: oldKey})
	raced := seedRekeyTarget(t, db, oldKey, "rekey-raced")
	rotation := New(testutil.ClonePostgres(t, db), Options{SecretKey: newKey, PreviousSecretKey: oldKey})
	api := New(testutil.ClonePostgres(t, db), Options{SecretKey: newKey, PreviousSecretKey: oldKey})

	var once sync.Once
	rotation.rekeyBeforeWrite = func(targetID string) {
		once.Do(func() {
			if _, err := api.Update(ctx, testActor, targetID, Patch{Secret: str("api-concurrent")}); err != nil {
				t.Errorf("concurrent API write: %v", err)
			}
		})
	}
	report, err := rotation.RekeySecrets(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 1 || report.AlreadyCurrent != 0 || report.Reencrypted != 0 || report.SkippedConcurrent != 1 || report.Failed != 0 {
		t.Fatalf("report = %+v", report)
	}
	if plain, err := rotation.decryptSecret(raced.ID, readCipher(t, db, raced.ID)); err != nil || plain != "api-concurrent" {
		t.Fatalf("stored credential = %q err=%v", plain, err)
	}
}
