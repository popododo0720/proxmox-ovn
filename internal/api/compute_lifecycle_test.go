package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
	delete(expiredBody, "migration_source")
	fenced := request(t, other.server(t, other.target.Name, false, nil).ComputeHandler(), http.MethodPost, computeStartPath, expiredBody, nil)
	if fenced.Code != http.StatusConflict || apiErrorCode(t, fenced) != "migration_intent_required" {
		t.Fatalf("ordinary target start did not remain fenced after expiry: %d %s", fenced.Code, fenced.Body.String())
	}
	operation := loadComputeOperation(t, other.store, decodeMigrationBegin(t, begin).Transaction.OperationID)
	payload, err := decodeMigrationPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationRunning || payload.Phase != "prepared" {
		t.Fatalf("expired target start mutated intent: op=%#v payload=%#v err=%v", operation, payload, err)
	}
}

func TestMigrationBeginReplayIgnoresTransientTargetReadinessAndSerializes(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 123, "net0", "02:00:00:00:00:7b")
	server := topology.server(t, topology.source.Name, false, nil)
	body := migrationBeginBody("migration-123", topology, true, port)
	first := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	firstData := decodeMigrationBegin(t, first)
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now.Add(-3*time.Minute))
	replay := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" || !reflect.DeepEqual(firstData, decodeMigrationBegin(t, replay)) {
		t.Fatalf("stale target blocked exact migration replay: %d %s", replay.Code, replay.Body.String())
	}

	concurrentTopology := newComputeTestTopology(t)
	concurrentPort := concurrentTopology.port(t, 124, "net0", "02:00:00:00:00:7c")
	concurrentServer := concurrentTopology.server(t, concurrentTopology.source.Name, false, nil)
	concurrentBody := migrationBeginBody("migration-124", concurrentTopology, true, concurrentPort)
	responses := make(chan *httptest.ResponseRecorder, 8)
	var wait sync.WaitGroup
	for attempt := 0; attempt < 8; attempt++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			responses <- request(t, concurrentServer.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, concurrentBody, nil)
		}()
	}
	wait.Wait()
	close(responses)
	var transaction *migrationBeginData
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent begin status=%d body=%s", response.Code, response.Body.String())
		}
		data := decodeMigrationBegin(t, response)
		if transaction == nil {
			transaction = &data
		} else if !reflect.DeepEqual(*transaction, data) {
			t.Fatalf("concurrent begin returned different transactions: %#v %#v", *transaction, data)
		}
	}
}

func TestMigrationPreparingReplayAcceptsExactSourceRuntimeBindingRevision(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 140, "net0", "02:00:00:00:00:93")
	server := topology.server(t, topology.source.Name, false, nil)
	input := computeMigrationBeginRequest{
		LifecycleID: "migration-source-runtime-140", VMID: 140, SourceNode: topology.source.Name, TargetNode: topology.target.Name,
		Online: true, SourceMTU: 1500, TargetMTU: 1500, NICs: []computeNIC{{NIC: port.NIC, MACAddress: port.MACAddress}},
	}
	payload, err := newMigrationPayload(topology.now, input, topology.source, topology.target, []*model.Port{port})
	if err != nil {
		t.Fatal(err)
	}
	operationID := computeMigrationOperationID(input.LifecycleID, input.VMID, topology.source.ID, topology.target.ID)
	if _, err := server.createComputeOperation(context.Background(), operationID, computeMigrationAction, input.VMID, payload); err != nil {
		t.Fatal(err)
	}
	reported := reportComputePortBound(t, topology, port.ID, "migration-source-runtime-report-140")
	if reported.Revision <= port.Revision || reported.BindingStatus != model.PortBound || reported.State != model.ResourceReady {
		t.Fatalf("source runtime report fixture=%#v", reported)
	}
	response := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, input, nil)
	if response.Code != http.StatusOK || response.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("source runtime preparing replay status=%d body=%s", response.Code, response.Body.String())
	}
	prepared := loadComputePort(t, topology.store, port.ID)
	if prepared.NodeID != topology.source.ID || prepared.RequestedChassis != topology.source.ChassisID+","+topology.target.ChassisID || prepared.Revision <= reported.Revision || prepared.Generation != port.Generation+1 {
		t.Fatalf("source runtime preparing replay port=%#v", prepared)
	}
}

