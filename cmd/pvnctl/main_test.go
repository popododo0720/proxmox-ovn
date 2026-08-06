package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/config"
	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/diagnostic"
	"github.com/popododo0720/proxmox-ovn/internal/model"
	"github.com/popododo0720/proxmox-ovn/internal/nodestate"
	"github.com/popododo0720/proxmox-ovn/internal/raftstatus"
)

type recoveryReconcilerStub struct {
	calls       int
	err         error
	deadline    time.Time
	hasDeadline bool
	events      *[]string
}

type recoveryAuditorStub struct {
	calls       int
	err         error
	deadline    time.Time
	hasDeadline bool
	events      *[]string
}

type recoverySnapshotterStub struct {
	snapshots []controlstore.ResourceSnapshot
	errors    []error
	calls     int
}

func (stub *recoverySnapshotterStub) Snapshot(context.Context, []model.Kind, controlstore.ListOptions) (controlstore.ResourceSnapshot, error) {
	index := stub.calls
	stub.calls++
	if index < len(stub.errors) && stub.errors[index] != nil {
		return nil, stub.errors[index]
	}
	if index >= len(stub.snapshots) {
		return nil, errors.New("unexpected snapshot call")
	}
	return stub.snapshots[index], nil
}

func (stub *recoveryReconcilerStub) ReconcileAll(ctx context.Context) error {
	stub.calls++
	stub.deadline, stub.hasDeadline = ctx.Deadline()
	if stub.events != nil {
		*stub.events = append(*stub.events, "reconcile")
	}
	return stub.err
}

func (stub *recoveryAuditorStub) AuditManagedGraph(ctx context.Context) error {
	stub.calls++
	stub.deadline, stub.hasDeadline = ctx.Deadline()
	if stub.events != nil {
		*stub.events = append(*stub.events, "audit")
	}
	return stub.err
}

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

