package ovs

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type runnerCall struct {
	binary string
	args   []string
}

type fakeRunner struct {
	mu      sync.Mutex
	outputs [][]byte
	calls   []runnerCall
}

func (runner *fakeRunner) Run(_ context.Context, binary string, args ...string) ([]byte, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.calls = append(runner.calls, runnerCall{binary: binary, args: append([]string(nil), args...)})
	output := runner.outputs[0]
	runner.outputs = runner.outputs[1:]
	return output, nil
}

func TestListInterfacesRestrictsResultsToBridge(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputs: [][]byte{
		[]byte(`{"headings":["name","external_ids"],"data":[["tap100i0",["map",[["managed-by","pvn"]]]],["tap200i0",["map",[]]],["eth0",["map",[]]]]}`),
		[]byte("tap200i0\ntap100i0\n"),
	}}
	client, err := NewClient(ClientConfig{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	interfaces, err := client.ListInterfaces(context.Background(), "br-int")
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{interfaces[0].Name, interfaces[1].Name}; !reflect.DeepEqual(got, []string{"tap100i0", "tap200i0"}) {
		t.Fatalf("interfaces = %#v", interfaces)
	}
	if interfaces[0].ExternalIDs["managed-by"] != "pvn" {
		t.Fatalf("external IDs = %#v", interfaces[0].ExternalIDs)
	}
}

func TestSetManagedBindingUsesOneValidatedTransaction(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{outputs: [][]byte{[]byte("")}}
	client, err := NewClient(ClientConfig{Runner: runner, Binary: "/usr/bin/ovs-vsctl"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.SetManagedBinding(context.Background(), "tap100i0", ManagedBinding{
		LSPName:    "pvn-lsp-port-1",
		Generation: "42",
		MACAddress: "02:00:00:00:00:01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
	joined := strings.Join(runner.calls[0].args, " ")
	for _, expected := range []string{
		"external_ids:iface-id=pvn-lsp-port-1",
		"external_ids:iface-id-ver=42",
		"external_ids:attached-mac=02:00:00:00:00:01",
		"external_ids:managed-by=pvn",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("command %q does not contain %q", joined, expected)
		}
	}
}

func TestSetManagedBindingRejectsUnsafeValuesWithoutExecuting(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	client, err := NewClient(ClientConfig{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	err = client.SetManagedBinding(context.Background(), "tap100i0", ManagedBinding{
		LSPName:    "-- destroy",
		Generation: "1",
		MACAddress: "02:00:00:00:00:01",
	})
	if err == nil {
		t.Fatal("unsafe binding unexpectedly accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was called: %#v", runner.calls)
	}
}
