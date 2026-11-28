// Command tenant is the operator-facing tenant management CLI. Phase F2 ships
// the only tenant-creation entry point: no HTTP API opens a second tenant, so the
// command is deliberately not a server. It reuses the shared database
// configuration (AEGIS_DATABASE_URL) and the same validation/hashing/audit code
// as the HTTP user API, so an operator cannot create something the service would
// reject.
//
// Commands:
//
//	create --slug <slug> --name <name> [--admin-email <email>] [--admin-password <password>]
//	create-admin --tenant <slug> --email <email> [--password <password>]
//	list
//	disable --tenant <slug>
//	enable --tenant <slug>
//
// Passwords passed as flags are visible in the process list and shell history;
// prefer letting the command generate one (printed once) or the platform's
// secret-injection wrapper. The generated password is printed to stdout exactly
// once and never written to a log or the audit trail. The command never runs
// migrations itself: it fails closed while the schema is older than this binary,
// so run the server (which migrates) first.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/config"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one CLI invocation. It returns the process exit code so tests
// can drive the real command without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "create":
		return runCreate(args[1:], stdout, stderr)
	case "create-admin":
		return runCreateAdmin(args[1:], stdout, stderr)
	case "list":
		return runList(args[1:], stdout, stderr)
	case "disable", "enable":
		return runSetStatus(args[0], args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "tenant: unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `tenant - AegisFlow tenant operations

Usage:
  tenant create --slug <slug> --name <name> [--admin-email <email>] [--admin-password <password>]
  tenant create-admin --tenant <slug> --email <email> [--password <password>]
  tenant list
  tenant disable --tenant <slug>
  tenant enable --tenant <slug>

The database connection is read from AEGIS_DATABASE_URL, the same setting the
server uses. The database must already be migrated by the server.

A generated initial password is printed to stdout exactly once and is never
logged or audited. Prefer it over --password/--admin-password, which are visible
in the process list and shell history.
`)
}

// commandContext bounds one CLI invocation; operations here are small.
func commandContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// openDatabase opens the shared connection configuration and refuses to run
// against a schema that is not at this binary's version: a CLI that writes a
// tenant into an old schema would create rows the server cannot read.
func openDatabase(ctx context.Context) (*sql.DB, error) {
	dsn, err := config.DatabaseURL()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("cannot connect to PostgreSQL: check AEGIS_DATABASE_URL")
	}
	var version int64
	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		db.Close()
		return nil, errors.New("database schema is not initialized; run the server once so it applies the migrations")
	}
	if version != int64(len(dbmigrate.Migrations)) {
		db.Close()
		return nil, fmt.Errorf("database schema is at version %d but this binary expects %d; run the server once to migrate", version, len(dbmigrate.Migrations))
	}
	return db, nil
}

// usernamePattern mirrors auth's username rule; the CLI derives a username from
// the admin email local part and must produce a value the account API accepts.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)

// deriveUsername turns an email local part into a valid username. Invalid
// characters become separators, a leading separator is dropped and the result is
// lowercased; an empty or too-short result falls back to "admin".
func deriveUsername(email string) string {
	local := email
	if at := strings.Index(email, "@"); at > 0 {
		local = email[:at]
	}
	var builder strings.Builder
	for _, r := range strings.ToLower(local) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	candidate := strings.TrimLeft(builder.String(), ".-_")
	if len(candidate) > 64 {
		candidate = candidate[:64]
	}
	if !usernamePattern.MatchString(candidate) {
		return "admin"
	}
	return candidate
}

// generatePassword returns a 24-byte random base64url password (32 characters),
// well inside the documented 12-72 byte range.
func generatePassword() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// resolvePassword uses the operator-provided password when set, otherwise
// generates one and reports it once. generated tells the caller to print the
// one-time notice; the password itself is never logged.
func resolvePassword(provided string) (password string, generated bool, err error) {
	if provided != "" {
		if len(provided) < 12 || len(provided) > 72 {
			return "", false, errors.New("password must be 12-72 bytes")
		}
		return provided, false, nil
	}
	password, err = generatePassword()
	return password, true, err
}

// provisionAdmin creates the admin account, suffixing the derived username when
// the tenant already holds it. The shared account path performs the validation,
// bcrypt hashing, per-tenant uniqueness check and the (credential-free) audit
// write, so this command cannot diverge from the HTTP user API.
func provisionAdmin(ctx context.Context, db *sql.DB, tenantID, email, password string) (*auth.User, error) {
	service := auth.NewProvisioner(db)
	base := deriveUsername(email)
	var lastErr error
	for attempt := 0; attempt < 100; attempt++ {
		username := base
		if attempt > 0 {
			suffix := fmt.Sprintf("-%d", attempt+1)
			room := 64 - len(suffix)
			if len(base) > room {
				base = base[:room]
			}
			username = base + suffix
			base = strings.TrimSuffix(username, suffix)
		}
		user, err := service.ProvisionCLIUser(ctx, tenantID, username, password, email, "admin")
		if err == nil {
			return user, nil
		}
		if errors.Is(err, auth.ErrConflict) {
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("could not derive a free username from the email: %w", lastErr)
}

func runCreate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	slug := flags.String("slug", "", "tenant slug (1-32 lowercase letters, digits and inner hyphens)")
	name := flags.String("name", "", "tenant display name")
	adminEmail := flags.String("admin-email", "", "optional first administrator email")
	adminPassword := flags.String("admin-password", "", "optional administrator password (prefer the generated one)")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "tenant create: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	if *slug == "" || *name == "" {
		fmt.Fprintln(stderr, "tenant create: --slug and --name are required")
		return exitUsage
	}
	if *adminPassword != "" && *adminEmail == "" {
		fmt.Fprintln(stderr, "tenant create: --admin-password requires --admin-email")
		return exitUsage
	}
	if *adminEmail != "" {
		normalized, err := auth.NormalizeEmail(*adminEmail)
		if err != nil {
			fmt.Fprintf(stderr, "tenant create: invalid admin email: %v\n", err)
			return exitUsage
		}
		*adminEmail = normalized
	}
	password, generated, err := resolvePassword(*adminPassword)
	if err != nil {
		fmt.Fprintf(stderr, "tenant create: %v\n", err)
		return exitUsage
	}
	ctx, cancel := commandContext()
	defer cancel()
	db, err := openDatabase(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "tenant create: %v\n", err)
		return exitError
	}
	defer db.Close()
	tenant, err := tenants.Create(ctx, db, *slug, *name)
	if err != nil {
		fmt.Fprintf(stderr, "tenant create: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "created tenant: id=%s slug=%s name=%q status=%s\n", tenant.ID, tenant.Slug, tenant.Name, tenant.Status)
	if *adminEmail == "" {
		return exitOK
	}
	user, err := provisionAdmin(ctx, db, tenant.ID, *adminEmail, password)
	if err != nil {
		fmt.Fprintf(stderr, "tenant create: tenant %q was created but the administrator could not be created: %v\n", tenant.Slug, err)
		fmt.Fprintf(stderr, "retry with: tenant create-admin --tenant %s --email %s\n", tenant.Slug, *adminEmail)
		return exitError
	}
	fmt.Fprintf(stdout, "created administrator: tenant=%s username=%s email=%s\n", tenant.Slug, user.Username, *adminEmail)
	if generated {
		fmt.Fprintf(stdout, "initial password (shown only once; store it securely): %s\n", password)
	}
	return exitOK
}

func runCreateAdmin(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tenant create-admin", flag.ContinueOnError)
	flags.SetOutput(stderr)
	slug := flags.String("tenant", "", "tenant slug")
	email := flags.String("email", "", "administrator email")
	passwordFlag := flags.String("password", "", "optional password (prefer the generated one)")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "tenant create-admin: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	if *slug == "" || *email == "" {
		fmt.Fprintln(stderr, "tenant create-admin: --tenant and --email are required")
		return exitUsage
	}
	normalizedEmail, err := auth.NormalizeEmail(*email)
	if err != nil {
		fmt.Fprintf(stderr, "tenant create-admin: invalid email: %v\n", err)
		return exitUsage
	}
	password, generated, err := resolvePassword(*passwordFlag)
	if err != nil {
		fmt.Fprintf(stderr, "tenant create-admin: %v\n", err)
		return exitUsage
	}
	ctx, cancel := commandContext()
	defer cancel()
	db, err := openDatabase(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "tenant create-admin: %v\n", err)
		return exitError
	}
	defer db.Close()
	if !tenants.ValidSlug(*slug) {
		fmt.Fprintf(stderr, "tenant create-admin: invalid tenant slug %q\n", *slug)
		return exitUsage
	}
	tenant, err := tenants.BySlug(ctx, db, *slug)
	if err != nil {
		if errors.Is(err, tenants.ErrNotFound) {
			fmt.Fprintf(stderr, "tenant create-admin: tenant %q does not exist\n", *slug)
		} else {
			fmt.Fprintf(stderr, "tenant create-admin: %v\n", err)
		}
		return exitError
	}
	user, err := provisionAdmin(ctx, db, tenant.ID, normalizedEmail, password)
	if err != nil {
		fmt.Fprintf(stderr, "tenant create-admin: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "created administrator: tenant=%s username=%s email=%s\n", tenant.Slug, user.Username, normalizedEmail)
	if generated {
		fmt.Fprintf(stdout, "initial password (shown only once; store it securely): %s\n", password)
	}
	return exitOK
}

func runList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tenant list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "tenant list: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	ctx, cancel := commandContext()
	defer cancel()
	db, err := openDatabase(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "tenant list: %v\n", err)
		return exitError
	}
	defer db.Close()
	list, err := tenants.List(ctx, db)
	if err != nil {
		fmt.Fprintf(stderr, "tenant list: %v\n", err)
		return exitError
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tSLUG\tNAME\tSTATUS\tUSERS\tCREATED_AT")
	for _, item := range list {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%s\n", item.ID, item.Slug, item.Name, item.Status, item.UserCount,
			time.Unix(item.CreatedAt, 0).UTC().Format(time.RFC3339))
	}
	if err = writer.Flush(); err != nil {
		fmt.Fprintf(stderr, "tenant list: %v\n", err)
		return exitError
	}
	return exitOK
}

func runSetStatus(command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tenant "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	slug := flags.String("tenant", "", "tenant slug")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "tenant %s: unexpected argument %q\n", command, flags.Arg(0))
		return exitUsage
	}
	if *slug == "" {
		fmt.Fprintf(stderr, "tenant %s: --tenant is required\n", command)
		return exitUsage
	}
	status := tenants.StatusSuspended
	if command == "enable" {
		status = tenants.StatusActive
	}
	ctx, cancel := commandContext()
	defer cancel()
	db, err := openDatabase(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "tenant %s: %v\n", command, err)
		return exitError
	}
	defer db.Close()
	tenant, err := tenants.SetStatus(ctx, db, *slug, status)
	if err != nil {
		if errors.Is(err, tenants.ErrNotFound) {
			fmt.Fprintf(stderr, "tenant %s: tenant %q does not exist\n", command, *slug)
		} else {
			fmt.Fprintf(stderr, "tenant %s: %v\n", command, err)
		}
		return exitError
	}
	fmt.Fprintf(stdout, "tenant %s: slug=%s status=%s\n", command, tenant.Slug, tenant.Status)
	return exitOK
}
