package testutil

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsDeadlock(t *testing.T) {
	if !isDeadlock(&pgconn.PgError{Code: "40P01"}) {
		t.Fatal("deadlock SQLSTATE not recognised")
	}
	if isDeadlock(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("unique violation treated as a deadlock")
	}
	if isDeadlock(errors.New("connection reset")) {
		t.Fatal("generic error treated as a deadlock")
	}
	if isDeadlock(nil) {
		t.Fatal("nil treated as a deadlock")
	}
}
