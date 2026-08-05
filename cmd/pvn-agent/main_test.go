package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/agent"
)

func TestParseConfigUsesEnvironmentAndFlags(t *testing.T) {
	t.Parallel()

	configPath, systemIDPath := writeAgentConfig(t)
	environment := map[string]string{
		"PVN_NODE_NAME":      "pve-env",
		"PVN_MANAGER_URL":    "unix:///run/pvn/from-env.sock",
		"PVN_MANAGER_CA":     "/etc/pve/pve-root-ca.pem",
		"PVN_WATCH_INTERVAL": "5s",
	}
	getenv := func(key string) string { return environment[key] }
	got, err := parseConfig([]string{"--config", configPath, "--node=pve-flag", "--bridge=br-pvn", "--once"}, getenv, func() (string, error) {
		return "hostname", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.node != "pve-flag" || got.bridge != "br-pvn" || got.managerURL != "unix:///run/pvn/from-env.sock" || got.managerCA != "/etc/pve/pve-root-ca.pem" || got.systemIDFile != systemIDPath || got.watchInterval != 5*time.Second || !got.once {
		t.Fatalf("config = %#v", got)
	}
}

func TestReadSystemIDSupportsOVSFileFormats(t *testing.T) {
	t.Parallel()

	for _, contents := range []string{"chassis-uuid\n", "system-id=\"chassis-uuid\"\n"} {
		path := filepath.Join(t.TempDir(), "system-id.conf")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readSystemID(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != "chassis-uuid" {
			t.Fatalf("system ID = %q", got)
		}
	}
}

func TestHealthHandlerReflectsWatcherReadiness(t *testing.T) {
	t.Parallel()

	status := agent.WatcherStatus{}
	handler := newHealthHandler(func() agent.WatcherStatus { return status }, time.Second)

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unready status = %d", response.Code)
	}

	status.LastSuccess = time.Now()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d", response.Code)
	}
}

func writeAgentConfig(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	systemIDPath := filepath.Join(directory, "system-id.conf")
	if err := os.WriteFile(systemIDPath, []byte("chassis-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.json")
	payload := fmt.Sprintf(`{
  "cluster": {"id":"lab","reconcile_every":30000000000,"orphan_grace":300000000000,"require_all_nodes":true,"supported_pve_major":9},
  "manager": {"listen_address":":8443","public_port":8443,"pve_url":"https://127.0.0.1:8006","unix_socket":"/run/pvn/manager.sock","web_root":"/usr/share/pvn/web"},
  "agent": {"poll_every":2000000000,"bridge":"br-json","manager_url":"unix:///run/pvn/from-json.sock","manager_ca":"/json/ca.pem","system_id_file":%q},
  "networking": {"encap_type":"geneve","guest_mtu":1400,"physnet":"provider","provider_bridge":"br-provider"},
  "security": {"session_ttl":900000000000}
}`, systemIDPath)
	if err := os.WriteFile(configPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, systemIDPath
}
