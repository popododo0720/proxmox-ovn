package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/ovs"
	"github.com/popododo0720/proxmox-ovn/internal/pve"
)

type TickSource interface {
	C() <-chan time.Time
	Stop()
}

type TickerFactory func(time.Duration) TickSource

type WatcherConfig struct {
	Node          string
	ChassisID     string
	Bridge        string
	Interval      time.Duration
	Source        ovs.InterfaceSource
	Binder        ovs.InterfaceBinder
	Manager       ManagerClient
	Logger        *slog.Logger
	TickerFactory TickerFactory
}

type ScanReport struct {
	Scanned         int `json:"scanned"`
	Candidates      int `json:"candidates"`
	Bound           int `json:"bound"`
	AlreadyBound    int `json:"already_bound"`
	Unbound         int `json:"unbound"`
	ReportedBound   int `json:"reported_bound"`
	ReportedUnbound int `json:"reported_unbound"`
	Unknown         int `json:"unknown"`
	Ambiguous       int `json:"ambiguous"`
	Conflicts       int `json:"conflicts"`
	Errors          int `json:"errors"`
}

type WatcherStatus struct {
	LastScan    time.Time  `json:"last_scan"`
	LastSuccess time.Time  `json:"last_success"`
	LastError   string     `json:"last_error,omitempty"`
	Report      ScanReport `json:"report"`
}

type Watcher struct {
	node          string
	chassisID     string
	bridge        string
	interval      time.Duration
	source        ovs.InterfaceSource
	binder        ovs.InterfaceBinder
	manager       ManagerClient
	logger        *slog.Logger
	tickerFactory TickerFactory

	statusMu sync.RWMutex
	status   WatcherStatus
}

func NewWatcher(config WatcherConfig) (*Watcher, error) {
	if config.Node == "" {
		return nil, errors.New("PVE node name is required")
	}
	if config.Bridge == "" {
		return nil, errors.New("OVS integration bridge is required")
	}
	if config.Interval <= 0 {
		return nil, errors.New("watch interval must be positive")
	}
	if config.Source == nil || config.Binder == nil || config.Manager == nil {
		return nil, errors.New("source, binder, and manager are required")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	factory := config.TickerFactory
	if factory == nil {
		factory = func(interval time.Duration) TickSource { return realTicker{Ticker: time.NewTicker(interval)} }
	}
	return &Watcher{
		node:          config.Node,
		chassisID:     config.ChassisID,
		bridge:        config.Bridge,
		interval:      config.Interval,
		source:        config.Source,
		binder:        config.Binder,
		manager:       config.Manager,
		logger:        logger,
		tickerFactory: factory,
	}, nil
}

type realTicker struct{ *time.Ticker }

func (ticker realTicker) C() <-chan time.Time { return ticker.Ticker.C }

func (watcher *Watcher) Run(ctx context.Context) error {
	if _, err := watcher.ScanOnce(ctx); err != nil {
		watcher.logger.Error("initial OVS interface scan failed", "error", err)
	}
	ticker := watcher.tickerFactory(watcher.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
			if _, err := watcher.ScanOnce(ctx); err != nil {
				watcher.logger.Error("OVS interface scan failed", "error", err)
			}
		}
	}
}

