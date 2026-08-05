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
	if err := run([]string{"node", "can-remove", "--state", path}); err == nil {
		t.Fatal("missing state must fail closed")
	}
}
