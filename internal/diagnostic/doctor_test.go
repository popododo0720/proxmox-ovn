package diagnostic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/config"
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

func TestExactOVSEncapValue(t *testing.T) {
	for _, value := range []string{"geneve", `"geneve"`} {
		if err := exactOVSValue("geneve")(value); err != nil {
			t.Fatalf("matching OVS value %q failed: %v", value, err)
		}
	}
	for _, value := range []string{"", "[]", "vxlan", `"192.0.2.11"`} {
		if err := exactOVSValue("192.0.2.10")(value); err == nil {
			t.Fatalf("mismatched OVS value %q unexpectedly passed", value)
		}
	}
}

func TestRunChecksActualOVSEncapsulation(t *testing.T) {
	cfg := config.Default()
	cfg.Cluster.ID = "lab"
	cfg.Networking.EncapIP = "192.0.2.10"
	cfg.OVN.ControlDB = []string{"unix:/run/pvn/control.sock"}
	cfg.OVN.Northbound = []string{"unix:/run/ovn/ovnnb_db.sock"}
	cfg.OVN.Southbound = []string{"unix:/run/ovn/ovnsb_db.sock"}
	runner := fakeRunner{outputs: map[string]string{
		"ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-encap-type": `"geneve"`,
		"ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-encap-ip":   `"192.0.2.10"`,
	}}
	checks := Run(context.Background(), cfg, runner)
	for _, name := range []string{"ovn-encap-type", "ovn-encap-ip"} {
		if check := checkByName(checks, name); check.Status != Pass {
			t.Fatalf("%s did not accept the configured OVS value: %+v", name, check)
		}
	}

	runner.outputs["ovs-vsctl --if-exists get Open_vSwitch . external_ids:ovn-encap-ip"] = `"192.0.2.11"`
	checks = Run(context.Background(), cfg, runner)
	if check := checkByName(checks, "ovn-encap-ip"); check.Status != Fail || !strings.Contains(check.Message, "does not match") {
		t.Fatalf("stale OVS encapsulation IP did not fail: %+v", check)
	}
}

func checkByName(checks []Check, name string) Check {
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	return Check{Name: name, Status: Fail, Message: "check was not emitted"}
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
