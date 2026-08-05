package hostconfig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	mappings string
	calls    []call
}

func (runner *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, call{name: name, args: append([]string(nil), args...)})
	if len(args) > 1 && args[1] == "br-exists" {
		return nil, nil
	}
	if len(args) > 2 && args[1] == "--if-exists" && args[2] == "get" {
		return []byte(runner.mappings), nil
	}
	return nil, nil
}

func TestApplyOVNTreatsMissingBridgeMappingKeyAsEmpty(t *testing.T) {
	runner := &fakeRunner{}
	err := ApplyOVN(context.Background(), runner, Config{
		IntegrationBridge: "br-int", ProviderBridge: "br-provider", PhysicalNetwork: "provider",
		EncapType: "geneve", EncapIP: "192.0.2.10", Southbound: []string{"unix:/run/ovn/ovnsb.sock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRead := []string{"--timeout=10", "--if-exists", "get", "Open_vSwitch", ".", "external_ids:ovn-bridge-mappings"}
	if !reflect.DeepEqual(runner.calls[2].args, wantRead) {
		t.Fatalf("get args = %#v", runner.calls[2].args)
	}
	if got := runner.calls[3].args[len(runner.calls[3].args)-1]; got != "external_ids:ovn-bridge-mappings=provider:br-provider" {
		t.Fatalf("bridge mapping assignment = %q", got)
	}
}

func TestApplyOVNPreservesExistingBridgeMappings(t *testing.T) {
	runner := &fakeRunner{mappings: `"storage:br-storage"`}
	err := ApplyOVN(context.Background(), runner, Config{
		IntegrationBridge: "br-int",
		ProviderBridge:    "br-provider",
		PhysicalNetwork:   "provider",
		EncapType:         "geneve",
		EncapIP:           "192.0.2.10",
		Southbound:        []string{"ssl:192.0.2.10:6642", "ssl:192.0.2.11:6642"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	want := []string{
		"--timeout=10", "set", "Open_vSwitch", ".",
		"external_ids:ovn-remote=ssl:192.0.2.10:6642,ssl:192.0.2.11:6642",
		"external_ids:ovn-encap-type=geneve",
		"external_ids:ovn-encap-ip=192.0.2.10",
		"external_ids:ovn-remote-probe-interval=10000",
		"external_ids:ovn-bridge-mappings=provider:br-provider,storage:br-storage",
	}
	if !reflect.DeepEqual(runner.calls[3].args, want) {
		t.Fatalf("set args = %#v", runner.calls[3].args)
	}
}

func TestApplyOVNRejectsConflictingBridgeMapping(t *testing.T) {
	runner := &fakeRunner{mappings: `"provider:br-old"`}
	err := ApplyOVN(context.Background(), runner, Config{
		IntegrationBridge: "br-int", ProviderBridge: "br-provider", PhysicalNetwork: "provider",
		EncapType: "geneve", EncapIP: "192.0.2.10", Southbound: []string{"unix:/run/ovn/ovnsb.sock"},
	})
	if err == nil || !strings.Contains(err.Error(), "already mapped") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("mutating command unexpectedly ran: %#v", runner.calls)
	}
}

func TestApplyOVNFailsBeforeMutationWhenBridgeIsMissing(t *testing.T) {
	runner := &failingBridgeRunner{}
	err := ApplyOVN(context.Background(), runner, Config{
		IntegrationBridge: "br-int", ProviderBridge: "br-provider", PhysicalNetwork: "provider",
		EncapType: "geneve", EncapIP: "192.0.2.10", Southbound: []string{"unix:/run/ovn/ovnsb.sock"},
	})
	if err == nil || !strings.Contains(err.Error(), "br-provider") {
		t.Fatalf("error = %v", err)
	}
}

func TestApplyOVNPreservesOtherBridgeMappingReadErrors(t *testing.T) {
	runner := &failingMappingReadRunner{}
	err := ApplyOVN(context.Background(), runner, Config{
		IntegrationBridge: "br-int", ProviderBridge: "br-provider", PhysicalNetwork: "provider",
		EncapType: "geneve", EncapIP: "192.0.2.10", Southbound: []string{"unix:/run/ovn/ovnsb.sock"},
	})
	if err == nil || !strings.Contains(err.Error(), "read OVN bridge mappings") || !strings.Contains(err.Error(), "database connection failed") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("mutating command unexpectedly ran: %#v", runner.calls)
	}
}

type failingBridgeRunner struct{}

func (*failingBridgeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if args[len(args)-1] == "br-provider" {
		return []byte("no bridge named br-provider"), errors.New("exit status 2")
	}
	return nil, nil
}

type failingMappingReadRunner struct {
	calls []call
}

func (runner *failingMappingReadRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, call{name: name, args: append([]string(nil), args...)})
	if args[len(args)-1] == "external_ids:ovn-bridge-mappings" {
		return []byte("database connection failed"), errors.New("exit status 1")
	}
	return nil, nil
}
