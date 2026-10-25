package auth

import (
	"context"
	"database/sql"

	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
)

// Migrate applies the shared versioned schema (auth tables plus later module
// tables) under the same ledger and advisory lock; see the dbmigrate package.
func (s *Service) Migrate(ctx context.Context) error {
	if err := s.valid(); err != nil {
		return err
	}
	return dbmigrate.Apply(ctx, s.db)
}

// advisoryLock serializes runtime user-state operations (bootstrap, disable and
// role changes) across processes; migrations use dbmigrate.Lock.
func advisoryLock(ctx context.Context, tx *sql.Tx, operation string) error {
	return dbmigrate.Lock(ctx, tx, operation)
}
