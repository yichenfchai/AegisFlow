package deployments

import (
	"context"
	"errors"
)

// RekeyReport summarises one re-encryption pass. In a dry run Reencrypted
// counts the rows that would be written. In apply mode SkippedConcurrent counts
// rows whose ciphertext changed between the snapshot and the write: the rekey
// leaves those to the newer writer instead of overwriting it.
type RekeyReport struct {
	Scanned           int `json:"scanned"`
	AlreadyCurrent    int `json:"alreadyCurrent"`
	Reencrypted       int `json:"reencrypted"`
	SkippedConcurrent int `json:"skippedConcurrent"`
	Failed            int `json:"failed"`
}

// RekeySecrets re-seals every stored credential that still depends on the
// previous key with the current key. It never deletes or blanks a row: a
// credential that neither key can open is counted as failed and left
// untouched, so a wrong key can never destroy a stored secret. The pass is
// idempotent — rows already sealed with the current key are counted and
// skipped — and dryRun performs the same reads and decryptions without
// writing.
//
// The write is a compare-and-swap on the ciphertext that was read, so the
// command is safe to run while the API is serving: a credential replaced or
// cleared concurrently is never resurrected and is reported as
// SkippedConcurrent instead.
//
// The service must be built with both keys, exactly like the running API
// instance during a rotation; the caller then drops the previous key once the
// report stops finding work.
func (s *Service) RekeySecrets(ctx context.Context, dryRun bool) (RekeyReport, error) {
	var report RekeyReport
	if !s.secretReady() {
		return report, secretNotConfigured()
	}
	if len(s.previousKey) != 32 {
		return report, errors.New("deployment target secret rotation requires the previous key")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, secret_cipher FROM deployment_targets
		WHERE secret_cipher IS NOT NULL AND octet_length(secret_cipher) > 0 ORDER BY id`)
	if err != nil {
		return report, err
	}
	defer rows.Close()

	// Read the whole work list before writing: the query cursor holds a
	// connection, and a large rotation should not depend on pool sizing.
	type pendingRow struct {
		id     string
		sealed []byte
	}
	var work []pendingRow
	for rows.Next() {
		var id string
		var sealed []byte
		if err := rows.Scan(&id, &sealed); err != nil {
			return report, err
		}
		report.Scanned++
		if _, err := s.openWithKey(s.secretKey, id, sealed); err == nil {
			report.AlreadyCurrent++
			continue
		}
		if _, err := s.openWithKey(s.previousKey, id, sealed); err != nil {
			report.Failed++
			continue
		}
		work = append(work, pendingRow{id: id, sealed: sealed})
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	if err := rows.Close(); err != nil {
		return report, err
	}

	for _, row := range work {
		if s.rekeyBeforeWrite != nil {
			s.rekeyBeforeWrite(row.id)
		}
		reencrypted, err := s.reencryptTargetRow(ctx, row.id, row.sealed, dryRun)
		if err != nil {
			report.Failed++
			continue
		}
		if !reencrypted {
			report.SkippedConcurrent++
			continue
		}
		report.Reencrypted++
	}
	return report, nil
}

// reencryptTargetRow re-seals one row's credential with the current key. The
// write is a compare-and-swap against the ciphertext snapshot: the UPDATE only
// applies while the row still holds the exact value the snapshot reported, so
// a concurrent API write (replace or clear) wins instead of being overwritten
// by a stale snapshot. It reports false when the row changed concurrently; a
// dry run reports the row as re-encryptable without writing.
func (s *Service) reencryptTargetRow(ctx context.Context, targetID string, snapshot []byte, dryRun bool) (bool, error) {
	secret, err := s.openWithKey(s.previousKey, targetID, snapshot)
	if err != nil {
		return false, err
	}
	resealed, err := s.encryptSecret(targetID, secret)
	if err != nil {
		return false, err
	}
	if dryRun {
		return true, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE deployment_targets SET secret_cipher=$2 WHERE id=$1 AND secret_cipher=$3`,
		targetID, resealed, snapshot)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
