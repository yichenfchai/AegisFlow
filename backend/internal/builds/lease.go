package builds

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// workerLeaseQuery takes a session-scoped advisory lock keyed by database and
// schema, so replicas of the same deployment contend for one lease while
// independent test schemas stay isolated. Renaming the key is not compatible
// with running older replicas, which would then run two workers.
const workerLeaseQuery = `SELECT pg_try_advisory_lock(hashtextextended(current_database() || ':' || current_schema() || ':aegisflow:builds:worker', 0))`

// defaultLeaseRetry is how long a standby instance waits before retrying the
// lease. It bounds takeover latency after the holder dies.
const defaultLeaseRetry = 5 * time.Second

// WorkerLease is the single-worker lock of one deployment. The advisory lock
// lives on one dedicated connection, so it disappears with the process or the
// connection and a standby replica can take over; no lease table or heartbeat
// is needed. It does not make execution idempotent: a build that survives a
// connection loss is killed by the holder's status watchdog once the row is
// recovered by the next holder.
type WorkerLease struct {
	conn *sql.Conn
	once sync.Once
}

// AcquireWorkerLease tries once to become the worker. held is false when
// another instance owns the lease; that is a normal standby state, not an
// error. Callers must Release a held lease.
func (s *Service) AcquireWorkerLease(ctx context.Context) (lease *WorkerLease, held bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, workerLeaseQuery).Scan(&ok); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !ok {
		_ = conn.Close()
		return nil, false, nil
	}
	return &WorkerLease{conn: conn}, true, nil
}

// Release unlocks and returns the dedicated connection. It is safe to call
// more than once and does nothing on a nil lease.
func (l *WorkerLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// The connection only ever holds the worker lock, so unlock_all is
		// exact here; closing the connection would drop the lock anyway.
		_, _ = l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock_all()`)
		_ = l.conn.Close()
	})
}

// RunWorker owns this instance's build worker until ctx is cancelled.
//
// Only the lease holder calls RecoverInterrupted and claims queued builds.
// Without that rule a second replica starting up would mark the first
// replica's running builds interrupted. Standby replicas retry the lease and
// take over when the holder stops; claiming already uses SKIP LOCKED as a
// second line of defence.
func (s *Service) RunWorker(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	retry := s.leaseRetry
	if retry <= 0 {
		retry = defaultLeaseRetry
	}
	standbyLogged := false
	for {
		if ctx.Err() != nil {
			return
		}
		lease, held, err := s.AcquireWorkerLease(ctx)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				logger.Error("build worker lease attempt failed", "error", err)
			}
		case held:
			standbyLogged = false
			logger.Info("build worker lease acquired")
			s.runAsLeaseHolder(ctx, lease, logger)
		default:
			if !standbyLogged {
				logger.Info("build worker on stand-by: another instance holds the lease")
				standbyLogged = true
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// runAsLeaseHolder recovers interrupted builds and then runs the worker until
// ctx is cancelled, always returning the lease.
func (s *Service) runAsLeaseHolder(ctx context.Context, lease *WorkerLease, logger *slog.Logger) {
	defer lease.Release()
	recovered, err := s.RecoverInterrupted(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("interrupted build recovery failed", "error", err)
		}
		return
	}
	if recovered > 0 {
		logger.Info("interrupted builds recovered", "count", recovered)
	}
	s.run(ctx)
}
