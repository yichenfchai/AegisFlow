package reviews

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
	"github.com/yichenfchai/AegisFlow/backend/internal/projects"
)

// ageReview moves one review's created_at so global ordering is deterministic
// inside a test (the column has second resolution).
func ageReview(t *testing.T, db *sql.DB, id string, createdAt int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE reviews SET created_at=$2 WHERE id=$1`, id, createdAt); err != nil {
		t.Fatal(err)
	}
}

// TestListVisiblePermissions covers the global list visibility matrix: an admin
// sees every review, while other roles only see reviews of public projects plus
// projects they created or belong to. An invisible project filter yields an
// empty page (never 404), matching the global deployment list.
func TestListVisiblePermissions(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	for _, actor := range []auth.Actor{maintainActor, memberActor, viewerActor, outsiderActor} {
		seedUser(t, db, actor)
	}
	adminPublic := createProject(t, registry, adminActor, "global-public-a", projects.VisibilityPublic)
	adminPublicB := createProject(t, registry, adminActor, "global-public-b", projects.VisibilityPublic)
	adminPrivate := createProject(t, registry, adminActor, "global-private", projects.VisibilityPrivate)
	addMaintainer(t, registry, adminPrivate.ID, maintainActor)
	// memberActor creates its own private project, so "own project" visibility
	// is exercised without membership granted by somebody else.
	ownPrivate := createProject(t, registry, memberActor, "global-own-private", projects.VisibilityPrivate)

	reviews := []*Review{
		createReview(t, s, adminActor, adminPublic.ID),
		createReview(t, s, adminActor, adminPublic.ID),
		createReview(t, s, adminActor, adminPublicB.ID),
		createReview(t, s, adminActor, adminPrivate.ID),
		createReview(t, s, memberActor, ownPrivate.ID),
	}
	base := int64(1_700_000_000)
	for i, review := range reviews {
		ageReview(t, db, review.ID, base+int64(i))
	}
	// reviews[4] newest, reviews[0] oldest.
	order := func(list []Review) []string {
		ids := make([]string, 0, len(list))
		for _, review := range list {
			ids = append(ids, review.ID)
		}
		return ids
	}
	list, total, err := s.ListVisible(ctx, adminActor, ListFilter{Limit: 20})
	if err != nil || total != 5 || len(list) != 5 {
		t.Fatalf("admin list = %d %d err=%v", len(list), total, err)
	}
	got := order(list)
	for i, review := range []*Review{reviews[4], reviews[3], reviews[2], reviews[1], reviews[0]} {
		if got[i] != review.ID {
			t.Fatalf("admin order[%d] = %s, want %s (newest first)", i, got[i], review.ID)
		}
	}

	// memberActor: the two public projects plus its own private project, but not
	// somebody else's private project.
	list, total, err = s.ListVisible(ctx, memberActor, ListFilter{Limit: 20})
	if err != nil || total != 4 || len(list) != 4 {
		t.Fatalf("member list = %d %d err=%v", len(list), total, err)
	}
	for _, review := range list {
		if review.ID == reviews[3].ID {
			t.Fatalf("member saw another private project's review %s", review.ID)
		}
	}
	// A maintainer of the private project does see its review.
	list, total, err = s.ListVisible(ctx, maintainActor, ListFilter{Limit: 20})
	if err != nil || total != 4 || len(list) != 4 {
		t.Fatalf("maintainer list = %d %d err=%v", len(list), total, err)
	}
	foundPrivate := false
	for _, review := range list {
		if review.ID == reviews[3].ID {
			foundPrivate = true
		}
	}
	if !foundPrivate {
		t.Fatal("private project maintainer did not see the private review")
	}
	// Viewers and outsiders see only the public projects' reviews.
	for _, actor := range []auth.Actor{viewerActor, outsiderActor} {
		list, total, err = s.ListVisible(ctx, actor, ListFilter{Limit: 20})
		if err != nil || total != 3 || len(list) != 3 {
			t.Fatalf("%s list = %d %d err=%v, want 3 public reviews", actor.Username, len(list), total, err)
		}
		for _, review := range list {
			if review.ProjectID == adminPrivate.ID || review.ProjectID == ownPrivate.ID {
				t.Fatalf("%s saw invisible project %s", actor.Username, review.ProjectID)
			}
		}
	}
}

// TestListVisibleFiltersAndPagination covers the status/project filters and the
// shared pagination bounds, including the non-enumerable projectId behavior.
func TestListVisibleFiltersAndPagination(t *testing.T) {
	s, registry, db := testService(t)
	ctx := context.Background()
	seedUser(t, db, maintainActor)
	public := createProject(t, registry, adminActor, "global-filter-public", projects.VisibilityPublic)
	addMaintainer(t, registry, public.ID, maintainActor)
	private := createProject(t, registry, adminActor, "global-filter-private", projects.VisibilityPrivate)
	other := createProject(t, registry, adminActor, "global-filter-other", projects.VisibilityPublic)

	created := make([]*Review, 5)
	for i := range created {
		created[i] = createReview(t, s, adminActor, public.ID)
		ageReview(t, db, created[i].ID, int64(1_700_000_000+i))
	}
	if _, err := s.Approve(ctx, maintainActor, created[1].ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reject(ctx, maintainActor, created[3].ID, "needs tests"); err != nil {
		t.Fatal(err)
	}
	// A review of a different public project must not appear under the filter.
	otherReview := createReview(t, s, adminActor, other.ID)
	ageReview(t, db, otherReview.ID, 1_700_000_500)

	page, total, err := s.ListVisible(ctx, adminActor, ListFilter{ProjectID: public.ID, Limit: 2, Offset: 1})
	if err != nil || total != 5 || len(page) != 2 {
		t.Fatalf("project page = %d %d err=%v", len(page), total, err)
	}
	if page[0].ID != created[3].ID || page[1].ID != created[2].ID {
		t.Fatalf("project page order = %s,%s want %s,%s", page[0].ID, page[1].ID, created[3].ID, created[2].ID)
	}
	approved, total, err := s.ListVisible(ctx, adminActor, ListFilter{Status: StatusApproved, Limit: 20})
	if err != nil || total != 1 || len(approved) != 1 || approved[0].ID != created[1].ID {
		t.Fatalf("approved filter = %+v total=%d err=%v", approved, total, err)
	}
	rejected, total, err := s.ListVisible(ctx, adminActor, ListFilter{Status: StatusRejected, Limit: 20})
	if err != nil || total != 1 || len(rejected) != 1 || rejected[0].ID != created[3].ID {
		t.Fatalf("rejected filter = %+v total=%d err=%v", rejected, total, err)
	}
	if _, total, err = s.ListVisible(ctx, adminActor, ListFilter{Status: StatusCancelled, Limit: 20}); err != nil || total != 0 {
		t.Fatalf("cancelled filter total=%d err=%v", total, err)
	}

	// An invisible project filter cannot widen visibility: it is an empty page,
	// not a 404, so the filter cannot be used to probe private project ids.
	outsiderPage, total, err := s.ListVisible(ctx, outsiderActor, ListFilter{ProjectID: private.ID, Limit: 20})
	if err != nil || total != 0 || len(outsiderPage) != 0 {
		t.Fatalf("invisible project filter = %d %d err=%v, want empty page", len(outsiderPage), total, err)
	}
	// A visible public filter still works for the same caller.
	outsiderPage, total, err = s.ListVisible(ctx, outsiderActor, ListFilter{ProjectID: public.ID, Limit: 20})
	if err != nil || total != 5 || len(outsiderPage) != 5 {
		t.Fatalf("visible project filter = %d %d err=%v", len(outsiderPage), total, err)
	}

	// Validation: ids, status values and pagination bounds.
	if _, _, err = s.ListVisible(ctx, adminActor, ListFilter{ProjectID: "bad id", Limit: 20}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid project id error = %v, want ErrValidation", err)
	}
	if _, _, err = s.ListVisible(ctx, adminActor, ListFilter{Status: "deleted", Limit: 20}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid status error = %v, want ErrValidation", err)
	}
	for _, filter := range []ListFilter{{Limit: 0}, {Limit: 101}, {Limit: 20, Offset: -1}} {
		if _, _, err = s.ListVisible(ctx, adminActor, filter); !errors.Is(err, ErrValidation) {
			t.Fatalf("pagination %+v error = %v, want ErrValidation", filter, err)
		}
	}
	// An actor without project-level privileges still sees the public reviews
	// (visibility is not a role matrix) but none of the private ones.
	anonymous, total, err := s.ListVisible(ctx, auth.Actor{ID: "nobody-00000000000001", Username: "nobody", Role: "viewer"}, ListFilter{Limit: 20})
	if err != nil || total != 6 || len(anonymous) != 6 {
		t.Fatalf("unprivileged list = %d %d err=%v", len(anonymous), total, err)
	}
	for _, review := range anonymous {
		if review.ProjectID == private.ID {
			t.Fatalf("unprivileged actor saw invisible project %s", review.ProjectID)
		}
	}
}
