package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pvnstack/proxmox-ovn/internal/nodestate"
)

func TestCentralPlan(t *testing.T) {
	if err := run([]string{"central", "plan", "--nodes", "pve-a,pve-b,pve-c"}); err != nil {
		t.Fatal(err)
	}
}

func TestNodeCanRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-state.json")
	if err := nodestate.Save(path, nodestate.State{Node: "pve-a", Drained: true}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"node", "can-remove", "--state", path}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configured, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"node", "can-remove", "--state", path, "--config", configured}); err == nil {
		t.Fatal("missing state must fail closed")
	}
	if err := run([]string{"node", "can-remove", "--state", path, "--config", filepath.Join(t.TempDir(), "missing-config.json")}); err != nil {
		t.Fatalf("unused package should be removable: %v", err)
	}
}
