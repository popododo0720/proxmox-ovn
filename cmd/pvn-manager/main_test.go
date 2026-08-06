package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pvnconfig "github.com/popododo0720/proxmox-ovn/internal/config"
	"github.com/popododo0720/proxmox-ovn/internal/pki"
)

func TestApplyClusterConfigHonorsExplicitFlags(t *testing.T) {
	target := managerConfig{listen: "flag-listen", unixSocket: "old-socket", tlsCert: "old-cert", tlsKey: "old-key", pveAPIURL: "old-pve", clusterName: "old-cluster", webRoot: "old-web", sessionTTL: time.Minute}
	cluster := pvnconfig.Default()
	cluster.Cluster.ID = "cluster-a"
	cluster.Manager.ListenAddress = ":8443"
	cluster.Manager.UnixSocket = "/run/pvn/manager.sock"
	cluster.Manager.WebRoot = "/usr/share/pvn/web"
	cluster.Security.SessionTTL = 20 * time.Minute
	cluster.Security.AllowedOrigins = []string{"https://pve.example:8006"}
	cluster.OVN.ControlDB = []string{"ssl:192.0.2.10:6645"}
	cluster.OVN.Northbound = []string{"ssl:192.0.2.10:6641"}
	cluster.OVN.Southbound = []string{"ssl:192.0.2.10:6642"}
	cluster.OVN.TLSCA = "/etc/pvn/pki/ca.pem"
	cluster.OVN.TLSCert = "/etc/pvn/pki/node.pem"
	cluster.OVN.TLSKey = "/etc/pvn/pki/node-key.pem"
	applyClusterConfig(&target, cluster, map[string]bool{"listen": true})
	if target.listen != "flag-listen" {
		t.Fatalf("explicit listen overwritten: %q", target.listen)
	}
	if target.unixSocket != cluster.Manager.UnixSocket || target.clusterName != "cluster-a" || target.webRoot != cluster.Manager.WebRoot || target.sessionTTL != 20*time.Minute {
		t.Fatalf("cluster config not applied: %#v", target)
	}
	if len(target.frameAncestors) != 1 || target.frameAncestors[0] != "https://pve.example:8006" {
		t.Fatalf("frame ancestors=%v", target.frameAncestors)
	}
	if len(target.controlDB) != 1 || len(target.northbound) != 1 || len(target.southbound) != 1 || target.southbound[0] != cluster.OVN.Southbound[0] || target.ovnTLSCA != cluster.OVN.TLSCA || target.reconcileEvery != cluster.Cluster.ReconcileEvery {
		t.Fatalf("OVN settings not applied: %#v", target)
	}
}

func TestReconcilerHealthTracksSuccessfulFailedAndStalePasses(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	health := newReconcilerHealth(time.Minute, func() time.Time { return now })
	if err := health.Probe(context.Background()); err == nil {
		t.Fatal("reconciler was ready before its first full pass")
	}
	health.start()
	if err := health.Probe(context.Background()); err == nil {
		t.Fatal("reconciler was ready while its first pass was in progress")
	}
	health.record(nil)
	if err := health.Probe(context.Background()); err != nil {
		t.Fatalf("successful pass was not ready: %v", err)
	}
	now = now.Add(3*time.Minute + time.Second)
	health.start()
	if err := health.Probe(context.Background()); err != nil {
		t.Fatalf("healthy in-flight pass was not ready: %v", err)
	}
	now = now.Add(health.timeout)
	if err := health.Probe(context.Background()); err != nil {
		t.Fatalf("in-flight pass at its deadline was not ready: %v", err)
	}
	now = now.Add(time.Nanosecond)
	if err := health.Probe(context.Background()); err == nil {
		t.Fatal("over-time pass was reported ready")
	}
	health.record(errors.New("render failed"))
	if err := health.Probe(context.Background()); err == nil {
		t.Fatal("failed pass was reported ready")
	}
	health.start()
	if err := health.Probe(context.Background()); err == nil {
		t.Fatal("a recovering pass hid the last failed pass")
	}
	health.record(nil)
	now = now.Add(3*time.Minute + time.Second)
	if err := health.Probe(context.Background()); err == nil {
		t.Fatal("stale pass was reported ready")
	}
}

func TestReconcileSchedulingDurations(t *testing.T) {
	tests := []struct {
		name      string
		interval  time.Duration
		freshness time.Duration
		timeout   time.Duration
	}{
		{name: "short worker retry", interval: time.Minute, freshness: 30 * time.Minute, timeout: 5 * time.Minute},
		{name: "freshness scales", interval: 4 * time.Minute, freshness: 40 * time.Minute, timeout: 5 * time.Minute},
		{name: "both scale", interval: 10 * time.Minute, freshness: 100 * time.Minute, timeout: 10 * time.Minute},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := reconcileFreshness(testCase.interval); got != testCase.freshness {
				t.Fatalf("freshness=%v want=%v", got, testCase.freshness)
			}
			if got := reconcilePassTimeout(testCase.interval); got != testCase.timeout {
				t.Fatalf("timeout=%v want=%v", got, testCase.timeout)
			}
		})
	}
}

type periodicReconcileCall struct {
	freshness time.Duration
	deadline  time.Time
	release   chan error
}

