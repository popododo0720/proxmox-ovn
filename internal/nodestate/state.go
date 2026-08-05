package nodestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const DefaultPath = "/var/lib/pvn/node-state.json"

type State struct {
	Node              string `json:"node"`
	Drained           bool   `json:"drained"`
	ManagedPorts      int    `json:"managed_ports"`
	PendingOperations int    `json:"pending_operations"`
	CentralVoter      bool   `json:"central_voter"`
	Gateway           bool   `json:"gateway"`
}

func Load(path string) (State, error) {
	if path == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("read node state %q: %w", path, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode node state %q: %w", path, err)
	}
	if state.ManagedPorts < 0 || state.PendingOperations < 0 {
		return State{}, errors.New("node state counters cannot be negative")
	}
	return state, nil
}

func Save(path string, state State) error {
	if path == "" {
		path = DefaultPath
	}
	if state.ManagedPorts < 0 || state.PendingOperations < 0 {
		return errors.New("node state counters cannot be negative")
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode node state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create node state directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".node-state-*")
	if err != nil {
		return fmt.Errorf("create temporary node state: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary node state: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary node state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary node state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary node state: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace node state: %w", err)
	}
	return nil
}

func (s State) RemovalBlockers() []string {
	var blockers []string
	if !s.Drained {
		blockers = append(blockers, "node is not marked drained")
	}
	if s.ManagedPorts != 0 {
		blockers = append(blockers, fmt.Sprintf("%d managed port(s) remain", s.ManagedPorts))
	}
	if s.PendingOperations != 0 {
		blockers = append(blockers, fmt.Sprintf("%d operation(s) are pending", s.PendingOperations))
	}
	if s.CentralVoter {
		blockers = append(blockers, "node is a central voter")
	}
	if s.Gateway {
		blockers = append(blockers, "node is a gateway chassis")
	}
	return blockers
}
