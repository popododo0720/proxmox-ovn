package ovnnb

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type probeRunner struct {
	arguments []string
	err       error
}

func (runner *probeRunner) Run(_ context.Context, _ string, arguments ...string) ([]byte, error) {
	runner.arguments = append([]string(nil), arguments...)
	return nil, runner.err
}

func TestClientRequiresSecureOrLocalEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"tcp:192.0.2.1:6641",
		"ssl:192.0.2.1:6641,unix:/tmp/x",
		" ssl:192.0.2.1:6641",
		"ssl:192.0.2.1",
		"ssl:bad host:6641",
		"ssl:2001:db8::1:6641",
		"unix:relative.sock",
		"unix:/",
	} {
		if _, err := NewClient(ClientConfig{Database: []string{endpoint}}); err == nil {
			t.Fatalf("unsafe endpoint %q was accepted", endpoint)
		}
	}
	if _, err := NewClient(ClientConfig{
		Database: []string{"ssl:192.0.2.1:6641"},
		TLSCA:    "/etc/pvn/ca.pem", TLSCert: "/etc/pvn/node.pem", TLSKey: "/etc/pvn/node-key.pem",
	}); err != nil {
		t.Fatalf("secure endpoint rejected: %v", err)
	}
	if _, err := NewClient(ClientConfig{
		Database: []string{"ssl:[2001:db8::1]:6641"},
		TLSCA:    "/etc/pvn/ca.pem", TLSCert: "/etc/pvn/node.pem", TLSKey: "/etc/pvn/node-key.pem",
	}); err != nil {
		t.Fatalf("secure IPv6 endpoint rejected: %v", err)
	}
}

func TestClientRejectsUnsafeRuntimeConfiguration(t *testing.T) {
	for name, config := range map[string]ClientConfig{
		"negative timeout": {Database: []string{"unix:/run/ovn/ovnnb_db.sock"}, Timeout: -1},
		"huge timeout":     {Database: []string{"unix:/run/ovn/ovnnb_db.sock"}, Timeout: 3601},
		"relative CA": {
			Database: []string{"ssl:ovn.example:6641"}, TLSCA: "ca.pem",
			TLSCert: "/etc/pvn/node.pem", TLSKey: "/etc/pvn/key.pem",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(config); err == nil {
				t.Fatal("unsafe configuration was accepted")
			}
		})
	}
}

func TestEnvironmentWithoutOVNOverrides(t *testing.T) {
	environment := []string{
		"PATH=/usr/bin",
		"OVN_NB_DB=ssl:attacker:6641",
		"OVN_NBCTL_OPTIONS=--dry-run",
		"OVN_NB_DAEMON=/tmp/attacker.sock",
		"PVN_KEEP=yes",
	}
	filtered := environmentWithout(environment, "OVN_NB_DB", "OVN_NBCTL_OPTIONS", "OVN_NB_DAEMON")
	if slices.Contains(filtered, "OVN_NBCTL_OPTIONS=--dry-run") || slices.Contains(filtered, "OVN_NB_DB=ssl:attacker:6641") || slices.Contains(filtered, "OVN_NB_DAEMON=/tmp/attacker.sock") {
		t.Fatalf("OVN override survived filtering: %v", filtered)
	}
	if !slices.Contains(filtered, "PATH=/usr/bin") || !slices.Contains(filtered, "PVN_KEEP=yes") {
		t.Fatalf("unrelated environment was removed: %v", filtered)
	}
}

func TestClientProbeUsesConfiguredClusterAndWaitsForSync(t *testing.T) {
	runner := &probeRunner{}
	client, err := NewClient(ClientConfig{
		Runner: runner, Database: []string{"unix:/run/ovn/ovnnb_db.sock"}, WaitForSync: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.arguments, " ")
	if !strings.Contains(joined, "--no-syslog --verbose=console:warn") ||
		!strings.Contains(joined, "--db=unix:/run/ovn/ovnnb_db.sock") ||
		!strings.Contains(joined, "--wait=sb") || !strings.Contains(joined, "list NB_Global") {
		t.Fatalf("probe arguments = %v", runner.arguments)
	}
	runner.err = errors.New("unreachable")
	if err := client.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "probe OVN Northbound") {
		t.Fatalf("probe error = %v", err)
	}
}

func TestClientQuietLoggingPreservesCommandFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ovn-nbctl")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' 'simulated OVN error' >&2\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Binary: binary, Database: []string{"unix:/run/ovn/ovnnb_db.sock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "simulated OVN error") {
		t.Fatalf("probe error = %v", err)
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 23 {
		t.Fatalf("probe did not preserve exit status 23: %v", err)
	}
}
