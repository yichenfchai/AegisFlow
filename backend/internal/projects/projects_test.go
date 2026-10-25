package projects

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
)

var testActor = auth.Actor{ID: "actor-000000000000001", Username: "tester", RequestID: "req-projects"}

func testService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return New(db), db
}

func str(value string) *string { return &value }
func boolPtr(value bool) *bool { return &value }

func TestCreateValidationAndUniqueness(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	for _, input := range []Input{
		{Name: "ab"}, {Name: " leading-space"}, {Name: "trailing "}, {Name: ".hidden"}, {Name: "-dash"},
		{Name: "has/slash"}, {Name: "has:colon"}, {Name: strings.Repeat("n", 65)},
		{Name: "valid-name", RepoURL: "ftp://example.com/repo.git"},
		{Name: "valid-name", RepoURL: "relative/path"},
		{Name: "valid-name", RepoURL: "https://example.com/has space.git"},
		{Name: "valid-name", DefaultBranch: "-main"},
		{Name: "valid-name", DefaultBranch: "feature..break"},
		{Name: "valid-name", DefaultBranch: "feature//dup"},
		{Name: "valid-name", DefaultBranch: "refs/heads/x.lock"},
		{Name: "valid-name", Description: strings.Repeat("d", 513)},
	} {
		if _, err := s.Create(ctx, testActor, input); !errors.Is(err, ErrValidation) {
			t.Fatalf("Create(%q, %q, %q) error = %v", input.Name, input.RepoURL, input.DefaultBranch, err)
		}
	}
	for _, input := range []Input{
		{Name: "alpha", RepoURL: "https://github.com/example/alpha.git", DefaultBranch: "main"},
		{Name: "贝塔项目", RepoURL: "git@github.com:example/beta.git"},
		{Name: "gamma-local", RepoURL: `D:\repos\gamma`},
		{Name: "delta-local", RepoURL: "/srv/repos/delta"},
		{Name: "epsilon-file", RepoURL: "file:///srv/repos/epsilon", DefaultBranch: "release/1.0.x"},
	} {
		if _, err := s.Create(ctx, testActor, input); err != nil {
			t.Fatalf("Create(%q) error = %v", input.Name, err)
		}
	}
	if _, err := s.Create(ctx, testActor, Input{Name: "ALPHA"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("case-insensitive duplicate error = %v", err)
	}
}

func TestListPaginationAndStatusFilter(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	for _, name := range []string{"curie", "ada", "grace"} {
		if _, err := s.Create(ctx, testActor, Input{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Update(ctx, testActor, mustID(t, s, "grace"), Patch{Archived: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	projects, total, err := s.List(ctx, "", 20, 0)
	if err != nil || total != 3 || len(projects) != 3 || projects[0].Name != "ada" || projects[2].Name != "grace" {
		t.Fatalf("list=%+v total=%d err=%v", projects, total, err)
	}
	if projects, total, err = s.List(ctx, StatusActive, 20, 0); err != nil || total != 2 || len(projects) != 2 {
		t.Fatalf("active filter: %d %d %v", len(projects), total, err)
	}
	if _, total, err = s.List(ctx, StatusArchived, 20, 0); err != nil || total != 1 {
		t.Fatalf("archived filter: %d %v", total, err)
	}
	if _, _, err = s.List(ctx, "deleted", 20, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad status error = %v", err)
	}
	if _, _, err = s.List(ctx, "", 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad bounds error = %v", err)
	}
}

func mustID(t *testing.T, s *Service, name string) string {
	t.Helper()
	projects, _, err := s.List(context.Background(), "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		if project.Name == name {
			return project.ID
		}
	}
	t.Fatalf("project %s not found", name)
	return ""
}

func TestUpdateRenameAndArchiveRules(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	alpha, err := s.Create(ctx, testActor, Input{Name: "alpha", RepoURL: "https://example.com/a.git"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, testActor, Input{Name: "beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, testActor, alpha.ID, Patch{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty patch error = %v", err)
	}
	if _, err = s.Update(ctx, testActor, "missing-0000000000000001", Patch{Name: str("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project error = %v", err)
	}
	if _, err = s.Update(ctx, testActor, alpha.ID, Patch{Name: str("BETA")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename conflict error = %v", err)
	}
	updated, err := s.Update(ctx, testActor, alpha.ID, Patch{Name: str("alpha-two"), Description: str("renamed")})
	if err != nil || updated.Name != "alpha-two" || updated.Description != "renamed" || updated.UpdatedAt < updated.CreatedAt {
		t.Fatalf("update = %+v err=%v", updated, err)
	}
	archived, err := s.Update(ctx, testActor, alpha.ID, Patch{Archived: boolPtr(true)})
	if err != nil || archived.Status != StatusArchived {
		t.Fatalf("archive = %+v err=%v", archived, err)
	}
	// Repeating the archive is an idempotent no-op.
	if _, err = s.Update(ctx, testActor, alpha.ID, Patch{Archived: boolPtr(true)}); err != nil {
		t.Fatalf("idempotent archive error = %v", err)
	}
	if _, err = s.Update(ctx, testActor, alpha.ID, Patch{Description: str("nope")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived field edit error = %v", err)
	}
	restored, err := s.Update(ctx, testActor, alpha.ID, Patch{Archived: boolPtr(false)})
	if err != nil || restored.Status != StatusActive {
		t.Fatalf("unarchive = %+v err=%v", restored, err)
	}
}

func TestAuditTrail(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	project, err := s.Create(ctx, testActor, Input{Name: "audited"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, testActor, project.ID, Patch{Archived: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	// No-op update writes no audit row.
	if _, err = s.Update(ctx, testActor, project.ID, Patch{Archived: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT action, object_type, result, actor_username FROM auth_audit ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]int{}
	for rows.Next() {
		var action, objectType, result, actor string
		if err = rows.Scan(&action, &objectType, &result, &actor); err != nil {
			t.Fatal(err)
		}
		if objectType != "project" || result != "success" || actor != "tester" {
			t.Fatalf("bad audit row: %s %s %s %s", action, objectType, result, actor)
		}
		seen[action]++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen[ActionCreate] != 1 || seen[ActionArchive] != 1 || len(seen) != 2 {
		t.Fatalf("audit actions = %v", seen)
	}
}
