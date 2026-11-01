package httpapi

import (
	"os"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestOpenAPIContractReferencesAndRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if document["openapi"] != "3.0.3" {
		t.Fatal("unexpected OpenAPI version")
	}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if reference, ok := value["$ref"].(string); ok {
				if !strings.HasPrefix(reference, "#/") {
					t.Fatalf("external reference not expected: %s", reference)
				}
				var target any = document
				for _, part := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
					object, ok := target.(map[string]any)
					if !ok {
						t.Fatalf("invalid reference: %s", reference)
					}
					target, ok = object[part]
					if !ok {
						t.Fatalf("missing reference: %s", reference)
					}
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(document)
	// Flow-style YAML descriptions containing commas must be quoted; otherwise
	// YAML can parse successfully while adding bogus JSON Schema keywords.
	components := document["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	tokenData := schemas["TokenData"].(map[string]any)
	properties := tokenData["properties"].(map[string]any)
	expiresIn := properties["expiresIn"].(map[string]any)
	if len(expiresIn) != 4 || expiresIn["description"] != "Access lifetime in seconds, capped by the absolute session deadline" {
		t.Fatalf("invalid expiresIn schema: %+v", expiresIn)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths")
	}
	for _, route := range []struct{ path, method string }{
		{"/healthz", "get"}, {"/readyz", "get"}, {"/metrics", "get"}, {"/api/v1/auth/login", "post"}, {"/api/v1/auth/refresh", "post"},
		{"/api/v1/auth/logout", "post"}, {"/api/v1/auth/me", "get"}, {"/api/v1/auth/password", "post"},
		{"/api/v1/auth/sessions", "get"}, {"/api/v1/auth/sessions", "delete"}, {"/api/v1/auth/sessions/{sessionId}", "delete"},
		{"/api/v1/users", "get"}, {"/api/v1/users", "post"}, {"/api/v1/users/{userId}", "patch"},
		{"/api/v1/users/{userId}/password-reset", "post"}, {"/api/v1/audit", "get"},
		{"/api/v1/projects", "get"}, {"/api/v1/projects", "post"}, {"/api/v1/projects/{projectId}", "get"},
		{"/api/v1/projects/{projectId}", "patch"}, {"/api/v1/projects/{projectId}/builds", "post"},
		{"/api/v1/projects/{projectId}/members", "get"}, {"/api/v1/projects/{projectId}/members", "post"},
		{"/api/v1/projects/{projectId}/members/{userId}", "patch"}, {"/api/v1/projects/{projectId}/members/{userId}", "delete"},
		{"/api/v1/builds", "get"}, {"/api/v1/builds/{buildId}", "get"}, {"/api/v1/builds/{buildId}/logs", "get"},
		{"/api/v1/builds/{buildId}/events", "get"}, {"/api/v1/builds/{buildId}/cancel", "post"},
		{"/api/v1/builds/{buildId}/retry", "post"},
		{"/api/v1/deployment-targets", "get"}, {"/api/v1/deployment-targets", "post"},
		{"/api/v1/deployment-targets/{targetId}", "get"}, {"/api/v1/deployment-targets/{targetId}", "patch"},
		{"/api/v1/deployment-targets/{targetId}", "delete"}, {"/api/v1/deployment-targets/{targetId}/check", "post"},
		{"/api/v1/projects/{projectId}/deployments", "get"}, {"/api/v1/projects/{projectId}/deployments", "post"},
		{"/api/v1/deployments", "get"},
		{"/api/v1/deployments/{deploymentId}", "get"}, {"/api/v1/deployments/{deploymentId}/events", "get"},
		{"/api/v1/deployments/{deploymentId}/cancel", "post"}, {"/api/v1/deployments/{deploymentId}/rollback", "post"},
		{"/api/v1/approvals", "get"}, {"/api/v1/approvals/{approvalId}/approve", "post"},
		{"/api/v1/approvals/{approvalId}/reject", "post"},
	} {
		item, ok := paths[route.path].(map[string]any)
		if !ok || item[route.method] == nil {
			t.Fatalf("undocumented route %s %s", route.method, route.path)
		}
	}
}
