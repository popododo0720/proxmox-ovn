package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/agent"
)

func TestParseConfigUsesEnvironmentAndFlags(t *testing.T) {
	t.Parallel()

	configPath, systemIDPath := writeAgentConfig(t)
	environment := map[string]string{
		"PVN_NODE_NAME":      "pve-env",
		"PVN_NODE_ROLES":     "central, compute, gateway",
		"PVN_WATCH_INTERVAL": "5s",
	}
	getenv := func(key string) string { return environment[key] }
	got, err := parseConfig([]string{"--config", configPath, "--node=pve-flag", "--bridge=br-pvn", "--once"}, getenv, func() (string, error) {
		return "hostname", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.node != "pve-flag" || got.bridge != "br-pvn" || got.managerURL != "unix:///run/pvn/manager.sock" || got.systemIDFile != systemIDPath || got.watchInterval != 5*time.Second || !got.once || !got.nodeRolesExplicit {
		t.Fatalf("config = %#v", got)
	}
	wantRoles := []string{"compute", "gateway", "central"}
	if fmt.Sprint(got.nodeRoles) != fmt.Sprint(wantRoles) {
		t.Fatalf("node roles=%v want=%v", got.nodeRoles, wantRoles)
	}
}

func TestParseNodeRolesRejectsUnknownAndDuplicateRoles(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "compute,compute", "compute,router"} {
		if _, err := parseNodeRoles(value); err == nil {
			t.Fatalf("parseNodeRoles(%q) unexpectedly succeeded", value)
		}
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

func TestHeartbeatWithMembershipRequiresMatchingReporter(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".members")
	if err := os.WriteFile(path, []byte(`{"nodename":"pve-a","cluster":{"quorate":1},"nodelist":{"pve-a":{"online":1},"pve-b":{"online":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	heartbeat, err := heartbeatWithMembership(agent.NodeHeartbeat{Name: "pve-a", ChassisID: "chassis-a"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.Quorate == nil || !*heartbeat.Quorate || fmt.Sprint(heartbeat.OnlineNodes) != "[pve-a pve-b]" {
		t.Fatalf("heartbeat=%#v", heartbeat)
	}
	if _, err := heartbeatWithMembership(agent.NodeHeartbeat{Name: "pve-b", ChassisID: "chassis-b"}, path); err == nil {
		t.Fatal("mismatched membership reporter unexpectedly accepted")
	}
}

func TestHeartbeatWithStandaloneMembership(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".members")
	if err := os.WriteFile(path, []byte(`{"nodename":"pve-solo","version":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	heartbeat, err := heartbeatWithMembership(agent.NodeHeartbeat{Name: "pve-solo", ChassisID: "chassis-solo"}, path)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.Quorate == nil || !*heartbeat.Quorate || fmt.Sprint(heartbeat.OnlineNodes) != "[pve-solo]" {
		t.Fatalf("standalone heartbeat=%#v", heartbeat)
	}
	if _, err := heartbeatWithMembership(agent.NodeHeartbeat{Name: "other", ChassisID: "chassis-other"}, path); err == nil {
		t.Fatal("mismatched standalone reporter unexpectedly accepted")
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
  "manager": {"unix_socket":"/run/pvn/manager.sock","browser_socket":"/run/pvn-api/manager.sock"},
  "agent": {"poll_every":2000000000,"bridge":"br-json","manager_url":"unix:///run/pvn/manager.sock","system_id_file":%q},
  "ovn": {"control_db":["unix:/run/pvn/control.sock"],"northbound":["unix:/run/ovn/ovnnb_db.sock"],"southbound":["unix:/run/ovn/ovnsb_db.sock"]},
  "networking": {"encap_type":"geneve","guest_mtu":1400,"physnet":"provider","provider_bridge":"br-provider"}
}`, systemIDPath)
	if err := os.WriteFile(configPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, systemIDPath
}
