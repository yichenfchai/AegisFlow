package httpapi

import (
	"encoding/json"
	"testing"
)

// TestProjectVisibilityAndMemberRoutes covers the HTTP surface of project
// visibility: private projects are invisible (404, never 403) to non-members,
// membership grants read access, and the creator cannot be removed.
func TestProjectVisibilityAndMemberRoutes(t *testing.T) {
	h, _ := testServer(t)
	adminToken, adminID, _ := loginHTTP(t, h, "admin", "a-strong-password")
	memberID := createUserHTTP(t, h, adminToken, "team-member", "member-password-01", "member")
	viewerID := createUserHTTP(t, h, adminToken, "team-viewer", "viewer-password-1", "viewer")
	_ = createUserHTTP(t, h, adminToken, "team-outsider", "outsider-password-1", "member")
	memberToken, _, _ := loginHTTP(t, h, "team-member", "member-password-01")
	viewerToken, _, _ := loginHTTP(t, h, "team-viewer", "viewer-password-1")
	outsiderToken, _, _ := loginHTTP(t, h, "team-outsider", "outsider-password-1")

	w := request(h, "POST", "/api/v1/projects", `{"name":"secret-lab","visibility":"private"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create private project: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.Visibility != "private" {
		t.Fatalf("created payload: %s", w.Body.String())
	}
	projectID := created.Data.ID
	base := "/api/v1/projects/" + projectID

	// Enumeration defence: every path answers 404 for a non-member, including
	// the member list and the build trigger.
	for _, route := range []struct{ method, path, body string }{
		{"GET", base, ""},
		{"GET", base + "/members", ""},
		{"PATCH", base, `{"description":"nope"}`},
		{"POST", base + "/builds", `{}`},
	} {
		if w = request(h, route.method, route.path, route.body, outsiderToken); w.Code != 404 || errorCode(t, w) != "not_found" {
			t.Fatalf("outsider %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}

	// The creator is the sole maintainer of a new project.
	w = request(h, "GET", base+"/members", "", adminToken)
	var members struct {
		Data []struct {
			UserID   string `json:"userId"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &members); err != nil || len(members.Data) != 1 ||
		members.Data[0].UserID != adminID || members.Data[0].Role != "maintainer" {
		t.Fatalf("initial members: %s", w.Body.String())
	}

	// Membership validation: duplicate 409, unknown account 404, bad role 400.
	if w = request(h, "POST", base+"/members", `{"userId":"`+memberID+`","role":"viewer"}`, adminToken); w.Code != 201 {
		t.Fatalf("add member: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", base+"/members", `{"userId":"`+memberID+`","role":"viewer"}`, adminToken); w.Code != 409 {
		t.Fatalf("duplicate member: %d", w.Code)
	}
	if w = request(h, "POST", base+"/members", `{"userId":"absent-00000000000001","role":"viewer"}`, adminToken); w.Code != 404 {
		t.Fatalf("unknown account: %d", w.Code)
	}
	if w = request(h, "POST", base+"/members", `{"userId":"`+viewerID+`","role":"owner"}`, adminToken); w.Code != 400 {
		t.Fatalf("unknown role: %d", w.Code)
	}

	// Membership grants read access to the private project and its member list.
	if w = request(h, "GET", base, "", memberToken); w.Code != 200 {
		t.Fatalf("member read: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", base+"/members", "", memberToken); w.Code != 200 {
		t.Fatalf("member lists members: %d", w.Code)
	}

	// A global viewer who becomes a project member still cannot write: the
	// global role bounds the action, page guards are not the control.
	if w = request(h, "POST", base+"/members", `{"userId":"`+viewerID+`","role":"viewer"}`, adminToken); w.Code != 201 {
		t.Fatalf("add viewer: %d", w.Code)
	}
	if w = request(h, "GET", base, "", viewerToken); w.Code != 200 {
		t.Fatalf("viewer member read: %d", w.Code)
	}
	if w = request(h, "PATCH", base, `{"description":"nope"}`, viewerToken); w.Code != 403 {
		t.Fatalf("viewer write: %d", w.Code)
	}

	// Role changes and removal; the creator is protected.
	if w = request(h, "PATCH", base+"/members/"+memberID, `{"role":"maintainer"}`, adminToken); w.Code != 200 {
		t.Fatalf("promote member: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "DELETE", base+"/members/"+adminID, "", adminToken); w.Code != 400 {
		t.Fatalf("remove creator: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "DELETE", base+"/members/"+memberID, "", adminToken); w.Code != 200 {
		t.Fatalf("remove member: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", base, "", memberToken); w.Code != 404 {
		t.Fatalf("removed member still reads: %d", w.Code)
	}

	// Public projects keep the pre-membership behaviour for every role.
	if w = request(h, "POST", "/api/v1/projects", `{"name":"open-lab"}`, adminToken); w.Code != 201 {
		t.Fatalf("create public project: %d", w.Code)
	}
	var public struct {
		Data struct {
			ID         string `json:"id"`
			Visibility string `json:"visibility"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil || public.Data.Visibility != "public" {
		t.Fatalf("public payload: %s", w.Body.String())
	}
	for _, token := range []string{viewerToken, outsiderToken} {
		if w = request(h, "GET", "/api/v1/projects/"+public.Data.ID, "", token); w.Code != 200 {
			t.Fatalf("public read: %d", w.Code)
		}
	}
}

// TestPrivateProjectWriteOverlay covers the maintainer overlay over HTTP: an
// invisible project is still 404, a viewer member is 403 on every write, and a
// maintainer gets through.
func TestPrivateProjectWriteOverlay(t *testing.T) {
	h, _ := testServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	memberID := createUserHTTP(t, h, adminToken, "overlay-member", "member-password-01", "member")
	_ = createUserHTTP(t, h, adminToken, "overlay-outsider", "outsider-password-1", "member")
	memberToken, _, _ := loginHTTP(t, h, "overlay-member", "member-password-01")
	outsiderToken, _, _ := loginHTTP(t, h, "overlay-outsider", "outsider-password-1")

	w := request(h, "POST", "/api/v1/projects", `{"name":"overlay-lab","visibility":"private","buildCommand":"echo overlay"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create private project: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.ID == "" {
		t.Fatalf("created payload: %s", w.Body.String())
	}
	base := "/api/v1/projects/" + created.Data.ID

	// A non-member cannot tell the project exists: writes answer 404, not 403.
	if w = request(h, "PATCH", base, `{"description":"nope"}`, outsiderToken); w.Code != 404 || errorCode(t, w) != "not_found" {
		t.Fatalf("outsider write: %d %s", w.Code, w.Body.String())
	}
	// A viewer member sees the project but is forbidden on every write path.
	if w = request(h, "POST", base+"/members", `{"userId":"`+memberID+`","role":"viewer"}`, adminToken); w.Code != 201 {
		t.Fatalf("add viewer: %d %s", w.Code, w.Body.String())
	}
	for _, route := range []struct{ method, path, body string }{
		{"PATCH", base, `{"description":"viewer-write"}`},
		{"POST", base + "/builds", `{}`},
		{"POST", base + "/members", `{"userId":"` + memberID + `","role":"viewer"}`},
		{"PATCH", base + "/members/" + memberID, `{"role":"maintainer"}`},
		{"DELETE", base + "/members/" + memberID, ""},
	} {
		if w = request(h, route.method, route.path, route.body, memberToken); w.Code != 403 || errorCode(t, w) != "forbidden" {
			t.Fatalf("viewer %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	// Promotion to maintainer unlocks the same paths.
	if w = request(h, "PATCH", base+"/members/"+memberID, `{"role":"maintainer"}`, adminToken); w.Code != 200 {
		t.Fatalf("promote: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "PATCH", base, `{"description":"maintainer-write"}`, memberToken); w.Code != 200 {
		t.Fatalf("maintainer patch: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", base+"/builds", `{}`, memberToken); w.Code != 201 {
		t.Fatalf("maintainer trigger: %d %s", w.Code, w.Body.String())
	}
}
