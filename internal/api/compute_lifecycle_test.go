package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type computeTestReconciler struct {
	mu          sync.Mutex
	store       controlstore.Store
	forcedCalls int
	failCalls   map[int]bool
}

func (reconciler *computeTestReconciler) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	return reconciler.mark(ctx, kind, id)
}

func (reconciler *computeTestReconciler) ReconcileForced(ctx context.Context, kind model.Kind, id string) error {
	reconciler.mu.Lock()
	reconciler.forcedCalls++
	call := reconciler.forcedCalls
	fail := reconciler.failCalls[call]
	reconciler.mu.Unlock()
	if fail {
		return fmt.Errorf("injected forced reconcile failure %d", call)
	}
	return reconciler.mark(ctx, kind, id)
}

func (reconciler *computeTestReconciler) mark(ctx context.Context, kind model.Kind, id string) error {
	resource, err := reconciler.store.Get(ctx, kind, id)
	if err != nil {
		return err
	}
	_, err = reconciler.store.MarkReconciled(ctx, kind, id, resource.GetMetadata().Revision, nil)
	return err
}

type computeTestTopology struct {
	store   controlstore.Store
	recon   *computeTestReconciler
	now     time.Time
	network *model.Network
	group   *model.SecurityGroup
	source  *model.Node
	target  *model.Node
}

