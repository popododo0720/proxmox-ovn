package pve

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type fakeVMNetworkClient struct {
	digest      int
	networks    map[int]NetProperty
	setValues   []string
	deleteCalls int
	failSetAt   int
	failDelete  bool
}

func newFakeVMNetworkClient() *fakeVMNetworkClient {
	return &fakeVMNetworkClient{digest: 1, networks: make(map[int]NetProperty)}
}

func (client *fakeVMNetworkClient) GetVMConfig(context.Context, string, int) (VMConfig, error) {
	copyNetworks := make(map[int]NetProperty, len(client.networks))
	for index, property := range client.networks {
		copyNetworks[index] = property.Clone()
	}
	return VMConfig{Digest: fmt.Sprintf("d%d", client.digest), Networks: copyNetworks}, nil
}

func (client *fakeVMNetworkClient) SetVMNetwork(_ context.Context, _ string, _, index int, property NetProperty, _ string) (string, error) {
	client.setValues = append(client.setValues, property.String())
	if client.failSetAt > 0 && len(client.setValues) == client.failSetAt {
		return "", errors.New("set failed")
	}
	client.networks[index] = property.Clone()
	client.digest++
	return fmt.Sprintf("UPID:set:%d", client.digest), nil
}

func (client *fakeVMNetworkClient) DeleteVMNetwork(_ context.Context, _ string, _, index int, _ string) (string, error) {
	client.deleteCalls++
	if client.failDelete {
		client.failDelete = false
		return "", errors.New("delete failed")
	}
	delete(client.networks, index)
	client.digest++
	return fmt.Sprintf("UPID:delete:%d", client.digest), nil
}

func (*fakeVMNetworkClient) WaitUPID(context.Context, string, string) error { return nil }

type fakeBindingLifecycle struct {
	calls         []string
	failWaitBound bool
}

func (binding *fakeBindingLifecycle) Prepare(context.Context, Attachment) error {
	binding.calls = append(binding.calls, "prepare")
	return nil
}

func (binding *fakeBindingLifecycle) WaitBound(context.Context, Attachment) error {
	binding.calls = append(binding.calls, "wait-bound")
	if binding.failWaitBound {
		binding.failWaitBound = false
		return errors.New("binding failed")
	}
	return nil
}

func (binding *fakeBindingLifecycle) Disable(context.Context, Attachment) error {
	binding.calls = append(binding.calls, "disable")
	return nil
}

func (binding *fakeBindingLifecycle) Release(context.Context, Attachment) error {
	binding.calls = append(binding.calls, "release")
	return nil
}

func mustProperty(t *testing.T, value string) NetProperty {
	t.Helper()
	property, err := ParseNetProperty(value)
	if err != nil {
		t.Fatal(err)
	}
	return property
}

func TestHotplugAttachStagesThenEnablesNIC(t *testing.T) {
	t.Parallel()

	api := newFakeVMNetworkClient()
	binding := &fakeBindingLifecycle{}
	hotplugger := Hotplugger{PVE: api, Bindings: binding}
	err := hotplugger.Attach(context.Background(), Attachment{
		Node: "pve-a", VMID: 100, NICIndex: 0, PVNPortID: "port-1",
		Property: mustProperty(t, "virtio=02:00:00:00:00:01,bridge=br-int,unknown=kept"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.setValues) != 2 || api.setValues[0] != "virtio=02:00:00:00:00:01,bridge=br-int,unknown=kept,link_down=1" || api.setValues[1] != "virtio=02:00:00:00:00:01,bridge=br-int,unknown=kept,link_down=0" {
		t.Fatalf("set values = %#v", api.setValues)
	}
	if got := api.networks[0].String(); got != "virtio=02:00:00:00:00:01,bridge=br-int,unknown=kept,link_down=0" {
		t.Fatalf("final network = %q", got)
	}
}

func TestHotplugAttachRollsBackWhenBindingFails(t *testing.T) {
	t.Parallel()

	api := newFakeVMNetworkClient()
	binding := &fakeBindingLifecycle{failWaitBound: true}
	hotplugger := Hotplugger{PVE: api, Bindings: binding}
	err := hotplugger.Attach(context.Background(), Attachment{
		Node: "pve-a", VMID: 100, NICIndex: 0,
		Property: mustProperty(t, "virtio=02:00:00:00:00:01,bridge=br-int"),
	})
	if err == nil {
		t.Fatal("Attach unexpectedly succeeded")
	}
	if _, exists := api.networks[0]; exists {
		t.Fatal("staged NIC was not removed")
	}
	if api.deleteCalls != 1 || binding.calls[len(binding.calls)-1] != "release" {
		t.Fatalf("deleteCalls=%d lifecycle=%#v", api.deleteCalls, binding.calls)
	}
}

func TestHotplugDetachRollsBackDeleteFailure(t *testing.T) {
	t.Parallel()

	api := newFakeVMNetworkClient()
	original := mustProperty(t, "virtio=02:00:00:00:00:01,bridge=br-int,unknown=kept")
	api.networks[0] = original
	api.failDelete = true
	binding := &fakeBindingLifecycle{}
	hotplugger := Hotplugger{PVE: api, Bindings: binding}
	err := hotplugger.Detach(context.Background(), Attachment{Node: "pve-a", VMID: 100, NICIndex: 0, PVNPortID: "port-1"})
	if err == nil {
		t.Fatal("Detach unexpectedly succeeded")
	}
	if got := api.networks[0].String(); got != original.String() {
		t.Fatalf("restored NIC = %q, want %q", got, original.String())
	}
	if got := binding.calls; len(got) < 3 || got[0] != "disable" || got[len(got)-2] != "prepare" || got[len(got)-1] != "wait-bound" {
		t.Fatalf("lifecycle calls = %#v", got)
	}
}