func (watcher *Watcher) ScanOnce(ctx context.Context) (ScanReport, error) {
	interfaces, err := watcher.source.ListInterfaces(ctx, watcher.bridge)
	if err != nil {
		watcher.recordStatus(ScanReport{Errors: 1}, err)
		return ScanReport{Errors: 1}, err
	}
	report := ScanReport{Scanned: len(interfaces)}
	var scanErrors []error
	for _, ovsInterface := range interfaces {
		identity, err := pve.ParseTapName(ovsInterface.Name)
		if err != nil {
			continue
		}
		report.Candidates++
		resolution, err := watcher.manager.ResolveInterface(ctx, InterfaceRef{
			Node:          watcher.node,
			VMID:          identity.VMID,
			NICIndex:      identity.NICIndex,
			InterfaceName: ovsInterface.Name,
		})
		if err != nil {
			switch {
			case errors.Is(err, ErrNotManaged), errors.Is(err, ErrNotBindable):
				report.Unknown++
			case errors.Is(err, ErrAmbiguous):
				report.Ambiguous++
			default:
				report.Errors++
				scanErrors = append(scanErrors, fmt.Errorf("resolve %s: %w", ovsInterface.Name, err))
				continue
			}
			if ovsInterface.ExternalIDs["managed-by"] == ovs.ManagedByPVN {
				if err := watcher.binder.ClearManagedBinding(ctx, ovsInterface.Name); err != nil {
					report.Errors++
					scanErrors = append(scanErrors, err)
				} else {
					report.Unbound++
				}
			}
			continue
		}
		if ownedByAnotherController(ovsInterface.ExternalIDs) {
			report.Conflicts++
			watcher.logger.Warn("leaving OVS interface owned by another controller unchanged", "interface", ovsInterface.Name)
			continue
		}

		switch resolution.Status {
		case PortStatusDetaching, PortStatusUnbound, PortStatusError:
			if ovsInterface.ExternalIDs["managed-by"] == ovs.ManagedByPVN {
				if err := watcher.binder.ClearManagedBinding(ctx, ovsInterface.Name); err != nil {
					report.Errors++
					scanErrors = append(scanErrors, err)
					continue
				}
				report.Unbound++
			}
			if resolution.Status == PortStatusDetaching {
				if err := watcher.manager.ReportPort(ctx, PortReport{PortID: resolution.PortID, Generation: resolution.Generation, Status: PortStatusUnbound}); err != nil {
					report.Errors++
					scanErrors = append(scanErrors, fmt.Errorf("report %s unbound: %w", ovsInterface.Name, err))
				} else {
					report.ReportedUnbound++
				}
			}
			continue
		case PortStatusBinding, PortStatusBound:
			// Continue with the binding checks below.
		default:
			report.Errors++
			scanErrors = append(scanErrors, fmt.Errorf("resolve %s: unknown port status %q", ovsInterface.Name, resolution.Status))
			continue
		}

		// requested-chassis is an OVN Northbound LSP option, owned by the
		// manager reconciler. It does not belong in local Interface external
		// IDs; the agent uses it as a guard against binding a TAP on the wrong
		// transport node.
		if resolution.RequestedChassis != watcher.node && resolution.RequestedChassis != watcher.chassisID {
			report.Conflicts++
			watcher.logger.Warn("leaving OVS interface unresolved because requested chassis is not local",
				"interface", ovsInterface.Name,
				"requested_chassis", resolution.RequestedChassis,
				"local_chassis", watcher.chassisID,
			)
			if ovsInterface.ExternalIDs["managed-by"] == ovs.ManagedByPVN {
				if err := watcher.binder.ClearManagedBinding(ctx, ovsInterface.Name); err != nil {
					report.Errors++
					scanErrors = append(scanErrors, err)
				} else {
					report.Unbound++
				}
			}
			continue
		}
		binding := ovs.ManagedBinding{
			LSPName:    resolution.LSPName,
			Generation: resolution.Generation,
			MACAddress: resolution.MACAddress,
		}
		if bindingMatches(ovsInterface.ExternalIDs, binding) {
			report.AlreadyBound++
			if resolution.Status == PortStatusBinding && strings.EqualFold(ovsInterface.ExternalIDs["ovn-installed"], "true") {
				if err := watcher.manager.ReportPort(ctx, PortReport{PortID: resolution.PortID, Generation: resolution.Generation, Status: PortStatusBound}); err != nil {
					report.Errors++
					scanErrors = append(scanErrors, fmt.Errorf("report %s bound: %w", ovsInterface.Name, err))
				} else {
					report.ReportedBound++
				}
			}
			continue
		}
		if err := watcher.binder.SetManagedBinding(ctx, ovsInterface.Name, binding); err != nil {
			report.Errors++
			scanErrors = append(scanErrors, err)
			continue
		}
		report.Bound++
		watcher.logger.Info("bound PVE TAP to OVN logical switch port", "interface", ovsInterface.Name, "lsp", resolution.LSPName)
	}
	joined := errors.Join(scanErrors...)
	watcher.recordStatus(report, joined)
	return report, joined
}

func (watcher *Watcher) Status() WatcherStatus {
	watcher.statusMu.RLock()
	defer watcher.statusMu.RUnlock()
	return watcher.status
}

func (watcher *Watcher) recordStatus(report ScanReport, err error) {
	now := time.Now().UTC()
	watcher.statusMu.Lock()
	defer watcher.statusMu.Unlock()
	watcher.status.LastScan = now
	watcher.status.Report = report
	if err == nil {
		watcher.status.LastSuccess = now
		watcher.status.LastError = ""
	} else {
		watcher.status.LastError = err.Error()
	}
}

func ownedByAnotherController(externalIDs map[string]string) bool {
	owner := externalIDs["managed-by"]
	if owner != "" && owner != ovs.ManagedByPVN {
		return true
	}
	return owner == "" && externalIDs["iface-id"] != ""
}

func bindingMatches(externalIDs map[string]string, binding ovs.ManagedBinding) bool {
	return externalIDs["managed-by"] == ovs.ManagedByPVN &&
		externalIDs["iface-id"] == binding.LSPName &&
		externalIDs["iface-id-ver"] == binding.Generation &&
		strings.EqualFold(externalIDs["attached-mac"], binding.MACAddress)
}
