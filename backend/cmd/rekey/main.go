// Command rekey re-seals deployment-target credentials with the current
// AEGIS_SECRET_KEY after a rotation.
//
// It is a maintenance command, not a server: run it while the API still has
// AEGIS_SECRET_KEY_PREVIOUS configured, check the dry-run report, then repeat
// with -apply. Rows that neither key can open are reported and left untouched.
// Once the report shows nothing left to re-encrypt, the previous key can be
// removed from the service configuration.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/yichenfchai/AegisFlow/backend/internal/deployments"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rekey:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("rekey", flag.ContinueOnError)
	apply := flags.Bool("apply", false, "write the re-encrypted rows (default is a dry run)")
	timeout := flags.Duration("timeout", 10*time.Minute, "overall deadline for the pass")
	if err := flags.Parse(args); err != nil {
		return err
	}

	databaseURL := strings.TrimSpace(os.Getenv("AEGIS_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("AEGIS_DATABASE_URL is required")
	}
	current, err := readKey("AEGIS_SECRET_KEY")
	if err != nil {
		return err
	}
	previous, err := readKey("AEGIS_SECRET_KEY_PREVIOUS")
	if err != nil {
		return err
	}
	if bytes.Equal(current, previous) {
		return errors.New("AEGIS_SECRET_KEY_PREVIOUS must differ from AEGIS_SECRET_KEY")
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("invalid PostgreSQL connection configuration")
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	service := deployments.New(db, deployments.Options{SecretKey: current, PreviousSecretKey: previous})
	report, err := service.RekeySecrets(ctx, !*apply)
	if err != nil {
		return err
	}

	mode := "dry run"
	if *apply {
		mode = "applied"
	}
	fmt.Printf("%s: scanned=%d already_current=%d reencrypted=%d failed=%d\n",
		mode, report.Scanned, report.AlreadyCurrent, report.Reencrypted, report.Failed)
	if !*apply && report.Reencrypted > 0 {
		fmt.Println("nothing was written; re-run with -apply to re-encrypt these rows")
	}
	if report.Failed > 0 {
		return fmt.Errorf("%d credentials could not be opened with either key and were left untouched", report.Failed)
	}
	return nil
}

// readKey decodes one base64 key without echoing its value in an error.
func readKey(name string) ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("%s must be base64 of exactly 32 bytes", name)
	}
	return decoded, nil
}
