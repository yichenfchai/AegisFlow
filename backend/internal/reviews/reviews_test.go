package reviews

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/dbmigrate"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
	"github.com/yichenfchai/AegisFlow/backend/internal/tenants"
	"github.com/yichenfchai/AegisFlow/backend/internal/testutil"
	"strings"
	"sync"
	"testing"
)

// Actors covering the global roles plus one project outsider. IDs must satisfy
// auth.ValidID so the registry treats them like real accounts.
var (
	adminActor    = auth.Actor{TenantID: tenants.DefaultID, ID: "admin-0000000000000001", Username: "root", Role: "admin", RequestID: "req-admin"}
	maintainActor = auth.Actor{TenantID: tenants.DefaultID, ID: "maint-0000000000000001", Username: "mira", Role: "member", RequestID: "req-maint"}
	memberActor   = auth.Actor{TenantID: tenants.DefaultID, ID: "member-000000000000001", Username: "mia", Role: "member", RequestID: "req-member"}
	viewerActor   = auth.Actor{TenantID: tenants.DefaultID, ID: "viewer-000000000000001", Username: "vic", Role: "viewer", RequestID: "req-viewer"}
	outsiderActor = auth.Actor{TenantID: tenants.DefaultID, ID: "outsider-0000000000001", Username: "otto", Role: "member", RequestID: "req-outsider"}
)

func testService(t *testing.T) (*Service, *projects.Service, *sql.DB) {
	t.Helper()
	db := testutil.OpenPostgres(t)
	if err := dbmigrate.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	registry := projects.New(db)
	return New(db, registry), registry, db
}

