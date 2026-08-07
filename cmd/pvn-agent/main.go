package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/agent"
	"github.com/popododo0720/proxmox-ovn/internal/buildinfo"
	pvnconfig "github.com/popododo0720/proxmox-ovn/internal/config"
	"github.com/popododo0720/proxmox-ovn/internal/ovs"
	"github.com/popododo0720/proxmox-ovn/internal/pve"
)

const defaultHealthListen = "127.0.0.1:9476"
const nodeHeartbeatInterval = 30 * time.Second

type agentConfig struct {
	configPath        string
	node              string
	bridge            string
	managerURL        string
	systemIDFile      string
	watchInterval     time.Duration
	ovsVSCTL          string
	ovsTimeout        int
	healthListen      string
	membershipFile    string
	requireAllNodes   bool
	nodeRoles         []string
	nodeRolesExplicit bool
	once              bool
	version           bool
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Hostname); err != nil {
		slog.Error("pvn-agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string, getenv func(string) string, hostname func() (string, error)) error {
	config, err := parseConfig(arguments, getenv, hostname)
	if err != nil {
		return err
	}
	if config.version {
		fmt.Printf("pvn-agent %s (%s, %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return nil
	}

	chassisID, err := readSystemID(config.systemIDFile)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ovsClient, err := ovs.NewClient(ovs.ClientConfig{
		Binary:         config.ovsVSCTL,
		TimeoutSeconds: config.ovsTimeout,
	})
	if err != nil {
		return err
	}
	managerClient, err := agent.NewHTTPManagerClient(agent.HTTPManagerClientConfig{
		BaseURL: config.managerURL,
	})
	if err != nil {
		return err
	}
	heartbeat := agent.NodeHeartbeat{
		Name: config.node, ChassisID: chassisID,
		Roles: config.nodeRoles, RolesExplicit: config.nodeRolesExplicit,
	}
	if config.requireAllNodes {
		heartbeat, err = heartbeatWithMembership(heartbeat, config.membershipFile)
		if err != nil {
			return err
		}
	}
	heartbeatCtx, heartbeatCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = managerClient.HeartbeatNode(heartbeatCtx, heartbeat)
	heartbeatCancel()
	if err != nil {
		return fmt.Errorf("register node with PVN manager: %w", err)
	}
	watcher, err := agent.NewWatcher(agent.WatcherConfig{
		Node:      config.node,
		ChassisID: chassisID,
		Bridge:    config.bridge,
		Interval:  config.watchInterval,
		Source:    ovsClient,
		Binder:    ovsClient,
		Manager:   managerClient,
		Logger:    logger,
	})
	if err != nil {
		return err
	}

	if config.once {
		report, err := watcher.ScanOnce(context.Background())
		encoded, _ := json.Marshal(report)
		fmt.Println(string(encoded))
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	logger.Info("starting pvn-agent",
		"version", buildinfo.Version,
		"node", config.node,
		"chassis_id", chassisID,
		"bridge", config.bridge,
		"manager_url", config.managerURL,
		"interval", config.watchInterval,
	)

	watchErrors := make(chan error, 1)
	go func() { watchErrors <- watcher.Run(ctx) }()
	go runNodeHeartbeats(ctx, managerClient, heartbeat, config.membershipFile, config.requireAllNodes, logger)

	var server *http.Server
	serverErrors := make(chan error, 1)
	if config.healthListen != "" {
		server = &http.Server{
			Addr:              config.healthListen,
			Handler:           newHealthHandler(watcher.Status, config.watchInterval),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			err := server.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			serverErrors <- err
		}()
	}

	var runErr error
	if server == nil {
		select {
		case <-ctx.Done():
		case runErr = <-watchErrors:
		}
	} else {
		select {
		case <-ctx.Done():
		case runErr = <-watchErrors:
		case runErr = <-serverErrors:
		}
	}
	cancel()
	if server != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	return runErr
}

// parseConfig applies settings in increasing precedence: package defaults,
// cluster JSON, node-local environment, and finally command-line flags.
func parseConfig(arguments []string, getenv func(string) string, hostname func() (string, error)) (agentConfig, error) {
	if len(arguments) == 1 && (arguments[0] == "--version" || arguments[0] == "-version") {
		return agentConfig{version: true}, nil
	}
	configPath, err := findConfigPath(arguments, getenv("PVN_CONFIG"))
	if err != nil {
		return agentConfig{}, err
	}
	clusterConfig, err := pvnconfig.Load(configPath)
	if err != nil {
		return agentConfig{}, err
	}

	defaults := agentConfig{
		configPath:      configPath,
		node:            clusterConfig.Cluster.NodeName,
		bridge:          clusterConfig.Agent.Bridge,
		managerURL:      clusterConfig.Agent.ManagerURL,
		systemIDFile:    clusterConfig.Agent.SystemIDFile,
		watchInterval:   clusterConfig.Agent.PollEvery,
		ovsVSCTL:        "ovs-vsctl",
		ovsTimeout:      5,
		healthListen:    defaultHealthListen,
		membershipFile:  pve.DefaultMembershipFile,
		requireAllNodes: clusterConfig.Cluster.RequireAllNodes,
	}

	// These settings describe only this process or node, so allowing them to
	// override the shared /etc/pve JSON cannot change desired network state.
	if value := firstNonEmpty(getenv("PVN_NODE_NAME"), getenv("PVN_NODE")); value != "" {
		defaults.node = value
	}
	if defaults.node == "" {
		defaults.node, err = hostname()
		if err != nil {
			return agentConfig{}, fmt.Errorf("determine node hostname: %w", err)
		}
	}
	if value := getenv("PVN_BRIDGE"); value != "" {
		defaults.bridge = value
	}
	if value := getenv("PVN_SYSTEM_ID_FILE"); value != "" {
		defaults.systemIDFile = value
	}
	if value := getenv("PVN_WATCH_INTERVAL"); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			return agentConfig{}, fmt.Errorf("parse PVN_WATCH_INTERVAL: %w", err)
		}
		defaults.watchInterval = interval
	}
	if value := getenv("PVN_OVS_VSCTL"); value != "" {
		defaults.ovsVSCTL = value
	}
	if value := getenv("PVN_OVS_TIMEOUT"); value != "" {
		timeout, err := strconv.Atoi(value)
		if err != nil {
			return agentConfig{}, fmt.Errorf("parse PVN_OVS_TIMEOUT: %w", err)
		}
		defaults.ovsTimeout = timeout
	}
	if value := getenv("PVN_HEALTH_LISTEN"); value != "" {
		defaults.healthListen = value
	}
	if value := getenv("PVN_MEMBERSHIP_FILE"); value != "" {
		defaults.membershipFile = value
	}
	if value := getenv("PVN_NODE_ROLES"); value != "" {
		roles, parseErr := parseNodeRoles(value)
		if parseErr != nil {
			return agentConfig{}, fmt.Errorf("parse PVN_NODE_ROLES: %w", parseErr)
		}
		defaults.nodeRoles = roles
		defaults.nodeRolesExplicit = true
	}

	flags := flag.NewFlagSet("pvn-agent", flag.ContinueOnError)
	flags.StringVar(&defaults.configPath, "config", defaults.configPath, "cluster configuration JSON")
	flags.StringVar(&defaults.node, "node", defaults.node, "PVE node name")
	flags.StringVar(&defaults.bridge, "bridge", defaults.bridge, "OVS integration bridge")
	flags.StringVar(&defaults.systemIDFile, "system-id-file", defaults.systemIDFile, "OVS persistent system ID file")
	flags.DurationVar(&defaults.watchInterval, "watch-interval", defaults.watchInterval, "OVS interface scan interval")
	flags.StringVar(&defaults.ovsVSCTL, "ovs-vsctl", defaults.ovsVSCTL, "ovs-vsctl binary path")
	flags.IntVar(&defaults.ovsTimeout, "ovs-timeout", defaults.ovsTimeout, "ovs-vsctl timeout in seconds")
	flags.StringVar(&defaults.healthListen, "health-listen", defaults.healthListen, "health HTTP listen address (empty disables)")
	flags.StringVar(&defaults.membershipFile, "membership-file", defaults.membershipFile, "PVE pmxcfs membership JSON")
	flags.BoolVar(&defaults.once, "once", false, "scan once and exit")
	flags.BoolVar(&defaults.version, "version", false, "print version and exit")
	if err := flags.Parse(arguments); err != nil {
		return agentConfig{}, err
	}
	if flags.NArg() != 0 {
		return agentConfig{}, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if defaults.configPath != configPath {
		return agentConfig{}, errors.New("internal error: --config was not applied before loading configuration")
	}
	if defaults.node == "" || defaults.bridge == "" || defaults.managerURL == "" || defaults.systemIDFile == "" || (defaults.requireAllNodes && defaults.membershipFile == "") {
		return agentConfig{}, errors.New("node, bridge, manager URL, system ID file, and required membership file must be configured")
	}
	if defaults.watchInterval <= 0 || defaults.ovsTimeout <= 0 {
		return agentConfig{}, errors.New("watch interval and OVS timeout must be positive")
	}
	return defaults, nil
}

func parseNodeRoles(value string) ([]string, error) {
	seen := make(map[string]bool, 3)
	for _, raw := range strings.Split(value, ",") {
		role := strings.TrimSpace(raw)
		if role != "compute" && role != "gateway" && role != "central" {
			return nil, fmt.Errorf("unknown node role %q", role)
		}
		if seen[role] {
			return nil, fmt.Errorf("duplicate node role %q", role)
		}
		seen[role] = true
	}
	roles := make([]string, 0, len(seen))
	for _, role := range []string{"compute", "gateway", "central"} {
		if seen[role] {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return nil, errors.New("at least one node role is required")
	}
	return roles, nil
}

func runNodeHeartbeats(ctx context.Context, client *agent.HTTPManagerClient, heartbeat agent.NodeHeartbeat, membershipFile string, requireAllNodes bool, logger *slog.Logger) {
	ticker := time.NewTicker(nodeHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := heartbeat
			if requireAllNodes {
				var err error
				current, err = heartbeatWithMembership(current, membershipFile)
				if err != nil {
					logger.Error("read PVE cluster membership for heartbeat", "error", err)
					continue
				}
			}
			heartbeatCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := client.HeartbeatNode(heartbeatCtx, current)
			cancel()
			if err != nil && ctx.Err() == nil {
				logger.Error("PVN node heartbeat failed", "error", err)
			}
		}
	}
}

func heartbeatWithMembership(heartbeat agent.NodeHeartbeat, path string) (agent.NodeHeartbeat, error) {
	membership, err := pve.ReadClusterMembership(path)
	if err != nil {
		return heartbeat, err
	}
	if membership.Reporter != heartbeat.Name {
		return heartbeat, fmt.Errorf("PVE membership reporter %q does not match PVN node %q", membership.Reporter, heartbeat.Name)
	}
	heartbeat.OnlineNodes = append([]string(nil), membership.OnlineNodes...)
	heartbeat.Quorate = &membership.Quorate
	return heartbeat, nil
}

func findConfigPath(arguments []string, environmentValue string) (string, error) {
	path := environmentValue
	if path == "" {
		path = pvnconfig.DefaultPath
	}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if strings.HasPrefix(argument, "--config=") || strings.HasPrefix(argument, "-config=") {
			_, path, _ = strings.Cut(argument, "=")
			if path == "" {
				return "", errors.New("--config requires a path")
			}
			continue
		}
		if argument == "--config" || argument == "-config" {
			index++
			if index >= len(arguments) || arguments[index] == "" {
				return "", errors.New("--config requires a path")
			}
			path = arguments[index]
		}
	}
	return path, nil
}

func readSystemID(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read OVS system ID %q: %w", path, err)
	}
	value := strings.TrimSpace(string(content))
	if key, parsed, found := strings.Cut(value, "="); found {
		if strings.TrimSpace(key) != "system-id" {
			return "", fmt.Errorf("invalid OVS system ID file %q", path)
		}
		value = strings.Trim(strings.TrimSpace(parsed), `"'`)
	}
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("invalid OVS system ID file %q", path)
	}
	return value, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func newHealthHandler(status func() agent.WatcherStatus, interval time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		current := status()
		writer.Header().Set("Content-Type", "application/json")
		staleAfter := 3 * interval
		if staleAfter < 30*time.Second {
			staleAfter = 30 * time.Second
		}
		if current.LastSuccess.IsZero() || time.Since(current.LastSuccess) > staleAfter {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(writer).Encode(current)
	})
	mux.HandleFunc("GET /status", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(status())
	})
	return mux
}
