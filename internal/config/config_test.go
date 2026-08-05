package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{
  "cluster": {"id":"lab", "reconcile_every":30000000000, "orphan_grace":300000000000, "require_all_nodes":true, "supported_pve_major":9},
  "manager": {"listen_address":":8443", "public_port":8443, "pve_url":"https://127.0.0.1:8006", "unix_socket":"/run/pvn/manager.sock", "web_root":"/usr/share/pvn/web"},
  "agent": {"poll_every":2000000000, "bridge":"br-int", "manager_url":"unix:///run/pvn/manager.sock", "system_id_file":"/etc/openvswitch/system-id.conf"},
  "ovn": {"control_db":["unix:/run/pvn/control.sock"], "northbound":["unix:/run/ovn/ovnnb_db.sock"], "southbound":["unix:/run/ovn/ovnsb_db.sock"]},
  "networking": {"encap_type":"geneve", "guest_mtu":1400, "physnet":"provider", "provider_bridge":"br-provider"},
  "security": {"session_ttl":900000000000}
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PVN_NODE_NAME", "pve-a")
	t.Setenv("PVN_GUEST_MTU", "1450")
	t.Setenv("PVN_TLS_CERT", "/run/credentials/pvn-manager.service/cert")
	t.Setenv("PVN_TLS_KEY", "/run/credentials/pvn-manager.service/key")
	t.Setenv("PVN_MANAGER_CA", "/etc/pve/pve-root-ca.pem")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cluster.NodeName != "pve-a" || cfg.Networking.GuestMTU != 1450 {
		t.Fatalf("environment overrides not applied: %+v", cfg)
	}
	if cfg.Manager.TLSCert != "/run/credentials/pvn-manager.service/cert" || cfg.Manager.TLSKey != "/run/credentials/pvn-manager.service/key" {
		t.Fatalf("credential overrides not applied: %+v", cfg.Manager)
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
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "HTTPS or a Unix socket") {
		t.Fatalf("plain HTTP manager URL must fail: %v", err)
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