// seedUser inserts an auth account so membership and username resolution can
// reference a real user.
func seedUser(t *testing.T, db *sql.DB, actor auth.Actor) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO auth_users(id,username,password_hash,display_name,role,tenant_id)
	 VALUES($1,$2,'unused',$2,'member',$3)`, actor.ID, actor.Username, tenants.DefaultID); err != nil {
		t.Fatal(err)
	}
}

func createProject(t *testing.T, registry *projects.Service, actor auth.Actor, name, visibility string) *projects.Project {
	t.Helper()
	project, err := registry.Create(context.Background(), actor, projects.Input{Name: name, Visibility: visibility})
	if err != nil {
		t.Fatalf("create project %q: %v", name, err)
	}
	return project
}

// addMaintainer makes actor a maintainer of project through the registry, the
// same path the HTTP layer uses.
func addMaintainer(t *testing.T, registry *projects.Service, projectID string, actor auth.Actor) {
	t.Helper()
	if _, err := registry.AddMember(context.Background(), adminActor, projectID, actor.ID, projects.RoleMaintainer); err != nil {
		t.Fatalf("add maintainer %s: %v", actor.Username, err)
	}
}

func createReview(t *testing.T, s *Service, actor auth.Actor, projectID string) *Review {
	t.Helper()
	review, err := s.Create(context.Background(), actor, projectID, Input{
		Subject: "Release candidate for review", Description: "Please review the upcoming release.",
		Version: "v1.2.3", ExternalURL: "https://example.com/pulls/42",
	})
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	return review
}

type reviewAuditRow struct {
	Action        string
	ActorID       string
	ActorUsername string
	Result        string
	Detail        string
	RequestID     string
}

// reviewAuditRows reads the audit trail of one review object, oldest first.
func reviewAuditRows(t *testing.T, db *sql.DB, objectID string) []reviewAuditRow {
	t.Helper()
	rows, err := db.Query(`SELECT action,actor_id,actor_username,result,detail,request_id
		FROM auth_audit WHERE object_type='review' AND object_id=$1 ORDER BY created_at,id`, objectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var audit []reviewAuditRow
	for rows.Next() {
		var row reviewAuditRow
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

// findReviewAudit returns the one row with the given action.
func findReviewAudit(t *testing.T, rows []reviewAuditRow, action string) reviewAuditRow {
	t.Helper()
	for _, row := range rows {
		if row.Action == action {
			return row
		}
	}
	t.Fatalf("missing %s audit row in %+v", action, rows)
	return reviewAuditRow{}
}

func countReviewAudit(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE object_type='review'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCreateValidationAndNumbering(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, maintainActor)
	public := createProject(t, registry, adminActor, "review-public", projects.VisibilityPublic)
	private := createProject(t, registry, adminActor, "review-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, private.ID, maintainActor)

	for _, input := range []Input{
		{Subject: "ab"},
		{Subject: strings.Repeat("s", 201)},
		{Subject: "multi\nline"},
		{Subject: "tab\there"},
		{Subject: "valid subject", Description: strings.Repeat("d", 2001)},
		{Subject: "valid subject", Version: strings.Repeat("v", 65)},
		{Subject: "valid subject", Version: "v1\nv2"},
		{Subject: "valid subject", ExternalURL: "ftp://example.com/file"},
		{Subject: "valid subject", ExternalURL: "http://user:pass@example.com/pr/1"},
		{Subject: "valid subject", ExternalURL: "https://example.com/pr/1#fragment"},
		{Subject: "valid subject", ExternalURL: "https:///no-host"},
		{Subject: "valid subject", ExternalURL: "not a url"},
		{Subject: "valid subject", ExternalURL: "https://example.com/" + strings.Repeat("p", 2040)},
	} {
		if _, err := s.Create(ctx, adminActor, public.ID, input); !errors.Is(err, ErrValidation) {
			t.Fatalf("Create(%q, %q, %q, %q) error = %v, want ErrValidation", input.Subject, input.Version, input.ExternalURL, input.Description, err)
		}
	}

	// Every field at its documented bound is accepted, and the stored values are
	// trimmed for the single-line fields but keep the description formatting.
	review, err := s.Create(ctx, adminActor, public.ID, Input{
		Subject:     "  审查发布候选  ",
		Description: "line one\nline two",
		Version:     " v9.9.9 ",
		ExternalURL: " https://example.com/pr/9?tab=files ",
	})
	if err != nil {
		t.Fatalf("create bounded review: %v", err)
	}
	if review.Number != 1 || review.Status != StatusPending || review.RequestedBy != adminActor.ID || review.RequestedByName != adminActor.Username {
		t.Fatalf("first review = %+v", review)
	}
	if review.Subject != "审查发布候选" || review.Version != "v9.9.9" || review.ExternalURL != "https://example.com/pr/9?tab=files" {
		t.Fatalf("trimmed fields = %q %q %q", review.Subject, review.Version, review.ExternalURL)
	}
	if review.Description != "line one\nline two" || review.DecidedBy != nil || review.DecidedAt != nil || review.Reason != nil {
		t.Fatalf("pending review decision fields = %+v", review)
	}

	second := createReview(t, s, adminActor, public.ID)
	if second.Number != 2 {
		t.Fatalf("second review number = %d, want 2", second.Number)
	}
	other := createReview(t, s, adminActor, private.ID)
	if other.Number != 1 {
		t.Fatalf("review number is not per project: %d", other.Number)
	}

	// A visible project the caller cannot write is 403; an invisible one is 404.
	if _, err = s.Create(ctx, viewerActor, public.ID, Input{Subject: "viewer request"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer create error = %v, want ErrForbidden", err)
	}
	if _, err = s.Create(ctx, outsiderActor, private.ID, Input{Subject: "outsider request"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible project create error = %v, want ErrNotFound", err)
	}
	// A private project maintainer may write it.
	if _, err = s.Create(ctx, maintainActor, private.ID, Input{Subject: "maintainer request"}); err != nil {
		t.Fatalf("maintainer create: %v", err)
	}
	if _, err = s.Create(ctx, adminActor, "missing-0000000000001", Input{Subject: "unknown project"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project create error = %v, want ErrNotFound", err)
	}
	if _, err = s.Create(ctx, adminActor, "bad id", Input{Subject: "bad project"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid project id error = %v, want ErrValidation", err)
	}
}

func TestVisibilityFiltersReviewReads(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, maintainActor)
	public := createProject(t, registry, adminActor, "visible-public", projects.VisibilityPublic)
	private := createProject(t, registry, adminActor, "visible-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, private.ID, maintainActor)

	publicReview := createReview(t, s, adminActor, public.ID)
	privateReview := createReview(t, s, adminActor, private.ID)

	if got, err := s.Get(ctx, outsiderActor, publicReview.ID); err != nil || got.ID != publicReview.ID {
		t.Fatalf("public review read = %+v err=%v", got, err)
	}
	// A private project's review is invisible to non-members: 404, never 403.
	if _, err := s.Get(ctx, outsiderActor, privateReview.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible review get error = %v, want ErrNotFound", err)
	}
	if _, _, err := s.List(ctx, outsiderActor, private.ID, "", 20, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible review list error = %v, want ErrNotFound", err)
	}
	if got, err := s.Get(ctx, maintainActor, privateReview.ID); err != nil || got.ID != privateReview.ID {
		t.Fatalf("maintainer review read = %+v err=%v", got, err)
	}
	if got, err := s.Get(ctx, adminActor, privateReview.ID); err != nil || got.ID != privateReview.ID {
		t.Fatalf("admin review read = %+v err=%v", got, err)
	}
	if _, err := s.Get(ctx, adminActor, "missing-0000000000001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown review error = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, adminActor, "bad id"); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid review id error = %v, want ErrValidation", err)
	}
}

func TestDecisionStateMachineAndIdempotency(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, maintainActor)
	project := createProject(t, registry, adminActor, "state-machine", projects.VisibilityPublic)
	addMaintainer(t, registry, project.ID, maintainActor)

	// pending -> approved: reviewer identity and optional reason are recorded.
	review := createReview(t, s, adminActor, project.ID)
	approved, err := s.Approve(ctx, maintainActor, review.ID, "looks good")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != StatusApproved || approved.DecidedBy == nil || *approved.DecidedBy != maintainActor.ID ||
		approved.DecidedByName == nil || *approved.DecidedByName != maintainActor.Username ||
		approved.DecidedAt == nil || approved.Reason == nil || *approved.Reason != "looks good" {
		t.Fatalf("approved review = %+v", approved)
	}
	// Repeating the approval is idempotent: same stored decision, no new audit.
	repeat, err := s.Approve(ctx, adminActor, review.ID, "another reason")
	if err != nil {
		t.Fatalf("repeat approve: %v", err)
	}
	if repeat.Status != StatusApproved || *repeat.DecidedBy != maintainActor.ID || *repeat.Reason != "looks good" {
		t.Fatalf("repeat approve changed the decision: %+v", repeat)
	}
	if got := len(reviewAuditRows(t, db, review.ID)); got != 2 { // review.request + review.approve
		t.Fatalf("audit rows after idempotent approve = %d, want 2", got)
	}
	// Every other terminal transition is a conflict.
	if _, err = s.Reject(ctx, maintainActor, review.ID, "late rejection"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reject after approve error = %v, want ErrConflict", err)
	}
	if _, err = s.Cancel(ctx, maintainActor, review.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel after approve error = %v, want ErrConflict", err)
	}

	// pending -> rejected: reason is required and repeated rejection is a no-op.
	second := createReview(t, s, adminActor, project.ID)
	if _, err = s.Reject(ctx, maintainActor, second.ID, "   "); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty reject reason error = %v, want ErrValidation", err)
	}
	rejected, err := s.Reject(ctx, maintainActor, second.ID, "missing changelog")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rejected.Status != StatusRejected || rejected.Reason == nil || *rejected.Reason != "missing changelog" {
		t.Fatalf("rejected review = %+v", rejected)
	}
	repeatReject, err := s.Reject(ctx, adminActor, second.ID, "different reason")
	if err != nil || *repeatReject.Reason != "missing changelog" {
		t.Fatalf("repeat reject = %+v err=%v", repeatReject, err)
	}
	if _, err = s.Approve(ctx, maintainActor, second.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve after reject error = %v, want ErrConflict", err)
	}
	if _, err = s.Cancel(ctx, adminActor, second.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel after reject error = %v, want ErrConflict", err)
	}

	// pending -> cancelled: the requester may withdraw; repeating is a no-op.
	third := createReview(t, s, adminActor, project.ID)
	cancelled, err := s.Cancel(ctx, adminActor, third.ID)
	if err != nil {
		t.Fatalf("requester cancel: %v", err)
	}
	if cancelled.Status != StatusCancelled || cancelled.DecidedBy == nil || *cancelled.DecidedBy != adminActor.ID || cancelled.Reason != nil {
		t.Fatalf("cancelled review = %+v", cancelled)
	}
	repeatCancel, err := s.Cancel(ctx, maintainActor, third.ID)
	if err != nil || repeatCancel.Status != StatusCancelled || *repeatCancel.DecidedBy != adminActor.ID {
		t.Fatalf("repeat cancel = %+v err=%v", repeatCancel, err)
	}
	for _, op := range []func(context.Context, auth.Actor, string, string) (*Review, error){
		s.Approve,
		s.Reject,
	} {
		if _, err = op(ctx, maintainActor, third.ID, "late"); !errors.Is(err, ErrConflict) {
			t.Fatalf("decision after cancel error = %v, want ErrConflict", err)
		}
	}
	if got := len(reviewAuditRows(t, db, third.ID)); got != 2 { // review.request + review.cancel
		t.Fatalf("audit rows after idempotent cancel = %d, want 2", got)
	}

	// A maintainer who did not request the review may also cancel it.
	fourth := createReview(t, s, adminActor, project.ID)
	if _, err = s.Cancel(ctx, maintainActor, fourth.ID); err != nil {
		t.Fatalf("maintainer cancel: %v", err)
	}

	// Reason bounds apply to approve as well.
	fifth := createReview(t, s, adminActor, project.ID)
	if _, err = s.Approve(ctx, maintainActor, fifth.ID, strings.Repeat("r", 513)); !errors.Is(err, ErrValidation) {
		t.Fatalf("oversized approve reason error = %v, want ErrValidation", err)
	}
	if _, err = s.Approve(ctx, maintainActor, fifth.ID, "two\nlines"); !errors.Is(err, ErrValidation) {
		t.Fatalf("multiline approve reason error = %v, want ErrValidation", err)
	}
}

func TestDecisionPermissionMatrix(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	for _, actor := range []auth.Actor{maintainActor, memberActor, viewerActor, outsiderActor} {
		seedUser(t, db, actor)
	}
	public := createProject(t, registry, adminActor, "permissions-public", projects.VisibilityPublic)
	addMaintainer(t, registry, public.ID, maintainActor)
	private := createProject(t, registry, adminActor, "permissions-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, private.ID, maintainActor)
	seedUser(t, db, adminActor)

	// A public project writer without the maintainer role cannot decide, but it
	// may cancel its own request.
	memberReview := createReview(t, s, memberActor, public.ID)
	for name, op := range map[string]func(context.Context, auth.Actor, string, string) (*Review, error){
		"approve": s.Approve, "reject": s.Reject,
	} {
		if _, err := op(ctx, memberActor, memberReview.ID, "member decision"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("non-maintainer %s error = %v, want ErrForbidden", name, err)
		}
	}
	if _, err := s.Cancel(ctx, memberActor, memberReview.ID); err != nil {
		t.Fatalf("requester cancel: %v", err)
	}
	// A global viewer never decides, and a global member that is not a
	// maintainer cannot decide a public project's review.
	viewerReview := createReview(t, s, adminActor, public.ID)
	// Cancel is requester-only for plain members: a visible project's member may
	// not withdraw somebody else's pending review.
	if _, err := s.Cancel(ctx, memberActor, viewerReview.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-requester cancel error = %v, want ErrForbidden", err)
	}
	if _, err := s.Approve(ctx, viewerActor, viewerReview.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer approve error = %v, want ErrForbidden", err)
	}
	if _, err := s.Approve(ctx, outsiderActor, viewerReview.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("outsider approve error = %v, want ErrForbidden", err)
	}
	// The maintainer and the global admin may decide.
	if _, err := s.Approve(ctx, maintainActor, viewerReview.ID, ""); err != nil {
		t.Fatalf("maintainer approve: %v", err)
	}
	adminReview := createReview(t, s, maintainActor, public.ID)
	if _, err := s.Approve(ctx, adminActor, adminReview.ID, "admin override"); err != nil {
		t.Fatalf("admin approve: %v", err)
	}

	// Private project: a member without membership cannot even see the review, so
	// the decision is 404; the maintainer and the admin may decide.
	privateReview := createReview(t, s, maintainActor, private.ID)
	if _, err := s.Approve(ctx, memberActor, privateReview.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible private approve error = %v, want ErrNotFound", err)
	}
	if _, err := s.Approve(ctx, maintainActor, privateReview.ID, ""); err != nil {
		t.Fatalf("private maintainer approve: %v", err)
	}
	privateTwo := createReview(t, s, maintainActor, private.ID)
	if _, err := s.Cancel(ctx, memberActor, privateTwo.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible private cancel error = %v, want ErrNotFound", err)
	}
	if _, err := s.Approve(ctx, adminActor, privateTwo.ID, ""); err != nil {
		t.Fatalf("private admin approve: %v", err)
	}
}

func TestAuditTrailContents(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, maintainActor)
	project := createProject(t, registry, adminActor, "audit-review", projects.VisibilityPublic)
	addMaintainer(t, registry, project.ID, maintainActor)

	review, err := s.Create(ctx, adminActor, project.ID, Input{
		Subject: "sensitive subject", Description: "sensitive description body",
		Version: "v1.0.0", ExternalURL: "https://example.com/secret-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := reviewAuditRows(t, db, review.ID)
	if len(rows) != 1 {
		t.Fatalf("want one request audit row, got %+v", rows)
	}
	request := rows[0]
	if request.Action != ActionRequest || request.ActorID != adminActor.ID || request.ActorUsername != adminActor.Username ||
		request.RequestID != adminActor.RequestID || request.Result != "success" {
		t.Fatalf("unexpected request audit row: %+v", request)
	}
	if !strings.Contains(request.Detail, "project="+project.ID) || !strings.Contains(request.Detail, "number=1") {
		t.Fatalf("request detail lacks project/number context: %q", request.Detail)
	}
	for _, forbidden := range []string{"sensitive subject", "sensitive description body", "v1.0.0", "secret-token"} {
		if strings.Contains(request.Detail, forbidden) {
			t.Fatalf("request audit detail leaks %q: %q", forbidden, request.Detail)
		}
	}

	if _, err = s.Approve(ctx, maintainActor, review.ID, "lgtm"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Approve(ctx, maintainActor, review.ID, "lgtm"); err != nil { // idempotent
		t.Fatal(err)
	}
	rows = reviewAuditRows(t, db, review.ID)
	if len(rows) != 2 {
		t.Fatalf("approve audit rows = %+v", rows)
	}
	approveRow := findReviewAudit(t, rows, ActionApprove)
	if approveRow.ActorID != maintainActor.ID {
		t.Fatalf("approve audit actor = %+v", approveRow)
	}
	if !strings.Contains(approveRow.Detail, "number=1") || !strings.Contains(approveRow.Detail, "reason=lgtm") {
		t.Fatalf("approve detail = %q", approveRow.Detail)
	}

	second := createReview(t, s, adminActor, project.ID)
	if _, err = s.Reject(ctx, maintainActor, second.ID, "needs tests"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reject(ctx, maintainActor, second.ID, "needs tests"); err != nil { // idempotent
		t.Fatal(err)
	}
	rows = reviewAuditRows(t, db, second.ID)
	if len(rows) != 2 {
		t.Fatalf("reject audit rows = %+v", rows)
	}
	if rejectRow := findReviewAudit(t, rows, ActionReject); !strings.Contains(rejectRow.Detail, "reason=needs tests") {
		t.Fatalf("reject detail = %q", rejectRow.Detail)
	}

	third := createReview(t, s, adminActor, project.ID)
	if _, err = s.Cancel(ctx, maintainActor, third.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Cancel(ctx, maintainActor, third.ID); err != nil { // idempotent
		t.Fatal(err)
	}
	rows = reviewAuditRows(t, db, third.ID)
	if len(rows) != 2 {
		t.Fatalf("cancel audit rows = %+v", rows)
	}
	findReviewAudit(t, rows, ActionCancel)
	total := countReviewAudit(t, db)
	if total != 6 { // 3 request + approve + reject + cancel
		t.Fatalf("review audit total = %d, want 6", total)
	}
	var wrongObjectType int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE action LIKE 'review.%' AND object_type<>'review'`).Scan(&wrongObjectType); err != nil {
		t.Fatal(err)
	}
	if wrongObjectType != 0 {
		t.Fatalf("%d review audit rows have the wrong object type", wrongObjectType)
	}
}

