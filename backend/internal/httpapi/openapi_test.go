package httpapi

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

// openAPIMethods are the path-item keys that declare an operation. Anything
// else in a path item (summary, parameters, $ref, ...) is not an operation.
var openAPIMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// publicOperations are the operations that may omit a Bearer security
// requirement: login/refresh/logout rely on the Origin check (and, for
// refresh/logout, the refresh cookie), and the health endpoints are not
// session-scoped. Everything else under /api/v1 must document a security
// requirement. /healthz and /readyz are outside the /api/v1 prefix, but they are
// listed so the public exception set is explicit rather than implied.
var publicOperations = map[string]bool{
	"POST /api/v1/auth/login":   true,
	"POST /api/v1/auth/refresh": true,
	"POST /api/v1/auth/logout":  true,
	"GET /healthz":              true,
	"GET /readyz":               true,
}

// metricsOperation is documented but not registered on the API engine: /metrics
// is served by a separate listener in cmd/server (AEGIS_METRICS_ADDR). It is
// the only allowed "documented but not in engine.Routes()" operation.
const metricsOperation = "GET /metrics"

// contractEngine builds the real Gin engine through the production constructor
// that server_test.go's testServer also wraps. Only the route table is inspected,
// so the business services (and therefore PostgreSQL/Redis) are not needed here:
// this contract check must stay runnable without external services.
func contractEngine(t *testing.T) *gin.Engine {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(nil, logger, func(context.Context) error { return nil }, []string{"http://localhost:5173"})
}

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

	// Documented operations, keyed by "METHOD /canonical/path". Parameter names
	// differ between the contract ({buildId}) and Gin (:id), so both sides are
	// canonicalized to "{}" before the two-way set comparison.
	documented := make(map[string]string, len(paths))
	for path, rawItem := range paths {
		item, ok := rawItem.(map[string]any)
		if !ok {
			t.Fatalf("invalid path item %s", path)
		}
		for method, rawOperation := range item {
			method = strings.ToUpper(method)
			if !openAPIMethods[strings.ToLower(method)] {
				continue
			}
			operation, ok := rawOperation.(map[string]any)
			if !ok {
				t.Fatalf("invalid operation %s %s", method, path)
			}
			key := method + " " + canonicalOpenAPIPath(path)
			documented[key] = method + " " + path
			if !strings.HasPrefix(path, "/api/v1") || publicOperations[method+" "+path] {
				continue
			}
			security, ok := operation["security"]
			if !ok {
				t.Errorf("%s %s documents no security scheme; authenticated /api/v1 operations must require bearerAuth", method, path)
				continue
			}
			requirements, ok := security.([]any)
			if !ok || len(requirements) == 0 {
				t.Errorf("%s %s must declare a non-empty security requirement", method, path)
			}
		}
	}

	// Registered operations from the real Gin engine.
	registered := make(map[string]string)
	for _, route := range contractEngine(t).Routes() {
		registered[route.Method+" "+canonicalGinPath(route.Path)] = route.Method + " " + route.Path
	}

	// Two-way comparison: a route the engine registers but the contract omits
	// fails, and a contract operation the engine does not serve fails too.
	keys := make([]string, 0, len(registered))
	for key := range registered {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := documented[key]; !ok {
			t.Errorf("route registered but missing from openapi.yaml: %s", registered[key])
		}
	}
	keys = keys[:0]
	for key := range documented {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := registered[key]; ok {
			continue
		}
		if key == metricsOperation {
			continue // separate metrics listener, documented but not mounted on this engine
		}
		t.Errorf("operation documented but not registered in the API engine: %s", documented[key])
	}
}

// openAPIPathParameter matches an OpenAPI path template placeholder such as
// {buildId}; Gin spells the same position as :id.
var openAPIPathParameter = regexp.MustCompile(`\{[^/{}]+\}`)

// canonicalOpenAPIPath rewrites every {parameter} placeholder to {}.
func canonicalOpenAPIPath(path string) string {
	return openAPIPathParameter.ReplaceAllString(path, "{}")
}

// canonicalGinPath rewrites Gin's :param and *param segments to {}.
func canonicalGinPath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "*") {
			segments[i] = "{}"
		}
	}
	return strings.Join(segments, "/")
}
