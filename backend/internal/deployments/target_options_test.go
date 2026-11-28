package deployments

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestTargetOptionsListsActiveTargets covers the read-only picker the deploy
// form uses: active targets only, ordered by environment then name, and never
// carrying secret material or admin-only fields.
func TestTargetOptionsListsActiveTargets(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()

	disabled, err := s.Create(ctx, testActor, Input{Name: "zz-disabled", Endpoint: "https://dev.example.com", Environment: EnvironmentDevelopment, Status: StatusDisabled})
	if err != nil {
		t.Fatal(err)
	}
	production, err := s.Create(ctx, testActor, Input{Name: "prod-target", Endpoint: "https://prod.example.com", Environment: EnvironmentProduction})
	if err != nil {
		t.Fatal(err)
	}
	staging, err := s.Create(ctx, testActor, Input{Name: "staging-target", Endpoint: "https://staging.example.com", Environment: EnvironmentStaging, Secret: "option-secret"})
	if err != nil {
		t.Fatal(err)
	}

	options, err := s.TargetOptions(ctx, testActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 2 {
		t.Fatalf("options = %+v, want the two active targets", options)
	}
	if options[0].ID != production.ID || options[1].ID != staging.ID {
		t.Fatalf("order = %+v, want production then staging", options)
	}
	if options[1].Name != "staging-target" || options[1].Environment != EnvironmentStaging || options[1].Endpoint != "https://staging.example.com" {
		t.Fatalf("staging option = %+v", options[1])
	}
	for _, option := range options {
		if option.ID == disabled.ID {
			t.Fatal("a disabled target leaked into the options")
		}
	}
	data, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "option-secret") {
		t.Fatalf("options leaked a credential: %s", data)
	}
}
