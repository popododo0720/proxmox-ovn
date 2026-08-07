package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/ovs"
)

func TestWatcherAcceptsLocalAdditionalMigrationChassis(t *testing.T) {
	binder := &fakeBinder{}
	watcher, err := NewWatcher(WatcherConfig{
		Node: "pve-b", ChassisID: "chassis-b", Bridge: "br-int", Interval: time.Second,
		Source: fakeSource{interfaces: []ovs.Interface{{Name: "tap100i0", ExternalIDs: map[string]string{}}}},
		Binder: binder,
		Manager: fakeManager{results: map[int]Resolution{100: {
			PortID: "port-1", LSPName: "pvn-lsp-1", MACAddress: "02:00:00:00:00:01",
			Generation: "7", RequestedChassis: "chassis-a,chassis-b", Status: PortStatusBinding,
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
	if report.Bound != 1 || report.Conflicts != 0 || len(binder.set) != 1 || binder.set[0].binding.Generation != "7" {
		t.Fatalf("dual-chassis target was not bound: report=%#v set=%#v", report, binder.set)
	}
}
