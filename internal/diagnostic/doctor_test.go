package diagnostic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pvnstack/proxmox-ovn/internal/config"
)

type fakeRunner struct {
	outputs map[string]string
	errors  map[string]error
}

func (f fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	if output, exists := f.outputs[key]; exists {
		return []byte(output), f.errors[key]
	}
	return []byte(f.outputs[name]), f.errors[name]
}

func TestValidateEncapMTU(t *testing.T) {
	inventory := `[{"ifname":"bond0","mtu":1500,"addr_info":[{"family":"inet","local":"192.0.2.10"}]}]`
	if err := validateEncapMTU(inventory, "192.0.2.10", 1500); err != nil {
		t.Fatal(err)
	}
	if err := validateEncapMTU(inventory, "192.0.2.10", 1501); err == nil {
		t.Fatal("undersized underlay unexpectedly passed")
	}
	if err := validateEncapMTU(inventory, "192.0.2.11", 1500); err == nil {
		t.Fatal("missing encap address unexpectedly passed")
	}
}

func TestOVSValueValidation(t *testing.T) {
	if err := nonEmptyOVSValue(`"chassis-a"`); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "[]", "{}"} {
		if err := nonEmptyOVSValue(value); err == nil {
			t.Fatalf("empty OVS value %q unexpectedly passed", value)
		}
	}
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
