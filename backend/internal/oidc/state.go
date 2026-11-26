package oidc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrStateNotFound means the state row is missing, expired or already
// consumed. The three cases are deliberately indistinguishable to the caller.
var ErrStateNotFound = errors.New("OIDC login state not found, expired or already consumed")

// LoginState is the server-side half of one authorization request.
type LoginState struct {
	Nonce        string
	CodeVerifier string
	RedirectTo   string
}

// StateStore persists one-time authorization requests. It deliberately has no
// Gin dependency; the HTTP layer only passes the hashed state and receives the
// nonce/verifier/redirect target on consumption.
type StateStore struct {
	db *sql.DB
}

// Create inserts a fresh state row. It also opportunistically deletes expired
// rows: only a successful /start writes here, and that route is behind the
// shared login rate limiter, so this cannot become a write amplification path.
// Cleanup failure is not fatal; an expired row is still rejected by Consume.
func (s *StateStore) Create(ctx context.Context, stateHash, nonce, codeVerifier, redirectTo string, expiresAt, now int64) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO oidc_login_states(state_hash,nonce,code_verifier,redirect_to,expires_at,created_at)
	 VALUES($1,$2,$3,$4,$5,$6)`, stateHash, nonce, codeVerifier, redirectTo, expiresAt, now); err != nil {
		return fmt.Errorf("create OIDC login state: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM oidc_login_states WHERE expires_at < $1`, now)
	return nil
}

// Consume atomically marks a state row used and returns its payload. The
// single UPDATE ... RETURNING is the concurrency control: a replayed callback
// finds consumed_at already set and gets ErrStateNotFound, so it can never mint
// a second session even when two callbacks race.
func (s *StateStore) Consume(ctx context.Context, stateHash string, now int64) (*LoginState, error) {
	var state LoginState
	err := s.db.QueryRowContext(ctx, `UPDATE oidc_login_states SET consumed_at=$2
	 WHERE state_hash=$1 AND consumed_at IS NULL AND expires_at > $2
	 RETURNING nonce, code_verifier, redirect_to`, stateHash, now).Scan(&state.Nonce, &state.CodeVerifier, &state.RedirectTo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("consume OIDC login state: %w", err)
	}
	return &state, nil
}
