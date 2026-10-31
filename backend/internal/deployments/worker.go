package deployments

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// deployPayload is the JSON body of the deploy POST (design §4). For a rollback
// deployment version and rollbackVersion are the version being restored;
// otherwise rollback is false and rollbackVersion is empty.
type deployPayload struct {
	DeploymentID    string `json:"deploymentId"`
	Project         string `json:"project"`
	Version         string `json:"version"`
	Environment     string `json:"environment"`
	Rollback        bool   `json:"rollback"`
	RollbackVersion string `json:"rollbackVersion"`
}

// executionTarget is the live target configuration used for one execution.
// The deployment row already holds the trigger-time snapshot for history; the
// request itself uses the current endpoint/deploy_path/secret so an operational
// fix applies before the worker claims the row.
type executionTarget struct {
	Endpoint   string
	DeployPath string
	Status     string
	Sealed     []byte
}

// run is the in-process worker loop: it waits for a wake signal (or a slow
// poll), claims the oldest queued deployment with FOR UPDATE SKIP LOCKED and
// executes it. The lease holder also expires overdue approvals every 30s (and
// once at takeover); that scan shares this loop, so it can never outlive the
// lease. Only the lease holder may call it (see RunWorker).
func (s *Service) run(ctx context.Context, logger *slog.Logger) {
	claimTicker := time.NewTicker(2 * time.Second)
	defer claimTicker.Stop()
	approvalTicker := time.NewTicker(defaultApprovalScanInterval)
	defer approvalTicker.Stop()
	s.expireApprovals(ctx, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.notify:
		case <-claimTicker.C:
		case <-approvalTicker.C:
			s.expireApprovals(ctx, logger)
		}
		for {
			deployment, err := s.claimNext(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					time.Sleep(time.Second)
				}
				break
			}
			s.execute(ctx, deployment)
		}
	}
}

// expireApprovals runs one bounded expiry scan and logs a non-zero count.
func (s *Service) expireApprovals(ctx context.Context, logger *slog.Logger) {
	expired, err := s.ExpireApprovals(ctx, time.Now().Unix())
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("approval expiry scan failed", "error", err)
		}
		return
	}
	if expired > 0 {
		logger.Info("expired approvals", "count", expired)
	}
}

