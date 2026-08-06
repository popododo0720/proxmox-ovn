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

	"github.com/popododo0720/proxmox-ovn/internal/diagnostic"
	"github.com/popododo0720/proxmox-ovn/internal/nodestate"
	"github.com/popododo0720/proxmox-ovn/internal/raftstatus"
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

func TestCentralPlanVoterCountPolicy(t *testing.T) {
	nodes := func(count int) string {
		values := make([]string, count)
		for index := range values {
			values[index] = fmt.Sprintf("pve-%d", index+1)
		}
		return strings.Join(values, ",")
	}
	for _, count := range []int{1, 3, 5, 7} {
		if err := run([]string{"central", "plan", "--nodes", nodes(count)}); err != nil {
			t.Fatalf("count %d: %v", count, err)
		}
	}
	for _, count := range []int{0, 2, 4, 6} {
		if err := run([]string{"central", "plan", "--nodes", nodes(count)}); err == nil {
			t.Fatalf("count %d: invalid central plan succeeded", count)
		}
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

func TestDoctorRejectsUnsupportedStandaloneCheck(t *testing.T) {
	err := doctor([]string{"--check", "not-a-doctor-check"})
	if err == nil || !strings.Contains(err.Error(), "unsupported standalone doctor check") {
		t.Fatalf("unsupported standalone check was accepted: %v", err)
	}
}

func TestDoctorStandaloneCheckWritesCompatibleJSON(t *testing.T) {
	var output bytes.Buffer
	err := doctorWith(
		[]string{"--check", diagnostic.CorosyncRuntimeCheckName},
		&output,
		func(context.Context, diagnostic.Runner) diagnostic.Check {
			return diagnostic.Check{
				Name:    diagnostic.CorosyncRuntimeCheckName,
				Status:  diagnostic.Pass,
				Message: "runtime matches",
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var checks []diagnostic.Check
	if err := json.Unmarshal(output.Bytes(), &checks); err != nil {
		t.Fatalf("decode doctor JSON: %v", err)
	}
	if len(checks) != 1 || checks[0].Name != diagnostic.CorosyncRuntimeCheckName || checks[0].Status != diagnostic.Pass {
		t.Fatalf("unexpected standalone doctor JSON: %+v", checks)
	}
}

func TestDoctorStandaloneCheckReturnsFailureAfterJSON(t *testing.T) {
	var output bytes.Buffer
	err := doctorWith(
		[]string{"--check", diagnostic.CorosyncRuntimeCheckName},
		&output,
		func(context.Context, diagnostic.Runner) diagnostic.Check {
			return diagnostic.Check{
				Name:    diagnostic.CorosyncRuntimeCheckName,
				Status:  diagnostic.Fail,
				Message: "stale runtime",
			}
		},
	)
	if err == nil || !strings.Contains(err.Error(), "checks failed") {
		t.Fatalf("failed standalone doctor check was accepted: %v", err)
	}
	var checks []diagnostic.Check
	if jsonErr := json.Unmarshal(output.Bytes(), &checks); jsonErr != nil {
		t.Fatalf("failed doctor did not write compatible JSON: %v", jsonErr)
	}
	if len(checks) != 1 || checks[0].Status != diagnostic.Fail {
		t.Fatalf("unexpected failed standalone doctor JSON: %+v", checks)
	}
}

func TestDoctorRejectsDuplicateStandaloneCheck(t *testing.T) {
	err := doctor([]string{
		"--check", "corosync-runtime-config",
		"--check", "corosync-runtime-config",
	})
	if err == nil || !strings.Contains(err.Error(), "specified only once") {
		t.Fatalf("duplicate standalone check was accepted: %v", err)
	}
}

func TestDoctorRejectsPositionalArguments(t *testing.T) {
	err := doctor([]string{"unexpected"})
	if err == nil || !strings.Contains(err.Error(), "does not accept positional arguments") {
		t.Fatalf("doctor positional argument was accepted: %v", err)
	}
}
