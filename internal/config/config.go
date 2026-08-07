package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultPath = "/etc/pve/pvn/config.json"
const DefaultNodeEnvPath = "/etc/pvn/node.env"

type Config struct {
	Cluster    ClusterConfig    `json:"cluster"`
	Manager    ManagerConfig    `json:"manager"`
	Agent      AgentConfig      `json:"agent"`
	OVN        OVNConfig        `json:"ovn"`
	Networking NetworkingConfig `json:"networking"`
}

type ClusterConfig struct {
	ID                string        `json:"id"`
	NodeName          string        `json:"node_name,omitempty"`
	ReconcileEvery    time.Duration `json:"reconcile_every"`
	OrphanGrace       time.Duration `json:"orphan_grace"`
	RequireAllNodes   bool          `json:"require_all_nodes"`
	SupportedPVEMajor int           `json:"supported_pve_major"`
}

type ManagerConfig struct {
	UnixSocket    string `json:"unix_socket"`
	BrowserSocket string `json:"browser_socket"`
}

type AgentConfig struct {
	PollEvery    time.Duration `json:"poll_every"`
	Bridge       string        `json:"bridge"`
	ManagerURL   string        `json:"manager_url"`
	ManagerCA    string        `json:"manager_ca,omitempty"`
	SystemIDFile string        `json:"system_id_file"`
}

type OVNConfig struct {
	ControlDB  []string `json:"control_db"`
	Northbound []string `json:"northbound"`
	Southbound []string `json:"southbound"`
	TLSCA      string   `json:"tls_ca"`
	TLSCert    string   `json:"tls_cert"`
	TLSKey     string   `json:"tls_key"`
}

type NetworkingConfig struct {
	EncapType      string `json:"encap_type"`
	EncapIP        string `json:"encap_ip,omitempty"`
	GuestMTU       int    `json:"guest_mtu"`
	Physnet        string `json:"physnet"`
	ProviderBridge string `json:"provider_bridge"`
}

func Default() Config {
	return Config{
		Cluster: ClusterConfig{
			ReconcileEvery:    30 * time.Second,
			OrphanGrace:       5 * time.Minute,
			RequireAllNodes:   true,
			SupportedPVEMajor: 9,
		},
		Manager: ManagerConfig{
			UnixSocket:    "/run/pvn/manager.sock",
			BrowserSocket: "/run/pvn-api/manager.sock",
		},
		Agent: AgentConfig{
			PollEvery:    2 * time.Second,
			Bridge:       "br-int",
			ManagerURL:   "unix:///run/pvn/manager.sock",
			SystemIDFile: "/etc/openvswitch/system-id.conf",
		},
		Networking: NetworkingConfig{
			EncapType:      "geneve",
			GuestMTU:       1400,
			Physnet:        "provider",
			ProviderBridge: "br-provider",
		},
	}
}

// Load reads a cluster-wide JSON configuration. Environment overrides are
// deliberately limited to local process settings; desired network state must
// never be passed through process environment variables.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %q: %w", path, err)
	}
	return cfg, nil
}

// LoadNode overlays the node-local settings that cannot live in the shared
// pmxcfs configuration. It parses the packaged node.env contract directly;
// callers must not rely on a systemd or interactive-shell environment having
// already imported the file.
func LoadNode(path, nodeEnvPath string) (Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return Config{}, err
	}
	values, err := readNodeEnv(nodeEnvPath)
	if err != nil {
		return Config{}, err
	}
	if value := values["PVN_NODE_NAME"]; value != "" {
		cfg.Cluster.NodeName = value
	}
	cfg.Networking.EncapIP = values["PVN_ENCAP_IP"]
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config with node environment %q: %w", nodeEnvPath, err)
	}
	return cfg, nil
}

func readNodeEnv(path string) (map[string]string, error) {
	if path == "" {
		return nil, errors.New("node environment path is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect node environment %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("node environment %q must be a regular non-symlink file", path)
	}
	// Permit the installed 0640 mode and stricter read-only variants. Never
	// trust a file writable by the group/others or readable by others.
	if permissions := info.Mode().Perm(); permissions&0o400 == 0 || permissions&^os.FileMode(0o640) != 0 {
		return nil, fmt.Errorf("node environment %q has unsafe permissions %04o", path, permissions)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read node environment %q: %w", path, err)
	}
	defer file.Close()

	allowed := map[string]bool{
		"PVN_NODE_NAME": true, "PVN_ENCAP_IP": true, "PVN_PVE_URL": true,
		"PVN_NODE_ROLES": true, "PVN_HEALTH_LISTEN": true,
		"PVN_AGENT_HEALTH_URL": true,
	}
	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || key == "" || strings.TrimSpace(key) != key || value == "" {
			return nil, fmt.Errorf("parse node environment %q line %d: expected KEY=VALUE", path, lineNumber)
		}
		if !allowed[key] {
			return nil, fmt.Errorf("parse node environment %q line %d: unsupported key %q", path, lineNumber, key)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("parse node environment %q line %d: duplicate key %q", path, lineNumber, key)
		}
		if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\\\"'\\#;\x00") {
			return nil, fmt.Errorf("parse node environment %q line %d: %s must be an unquoted single value", path, lineNumber, key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read node environment %q: %w", path, err)
	}
	if values["PVN_ENCAP_IP"] == "" {
		return nil, fmt.Errorf("node environment %q requires PVN_ENCAP_IP", path)
	}
	return values, nil
}

func applyEnv(cfg *Config) {
	if value := os.Getenv("PVN_NODE_NAME"); value != "" {
		cfg.Cluster.NodeName = value
	}
	if value := os.Getenv("PVN_MANAGER_URL"); value != "" {
		cfg.Agent.ManagerURL = value
	}
	if value := os.Getenv("PVN_MANAGER_CA"); value != "" {
		cfg.Agent.ManagerCA = value
	}
	if value := os.Getenv("PVN_ENCAP_IP"); value != "" {
		cfg.Networking.EncapIP = value
	}
	if value := os.Getenv("PVN_GUEST_MTU"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.Networking.GuestMTU = parsed
		}
	}
}

