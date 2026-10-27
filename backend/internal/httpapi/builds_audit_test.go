package httpapi

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/yichenfchai/AegisFlow/backend/internal/builds"
)

// auditSleepCommand keeps a build running long enough to cancel it on any OS.
func auditSleepCommand(seconds int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("ping -n %d 127.0.0.1 >nul", seconds+1)
	}
	return fmt.Sprintf("sleep %d", seconds)
}

func TestBuildOperationsAppearInAudit(t *testing.T) {
	h, _ := testServer(t)
	adminToken, _, _ := loginHTTP(t, h, "admin", "a-strong-password")
	projectID := createBuildableProject(t, h, adminToken, "audit-build", "echo audited")
	buildID := triggerBuild(t, h, adminToken, projectID, `{}`)
	waitForHTTPBuild(t, h, adminToken, buildID, builds.StatusSucceeded)

	// Cancel a long-running build so the second auditable action happens too.
	if w := request(h, "PATCH", "/api/v1/projects/"+projectID, `{"buildCommand":"`+auditSleepCommand(30)+`"}`, adminToken); w.Code != 200 {
		t.Fatalf("set long command: %d %s", w.Code, w.Body.String())
	}
	longID := triggerBuild(t, h, adminToken, projectID, `{}`)
	waitForHTTPBuild(t, h, adminToken, longID, builds.StatusRunning)
	if w := request(h, "POST", "/api/v1/builds/"+longID+"/cancel", `{}`, adminToken); w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	waitForHTTPBuild(t, h, adminToken, longID, builds.StatusCancelled)

	w := request(h, "GET", "/api/v1/audit?limit=50&offset=0", "", adminToken)
	if w.Code != 200 {
		t.Fatalf("audit: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`"action":"build.trigger"`,
		`"action":"build.cancel"`,
		`"objectType":"build"`,
		`"objectId":"` + buildID + `"`,
		`"actorUsername":"admin"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("audit response lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "echo audited") {
		t.Fatal("audit response leaked the build command")
	}

	// A viewer still cannot read the audit trail.
	createUserHTTP(t, h, adminToken, "audit-viewer", "viewer-password-1", "viewer")
	viewerToken, _, _ := loginHTTP(t, h, "audit-viewer", "viewer-password-1")
	if w = request(h, "GET", "/api/v1/audit", "", viewerToken); w.Code != 403 {
		t.Fatalf("viewer audit: %d", w.Code)
	}
}
