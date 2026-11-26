package httpapi

import (
	"encoding/json"
	"testing"
)

// TestGlobalReviewListRoutes covers GET /api/v1/reviews: authentication, the
// visibility matrix (admin all; member public + own project; private invisible),
// the projectId/status filters and pagination validation. It also pins the
// non-enumerable projectId behavior: filtering by an invisible project is an
// empty page, matching GET /api/v1/deployments.
func TestGlobalReviewListRoutes(t *testing.T) {
	h, _, db := reviewTestServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	createUserHTTP(t, h, adminToken, "global-review-member", "member-password-01", "member")
	createUserHTTP(t, h, adminToken, "global-review-viewer", "viewer-password-1", "viewer")
	memberToken, _, _ := loginHTTP(t, h, "global-review-member", "member-password-01")
	viewerToken, _, _ := loginHTTP(t, h, "global-review-viewer", "viewer-password-1")

	publicID := createProjectHTTP(t, h, adminToken, "global-review-public", "public")
	privateID := createProjectHTTP(t, h, adminToken, "global-review-private", "private")
	ownPrivateID := createProjectHTTP(t, h, memberToken, "global-review-own", "private")

	publicOne := createReviewHTTP(t, h, adminToken, publicID, "public one")
	publicTwo := createReviewHTTP(t, h, adminToken, publicID, "public two")
	privateReview := createReviewHTTP(t, h, adminToken, privateID, "private one")
	ownReview := createReviewHTTP(t, h, memberToken, ownPrivateID, "own private one")
	// Pin creation times so "newest first" is deterministic (the column has
	// second resolution and these rows are created in the same second).
	for i, id := range []string{publicOne.ID, publicTwo.ID, privateReview.ID, ownReview.ID} {
		if _, err := db.Exec(`UPDATE reviews SET created_at=$2 WHERE id=$1`, id, int64(1_700_000_000+i)); err != nil {
			t.Fatal(err)
		}
	}
	if w := request(h, "POST", "/api/v1/reviews/"+publicOne.ID+"/approve", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("approve review: %d %s", w.Code, w.Body.String())
	}

	// The endpoint requires a Bearer token.
	if w := request(h, "GET", "/api/v1/reviews", "", ""); w.Code != 401 || errorCode(t, w) != "unauthorized" {
		t.Fatalf("unauthenticated list: %d %s", w.Code, w.Body.String())
	}

	// Admin sees every review; the member sees the public project's reviews and
	// its own private project's review; the viewer sees only public reviews.
	adminPage := parseReviewPageHTTP(t, request(h, "GET", "/api/v1/reviews", "", adminToken).Body.Bytes())
	if adminPage.Data.Total != 4 || len(adminPage.Data.Items) != 4 {
		t.Fatalf("admin list = %+v", adminPage.Data)
	}
	memberPage := parseReviewPageHTTP(t, request(h, "GET", "/api/v1/reviews", "", memberToken).Body.Bytes())
	if memberPage.Data.Total != 3 || len(memberPage.Data.Items) != 3 {
		t.Fatalf("member list = %+v", memberPage.Data)
	}
	for _, review := range memberPage.Data.Items {
		if review.ID == privateReview.ID {
			t.Fatalf("member saw another private project's review %s", review.ID)
		}
	}
	if memberPage.Data.Items[0].ID != ownReview.ID {
		// Newest first: the own review was created last in this test.
		t.Fatalf("member newest review = %s, want %s", memberPage.Data.Items[0].ID, ownReview.ID)
	}
	viewerPage := parseReviewPageHTTP(t, request(h, "GET", "/api/v1/reviews", "", viewerToken).Body.Bytes())
	if viewerPage.Data.Total != 2 || len(viewerPage.Data.Items) != 2 {
		t.Fatalf("viewer list = %+v", viewerPage.Data)
	}
	for _, review := range viewerPage.Data.Items {
		if review.ProjectID != publicID {
			t.Fatalf("viewer saw project %s, want only the public project", review.ProjectID)
		}
	}

	// Filters: projectId and status narrow the visible set.
	filtered := parseReviewPageHTTP(t, request(h, "GET", "/api/v1/reviews?projectId="+publicID, "", viewerToken).Body.Bytes())
	if filtered.Data.Total != 2 || len(filtered.Data.Items) != 2 {
		t.Fatalf("public project filter = %+v", filtered.Data)
	}
	approved := parseReviewPageHTTP(t, request(h, "GET", "/api/v1/reviews?status=approved", "", viewerToken).Body.Bytes())
	if approved.Data.Total != 1 || approved.Data.Items[0].ID != publicOne.ID {
		t.Fatalf("approved filter = %+v", approved.Data)
	}
	pending := parseReviewPageHTTP(t, request(h, "GET", "/api/v1/reviews?status=pending", "", viewerToken).Body.Bytes())
	if pending.Data.Total != 1 || pending.Data.Items[0].ID != publicTwo.ID {
		t.Fatalf("pending filter = %+v", pending.Data)
	}
	ownFiltered := parseReviewPageHTTP(t, request(h, "GET", "/api/v1/reviews?projectId="+ownPrivateID, "", memberToken).Body.Bytes())
	if ownFiltered.Data.Total != 1 || ownFiltered.Data.Items[0].ID != ownReview.ID {
		t.Fatalf("own project filter = %+v", ownFiltered.Data)
	}
	// A visible project filter cannot be combined into somebody else's results:
	// the admin-only project is an empty page for the viewer, never a 404 that
	// would reveal whether the id exists.
	invisible := request(h, "GET", "/api/v1/reviews?projectId="+privateID, "", viewerToken)
	if invisible.Code != 200 {
		t.Fatalf("invisible project filter status = %d %s", invisible.Code, invisible.Body.String())
	}
	if page := parseReviewPageHTTP(t, invisible.Body.Bytes()); page.Data.Total != 0 || len(page.Data.Items) != 0 {
		t.Fatalf("invisible project filter leaked: %+v", page.Data)
	}
	// The admin can filter the private project explicitly.
	adminPrivate := parseReviewPageHTTP(t, request(h, "GET", "/api/v1/reviews?projectId="+privateID, "", adminToken).Body.Bytes())
	if adminPrivate.Data.Total != 1 || adminPrivate.Data.Items[0].ID != privateReview.ID {
		t.Fatalf("admin private filter = %+v", adminPrivate.Data)
	}

	// Pagination is reported and validated with the shared page bounds.
	page := request(h, "GET", "/api/v1/reviews?limit=2&offset=1", "", adminToken)
	if page.Code != 200 {
		t.Fatalf("paged list: %d %s", page.Code, page.Body.String())
	}
	var paged struct {
		Data struct {
			Items  []struct{ ID string } `json:"items"`
			Total  int                   `json:"total"`
			Limit  int                   `json:"limit"`
			Offset int                   `json:"offset"`
		} `json:"data"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &paged); err != nil {
		t.Fatal(err)
	}
	if paged.Data.Total != 4 || len(paged.Data.Items) != 2 || paged.Data.Limit != 2 || paged.Data.Offset != 1 {
		t.Fatalf("paged payload = %+v", paged.Data)
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?offset=-1"} {
		if w := request(h, "GET", "/api/v1/reviews"+query, "", adminToken); w.Code != 400 || errorCode(t, w) != "invalid_request" {
			t.Fatalf("invalid pagination %q: %d %s", query, w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"?status=deleted", "?projectId=short"} {
		if w := request(h, "GET", "/api/v1/reviews"+query, "", adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
			t.Fatalf("invalid filter %q: %d %s", query, w.Code, w.Body.String())
		}
	}
}
