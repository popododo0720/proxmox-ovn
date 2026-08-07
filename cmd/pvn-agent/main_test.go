package main

import (
	"context"
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

	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	status := agent.WatcherStatus{}
	handler := newHealthHandlerWithClock(func() agent.WatcherStatus { return status }, time.Second, func() time.Time { return now })

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unready status = %d", response.Code)
	}

	status.LastSuccess = now
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d", response.Code)
	}

	status.LastError = "OVS unavailable"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("error status = %d", response.Code)
	}

	status.LastError = ""
	status.Report.Errors = 1
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("reported-error status = %d", response.Code)
	}

	status.Report.Errors = 0
	status.LastSuccess = now.Add(-30*time.Second - time.Nanosecond)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale status = %d", response.Code)
	}
}

func TestWatcherReadyRequiresFreshErrorFreeScan(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		status   agent.WatcherStatus
		interval time.Duration
		ready    bool
	}{
		{name: "never scanned", status: agent.WatcherStatus{}, interval: time.Second},
		{name: "healthy", status: agent.WatcherStatus{LastSuccess: now}, interval: time.Second, ready: true},
		{name: "last error", status: agent.WatcherStatus{LastSuccess: now, LastError: "manager unavailable"}, interval: time.Second},
		{name: "report errors", status: agent.WatcherStatus{LastSuccess: now, Report: agent.ScanReport{Errors: 1}}, interval: time.Second},
		{name: "minimum freshness boundary", status: agent.WatcherStatus{LastSuccess: now.Add(-30 * time.Second)}, interval: time.Second, ready: true},
		{name: "minimum freshness stale", status: agent.WatcherStatus{LastSuccess: now.Add(-30*time.Second - time.Nanosecond)}, interval: time.Second},
		{name: "scaled freshness boundary", status: agent.WatcherStatus{LastSuccess: now.Add(-60 * time.Second)}, interval: 20 * time.Second, ready: true},
		{name: "scaled freshness stale", status: agent.WatcherStatus{LastSuccess: now.Add(-60*time.Second - time.Nanosecond)}, interval: 20 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready, reason := watcherReady(test.status, test.interval, now)
			if ready != test.ready {
				t.Fatalf("ready=%t reason=%q want=%t", ready, reason, test.ready)
			}
			if ready && reason != "" {
				t.Fatalf("ready watcher returned reason %q", reason)
			}
			if !ready && reason == "" {
				t.Fatal("unready watcher returned no reason")
			}
		})
	}
}

type recordingHeartbeatClient struct {
	heartbeats []agent.NodeHeartbeat
}

func (client *recordingHeartbeatClient) HeartbeatNode(_ context.Context, heartbeat agent.NodeHeartbeat) error {
	client.heartbeats = append(client.heartbeats, heartbeat)
	return nil
}

func TestHeartbeatReadinessSkipsMembershipUntilHealthyAndRecovers(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	client := &recordingHeartbeatClient{}
	heartbeat := agent.NodeHeartbeat{Name: "pve-a", ChassisID: "chassis-a"}
	unhealthy := agent.WatcherStatus{LastError: "OVS unavailable", Report: agent.ScanReport{Errors: 1}}

	sent, reason, err := heartbeatNodeIfReady(
		context.Background(), client, heartbeat, filepath.Join(t.TempDir(), "missing-membership"), true,
		unhealthy, time.Second, now,
	)
	if err != nil || sent || reason == "" || len(client.heartbeats) != 0 {
		t.Fatalf("unhealthy heartbeat sent=%t reason=%q err=%v calls=%d", sent, reason, err, len(client.heartbeats))
	}

	membershipPath := filepath.Join(t.TempDir(), ".members")
	if err := os.WriteFile(membershipPath, []byte(`{"nodename":"pve-a","cluster":{"quorate":1},"nodelist":{"pve-a":{"online":1},"pve-b":{"online":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	healthy := agent.WatcherStatus{LastSuccess: now}
	sent, reason, err = heartbeatNodeIfReady(
		context.Background(), client, heartbeat, membershipPath, true,
		healthy, time.Second, now,
	)
	if err != nil || !sent || reason != "" || len(client.heartbeats) != 1 {
		t.Fatalf("healthy heartbeat sent=%t reason=%q err=%v calls=%d", sent, reason, err, len(client.heartbeats))
	}
	got := client.heartbeats[0]
	if got.Quorate == nil || !*got.Quorate || fmt.Sprint(got.OnlineNodes) != "[pve-a pve-b]" {
		t.Fatalf("membership heartbeat=%#v", got)
	}

	failed := agent.WatcherStatus{LastSuccess: now, LastError: "resolve failed", Report: agent.ScanReport{Errors: 1}}
	sent, _, err = heartbeatNodeIfReady(
		context.Background(), client, heartbeat, membershipPath, true,
		failed, time.Second, now.Add(time.Second),
	)
	if err != nil || sent || len(client.heartbeats) != 1 {
		t.Fatalf("failed watcher sent=%t err=%v calls=%d", sent, err, len(client.heartbeats))
	}

	recoveredAt := now.Add(2 * time.Second)
	recovered := agent.WatcherStatus{LastSuccess: recoveredAt}
	sent, _, err = heartbeatNodeIfReady(
		context.Background(), client, heartbeat, membershipPath, true,
		recovered, time.Second, recoveredAt,
	)
	if err != nil || !sent || len(client.heartbeats) != 2 {
		t.Fatalf("recovered watcher sent=%t err=%v calls=%d", sent, err, len(client.heartbeats))
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
