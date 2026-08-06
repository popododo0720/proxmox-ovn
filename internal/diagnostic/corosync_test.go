package diagnostic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCorosyncConfig = `logging {
	debug: off
}

nodelist {
	node {
		name: prox1
		nodeid: 2
		quorum_votes: 1
		ring0_addr: 192.168.0.80
	}
	node {
		name: prox2
		nodeid: 1
		quorum_votes: 1
		ring0_addr: 192.168.0.126
	}
	node {
		name: prox3
		nodeid: 3
		quorum_votes: 1
		ring0_addr: 192.168.0.78
	}
}

totem {
	cluster_name: pve
	config_version: 4
	transport: knet
}
`

const testCorosyncCmap = `nodelist.local_node_pos (u32) = 0
nodelist.node.0.name (str) = prox1
nodelist.node.0.nodeid (u32) = 2
nodelist.node.0.quorum_votes (u32) = 1
nodelist.node.0.ring0_addr (str) = 192.168.0.80
nodelist.node.1.name (str) = prox2
nodelist.node.1.nodeid (u32) = 1
nodelist.node.1.quorum_votes (u32) = 1
nodelist.node.1.ring0_addr (str) = 192.168.0.126
nodelist.node.2.name (str) = prox3
nodelist.node.2.nodeid (u32) = 3
nodelist.node.2.quorum_votes (u32) = 1
nodelist.node.2.ring0_addr (str) = 192.168.0.78
runtime.members.1.config_version (u64) = 4
runtime.members.1.ip (str) = r(0) ip(192.168.0.126)
runtime.members.1.join_count (u32) = 2
runtime.members.1.status (str) = joined
runtime.members.2.config_version (u64) = 4
runtime.members.2.ip (str) = r(0) ip(192.168.0.80)
runtime.members.2.join_count (u32) = 1
runtime.members.2.status (str) = joined
runtime.members.3.config_version (u64) = 4
runtime.members.3.ip (str) = r(0) ip(192.168.0.78)
runtime.members.3.join_count (u32) = 1
runtime.members.3.status (str) = joined
totem.config_version (u64) = 4
totem.interface.0.bindnetaddr (str) = 192.168.0.80
`

func TestCorosyncRuntimeCheckMatchesPersistedState(t *testing.T) {
	path := writeCorosyncFixture(t, testCorosyncConfig)
	check := corosyncRuntimeCheck(context.Background(), fakeRunner{outputs: map[string]string{
		"corosync-cmapctl": testCorosyncCmap,
	}}, path)
	if check.Status != Pass || !strings.Contains(check.Message, "3 configured nodes") {
		t.Fatalf("matching Corosync state did not pass: %+v", check)
	}
}

func TestCorosyncRuntimeCheckRejectsStaleRuntime(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		runtime string
		want    string
	}{
		{
			name:    "config version",
			config:  strings.Replace(testCorosyncConfig, "config_version: 4", "config_version: 5", 1),
			runtime: testCorosyncCmap,
			want:    "persisted config_version 5 does not match live",
		},
		{
			name:   "configured ring address",
			config: testCorosyncConfig,
			runtime: strings.Replace(
				testCorosyncCmap,
				"nodelist.node.1.ring0_addr (str) = 192.168.0.126",
				"nodelist.node.1.ring0_addr (str) = 10.20.0.126",
				1,
			),
			want: "prox2(id=1) ring0 persisted address",
		},
		{
			name:   "joined link address",
			config: testCorosyncConfig,
			runtime: strings.Replace(
				testCorosyncCmap,
				"runtime.members.1.ip (str) = r(0) ip(192.168.0.126)",
				"runtime.members.1.ip (str) = r(0) ip(10.20.0.126)",
				1,
			),
			want: "prox2(id=1) ring0 persisted address",
		},
		{
			name:   "missing member",
			config: testCorosyncConfig,
			runtime: strings.Replace(
				testCorosyncCmap,
				"runtime.members.3.config_version (u64) = 4\nruntime.members.3.ip (str) = r(0) ip(192.168.0.78)\nruntime.members.3.join_count (u32) = 1\nruntime.members.3.status (str) = joined\n",
				"",
				1,
			),
			want: "runtime membership",
		},
		{
			name:    "member not joined",
			config:  testCorosyncConfig,
			runtime: strings.Replace(testCorosyncCmap, "runtime.members.3.status (str) = joined", "runtime.members.3.status (str) = left", 1),
			want:    "prox3(id=3) status is \"left\"",
		},
		{
			name:    "member config version",
			config:  testCorosyncConfig,
			runtime: strings.Replace(testCorosyncCmap, "runtime.members.3.config_version (u64) = 4", "runtime.members.3.config_version (u64) = 3", 1),
			want:    "prox3(id=3) config_version 3",
		},
		{
			name:    "local bind address",
			config:  testCorosyncConfig,
			runtime: strings.Replace(testCorosyncCmap, "totem.interface.0.bindnetaddr (str) = 192.168.0.80", "totem.interface.0.bindnetaddr (str) = 10.20.0.80", 1),
			want:    "prox1(id=2) ring0 persisted address",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeCorosyncFixture(t, test.config)
			check := corosyncRuntimeCheck(context.Background(), fakeRunner{outputs: map[string]string{
				"corosync-cmapctl": test.runtime,
			}}, path)
			if check.Status != Fail || !strings.Contains(check.Message, test.want) {
				t.Fatalf("stale runtime was not rejected with %q: %+v", test.want, check)
			}
		})
	}
}

func TestCorosyncRuntimeCheckAcceptsConfiguredHostnames(t *testing.T) {
	config := strings.NewReplacer(
		"ring0_addr: 192.168.0.80", "ring0_addr: prox1",
		"ring0_addr: 192.168.0.126", "ring0_addr: prox2",
		"ring0_addr: 192.168.0.78", "ring0_addr: prox3",
	).Replace(testCorosyncConfig)
	runtime := strings.NewReplacer(
		"nodelist.node.0.ring0_addr (str) = 192.168.0.80", "nodelist.node.0.ring0_addr (str) = prox1",
		"nodelist.node.1.ring0_addr (str) = 192.168.0.126", "nodelist.node.1.ring0_addr (str) = prox2",
		"nodelist.node.2.ring0_addr (str) = 192.168.0.78", "nodelist.node.2.ring0_addr (str) = prox3",
	).Replace(testCorosyncCmap)

	check := corosyncRuntimeCheck(context.Background(), fakeRunner{outputs: map[string]string{
		"corosync-cmapctl": runtime,
	}}, writeCorosyncFixture(t, config))
	if check.Status != Pass {
		t.Fatalf("matching hostname-based Corosync state did not pass: %+v", check)
	}
}

func TestCorosyncRuntimeCheckAllowsStandaloneNode(t *testing.T) {
	check := corosyncRuntimeCheck(context.Background(), fakeRunner{}, filepath.Join(t.TempDir(), "missing-corosync.conf"))
	if check.Status != Pass || !strings.Contains(check.Message, "standalone") {
		t.Fatalf("standalone node did not pass: %+v", check)
	}
}

func TestParseCorosyncConfigRejectsDuplicateIdentity(t *testing.T) {
	duplicate := strings.Replace(testCorosyncConfig, "nodeid: 3", "nodeid: 1", 1)
	if _, err := parseCorosyncConfig(duplicate); err == nil || !strings.Contains(err.Error(), "share nodeid") {
		t.Fatalf("duplicate nodeid was not rejected: %v", err)
	}
}

func writeCorosyncFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corosync.conf")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}
