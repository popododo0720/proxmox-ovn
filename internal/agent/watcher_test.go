package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/ovs"
)

type fakeSource struct{ interfaces []ovs.Interface }

func (source fakeSource) ListInterfaces(context.Context, string) ([]ovs.Interface, error) {
	return source.interfaces, nil
}

type fakeManager struct {
	results map[int]Resolution
	errors  map[int]error
}

func (manager fakeManager) ResolveInterface(_ context.Context, reference InterfaceRef) (Resolution, error) {
	return manager.results[reference.VMID], manager.errors[reference.VMID]
}

type bindingCall struct {
	name    string
	binding ovs.ManagedBinding
}

type fakeBinder struct {
	set     []bindingCall
	cleared []string
}

func (binder *fakeBinder) SetManagedBinding(_ context.Context, name string, binding ovs.ManagedBinding) error {
	binder.set = append(binder.set, bindingCall{name: name, binding: binding})
	return nil
}

func (binder *fakeBinder) ClearManagedBinding(_ context.Context, name string) error {
	binder.cleared = append(binder.cleared, name)
	return nil
}

func TestWatcherBindsExactlyResolvedInterfaces(t *testing.T) {
	t.Parallel()

	source := fakeSource{interfaces: []ovs.Interface{
		{Name: "tap100i0", ExternalIDs: map[string]string{}},
		{Name: "tap200i0", ExternalIDs: map[string]string{}},
		{Name: "tap300i0", ExternalIDs: map[string]string{"managed-by": "pvn", "iface-id": "stale"}},
		{Name: "tap400i0", ExternalIDs: map[string]string{"managed-by": "other", "iface-id": "other-lsp"}},
		{Name: "eth0", ExternalIDs: map[string]string{}},
	}}
	manager := fakeManager{
		results: map[int]Resolution{100: {
			PortID: "port-1", LSPName: "pvn-lsp-1", MACAddress: "02:00:00:00:00:01", Generation: "5", RequestedChassis: "chassis-a", Status: "binding",
		}},
		errors: map[int]error{200: ErrAmbiguous, 300: ErrNotManaged},
	}
	binder := &fakeBinder{}
	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-a", ChassisID: "chassis-a", Bridge: "br-int", Interval: time.Second,
		Source: source, Binder: binder, Manager: manager,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := watcher.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Bound != 1 || report.Ambiguous != 1 || report.Unknown != 1 || report.Conflicts != 1 || report.Candidates != 4 {
		t.Fatalf("report = %#v", report)
	}
	if len(binder.set) != 1 || binder.set[0].name != "tap100i0" || binder.set[0].binding.LSPName != "pvn-lsp-1" {
		t.Fatalf("set calls = %#v", binder.set)
	}
	if !reflect.DeepEqual(binder.cleared, []string{"tap300i0"}) {
		t.Fatalf("cleared = %#v", binder.cleared)
	}
}

func TestWatcherRejectsPortRequestedOnAnotherChassis(t *testing.T) {
	t.Parallel()

	binder := &fakeBinder{}
	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-a", ChassisID: "chassis-a", Bridge: "br-int", Interval: time.Second,
		Source: fakeSource{interfaces: []ovs.Interface{{Name: "tap100i0", ExternalIDs: map[string]string{
			"managed-by": "pvn", "iface-id": "stale-lsp",
		}}}},
		Binder: binder,
		Manager: fakeManager{results: map[int]Resolution{100: {
			PortID: "port-1", LSPName: "pvn-lsp-1", MACAddress: "02:00:00:00:00:01",
			Generation: "1", RequestedChassis: "chassis-b", Status: "binding",
		}}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := watcher.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Conflicts != 1 || report.Unbound != 1 || len(binder.set) != 0 || !reflect.DeepEqual(binder.cleared, []string{"tap100i0"}) {
		t.Fatalf("report=%#v set=%#v cleared=%#v", report, binder.set, binder.cleared)
	}
}

func TestWatcherDoesNotMutateOnManagerFailure(t *testing.T) {
	t.Parallel()

	binder := &fakeBinder{}
	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-a", Bridge: "br-int", Interval: time.Second,
		Source:  fakeSource{interfaces: []ovs.Interface{{Name: "tap100i0"}}},
		Binder:  binder,
		Manager: fakeManager{errors: map[int]error{100: errors.New("manager unavailable")}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := watcher.ScanOnce(context.Background())
	if err == nil || report.Errors != 1 {
		t.Fatalf("report=%#v error=%v", report, err)
	}
	if len(binder.set) != 0 || len(binder.cleared) != 0 {
		t.Fatalf("unexpected mutations: %#v %#v", binder.set, binder.cleared)
	}
}
