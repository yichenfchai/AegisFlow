package deployments

import (
	"context"
	"errors"
)

// RekeyReport summarises one re-encryption pass. In a dry run Reencrypted
// counts the rows that would be written.
type RekeyReport struct {
	Scanned        int `json:"scanned"`
	AlreadyCurrent int `json:"alreadyCurrent"`
	Reencrypted    int `json:"reencrypted"`
	Failed         int `json:"failed"`
}

// RekeySecrets re-seals every stored credential that still depends on the
// previous key with the current key. It never deletes or blanks a row: a
// credential that neither key can open is counted as failed and left
// untouched, so a wrong key can never destroy a stored secret. The pass is
// idempotent — rows already sealed with the current key are counted and
// skipped — and dryRun performs the same reads and decryptions without
// writing.
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
		secret, err := s.openWithKey(s.previousKey, row.id, row.sealed)
		if err != nil {
			report.Failed++
			continue
		}
		resealed, err := s.encryptSecret(row.id, secret)
		if err != nil {
			report.Failed++
			continue
		}
		if dryRun {
			report.Reencrypted++
			continue
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE deployment_targets SET secret_cipher=$2 WHERE id=$1`, row.id, resealed); err != nil {
			report.Failed++
			continue
		}
		report.Reencrypted++
	}
	return report, nil
}
