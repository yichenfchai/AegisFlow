package deployments

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// P2-08 / decision 5B: a deployment target endpoint may point at a private or
// loopback host (self-hosted deployments are the primary use case) but never at
// a cloud metadata or link-local address, even though admin configures it.
func TestCreateRejectsMetadataAndLinkLocalTargets(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()

	blocked := []string{
		"https://169.254.169.254/",
		"http://169.254.169.254",
		"https://169.254.0.1:8080/",
		"https://[::ffff:169.254.169.254]/",
		"https://100.100.100.200/",
		"https://[fd00:ec2::254]/",
		"https://[fe80::1]/",
		"https://[fe80::1%25eth0]/",
		"https://[fe80::abcd]:8443/",
	}
	for i, endpoint := range blocked {
		if _, err := s.Create(ctx, testActor, validInput(fmt.Sprintf("blocked-%d", i), endpoint)); !errors.Is(err, ErrValidation) {
			t.Fatalf("Create(%q) error = %v, want ErrValidation", endpoint, err)
		}
	}
	if _, err := s.Create(ctx, testActor, validInput("blocked-detail", "https://169.254.169.254/")); !errors.Is(err, ErrValidation) ||
		!strings.Contains(err.Error(), "metadata") || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("blocked endpoint error = %v, want a validation error mentioning metadata/link-local", err)
	}

	allowed := []string{
		"http://127.0.0.1:8080",
		"https://10.0.0.5/",
		"https://172.16.1.10:8443/",
		"https://192.168.1.10/",
		"https://[::1]:8443/",
	}
	for i, endpoint := range allowed {
		if _, err := s.Create(ctx, testActor, validInput(fmt.Sprintf("allowed-%d", i), endpoint)); err != nil {
			t.Fatalf("Create(%q) = %v, want success", endpoint, err)
		}
	}
}

func TestUpdateRejectsMetadataTarget(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	target, err := s.Create(ctx, testActor, validInput("moves-to-metadata", "https://example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Endpoint: str("http://169.254.169.254/")}); !errors.Is(err, ErrValidation) {
		t.Fatalf("update to metadata endpoint error = %v, want ErrValidation", err)
	}
	if _, err = s.Update(ctx, testActor, target.ID, Patch{Endpoint: str("https://10.0.0.5/")}); err != nil {
		t.Fatalf("update to private endpoint = %v, want success", err)
	}
}

func TestBlockedTargetIP(t *testing.T) {
	blocked := []string{
		"169.254.169.254",
		"169.254.0.1",
		"169.254.255.255",
		"::ffff:169.254.169.254",
		"100.100.100.200",
		"::ffff:100.100.100.200",
		"fe80::1",
		"fe80::",
		"fe80::abcd",
		"fd00:ec2::254",
	}
	for _, value := range blocked {
		if ip := net.ParseIP(value); !blockedTargetIP(ip) {
			t.Fatalf("blockedTargetIP(%s) = false, want true", value)
		}
	}
	allowed := []string{
		"127.0.0.1",
		"::1",
		"10.0.0.5",
		"172.16.1.10",
		"172.31.255.255",
		"192.168.1.10",
		"8.8.8.8",
		"169.253.255.255",
		"169.255.0.0",
		"100.100.100.201",
		"fec0::1",
		"fd00:ec2::255",
	}
	for _, value := range allowed {
		if ip := net.ParseIP(value); blockedTargetIP(ip) {
			t.Fatalf("blockedTargetIP(%s) = true, want false", value)
		}
	}
	if blockedTargetIP(nil) {
		t.Fatal("blockedTargetIP(nil) = true, want false")
	}
}

func TestTargetDialControl(t *testing.T) {
	rejected := []string{
		"169.254.169.254:80",
		"169.254.0.1:8080",
		"[fe80::1]:443",
		"[fe80::1%eth0]:443",
		"[fd00:ec2::254]:80",
		"100.100.100.200:80",
		"not-an-ip:80",
		"no-port",
	}
	for _, address := range rejected {
		err := targetDialControl("tcp", address, nil)
		if !errors.Is(err, errBlockedTargetAddress) {
			t.Fatalf("targetDialControl(%q) error = %v, want errBlockedTargetAddress", address, err)
		}
	}
	allowed := []string{
		"127.0.0.1:8080",
		"[::1]:443",
		"10.0.0.5:80",
		"172.16.1.10:8443",
		"192.168.1.10:80",
		"8.8.8.8:443",
	}
	for _, address := range allowed {
		if err := targetDialControl("tcp", address, nil); err != nil {
			t.Fatalf("targetDialControl(%q) = %v, want nil", address, err)
		}
	}
}

// The guarded transport must be direct (a proxy would hide the target IP) and
// dial through the dialer whose Control hook refuses blocked peers. The hook is
// exercised directly instead of by dialing a real metadata address: a test must
// never send traffic there, not even if the guard regresses.
func TestGuardedTransportConfiguration(t *testing.T) {
	transport := guardedTransport()
	if transport.Proxy != nil {
		t.Fatal("guarded transport must not use a proxy: the dial guard needs the target IP")
	}
	if transport.DialContext == nil {
		t.Fatal("guarded transport has no DialContext")
	}
	dialer := newTargetDialer()
	if dialer.Control == nil {
		t.Fatal("guarded dialer has no Control hook")
	}
	if err := dialer.Control("tcp", "169.254.169.254:80", nil); !errors.Is(err, errBlockedTargetAddress) {
		t.Fatalf("dialer Control error = %v, want errBlockedTargetAddress", err)
	}
	if err := dialer.Control("tcp", "10.0.0.5:80", nil); err != nil {
		t.Fatalf("dialer Control on a private address = %v, want nil", err)
	}
}

// Loopback and private hosts stay reachable, which is the self-hosted case the
// decision protects.
func TestGuardedTransportAllowsLoopback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: guardedTransport(), Timeout: 2 * time.Second}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("loopback request = %v, want success", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("loopback status = %d, want 200", response.StatusCode)
	}
}

func TestWithTargetDialGuardKeepsClientSettings(t *testing.T) {
	custom := &http.Client{Timeout: 3 * time.Second}
	guarded := withTargetDialGuard(custom)
	if guarded.Timeout != custom.Timeout {
		t.Fatalf("timeout = %v, want %v", guarded.Timeout, custom.Timeout)
	}
	transport, ok := guarded.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.DialContext == nil {
		t.Fatalf("guarded transport = %T proxy=%v, want a direct *http.Transport with a guarded dialer", guarded.Transport, transport)
	}
	if custom.Transport != nil {
		t.Fatal("withTargetDialGuard mutated the caller's client")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	response, err := guarded.Get(upstream.URL)
	if err != nil {
		t.Fatalf("guarded client loopback request = %v, want success", err)
	}
	defer response.Body.Close()
}