type controlledPeriodicReconciler struct {
	calls chan periodicReconcileCall
}

func (reconciler *controlledPeriodicReconciler) ReconcilePeriodic(ctx context.Context, freshness time.Duration) error {
	deadline, _ := ctx.Deadline()
	call := periodicReconcileCall{freshness: freshness, deadline: deadline, release: make(chan error, 1)}
	select {
	case reconciler.calls <- call:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-call.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type testDefaultSecurityRepairer struct {
	calls int
	err   error
}

func (repairer *testDefaultSecurityRepairer) EnsureDefaultSecurityPolicies(context.Context) error {
	repairer.calls++
	return repairer.err
}

type testPeriodicReconciler struct {
	calls int
	err   error
}

func (reconciler *testPeriodicReconciler) ReconcilePeriodic(context.Context, time.Duration) error {
	reconciler.calls++
	return reconciler.err
}

func TestDefaultSecurityStartupFailureIsNonFatal(t *testing.T) {
	repairer := &testDefaultSecurityRepairer{err: errors.New("legacy name collision")}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repairDefaultSecurityOnStartup(context.Background(), repairer, logger)
	if repairer.calls != 1 {
		t.Fatalf("startup repair calls=%d", repairer.calls)
	}
}

func TestPeriodicReconcileContinuesWhenDefaultSecurityRepairFails(t *testing.T) {
	policyErr := errors.New("default policy blocked")
	reconcileErr := errors.New("render failed")
	repairer := &testDefaultSecurityRepairer{err: policyErr}
	controller := &testPeriodicReconciler{err: reconcileErr}
	err := (managerPeriodicReconciler{controller: controller, defaultSecurity: repairer}).ReconcilePeriodic(context.Background(), time.Minute)
	if repairer.calls != 1 || controller.calls != 1 {
		t.Fatalf("repair calls=%d reconcile calls=%d", repairer.calls, controller.calls)
	}
	if !errors.Is(err, policyErr) || !errors.Is(err, reconcileErr) {
		t.Fatalf("joined error=%v", err)
	}
}

func TestReconcileLoopRunsImmediatelyThenWaitsAfterCompletion(t *testing.T) {
	const interval = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reconciler := &controlledPeriodicReconciler{calls: make(chan periodicReconcileCall, 1)}
	health := newReconcilerHealth(interval, time.Now)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	waits := make(chan time.Duration, 1)
	resume := make(chan bool, 1)
	wait := func(ctx context.Context, duration time.Duration) bool {
		waits <- duration
		select {
		case proceed := <-resume:
			return proceed
		case <-ctx.Done():
			return false
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcilePeriodicallyWithWait(ctx, reconciler, interval, health, logger, wait)
	}()

	var first periodicReconcileCall
	select {
	case first = <-reconciler.calls:
	case <-time.After(time.Second):
		t.Fatal("first periodic pass did not start immediately")
	}
	if first.freshness != 30*time.Minute {
		t.Fatalf("freshness=%v", first.freshness)
	}
	if remaining := time.Until(first.deadline); remaining < 4*time.Minute || remaining > 5*time.Minute {
		t.Fatalf("pass deadline remaining=%v", remaining)
	}
	select {
	case <-waits:
		t.Fatal("retry interval started before the pass completed")
	default:
	}
	first.release <- nil
	select {
	case duration := <-waits:
		if duration != interval {
			t.Fatalf("wait duration=%v", duration)
		}
	case <-time.After(time.Second):
		t.Fatal("retry interval did not start after completion")
	}
	select {
	case <-reconciler.calls:
		t.Fatal("next pass started before the completion-based wait ended")
	default:
	}
	resume <- true
	select {
	case <-reconciler.calls:
	case <-time.After(time.Second):
		t.Fatal("next pass did not start after the wait ended")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("periodic loop did not stop after cancellation")
	}
}

func TestLoadMutualTLS(t *testing.T) {
	directory := t.TempDir()
	ca, err := pki.CreateCA(pki.CAOptions{Directory: filepath.Join(directory, "ca"), ClusterID: "manager-test"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := pki.IssueNode(pki.IssueOptions{
		CACertificate: ca.Certificate,
		CAKey:         ca.PrivateKey,
		Directory:     filepath.Join(directory, "node"),
		Name:          "pve-a",
		DNSNames:      []string{"pve-a"},
		IPAddresses:   []net.IP{net.ParseIP("192.0.2.10")},
	})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := loadMutualTLS(ca.Certificate, node.Certificate, node.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.MinVersion < tls.VersionTLS12 || configuration.RootCAs == nil || len(configuration.Certificates) != 1 {
		t.Fatalf("TLS configuration = %#v", configuration)
	}
}

func TestListenUnixCreatesRestrictedSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "manager.sock")
	listener, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("mode %v is not a socket", info.Mode())
	}
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("socket permissions=%o", info.Mode().Perm())
	}
}

func TestListenUnixRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.sock")
	if err := os.WriteFile(path, []byte("do not replace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnix(path); err == nil {
		t.Fatal("regular file was replaced")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "do not replace" {
		t.Fatalf("regular file changed: %q err=%v", content, err)
	}
}

func TestRunVersionDoesNotRequireRuntimeConfig(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
}
