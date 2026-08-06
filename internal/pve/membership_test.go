package pve

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadClusterMembership(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".members")
	payload := []byte(`{
  "nodename": "pve-b",
  "cluster": {"name": "lab", "quorate": 1},
  "nodelist": {
    "pve-c": {"id": 3, "online": 1, "ip": "192.0.2.3"},
    "pve-a": {"id": 1, "online": 1, "ip": "192.0.2.1"},
    "pve-b": {"id": 2, "online": 0, "ip": "192.0.2.2"}
  }
}`)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	membership, err := ReadClusterMembership(path)
	if err != nil {
		t.Fatal(err)
	}
	if membership.Reporter != "pve-b" || !membership.Quorate || !reflect.DeepEqual(membership.OnlineNodes, []string{"pve-a", "pve-c"}) {
		t.Fatalf("membership=%#v", membership)
	}
}

func TestReadStandaloneMembership(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".members")
	if err := os.WriteFile(path, []byte(`{"nodename":"pve-solo","version":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	membership, err := ReadClusterMembership(path)
	if err != nil {
		t.Fatal(err)
	}
	if membership.Reporter != "pve-solo" || !membership.Quorate || !reflect.DeepEqual(membership.OnlineNodes, []string{"pve-solo"}) {
		t.Fatalf("standalone membership=%#v", membership)
	}
}

func TestReadClusterMembershipFailsClosed(t *testing.T) {
	for name, payload := range map[string]string{
		"missing reporter":            `{"cluster":{"quorate":1},"nodelist":{"pve-a":{"online":1}}}`,
		"no online nodes":             `{"nodename":"pve-a","cluster":{"quorate":1},"nodelist":{"pve-a":{"online":0}}}`,
		"invalid JSON":                `{`,
		"standalone missing version":  `{"nodename":"pve-a"}`,
		"standalone extra state":      `{"nodename":"pve-a","version":1,"extra":true}`,
		"standalone negative version": `{"nodename":"pve-a","version":-1}`,
		"standalone string version":   `{"nodename":"pve-a","version":"1"}`,
		"standalone cluster null":     `{"nodename":"pve-a","version":1,"cluster":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".members")
			if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadClusterMembership(path); err == nil {
				t.Fatal("expected membership read to fail")
			}
		})
	}
}