// claimNext atomically flips the oldest queued deployment to running. The
// RETURNING list is qualified because the CTE also exposes an id column.
func (s *Service) claimNext(ctx context.Context) (Deployment, error) {
	deployment, err := scanDeployment(s.db.QueryRowContext(ctx, `
	WITH next AS (SELECT id FROM deployments WHERE status=$1 ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
	UPDATE deployments d SET status=$2, started_at=$3 FROM next WHERE d.id=next.id
	RETURNING d.id,d.project_id,d.target_id,d.target_name,d.target_endpoint,d.target_environment,d.deployment_number,d.status,d.version,d.build_id,d.rollback_of,d.approval_id,d.triggered_by,d.detail,d.created_at,d.started_at,d.finished_at`,
		DeployQueued, DeployRunning, time.Now().Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, sql.ErrNoRows
	}
	return deployment, err
}

// execute runs one claimed deployment to a terminal state.
func (s *Service) execute(ctx context.Context, deployment Deployment) {
	execCtx, stopTimeout := context.WithTimeout(ctx, s.deployTimeout)
	defer stopTimeout()
	runCtx, cancelRun := context.WithCancel(execCtx)
	defer cancelRun()

	writer := newDeploymentEventWriter(s, deployment.ID)
	defer writer.close()
	if deployment.RollbackOf != "" {
		writer.systemf("deployment #%d started (target=%q version=%q rollback_of=%s)",
			deployment.DeploymentNumber, deployment.TargetName, deployment.Version, deployment.RollbackOf)
	} else {
		writer.systemf("deployment #%d started (target=%q version=%q)",
			deployment.DeploymentNumber, deployment.TargetName, deployment.Version)
	}

	target, err := s.executionTargetFor(execCtx, deployment.TargetID)
	if err != nil {
		writer.systemf("deployment target unavailable")
		s.finish(deployment, DeployFailed, "deployment target unavailable")
		return
	}
	if target.Status != StatusActive {
		writer.systemf("deployment target is %s", target.Status)
		s.finish(deployment, DeployFailed, "deployment target is disabled")
		return
	}
	secret, err := s.decryptSecret(deployment.TargetID, target.Sealed)
	if err != nil {
		// Never include the decrypt error: it could describe key material.
		writer.systemf("deployment target credential unavailable")
		s.finish(deployment, DeployFailed, "deployment target credential unavailable")
		return
	}
	projectName, err := s.projectName(execCtx, deployment.ProjectID)
	if err != nil {
		writer.systemf("deployment project unavailable")
		s.finish(deployment, DeployFailed, "deployment project unavailable")
		return
	}
	payload := deployPayload{
		DeploymentID: deployment.ID,
		Project:      projectName,
		Version:      deployment.Version,
		Environment:  deployment.TargetEnvironment,
		Rollback:     deployment.RollbackOf != "",
	}
	if payload.Rollback {
		payload.RollbackVersion = deployment.Version
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writer.systemf("deployment request could not be encoded")
		s.finish(deployment, DeployFailed, "request encoding failed")
		return
	}
	targetURL := strings.TrimRight(target.Endpoint, "/") + target.DeployPath
	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		writer.systemf("deployment request could not be built")
		s.finish(deployment, DeployFailed, "request build failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "AegisFlow-deployment")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	// The watchdog aborts the HTTP request when an operator cancels or the row
	// leaves running for any other reason (design §4, 300ms like builds).
	watchDone := make(chan struct{})
	var watchOnce sync.Once
	stopWatch := func() { watchOnce.Do(func() { close(watchDone) }) }
	defer stopWatch()
	go s.watchDeployment(runCtx, deployment.ID, cancelRun, watchDone)

	start := time.Now()
	resp, err := s.httpClient.Do(req)
	stopWatch()
	if err != nil {
		if s.deploymentCancelled(deployment.ID) {
			writer.systemf("deployment cancelled")
			return
		}
		switch {
		case ctx.Err() != nil:
			writer.systemf("interrupted by server shutdown")
			s.finish(deployment, DeployInterrupted, "interrupted by server shutdown")
		case execCtx.Err() == context.DeadlineExceeded:
			writer.systemf("deployment timed out after %s", s.deployTimeout)
			s.finish(deployment, DeployFailed, "timeout")
		default:
			detail := describeProbeError(execCtx, err, targetURL, secret)
			writer.systemf("deployment request failed: %s", detail)
			s.finish(deployment, DeployFailed, detail)
		}
		return
	}
	defer resp.Body.Close()
	scanErr := writer.stream(resp.Body)
	if scanErr != nil {
		// A read error after a status line is not a deployment failure by
		// itself: the upstream already answered. Record the truncation and
		// let the status code decide the outcome.
		writer.systemf("response read stopped: %s", sanitizeDetail(scanErr.Error(), targetURL, secret))
	}

	if s.deploymentCancelled(deployment.ID) {
		writer.systemf("deployment cancelled")
		return
	}
	if ctx.Err() != nil {
		writer.systemf("interrupted by server shutdown")
		s.finish(deployment, DeployInterrupted, "interrupted by server shutdown")
		return
	}
	if execCtx.Err() == context.DeadlineExceeded {
		writer.systemf("deployment timed out after %s", s.deployTimeout)
		s.finish(deployment, DeployFailed, "timeout")
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		writer.systemf("deployment succeeded (http %d, %dms)", resp.StatusCode, time.Since(start).Milliseconds())
		s.finish(deployment, DeploySucceeded, "")
		return
	}
	detail := fmt.Sprintf("http %d", resp.StatusCode)
	writer.systemf("deployment failed: %s", detail)
	s.finish(deployment, DeployFailed, detail)
}

// watchDeployment cancels the in-flight request once the row leaves running.
func (s *Service) watchDeployment(ctx context.Context, id string, cancel context.CancelFunc, done <-chan struct{}) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := s.getDeploymentByID(context.Background(), id)
			if err != nil || current.Status != DeployRunning {
				cancel()
				return
			}
		}
	}
}

// finish writes the terminal state; it uses a fresh context so the final update
// lands even during shutdown. The WHERE clause leaves a cancelled row alone.
func (s *Service) finish(deployment Deployment, status, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, `UPDATE deployments SET status=$2, finished_at=$3, detail=$4 WHERE id=$1 AND status=$5`,
		deployment.ID, status, time.Now().Unix(), detail, DeployRunning)
}

// RecoverInterrupted marks running deployments interrupted at lease takeover.
func (s *Service) RecoverInterrupted(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE deployments SET status=$1, finished_at=$2, detail=$3 WHERE status=$4`,
		DeployInterrupted, time.Now().Unix(), "interrupted by server restart", DeployRunning)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Service) getDeploymentByID(ctx context.Context, id string) (Deployment, error) {
	deployment, err := scanDeployment(s.db.QueryRowContext(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	return deployment, nil
}

func (s *Service) deploymentCancelled(id string) bool {
	current, err := s.getDeploymentByID(context.Background(), id)
	return err == nil && current.Status == DeployCancelled
}

func (s *Service) executionTargetFor(ctx context.Context, id string) (executionTarget, error) {
	var target executionTarget
	err := s.db.QueryRowContext(ctx, `SELECT endpoint,deploy_path,status,secret_cipher FROM deployment_targets WHERE id=$1`, id).
		Scan(&target.Endpoint, &target.DeployPath, &target.Status, &target.Sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return executionTarget{}, ErrNotFound
	}
	if err != nil {
		return executionTarget{}, err
	}
	return target, nil
}

func (s *Service) projectName(ctx context.Context, id string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM projects WHERE id=$1`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}
