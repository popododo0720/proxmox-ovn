package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/ovs"
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

func (fakeManager) ReportPort(context.Context, PortReport) error { return nil }

type recordingManager struct {
	resolution Resolution
	reports    []PortReport
	reportErr  error
}

func (manager *recordingManager) ResolveInterface(context.Context, InterfaceRef) (Resolution, error) {
	return manager.resolution, nil
}

func (manager *recordingManager) ReportPort(_ context.Context, report PortReport) error {
	manager.reports = append(manager.reports, report)
	return manager.reportErr
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

type signalingSource struct {
	interfaces []ovs.Interface
	calls      chan struct{}
}

func (source *signalingSource) ListInterfaces(context.Context, string) ([]ovs.Interface, error) {
	source.calls <- struct{}{}
	return source.interfaces, nil
}

type manualTicker struct {
	ticks   chan time.Time
	stopped chan struct{}
}

func (ticker *manualTicker) C() <-chan time.Time { return ticker.ticks }
func (ticker *manualTicker) Stop()               { close(ticker.stopped) }

func TestWatcherRunAfterInitialScanDoesNotRepeatStartupSideEffects(t *testing.T) {
	source := &signalingSource{
		interfaces: []ovs.Interface{{Name: "tap100i0", ExternalIDs: map[string]string{}}},
		calls:      make(chan struct{}, 3),
	}
	binder := &fakeBinder{}
	ticker := &manualTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
	tickerCreated := make(chan struct{})
	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-a", ChassisID: "chassis-a", Bridge: "br-int", Interval: time.Second,
		Source: source, Binder: binder,
		Manager: fakeManager{results: map[int]Resolution{100: {
			PortID: "port-1", LSPName: "pvn-lsp-1", MACAddress: "02:00:00:00:00:01",
			Generation: "1", RequestedChassis: "chassis-a", Status: PortStatusBinding,
		}}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		TickerFactory: func(time.Duration) TickSource {
			close(tickerCreated)
			return ticker
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watcher.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-source.calls
	if len(binder.set) != 1 {
		t.Fatalf("initial binding calls=%d", len(binder.set))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watcher.RunAfterInitialScan(ctx) }()
	<-tickerCreated
	select {
	case <-source.calls:
		t.Fatal("RunAfterInitialScan repeated the initial scan before the first tick")
	default:
	}
	if len(binder.set) != 1 {
		t.Fatalf("binding repeated before first tick: calls=%d", len(binder.set))
	}

	ticker.ticks <- time.Now()
	select {
	case <-source.calls:
	case <-time.After(time.Second):
		t.Fatal("watcher did not scan on tick")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-ticker.stopped:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop ticker")
	}
}

func TestWatcherRunAfterInitialScanRequiresInitialAttempt(t *testing.T) {
	t.Parallel()

	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-a", Bridge: "br-int", Interval: time.Second,
		Source: fakeSource{}, Binder: &fakeBinder{}, Manager: fakeManager{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.RunAfterInitialScan(context.Background()); err == nil {
		t.Fatal("RunAfterInitialScan unexpectedly accepted a missing initial scan")
	}
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

func TestWatcherKeepsBoundPortsAndReportsOVNInstallation(t *testing.T) {
	t.Parallel()

	externalIDs := map[string]string{
		"managed-by": "pvn", "iface-id": "pvn-lsp-1", "iface-id-ver": "5",
		"attached-mac": "02:00:00:00:00:01", "ovn-installed": "true",
	}
	manager := &recordingManager{resolution: Resolution{
		PortID: "port-1", LSPName: "pvn-lsp-1", MACAddress: "02:00:00:00:00:01",
		Generation: "5", RequestedChassis: "chassis-a", Status: PortStatusBinding,
	}}
	binder := &fakeBinder{}
	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-a", ChassisID: "chassis-a", Bridge: "br-int", Interval: time.Second,
		Source: fakeSource{interfaces: []ovs.Interface{{Name: "tap100i0", ExternalIDs: externalIDs}}},
		Binder: binder, Manager: manager,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := watcher.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.AlreadyBound != 1 || report.ReportedBound != 1 || len(binder.cleared) != 0 || len(binder.set) != 0 {
		t.Fatalf("report=%#v set=%#v cleared=%#v", report, binder.set, binder.cleared)
	}
	if len(manager.reports) != 1 || manager.reports[0].Status != PortStatusBound || manager.reports[0].Generation != "5" {
		t.Fatalf("manager reports = %#v", manager.reports)
	}

	manager.resolution.Status = PortStatusBound
	manager.reports = nil
	report, err = watcher.ScanOnce(context.Background())
	if err != nil || report.AlreadyBound != 1 || report.ReportedBound != 0 || len(manager.reports) != 0 {
		t.Fatalf("bound rescan report=%#v manager=%#v err=%v", report, manager.reports, err)
	}
}

func TestWatcherClearsDetachingPortThenReportsUnbound(t *testing.T) {
	t.Parallel()

	manager := &recordingManager{resolution: Resolution{
		PortID: "port-1", LSPName: "pvn-lsp-1", MACAddress: "02:00:00:00:00:01",
		Generation: "5", RequestedChassis: "chassis-a", Status: PortStatusDetaching,
	}}
	binder := &fakeBinder{}
	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-a", ChassisID: "chassis-a", Bridge: "br-int", Interval: time.Second,
		Source: fakeSource{interfaces: []ovs.Interface{{Name: "tap100i0", ExternalIDs: map[string]string{
			"managed-by": "pvn", "iface-id": "pvn-lsp-1", "iface-id-ver": "5",
		}}}},
		Binder: binder, Manager: manager,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := watcher.ScanOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Unbound != 1 || report.ReportedUnbound != 1 || !reflect.DeepEqual(binder.cleared, []string{"tap100i0"}) {
		t.Fatalf("report=%#v cleared=%#v", report, binder.cleared)
	}
	if len(manager.reports) != 1 || manager.reports[0].Status != PortStatusUnbound || manager.reports[0].PortID != "port-1" {
		t.Fatalf("manager reports = %#v", manager.reports)
	}
}

func TestWatcherClearsErrorWithoutReportingDetached(t *testing.T) {
	t.Parallel()

	manager := &recordingManager{resolution: Resolution{PortID: "port-1", Generation: "5", Status: PortStatusError}}
	binder := &fakeBinder{}
	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-a", Bridge: "br-int", Interval: time.Second,
		Source: fakeSource{interfaces: []ovs.Interface{{Name: "tap100i0", ExternalIDs: map[string]string{"managed-by": "pvn", "iface-id": "old"}}}},
		Binder: binder, Manager: manager,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := watcher.ScanOnce(context.Background())
	if err != nil || report.Unbound != 1 || len(manager.reports) != 0 || !reflect.DeepEqual(binder.cleared, []string{"tap100i0"}) {
		t.Fatalf("report=%#v manager=%#v cleared=%#v err=%v", report, manager.reports, binder.cleared, err)
	}
}