func newComputeTestTopology(t *testing.T) *computeTestTopology {
	t.Helper()
	topology := &computeTestTopology{store: controlstore.NewMemory(), now: time.Date(2026, 8, 8, 3, 0, 0, 0, time.UTC)}
	createReady := func(resource model.Resource, key string) model.Resource {
		created, _, err := topology.store.Create(context.Background(), resource, key)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := topology.store.MarkReconciled(context.Background(), created.ResourceKind(), created.GetMetadata().ID, created.GetMetadata().Revision, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ready
	}
	topology.network = createReady(&model.Network{Name: "compute-private"}, "compute-network").(*model.Network)
	topology.group = createReady(&model.SecurityGroup{Name: "compute-baseline"}, "compute-group").(*model.SecurityGroup)
	topology.source = createReady(&model.Node{
		Name: "pve-source", ChassisID: "chassis-source", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true, LastSeenAt: timePointer(topology.now),
	}, "compute-source").(*model.Node)
	topology.target = createReady(&model.Node{
		Name: "pve-target", ChassisID: "chassis-target", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true, LastSeenAt: timePointer(topology.now),
	}, "compute-target").(*model.Node)
	topology.recon = &computeTestReconciler{store: topology.store, failCalls: make(map[int]bool)}
	return topology
}

func (topology *computeTestTopology) server(t *testing.T, local string, requireAll bool, probeErr error) *Server {
	t.Helper()
	server, err := New(Options{
		Store: topology.store, Reconciler: topology.recon, RequireAllNodes: requireAll,
		NodeHeartbeatTTL: 2 * time.Minute, Clock: func() time.Time { return topology.now }, GuestMTU: 1400,
		ComputeNode: local, ComputeProbe: HealthProbeFunc(func(context.Context) error { return probeErr }),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func (topology *computeTestTopology) port(t *testing.T, vmid int, nic, mac string) *model.Port {
	t.Helper()
	created, _, err := topology.store.Create(context.Background(), &model.Port{
		NetworkID: topology.network.ID, Name: fmt.Sprintf("vm%d-%s", vmid, nic), MACAddress: mac,
		SecurityGroupIDs: []string{topology.group.ID}, AdminStateUp: true, BindingStatus: model.PortBound,
		NodeID: topology.source.ID, VMID: vmid, NIC: nic, RequestedChassis: topology.source.ChassisID, Generation: 2,
	}, fmt.Sprintf("compute-port-%d-%s", vmid, nic))
	if err != nil {
		t.Fatal(err)
	}
	ready, err := topology.store.MarkReconciled(context.Background(), model.KindPort, created.GetMetadata().ID, created.GetMetadata().Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ready.(*model.Port)
}

func TestComputeStartIsLocalProbeGatedForcedAndOffAgentSocket(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 100, "net0", "02:00:00:00:00:64")
	body := computeStartBody(100, topology.source.Name, port)
	server := topology.server(t, topology.source.Name, false, nil)

	ready := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if ready.Code != http.StatusOK || topology.recon.forcedCalls != 1 {
		t.Fatalf("start status=%d forced=%d body=%s", ready.Code, topology.recon.forcedCalls, ready.Body.String())
	}
	agentSocket := request(t, server.RuntimeHandler(), http.MethodPost, computeStartPath, body, nil)
	if agentSocket.Code != http.StatusNotFound {
		t.Fatalf("compute route leaked onto agent socket: %d %s", agentSocket.Code, agentSocket.Body.String())
	}
	crossNode := request(t, topology.server(t, topology.target.Name, false, nil).ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if crossNode.Code != http.StatusConflict || apiErrorCode(t, crossNode) != "wrong_compute_node" {
		t.Fatalf("cross-node start status=%d body=%s", crossNode.Code, crossNode.Body.String())
	}
	unhealthy := request(t, topology.server(t, topology.source.Name, false, errors.New("watcher scan failed")).ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if unhealthy.Code != http.StatusServiceUnavailable || apiErrorCode(t, unhealthy) != "compute_agent_unhealthy" {
		t.Fatalf("unhealthy agent start status=%d body=%s", unhealthy.Code, unhealthy.Body.String())
	}
	topology.recon.mu.Lock()
	topology.recon.failCalls[topology.recon.forcedCalls+1] = true
	topology.recon.mu.Unlock()
	driftFailure := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if driftFailure.Code != http.StatusServiceUnavailable || apiErrorCode(t, driftFailure) != "reconcile_failed" {
		t.Fatalf("ready desired state bypassed forced OVN check: %d %s", driftFailure.Code, driftFailure.Body.String())
	}
}

func TestComputeStartRequiresExactFullManagedNICSet(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 101, "net0", "02:00:00:00:00:65")
	second := topology.port(t, 101, "net1", "02:00:00:00:00:66")
	server := topology.server(t, topology.source.Name, false, nil)
	missing := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, computeStartBody(101, topology.source.Name, first), nil)
	if missing.Code != http.StatusConflict || apiErrorCode(t, missing) != "pvn_nic_set_mismatch" {
		t.Fatalf("subset start status=%d body=%s", missing.Code, missing.Body.String())
	}
	wrongMAC := computeStartBody(101, topology.source.Name, first, second)
	wrongMAC["nics"].([]computeNIC)[1].MACAddress = "02:00:00:00:ff:ff"
	rejected := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, wrongMAC, nil)
	if rejected.Code != http.StatusConflict || apiErrorCode(t, rejected) != "pvn_mac_mismatch" {
		t.Fatalf("MAC mismatch status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}

func TestComputeStartRejectsFarFutureNodeHeartbeat(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 102, "net0", "02:00:00:00:00:67")
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(computeClockSkew+time.Second))
	response := request(t, topology.server(t, topology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeStartPath, computeStartBody(102, topology.source.Name, port), nil)
	if response.Code != http.StatusServiceUnavailable || apiErrorCode(t, response) != "node_not_ready" {
		t.Fatalf("future heartbeat status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMigrationBeginResponseLossReplaysExactTransactionAndRejectsConflict(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 110, "net0", "02:00:00:00:00:6e")
	server := topology.server(t, topology.source.Name, false, nil)
	body := migrationBeginBody("migration-110", topology, true, port)
	first := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("begin status=%d body=%s", first.Code, first.Body.String())
	}
	firstData := decodeMigrationBegin(t, first)
	if firstData.SourceNode != topology.source.Name || firstData.TargetNode != topology.target.Name || len(firstData.Transaction.Ports) != 1 {
		t.Fatalf("begin response is not a complete finish echo: %#v", firstData)
	}
	second := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	secondData := decodeMigrationBegin(t, second)
	if second.Code != http.StatusOK || second.Header().Get("Idempotency-Replayed") != "true" || !reflect.DeepEqual(firstData, secondData) {
		t.Fatalf("lost-response replay status=%d headers=%v first=%#v second=%#v body=%s", second.Code, second.Header(), firstData, secondData, second.Body.String())
	}
	conflict := migrationBeginBody("migration-110", topology, false, port)
	conflict["source_stopped"] = true
	changed := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, conflict, nil)
	if changed.Code != http.StatusConflict || apiErrorCode(t, changed) != "migration_id_conflict" {
		t.Fatalf("conflicting lifecycle replay status=%d body=%s", changed.Code, changed.Body.String())
	}
	operation := loadComputeOperation(t, topology.store, firstData.Transaction.OperationID)
	if operation.OperationStatus != model.OperationRunning || operation.Payload == "" || operation.StartedAt == nil {
		t.Fatalf("migration parent operation=%#v", operation)
	}
}

func TestOnlineMigrationTargetStartRequiresOneFreshExactIntent(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 120, "net0", "02:00:00:00:00:78")
	sourceServer := topology.server(t, topology.source.Name, false, nil)
	targetServer := topology.server(t, topology.target.Name, false, nil)
	begin := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-120", topology, true, port), nil)
	data := decodeMigrationBegin(t, begin)

	ordinarySource := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeStartPath, computeStartBody(120, topology.source.Name, port), nil)
	if ordinarySource.Code != http.StatusConflict || apiErrorCode(t, ordinarySource) != "migration_intent_required" {
		t.Fatalf("source ordinary start accepted dual intent: %d %s", ordinarySource.Code, ordinarySource.Body.String())
	}
	targetBody := computeStartBody(120, topology.target.Name, port)
	targetBody["migration_source"] = topology.source.Name
	allowed := request(t, targetServer.ComputeHandler(), http.MethodPost, computeStartPath, targetBody, nil)
	if allowed.Code != http.StatusOK {
		t.Fatalf("target start could not discover exact intent: %d %s", allowed.Code, allowed.Body.String())
	}
	targetBody["lifecycle_id"] = "wrong-id"
	wrongID := request(t, targetServer.ComputeHandler(), http.MethodPost, computeStartPath, targetBody, nil)
	if wrongID.Code != http.StatusConflict || apiErrorCode(t, wrongID) != "migration_intent_mismatch" {
		t.Fatalf("wrong diagnostic lifecycle id status=%d body=%s", wrongID.Code, wrongID.Body.String())
	}
	targetBody["lifecycle_id"] = ""
	addAmbiguousMigrationOperation(t, topology, data.Transaction.OperationID, "migration-120-other")
	ambiguous := request(t, targetServer.ComputeHandler(), http.MethodPost, computeStartPath, targetBody, nil)
	if ambiguous.Code != http.StatusConflict || apiErrorCode(t, ambiguous) != "migration_intent_ambiguous" {
		t.Fatalf("ambiguous target start status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}
}

func TestOnlineMigrationTargetStartRejectsMissingAndExpiredIntent(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 121, "net0", "02:00:00:00:00:79")
	prepared := clonePort(port)
	prepared.Metadata = model.Metadata{ID: port.ID}
	prepared.RequestedChassis = topology.source.ChassisID + "," + topology.target.ChassisID
	prepared.BindingStatus = model.PortBinding
	prepared.Generation++
	updated, _, err := topology.store.Update(context.Background(), prepared, port.Revision, "manual-dual-without-intent")
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, updated)
	targetBody := computeStartBody(121, topology.target.Name, updated.(*model.Port))
	targetBody["migration_source"] = topology.source.Name
	missing := request(t, topology.server(t, topology.target.Name, false, nil).ComputeHandler(), http.MethodPost, computeStartPath, targetBody, nil)
	if missing.Code != http.StatusConflict || apiErrorCode(t, missing) != "migration_intent_mismatch" {
		t.Fatalf("missing intent target start status=%d body=%s", missing.Code, missing.Body.String())
	}

	other := newComputeTestTopology(t)
	otherPort := other.port(t, 122, "net0", "02:00:00:00:00:7a")
	begin := request(t, other.server(t, other.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-expired", other, true, otherPort), nil)
	if begin.Code != http.StatusOK {
		t.Fatal(begin.Body.String())
	}
	other.now = other.now.Add(computeIntentLifetime + time.Second)
	currentTarget, err := other.store.Get(context.Background(), model.KindNode, other.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.store.ObserveNodeHeartbeat(context.Background(), other.target.ID, currentTarget.GetMetadata().Revision, other.now); err != nil {
		t.Fatal(err)
	}
	expiredBody := computeStartBody(122, other.target.Name, loadComputePort(t, other.store, otherPort.ID))
	expiredBody["migration_source"] = other.source.Name
	expired := request(t, other.server(t, other.target.Name, false, nil).ComputeHandler(), http.MethodPost, computeStartPath, expiredBody, nil)
	if expired.Code != http.StatusConflict || apiErrorCode(t, expired) != "migration_intent_expired" {
		t.Fatalf("expired target start status=%d body=%s", expired.Code, expired.Body.String())
	}
}

func TestMigrationFinishRequiresExactPortBijectionAndReplaysTerminalSuccess(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 130, "net0", "02:00:00:00:00:82")
	second := topology.port(t, 130, "net1", "02:00:00:00:00:83")
	server := topology.server(t, topology.source.Name, false, nil)
	begin := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-130", topology, true, first, second), nil)
	data := decodeMigrationBegin(t, begin)
	finish := migrationFinishBody(data)

	subset := cloneFinishRequest(finish)
	subset.Transaction.Ports = subset.Transaction.Ports[:1]
	assertTransactionRejected(t, server, subset)
	duplicate := cloneFinishRequest(finish)
	duplicate.Transaction.Ports[1] = duplicate.Transaction.Ports[0]
	assertTransactionRejected(t, server, duplicate)
	reordered := cloneFinishRequest(finish)
	reordered.Transaction.Ports[0], reordered.Transaction.Ports[1] = reordered.Transaction.Ports[1], reordered.Transaction.Ports[0]
	assertTransactionRejected(t, server, reordered)
	tampered := cloneFinishRequest(finish)
	tampered.Transaction.PayloadHash = "00" + tampered.Transaction.PayloadHash[2:]
	assertTransactionRejected(t, server, tampered)

	finalized := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, finish, nil)
	if finalized.Code != http.StatusOK {
		t.Fatalf("finalize status=%d body=%s", finalized.Code, finalized.Body.String())
	}
	for _, original := range []*model.Port{first, second} {
		port := loadComputePort(t, topology.store, original.ID)
		if port.NodeID != topology.target.ID || port.RequestedChassis != topology.target.ChassisID || port.Generation != original.Generation+1 || port.State != model.ResourceReady {
			t.Fatalf("finalized port=%#v", port)
		}
	}
	for replay := 0; replay < 3; replay++ {
		response := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, finish, nil)
		if response.Code != http.StatusOK || response.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatalf("finalize replay %d status=%d headers=%v body=%s", replay, response.Code, response.Header(), response.Body.String())
		}
	}
	abort := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationAbortPath, finish, nil)
	if abort.Code != http.StatusConflict || apiErrorCode(t, abort) != "migration_transaction_terminal" {
		t.Fatalf("opposite terminal phase status=%d body=%s", abort.Code, abort.Body.String())
	}
}

func TestMigrationPartialFinalizeIsRetryableWithoutAgentProbe(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 131, "net0", "02:00:00:00:00:84")
	second := topology.port(t, 131, "net1", "02:00:00:00:00:85")
	server := topology.server(t, topology.source.Name, false, nil)
	begin := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-131", topology, true, first, second), nil)
	data := decodeMigrationBegin(t, begin)
	topology.recon.mu.Lock()
	topology.recon.failCalls[topology.recon.forcedCalls+2] = true
	topology.recon.mu.Unlock()
	finish := migrationFinishBody(data)
	partial := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, finish, nil)
	if partial.Code != http.StatusServiceUnavailable || apiErrorCode(t, partial) != "migration_finish_failed" {
		t.Fatalf("partial finalize status=%d body=%s", partial.Code, partial.Body.String())
	}
	// Cleanup paths intentionally ignore local watcher health; the OVN forced
	// reconcile remains mandatory and can repair the pending second port.
	unhealthyFinishServer := topology.server(t, topology.source.Name, false, errors.New("agent unavailable"))
	retried := request(t, unhealthyFinishServer.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, finish, nil)
	if retried.Code != http.StatusOK {
		t.Fatalf("finalize retry was blocked by agent health: %d %s", retried.Code, retried.Body.String())
	}
}