func TestListPaginationAndStatusFilter(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, maintainActor)
	project := createProject(t, registry, adminActor, "list-reviews", projects.VisibilityPublic)
	addMaintainer(t, registry, project.ID, maintainActor)
	privateProject := createProject(t, registry, adminActor, "list-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, privateProject.ID, maintainActor)

	for i := 0; i < 5; i++ {
		createReview(t, s, adminActor, project.ID)
	}
	all, total, err := s.List(ctx, adminActor, project.ID, "", 20, 0)
	if err != nil || total != 5 || len(all) != 5 {
		t.Fatalf("list all = %d %d %v", len(all), total, err)
	}
	for i, review := range all {
		if want := int64(5 - i); review.Number != want {
			t.Fatalf("list order[%d] number = %d, want %d", i, review.Number, want)
		}
	}
	page, total, err := s.List(ctx, adminActor, project.ID, "", 2, 1)
	if err != nil || total != 5 || len(page) != 2 || page[0].Number != 4 || page[1].Number != 3 {
		t.Fatalf("list page = %+v total=%d err=%v", page, total, err)
	}

	// Status filter: approve #2 and reject #4.
	byNumber := map[int64]*Review{}
	for i := range all {
		byNumber[all[i].Number] = &all[i]
	}
	if _, err = s.Approve(ctx, maintainActor, byNumber[2].ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reject(ctx, maintainActor, byNumber[4].ID, "no"); err != nil {
		t.Fatal(err)
	}
	approved, total, err := s.List(ctx, adminActor, project.ID, StatusApproved, 20, 0)
	if err != nil || total != 1 || len(approved) != 1 || approved[0].Number != 2 {
		t.Fatalf("approved filter = %+v total=%d err=%v", approved, total, err)
	}
	pending, total, err := s.List(ctx, adminActor, project.ID, StatusPending, 20, 0)
	if err != nil || total != 3 || len(pending) != 3 {
		t.Fatalf("pending filter = %+v total=%d err=%v", pending, total, err)
	}
	if _, _, err = s.List(ctx, adminActor, project.ID, "deleted", 20, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid status filter error = %v, want ErrValidation", err)
	}
	if _, _, err = s.List(ctx, adminActor, project.ID, "", 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid pagination error = %v, want ErrValidation", err)
	}

	// A private project's reviews are invisible (404), never an empty page.
	if _, _, err = s.List(ctx, outsiderActor, privateProject.ID, "", 20, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible project list error = %v, want ErrNotFound", err)
	}
	if _, _, err = s.List(ctx, adminActor, "bad id", "", 20, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid project id list error = %v, want ErrValidation", err)
	}
}

// Concurrent creators on the same project must not reuse a review number: the
// projects row lock serializes the allocation, exactly like build/deployment
// numbering.
func TestConcurrentCreateAllocatesUniqueNumbers(t *testing.T) {
	s, registry, _ := testService(t)
	ctx := context.Background()
	project := createProject(t, registry, adminActor, "concurrent-reviews", projects.VisibilityPublic)

	const creators = 8
	var wg sync.WaitGroup
	numbers := make(chan int64, creators)
	errs := make(chan error, creators)
	for i := 0; i < creators; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			review, err := s.Create(ctx, adminActor, project.ID, Input{Subject: fmt.Sprintf("parallel request %d", i)})
			if err != nil {
				errs <- err
				return
			}
			numbers <- review.Number
		}(i)
	}
	wg.Wait()
	close(numbers)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent create: %v", err)
	}
	seen := map[int64]bool{}
	for number := range numbers {
		if seen[number] {
			t.Fatalf("duplicate review number %d", number)
		}
		seen[number] = true
	}
	if len(seen) != creators {
		t.Fatalf("numbers = %v, want %d distinct", seen, creators)
	}
	for i := int64(1); i <= creators; i++ {
		if !seen[i] {
			t.Fatalf("missing review number %d in %v", i, seen)
		}
	}
}
