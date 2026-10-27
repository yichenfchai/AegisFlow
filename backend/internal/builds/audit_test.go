package builds

import (
	"context"
	"strings"
	"testing"
)

type buildAuditRow struct {
	Action        string
	ActorID       string
	ActorUsername string
	Result        string
	Detail        string
	RequestID     string
}

// buildAuditRows reads the audit trail of one build object, oldest first.
func buildAuditRows(t *testing.T, s *Service, objectID string) []buildAuditRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT action,actor_id,actor_username,result,detail,request_id
		FROM auth_audit WHERE object_type='build' AND object_id=$1 ORDER BY created_at,id`, objectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var audit []buildAuditRow
	for rows.Next() {
		var row buildAuditRow
		if err := rows.Scan(&row.Action, &row.ActorID, &row.ActorUsername, &row.Result, &row.Detail, &row.RequestID); err != nil {
			t.Fatal(err)
		}
		audit = append(audit, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return audit
}

func TestTriggerWritesAudit(t *testing.T) {
	s, registry := testService(t)
	project := createProject(t, registry, "audit-trigger", "echo ok")
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	audit := buildAuditRows(t, s, build.ID)
	if len(audit) != 1 {
		t.Fatalf("want one trigger audit row, got %+v", audit)
	}
	row := audit[0]
	if row.Action != "build.trigger" || row.ActorID != testActor.ID || row.ActorUsername != testActor.Username || row.RequestID != testActor.RequestID || row.Result != "success" {
		t.Fatalf("unexpected trigger audit row: %+v", row)
	}
	if !strings.Contains(row.Detail, "project="+project.ID) || !strings.Contains(row.Detail, "build=1") {
		t.Fatalf("detail lacks project/number context: %q", row.Detail)
	}
	if strings.Contains(row.Detail, "echo ok") {
		t.Fatalf("audit detail must not leak the build command: %q", row.Detail)
	}
}

func TestCancelWritesOneAuditRow(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	ctx := context.Background()
	project := createProject(t, registry, "audit-cancel", sleepCommand(30))
	build, err := s.Trigger(ctx, testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusRunning)
	if _, err = s.Cancel(ctx, testActor, build.ID); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusCancelled)

	counts := map[string]int{}
	for _, row := range buildAuditRows(t, s, build.ID) {
		counts[row.Action]++
		if row.Result != "success" {
			t.Fatalf("audit row not successful: %+v", row)
		}
	}
	if counts["build.trigger"] != 1 || counts["build.cancel"] != 1 {
		t.Fatalf("want one trigger and one cancel audit row, got %v", counts)
	}

	// Idempotent repeat must not duplicate the cancel audit row.
	if _, err = s.Cancel(ctx, testActor, build.ID); err != nil {
		t.Fatal(err)
	}
	counts = map[string]int{}
	for _, row := range buildAuditRows(t, s, build.ID) {
		counts[row.Action]++
	}
	if counts["build.cancel"] != 1 {
		t.Fatalf("repeat cancel duplicated audit: %v", counts)
	}
}

func TestRejectedTriggerWritesNoAudit(t *testing.T) {
	s, registry := testService(t)
	ctx := context.Background()
	project := createProject(t, registry, "audit-commandless", "")
	if _, err := s.Trigger(ctx, testActor, project.ID, ""); err == nil {
		t.Fatal("expected a validation error for a project without a command")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE object_type='build'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected trigger wrote %d audit rows", count)
	}
}