func TestMigrationPrepareFailureCompensatesAllPortsAndFencesLifecycleID(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 140, "net0", "02:00:00:00:00:8c")
	second := topology.port(t, 140, "net1", "02:00:00:00:00:8d")
	server := topology.server(t, topology.source.Name, false, nil)
	topology.recon.failCalls[2] = true
	body := migrationBeginBody("migration-140", topology, true, first, second)
	failed := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	if failed.Code != http.StatusServiceUnavailable || apiErrorCode(t, failed) != "migration_prepare_failed" || computeRecoveryRequired(t, failed) {
		t.Fatalf("prepare failure status=%d body=%s", failed.Code, failed.Body.String())
	}
	for _, original := range []*model.Port{first, second} {
		port := loadComputePort(t, topology.store, original.ID)
		if port.NodeID != topology.source.ID || port.RequestedChassis != topology.source.ChassisID || port.Generation != original.Generation+1 || port.State != model.ResourceReady {
			t.Fatalf("compensated port=%#v", port)
		}
	}
	sameID := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	if sameID.Code != http.StatusConflict || apiErrorCode(t, sameID) != "migration_id_terminal" {
		t.Fatalf("compensated lifecycle id was reusable: %d %s", sameID.Code, sameID.Body.String())
	}
	newBody := migrationBeginBody("migration-140-retry", topology, true, loadComputePort(t, topology.store, first.ID), loadComputePort(t, topology.store, second.ID))
	newID := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, newBody, nil)
	if newID.Code != http.StatusOK {
		t.Fatalf("new lifecycle id could not retry compensated migration: %d %s", newID.Code, newID.Body.String())
	}
}

