package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/api"
	"github.com/popododo0720/proxmox-ovn/internal/buildinfo"
	pvnconfig "github.com/popododo0720/proxmox-ovn/internal/config"
	"github.com/popododo0720/proxmox-ovn/internal/ovnnb"
	"github.com/popododo0720/proxmox-ovn/internal/ovsdbstore"
	"github.com/popododo0720/proxmox-ovn/internal/reconcile"
)

type managerConfig struct {
	runtimeSocket   string
	browserSocket   string
	pveMembersFile  string
	clusterName     string
	controlDB       []string
	northbound      []string
	southbound      []string
	ovnTLSCA        string
	ovnTLSCert      string
	ovnTLSKey       string
	reconcileEvery  time.Duration
	orphanGrace     time.Duration
	requireAllNodes bool
	guestMTU        int
	physnet         string
	shutdownWait    time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("pvn-manager stopped", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	defaults := managerConfig{
		runtimeSocket:  env("PVN_RUNTIME_SOCKET", env("PVN_UNIX_SOCKET", "/run/pvn/manager.sock")),
		browserSocket:  env("PVN_BROWSER_SOCKET", "/run/pvn-api/manager.sock"),
		pveMembersFile: os.Getenv("PVN_PVE_MEMBERS_FILE"),
		clusterName:    os.Getenv("PVN_CLUSTER_NAME"),
		shutdownWait:   envDuration("PVN_SHUTDOWN_TIMEOUT", 15*time.Second),
	}
	flags := flag.NewFlagSet("pvn-manager", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", os.Getenv("PVN_CONFIG"), "cluster configuration JSON file")
	flags.StringVar(&defaults.runtimeSocket, "runtime-socket", defaults.runtimeSocket, "node-agent Unix socket path")
	flags.StringVar(&defaults.browserSocket, "browser-socket", defaults.browserSocket, "local pveproxy Unix socket path")
	flags.StringVar(&defaults.pveMembersFile, "pve-members-file", defaults.pveMembersFile, "PVE pmxcfs membership JSON used for the deployment display name")
	flags.StringVar(&defaults.clusterName, "cluster-name", defaults.clusterName, "cluster name override")
	flags.DurationVar(&defaults.shutdownWait, "shutdown-timeout", defaults.shutdownWait, "graceful shutdown timeout")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("pvn-manager %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return nil
	}
	if *configPath == "" {
		return errors.New("--config is required")
	}
	clusterConfig, err := pvnconfig.Load(*configPath)
	if err != nil {
		return err
	}
	explicit := make(map[string]bool)
	flags.Visit(func(item *flag.Flag) { explicit[item.Name] = true })
	applyClusterConfig(&defaults, clusterConfig, explicit)
	if err := applyPVEDeploymentName(&defaults, explicit["cluster-name"]); err != nil {
		return err
	}
	if defaults.shutdownWait <= 0 {
		return errors.New("shutdown-timeout must be positive")
	}
	if defaults.runtimeSocket == "" || defaults.browserSocket == "" || defaults.runtimeSocket == defaults.browserSocket {
		return errors.New("distinct runtime and browser Unix sockets are required")
	}
	if defaults.reconcileEvery <= 0 {
		return errors.New("reconcile interval must be positive")
	}
	if defaults.orphanGrace <= 0 {
		return errors.New("orphan grace must be positive")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	startupContext, startupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startupCancel()
	var controlTLS *tls.Config
	if containsSSL(defaults.controlDB) {
		controlTLS, err = loadMutualTLS(defaults.ovnTLSCA, defaults.ovnTLSCert, defaults.ovnTLSKey)
		if err != nil {
			return err
		}
	}
	store, err := ovsdbstore.Open(startupContext, ovsdbstore.Config{Endpoints: defaults.controlDB, TLSConfig: controlTLS})
	if err != nil {
		return fmt.Errorf("open PVN control store: %w", err)
	}
	defer store.Close()
	ovnClient, err := ovnnb.NewClient(ovnnb.ClientConfig{
		Database:    defaults.northbound,
		TLSCA:       defaults.ovnTLSCA,
		TLSCert:     defaults.ovnTLSCert,
		TLSKey:      defaults.ovnTLSKey,
		Timeout:     15,
		WaitForSync: true,
	})
	if err != nil {
		return fmt.Errorf("configure OVN Northbound client: %w", err)
	}
	if err := ovnClient.ProbeReachable(startupContext); err != nil {
		return err
	}
	southboundProbe, err := ovnnb.NewSouthboundProbe(ovnnb.SouthboundProbeConfig{
		Database: defaults.southbound, TLSCA: defaults.ovnTLSCA,
		TLSCert: defaults.ovnTLSCert, TLSKey: defaults.ovnTLSKey, Timeout: 15,
	})
	if err != nil {
		return fmt.Errorf("configure OVN Southbound probe: %w", err)
	}
	renderer, err := ovnnb.NewRenderer(ovnClient, store)
	if err != nil {
		return err
	}
	controller := reconcile.NewController(store, renderer, reconcile.WithLeaseDuration(defaults.orphanGrace))
	reconcilerHealth := newReconcilerHealth(defaults.reconcileEvery, time.Now)
	handler, err := api.New(api.Options{
		Store: store, Reconciler: controller, Logger: logger,
		RequireAllNodes: defaults.requireAllNodes, NodeHeartbeatTTL: 2 * time.Minute,
		GuestMTU: defaults.guestMTU, Physnet: defaults.physnet,
		ClusterName: defaults.clusterName, NorthboundProbe: ovnClient,
		SouthboundProbe: southboundProbe, ReconcilerProbe: reconcilerHealth,
	})
	if err != nil {
		return err
	}
	repairDefaultSecurityOnStartup(startupContext, handler, logger)
	browserServer := &http.Server{
		Handler:           handler.BrowserHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	runtimeServer := &http.Server{
		Handler:           handler.RuntimeHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	runtimeListener, err := listenUnix(defaults.runtimeSocket)
	if err != nil {
		return fmt.Errorf("listen for PVN runtime API: %w", err)
	}
	defer func() {
		_ = runtimeListener.Close()
		_ = os.Remove(defaults.runtimeSocket)
	}()
	browserListener, err := listenBrowserUnix(defaults.browserSocket, "www-data", "www-data")
	if err != nil {
		return fmt.Errorf("listen for PVN browser API: %w", err)
	}
	defer func() {
		_ = browserListener.Close()
		_ = os.Remove(defaults.browserSocket)
	}()
	reconcileContext, stopReconciler := context.WithCancel(context.Background())
	defer stopReconciler()

	serverErrors := make(chan error, 2)
	go func() {
		logger.Info("pvn-manager browser API listening", "socket", defaults.browserSocket, "version", buildinfo.Version)
		serverErrors <- browserServer.Serve(browserListener)
	}()
	go func() {
		logger.Info("pvn-manager runtime API listening", "socket", defaults.runtimeSocket)
		serverErrors <- runtimeServer.Serve(runtimeListener)
	}()
	go reconcilePeriodically(reconcileContext, managerPeriodicReconciler{controller: controller, defaultSecurity: handler}, defaults.reconcileEvery, reconcilerHealth, logger)

	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var runError error
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			runError = err
		}
	case <-signalContext.Done():
		logger.Info("shutting down pvn-manager")
	}
	stopReconciler()
	shutdownContext, cancel := context.WithTimeout(context.Background(), defaults.shutdownWait)
	defer cancel()
	if err := browserServer.Shutdown(shutdownContext); err != nil {
		_ = browserServer.Close()
		runError = errors.Join(runError, fmt.Errorf("browser API graceful shutdown: %w", err))
	}
	if err := runtimeServer.Shutdown(shutdownContext); err != nil {
		_ = runtimeServer.Close()
		runError = errors.Join(runError, fmt.Errorf("runtime API graceful shutdown: %w", err))
	}
	return runError
}

func applyClusterConfig(target *managerConfig, clusterConfig pvnconfig.Config, explicit map[string]bool) {
	if !explicit["runtime-socket"] {
		target.runtimeSocket = clusterConfig.Manager.UnixSocket
	}
	if !explicit["browser-socket"] {
		target.browserSocket = clusterConfig.Manager.BrowserSocket
	}
	if !explicit["cluster-name"] {
		target.clusterName = clusterConfig.Cluster.ID
	}
	target.controlDB = append([]string(nil), clusterConfig.OVN.ControlDB...)
	target.northbound = append([]string(nil), clusterConfig.OVN.Northbound...)
	target.southbound = append([]string(nil), clusterConfig.OVN.Southbound...)
	target.ovnTLSCA = clusterConfig.OVN.TLSCA
	target.ovnTLSCert = clusterConfig.OVN.TLSCert
	target.ovnTLSKey = clusterConfig.OVN.TLSKey
	target.reconcileEvery = clusterConfig.Cluster.ReconcileEvery
	target.orphanGrace = clusterConfig.Cluster.OrphanGrace
	target.requireAllNodes = clusterConfig.Cluster.RequireAllNodes
	target.guestMTU = clusterConfig.Networking.GuestMTU
	target.physnet = clusterConfig.Networking.Physnet
}

func containsSSL(endpoints []string) bool {
	for _, endpoint := range endpoints {
		if strings.HasPrefix(endpoint, "ssl:") {
			return true
		}
	}
	return false
}

func loadMutualTLS(caPath, certificatePath, keyPath string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read OVN CA certificate %q: %w", caPath, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("OVN CA certificate %q contains no certificates", caPath)
	}
	certificate, err := tls.LoadX509KeyPair(certificatePath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load OVN client certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		Certificates: []tls.Certificate{certificate},
	}, nil
}

type reconcilerHealth struct {
	mu          sync.RWMutex
	interval    time.Duration
	timeout     time.Duration
	clock       func() time.Time
	startedAt   time.Time
	completedAt time.Time
	inFlight    bool
	err         error
}

func newReconcilerHealth(interval time.Duration, clock func() time.Time) *reconcilerHealth {
	return &reconcilerHealth{interval: interval, timeout: reconcilePassTimeout(interval), clock: clock}
}

func (health *reconcilerHealth) start() {
	health.mu.Lock()
	health.startedAt = health.clock().UTC()
	health.inFlight = true
	health.mu.Unlock()
}

func (health *reconcilerHealth) record(err error) {
	health.mu.Lock()
	health.completedAt = health.clock().UTC()
	health.inFlight = false
	health.err = err
	health.mu.Unlock()
}

func (health *reconcilerHealth) Probe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := health.clock().UTC()
	health.mu.RLock()
	startedAt, completedAt := health.startedAt, health.completedAt
	inFlight, reconcileErr := health.inFlight, health.err
	health.mu.RUnlock()
	if inFlight && now.Sub(startedAt) > health.timeout {
		return errors.New("periodic reconciliation exceeded its deadline")
	}
	if completedAt.IsZero() {
		if inFlight {
			return errors.New("first periodic reconciliation is still in progress")
		}
		return errors.New("full reconciliation has not completed")
	}
	if reconcileErr != nil {
		return errors.New("last full reconciliation failed")
	}
	if inFlight {
		return nil
	}
	if now.Sub(completedAt) > 3*health.interval {
		return errors.New("last full reconciliation is stale")
	}
	return nil
}

type periodicReconciler interface {
	ReconcilePeriodic(context.Context, time.Duration) error
}

type defaultSecurityRepairer interface {
	EnsureDefaultSecurityPolicies(context.Context) error
}

type managerPeriodicReconciler struct {
	controller      periodicReconciler
	defaultSecurity defaultSecurityRepairer
}

func (reconciler managerPeriodicReconciler) ReconcilePeriodic(ctx context.Context, freshness time.Duration) error {
	var policyErr, reconcileErr error
	if reconciler.defaultSecurity != nil {
		policyErr = reconciler.defaultSecurity.EnsureDefaultSecurityPolicies(ctx)
	}
	if reconciler.controller != nil {
		reconcileErr = reconciler.controller.ReconcilePeriodic(ctx, freshness)
	}
	return errors.Join(policyErr, reconcileErr)
}

func repairDefaultSecurityOnStartup(ctx context.Context, repairer defaultSecurityRepairer, logger *slog.Logger) {
	if repairer == nil {
		return
	}
	if err := repairer.EnsureDefaultSecurityPolicies(ctx); err != nil {
		logger.Warn("default security policy startup repair incomplete; serving in degraded mode", "error", err)
	}
}

func reconcileFreshness(interval time.Duration) time.Duration {
	const minimumFreshness = 30 * time.Minute
	if freshness := 10 * interval; freshness > minimumFreshness {
		return freshness
	}
	return minimumFreshness
}

func reconcilePassTimeout(interval time.Duration) time.Duration {
	const minimumTimeout = 5 * time.Minute
	if interval > minimumTimeout {
		return interval
	}
	return minimumTimeout
}

func reconcilePeriodically(ctx context.Context, controller periodicReconciler, interval time.Duration, health *reconcilerHealth, logger *slog.Logger) {
	reconcilePeriodicallyWithWait(ctx, controller, interval, health, logger, waitForReconcileInterval)
}

func reconcilePeriodicallyWithWait(ctx context.Context, controller periodicReconciler, interval time.Duration, health *reconcilerHealth, logger *slog.Logger, wait func(context.Context, time.Duration) bool) {
	freshness := reconcileFreshness(interval)
	timeout := reconcilePassTimeout(interval)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		health.start()
		reconcileContext, cancel := context.WithTimeout(ctx, timeout)
		err := controller.ReconcilePeriodic(reconcileContext, freshness)
		cancel()
		health.record(err)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("periodic PVN reconciliation incomplete", "error", err)
		}
		if !wait(ctx, interval) {
			return
		}
	}
}

func waitForReconcileInterval(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
