package builds

import (
	"context"
	"database/sql"
	"errors"
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

// defaultLeaseProbeInterval is how often the holder verifies that its dedicated
// connection still lives. The advisory lock disappears with the connection, so
// a lost connection means a standby replica may already be executing builds;
// the old holder must stop claiming instead of racing it.
const defaultLeaseProbeInterval = 5 * time.Second

// leaseProbeInterval is the holder liveness check period. A variable so tests
// can shrink the interval instead of waiting five seconds per assertion.
var leaseProbeInterval = defaultLeaseProbeInterval

// leaseProbeTimeout bounds one liveness probe: a blackholed TCP peer can
// otherwise leave the check blocked past the next interval.
const leaseProbeTimeout = 3 * time.Second

// WorkerLease is the single-worker lock of one deployment. The advisory lock
// lives on one dedicated connection, so it disappears with the process or the
// connection and a standby replica can take over; no lease table or heartbeat
// is needed. The holder periodically verifies that connection (Alive) and
// returns to stand-by when it fails. It does not make execution idempotent: a
// build that survives a connection loss is killed by the holder's status
// watchdog once the row is recovered by the next holder.
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

// Alive reports whether the dedicated lease connection is still usable. The
// advisory lock is bound to that session: when the ping fails, the lock is
// either already gone or about to be, so the caller must stop acting as the
// worker and let a standby replica take over.
func (l *WorkerLease) Alive(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return errors.New("build worker lease is not held")
	}
	return l.conn.PingContext(ctx)
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
// ctx is cancelled or the lease connection dies, always returning the lease.
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
	// A long-running build must not outlive its lease. The advisory lock is
	// released the moment the dedicated connection dies, so a stale holder that
	// kept executing would race the standby that just took over. Probe the
	// connection and cancel the worker loop on the first failure.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		interval := leaseProbeInterval
		if interval <= 0 {
			interval = defaultLeaseProbeInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				probeCtx, probeCancel := context.WithTimeout(runCtx, leaseProbeTimeout)
				probeErr := lease.Alive(probeCtx)
				probeCancel()
				if probeErr == nil {
					continue
				}
				if runCtx.Err() == nil {
					logger.Warn("build worker lease lost; returning to stand-by", "error", probeErr)
					cancel()
				}
				return
			}
		}
	}()
	s.run(runCtx)
	cancel()
	<-probeDone
}