func TestOfflineMigrationPrepareFailureRestoresSourceOnly(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 141, "net0", "02:00:00:00:00:8e")
	server := topology.server(t, topology.source.Name, false, nil)
	topology.recon.failCalls[1] = true
	body := migrationBeginBody("migration-offline", topology, false, port)
	body["source_stopped"] = true
	failed := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	if failed.Code != http.StatusServiceUnavailable || computeRecoveryRequired(t, failed) {
		t.Fatalf("offline prepare failure status=%d body=%s", failed.Code, failed.Body.String())
	}
	restored := loadComputePort(t, topology.store, port.ID)
	if restored.NodeID != topology.source.ID || restored.RequestedChassis != topology.source.ChassisID || restored.Generation != port.Generation+1 || restored.State != model.ResourceReady {
		t.Fatalf("offline compensation did not restore source: %#v", restored)
	}
}

func TestMigrationCompensationFailureReportsRecoveryRequired(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 142, "net0", "02:00:00:00:00:8f")
	server := topology.server(t, topology.source.Name, false, nil)
	topology.recon.failCalls[1] = true
	topology.recon.failCalls[2] = true
	failed := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-recovery", topology, true, port), nil)
	if failed.Code != http.StatusServiceUnavailable || !computeRecoveryRequired(t, failed) {
		t.Fatalf("compensation failure did not expose recovery_required: %d %s", failed.Code, failed.Body.String())
	}
	operationID := computeMigrationOperationID("migration-recovery", 142, topology.source.ID, topology.target.ID)
	operation := loadComputeOperation(t, topology.store, operationID)
	payload, err := decodeMigrationPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationFailed || payload.Phase != "recovery-required" {
		t.Fatalf("recovery operation=%#v payload=%#v err=%v", operation, payload, err)
	}
}