func (c Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Cluster.ID) == "" {
		problems = append(problems, "cluster.id is required")
	}
	if c.Cluster.SupportedPVEMajor != 9 {
		problems = append(problems, "only Proxmox VE major version 9 is supported")
	}
	if c.Cluster.ReconcileEvery <= 0 {
		problems = append(problems, "cluster.reconcile_every must be positive")
	}
	if c.Cluster.OrphanGrace < time.Minute {
		problems = append(problems, "cluster.orphan_grace must be at least one minute")
	}
	if c.Agent.PollEvery <= 0 {
		problems = append(problems, "agent.poll_every must be positive")
	}
	if !filepath.IsAbs(c.Manager.UnixSocket) {
		problems = append(problems, "manager.unix_socket must be an absolute path")
	}
	if !filepath.IsAbs(c.Manager.BrowserSocket) {
		problems = append(problems, "manager.browser_socket must be an absolute path")
	}
	if filepath.Clean(c.Manager.UnixSocket) == filepath.Clean(c.Manager.BrowserSocket) {
		problems = append(problems, "manager.browser_socket must differ from manager.unix_socket")
	}
	if c.Agent.Bridge == "" {
		problems = append(problems, "agent.bridge is required")
	}
	if !filepath.IsAbs(c.Agent.SystemIDFile) {
		problems = append(problems, "agent.system_id_file must be an absolute path")
	}
	if c.Networking.EncapType != "geneve" {
		problems = append(problems, "networking.encap_type must be geneve")
	}
	if c.Networking.EncapIP != "" {
		address, err := netip.ParseAddr(c.Networking.EncapIP)
		if err != nil || !address.Is4() {
			problems = append(problems, "networking.encap_ip must be an IPv4 address")
		}
	}
	if c.Networking.GuestMTU < 576 || c.Networking.GuestMTU > 9000 {
		problems = append(problems, "networking.guest_mtu must be between 576 and 9000")
	}
	if strings.TrimSpace(c.Networking.Physnet) == "" {
		problems = append(problems, "networking.physnet is required")
	}
	if strings.TrimSpace(c.Networking.ProviderBridge) == "" {
		problems = append(problems, "networking.provider_bridge is required")
	}
	parsedManager, err := url.Parse(c.Agent.ManagerURL)
	if err != nil || parsedManager.Scheme != "unix" {
		problems = append(problems, "agent.manager_url must use a Unix socket URL")
	} else if !filepath.IsAbs(parsedManager.Path) || parsedManager.Host != "" {
		problems = append(problems, "agent.manager_url Unix address must be an absolute socket path")
	} else if filepath.Clean(parsedManager.Path) == filepath.Clean(c.Manager.BrowserSocket) {
		problems = append(problems, "agent.manager_url must not use manager.browser_socket")
	}
	usesOVNTLS := false
	for label, endpoints := range map[string][]string{
		"ovn.control_db": c.OVN.ControlDB,
		"ovn.northbound": c.OVN.Northbound,
		"ovn.southbound": c.OVN.Southbound,
	} {
		needsTLS, endpointProblems := validateOVSDBEndpoints(label, endpoints)
		usesOVNTLS = usesOVNTLS || needsTLS
		problems = append(problems, endpointProblems...)
	}
	if usesOVNTLS {
		for label, path := range map[string]string{
			"ovn.tls_ca":   c.OVN.TLSCA,
			"ovn.tls_cert": c.OVN.TLSCert,
			"ovn.tls_key":  c.OVN.TLSKey,
		} {
			if !filepath.IsAbs(path) {
				problems = append(problems, label+" must be an absolute path when SSL endpoints are configured")
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validateOVSDBEndpoints(label string, endpoints []string) (bool, []string) {
	if len(endpoints) == 0 {
		return false, []string{label + " requires at least one endpoint"}
	}
	usesTLS := false
	var problems []string
	for _, endpoint := range endpoints {
		if strings.TrimSpace(endpoint) != endpoint || strings.ContainsAny(endpoint, "\r\n\x00,") {
			problems = append(problems, fmt.Sprintf("%s contains an unsafe endpoint %q", label, endpoint))
			continue
		}
		switch {
		case strings.HasPrefix(endpoint, "unix:"):
			if !filepath.IsAbs(strings.TrimPrefix(endpoint, "unix:")) {
				problems = append(problems, fmt.Sprintf("%s Unix endpoint must use an absolute path: %q", label, endpoint))
			}
		case strings.HasPrefix(endpoint, "ssl:"):
			usesTLS = true
			host, port, err := net.SplitHostPort(strings.TrimPrefix(endpoint, "ssl:"))
			if err != nil || host == "" || port == "" {
				problems = append(problems, fmt.Sprintf("%s SSL endpoint must use ssl:HOST:PORT syntax: %q", label, endpoint))
				continue
			}
			portNumber, portErr := strconv.Atoi(port)
			if portErr != nil || portNumber < 1 || portNumber > 65535 {
				problems = append(problems, fmt.Sprintf("%s SSL endpoint has an invalid port: %q", label, endpoint))
			}
		default:
			problems = append(problems, fmt.Sprintf("%s endpoint must use unix: or ssl: transport: %q", label, endpoint))
		}
	}
	return usesTLS, problems
}
