package nodestate

import (
	"path/filepath"
	"testing"
)

func TestSaveLoadAndRemovalBlockers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := State{Node: "pve-a", Drained: true}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if blockers := got.RemovalBlockers(); len(blockers) != 0 {
		t.Fatalf("unexpected blockers: %v", blockers)
	}
}

func TestRemovalBlockersAreConservative(t *testing.T) {
	state := State{ManagedPorts: 2, PendingOperations: 1, CentralVoter: true, Gateway: true}
	if blockers := state.RemovalBlockers(); len(blockers) != 5 {
		t.Fatalf("got blockers %v", blockers)
	}
}
