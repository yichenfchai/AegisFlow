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
	projects, total, err := s.List(ctx, testActor, "", 20, 0)
	if err != nil || total != 3 || len(projects) != 3 || projects[0].Name != "ada" || projects[2].Name != "grace" {
		t.Fatalf("list=%+v total=%d err=%v", projects, total, err)
	}
	if projects, total, err = s.List(ctx, testActor, StatusActive, 20, 0); err != nil || total != 2 || len(projects) != 2 {
		t.Fatalf("active filter: %d %d %v", len(projects), total, err)
	}
	if _, total, err = s.List(ctx, testActor, StatusArchived, 20, 0); err != nil || total != 1 {
		t.Fatalf("archived filter: %d %v", total, err)
	}
	if _, _, err = s.List(ctx, testActor, "deleted", 20, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad status error = %v", err)
	}
	if _, _, err = s.List(ctx, testActor, "", 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad bounds error = %v", err)
	}
}

func mustID(t *testing.T, s *Service, name string) string {
	t.Helper()
	projects, _, err := s.List(context.Background(), testActor, "", 100, 0)
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

// Actors covering the global roles plus one project outsider. IDs must satisfy
// auth.ValidID so the registry treats them like real accounts.
var (
	adminActor    = auth.Actor{ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-admin"}
	memberActor   = auth.Actor{ID: "member-000000000000001", Username: "mia", Role: "member", RequestID: "req-member"}
	viewerActor   = auth.Actor{ID: "viewer-000000000000001", Username: "vic", Role: "viewer", RequestID: "req-viewer"}
	outsiderActor = auth.Actor{ID: "outsider-0000000000001", Username: "otto", Role: "member", RequestID: "req-outsider"}
)

// seedUser inserts an auth account so membership can reference a real user.
func seedUser(t *testing.T, db *sql.DB, id, username string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO auth_users(id,username,password_hash,display_name,role)
	 VALUES($1,$2,'unused',$2,'member')`, id, username); err != nil {
		t.Fatal(err)
	}
}

func TestVisibilityFiltersProjectReads(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, memberActor.ID, memberActor.Username)
	seedUser(t, db, viewerActor.ID, viewerActor.Username)
	public, err := s.Create(ctx, adminActor, Input{Name: "public-one"})
	if err != nil {
		t.Fatal(err)
	}
	if public.Visibility != VisibilityPublic {
		t.Fatalf("default visibility = %q", public.Visibility)
	}
	private, err := s.Create(ctx, adminActor, Input{Name: "private-one", Visibility: VisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, adminActor, Input{Name: "bogus", Visibility: "secret"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid visibility error = %v", err)
	}
	// Global admin sees every project regardless of visibility.
	if _, total, err := s.List(ctx, adminActor, "", 20, 0); err != nil || total != 2 {
		t.Fatalf("admin list total=%d err=%v", total, err)
	}
	// A non-member reads the public project only, and the private one is not found.
	list, total, err := s.List(ctx, memberActor, "", 20, 0)
	if err != nil || total != 1 || len(list) != 1 || list[0].ID != public.ID {
		t.Fatalf("member list = %+v total=%d err=%v", list, total, err)
	}
	if _, err = s.Get(ctx, memberActor, private.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible private project error = %v", err)
	}
	if _, err = s.Update(ctx, outsiderActor, private.ID, Patch{Description: str("hijack")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible update error = %v", err)
	}
	// Viewer membership grants read access without any global write role.
	if _, err = s.AddMember(ctx, adminActor, private.ID, memberActor.ID, RoleViewer); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, memberActor, private.ID); err != nil || got.ID != private.ID {
		t.Fatalf("member read after add = %+v err=%v", got, err)
	}
	if _, total, err = s.List(ctx, memberActor, "", 20, 0); err != nil || total != 2 {
		t.Fatalf("member list after add total=%d err=%v", total, err)
	}
	// Switching a project back to public restores the old behaviour.
	if _, err = s.Update(ctx, adminActor, private.ID, Patch{Visibility: str(VisibilityPublic)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, outsiderActor, private.ID); err != nil {
		t.Fatalf("public read after flip err=%v", err)
	}
}

func TestMemberLifecycle(t *testing.T) {
	s, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, memberActor.ID, memberActor.Username)
	seedUser(t, db, viewerActor.ID, viewerActor.Username)
	project, err := s.Create(ctx, adminActor, Input{Name: "team", Visibility: VisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	// The creator is the first maintainer.
	members, err := s.ListMembers(ctx, adminActor, project.ID)
	if err != nil || len(members) != 1 || members[0].UserID != adminActor.ID || members[0].Role != RoleMaintainer {
		t.Fatalf("initial members = %+v err=%v", members, err)
	}
	member, err := s.AddMember(ctx, adminActor, project.ID, memberActor.ID, RoleViewer)
	if err != nil || member.Username != memberActor.Username || member.Role != RoleViewer {
		t.Fatalf("add member = %+v err=%v", member, err)
	}
	if _, err = s.AddMember(ctx, adminActor, project.ID, memberActor.ID, RoleViewer); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate member error = %v", err)
	}
	if _, err = s.AddMember(ctx, adminActor, project.ID, "absent-00000000000001", RoleViewer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user error = %v", err)
	}
	if _, err = s.AddMember(ctx, adminActor, project.ID, viewerActor.ID, "owner"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid role error = %v", err)
	}
	promoted, err := s.UpdateMemberRole(ctx, adminActor, project.ID, memberActor.ID, RoleMaintainer)
	if err != nil || promoted.Role != RoleMaintainer {
		t.Fatalf("promote = %+v err=%v", promoted, err)
	}
	if _, err = s.UpdateMemberRole(ctx, adminActor, project.ID, viewerActor.ID, RoleViewer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown membership error = %v", err)
	}
	// The creator keeps owner access and cannot be removed.
	if err = s.RemoveMember(ctx, adminActor, project.ID, adminActor.ID); !errors.Is(err, ErrValidation) {
		t.Fatalf("creator removal error = %v", err)
	}
	if err = s.RemoveMember(ctx, adminActor, project.ID, memberActor.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveMember(ctx, adminActor, project.ID, memberActor.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeat removal error = %v", err)
	}
	// Removal also removes visibility.
	if _, err = s.Get(ctx, memberActor, project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("visibility after removal error = %v", err)
	}
	// A non-member cannot probe a private project's member list.
	if _, err = s.ListMembers(ctx, outsiderActor, project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member list enumeration error = %v", err)
	}
}
