package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/auth"
)

func loginHTTP(t *testing.T, h http.Handler, username, password string) (token, userID string, cookie *http.Cookie) {
	t.Helper()
	w := request(h, "POST", "/api/v1/auth/login", `{"username":"`+username+`","password":"`+password+`"}`, "")
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", username, w.Code, w.Body.String())
	}
	var result struct {
		Data auth.Tokens `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Data.AccessToken, result.Data.User.ID, w.Result().Cookies()[0]
}

func createUserHTTP(t *testing.T, h http.Handler, adminToken, username, password, role string) string {
	t.Helper()
	w := request(h, "POST", "/api/v1/users", `{"username":"`+username+`","password":"`+password+`","role":"`+role+`"}`, adminToken)
	if w.Code != 201 {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Data auth.User `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Data.ID
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var result struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid error body: %s", w.Body.String())
	}
	return result.Error.Code
}

func TestSelfServicePasswordAndSessions(t *testing.T) {
	h, _ := testServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	memberID := createUserHTTP(t, h, adminToken, "member", "member-password-01", "member")
	memberToken, _, memberCookie := loginHTTP(t, h, "member", "member-password-01")
	// Non-admins cannot reach account-management admin surfaces.
	for _, route := range []struct {
		method, path, body string
	}{{"PATCH", "/api/v1/users/" + memberID, `{"role":"viewer"}`}, {"POST", "/api/v1/users/" + memberID + "/password-reset", `{"password":"x-password-123456"}`}, {"GET", "/api/v1/audit", ""}} {
		if w := request(h, route.method, route.path, route.body, memberToken); w.Code != 403 {
			t.Fatalf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
		}
	}
	if w := request(h, "POST", "/api/v1/auth/password", `{"currentPassword":"wrong-current-xx","newPassword":"member-new-pass-1"}`, memberToken); w.Code != 401 {
		t.Fatalf("wrong current password: %d %s", w.Code, w.Body.String())
	}
	// A second browser session survives until the password changes.
	otherToken, _, otherCookie := loginHTTP(t, h, "member", "member-password-01")
	if w := request(h, "POST", "/api/v1/auth/password", `{"currentPassword":"member-password-01","newPassword":"member-new-pass-1"}`, otherToken); w.Code != 200 {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/api/v1/auth/me", "", otherToken); w.Code != 200 {
		t.Fatalf("changing session invalidated: %d", w.Code)
	}
	refreshed := cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, otherCookie)
	if refreshed.Code != 200 {
		t.Fatalf("changing session refresh: %d", refreshed.Code)
	}
	// Refresh rotates the cookie; reuse of the consumed value would be a replay.
	otherCookie = refreshed.Result().Cookies()[0]
	if w := request(h, "GET", "/api/v1/auth/me", "", memberToken); w.Code != 401 {
		t.Fatalf("other session not revoked: %d", w.Code)
	}
	if w := cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, memberCookie); w.Code != 401 {
		t.Fatalf("other session cookie not revoked: %d", w.Code)
	}
	// Session listing marks the current session.
	w := request(h, "GET", "/api/v1/auth/sessions", "", otherToken)
	if w.Code != 200 {
		t.Fatalf("list sessions: %d %s", w.Code, w.Body.String())
	}
	var list struct {
		Data []auth.SessionInfo `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || !list.Data[0].Current {
		t.Fatalf("sessions after password change: %+v", list.Data)
	}
	// Revoke all other sessions keeps the caller alive.
	_, _, thirdCookie := loginHTTP(t, h, "member", "member-new-pass-1")
	w = request(h, "DELETE", "/api/v1/auth/sessions", "", otherToken)
	if w.Code != 200 {
		t.Fatalf("revoke others: %d %s", w.Code, w.Body.String())
	}
	var revoked struct {
		Data struct {
			Revoked int `json:"revoked"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &revoked); err != nil || revoked.Data.Revoked != 1 {
		t.Fatalf("revoke others count: %s", w.Body.String())
	}
	if w = cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, thirdCookie); w.Code != 401 {
		t.Fatalf("third session refresh after revoke others: %d", w.Code)
	}
	if w = cookieRequest(h, "POST", "/api/v1/auth/refresh", `{}`, otherCookie); w.Code != 200 {
		t.Fatalf("current session lost by revoke others: %d", w.Code)
	}
	// Revoke a single session by id.
	loginHTTP(t, h, "member", "member-new-pass-1")
	w = request(h, "GET", "/api/v1/auth/sessions", "", otherToken)
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("expected two sessions: %+v", list.Data)
	}
	victim := ""
	for _, session := range list.Data {
		if !session.Current {
			victim = session.ID
		}
	}
	if victim == "" {
		t.Fatal("no non-current session")
	}
	if w = request(h, "DELETE", "/api/v1/auth/sessions/"+victim, "", otherToken); w.Code != 200 {
		t.Fatalf("revoke session by id: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "DELETE", "/api/v1/auth/sessions/missing-0000000000000001", "", otherToken); w.Code != 404 {
		t.Fatalf("unknown session id: %d", w.Code)
	}
	if w = request(h, "DELETE", "/api/v1/auth/sessions/short", "", otherToken); w.Code != 400 {
		t.Fatalf("malformed session id: %d", w.Code)
	}
}