func TestExpiredMigrationOrdinarySourceStartRemainsFenced(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 125, "net0", "02:00:00:00:00:7d")
	server := topology.server(t, topology.source.Name, false, nil)
	begin := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-source-recovery", topology, true, port), nil)
	data := decodeMigrationBegin(t, begin)
	topology.now = topology.now.Add(computeIntentLifetime + time.Second)
	currentSource, err := topology.store.Get(context.Background(), model.KindNode, topology.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := topology.store.ObserveNodeHeartbeat(context.Background(), topology.source.ID, currentSource.GetMetadata().Revision, topology.now); err != nil {
		t.Fatal(err)
	}
	current := loadComputePort(t, topology.store, port.ID)
	fenced := request(t, topology.server(t, topology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeStartPath, computeStartBody(125, topology.source.Name, current), nil)
	if fenced.Code != http.StatusConflict || apiErrorCode(t, fenced) != "migration_intent_required" {
		t.Fatalf("expired source start status=%d body=%s", fenced.Code, fenced.Body.String())
	}
	operation := loadComputeOperation(t, topology.store, data.Transaction.OperationID)
	payload, err := decodeMigrationPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationRunning || payload.Phase != "prepared" {
		t.Fatalf("expired source start mutated intent: op=%#v payload=%#v err=%v", operation, payload, err)
	}
}

func TestHARecoversExactExpiredPreparedMigrationToAssignedTarget(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 126, "net0", "02:00:00:00:00:7e")
	second := topology.port(t, 126, "net1", "02:00:00:00:00:7f")
	begin := request(t, topology.server(t, topology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-ha-126", topology, true, first, second), nil)
	data := decodeMigrationBegin(t, begin)
	topology.now = topology.now.Add(computeIntentLifetime + time.Second)
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now)
	targetServer := topology.server(t, topology.target.Name, true, nil)
	if err := targetServer.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(126, topology.target.Name, loadComputePort(t, topology.store, first.ID), loadComputePort(t, topology.store, second.ID))
	body["ha_managed"] = true
	addHAProof(body, topology, 126, topology.target, "HAuid126target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	recovered := request(t, targetServer.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if recovered.Code != http.StatusOK {
		t.Fatalf("expired migration HA recovery status=%d body=%s", recovered.Code, recovered.Body.String())
	}
	for _, original := range []*model.Port{first, second} {
		current := loadComputePort(t, topology.store, original.ID)
		if current.NodeID != topology.target.ID || current.RequestedChassis != topology.target.ChassisID || current.Revision != original.Revision+2 || current.Generation != original.Generation+1 || current.State != model.ResourceReady {
			t.Fatalf("HA-recovered migration port=%#v", current)
		}
	}
	operation, payload := loadPromotedMigrationHA(t, targetServer, topology.store, data.Transaction.OperationID, "target", "prepared")
	haOperationID := computeResourceOperationID(computeHAAction, body["lifecycle_id"].(string), 126)
	if haOperationID != operation.ID {
		if _, err := topology.store.Get(context.Background(), model.KindOperation, haOperationID); !errors.Is(err, controlstore.ErrNotFound) {
			t.Fatalf("separate canonical HA row was created and broke slot continuity: %v", err)
		}
	}
	if payload.Proof.ServiceUID != "HAuid126target" {
		t.Fatalf("promoted HA authority=%#v", payload.Proof)
	}
	regressedBody := computeStartBody(126, topology.target.Name, loadComputePort(t, topology.store, first.ID), loadComputePort(t, topology.store, second.ID))
	regressedBody["ha_managed"] = true
	addHAProof(regressedBody, topology, 126, topology.target, "HAuid126target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	regressed := regressedBody["ha_proof"].(computeHAProof)
	regressed.ManagerEpoch--
	regressed.LRMEpoch--
	regressed.AgentLockEpoch--
	regressedBody["ha_proof"] = regressed
	rejected := request(t, targetServer.ComputeHandler(), http.MethodPost, computeStartPath, regressedBody, nil)
	if rejected.Code != http.StatusConflict || apiErrorCode(t, rejected) != "ha_proof_regressed" {
		t.Fatalf("regressed post-recovery HA proof status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}

func TestHAExpiredMigrationRecoveryRequiresFinalFenceAndResumesFailure(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 127, "net0", "02:00:00:00:00:80")
	second := topology.port(t, 127, "net1", "02:00:00:00:00:81")
	begin := request(t, topology.server(t, topology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-ha-127", topology, true, first, second), nil)
	data := decodeMigrationBegin(t, begin)
	topology.now = topology.now.Add(computeIntentLifetime + time.Second)
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	thirdResource, _, err := topology.store.Create(context.Background(), &model.Node{Name: "pve-third", ChassisID: "chassis-third", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true, LastSeenAt: timePointer(topology.now)}, "compute-third-127")
	if err != nil {
		t.Fatal(err)
	}
	third := markReady(t, topology.store, thirdResource).(*model.Node)
	server := topology.server(t, third.Name, true, nil)
	if err := server.clusterGate.report(third.Name, []string{third.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(127, third.Name, loadComputePort(t, topology.store, first.ID), loadComputePort(t, topology.store, second.ID))
	body["ha_managed"] = true
	addHAProof(body, topology, 127, third, "HAuid127target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "fence", third.Name: "online"})
	transitional := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if transitional.Code != http.StatusConflict || apiErrorCode(t, transitional) != "ha_source_not_fenced" {
		t.Fatalf("transitional migration target fence status=%d body=%s", transitional.Code, transitional.Body.String())
	}
	operation := loadComputeOperation(t, topology.store, data.Transaction.OperationID)
	payload, decodeErr := decodeMigrationPayload(operation)
	if decodeErr != nil || payload.Phase != "prepared" || payload.HARecovery != nil {
		t.Fatalf("rejected HA recovery mutated migration: op=%#v payload=%#v err=%v", operation, payload, decodeErr)
	}

	addHAProof(body, topology, 127, third, "HAuid127target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "unknown", third.Name: "online"})
	topology.recon.mu.Lock()
	topology.recon.failCalls[topology.recon.forcedCalls+1] = true
	topology.recon.mu.Unlock()
	failed := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if failed.Code != http.StatusServiceUnavailable || apiErrorCode(t, failed) != "ha_migration_recovery_failed" || !computeRecoveryRequired(t, failed) {
		t.Fatalf("interrupted HA migration recovery status=%d body=%s", failed.Code, failed.Body.String())
	}
	operation = loadComputeOperation(t, topology.store, data.Transaction.OperationID)
	payload, decodeErr = decodeMigrationPayload(operation)
	if decodeErr != nil || operation.OperationStatus != model.OperationRunning || payload.Phase != "ha-recovering-target" || payload.HARecovery == nil || operation.Error == "" || operation.TargetID != computeVMOperationTarget(127) {
		t.Fatalf("interrupted HA recovery operation=%#v payload=%#v err=%v", operation, payload, decodeErr)
	}
	conflictingBody := maps.Clone(body)
	conflictingProof := body["ha_proof"].(computeHAProof)
	conflictingProof.NodeStates = maps.Clone(conflictingProof.NodeStates)
	conflictingProof.NodeStates[topology.source.Name] = "gone"
	conflictingBody["ha_proof"] = conflictingProof
	beforeConflictFirst := loadComputePort(t, topology.store, first.ID)
	beforeConflictSecond := loadComputePort(t, topology.store, second.ID)
	conflicting := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, conflictingBody, nil)
	if conflicting.Code != http.StatusConflict || apiErrorCode(t, conflicting) != "ha_proof_conflict" {
		t.Fatalf("equal-epoch conflicting recovery proof status=%d body=%s", conflicting.Code, conflicting.Body.String())
	}
	if afterFirst, afterSecond := loadComputePort(t, topology.store, first.ID), loadComputePort(t, topology.store, second.ID); afterFirst.Revision != beforeConflictFirst.Revision || afterSecond.Revision != beforeConflictSecond.Revision {
		t.Fatalf("conflicting recovery proof mutated ports: first=%#v second=%#v", afterFirst, afterSecond)
	}
	retried := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if retried.Code != http.StatusOK {
		t.Fatalf("HA migration recovery retry status=%d body=%s", retried.Code, retried.Body.String())
	}
	for _, original := range []*model.Port{first, second} {
		current := loadComputePort(t, topology.store, original.ID)
		if current.NodeID != third.ID || current.RequestedChassis != third.ChassisID || current.Revision != original.Revision+3 || current.Generation != original.Generation+2 {
			t.Fatalf("resumed third-target HA recovery port=%#v", current)
		}
	}
}

func TestHAMigrationTakeoverRejectsIncompleteFenceWithoutMutation(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 128, "net0", "02:00:00:00:00:8a")
	begin := request(t, topology.server(t, topology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-ha-128", topology, true, port), nil)
	data := decodeMigrationBegin(t, begin)
	server := topology.server(t, topology.target.Name, true, nil)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.source.Name, topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(128, topology.target.Name, loadComputePort(t, topology.store, port.ID))
	body["ha_managed"] = true
	addHAProof(body, topology, 128, topology.target, "HAuid128target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	response := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if response.Code != http.StatusConflict || apiErrorCode(t, response) != "ha_source_online" {
		t.Fatalf("incompletely fenced HA takeover status=%d body=%s", response.Code, response.Body.String())
	}
	operation := loadComputeOperation(t, topology.store, data.Transaction.OperationID)
	payload, err := decodeMigrationPayload(operation)
	if err != nil || payload.Phase != "prepared" || payload.HARecovery != nil || operation.OperationStatus != model.OperationRunning {
		t.Fatalf("rejected takeover mutated operation=%#v payload=%#v err=%v", operation, payload, err)
	}
}

func TestHATakeoverCompletesFreshOfflinePreparedMigration(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 129, "net0", "02:00:00:00:00:8b")
	body := migrationBeginBody("migration-ha-offline-129", topology, false, port)
	body["source_stopped"] = true
	begin := request(t, topology.server(t, topology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	data := decodeMigrationBegin(t, begin)
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	server := topology.server(t, topology.target.Name, true, nil)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	start := computeStartBody(129, topology.target.Name, loadComputePort(t, topology.store, port.ID))
	start["ha_managed"] = true
	addHAProof(start, topology, 129, topology.target, "HAuid129target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	recovered := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, start, nil)
	if recovered.Code != http.StatusOK {
		t.Fatalf("fresh offline HA takeover status=%d body=%s", recovered.Code, recovered.Body.String())
	}
	current := loadComputePort(t, topology.store, port.ID)
	if current.NodeID != topology.target.ID || current.RequestedChassis != topology.target.ChassisID || current.Revision != port.Revision+1 || current.Generation != port.Generation+1 {
		t.Fatalf("offline HA takeover port=%#v", current)
	}
	loadPromotedMigrationHA(t, server, topology.store, data.Transaction.OperationID, "target", "prepared")
}

func TestHAMigrationRecoveryTerminalBaselineRejectsEqualEpochConflictBeforeCanonicalOperation(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 135, "net0", "02:00:00:00:00:8e")
	begin := request(t, topology.server(t, topology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-ha-baseline-135", topology, true, port), nil)
	decodeMigrationBegin(t, begin)
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	server := topology.server(t, topology.target.Name, true, nil)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(135, topology.target.Name, loadComputePort(t, topology.store, port.ID))
	body["ha_managed"] = true
	addHAProof(body, topology, 135, topology.target, "HAuid135target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	proof := body["ha_proof"].(computeHAProof)
	input := computeStartRequest{
		LifecycleID: body["lifecycle_id"].(string), VMID: 135, Node: topology.target.Name,
		NICs: []computeNIC{{NIC: port.NIC, MACAddress: port.MACAddress}}, HAManaged: true, HAProof: &proof,
	}
	recovered, matched, err := server.recoverMigrationForHA(context.Background(), input, topology.target, []*model.Port{loadComputePort(t, topology.store, port.ID)})
	if err != nil || !matched || recovered == nil || len(recovered.Ports) != 1 {
		t.Fatalf("direct migration recovery matched=%v recovered=%#v err=%v", matched, recovered, err)
	}
	canonicalID := computeResourceOperationID(computeHAAction, input.LifecycleID, 135)
	if recovered.Operation.ID == canonicalID {
		t.Fatalf("migration takeover unexpectedly changed the operation identity")
	}
	if recovered.Operation.Action != computeHAAction || recovered.Operation.OperationStatus != model.OperationRunning || recovered.Operation.TargetID != computeVMOperationTarget(135) || recovered.Payload.MigrationRecovery == nil {
		t.Fatalf("migration takeover did not atomically retain the active slot: %#v %#v", recovered.Operation, recovered.Payload)
	}
	if _, err := topology.store.Get(context.Background(), model.KindOperation, canonicalID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("canonical HA operation unexpectedly exists before callback continuation: %v", err)
	}
	conflicting := proof
	conflicting.NodeStates = maps.Clone(proof.NodeStates)
	conflicting.NodeStates[topology.source.Name] = "gone"
	body["ha_proof"] = conflicting
	rejected := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if rejected.Code != http.StatusConflict || apiErrorCode(t, rejected) != "ha_proof_conflict" {
		t.Fatalf("terminal recovery baseline conflict status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if _, err := topology.store.Get(context.Background(), model.KindOperation, canonicalID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("conflicting baseline created canonical HA operation: %v", err)
	}
}

func TestPromotedHAMigrationRetainsSlotAndFencesOrdinaryStartAndOldFinish(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 142, "net0", "02:00:00:00:00:94")
	sourceServer := topology.server(t, topology.source.Name, false, nil)
	begin := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-ha-slot-142", topology, true, port), nil)
	data := decodeMigrationBegin(t, begin)
	thirdResource, _, err := topology.store.Create(context.Background(), &model.Node{
		Name: "pve-third", ChassisID: "chassis-third", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true, LastSeenAt: timePointer(topology.now),
	}, "compute-third-142")
	if err != nil {
		t.Fatal(err)
	}
	third := markReady(t, topology.store, thirdResource).(*model.Node)
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	server := topology.server(t, third.Name, true, nil)
	if err := server.clusterGate.report(third.Name, []string{third.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(142, third.Name, loadComputePort(t, topology.store, port.ID))
	body["ha_managed"] = true
	addHAProof(body, topology, 142, third, "HAuid142target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "unknown", third.Name: "online"})
	topology.recon.mu.Lock()
	topology.recon.failCalls[topology.recon.forcedCalls+2] = true
	topology.recon.mu.Unlock()
	failed := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if failed.Code != http.StatusServiceUnavailable || apiErrorCode(t, failed) != "ha_rebind_failed" {
		t.Fatalf("promoted HA partial failure status=%d body=%s", failed.Code, failed.Body.String())
	}
	operation, payload, err := server.loadHAOperation(context.Background(), data.Transaction.OperationID)
	if err != nil || operation.OperationStatus != model.OperationRunning || operation.Action != computeHAAction || operation.TargetID != computeVMOperationTarget(142) || payload.Phase != "rebinding" || payload.MigrationRecovery == nil {
		t.Fatalf("promoted active slot operation=%#v payload=%#v err=%v", operation, payload, err)
	}
	before := loadComputePort(t, topology.store, port.ID)
	manual := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, computeStartBody(142, third.Name, before), nil)
	if manual.Code != http.StatusConflict || apiErrorCode(t, manual) != "compute_lifecycle_active" {
		t.Fatalf("ordinary start bypassed promoted HA slot status=%d body=%s", manual.Code, manual.Body.String())
	}
	oldFinish := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, migrationFinishBody(data), nil)
	if oldFinish.Code != http.StatusConflict || apiErrorCode(t, oldFinish) != "migration_transaction_mismatch" {
		t.Fatalf("old migration finalize bypassed action takeover status=%d body=%s", oldFinish.Code, oldFinish.Body.String())
	}
	if after := loadComputePort(t, topology.store, port.ID); after.Revision != before.Revision || after.Generation != before.Generation || after.NodeID != before.NodeID {
		t.Fatalf("fenced ordinary/finalize request mutated promoted port=%#v before=%#v", after, before)
	}
	topology.recon.mu.Lock()
	delete(topology.recon.failCalls, topology.recon.forcedCalls)
	for call := range topology.recon.failCalls {
		delete(topology.recon.failCalls, call)
	}
	topology.recon.mu.Unlock()
	retried := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if retried.Code != http.StatusOK {
		t.Fatalf("promoted HA exact replay status=%d body=%s", retried.Code, retried.Body.String())
	}
	loadPromotedMigrationHA(t, server, topology.store, data.Transaction.OperationID, "target", "prepared")
}

func TestPreparingMigrationLateDualWriteIsAcceptedOnlyAsFencedHAPredecessor(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 143, "net0", "02:00:00:00:00:95")
	beginInput := computeMigrationBeginRequest{
		LifecycleID: "migration-ha-late-prepare-143", VMID: 143, SourceNode: topology.source.Name, TargetNode: topology.target.Name,
		Online: true, SourceMTU: 1500, TargetMTU: 1500, NICs: []computeNIC{{NIC: port.NIC, MACAddress: port.MACAddress}},
	}
	migration, err := newMigrationPayload(topology.now, beginInput, topology.source, topology.target, []*model.Port{port})
	if err != nil {
		t.Fatal(err)
	}
	operationID := computeMigrationOperationID(beginInput.LifecycleID, beginInput.VMID, topology.source.ID, topology.target.ID)
	if _, err := topology.server(t, topology.source.Name, false, nil).createComputeOperation(context.Background(), operationID, computeMigrationAction, 143, migration); err != nil {
		t.Fatal(err)
	}
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	server := topology.server(t, topology.target.Name, true, nil)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(143, topology.target.Name, port)
	body["ha_managed"] = true
	addHAProof(body, topology, 143, topology.target, "HAuid143target", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	proof := body["ha_proof"].(computeHAProof)
	input := computeStartRequest{
		LifecycleID: body["lifecycle_id"].(string), VMID: 143, Node: topology.target.Name,
		NICs: []computeNIC{{NIC: port.NIC, MACAddress: port.MACAddress}}, HAManaged: true, HAProof: &proof,
	}
	adoption, matched, err := server.recoverMigrationForHA(context.Background(), input, topology.target, []*model.Port{port})
	if err != nil || !matched || adoption == nil || adoption.Payload.MigrationRecovery == nil || adoption.Payload.MigrationRecovery.OriginalPhase != "preparing" {
		t.Fatalf("preparing migration promotion adoption=%#v matched=%v err=%v", adoption, matched, err)
	}
	late := clonePort(loadComputePort(t, topology.store, port.ID))
	expectedRevision := late.Revision
	late.Metadata = model.Metadata{ID: late.ID}
	late.NodeID, late.RequestedChassis = topology.source.ID, topology.source.ChassisID+","+topology.target.ChassisID
	late.BindingStatus, late.Generation = model.PortBinding, migration.Ports[0].Generation
	if _, _, err := topology.store.Update(context.Background(), late, expectedRevision, "simulated-late-migration-prepare-143"); err != nil {
		t.Fatal(err)
	}
	retried := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if retried.Code != http.StatusOK {
		t.Fatalf("late dual predecessor HA replay status=%d body=%s", retried.Code, retried.Body.String())
	}
	current := loadComputePort(t, topology.store, port.ID)
	if current.NodeID != topology.target.ID || current.RequestedChassis != topology.target.ChassisID || current.Generation != migration.Ports[0].Generation+1 {
		t.Fatalf("late dual predecessor was not strictly generation-fenced: %#v", current)
	}
}

func TestPromotedHAMigrationAuditSurvivesNewerAssignmentSupersession(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 145, "net0", "02:00:00:00:00:97")
	begin := request(t, topology.server(t, topology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-ha-audit-145", topology, true, port), nil)
	data := decodeMigrationBegin(t, begin)
	createNode := func(name, chassis, key string) *model.Node {
		resource, _, err := topology.store.Create(context.Background(), &model.Node{
			Name: name, ChassisID: chassis, Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true, LastSeenAt: timePointer(topology.now),
		}, key)
		if err != nil {
			t.Fatal(err)
		}
		return markReady(t, topology.store, resource).(*model.Node)
	}
	third := createNode("pve-third", "chassis-third", "compute-third-145")
	fourth := createNode("pve-fourth", "chassis-fourth", "compute-fourth-145")
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	serverC := topology.server(t, third.Name, true, nil)
	if err := serverC.clusterGate.report(third.Name, []string{third.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	bodyC := computeStartBody(145, third.Name, loadComputePort(t, topology.store, port.ID))
	bodyC["ha_managed"] = true
	addHAProof(bodyC, topology, 145, third, "HAuid145targetC", map[string]string{topology.source.Name: "unknown", topology.target.Name: "unknown", third.Name: "online"})
	topology.recon.mu.Lock()
	topology.recon.failCalls[topology.recon.forcedCalls+2] = true
	topology.recon.mu.Unlock()
	failed := request(t, serverC.ComputeHandler(), http.MethodPost, computeStartPath, bodyC, nil)
	if failed.Code != http.StatusServiceUnavailable || apiErrorCode(t, failed) != "ha_rebind_failed" {
		t.Fatalf("partial promoted C assignment status=%d body=%s", failed.Code, failed.Body.String())
	}
	operation, promoted, err := serverC.loadHAOperation(context.Background(), data.Transaction.OperationID)
	if err != nil || operation.OperationStatus != model.OperationRunning || promoted.MigrationRecovery == nil {
		t.Fatalf("partial promoted operation=%#v payload=%#v err=%v", operation, promoted, err)
	}
	audit := *promoted.MigrationRecovery
	audit.PortIDs = append([]string(nil), promoted.MigrationRecovery.PortIDs...)

	topology.recon.mu.Lock()
	for call := range topology.recon.failCalls {
		delete(topology.recon.failCalls, call)
	}
	topology.recon.mu.Unlock()
	topology.now = topology.now.Add(time.Second)
	setComputeNodeLastSeen(t, topology, third.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	setComputeNodeLastSeen(t, topology, fourth.ID, topology.now)
	serverD := topology.server(t, fourth.Name, true, nil)
	if err := serverD.clusterGate.report(fourth.Name, []string{fourth.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	bodyD := computeStartBody(145, fourth.Name, loadComputePort(t, topology.store, port.ID))
	bodyD["ha_managed"] = true
	addHAProof(bodyD, topology, 145, fourth, "HAuid145targetD", map[string]string{
		topology.source.Name: "unknown", topology.target.Name: "unknown", third.Name: "unknown", fourth.Name: "online",
	})
	finished := request(t, serverD.ComputeHandler(), http.MethodPost, computeStartPath, bodyD, nil)
	if finished.Code != http.StatusOK {
		t.Fatalf("superseding D assignment status=%d body=%s", finished.Code, finished.Body.String())
	}
	operation, finalPayload, err := serverD.loadHAOperation(context.Background(), data.Transaction.OperationID)
	if err != nil || operation.OperationStatus != model.OperationSucceeded || finalPayload.Phase != "ready" || finalPayload.TargetNodeID != fourth.ID ||
		finalPayload.MigrationRecovery == nil || !reflect.DeepEqual(*finalPayload.MigrationRecovery, audit) || len(finalPayload.AuthorityHistory) != 1 || finalPayload.AuthorityHistory[0].ServiceUID != "HAuid145targetC" {
		t.Fatalf("superseded promoted audit operation=%#v payload=%#v err=%v", operation, finalPayload, err)
	}
}

func TestHATakeoverResumesPartiallyCommittingOnlineMigration(t *testing.T) {
	for _, destination := range []string{"original target", "third target"} {
		t.Run(destination, func(t *testing.T) {
			topology := newComputeTestTopology(t)
			first := topology.port(t, 134, "net0", "02:00:00:00:00:8c")
			second := topology.port(t, 134, "net1", "02:00:00:00:00:8d")
			sourceServer := topology.server(t, topology.source.Name, false, nil)
			begin := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-ha-committing-134-"+destination, topology, true, first, second), nil)
			data := decodeMigrationBegin(t, begin)
			topology.recon.mu.Lock()
			topology.recon.failCalls[topology.recon.forcedCalls+2] = true
			topology.recon.mu.Unlock()
			partial := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, migrationFinishBody(data), nil)
			if partial.Code != http.StatusServiceUnavailable {
				t.Fatalf("partial migration finalize status=%d body=%s", partial.Code, partial.Body.String())
			}

			target := topology.target
			if destination == "third target" {
				thirdResource, _, err := topology.store.Create(context.Background(), &model.Node{Name: "pve-third", ChassisID: "chassis-third", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true, LastSeenAt: timePointer(topology.now)}, "compute-third-134")
				if err != nil {
					t.Fatal(err)
				}
				target = markReady(t, topology.store, thirdResource).(*model.Node)
				setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
			}
			setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
			server := topology.server(t, target.Name, true, nil)
			if err := server.clusterGate.report(target.Name, []string{target.Name}, true, topology.now); err != nil {
				t.Fatal(err)
			}
			start := computeStartBody(134, target.Name, loadComputePort(t, topology.store, first.ID), loadComputePort(t, topology.store, second.ID))
			start["ha_managed"] = true
			states := map[string]string{topology.source.Name: "unknown", target.Name: "online"}
			if target.ID != topology.target.ID {
				states[topology.target.Name] = "unknown"
			}
			addHAProof(start, topology, 134, target, "HAuid134"+strings.ReplaceAll(destination, " ", ""), states)
			recovered := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, start, nil)
			if recovered.Code != http.StatusOK {
				t.Fatalf("committing migration HA takeover status=%d body=%s", recovered.Code, recovered.Body.String())
			}
			for _, original := range []*model.Port{first, second} {
				current := loadComputePort(t, topology.store, original.ID)
				if current.NodeID != target.ID || current.RequestedChassis != target.ChassisID || current.Generation < original.Generation+1 {
					t.Fatalf("committing migration recovered port=%#v", current)
				}
			}
			loadPromotedMigrationHA(t, server, topology.store, data.Transaction.OperationID, "target", "committing")
		})
	}
}

func TestHATakeoverRestoresSourceDirectedMigrationClaimsBeforeRelocation(t *testing.T) {
	for _, phase := range []string{"preparing", "compensating", "aborting"} {
		t.Run(phase, func(t *testing.T) {
			topology := newComputeTestTopology(t)
			port := topology.port(t, 138, "net0", "02:00:00:00:00:91")
			sourceServer := topology.server(t, topology.source.Name, false, nil)
			begin := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-ha-source-138-"+phase, topology, true, port), nil)
			data := decodeMigrationBegin(t, begin)
			if err := sourceServer.claimMigrationOperation(context.Background(), data.Transaction.OperationID, []string{"prepared"}, phase); err != nil {
				t.Fatal(err)
			}
			setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
			server := topology.server(t, topology.target.Name, true, nil)
			if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
				t.Fatal(err)
			}
			body := computeStartBody(138, topology.target.Name, loadComputePort(t, topology.store, port.ID))
			body["ha_managed"] = true
			addHAProof(body, topology, 138, topology.target, "HAuid138"+phase, map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
			recovered := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
			if recovered.Code != http.StatusOK {
				t.Fatalf("%s HA source recovery status=%d body=%s", phase, recovered.Code, recovered.Body.String())
			}
			current := loadComputePort(t, topology.store, port.ID)
			if current.NodeID != topology.target.ID || current.RequestedChassis != topology.target.ChassisID || current.Revision != port.Revision+3 || current.Generation != port.Generation+2 {
				t.Fatalf("%s HA source recovery port=%#v", phase, current)
			}
			loadPromotedMigrationHA(t, server, topology.store, data.Transaction.OperationID, "source", phase)
		})
	}
}

func TestHAAuthorityHistoryRequiresStrictlyNewerDifferentServiceUID(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 139, "net0", "02:00:00:00:00:92")
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	server := topology.server(t, topology.target.Name, true, nil)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	firstBody := computeStartBody(139, topology.target.Name, port)
	firstBody["ha_managed"] = true
	addHAProof(firstBody, topology, 139, topology.target, "HAuid139first", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	first := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, firstBody, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first HA assignment status=%d body=%s", first.Code, first.Body.String())
	}
	before := loadComputePort(t, topology.store, port.ID)
	olderBody := computeStartBody(139, topology.target.Name, before)
	olderBody["ha_managed"] = true
	addHAProof(olderBody, topology, 139, topology.target, "HAuid139older", map[string]string{topology.target.Name: "online"})
	olderProof := olderBody["ha_proof"].(computeHAProof)
	olderProof.ManagerEpoch--
	olderBody["ha_proof"] = olderProof
	older := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, olderBody, nil)
	if older.Code != http.StatusConflict || apiErrorCode(t, older) != "ha_assignment_not_newer" {
		t.Fatalf("older different-UID assignment status=%d body=%s", older.Code, older.Body.String())
	}
	if after := loadComputePort(t, topology.store, port.ID); after.Revision != before.Revision || after.Generation != before.Generation {
		t.Fatalf("older assignment mutated port=%#v", after)
	}
	olderID := computeResourceOperationID(computeHAAction, olderBody["lifecycle_id"].(string), 139)
	if _, err := topology.store.Get(context.Background(), model.KindOperation, olderID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("older assignment created operation: %v", err)
	}

	topology.now = topology.now.Add(time.Second)
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	newerBody := computeStartBody(139, topology.target.Name, before)
	newerBody["ha_managed"] = true
	addHAProof(newerBody, topology, 139, topology.target, "HAuid139newer", map[string]string{topology.target.Name: "online"})
	newer := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, newerBody, nil)
	if newer.Code != http.StatusOK {
		t.Fatalf("strictly newer different-UID assignment status=%d body=%s", newer.Code, newer.Body.String())
	}
}

func TestHASupersededServiceUIDCannotBeResurrected(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 144, "net0", "02:00:00:00:00:96")
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	serverB := topology.server(t, topology.target.Name, true, nil)
	if err := serverB.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	bodyA := computeStartBody(144, topology.target.Name, port)
	bodyA["ha_managed"] = true
	addHAProof(bodyA, topology, 144, topology.target, "HAuid144assignmentA", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	if response := request(t, serverB.ComputeHandler(), http.MethodPost, computeStartPath, bodyA, nil); response.Code != http.StatusOK {
		t.Fatalf("initial A assignment status=%d body=%s", response.Code, response.Body.String())
	}

	topology.now = topology.now.Add(time.Second)
	thirdResource, _, err := topology.store.Create(context.Background(), &model.Node{
		Name: "pve-third", ChassisID: "chassis-third", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true, LastSeenAt: timePointer(topology.now),
	}, "compute-third-144")
	if err != nil {
		t.Fatal(err)
	}
	third := markReady(t, topology.store, thirdResource).(*model.Node)
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	serverC := topology.server(t, third.Name, true, nil)
	if err := serverC.clusterGate.report(third.Name, []string{third.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	bodyB := computeStartBody(144, third.Name, loadComputePort(t, topology.store, port.ID))
	bodyB["ha_managed"] = true
	addHAProof(bodyB, topology, 144, third, "HAuid144assignmentB", map[string]string{topology.target.Name: "unknown", third.Name: "online"})
	if response := request(t, serverC.ComputeHandler(), http.MethodPost, computeStartPath, bodyB, nil); response.Code != http.StatusOK {
		t.Fatalf("intervening B assignment status=%d body=%s", response.Code, response.Body.String())
	}

	topology.now = topology.now.Add(time.Second)
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now)
	setComputeNodeLastSeen(t, topology, third.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	serverAReplay := topology.server(t, topology.target.Name, true, nil)
	if err := serverAReplay.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	resurrectedBody := computeStartBody(144, topology.target.Name, loadComputePort(t, topology.store, port.ID))
	resurrectedBody["ha_managed"] = true
	addHAProof(resurrectedBody, topology, 144, topology.target, "HAuid144assignmentA", map[string]string{topology.target.Name: "online", third.Name: "unknown"})
	before := loadComputePort(t, topology.store, port.ID)
	rejected := request(t, serverAReplay.ComputeHandler(), http.MethodPost, computeStartPath, resurrectedBody, nil)
	if rejected.Code != http.StatusConflict || apiErrorCode(t, rejected) != "ha_service_uid_reused" {
		t.Fatalf("resurrected HA service UID status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if after := loadComputePort(t, topology.store, port.ID); after.Revision != before.Revision || after.Generation != before.Generation || after.NodeID != before.NodeID {
		t.Fatalf("rejected resurrected UID mutated port=%#v before=%#v", after, before)
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

func TestConcurrentSameDirectionMigrationFinalizeSerializesLocally(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 133, "net0", "02:00:00:00:00:87")
	second := topology.port(t, 133, "net1", "02:00:00:00:00:88")
	firstServer := topology.server(t, topology.source.Name, false, nil)
	secondServer := topology.server(t, topology.source.Name, false, nil)
	begin := request(t, firstServer.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-133", topology, true, first, second), nil)
	finish := migrationFinishBody(decodeMigrationBegin(t, begin))

	responses := make(chan *httptest.ResponseRecorder, 2)
	var wait sync.WaitGroup
	for _, server := range []*Server{firstServer, secondServer} {
		wait.Add(1)
		go func(server *Server) {
			defer wait.Done()
			responses <- request(t, server.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, finish, nil)
		}(server)
	}
	wait.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("same-direction migration finalize status=%d body=%s", response.Code, response.Body.String())
		}
	}

	operation := loadComputeOperation(t, topology.store, finish.Transaction.OperationID)
	payload, err := decodeMigrationPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationSucceeded || payload.Phase != "finalized" || operation.TargetID == computeVMOperationTarget(133) {
		t.Fatalf("same-direction migration terminal=%#v payload=%#v err=%v", operation, payload, err)
	}
	for _, original := range []*model.Port{first, second} {
		current := loadComputePort(t, topology.store, original.ID)
		if current.NodeID != topology.target.ID || current.RequestedChassis != topology.target.ChassisID || current.Generation != original.Generation+1 {
			t.Fatalf("same-direction migration port=%#v", current)
		}
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

func TestOfflineMigrationFinalizeAcceptsExactPreparedTargetState(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 132, "net0", "02:00:00:00:00:86")
	server := topology.server(t, topology.source.Name, false, nil)
	body := migrationBeginBody("migration-offline-finalize", topology, false, port)
	body["source_stopped"] = true
	begin := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	data := decodeMigrationBegin(t, begin)
	prepared := loadComputePort(t, topology.store, port.ID)
	if prepared.NodeID != topology.target.ID || prepared.RequestedChassis != topology.target.ChassisID || prepared.Revision != data.Transaction.Ports[0].PreparedRevision {
		t.Fatalf("offline prepared port=%#v transaction=%#v", prepared, data.Transaction.Ports[0])
	}
	finished := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, migrationFinishBody(data), nil)
	if finished.Code != http.StatusOK {
		t.Fatalf("offline finalize status=%d body=%s", finished.Code, finished.Body.String())
	}
	finalized := loadComputePort(t, topology.store, port.ID)
	if finalized.Revision != data.Transaction.Ports[0].PreparedRevision || finalized.NodeID != topology.target.ID || finalized.RequestedChassis != topology.target.ChassisID {
		t.Fatalf("offline finalize performed an unexpected second write: %#v", finalized)
	}
}

func TestMigrationAcceptsOnlyExactRuntimeBoundRevisionAdvances(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 136, "net0", "02:00:00:00:00:8f")
	sourceServer := topology.server(t, topology.source.Name, false, nil)
	targetServer := topology.server(t, topology.target.Name, false, nil)
	begin := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-runtime-bound-136", topology, true, port), nil)
	data := decodeMigrationBegin(t, begin)
	preparedBound := reportComputePortBound(t, topology, port.ID, "migration-prepared-bound-136")
	targetBody := computeStartBody(136, topology.target.Name, preparedBound)
	targetBody["migration_source"] = topology.source.Name
	started := request(t, targetServer.ComputeHandler(), http.MethodPost, computeStartPath, targetBody, nil)
	if started.Code != http.StatusOK {
		t.Fatalf("runtime-bound prepared target start status=%d body=%s", started.Code, started.Body.String())
	}
	topology.recon.mu.Lock()
	topology.recon.failCalls[topology.recon.forcedCalls+1] = true
	topology.recon.mu.Unlock()
	interrupted := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, migrationFinishBody(data), nil)
	if interrupted.Code != http.StatusServiceUnavailable || apiErrorCode(t, interrupted) != "migration_finish_failed" {
		t.Fatalf("runtime-bound interrupted finalize status=%d body=%s", interrupted.Code, interrupted.Body.String())
	}
	finished := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, migrationFinishBody(data), nil)
	if finished.Code != http.StatusOK {
		t.Fatalf("runtime-bound Binding/Pending finalize retry status=%d body=%s", finished.Code, finished.Body.String())
	}
	finalBound := reportComputePortBound(t, topology, port.ID, "migration-final-bound-136")
	replayed := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeMigrationFinalPath, migrationFinishBody(data), nil)
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("runtime-bound finalize replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	if current := loadComputePort(t, topology.store, port.ID); current.Revision != finalBound.Revision || current.Generation != finalBound.Generation || current.BindingStatus != model.PortBound {
		t.Fatalf("final runtime report changed during replay: %#v", current)
	}

	for _, drift := range []string{"immutable", "generation", "node"} {
		t.Run(drift, func(t *testing.T) {
			driftTopology := newComputeTestTopology(t)
			driftPort := driftTopology.port(t, 137, "net0", "02:00:00:00:00:90")
			begin := request(t, driftTopology.server(t, driftTopology.source.Name, false, nil).ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-runtime-drift-137-"+drift, driftTopology, true, driftPort), nil)
			decodeMigrationBegin(t, begin)
			current := reportComputePortBound(t, driftTopology, driftPort.ID, "migration-drift-bound-137-"+drift)
			desired := clonePort(current)
			desired.Metadata = model.Metadata{ID: current.ID}
			switch drift {
			case "immutable":
				desired.AdminStateUp = false
			case "generation":
				desired.Generation++
			case "node":
				desired.NodeID = driftTopology.target.ID
			}
			updated, _, err := driftTopology.store.Update(context.Background(), desired, current.Revision, "migration-drift-137-"+drift)
			if err != nil {
				t.Fatal(err)
			}
			updatedPort := markReady(t, driftTopology.store, updated).(*model.Port)
			body := computeStartBody(137, driftTopology.target.Name, updatedPort)
			body["migration_source"] = driftTopology.source.Name
			rejected := request(t, driftTopology.server(t, driftTopology.target.Name, false, nil).ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
			if rejected.Code != http.StatusConflict || apiErrorCode(t, rejected) != "migration_intent_mismatch" {
				t.Fatalf("%s drift target start status=%d body=%s", drift, rejected.Code, rejected.Body.String())
			}
		})
	}
}

func TestMigrationReplayCompensationRefusesEditedPreparedPort(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 133, "net0", "02:00:00:00:00:87")
	server := topology.server(t, topology.source.Name, false, nil)
	body := migrationBeginBody("migration-edited-prepared", topology, true, port)
	begin := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	data := decodeMigrationBegin(t, begin)
	prepared := loadComputePort(t, topology.store, port.ID)
	edited := clonePort(prepared)
	edited.Metadata = model.Metadata{ID: prepared.ID}
	edited.AdminStateUp = false
	updated, _, err := topology.store.Update(context.Background(), edited, prepared.Revision, "external-migration-edit")
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, updated)

	replay := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, body, nil)
	if replay.Code != http.StatusServiceUnavailable || apiErrorCode(t, replay) != "migration_prepare_failed" || !computeRecoveryRequired(t, replay) {
		t.Fatalf("edited prepared replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	preserved := loadComputePort(t, topology.store, port.ID)
	if preserved.AdminStateUp || preserved.NodeID != topology.source.ID || preserved.RequestedChassis != topology.source.ChassisID+","+topology.target.ChassisID {
		t.Fatalf("compensation overwrote edited prepared port: %#v", preserved)
	}
	operation := loadComputeOperation(t, topology.store, data.Transaction.OperationID)
	payload, err := decodeMigrationPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationRunning || payload.Phase != "compensating" || operation.Error == "" {
		t.Fatalf("edited compensation intent=%#v payload=%#v err=%v", operation, payload, err)
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
	if err != nil || operation.OperationStatus != model.OperationRunning || payload.Phase != "compensating" || operation.Error == "" || operation.TargetID != computeVMOperationTarget(142) {
		t.Fatalf("recovery operation=%#v payload=%#v err=%v", operation, payload, err)
	}
	delete(topology.recon.failCalls, 2)
	retried := request(t, server.ComputeHandler(), http.MethodPost, computeMigrationBeginPath, migrationBeginBody("migration-recovery", topology, true, port), nil)
	if retried.Code != http.StatusServiceUnavailable || computeRecoveryRequired(t, retried) {
		t.Fatalf("compensation retry status=%d body=%s", retried.Code, retried.Body.String())
	}
	operation = loadComputeOperation(t, topology.store, operationID)
	payload, err = decodeMigrationPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationFailed || payload.Phase != "compensated" || operation.TargetID == computeVMOperationTarget(142) {
		t.Fatalf("recovered operation=%#v payload=%#v err=%v", operation, payload, err)
	}
}

func TestHARebindRequiresManagedVMAndCompleteStableMembershipFence(t *testing.T) {
	tests := []struct {
		name        string
		haManaged   bool
		online      []string
		stale       bool
		sourceState string
		wantCode    string
		wantStatus  int
	}{
		{name: "not HA managed", online: []string{"pve-target"}, stale: true, wantCode: "wrong_chassis", wantStatus: http.StatusConflict},
		{name: "target absent", haManaged: true, online: []string{"pve-source"}, stale: true, wantCode: "ha_target_offline", wantStatus: http.StatusConflict},
		{name: "source online", haManaged: true, online: []string{"pve-source", "pve-target"}, stale: true, wantCode: "ha_source_online", wantStatus: http.StatusConflict},
		{name: "source heartbeat fresh", haManaged: true, online: []string{"pve-target"}, wantCode: "ha_source_not_stale", wantStatus: http.StatusConflict},
		{name: "CRM fence transitional state", haManaged: true, online: []string{"pve-target"}, stale: true, sourceState: "fence", wantCode: "ha_source_not_fenced", wantStatus: http.StatusConflict},
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
			body["ha_managed"] = test.haManaged
			if test.haManaged {
				sourceState := test.sourceState
				if sourceState == "" {
					sourceState = "unknown"
				}
				addHAProof(body, topology, 150, topology.target, "HAuid15000000", map[string]string{topology.source.Name: sourceState, topology.target.Name: "online"})
			}
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

func TestHARebindResumesMixedSourceTargetStateWithoutDoubleIncrement(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 151, "net0", "02:00:00:00:00:97")
	second := topology.port(t, 151, "net1", "02:00:00:00:00:98")
	firstTarget := moveComputePort(t, topology, first, topology.target, first.Generation+1)
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	server := topology.server(t, topology.target.Name, true, nil)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(151, topology.target.Name, firstTarget, second)
	body["ha_managed"] = true
	addHAProof(body, topology, 151, topology.target, "HAuid15100000", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	response := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("mixed HA resume status=%d body=%s", response.Code, response.Body.String())
	}
	resumedFirst := loadComputePort(t, topology.store, first.ID)
	resumedSecond := loadComputePort(t, topology.store, second.ID)
	if resumedFirst.Generation != firstTarget.Generation || resumedSecond.Generation != second.Generation+1 || resumedFirst.NodeID != topology.target.ID || resumedSecond.NodeID != topology.target.ID {
		t.Fatalf("mixed HA generations first=%#v second=%#v", resumedFirst, resumedSecond)
	}
}

func TestHAProofGatesEveryStartAndAcceptsOnlyNondecreasingSameUIDReplay(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 152, "net0", "02:00:00:00:00:99")
	port = moveComputePort(t, topology, port, topology.target, port.Generation+1)
	server := topology.server(t, topology.target.Name, true, nil)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(152, topology.target.Name, port)
	body["ha_managed"] = true
	missing := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if missing.Code != http.StatusConflict || apiErrorCode(t, missing) != "ha_proof_required" {
		t.Fatalf("HA start without proof status=%d body=%s", missing.Code, missing.Body.String())
	}
	addHAProof(body, topology, 152, topology.target, "HAuid15200000", map[string]string{topology.target.Name: "online"})
	body["lifecycle_id"] = "pve-ha-wrong"
	wrongID := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if wrongID.Code != http.StatusConflict || apiErrorCode(t, wrongID) != "ha_lifecycle_mismatch" {
		t.Fatalf("noncanonical HA lifecycle status=%d body=%s", wrongID.Code, wrongID.Body.String())
	}
	addHAProof(body, topology, 152, topology.target, "HAuid15200000", map[string]string{topology.target.Name: "online"})
	first := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first HA proof start status=%d body=%s", first.Code, first.Body.String())
	}
	operationID := computeResourceOperationID(computeHAAction, body["lifecycle_id"].(string), 152)
	firstOperation := loadComputeOperation(t, topology.store, operationID)
	if firstOperation.OperationStatus != model.OperationSucceeded || firstOperation.TargetID == computeVMOperationTarget(152) {
		t.Fatalf("HA proof operation did not archive: %#v", firstOperation)
	}
	currentBound := loadComputePort(t, topology.store, port.ID)
	bound := clonePort(currentBound)
	bound.Metadata = model.Metadata{ID: port.ID}
	bound.BindingStatus = model.PortBound
	boundResource, _, err := topology.store.Update(context.Background(), bound, currentBound.Revision, "compute-agent-bound-152")
	if err != nil {
		t.Fatal(err)
	}
	bound = markReady(t, topology.store, boundResource).(*model.Port)

	oldBody := maps.Clone(body)
	topology.now = topology.now.Add(10 * time.Second)
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	addHAProof(body, topology, 152, topology.target, "HAuid15200000", map[string]string{topology.target.Name: "online"})
	replayed := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if replayed.Code != http.StatusOK {
		t.Fatalf("newer same-UID HA proof replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	if current := loadComputePort(t, topology.store, port.ID); current.Revision != bound.Revision || current.Generation != bound.Generation || current.BindingStatus != model.PortBound {
		t.Fatalf("bound agent report was not accepted as an exact HA replay state: %#v", current)
	}
	regressed := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, oldBody, nil)
	if regressed.Code != http.StatusConflict || apiErrorCode(t, regressed) != "ha_proof_regressed" {
		t.Fatalf("regressed HA proof status=%d body=%s", regressed.Code, regressed.Body.String())
	}
}

func TestHARebindFailureStaysTargetDirectedAndSameProofResumes(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 153, "net0", "02:00:00:00:00:9a")
	second := topology.port(t, 153, "net1", "02:00:00:00:00:9b")
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	server := topology.server(t, topology.target.Name, true, nil)
	if err := server.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	body := computeStartBody(153, topology.target.Name, first, second)
	body["ha_managed"] = true
	addHAProof(body, topology, 153, topology.target, "HAuid15300000", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	topology.recon.failCalls[1] = true
	failed := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if failed.Code != http.StatusServiceUnavailable || apiErrorCode(t, failed) != "ha_rebind_failed" {
		t.Fatalf("HA partial failure status=%d body=%s", failed.Code, failed.Body.String())
	}
	moved := loadComputePort(t, topology.store, first.ID)
	remaining := loadComputePort(t, topology.store, second.ID)
	if moved.NodeID != topology.target.ID || remaining.NodeID != topology.source.ID {
		t.Fatalf("HA failure was compensated or over-mutated: first=%#v second=%#v", moved, remaining)
	}
	advanced := clonePort(moved)
	advanced.Metadata = model.Metadata{ID: moved.ID}
	advanced.BindingStatus = model.PortBinding
	advancedResource, _, err := topology.store.Update(context.Background(), advanced, moved.Revision, "ha-manager-binding-retry-153")
	if err != nil {
		t.Fatal(err)
	}
	advanced = advancedResource.(*model.Port)
	if advanced.State != model.ResourcePending || advanced.AppliedRevision == advanced.Revision {
		t.Fatalf("HA retry fixture is not a higher Binding/Pending manager state: %#v", advanced)
	}
	operationID := computeResourceOperationID(computeHAAction, body["lifecycle_id"].(string), 153)
	operation := loadComputeOperation(t, topology.store, operationID)
	payload, err := decodeHAPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationRunning || payload.Phase != "rebinding" || operation.Error == "" || operation.TargetID != computeVMOperationTarget(153) {
		t.Fatalf("partial HA operation=%#v payload=%#v err=%v", operation, payload, err)
	}
	delete(topology.recon.failCalls, 1)
	retried := request(t, server.ComputeHandler(), http.MethodPost, computeStartPath, body, nil)
	if retried.Code != http.StatusOK {
		t.Fatalf("HA partial replay status=%d body=%s", retried.Code, retried.Body.String())
	}
	for _, original := range []*model.Port{first, second} {
		current := loadComputePort(t, topology.store, original.ID)
		if current.NodeID != topology.target.ID || current.RequestedChassis != topology.target.ChassisID || current.Generation != original.Generation+1 {
			t.Fatalf("resumed HA port=%#v", current)
		}
	}
}

func TestHANewerFencedAssignmentSupersedesPartialPriorTarget(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 154, "net0", "02:00:00:00:00:9c")
	second := topology.port(t, 154, "net1", "02:00:00:00:00:9d")
	setComputeNodeLastSeen(t, topology, topology.source.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	serverB := topology.server(t, topology.target.Name, true, nil)
	if err := serverB.clusterGate.report(topology.target.Name, []string{topology.target.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	bodyB := computeStartBody(154, topology.target.Name, first, second)
	bodyB["ha_managed"] = true
	addHAProof(bodyB, topology, 154, topology.target, "HAuid154targetB", map[string]string{topology.source.Name: "unknown", topology.target.Name: "online"})
	topology.recon.failCalls[1] = true
	failedB := request(t, serverB.ComputeHandler(), http.MethodPost, computeStartPath, bodyB, nil)
	if failedB.Code != http.StatusServiceUnavailable {
		t.Fatalf("partial B relocation status=%d body=%s", failedB.Code, failedB.Body.String())
	}
	// Keep the second port at its original source revision: the superseding
	// target revision must be based on this observed state, not on the older
	// transaction's unrealized target-revision prediction.

	topology.now = topology.now.Add(time.Second)
	thirdResource, _, err := topology.store.Create(context.Background(), &model.Node{Name: "pve-third", ChassisID: "chassis-third", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true, LastSeenAt: timePointer(topology.now)}, "compute-third")
	if err != nil {
		t.Fatal(err)
	}
	third := markReady(t, topology.store, thirdResource).(*model.Node)
	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now.Add(-2*time.Minute-haStabilizationDelay-time.Second))
	delete(topology.recon.failCalls, 1)
	serverC := topology.server(t, third.Name, true, nil)
	if err := serverC.clusterGate.report(third.Name, []string{third.Name}, true, topology.now); err != nil {
		t.Fatal(err)
	}
	currentFirst := loadComputePort(t, topology.store, first.ID)
	currentSecond := loadComputePort(t, topology.store, second.ID)
	bodyC := computeStartBody(154, third.Name, currentFirst, currentSecond)
	bodyC["ha_managed"] = true
	addHAProof(bodyC, topology, 154, third, "HAuid154targetC", map[string]string{topology.source.Name: "unknown", topology.target.Name: "fence", third.Name: "online"})
	incomplete := request(t, serverC.ComputeHandler(), http.MethodPost, computeStartPath, bodyC, nil)
	if incomplete.Code != http.StatusConflict || apiErrorCode(t, incomplete) != "ha_source_not_fenced" {
		t.Fatalf("incompletely fenced C supersession status=%d body=%s", incomplete.Code, incomplete.Body.String())
	}
	if afterFirst, afterSecond := loadComputePort(t, topology.store, first.ID), loadComputePort(t, topology.store, second.ID); afterFirst.Revision != currentFirst.Revision || afterSecond.Revision != currentSecond.Revision {
		t.Fatalf("rejected C supersession mutated ports: first=%#v second=%#v", afterFirst, afterSecond)
	}
	addHAProof(bodyC, topology, 154, third, "HAuid154targetC", map[string]string{topology.source.Name: "unknown", topology.target.Name: "unknown", third.Name: "online"})
	finishedC := request(t, serverC.ComputeHandler(), http.MethodPost, computeStartPath, bodyC, nil)
	if finishedC.Code != http.StatusOK {
		t.Fatalf("superseding C relocation status=%d body=%s", finishedC.Code, finishedC.Body.String())
	}
	for _, original := range []*model.Port{first, second} {
		current := loadComputePort(t, topology.store, original.ID)
		if current.NodeID != third.ID || current.RequestedChassis != third.ChassisID {
			t.Fatalf("superseded HA port=%#v", current)
		}
	}
	oldID := computeResourceOperationID(computeHAAction, bodyB["lifecycle_id"].(string), 154)
	oldOperation := loadComputeOperation(t, topology.store, oldID)
	oldPayload, err := decodeHAPayload(oldOperation)
	if err != nil || oldOperation.OperationStatus != model.OperationSucceeded || oldPayload.Phase != "ready" || oldPayload.TargetNodeID != third.ID || oldPayload.Proof.ServiceUID != "HAuid154targetC" || oldOperation.TargetID == computeVMOperationTarget(154) || len(oldPayload.AuthorityHistory) != 1 || oldPayload.AuthorityHistory[0].ServiceUID != "HAuid154targetB" {
		t.Fatalf("old HA operation=%#v payload=%#v err=%v", oldOperation, oldPayload, err)
	}
	staleProof := bodyB["ha_proof"].(computeHAProof)
	staleInput := computeStartRequest{
		LifecycleID: bodyB["lifecycle_id"].(string), VMID: 154, Node: topology.target.Name,
		NICs: []computeNIC{{NIC: first.NIC, MACAddress: first.MACAddress}, {NIC: second.NIC, MACAddress: second.MACAddress}}, HAManaged: true, HAProof: &staleProof,
	}
	beforeClaim := oldOperation.Revision
	if _, _, err := serverB.claimHAStartOperation(context.Background(), oldID, staleInput, topology.target, staleProof); computeLifecycleErrorCode(err) != "ha_lifecycle_conflict" {
		t.Fatalf("stale prior owner reclaimed superseded HA row: %v", err)
	}
	if after := loadComputeOperation(t, topology.store, oldID); after.Revision != beforeClaim {
		t.Fatalf("rejected stale HA claim mutated operation=%#v", after)
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

func addHAProof(body map[string]any, topology *computeTestTopology, vmid int, target *model.Node, serviceUID string, nodeStates map[string]string) {
	body["lifecycle_id"] = computeHALifecycleID(vmid, target.Name, serviceUID)
	body["ha_proof"] = computeHAProof{
		Origin: "ha", ServiceID: "vm:" + fmt.Sprint(vmid), ManagerEpoch: topology.now.Unix(), ServiceUID: serviceUID,
		ServiceNode: target.Name, ServiceState: "started", NodeStates: nodeStates, LRMNode: target.Name,
		LRMEpoch: topology.now.Unix(), LRMState: "active", LRMMode: "active", AgentLockEpoch: topology.now.Unix(),
	}
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

func computeLifecycleErrorCode(err error) string {
	var lifecycle *computeLifecycleError
	if errors.As(err, &lifecycle) {
		return lifecycle.code
	}
	return ""
}

func loadPromotedMigrationHA(t *testing.T, server *Server, store controlstore.Store, id, direction, originalPhase string) (*model.Operation, computeHAPayload) {
	t.Helper()
	operation, payload, err := server.loadHAOperation(context.Background(), id)
	if err != nil || operation.Action != computeHAAction || operation.OperationStatus != model.OperationSucceeded || payload.Phase != "ready" ||
		payload.MigrationRecovery == nil || payload.MigrationRecovery.OperationID != id || payload.MigrationRecovery.Direction != direction ||
		payload.MigrationRecovery.OriginalPhase != originalPhase || operation.TargetID == computeVMOperationTarget(payload.VMID) {
		t.Fatalf("promoted migration HA operation=%#v payload=%#v err=%v", operation, payload, err)
	}
	history := loadComputeOperation(t, store, payload.MigrationRecovery.HistoryOperationID)
	original, err := decodeMigrationPayload(history)
	if err != nil || history.Action != computeMigrationAction || history.OperationStatus != model.OperationFailed || original.Phase != originalPhase ||
		migrationPayloadHash(original) != payload.MigrationRecovery.PayloadHash {
		t.Fatalf("migration HA audit history=%#v payload=%#v err=%v", history, original, err)
	}
	return operation, payload
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

func reportComputePortBound(t *testing.T, topology *computeTestTopology, id, key string) *model.Port {
	t.Helper()
	current := loadComputePort(t, topology.store, id)
	desired := clonePort(current)
	desired.Metadata = model.Metadata{ID: current.ID}
	desired.BindingStatus = model.PortBound
	updated, _, err := topology.store.Update(context.Background(), desired, current.Revision, key)
	if err != nil {
		t.Fatal(err)
	}
	return markReady(t, topology.store, updated).(*model.Port)
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
	updated, _, err := topology.store.Update(context.Background(), node, node.Revision, fmt.Sprintf("set-compute-last-seen-%s-%d", id, observed.UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, updated)
}

func timePointer(value time.Time) *time.Time { return &value }
