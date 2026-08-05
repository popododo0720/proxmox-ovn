package diagnostic

import (
	"context"
	"errors"
	"testing"

	"github.com/pvnstack/proxmox-ovn/internal/config"
)

type fakeRunner struct {
	outputs map[string]string
	errors  map[string]error
}

func (f fakeRunner) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	return []byte(f.outputs[name]), f.errors[name]
}

func TestRunReportsHostFailures(t *testing.T) {
	cfg := config.Default()
	cfg.Cluster.ID = "lab"
	cfg.Networking.EncapIP = "192.0.2.10"
	cfg.Manager.TLSCert = t.TempDir() + "/missing-cert"
	cfg.Manager.TLSKey = t.TempDir() + "/missing-key"
	runner := fakeRunner{
		outputs: map[string]string{"pveversion": "pve-manager/9.0", "ovn-appctl": "ovn 25.03"},
		errors:  map[string]error{"ovs-vsctl": errors.New("bridge missing")},
	}
	checks := Run(context.Background(), cfg, runner)
	if Healthy(checks) {
		t.Fatalf("expected unhealthy checks: %+v", checks)
	}
}
