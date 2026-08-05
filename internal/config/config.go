package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultPath = "/etc/pve/pvn/config.json"

type Config struct {
	Cluster    ClusterConfig    `json:"cluster"`
	Manager    ManagerConfig    `json:"manager"`
	Agent      AgentConfig      `json:"agent"`
	OVN        OVNConfig        `json:"ovn"`
	Networking NetworkingConfig `json:"networking"`
	Security   SecurityConfig   `json:"security"`
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
	ListenAddress string `json:"listen_address"`
	PublicPort    int    `json:"public_port"`
	PVEURL        string `json:"pve_url"`
	UnixSocket    string `json:"unix_socket"`
	WebRoot       string `json:"web_root"`
	TLSCert       string `json:"tls_cert"`
	TLSKey        string `json:"tls_key"`
}

type AgentConfig struct {
	PollEvery    time.Duration `json:"poll_every"`
	Bridge       string        `json:"bridge"`
	ManagerURL   string        `json:"manager_url"`
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

type SecurityConfig struct {
	AllowedOrigins []string      `json:"allowed_origins,omitempty"`
	SessionTTL     time.Duration `json:"session_ttl"`
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
			ListenAddress: ":8443",
			PublicPort:    8443,
			PVEURL:        "https://127.0.0.1:8006",
			UnixSocket:    "/run/pvn/manager.sock",
			WebRoot:       "/usr/share/pvn/web",
			TLSCert:       "/etc/pve/local/pveproxy-ssl.pem",
			TLSKey:        "/etc/pve/local/pveproxy-ssl.key",
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
		Security: SecurityConfig{SessionTTL: 15 * time.Minute},
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

func applyEnv(cfg *Config) {
	if value := os.Getenv("PVN_LISTEN_ADDRESS"); value != "" {
		cfg.Manager.ListenAddress = value
	}
	if value := os.Getenv("PVN_PVE_URL"); value != "" {
		cfg.Manager.PVEURL = value
	}
	if value := os.Getenv("PVN_TLS_CERT"); value != "" {
		cfg.Manager.TLSCert = value
	}
	if value := os.Getenv("PVN_TLS_KEY"); value != "" {
		cfg.Manager.TLSKey = value
	}
	if value := os.Getenv("PVN_NODE_NAME"); value != "" {
		cfg.Cluster.NodeName = value
	}
	if value := os.Getenv("PVN_MANAGER_URL"); value != "" {
		cfg.Agent.ManagerURL = value
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
	if c.Agent.PollEvery <= 0 {
		problems = append(problems, "agent.poll_every must be positive")
	}
	if c.Agent.Bridge == "" {
		problems = append(problems, "agent.bridge is required")
	}
	if c.Networking.EncapType != "geneve" {
		problems = append(problems, "networking.encap_type must be geneve")
	}
	if c.Networking.GuestMTU < 576 || c.Networking.GuestMTU > 9000 {
		problems = append(problems, "networking.guest_mtu must be between 576 and 9000")
	}
	parsedPVE, err := url.Parse(c.Manager.PVEURL)
	if err != nil || parsedPVE.Scheme != "https" || parsedPVE.Host == "" {
		problems = append(problems, "manager.pve_url must be an absolute HTTPS URL")
	}
	parsedManager, err := url.Parse(c.Agent.ManagerURL)
	if err != nil || (parsedManager.Scheme != "https" && parsedManager.Scheme != "unix") {
		problems = append(problems, "agent.manager_url must use HTTPS or a Unix socket URL")
	} else if parsedManager.Scheme == "https" && parsedManager.Host == "" {
		problems = append(problems, "agent.manager_url HTTPS address must include a host")
	} else if parsedManager.Scheme == "unix" && (parsedManager.Path == "" || parsedManager.Host != "") {
		problems = append(problems, "agent.manager_url Unix address must be an absolute socket path")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
