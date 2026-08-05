package ovnnb

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSouthboundProbeUsesConfiguredCluster(t *testing.T) {
	runner := &probeRunner{}
	probe, err := NewSouthboundProbe(SouthboundProbeConfig{
		Runner: runner, Database: []string{"unix:/run/ovn/ovnsb_db.sock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.arguments, " ")
	if !strings.Contains(joined, "--db=unix:/run/ovn/ovnsb_db.sock") || !strings.Contains(joined, "--columns=nb_cfg list SB_Global") {
		t.Fatalf("probe arguments = %v", runner.arguments)
	}
	runner.err = errors.New("unreachable")
	if err := probe.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "probe OVN Southbound") {
		t.Fatalf("probe error = %v", err)
	}
}

func TestSouthboundProbeRejectsUnsafeConfiguration(t *testing.T) {
	for name, config := range map[string]SouthboundProbeConfig{
		"missing endpoints": {},
		"insecure endpoint": {Database: []string{"tcp:192.0.2.1:6642"}},
		"wrong binary":      {Binary: "ovn-nbctl", Database: []string{"unix:/run/ovn/ovnsb_db.sock"}},
		"missing TLS":       {Database: []string{"ssl:192.0.2.1:6642"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSouthboundProbe(config); err == nil {
				t.Fatal("unsafe configuration was accepted")
			}
		})
	}
}