func TestHARebindRequiresManagedVMAndCompleteStableMembershipFence(t *testing.T) {
	tests := []struct {
		name       string
		haManaged  bool
		online     []string
		stale      bool
		wantCode   string
		wantStatus int
	}{
		{name: "not HA managed", online: []string{"pve-target"}, stale: true, wantCode: "wrong_chassis", wantStatus: http.StatusConflict},
		{name: "target absent", haManaged: true, online: []string{"pve-source"}, stale: true, wantCode: "ha_target_offline", wantStatus: http.StatusConflict},
		{name: "source online", haManaged: true, online: []string{"pve-source", "pve-target"}, stale: true, wantCode: "ha_source_online", wantStatus: http.StatusConflict},
		{name: "source heartbeat fresh", haManaged: true, online: []string{"pve-target"}, wantCode: "ha_source_not_stale", wantStatus: http.StatusConflict},
		{name: "fenced", haManaged: true, online: []string{"pve-target"}, stale: true, wantStatus: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			topology := newComputeTestTopology(t)
			port := topology.port(t, 150, "net0", "02:00:00:00:00:96")
			if test.stale {
				setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
			}
			server := topology.server(t, topology.target.Name, true, nil)
			reporter := test.online[0]
			if err := server.clusterGate.report(reporter, test.online, true, topology.now); err != nil {
				t.Fatal(err)
			}
			body := computeStartBody(150, topology.target.Name, port)
			body["lifecycle_id"] = "ha-start-150"
			body["ha_managed"] = test.haManaged
			response := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
			if response.Code != test.wantStatus || (test.wantCode != "" && apiErrorCode(t, response) != test.wantCode) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Code == http.StatusOK {
				rebound := loadComputePort(t, topology.store, port.ID)
				if rebound.NodeID != topology.target.ID || rebound.RequestedChassis != topology.target.ChassisID || rebound.Generation != port.Generation+1 {
					t.Fatalf("rebound=%#v", rebound)
				}
			}
		})
	}
}

