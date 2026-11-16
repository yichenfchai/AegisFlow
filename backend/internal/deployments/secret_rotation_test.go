package deployments

import (
	"context"
	"database/sql"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

// readCipher returns the stored ciphertext for one target.
func readCipher(t *testing.T, db *sql.DB, targetID string) []byte {
	t.Helper()
	var sealed []byte
	if err := db.QueryRow(`SELECT secret_cipher FROM deployment_targets WHERE id=$1`, targetID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	return sealed
}

// TestSecretRotationKeepsTargetsReadable covers the read half of a key
// rotation: a target sealed with the retiring key must stay usable while the
// new key takes over every write.
func TestSecretRotationKeepsTargetsReadable(t *testing.T) {
	oldKey := []byte("0123456789abcdef0123456789abcdef")
	newKey := []byte("fedcba9876543210fedcba9876543210")
	ctx := context.Background()

	// The service that sealed the credential, before the rotation.
	before, db := testServiceWith(t, Options{SecretKey: oldKey})
	target, err := before.Create(ctx, testActor, Input{Name: "rotating-target", Endpoint: "https://example.com", Secret: "rotation-token"})
	if err != nil {
		t.Fatal(err)
	}
	sealed := readCipher(t, db, target.ID)

	// During the rotation both keys are configured: reads fall back, writes
	// already use the new key.
	rotated := New(testutil.ClonePostgres(t, db), Options{SecretKey: newKey, PreviousSecretKey: oldKey})
	if plain, err := rotated.decryptSecret(target.ID, sealed); err != nil || plain != "rotation-token" {
		t.Fatalf("decrypt with the previous key = %q err=%v", plain, err)
	}
	// The previous key does not break the AAD binding.
	if _, err := rotated.decryptSecret("other-0000000000000001", sealed); err == nil {
		t.Fatal("ciphertext decrypted under a different target id")
	}
	// Dropping the previous key before the data is rewritten makes the old
	// ciphertext unreadable, which is exactly why the rotation needs both.
	strict := New(testutil.ClonePostgres(t, db), Options{SecretKey: newKey})
	if _, err := strict.decryptSecret(target.ID, sealed); err == nil {
		t.Fatal("the new key alone decrypted a ciphertext sealed with the old key")
	}

	// A rewrite after the rotation seals with the new key.
	if _, err := rotated.Update(ctx, testActor, target.ID, Patch{Secret: str("post-rotation")}); err != nil {
		t.Fatal(err)
	}
	rewritten := readCipher(t, db, target.ID)
	if _, err := before.decryptSecret(target.ID, rewritten); err == nil {
		t.Fatal("the old key alone decrypted a ciphertext sealed after the rotation")
	}
	if plain, err := rotated.decryptSecret(target.ID, rewritten); err != nil || plain != "post-rotation" {
		t.Fatalf("rewritten secret = %q err=%v", plain, err)
	}
}