func TestRecoveryReconcileOVNRequiresExplicitRootGates(t *testing.T) {
	base := recoveryDependencies{
		getEUID: func() int { return 0 },
		loadConfig: func(string) (config.Config, error) {
			return config.Config{Cluster: config.ClusterConfig{ID: "lab-cluster"}}, nil
		},
		open: func(context.Context, config.Config) (recoveryRuntime, error) {
			t.Fatal("unsafe input reached recovery runtime")
			return recoveryRuntime{}, nil
		},
		output: &bytes.Buffer{},
	}
	tests := []struct {
		name string
		args []string
		edit func(*recoveryDependencies)
		want string
	}{
		{name: "missing subcommand", want: "usage"},
		{name: "unknown subcommand", args: []string{"wrong"}, want: "unknown recovery command"},
		{name: "not root", args: []string{"reconcile-ovn", "--apply", "--confirm", "lab-cluster"}, edit: func(deps *recoveryDependencies) { deps.getEUID = func() int { return 1000 } }, want: "must run as root"},
		{name: "dry run", args: []string{"reconcile-ovn", "--confirm", "lab-cluster"}, want: "pass --apply"},
		{name: "wrong confirmation", args: []string{"reconcile-ovn", "--apply", "--confirm", "other"}, want: "exactly match"},
		{name: "short timeout", args: []string{"reconcile-ovn", "--apply", "--confirm", "lab-cluster", "--timeout", "59s"}, want: "between 1m and 30m"},
		{name: "long timeout", args: []string{"reconcile-ovn", "--apply", "--confirm", "lab-cluster", "--timeout", "31m"}, want: "between 1m and 30m"},
		{name: "positional", args: []string{"reconcile-ovn", "--apply", "--confirm", "lab-cluster", "extra"}, want: "does not accept positional"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := base
			if test.edit != nil {
				test.edit(&deps)
			}
			err := recoveryCommandWith(test.args, deps)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRecoveryReconcileOVNRunsOneForcedPassAndCloses(t *testing.T) {
	events := make([]string, 0, 2)
	reconciler := &recoveryReconcilerStub{events: &events}
	auditor := &recoveryAuditorStub{events: &events}
	closed := 0
	var output bytes.Buffer
	err := recoveryCommandWith(
		[]string{"reconcile-ovn", "--config", "/test/config.json", "--apply", "--confirm", "lab-cluster", "--timeout", "2m"},
		recoveryDependencies{
			getEUID: func() int { return 0 },
			loadConfig: func(path string) (config.Config, error) {
				if path != "/test/config.json" {
					t.Fatalf("config path=%q", path)
				}
				return config.Config{Cluster: config.ClusterConfig{ID: "lab-cluster", OrphanGrace: time.Minute}}, nil
			},
			open: func(_ context.Context, cfg config.Config) (recoveryRuntime, error) {
				if cfg.Cluster.ID != "lab-cluster" {
					t.Fatalf("config=%+v", cfg)
				}
				return recoveryRuntime{reconciler: reconciler, auditor: auditor, close: func() { closed++ }}, nil
			},
			output: &output,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconciler.calls != 1 || auditor.calls != 1 || closed != 1 {
		t.Fatalf("reconcile calls=%d audit calls=%d close calls=%d", reconciler.calls, auditor.calls, closed)
	}
	if strings.Join(events, ",") != "reconcile,audit" {
		t.Fatalf("recovery call order=%v", events)
	}
	remaining := time.Until(reconciler.deadline)
	if !reconciler.hasDeadline || remaining < 90*time.Second || remaining > 2*time.Minute {
		t.Fatalf("reconcile deadline present=%v remaining=%s", reconciler.hasDeadline, remaining)
	}
	if !auditor.hasDeadline || !auditor.deadline.Equal(reconciler.deadline) {
		t.Fatalf("audit deadline present=%v deadline=%s, reconcile deadline=%s", auditor.hasDeadline, auditor.deadline, reconciler.deadline)
	}
	var report map[string]string
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if report["action"] != "reconcile-ovn" || report["cluster"] != "lab-cluster" || report["status"] != "succeeded" {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestRecoveryReconcileOVNPropagatesOpenAndReconcileFailures(t *testing.T) {
	wantOpen := errors.New("database unavailable")
	deps := recoveryDependencies{
		getEUID: func() int { return 0 },
		loadConfig: func(string) (config.Config, error) {
			return config.Config{Cluster: config.ClusterConfig{ID: "lab-cluster"}}, nil
		},
		open: func(context.Context, config.Config) (recoveryRuntime, error) {
			return recoveryRuntime{}, wantOpen
		},
		output: &bytes.Buffer{},
	}
	args := []string{"reconcile-ovn", "--apply", "--confirm", "lab-cluster"}
	if err := recoveryCommandWith(args, deps); !errors.Is(err, wantOpen) {
		t.Fatalf("open error=%v", err)
	}

	wantReconcile := errors.New("duplicate restored row")
	closed := 0
	deps.open = func(context.Context, config.Config) (recoveryRuntime, error) {
		return recoveryRuntime{
			reconciler: &recoveryReconcilerStub{err: wantReconcile},
			auditor:    &recoveryAuditorStub{},
			close:      func() { closed++ },
		}, nil
	}
	if err := recoveryCommandWith(args, deps); !errors.Is(err, wantReconcile) {
		t.Fatalf("reconcile error=%v", err)
	}
	if closed != 1 {
		t.Fatalf("close calls=%d", closed)
	}
}

func TestRecoveryReconcileOVNFailsClosedWhenManagedGraphAuditFails(t *testing.T) {
	wantAudit := errors.New("orphaned managed NAT row")
	reconciler := &recoveryReconcilerStub{}
	auditor := &recoveryAuditorStub{err: wantAudit}
	closed := 0
	var output bytes.Buffer
	err := recoveryCommandWith(
		[]string{"reconcile-ovn", "--apply", "--confirm", "lab-cluster"},
		recoveryDependencies{
			getEUID: func() int { return 0 },
			loadConfig: func(string) (config.Config, error) {
				return config.Config{Cluster: config.ClusterConfig{ID: "lab-cluster"}}, nil
			},
			open: func(context.Context, config.Config) (recoveryRuntime, error) {
				return recoveryRuntime{
					reconciler: reconciler, auditor: auditor, close: func() { closed++ },
				}, nil
			},
			output: &output,
		},
	)
	if !errors.Is(err, wantAudit) || !strings.Contains(err.Error(), "audit reconciled OVN managed graph") {
		t.Fatalf("audit error=%v", err)
	}
	if reconciler.calls != 1 || auditor.calls != 1 || closed != 1 {
		t.Fatalf("reconcile calls=%d audit calls=%d close calls=%d", reconciler.calls, auditor.calls, closed)
	}
	if output.Len() != 0 {
		t.Fatalf("failed audit emitted success output: %q", output.String())
	}
}

func TestRecoveryReconcileOVNRequiresManagedGraphAuditorBeforeWriting(t *testing.T) {
	reconciler := &recoveryReconcilerStub{}
	var output bytes.Buffer
	err := recoveryCommandWith(
		[]string{"reconcile-ovn", "--apply", "--confirm", "lab-cluster"},
		recoveryDependencies{
			getEUID: func() int { return 0 },
			loadConfig: func(string) (config.Config, error) {
				return config.Config{Cluster: config.ClusterConfig{ID: "lab-cluster"}}, nil
			},
			open: func(context.Context, config.Config) (recoveryRuntime, error) {
				return recoveryRuntime{reconciler: reconciler}, nil
			},
			output: &output,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "auditor is unavailable") {
		t.Fatalf("missing auditor error=%v", err)
	}
	if reconciler.calls != 0 || output.Len() != 0 {
		t.Fatalf("missing auditor ran reconcile=%d or wrote output=%q", reconciler.calls, output.String())
	}
}

func TestVerifiedRecoveryReconcilerRequiresACompletedOperationFromThisPass(t *testing.T) {
	completed := time.Now().UTC()
	project := &model.Project{Metadata: model.Metadata{
		ID: "project-a", Revision: 3, AppliedRevision: 3, State: model.ResourceReady,
	}}
	operation := &model.Operation{
		Metadata: model.Metadata{ID: "operation-a", Revision: 8},
		Action:   "reconcile", TargetKind: model.KindProject, TargetID: project.ID, TargetRevision: project.Revision,
		OperationStatus: model.OperationSucceeded, CompletedAt: &completed,
	}
	store := &recoverySnapshotterStub{snapshots: []controlstore.ResourceSnapshot{
		{model.KindOperation: {operation}},
		{model.KindProject: {project}, model.KindOperation: {operation}},
	}}
	forced := &recoveryReconcilerStub{}
	err := (verifiedRecoveryReconciler{reconciler: forced, store: store}).ReconcileAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not complete in this pass") {
		t.Fatalf("stale operation verification error=%v", err)
	}
	if forced.calls != 1 || store.calls != 2 {
		t.Fatalf("reconcile calls=%d snapshot calls=%d", forced.calls, store.calls)
	}
}

func TestVerifiedRecoveryReconcilerAcceptsNewlyCompletedDesiredRevision(t *testing.T) {
	completed := time.Now().UTC()
	project := &model.Project{Metadata: model.Metadata{
		ID: "project-a", Revision: 3, AppliedRevision: 3, State: model.ResourceReady,
	}}
	before := &model.Operation{
		Metadata: model.Metadata{ID: "operation-a", Revision: 8},
		Action:   "reconcile", TargetKind: model.KindProject, TargetID: project.ID, TargetRevision: project.Revision,
		OperationStatus: model.OperationRunning,
	}
	after := &model.Operation{
		Metadata: model.Metadata{ID: before.ID, Revision: 10},
		Action:   "reconcile", TargetKind: model.KindProject, TargetID: project.ID, TargetRevision: project.Revision,
		OperationStatus: model.OperationSucceeded, CompletedAt: &completed,
	}
	store := &recoverySnapshotterStub{snapshots: []controlstore.ResourceSnapshot{
		{model.KindOperation: {before}},
		{model.KindProject: {project}, model.KindOperation: {after}},
	}}
	forced := &recoveryReconcilerStub{}
	if err := (verifiedRecoveryReconciler{reconciler: forced, store: store}).ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if forced.calls != 1 || store.calls != 2 {
		t.Fatalf("reconcile calls=%d snapshot calls=%d", forced.calls, store.calls)
	}
}

func TestVerifiedRecoveryReconcilerFailsClosedOnIncompleteControlState(t *testing.T) {
	project := &model.Project{Metadata: model.Metadata{
		ID: "project-a", Revision: 3, AppliedRevision: 2, State: model.ResourcePending,
	}}
	store := &recoverySnapshotterStub{snapshots: []controlstore.ResourceSnapshot{
		{model.KindOperation: nil},
		{model.KindProject: {project}, model.KindOperation: nil},
	}}
	err := (verifiedRecoveryReconciler{reconciler: &recoveryReconcilerStub{}, store: store}).ReconcileAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is pending at applied revision 2") {
		t.Fatalf("incomplete control state error=%v", err)
	}
}

func TestVerifiedRecoveryReconcilerPropagatesSnapshotAndPassFailures(t *testing.T) {
	wantSnapshot := errors.New("control snapshot unavailable")
	store := &recoverySnapshotterStub{errors: []error{wantSnapshot}}
	forced := &recoveryReconcilerStub{}
	if err := (verifiedRecoveryReconciler{reconciler: forced, store: store}).ReconcileAll(context.Background()); !errors.Is(err, wantSnapshot) {
		t.Fatalf("pre-snapshot error=%v", err)
	}
	if forced.calls != 0 {
		t.Fatalf("forced reconcile ran after failed pre-snapshot: %d", forced.calls)
	}

	wantPass := errors.New("northbound write failed")
	store = &recoverySnapshotterStub{snapshots: []controlstore.ResourceSnapshot{{model.KindOperation: nil}}}
	forced = &recoveryReconcilerStub{err: wantPass}
	if err := (verifiedRecoveryReconciler{reconciler: forced, store: store}).ReconcileAll(context.Background()); !errors.Is(err, wantPass) {
		t.Fatalf("reconcile pass error=%v", err)
	}
	if store.calls != 1 {
		t.Fatalf("post-snapshot ran after failed pass: %d calls", store.calls)
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
