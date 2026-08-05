package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/api"
	"github.com/pvnstack/proxmox-ovn/internal/buildinfo"
	pvnconfig "github.com/pvnstack/proxmox-ovn/internal/config"
	"github.com/pvnstack/proxmox-ovn/internal/ovnnb"
	"github.com/pvnstack/proxmox-ovn/internal/ovsdbstore"
	"github.com/pvnstack/proxmox-ovn/internal/reconcile"
)

type managerConfig struct {
	listen          string
	unixSocket      string
	tlsCert         string
	tlsKey          string
	pveAPIURL       string
	pveCAFile       string
	clusterName     string
	webRoot         string
	frameAncestors  []string
	controlDB       []string
	northbound      []string
	ovnTLSCA        string
	ovnTLSCert      string
	ovnTLSKey       string
	reconcileEvery  time.Duration
	orphanGrace     time.Duration
	requireAllNodes bool
	guestMTU        int
	physnet         string
	insecureNoAuth  bool
	shutdownWait    time.Duration
	sessionTTL      time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("pvn-manager stopped", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	hostname, _ := os.Hostname()
	defaults := managerConfig{
		listen:         env("PVN_LISTEN_ADDR", "127.0.0.1:8443"),
		unixSocket:     env("PVN_UNIX_SOCKET", "/run/pvn/manager.sock"),
		tlsCert:        os.Getenv("PVN_TLS_CERT"),
		tlsKey:         os.Getenv("PVN_TLS_KEY"),
		pveAPIURL:      env("PVN_PVE_API_URL", "https://"+hostname+":8006"),
		pveCAFile:      env("PVN_PVE_CA_FILE", "/etc/pve/pve-root-ca.pem"),
		clusterName:    os.Getenv("PVN_CLUSTER_NAME"),
		webRoot:        env("PVN_WEB_ROOT", "/usr/share/pvn/web"),
		insecureNoAuth: envBool("PVN_INSECURE_NO_AUTH", false),
		shutdownWait:   envDuration("PVN_SHUTDOWN_TIMEOUT", 15*time.Second),
		sessionTTL:     envDuration("PVN_SESSION_TTL", 15*time.Minute),
	}
	flags := flag.NewFlagSet("pvn-manager", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", os.Getenv("PVN_CONFIG"), "cluster configuration JSON file")
	flags.StringVar(&defaults.listen, "listen", defaults.listen, "HTTP listen address")
	flags.StringVar(&defaults.unixSocket, "unix-socket", defaults.unixSocket, "node-agent Unix socket path (empty disables)")
	flags.StringVar(&defaults.tlsCert, "tls-cert", defaults.tlsCert, "TLS certificate file")
	flags.StringVar(&defaults.tlsKey, "tls-key", defaults.tlsKey, "TLS private key file")
	flags.StringVar(&defaults.pveAPIURL, "pve-api-url", defaults.pveAPIURL, "local Proxmox API base URL")
	flags.StringVar(&defaults.pveCAFile, "pve-ca-file", defaults.pveCAFile, "Proxmox cluster CA PEM file")
	flags.StringVar(&defaults.clusterName, "cluster-name", defaults.clusterName, "cluster name override")
	flags.StringVar(&defaults.webRoot, "web-root", defaults.webRoot, "compiled PVN UI directory")
	flags.BoolVar(&defaults.insecureNoAuth, "insecure-no-auth", defaults.insecureNoAuth, "disable PVE session validation (development only)")
	flags.DurationVar(&defaults.shutdownWait, "shutdown-timeout", defaults.shutdownWait, "graceful shutdown timeout")
	flags.DurationVar(&defaults.sessionTTL, "session-ttl", defaults.sessionTTL, "PVN browser session lifetime")
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
	if (defaults.tlsCert == "") != (defaults.tlsKey == "") {
		return errors.New("tls-cert and tls-key must be configured together")
	}
	if defaults.shutdownWait <= 0 {
		return errors.New("shutdown-timeout must be positive")
	}
	if defaults.sessionTTL <= 0 {
		return errors.New("session-ttl must be positive")
	}
	_, listenPort, listenErr := net.SplitHostPort(defaults.listen)
	if listenErr != nil || listenPort != "8443" {
		return errors.New("listen address must use port 8443 for the Proxmox PVN UI")
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
	if err := ovnClient.Probe(startupContext); err != nil {
		return err
	}
	renderer, err := ovnnb.NewRenderer(ovnClient, store)
	if err != nil {
		return err
	}
	controller := reconcile.NewController(store, renderer, reconcile.WithLeaseDuration(defaults.orphanGrace))
	var sessionProvider api.SessionProvider
	if defaults.insecureNoAuth {
		logger.Warn("PVE authentication is disabled; do not use this mode in production")
	} else {
		provider, err := api.NewPVESessionProvider(api.PVESessionProviderOptions{BaseURL: defaults.pveAPIURL, CAFile: defaults.pveCAFile, ClusterName: defaults.clusterName, SessionTTL: defaults.sessionTTL})
		if err != nil {
			return err
		}
		sessionProvider = provider
	}
	handler, err := api.New(api.Options{
		Store: store, Reconciler: controller, SessionProvider: sessionProvider, Logger: logger,
		RequireAllNodes: defaults.requireAllNodes, NodeHeartbeatTTL: 2 * time.Minute,
		GuestMTU: defaults.guestMTU, Physnet: defaults.physnet,
		ClusterName: defaults.clusterName,
	})
	if err != nil {
		return err
	}
	application, err := api.NewApplicationHandler(handler, api.WebOptions{Root: defaults.webRoot, FrameAncestors: defaults.frameAncestors})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              defaults.listen,
		Handler:           application,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	runtimeServer := &http.Server{
		Handler:           handler.RuntimeHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	var runtimeListener net.Listener
	if defaults.unixSocket != "" {
		listener, listenErr := listenUnix(defaults.unixSocket)
		if listenErr != nil {
			return listenErr
		}
		runtimeListener = listener
		defer func() {
			_ = runtimeListener.Close()
			_ = os.Remove(defaults.unixSocket)
		}()
	}
	reconcileContext, stopReconciler := context.WithCancel(context.Background())
	defer stopReconciler()
	if err := controller.ReconcileAll(startupContext); err != nil {
		logger.Warn("initial PVN reconciliation incomplete", "error", err)
	}
	go reconcilePeriodically(reconcileContext, controller, defaults.reconcileEvery, logger)

	serverErrors := make(chan error, 2)
	go func() {
		logger.Info("pvn-manager listening", "address", defaults.listen, "version", buildinfo.Version, "tls", defaults.tlsCert != "")
		if defaults.tlsCert != "" {
			serverErrors <- httpServer.ListenAndServeTLS(defaults.tlsCert, defaults.tlsKey)
			return
		}
		serverErrors <- httpServer.ListenAndServe()
	}()
	if runtimeListener != nil {
		go func() {
			logger.Info("pvn-manager runtime API listening", "socket", defaults.unixSocket)
			serverErrors <- runtimeServer.Serve(runtimeListener)
		}()
	}

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
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		_ = httpServer.Close()
		runError = errors.Join(runError, fmt.Errorf("graceful shutdown: %w", err))
	}
	if runtimeListener != nil {
		if err := runtimeServer.Shutdown(shutdownContext); err != nil {
			_ = runtimeServer.Close()
			runError = errors.Join(runError, fmt.Errorf("runtime API graceful shutdown: %w", err))
		}
	}
	return runError
}

func applyClusterConfig(target *managerConfig, clusterConfig pvnconfig.Config, explicit map[string]bool) {
	if !explicit["listen"] {
		target.listen = clusterConfig.Manager.ListenAddress
	}
	if !explicit["unix-socket"] {
		target.unixSocket = clusterConfig.Manager.UnixSocket
	}
	if !explicit["tls-cert"] {
		target.tlsCert = clusterConfig.Manager.TLSCert
	}
	if !explicit["tls-key"] {
		target.tlsKey = clusterConfig.Manager.TLSKey
	}
	if !explicit["pve-api-url"] {
		target.pveAPIURL = clusterConfig.Manager.PVEURL
	}
	if !explicit["cluster-name"] {
		target.clusterName = clusterConfig.Cluster.ID
	}
	if !explicit["web-root"] {
		target.webRoot = clusterConfig.Manager.WebRoot
	}
	if !explicit["session-ttl"] {
		target.sessionTTL = clusterConfig.Security.SessionTTL
	}
	target.frameAncestors = append([]string(nil), clusterConfig.Security.AllowedOrigins...)
	target.controlDB = append([]string(nil), clusterConfig.OVN.ControlDB...)
	target.northbound = append([]string(nil), clusterConfig.OVN.Northbound...)
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

func reconcilePeriodically(ctx context.Context, controller *reconcile.Controller, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileContext, cancel := context.WithTimeout(ctx, interval)
			err := controller.ReconcileAll(reconcileContext)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("periodic PVN reconciliation incomplete", "error", err)
			}
		}
	}
}

func listenUnix(path string) (net.Listener, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create Unix socket directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket path %q", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale Unix socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Unix socket path: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("set Unix socket permissions: %w", err)
	}
	return listener, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
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