func TestMigrationBeginPinsSourceToLocalComputeAndProbe(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 160, "net0", "02:00:00:00:00:a0")
	body := migrationBeginBody("migration-160", topology, true, port)
	crossNode := request(t, topology.server(t, topology.target.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	if crossNode.Code != http.StatusConflict || apiErrorCode(t, crossNode) != "wrong_compute_node" {
		t.Fatalf("cross-node begin status=%d body=%s", crossNode.Code, crossNode.Body.String())
	}
	unhealthy := request(t, topology.server(t, topology.source.Name, false, errors.New("agent unhealthy")).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	if unhealthy.Code != http.StatusServiceUnavailable || apiErrorCode(t, unhealthy) != "compute_agent_unhealthy" {
		t.Fatalf("unhealthy begin status=%d body=%s", unhealthy.Code, unhealthy.Body.String())
	}
}

type migrationBeginData struct {
	LifecycleID string                      `json:"lifecycle_id"`
	VMID        int                         `json:"vmid"`
	SourceNode  string                      `json:"source_node"`
	TargetNode  string                      `json:"target_node"`
	Online      bool                        `json:"online"`
	Transaction computeMigrationTransaction `json:"transaction"`
}

func computeStartBody(vmid int, node string, ports ...*model.Port) map[string]any {
	nics := make([]computeNIC, 0, len(ports))
	for _, port := range ports {
		nics = append(nics, computeNIC{NIC: port.NIC, MACAddress: port.MACAddress})
	}
	return map[string]any{"vmid": vmid, "node": node, "nics": nics}
}

func migrationBeginBody(id string, topology *computeTestTopology, online bool, ports ...*model.Port) map[string]any {
	nics := make([]computeNIC, 0, len(ports))
	for _, port := range ports {
		nics = append(nics, computeNIC{NIC: port.NIC, MACAddress: port.MACAddress})
	}
	return map[string]any{
		"lifecycle_id": id, "vmid": ports[0].VMID, "source_node": topology.source.Name, "target_node": topology.target.Name,
		"online": online, "source_mtu": 1500, "target_mtu": 1500, "nics": nics,
	}
}

func decodeMigrationBegin(t *testing.T, response *httptest.ResponseRecorder) migrationBeginData {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("migration begin status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeData[migrationBeginData](t, response)
}

func migrationFinishBody(data migrationBeginData) computeMigrationFinishRequest {
	return computeMigrationFinishRequest{
		LifecycleID: data.LifecycleID, VMID: data.VMID, SourceNode: data.SourceNode, TargetNode: data.TargetNode,
		Online: data.Online, Transaction: data.Transaction,
	}
}

func cloneFinishRequest(input computeMigrationFinishRequest) computeMigrationFinishRequest {
	result := input
	result.Transaction.Ports = append([]computeMigrationPortState(nil), input.Transaction.Ports...)
	return result
}

func assertTransactionRejected(t *testing.T, server *Server, input computeMigrationFinishRequest) {
	t.Helper()
	response := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, input, nil)
	if response.Code != http.StatusConflict || apiErrorCode(t, response) != "migration_transaction_mismatch" {
		t.Fatalf("transaction mutation status=%d body=%s", response.Code, response.Body.String())
	}
}

func computeRecoveryRequired(t *testing.T, response *httptest.ResponseRecorder) bool {
	t.Helper()
	var envelope struct {
		Error struct {
			Details struct {
				RecoveryRequired bool `json:"recovery_required"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Error.Details.RecoveryRequired
}

func loadComputePort(t *testing.T, store controlstore.Store, id string) *model.Port {
	t.Helper()
	resource, err := store.Get(context.Background(), model.KindPort, id)
	if err != nil {
		t.Fatal(err)
	}
	return resource.(*model.Port)
}

func loadComputeOperation(t *testing.T, store controlstore.Store, id string) *model.Operation {
	t.Helper()
	resource, err := store.Get(context.Background(), model.KindOperation, id)
	if err != nil {
		t.Fatal(err)
	}
	return resource.(*model.Operation)
}

func markReady(t *testing.T, store controlstore.Store, resource model.Resource) model.Resource {
	t.Helper()
	ready, err := store.MarkReconciled(context.Background(), resource.ResourceKind(), resource.GetMetadata().ID, resource.GetMetadata().Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}

func addAmbiguousMigrationOperation(t *testing.T, topology *computeTestTopology, originalID, lifecycleID string) {
	t.Helper()
	original := loadComputeOperation(t, topology.store, originalID)
	payload, err := decodeMigrationPayload(original)
	if err != nil {
		t.Fatal(err)
	}
	payload.LifecycleID = lifecycleID
	encoded, err := model.MarshalOperationPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	id := computeMigrationOperationID(lifecycleID, payload.VMID, payload.SourceNodeID, payload.TargetNodeID)
	created, _, err := topology.store.Create(context.Background(), &model.Operation{
		Metadata: model.Metadata{ID: id}, Action: computeMigrationAction, TargetKind: model.KindPort,
		TargetID: payload.Ports[0].PortID, TargetRevision: math.MaxInt64 - 7,
		OperationStatus: model.OperationRunning, IdempotencyKey: "ambiguous-" + id,
		LeaseOwner: "compute-lifecycle", StartedAt: timePointer(payload.StartedAt), Payload: encoded,
	}, "ambiguous-"+id)
	if err != nil || created == nil {
		t.Fatalf("create ambiguous operation: %v", err)
	}
}

func setComputeNodeLastSeen(t *testing.T, topology *computeTestTopology, id string, observed time.Time) {
	t.Helper()
	resource, err := topology.store.Get(context.Background(), model.KindNode, id)
	if err != nil {
		t.Fatal(err)
	}
	copyResource, err := model.Clone(resource)
	if err != nil {
		t.Fatal(err)
	}
	node := copyResource.(*model.Node)
	node.LastSeenAt = timePointer(observed)
	updated, _, err := topology.store.Update(context.Background(), node, node.Revision, "set-compute-last-seen-"+id)
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, updated)
}

func timePointer(value time.Time) *time.Time { return &value }
