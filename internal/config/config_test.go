package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadNodeUsesExplicitLocalEncapIP(t *testing.T) {
	configPath := writeValidConfig(t)
	t.Setenv("PVN_ENCAP_IP", "198.51.100.99")

	for _, test := range []struct {
		name string
		ip   string
	}{
		{name: "first-node", ip: "192.0.2.10"},
		{name: "second-node", ip: "192.0.2.11"},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodeEnvPath := filepath.Join(t.TempDir(), "node.env")
			contents := "# node-local settings\nPVN_NODE_NAME=" + test.name + "\nPVN_ENCAP_IP=" + test.ip + "\n"
			if err := os.WriteFile(nodeEnvPath, []byte(contents), 0o640); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadNode(configPath, nodeEnvPath)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Networking.EncapIP != test.ip || cfg.Cluster.NodeName != test.name {
				t.Fatalf("node-local overlay not applied: %+v", cfg)
			}
		})
	}
}

func TestLoadNodeFailsClosed(t *testing.T) {
	configPath := writeValidConfig(t)
	tests := []struct {
		name     string
		contents string
		mode     os.FileMode
		want     string
	}{
		{name: "missing-encap", contents: "PVN_NODE_NAME=pve-a\n", mode: 0o640, want: "requires PVN_ENCAP_IP"},
		{name: "duplicate", contents: "PVN_ENCAP_IP=192.0.2.10\nPVN_ENCAP_IP=192.0.2.11\n", mode: 0o640, want: "duplicate key"},
		{name: "unsupported", contents: "PVN_ENCAP_IP=192.0.2.10\nUNSAFE=value\n", mode: 0o640, want: "unsupported key"},
		{name: "shell-syntax", contents: "PVN_ENCAP_IP='192.0.2.10'\n", mode: 0o640, want: "unquoted single value"},
		{name: "invalid-ip", contents: "PVN_ENCAP_IP=not-an-ip\n", mode: 0o640, want: "must be an IPv4 address"},
		{name: "unsafe-permissions", contents: "PVN_ENCAP_IP=192.0.2.10\n", mode: 0o660, want: "unsafe permissions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.env")
			if err := os.WriteFile(path, []byte(test.contents), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadNode(configPath, path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q failure, got %v", test.want, err)
			}
		})
	}

	missing := filepath.Join(t.TempDir(), "missing.env")
	if _, err := LoadNode(configPath, missing); err == nil || !strings.Contains(err.Error(), "inspect node environment") {
		t.Fatalf("missing node environment must fail closed: %v", err)
	}
	realPath := filepath.Join(t.TempDir(), "real.env")
	if err := os.WriteFile(realPath, []byte("PVN_ENCAP_IP=192.0.2.10\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(t.TempDir(), "node.env")
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNode(configPath, symlinkPath); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlinked node environment must fail closed: %v", err)
	}
}

func writeValidConfig(t *testing.T) string {
	t.Helper()
	cfg := validConfig()
	cfg.Networking.EncapIP = ""
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAndEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
  "cluster": {"id":"lab", "reconcile_every":30000000000, "orphan_grace":300000000000, "require_all_nodes":true, "supported_pve_major":9},
  "manager": {"unix_socket":"/run/pvn/manager.sock", "browser_socket":"/run/pvn-api/manager.sock"},
  "agent": {"poll_every":2000000000, "bridge":"br-int", "manager_url":"unix:///run/pvn/manager.sock", "system_id_file":"/etc/openvswitch/system-id.conf"},
  "ovn": {"control_db":["unix:/run/pvn/control.sock"], "northbound":["unix:/run/ovn/ovnnb_db.sock"], "southbound":["unix:/run/ovn/ovnsb_db.sock"]},
  "networking": {"encap_type":"geneve", "guest_mtu":1400, "physnet":"provider", "provider_bridge":"br-provider"}
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PVN_NODE_NAME", "pve-a")
	t.Setenv("PVN_GUEST_MTU", "1450")
	t.Setenv("PVN_MANAGER_CA", "/etc/pve/pve-root-ca.pem")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cluster.NodeName != "pve-a" || cfg.Networking.GuestMTU != 1450 {
		t.Fatalf("environment overrides not applied: %+v", cfg)
	}
	if cfg.Agent.ManagerCA != "/etc/pve/pve-root-ca.pem" {
		t.Fatalf("agent manager CA override not applied: %+v", cfg.Agent)
	}
}

func TestValidateAgentManagerTransport(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Unix socket default should validate: %v", err)
	}
	cfg.Agent.ManagerURL = "http://127.0.0.1:8443"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must use a Unix socket") {
		t.Fatalf("plain HTTP manager URL must fail: %v", err)
	}
}

func TestValidateSeparatesManagerSockets(t *testing.T) {
	cfg := validConfig()
	cfg.Manager.BrowserSocket = cfg.Manager.UnixSocket
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), DefaultManagerBrowserSocket) {
		t.Fatalf("shared manager sockets must fail: %v", err)
	}
	cfg = validConfig()
	cfg.Manager.BrowserSocket = "relative.sock"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), DefaultManagerBrowserSocket) {
		t.Fatalf("relative browser socket must fail: %v", err)
	}
	cfg = validConfig()
	cfg.Manager.UnixSocket = "/run/pvn/custom.sock"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), DefaultManagerRuntimeSocket) {
		t.Fatalf("custom runtime socket must fail: %v", err)
	}
	cfg = validConfig()
	cfg.Agent.ManagerURL = "unix:///run/pvn-api/manager.sock"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), DefaultManagerRuntimeSocket) {
		t.Fatalf("browser socket used by agent must fail: %v", err)
	}
}

func TestValidateOVSDBTransportsAndTLS(t *testing.T) {
	cfg := validConfig()
	cfg.OVN.Northbound = []string{"tcp:127.0.0.1:6641"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unix: or ssl:") {
		t.Fatalf("plain OVSDB transport must fail: %v", err)
	}

	cfg = validConfig()
	cfg.OVN.ControlDB = []string{"ssl:192.0.2.10:6645"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ovn.tls_ca") {
		t.Fatalf("SSL endpoints without PKI paths must fail: %v", err)
	}
	cfg.OVN.TLSCA = "/etc/pvn/pki/ca.pem"
	cfg.OVN.TLSCert = "/etc/pvn/pki/node.pem"
	cfg.OVN.TLSKey = "/etc/pvn/pki/node-key.pem"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("secure OVSDB endpoints should validate: %v", err)
	}
}

func TestValidateRejectsUnsafeDefaults(t *testing.T) {
	cfg := Default()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "cluster.id") {
		t.Fatalf("expected missing cluster ID error, got %v", err)
	}
}

func validConfig() Config {
	cfg := Default()
	cfg.Cluster.ID = "lab"
	cfg.OVN.ControlDB = []string{"unix:/run/pvn/control.sock"}
	cfg.OVN.Northbound = []string{"unix:/run/ovn/ovnnb_db.sock"}
	cfg.OVN.Southbound = []string{"unix:/run/ovn/ovnsb_db.sock"}
	return cfg
}
