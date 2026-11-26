package deployments

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

var (
	accessMember = auth.Actor{ID: "member-0000000000000001", Username: "dev", Role: "member", RequestID: "req-access-member"}
	accessViewer = auth.Actor{ID: "viewer-0000000000000001", Username: "read", Role: "viewer", RequestID: "req-access-viewer"}
)

// accessService builds a service that owns the real project registry, which is
// what the write overlay needs for non-admin callers. The two non-admin actors
// get real user rows so project membership can name them.
func accessService(t *testing.T) (*Service, *sql.DB, *projects.Service) {
	t.Helper()
	_, db := testServiceWith(t, Options{SecretKey: []byte(testSecretKey)})
	for _, actor := range []auth.Actor{accessMember, accessViewer} {
		if _, err := db.Exec(`INSERT INTO auth_users(id,username,password_hash,display_name,role,disabled,created_at)
			VALUES($1,$2,'x',$3,$4,0,0)`, actor.ID, actor.Username, actor.Username, actor.Role); err != nil {
			t.Fatal(err)
		}
	}
	registry := projects.New(db)
	service := New(db, Options{
		SecretKey:            []byte(testSecretKey),
		Projects:             registry,
		ApprovalEnvironments: []string{},
	})
	return service, db, registry
}

func accessProject(t *testing.T, registry *projects.Service, name, visibility string) projects.Project {
	t.Helper()
	project, err := registry.Create(context.Background(), testActor, projects.Input{Name: name, Visibility: visibility})
	if err != nil {
		t.Fatal(err)
	}
	return *project
}

func accessTarget(t *testing.T, s *Service) Target {
	t.Helper()
	target, err := s.Create(context.Background(), testActor, Input{Name: "access-target", Endpoint: "https://example.com", Environment: EnvironmentStaging})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// TestDeploymentWritesFollowProjectWriteOverlay covers the D1/D3 decision: an
// admin always writes, a member writes on public projects and on private ones
// it maintains, a viewer never writes, and an invisible project stays a 404.
func TestDeploymentWritesFollowProjectWriteOverlay(t *testing.T) {
	s, _, registry := accessService(t)
	ctx := context.Background()
	target := accessTarget(t, s)

	public := accessProject(t, registry, "access-public", projects.VisibilityPublic)
	private := accessProject(t, registry, "access-private", projects.VisibilityPrivate)
	maintained := accessProject(t, registry, "access-maintained", projects.VisibilityPrivate)
	if _, err := registry.AddMember(ctx, testActor, maintained.ID, accessMember.ID, projects.RoleMaintainer); err != nil {
		t.Fatal(err)
	}
	// The viewer is a project member, so the project is visible to it and the
	// refusal must be a 403 rather than the 404 an outsider gets.
	if _, err := registry.AddMember(ctx, testActor, maintained.ID, accessViewer.ID, projects.RoleViewer); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateDeployment(ctx, accessMember, public.ID, DeployInput{TargetID: target.ID, Version: "v1"}); err != nil {
		t.Fatalf("member on public project: %v", err)
	}
	if _, err := s.CreateDeployment(ctx, accessMember, maintained.ID, DeployInput{TargetID: target.ID, Version: "v1"}); err != nil {
		t.Fatalf("maintainer on private project: %v", err)
	}
	if _, err := s.CreateDeployment(ctx, accessMember, private.ID, DeployInput{TargetID: target.ID, Version: "v1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member on foreign private project = %v, want not found", err)
	}
	if _, err := s.CreateDeployment(ctx, accessViewer, public.ID, DeployInput{TargetID: target.ID, Version: "v1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer on public project = %v, want forbidden", err)
	}
	if _, err := s.CreateDeployment(ctx, accessViewer, maintained.ID, DeployInput{TargetID: target.ID, Version: "v1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer on maintainer project = %v, want forbidden", err)
	}
}

// TestDeploymentReadsFollowProjectVisibility covers D5/D6: the member sees the
// public and maintained deployments, nothing of the foreign private project,
// and cancel on an invisible deployment is a 404 rather than a 403.
func TestDeploymentReadsFollowProjectVisibility(t *testing.T) {
	s, _, registry := accessService(t)
	ctx := context.Background()
	target := accessTarget(t, s)

	public := accessProject(t, registry, "read-public", projects.VisibilityPublic)
	private := accessProject(t, registry, "read-private", projects.VisibilityPrivate)
	maintained := accessProject(t, registry, "read-maintained", projects.VisibilityPrivate)
	if _, err := registry.AddMember(ctx, testActor, maintained.ID, accessMember.ID, projects.RoleMaintainer); err != nil {
		t.Fatal(err)
	}
	visible, err := s.CreateDeployment(ctx, testActor, public.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	maintainedDeployment, err := s.CreateDeployment(ctx, testActor, maintained.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := s.CreateDeployment(ctx, testActor, private.ID, DeployInput{TargetID: target.ID, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}

	list, total, err := s.ListDeployments(ctx, accessMember, "", "", 20, 0)
	if err != nil || total != 2 || len(list) != 2 {
		t.Fatalf("member list = %d/%d err=%v, want the two visible deployments", len(list), total, err)
	}
	for _, deployment := range list {
		if deployment.ID == hidden.ID {
			t.Fatal("member list leaked the private project's deployment")
		}
	}
	if page, filteredTotal, err := s.ListDeployments(ctx, accessMember, private.ID, "", 20, 0); err != nil || filteredTotal != 0 || len(page) != 0 {
		t.Fatalf("filtered private list = %d/%d err=%v, want empty", len(page), filteredTotal, err)
	}
	if _, err := s.GetDeployment(ctx, accessMember, hidden.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member get hidden = %v, want not found", err)
	}
	if _, err := s.DeploymentEvents(ctx, accessViewer, hidden.ID, 0, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer events hidden = %v, want not found", err)
	}
	if _, err := s.GetDeployment(ctx, accessViewer, visible.ID); err != nil {
		t.Fatalf("viewer get public = %v, want ok", err)
	}
	if _, err := s.CancelDeployment(ctx, accessMember, hidden.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member cancel hidden = %v, want not found", err)
	}
	if _, err := s.CancelDeployment(ctx, accessViewer, visible.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer cancel = %v, want forbidden", err)
	}
	if _, err := s.CancelDeployment(ctx, accessMember, maintainedDeployment.ID); err != nil {
		t.Fatalf("maintainer cancel = %v, want ok", err)
	}
	if _, err := s.RollbackDeployment(ctx, accessMember, hidden.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member rollback hidden = %v, want not found", err)
	}
}

// TestDeploymentWriteFailsClosedWithoutRegistry keeps a service built without
// the project registry admin-only instead of silently allowing every member.
func TestDeploymentWriteFailsClosedWithoutRegistry(t *testing.T) {
	s, _ := testServiceWith(t, Options{SecretKey: []byte(testSecretKey)})
	ctx := context.Background()
	target := accessTarget(t, s)
	project := accessProject(t, projects.New(s.db), "no-registry", projects.VisibilityPublic)
	if _, err := s.CreateDeployment(ctx, accessMember, project.ID, DeployInput{TargetID: target.ID, Version: "v1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member without registry = %v, want forbidden", err)
	}
}
