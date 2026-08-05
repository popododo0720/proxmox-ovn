package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pvnstack/proxmox-ovn/internal/nodestate"
	"github.com/pvnstack/proxmox-ovn/internal/raftstatus"
)

type statusRunner struct {
	calls [][]string
}

func (r *statusRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	database := args[len(args)-1]
	return []byte(fmt.Sprintf(`aaaa
Name: %s
Cluster ID: 1111 (11111111-1111-4111-8111-111111111111)
Server ID: aaaa (aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa)
Address: ssl:192.0.2.10:6646
Status: cluster member
Role: leader
Term: 1
Leader: self
Vote: self
Connections: ->bbbb ->cccc
Servers:
    aaaa (aaaa at ssl:192.0.2.10:6646) (self)
    bbbb (bbbb at ssl:192.0.2.11:6646)
    cccc (cccc at ssl:192.0.2.12:6646)
`, database)), nil
}

func TestCentralPlan(t *testing.T) {
	if err := run([]string{"central", "plan", "--nodes", "pve-a,pve-b,pve-c"}); err != nil {
		t.Fatal(err)
	}
}

func TestCentralStatusProducesHealthyJSONAndUsesSocketOverrides(t *testing.T) {
	runner := &statusRunner{}
	var output bytes.Buffer
	err := centralStatusWith(runner, &output, []string{
		"--pvn-control-ctl", "/custom/control.ctl",
		"--ovn-nb-ctl", "/custom/nb.ctl",
		"--ovn-sb-ctl", "/custom/sb.ctl",
		"--timeout", "2s",
	})
	if err != nil {
		t.Fatal(err)
	}
	var report raftstatus.Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, output.String())
	}
	if !report.Healthy || len(report.Databases) != 3 {
		t.Fatalf("unexpected report: %+v", report)
	}
	for index, socket := range []string{"/custom/control.ctl", "/custom/nb.ctl", "/custom/sb.ctl"} {
		if got := runner.calls[index][1]; got != "--target="+socket {
			t.Fatalf("call %d used %q", index, got)
		}
	}
}

func TestCentralStatusReturnsErrorAfterWritingUnhealthyJSON(t *testing.T) {
	runner := &statusRunner{}
	var output bytes.Buffer
	err := centralStatusWith(runner, &output, []string{"--pvn-control-ctl", "relative.ctl"})
	if err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("expected unhealthy error, got %v", err)
	}
	var report raftstatus.Report
	if jsonErr := json.Unmarshal(output.Bytes(), &report); jsonErr != nil {
		t.Fatalf("decode status JSON: %v", jsonErr)
	}
	if report.Healthy || report.Databases[0].Error == "" {
		t.Fatalf("expected structured validation failure: %+v", report)
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
