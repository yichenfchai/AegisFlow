package builds

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// dumpEnvCommand prints the environment the shell handed to the command.
func dumpEnvCommand() string {
	if runtime.GOOS == "windows" {
		return "set"
	}
	return "env"
}

func TestBuildCommandEnvIsAllowlisted(t *testing.T) {
	t.Setenv("AEGIS_JWT_SECRET", "leak-canary-secret")
	t.Setenv("AEGIS_DATABASE_URL", "postgres://user:pass@db/aegis")
	env := buildCommandEnv(`C:\tmp\aegis-build-test`)
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"AEGIS_JWT_SECRET", "AEGIS_DATABASE_URL", "leak-canary-secret",
		"postgres://user:pass", "AEGIS_REDIS_URL", "AEGIS_TEST_DATABASE_URL",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("service environment leaked %q into the build environment: %s", forbidden, joined)
		}
	}
	for _, want := range []string{"PATH=", "HOME=", "TMPDIR=", "TMP=", "TEMP="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("build environment lacks %s: %s", want, joined)
		}
	}
}

func TestNewBuildWorkDirIsOwnerOnly(t *testing.T) {
	dir, err := newBuildWorkDir()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if !strings.Contains(filepath.Base(dir), "aegis-build-") {
		t.Fatalf("unexpected work directory name: %s", dir)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("work directory mode = %o, want 700", perm)
		}
	}
}

func TestBuildCommandCannotReadServiceEnv(t *testing.T) {
	s, registry := testService(t)
	startWorker(t, s)
	t.Setenv("AEGIS_JWT_SECRET", "leak-canary-secret")
	t.Setenv("AEGIS_DATABASE_URL", "postgres://user:pass@db/aegis")
	project := createProject(t, registry, "env-project", dumpEnvCommand())
	build, err := s.Trigger(context.Background(), testActor, project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, s, build.ID, StatusSucceeded)
	text := waitForLog(t, s, build.ID, "HOME=")
	for _, forbidden := range []string{
		"AEGIS_JWT_SECRET", "AEGIS_DATABASE_URL", "AEGIS_TEST_DATABASE_URL",
		"AEGIS_REDIS_URL", "leak-canary-secret", "postgres://user:pass",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("build command could read %q from the service environment: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "aegis-build-") {
		t.Fatalf("HOME/TMP are not inside the build directory: %s", text)
	}
}
