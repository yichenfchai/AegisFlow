package deployments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

// Check probes the target over HTTP(S) and records the outcome. A stored secret
// is decrypted for this request only and sent as a Bearer Authorization header;
// it never appears in the result, the audit detail or a log line. 2xx is
// healthy; any other status, a timeout or a network error is not.
func (s *Service) Check(ctx context.Context, actor auth.Actor, id string) (CheckResult, error) {
	if !auth.ValidID(id) {
		return CheckResult{}, fmt.Errorf("%w: invalid deployment target id", ErrValidation)
	}
	var endpoint, healthPath string
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT endpoint,health_path,secret_cipher FROM deployment_targets WHERE id=$1`, id).Scan(&endpoint, &healthPath, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckResult{}, ErrNotFound
	}
	if err != nil {
		return CheckResult{}, err
	}
	secret, err := s.decryptSecret(id, sealed)
	if err != nil {
		return CheckResult{}, err
	}

	result := s.probe(ctx, endpoint, healthPath, secret)
	result.CheckedAt = time.Now().Unix()

	lastCheckOK := 0
	if result.OK {
		lastCheckOK = 1
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return CheckResult{}, err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE deployment_targets SET last_check_at=$2,last_check_ok=$3,last_check_detail=$4 WHERE id=$1`,
		id, result.CheckedAt, lastCheckOK, result.Detail)
	if err != nil {
		return CheckResult{}, err
	}
	if n, err := updated.RowsAffected(); err != nil {
		return CheckResult{}, err
	} else if n == 0 {
		return CheckResult{}, ErrNotFound
	}
	if err = writeAudit(ctx, tx, actor, ActionCheck, id, fmt.Sprintf("status=%d %s", result.StatusCode, result.Detail)); err != nil {
		return CheckResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return CheckResult{}, err
	}
	return result, nil
}

// probe performs one timed GET. It is deliberately free of business state so a
// slow upstream can never hold a database transaction open.
func (s *Service) probe(ctx context.Context, endpoint, healthPath, secret string) CheckResult {
	probeCtx, cancel := context.WithTimeout(ctx, s.checkTimeout)
	defer cancel()
	targetURL := strings.TrimRight(endpoint, "/") + healthPath
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return CheckResult{Detail: "network error: invalid request"}
	}
	req.Header.Set("User-Agent", "AegisFlow-deployment-check")
	req.Header.Set("Accept", "*/*")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	start := time.Now()
	resp, err := s.httpClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return CheckResult{LatencyMS: latency, Detail: describeProbeError(probeCtx, err, endpoint, secret)}
	}
	defer resp.Body.Close()
	// Drain a bounded prefix so the connection can be reused without buffering
	// an arbitrarily large upstream body.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	result := CheckResult{LatencyMS: latency, StatusCode: resp.StatusCode}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result.OK = true
		result.Detail = "ok"
	} else {
		result.Detail = fmt.Sprintf("http %d", resp.StatusCode)
	}
	return result
}

// describeProbeError produces a short, sanitized detail: no URL that might carry
// credentials, no secret, no control characters.
func describeProbeError(ctx context.Context, err error, endpoint, secret string) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	message := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		message = urlErr.Err.Error()
	}
	message = sanitizeDetail(message, endpoint, secret)
	if message == "" {
		message = "request failed"
	}
	return "network error: " + message
}

func sanitizeDetail(message, endpoint, secret string) string {
	if endpoint != "" {
		message = strings.ReplaceAll(message, endpoint, "<endpoint>")
	}
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 120 {
		cut := 120
		for cut > 0 && message[cut]&0xC0 == 0x80 {
			cut--
		}
		message = message[:cut] + "…"
	}
	return message
}