func TestAdminUserManagementAndAudit(t *testing.T) {
	h, _ := testServer(t)
	adminToken, adminID, _ := loginHTTP(t, h, "admin", "a-strong-password")
	memberID := createUserHTTP(t, h, adminToken, "member-2", "member-password-02", "member")
	if w := request(h, "PATCH", "/api/v1/users/"+memberID, `{"role":"viewer"}`, adminToken); w.Code != 200 {
		t.Fatalf("patch role: %d %s", w.Code, w.Body.String())
	}
	// Pagination and ordering (admin < member-2 case-insensitively).
	type userPage struct {
		Data struct {
			Items []auth.User `json:"items"`
			Total int         `json:"total"`
		} `json:"data"`
	}
	w := request(h, "GET", "/api/v1/users?limit=1&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("users page: %d %s", w.Code, w.Body.String())
	}
	var page userPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Total != 2 || len(page.Data.Items) != 1 || page.Data.Items[0].Username != "admin" {
		t.Fatalf("users page 1: %s", w.Body.String())
	}
	w = request(h, "GET", "/api/v1/users?limit=1&offset=1", "", adminToken)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Data.Items[0].Username != "member-2" || page.Data.Items[0].Role != "viewer" {
		t.Fatalf("users page 2: %s", w.Body.String())
	}
	for _, query := range []string{"limit=0", "limit=101", "offset=-1", "limit=abc"} {
		if w = request(h, "GET", "/api/v1/users?"+query, "", adminToken); w.Code != 400 {
			t.Fatalf("bad pagination %s: %d", query, w.Code)
		}
	}
	if w = request(h, "PATCH", "/api/v1/users/"+memberID, `{}`, adminToken); w.Code != 400 {
		t.Fatalf("empty patch accepted: %d", w.Code)
	}
	if w = request(h, "PATCH", "/api/v1/users/missing-0000000000000001", `{"role":"viewer"}`, adminToken); w.Code != 404 {
		t.Fatalf("unknown user patch: %d", w.Code)
	}
	// Disabling revokes sessions and blocks login without leaking state.
	memberToken, _, _ := loginHTTP(t, h, "member-2", "member-password-02")
	if w = request(h, "PATCH", "/api/v1/users/"+memberID, `{"disabled":true}`, adminToken); w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/auth/me", "", memberToken); w.Code != 401 {
		t.Fatalf("disabled member still authenticated: %d", w.Code)
	}
	w = request(h, "POST", "/api/v1/auth/login", `{"username":"member-2","password":"member-password-02"}`, "")
	if w.Code != 403 || errorCode(t, w) != "account_disabled" {
		t.Fatalf("disabled login: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "PATCH", "/api/v1/users/"+memberID, `{"disabled":false}`, adminToken); w.Code != 200 {
		t.Fatalf("enable: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/auth/login", `{"username":"member-2","password":"member-password-02"}`, ""); w.Code != 200 {
		t.Fatalf("enabled login: %d %s", w.Code, w.Body.String())
	}
	// Reset revokes the member's sessions and sets the new password.
	if w = request(h, "POST", "/api/v1/users/"+memberID+"/password-reset", `{"password":"reset-password-03"}`, adminToken); w.Code != 200 {
		t.Fatalf("reset password: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/api/v1/auth/login", `{"username":"member-2","password":"member-password-02"}`, ""); w.Code != 401 {
		t.Fatalf("old password after reset: %d", w.Code)
	}
	if w = request(h, "POST", "/api/v1/auth/login", `{"username":"member-2","password":"reset-password-03"}`, ""); w.Code != 200 {
		t.Fatalf("reset password login: %d %s", w.Code, w.Body.String())
	}
	// The last enabled administrator cannot be disabled or demoted.
	for _, body := range []string{`{"disabled":true}`, `{"role":"viewer"}`} {
		if w = request(h, "PATCH", "/api/v1/users/"+adminID, body, adminToken); w.Code != 400 || errorCode(t, w) != "validation_failed" {
			t.Fatalf("last admin protection %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	// The audit trail records the operations and stays admin-only.
	w = request(h, "GET", "/api/v1/audit?limit=100&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("audit list: %d %s", w.Code, w.Body.String())
	}
	var audit struct {
		Data struct {
			Items []auth.AuditEntry `json:"items"`
			Total int               `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &audit); err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, entry := range audit.Data.Items {
		actions[entry.Action] = true
		if entry.Result != "success" && entry.Result != "failure" {
			t.Fatalf("invalid audit result: %+v", entry)
		}
	}
	for _, action := range []string{auth.ActionLogin, auth.ActionUserDisable, auth.ActionUserEnable, auth.ActionPasswordReset, auth.ActionRoleChange, auth.ActionUserCreate} {
		if !actions[action] {
			t.Fatalf("audit missing action %s: %+v", action, actions)
		}
	}
	// Server-side filters narrow both the page and the total; the query parser
	// accepts RFC3339 and date-only boundaries.
	filtered := func(query string) (int, []auth.AuditEntry) {
		t.Helper()
		resp := request(h, "GET", "/api/v1/audit?limit=100&offset=0&"+query, "", adminToken)
		if resp.Code != 200 {
			t.Fatalf("audit filter %q: %d %s", query, resp.Code, resp.Body.String())
		}
		var payload struct {
			Data struct {
				Items []auth.AuditEntry `json:"items"`
				Total int               `json:"total"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Data.Total, payload.Data.Items
	}
	if total, items := filtered("action=" + auth.ActionUserCreate); total == 0 || len(items) == 0 {
		t.Fatalf("action filter returned nothing: total=%d", total)
	} else {
		for _, entry := range items {
			if entry.Action != auth.ActionUserCreate {
				t.Fatalf("action filter leaked %q", entry.Action)
			}
		}
	}
	if total, _ := filtered("result=failure"); total != 0 {
		t.Fatalf("failure filter expected no rows, got %d", total)
	}
	if total, _ := filtered("from=2000-01-01&to=2999-12-31"); total == 0 {
		t.Fatalf("date range returned nothing")
	}
	if total, _ := filtered("from=2999-01-01"); total != 0 {
		t.Fatalf("future range expected no rows, got %d", total)
	}
	if w = request(h, "GET", "/api/v1/audit?from=not-a-date", "", adminToken); w.Code != 400 || errorCode(t, w) != "invalid_request" {
		t.Fatalf("bad from: %d %s", w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/v1/audit?result=maybe", "", adminToken); w.Code != 400 {
		t.Fatalf("bad result filter: %d %s", w.Code, w.Body.String())
	}
	// Role and disabled changes in one PATCH commit together.
	if w = request(h, "PATCH", "/api/v1/users/"+memberID, `{"role":"viewer","disabled":true}`, adminToken); w.Code != 200 {
		t.Fatalf("combined patch: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "GET", "/api/v1/users?limit=100&offset=0", "", adminToken)
	page = userPage{}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range page.Data.Items {
		if u.ID == memberID {
			found = true
			if u.Role != "viewer" || !u.Disabled {
				t.Fatalf("combined patch not applied: %+v", u)
			}
		}
	}
	if !found {
		t.Fatal("patched member missing from list")
	}
	if w = request(h, "POST", "/api/v1/auth/login", `{"username":"member-2","password":"reset-password-03"}`, ""); w.Code != 403 {
		t.Fatalf("combined disable did not block login: %d", w.Code)
	}
}
