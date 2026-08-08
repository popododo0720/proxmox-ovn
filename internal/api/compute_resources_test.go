package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestComputeClonePrepareRemoteTargetReplayExactAndTerminalFence(t *testing.T) {
	topology := newComputeTestTopology(t)
	source := topology.port(t, 200, "net0", "02:00:00:00:00:c8")
	server := topology.server(t, topology.source.Name, false, errors.New("agent intentionally unhealthy"))
	body := clonePrepareBody("clone-200", topology, 200, 201, source)
	first := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, body, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("clone prepare status=%d body=%s", first.Code, first.Body.String())
	}
	transaction := decodeData[computeCloneTransaction](t, first)
	if transaction.SourceNode != topology.source.Name || transaction.TargetNode != topology.target.Name || len(transaction.Ports) != 1 {
		t.Fatalf("incomplete clone transaction: %#v", transaction)
	}
	owned := loadComputePort(t, topology.store, transaction.Ports[0].PortID)
	if owned.VMID != 201 || owned.NodeID != topology.target.ID || owned.RequestedChassis != topology.target.ChassisID || owned.BindingStatus != model.PortBinding || owned.State != model.ResourceReady {
		t.Fatalf("clone port was not bound and realized on remote target: %#v", owned)
	}
	operation := loadComputeOperation(t, topology.store, transaction.OperationID)
	if operation.TargetKind != model.KindNode || operation.TargetID != computeVMOperationTarget(201) || operation.TargetRevision != 1 || operation.OperationStatus != model.OperationRunning {
		t.Fatalf("clone operation must own the target VM lifecycle slot: %#v", operation)
	}

	setComputeNodeLastSeen(t, topology, topology.target.ID, topology.now.Add(-3*time.Minute))
	replayed := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, body, nil)
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotency-Replayed") != "true" || !reflect.DeepEqual(transaction, decodeData[computeCloneTransaction](t, replayed)) {
		t.Fatalf("stale-target response-loss replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	conflicting := clonePrepareBody("clone-200", topology, 200, 201, source)
	conflicting["target_node"] = topology.source.Name
	conflict := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, conflicting, nil)
	if conflict.Code != http.StatusConflict || apiErrorCode(t, conflict) != "clone_id_conflict" {
		t.Fatalf("conflicting replay status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	committed := request(t, server.ComputeHandler(), http.MethodPost, computeCloneCommitPath, transaction, nil)
	if committed.Code != http.StatusOK {
		t.Fatalf("clone commit status=%d body=%s", committed.Code, committed.Body.String())
	}
	for attempt := 0; attempt < 3; attempt++ {
		replay := request(t, server.ComputeHandler(), http.MethodPost, computeCloneCommitPath, transaction, nil)
		if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatalf("commit replay %d status=%d body=%s", attempt, replay.Code, replay.Body.String())
		}
	}
	opposite := request(t, server.ComputeHandler(), http.MethodPost, computeCloneAbortPath, transaction, nil)
	if opposite.Code != http.StatusConflict {
		t.Fatalf("opposite terminal phase status=%d body=%s", opposite.Code, opposite.Body.String())
	}
}

func TestComputeClonePersistsAllocationAndRejectsExactSetOrOwnershipDrift(t *testing.T) {
	t.Run("allocation response loss", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		subnet := createComputeSubnet(t, topology, "10.20.0.0/24")
		source := topology.port(t, 210, "net0", "02:00:00:00:00:d2")
		attachComputeFixedIP(t, topology, source, subnet, "10.20.0.10")
		source = loadComputePort(t, topology.store, source.ID)
		server := topology.server(t, topology.source.Name, false, nil)
		body := clonePrepareBody("clone-210", topology, 210, 211, source)
		first := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, body, nil)
		transaction := decodeData[computeCloneTransaction](t, first)
		if len(transaction.Ports) != 1 || len(transaction.Ports[0].FixedIPs) != 1 || transaction.Ports[0].AllocationID == "" {
			t.Fatalf("allocated clone ownership was not persisted: %#v", transaction)
		}
		replayed := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, body, nil)
		if replayed.Code != http.StatusOK || !reflect.DeepEqual(transaction, decodeData[computeCloneTransaction](t, replayed)) {
			t.Fatalf("allocation response-loss replay status=%d body=%s", replayed.Code, replayed.Body.String())
		}
	})

	t.Run("extra target port blocks commit", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 220, "net0", "02:00:00:00:00:dc")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, clonePrepareBody("clone-220", topology, 220, 221, source), nil)
		transaction := decodeData[computeCloneTransaction](t, prepared)
		extra := topology.port(t, 221, "net9", "02:00:00:00:09:dd")
		moveComputePort(t, topology, extra, topology.target, extra.Generation)
		commit := request(t, server.ComputeHandler(), http.MethodPost, computeCloneCommitPath, transaction, nil)
		if commit.Code != http.StatusConflict || apiErrorCode(t, commit) != "clone_port_set_drift" {
			t.Fatalf("extra target port commit status=%d body=%s", commit.Code, commit.Body.String())
		}
	})

	t.Run("extra target port blocks abort before mutation", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 225, "net0", "02:00:00:00:00:e1")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, clonePrepareBody("clone-225", topology, 225, 226, source), nil)
		transaction := decodeData[computeCloneTransaction](t, prepared)
		extra := topology.port(t, 226, "net9", "02:00:00:00:09:e2")
		moveComputePort(t, topology, extra, topology.target, extra.Generation)
		abort := request(t, server.ComputeHandler(), http.MethodPost, computeCloneAbortPath, transaction, nil)
		if abort.Code != http.StatusConflict || apiErrorCode(t, abort) != "clone_port_set_drift" {
			t.Fatalf("extra target port abort status=%d body=%s", abort.Code, abort.Body.String())
		}
		if _, err := topology.store.Get(context.Background(), model.KindPort, transaction.Ports[0].PortID); err != nil {
			t.Fatalf("abort mutated owned port before exact-set fence: %v", err)
		}
	})

	t.Run("edited owned port blocks abort", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 230, "net0", "02:00:00:00:00:e6")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, clonePrepareBody("clone-230", topology, 230, 231, source), nil)
		transaction := decodeData[computeCloneTransaction](t, prepared)
		port := loadComputePort(t, topology.store, transaction.Ports[0].PortID)
		edited := clonePort(port)
		edited.Metadata = model.Metadata{ID: port.ID}
		edited.AdminStateUp = false
		updated, _, err := topology.store.Update(context.Background(), edited, port.Revision, "external-clone-edit")
		if err != nil {
			t.Fatal(err)
		}
		markReady(t, topology.store, updated)
		abort := request(t, server.ComputeHandler(), http.MethodPost, computeCloneAbortPath, transaction, nil)
		if abort.Code != http.StatusServiceUnavailable || apiErrorCode(t, abort) != "clone_abort_failed" || !computeRecoveryRequired(t, abort) {
			t.Fatalf("edited clone abort status=%d body=%s", abort.Code, abort.Body.String())
		}
		if _, err := topology.store.Get(context.Background(), model.KindPort, port.ID); err != nil {
			t.Fatalf("edited port was destructively removed: %v", err)
		}
	})

	t.Run("abort resumes an exact detached clone port", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 232, "net0", "02:00:00:00:00:e8")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, clonePrepareBody("clone-232", topology, 232, 233, source), nil)
		transaction := decodeData[computeCloneTransaction](t, prepared)
		expected := transaction.Ports[0]
		port := loadComputePort(t, topology.store, expected.PortID)
		detached := clonePort(port)
		detached.Metadata = model.Metadata{ID: port.ID}
		detached.NodeID, detached.VMID, detached.NIC, detached.RequestedChassis = "", 0, "", ""
		detached.BindingStatus, detached.Generation = model.PortUnbound, expected.DetachedGeneration
		updated, _, err := topology.store.Update(context.Background(), detached, port.Revision, "inject-clone-detach")
		if err != nil {
			t.Fatal(err)
		}
		markReady(t, topology.store, updated)
		aborted := request(t, server.ComputeHandler(), http.MethodPost, computeCloneAbortPath, transaction, nil)
		if aborted.Code != http.StatusOK {
			t.Fatalf("detached clone abort replay status=%d body=%s", aborted.Code, aborted.Body.String())
		}
		if _, err := topology.store.Get(context.Background(), model.KindPort, expected.PortID); !errors.Is(err, controlstore.ErrNotFound) {
			t.Fatalf("detached clone port remains: %v", err)
		}
	})

	t.Run("abort resumes allocation tombstone after detach", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		subnet := createComputeSubnet(t, topology, "10.20.0.0/24")
		source := topology.port(t, 234, "net0", "02:00:00:00:00:ea")
		attachComputeFixedIP(t, topology, source, subnet, "10.20.0.10")
		source = loadComputePort(t, topology.store, source.ID)
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, clonePrepareBody("clone-234", topology, 234, 235, source), nil)
		transaction := decodeData[computeCloneTransaction](t, prepared)
		expected := transaction.Ports[0]
		port := loadComputePort(t, topology.store, expected.PortID)
		detached := clonePort(port)
		detached.Metadata = model.Metadata{ID: port.ID}
		detached.NodeID, detached.VMID, detached.NIC, detached.RequestedChassis = "", 0, "", ""
		detached.BindingStatus, detached.Generation = model.PortUnbound, expected.DetachedGeneration
		updated, _, err := topology.store.Update(context.Background(), detached, port.Revision, "inject-clone-allocation-detach")
		if err != nil {
			t.Fatal(err)
		}
		markReady(t, topology.store, updated)
		deleteBaseKey := "compute-clone-prepare-delete-" + expected.OwnershipDigest
		deleteKey := deprovisionAllocationKey(deleteBaseKey, expected.PortID, expected.AllocationID)
		if _, _, err := topology.store.BeginDelete(context.Background(), model.KindIPAllocation, expected.AllocationID, expected.AllocationRevision, deleteKey); err != nil {
			t.Fatal(err)
		}
		aborted := request(t, server.ComputeHandler(), http.MethodPost, computeCloneAbortPath, transaction, nil)
		if aborted.Code != http.StatusOK {
			t.Fatalf("allocation tombstone abort replay status=%d body=%s", aborted.Code, aborted.Body.String())
		}
		for _, item := range []struct {
			kind model.Kind
			id   string
		}{{model.KindPort, expected.PortID}, {model.KindIPAllocation, expected.AllocationID}} {
			if _, err := topology.store.Get(context.Background(), item.kind, item.id); !errors.Is(err, controlstore.ErrNotFound) {
				t.Fatalf("cleanup residue %s %s: %v", item.kind, item.id, err)
			}
		}
	})
}

func TestComputeTemplateDetachAbortCommitAndCloneRecovery(t *testing.T) {
	t.Run("abort restores exact ownership", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 240, "net0", "02:00:00:00:00:f0")
		server := topology.server(t, topology.source.Name, false, errors.New("probe must not gate lifecycle cleanup"))
		body := resourcePrepareBody("template-240", 240, source)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeTemplatePreparePath, body, nil)
		transaction := decodeData[computeTemplateTransaction](t, prepared)
		detached := loadComputePort(t, topology.store, source.ID)
		if !portCanBeDeprovisioned(detached) || detached.Generation != source.Generation+1 {
			t.Fatalf("template source not detached: %#v", detached)
		}
		replay := request(t, server.ComputeHandler(), http.MethodPost, computeTemplatePreparePath, body, nil)
		if replay.Code != http.StatusOK || !reflect.DeepEqual(transaction, decodeData[computeTemplateTransaction](t, replay)) {
			t.Fatalf("template response replay status=%d body=%s", replay.Code, replay.Body.String())
		}
		aborted := request(t, server.ComputeHandler(), http.MethodPost, computeTemplateAbortPath, transaction, nil)
		if aborted.Code != http.StatusOK {
			t.Fatalf("template abort status=%d body=%s", aborted.Code, aborted.Body.String())
		}
		restored := loadComputePort(t, topology.store, source.ID)
		if restored.VMID != 240 || restored.NIC != "net0" || restored.NodeID != topology.source.ID || restored.Generation != source.Generation+2 {
			t.Fatalf("template abort did not restore ownership: %#v", restored)
		}
	})

	t.Run("template response loss recovered by proven template clone", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 250, "net0", "02:00:00:00:00:fa")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeTemplatePreparePath, resourcePrepareBody("template-250", 250, source), nil)
		if prepared.Code != http.StatusOK {
			t.Fatal(prepared.Body.String())
		}
		cloneBody := clonePrepareBody("clone-template-250", topology, 250, 251, source)
		cloneBody["source_template"] = true
		clone := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, cloneBody, nil)
		if clone.Code != http.StatusOK {
			t.Fatalf("template recovery clone status=%d body=%s", clone.Code, clone.Body.String())
		}
		if _, err := topology.store.Get(context.Background(), model.KindPort, source.ID); !errors.Is(err, controlstore.ErrNotFound) {
			t.Fatalf("recovered template source port was not deprovisioned: %v", err)
		}
		operationID := computeResourceOperationID(computeTemplateAction, "template-250", 250)
		operation := loadComputeOperation(t, topology.store, operationID)
		payload, err := decodeTemplatePayload(operation)
		if err != nil || operation.OperationStatus != model.OperationSucceeded || payload.Phase != "committed" {
			t.Fatalf("template intent was not recovered: op=%#v payload=%#v err=%v", operation, payload, err)
		}
	})

	t.Run("committing template response loss is recoverable only with template proof", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 252, "net0", "02:00:00:00:00:fc")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeTemplatePreparePath, resourcePrepareBody("template-252", 252, source), nil)
		transaction := decodeData[computeTemplateTransaction](t, prepared)
		if err := server.claimTemplateOperation(context.Background(), transaction.OperationID, "prepared", "committing"); err != nil {
			t.Fatal(err)
		}

		unproven := clonePrepareBody("clone-live-252", topology, 252, 253, source)
		blocked := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, unproven, nil)
		if blocked.Code == http.StatusOK {
			t.Fatalf("non-template clone silently consumed template blueprint: %s", blocked.Body.String())
		}
		proven := clonePrepareBody("clone-template-252", topology, 252, 253, source)
		proven["source_template"] = true
		clone := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, proven, nil)
		if clone.Code != http.StatusOK {
			t.Fatalf("committing template recovery clone status=%d body=%s", clone.Code, clone.Body.String())
		}
		operation := loadComputeOperation(t, topology.store, transaction.OperationID)
		payload, err := decodeTemplatePayload(operation)
		if err != nil || operation.OperationStatus != model.OperationSucceeded || payload.Phase != "committed" {
			t.Fatalf("committing template was not recovered: op=%#v payload=%#v err=%v", operation, payload, err)
		}
	})

	t.Run("commit rejects new VM port after detach", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 255, "net0", "02:00:00:00:00:ff")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeTemplatePreparePath, resourcePrepareBody("template-255", 255, source), nil)
		transaction := decodeData[computeTemplateTransaction](t, prepared)
		topology.port(t, 255, "net9", "02:00:00:00:09:ff")
		commit := request(t, server.ComputeHandler(), http.MethodPost, computeTemplateCommitPath, transaction, nil)
		if commit.Code != http.StatusConflict || apiErrorCode(t, commit) != "lifecycle_port_set_drift" {
			t.Fatalf("template commit exact-set status=%d body=%s", commit.Code, commit.Body.String())
		}
		if _, err := topology.store.Get(context.Background(), model.KindPort, source.ID); err != nil {
			t.Fatalf("template commit deleted source before exact-set fence: %v", err)
		}
	})
}

func TestComputeCloneSourceKindIsExplicit(t *testing.T) {
	topology := newComputeTestTopology(t)
	source := topology.port(t, 258, "net0", "02:00:00:00:01:02")
	server := topology.server(t, topology.source.Name, false, nil)
	body := clonePrepareBody("clone-template-lie-258", topology, 258, 259, source)
	body["source_template"] = true
	response := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, body, nil)
	if response.Code != http.StatusConflict || apiErrorCode(t, response) != "lifecycle_port_set_drift" {
		t.Fatalf("template proof accepted live source ports: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestComputeSnapshotImmutableManifestEpochReuseAndDeletePurpose(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 260, "net0", "02:00:00:00:01:04")
	second := topology.port(t, 260, "net1", "02:00:00:00:01:05")
	sourceServer := topology.server(t, topology.source.Name, false, nil)
	createBody := snapshotCreateBody("snapshot-260-a", 260, "daily", 1_754_605_200, first, second)
	created := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, createBody, nil)
	if created.Code != http.StatusOK {
		t.Fatalf("snapshot create status=%d body=%s", created.Code, created.Body.String())
	}
	transaction := decodeData[computeSnapshotTransaction](t, created)
	replayed := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, createBody, nil)
	if replayed.Code != http.StatusOK || !reflect.DeepEqual(transaction, decodeData[computeSnapshotTransaction](t, replayed)) {
		t.Fatalf("snapshot response replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}

	duplicateNICs := map[string]any{"lifecycle_id": "snapshot-rollback-duplicate", "action": "rollback", "vmid": 260, "snapshot_id": "daily", "snapshot_epoch": int64(1_754_605_200), "nics": []computeNIC{{NIC: first.NIC, MACAddress: first.MACAddress}, {NIC: first.NIC, MACAddress: first.MACAddress}}}
	duplicate := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeSnapshotPreparePath, duplicateNICs, nil)
	if duplicate.Code != http.StatusBadRequest {
		t.Fatalf("duplicate snapshot NIC set status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	moveComputePort(t, topology, first, topology.target, first.Generation+1)
	moveComputePort(t, topology, second, topology.target, second.Generation+1)
	targetServer := topology.server(t, topology.target.Name, false, nil)
	verifyBody := map[string]any{"lifecycle_id": "snapshot-rollback-260", "action": "rollback", "vmid": 260, "snapshot_id": "daily", "snapshot_epoch": int64(1_754_605_200), "nics": computeNICs(first, second)}
	verifiedAfterMigration := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotPreparePath, verifyBody, nil)
	if verifiedAfterMigration.Code != http.StatusOK {
		t.Fatalf("runtime relocation invalidated immutable snapshot config: %d %s", verifiedAfterMigration.Code, verifiedAfterMigration.Body.String())
	}
	rollbackTransaction := decodeData[computeSnapshotMutationTransaction](t, verifiedAfterMigration)
	currentBeforeCommit := loadComputePort(t, topology.store, first.ID)
	commitDrift := clonePort(currentBeforeCommit)
	commitDrift.Metadata = model.Metadata{ID: currentBeforeCommit.ID}
	commitDrift.AdminStateUp = false
	changed, _, err := topology.store.Update(context.Background(), commitDrift, currentBeforeCommit.Revision, "snapshot-post-prepare-drift")
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, changed)
	commitBlocked := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotCommitPath, rollbackTransaction, nil)
	if commitBlocked.Code != http.StatusConflict || apiErrorCode(t, commitBlocked) != "snapshot_port_drift" {
		t.Fatalf("post-prepare snapshot drift commit status=%d body=%s", commitBlocked.Code, commitBlocked.Body.String())
	}
	currentBeforeCommit = loadComputePort(t, topology.store, first.ID)
	restoredConfig := clonePort(currentBeforeCommit)
	restoredConfig.Metadata = model.Metadata{ID: currentBeforeCommit.ID}
	restoredConfig.AdminStateUp = true
	changed, _, err = topology.store.Update(context.Background(), restoredConfig, currentBeforeCommit.Revision, "repair-snapshot-post-prepare-drift")
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, changed)
	released := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotAbortPath, rollbackTransaction, nil)
	if released.Code != http.StatusConflict {
		t.Fatalf("opposite snapshot abort after commit claim status=%d body=%s", released.Code, released.Body.String())
	}
	committed := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotCommitPath, rollbackTransaction, nil)
	if committed.Code != http.StatusOK {
		t.Fatalf("snapshot rollback commit recovery status=%d body=%s", committed.Code, committed.Body.String())
	}
	current := loadComputePort(t, topology.store, first.ID)
	edited := clonePort(current)
	edited.Metadata = model.Metadata{ID: current.ID}
	edited.AdminStateUp = false
	updated, _, err := topology.store.Update(context.Background(), edited, current.Revision, "snapshot-static-drift")
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, updated)
	driftBody := map[string]any{"lifecycle_id": "snapshot-rollback-drift", "action": "rollback", "vmid": 260, "snapshot_id": "daily", "snapshot_epoch": int64(1_754_605_200), "nics": computeNICs(first, second)}
	rollbackVerify := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotPreparePath, driftBody, nil)
	if rollbackVerify.Code != http.StatusConflict || apiErrorCode(t, rollbackVerify) != "snapshot_port_drift" {
		t.Fatalf("static snapshot drift status=%d body=%s", rollbackVerify.Code, rollbackVerify.Body.String())
	}
	deletePreflight := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotPreparePath, map[string]any{"lifecycle_id": "snapshot-delete-260", "action": "delete", "vmid": 260, "snapshot_id": "daily", "snapshot_epoch": int64(1_754_605_200)}, nil)
	if deletePreflight.Code != http.StatusOK {
		t.Fatalf("identity-only delete preflight status=%d body=%s", deletePreflight.Code, deletePreflight.Body.String())
	}
	deleteTransaction := decodeData[computeSnapshotMutationTransaction](t, deletePreflight)
	deleted := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotCommitPath, deleteTransaction, nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("migrated snapshot delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	for attempt := 0; attempt < 3; attempt++ {
		replay := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotCommitPath, deleteTransaction, nil)
		if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatalf("snapshot delete replay %d status=%d body=%s", attempt, replay.Code, replay.Body.String())
		}
	}
	sameEpoch := snapshotCreateBody("snapshot-260-b", 260, "daily", 1_754_605_200, loadComputePort(t, topology.store, first.ID), loadComputePort(t, topology.store, second.ID))
	rejectedReuse := request(t, targetServer.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, sameEpoch, nil)
	if rejectedReuse.Code != http.StatusConflict || apiErrorCode(t, rejectedReuse) != "snapshot_epoch_conflict" {
		t.Fatalf("same-second snapshot epoch reuse status=%d body=%s", rejectedReuse.Code, rejectedReuse.Body.String())
	}
}

type failNthResourceGetStore struct {
	controlstore.Store
	mu       sync.Mutex
	kind     model.Kind
	id       string
	failAt   int
	matching int
}

func (store *failNthResourceGetStore) Get(ctx context.Context, kind model.Kind, id string) (model.Resource, error) {
	store.mu.Lock()
	if kind == store.kind && id == store.id {
		store.matching++
		if store.matching == store.failAt {
			store.mu.Unlock()
			return nil, errors.New("injected snapshot manifest lookup failure")
		}
	}
	store.mu.Unlock()
	return store.Store.Get(ctx, kind, id)
}

func TestComputeSnapshotPrepareReplayHandlesTransientManifestReloadFailure(t *testing.T) {
	topology := newComputeTestTopology(t)
	port := topology.port(t, 261, "net0", "02:00:00:00:01:06")
	server := topology.server(t, topology.source.Name, false, nil)
	epoch := int64(1_754_605_261)
	created := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, snapshotCreateBody("snapshot-261", 261, "replay", epoch, port), nil)
	if created.Code != http.StatusOK {
		t.Fatalf("snapshot create status=%d body=%s", created.Code, created.Body.String())
	}
	prepareBody := map[string]any{
		"lifecycle_id": "snapshot-rollback-261", "action": "rollback", "vmid": 261,
		"snapshot_id": "replay", "snapshot_epoch": epoch, "nics": computeNICs(port),
	}
	prepared := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotPreparePath, prepareBody, nil)
	if prepared.Code != http.StatusOK {
		t.Fatalf("snapshot prepare status=%d body=%s", prepared.Code, prepared.Body.String())
	}
	transaction := decodeData[computeSnapshotMutationTransaction](t, prepared)

	server.store = &failNthResourceGetStore{
		Store: topology.store, kind: model.KindOperation,
		id: computeSnapshotOperationID(261, "replay", epoch), failAt: 2,
	}
	failedReplay := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotPreparePath, prepareBody, nil)
	if failedReplay.Code != http.StatusInternalServerError {
		t.Fatalf("transient manifest reload status=%d body=%s", failedReplay.Code, failedReplay.Body.String())
	}
	operation := loadComputeOperation(t, topology.store, transaction.OperationID)
	payload, err := decodeSnapshotMutationPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationRunning || payload.Phase != "prepared" || operation.TargetID != computeVMOperationTarget(261) {
		t.Fatalf("transient replay lost resumable prepared claim: op=%#v payload=%#v err=%v", operation, payload, err)
	}

	replayed := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotPreparePath, prepareBody, nil)
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotency-Replayed") != "true" || !reflect.DeepEqual(transaction, decodeData[computeSnapshotMutationTransaction](t, replayed)) {
		t.Fatalf("snapshot replay did not recover after transient lookup: status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	aborted := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotAbortPath, transaction, nil)
	if aborted.Code != http.StatusOK {
		t.Fatalf("snapshot abort after replay recovery status=%d body=%s", aborted.Code, aborted.Body.String())
	}
	operation = loadComputeOperation(t, topology.store, transaction.OperationID)
	payload, err = decodeSnapshotMutationPayload(operation)
	if err != nil || operation.OperationStatus != model.OperationSucceeded || payload.Phase != "aborted" || operation.TargetID == computeVMOperationTarget(261) {
		t.Fatalf("snapshot replay recovery left VM slot stuck: op=%#v payload=%#v err=%v", operation, payload, err)
	}
}

func TestComputeSnapshotCleanupFencesMissingAndInterruptedCreate(t *testing.T) {
	t.Run("missing manifest becomes an exact deleted sentinel", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		port := topology.port(t, 262, "net0", "02:00:00:00:01:06")
		server := topology.server(t, topology.source.Name, false, nil)
		cleanupBody := map[string]any{"vmid": 262, "snapshot_id": "ambiguous", "snapshot_epoch": int64(1_754_605_262)}
		cleaned := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCleanupPath, cleanupBody, nil)
		if cleaned.Code != http.StatusOK {
			t.Fatalf("missing snapshot cleanup status=%d body=%s", cleaned.Code, cleaned.Body.String())
		}
		replay := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCleanupPath, cleanupBody, nil)
		if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatalf("snapshot cleanup replay status=%d body=%s", replay.Code, replay.Body.String())
		}
		create := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, snapshotCreateBody("snapshot-after-cleanup", 262, "ambiguous", 1_754_605_262, port), nil)
		if create.Code != http.StatusConflict || apiErrorCode(t, create) != "snapshot_epoch_conflict" {
			t.Fatalf("deleted epoch was resurrected: status=%d body=%s", create.Code, create.Body.String())
		}
	})

	t.Run("running create is durably cleaned after PVE rollback", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		port := topology.port(t, 263, "net0", "02:00:00:00:01:07")
		server := topology.server(t, topology.source.Name, false, nil)
		topology.recon.failCalls[1] = true
		epoch := int64(1_754_605_263)
		failed := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, snapshotCreateBody("snapshot-create-263", 263, "failed-create", epoch, port), nil)
		if failed.Code == http.StatusOK {
			t.Fatalf("snapshot create failure was not injected: %s", failed.Body.String())
		}
		operationID := computeSnapshotOperationID(263, "failed-create", epoch)
		operation := loadComputeOperation(t, topology.store, operationID)
		payload, err := decodeSnapshotPayload(operation)
		if err != nil || operation.OperationStatus != model.OperationRunning || payload.Phase != "creating" {
			t.Fatalf("interrupted snapshot create=%#v payload=%#v err=%v", operation, payload, err)
		}
		cleaned := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCleanupPath, map[string]any{"vmid": 263, "snapshot_id": "failed-create", "snapshot_epoch": epoch}, nil)
		if cleaned.Code != http.StatusOK {
			t.Fatalf("interrupted snapshot cleanup status=%d body=%s", cleaned.Code, cleaned.Body.String())
		}
		operation = loadComputeOperation(t, topology.store, operationID)
		payload, err = decodeSnapshotPayload(operation)
		if err != nil || operation.OperationStatus != model.OperationSucceeded || payload.Phase != "deleted" || operation.TargetID == computeVMOperationTarget(263) {
			t.Fatalf("cleaned snapshot operation=%#v payload=%#v err=%v", operation, payload, err)
		}
	})

	t.Run("wrong node cannot clean another node's callback", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		port := topology.port(t, 266, "net0", "02:00:00:00:01:0a")
		sourceServer := topology.server(t, topology.source.Name, false, nil)
		epoch := int64(1_754_605_266)
		created := request(t, sourceServer.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, snapshotCreateBody("snapshot-266", 266, "local-only-cleanup", epoch, port), nil)
		if created.Code != http.StatusOK {
			t.Fatal(created.Body.String())
		}
		operationID := computeSnapshotOperationID(266, "local-only-cleanup", epoch)
		before := loadComputeOperation(t, topology.store, operationID)
		wrong := request(t, topology.server(t, topology.target.Name, false, nil).ComputeHandler(), http.MethodPost, computeSnapshotCleanupPath, map[string]any{"vmid": 266, "snapshot_id": "local-only-cleanup", "snapshot_epoch": epoch}, nil)
		if wrong.Code != http.StatusConflict || apiErrorCode(t, wrong) != "snapshot_cleanup_wrong_node" {
			t.Fatalf("wrong-node cleanup status=%d body=%s", wrong.Code, wrong.Body.String())
		}
		after := loadComputeOperation(t, topology.store, operationID)
		if after.Revision != before.Revision || after.OperationStatus != before.OperationStatus || after.TargetID != before.TargetID || after.Payload != before.Payload {
			t.Fatalf("wrong-node cleanup mutated manifest: before=%#v after=%#v", before, after)
		}
	})
}

func TestComputeSnapshotCloneUsesImmutableBlueprintWithoutLiveAllocation(t *testing.T) {
	topology := newComputeTestTopology(t)
	subnet := createComputeSubnet(t, topology, "10.20.0.0/24")
	source := topology.port(t, 264, "net0", "02:00:00:00:01:08")
	attachComputeFixedIP(t, topology, source, subnet, "10.20.0.10")
	source = loadComputePort(t, topology.store, source.ID)
	server := topology.server(t, topology.source.Name, false, nil)
	epoch := int64(1_754_605_264)
	created := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, snapshotCreateBody("snapshot-264", 264, "old", epoch, source), nil)
	if created.Code != http.StatusOK {
		t.Fatal(created.Body.String())
	}
	allocations, err := topology.store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil || len(allocations) != 1 {
		t.Fatalf("source allocation setup=%v err=%v", allocations, err)
	}
	allocation := allocations[0].(*model.IPAllocation)
	tombstone, _, err := topology.store.BeginDelete(context.Background(), model.KindIPAllocation, allocation.ID, allocation.Revision, "remove-snapshot-source-allocation")
	if err != nil {
		t.Fatal(err)
	}
	if err := topology.store.Purge(context.Background(), model.KindIPAllocation, allocation.ID, tombstone.GetMetadata().Revision); err != nil {
		t.Fatal(err)
	}
	body := clonePrepareBody("clone-snapshot-264", topology, 264, 265, source)
	body["snapshot_id"], body["snapshot_epoch"] = "old", epoch
	clone := request(t, server.ComputeHandler(), http.MethodPost, computeClonePreparePath, body, nil)
	if clone.Code != http.StatusOK {
		t.Fatalf("snapshot blueprint clone depended on old live allocation: status=%d body=%s", clone.Code, clone.Body.String())
	}
}

func TestComputeDestroyCaptureAbortCommitAndSnapshotCleanup(t *testing.T) {
	t.Run("live abort and commit replay", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		port := topology.port(t, 270, "net0", "02:00:00:00:01:0e")
		server := topology.server(t, topology.source.Name, false, errors.New("probe ignored"))
		captureBody := destroyCaptureBody("destroy-270-abort", 270, false, nil, port)
		captured := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCapturePath, captureBody, nil)
		transaction := decodeData[computeDestroyTransaction](t, captured)
		if !portCanBeDeprovisioned(loadComputePort(t, topology.store, port.ID)) {
			t.Fatal("destroy capture did not detach live port")
		}
		aborted := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyAbortPath, transaction, nil)
		if aborted.Code != http.StatusOK || loadComputePort(t, topology.store, port.ID).VMID != 270 {
			t.Fatalf("destroy abort status=%d body=%s", aborted.Code, aborted.Body.String())
		}

		current := loadComputePort(t, topology.store, port.ID)
		commitCapture := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCapturePath, destroyCaptureBody("destroy-270-commit", 270, false, nil, current), nil)
		commitTransaction := decodeData[computeDestroyTransaction](t, commitCapture)
		committed := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCommitPath, commitTransaction, nil)
		if committed.Code != http.StatusOK {
			t.Fatalf("destroy commit status=%d body=%s", committed.Code, committed.Body.String())
		}
		if _, err := topology.store.Get(context.Background(), model.KindPort, port.ID); !errors.Is(err, controlstore.ErrNotFound) {
			t.Fatalf("destroyed port remains: %v", err)
		}
		replay := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCommitPath, commitTransaction, nil)
		if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatalf("destroy commit replay status=%d body=%s", replay.Code, replay.Body.String())
		}
	})

	t.Run("commit terminalizes captured snapshot before VMID reuse", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		port := topology.port(t, 280, "net0", "02:00:00:00:01:18")
		server := topology.server(t, topology.source.Name, false, nil)
		epoch := int64(1_754_605_280)
		created := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, snapshotCreateBody("snapshot-280", 280, "before-delete", epoch, port), nil)
		if created.Code != http.StatusOK {
			t.Fatal(created.Body.String())
		}
		capture := destroyCaptureBody("destroy-280", 280, false, []computeSnapshotIdentity{{SnapshotID: "before-delete", SnapshotEpoch: epoch}}, port)
		captured := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCapturePath, capture, nil)
		transaction := decodeData[computeDestroyTransaction](t, captured)
		committed := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCommitPath, transaction, nil)
		if committed.Code != http.StatusOK {
			t.Fatalf("snapshot destroy commit status=%d body=%s", committed.Code, committed.Body.String())
		}
		if _, err := sSnapshotPayload(topology, 280, "before-delete", epoch); err == nil {
			t.Fatal("destroyed VM snapshot remained active for VMID reuse")
		}
	})

	t.Run("new live port after capture blocks commit", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		port := topology.port(t, 285, "net0", "02:00:00:00:01:1d")
		server := topology.server(t, topology.source.Name, false, nil)
		captured := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCapturePath, destroyCaptureBody("destroy-285", 285, false, nil, port), nil)
		transaction := decodeData[computeDestroyTransaction](t, captured)
		topology.port(t, 285, "net9", "02:00:00:00:09:1d")
		commit := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCommitPath, transaction, nil)
		if commit.Code != http.StatusConflict || apiErrorCode(t, commit) != "destroy_port_set_drift" {
			t.Fatalf("destroy exact-set status=%d body=%s", commit.Code, commit.Body.String())
		}
		if _, err := topology.store.Get(context.Background(), model.KindPort, port.ID); err != nil {
			t.Fatalf("destroy mutated captured source before exact-set fence: %v", err)
		}
	})

	t.Run("snapshot-only capture rejects a hidden live port before creating an operation", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		port := topology.port(t, 286, "net0", "02:00:00:00:01:1e")
		server := topology.server(t, topology.source.Name, false, nil)
		epoch := int64(1_754_605_286)
		created := request(t, server.ComputeHandler(), http.MethodPost, computeSnapshotCreatePath, snapshotCreateBody("snapshot-286", 286, "hidden-live", epoch, port), nil)
		if created.Code != http.StatusOK {
			t.Fatal(created.Body.String())
		}
		body := destroyCaptureBody("destroy-286", 286, false, []computeSnapshotIdentity{{SnapshotID: "hidden-live", SnapshotEpoch: epoch}})
		blocked := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCapturePath, body, nil)
		if blocked.Code != http.StatusConflict || apiErrorCode(t, blocked) != "pvn_nic_set_mismatch" {
			t.Fatalf("snapshot-only hidden live capture status=%d body=%s", blocked.Code, blocked.Body.String())
		}
		operationID := computeResourceOperationID(computeDestroyAction, "destroy-286", 286)
		if _, err := topology.store.Get(context.Background(), model.KindOperation, operationID); !errors.Is(err, controlstore.ErrNotFound) {
			t.Fatalf("rejected destroy created an operation: %v", err)
		}
	})

	t.Run("template capture rejects a stray live port before creating an operation", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 287, "net0", "02:00:00:00:01:1f")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeTemplatePreparePath, resourcePrepareBody("template-287", 287, source), nil)
		templateTransaction := decodeData[computeTemplateTransaction](t, prepared)
		committed := request(t, server.ComputeHandler(), http.MethodPost, computeTemplateCommitPath, templateTransaction, nil)
		if committed.Code != http.StatusOK {
			t.Fatal(committed.Body.String())
		}
		topology.port(t, 287, "net9", "02:00:00:00:09:1f")
		blocked := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCapturePath, destroyCaptureBody("destroy-287", 287, true, nil, source), nil)
		if blocked.Code != http.StatusConflict || apiErrorCode(t, blocked) != "lifecycle_port_set_drift" {
			t.Fatalf("template stray-port destroy status=%d body=%s", blocked.Code, blocked.Body.String())
		}
		operationID := computeResourceOperationID(computeDestroyAction, "destroy-287", 287)
		if _, err := topology.store.Get(context.Background(), model.KindOperation, operationID); !errors.Is(err, controlstore.ErrNotFound) {
			t.Fatalf("rejected template destroy created an operation: %v", err)
		}
	})

	t.Run("template destroy recovers a committing template callback", func(t *testing.T) {
		topology := newComputeTestTopology(t)
		source := topology.port(t, 288, "net0", "02:00:00:00:01:20")
		server := topology.server(t, topology.source.Name, false, nil)
		prepared := request(t, server.ComputeHandler(), http.MethodPost, computeTemplatePreparePath, resourcePrepareBody("template-288", 288, source), nil)
		templateTransaction := decodeData[computeTemplateTransaction](t, prepared)
		if err := server.claimTemplateOperation(context.Background(), templateTransaction.OperationID, "prepared", "committing"); err != nil {
			t.Fatal(err)
		}
		captured := request(t, server.ComputeHandler(), http.MethodPost, computeDestroyCapturePath, destroyCaptureBody("destroy-288", 288, true, nil, source), nil)
		if captured.Code != http.StatusOK {
			t.Fatalf("committing template destroy capture status=%d body=%s", captured.Code, captured.Body.String())
		}
		templateOperation := loadComputeOperation(t, topology.store, templateTransaction.OperationID)
		templatePayload, err := decodeTemplatePayload(templateOperation)
		if err != nil || templateOperation.OperationStatus != model.OperationSucceeded || templatePayload.Phase != "committed" {
			t.Fatalf("template was not recovered before destroy: op=%#v payload=%#v err=%v", templateOperation, templatePayload, err)
		}
	})
}

func TestComputeConcurrentCloneCommitAbortHasSingleTerminalWinner(t *testing.T) {
	topology := newComputeTestTopology(t)
	source := topology.port(t, 290, "net0", "02:00:00:00:01:22")
	firstServer := topology.server(t, topology.source.Name, false, nil)
	secondServer := topology.server(t, topology.source.Name, false, nil)
	prepared := request(t, firstServer.ComputeHandler(), http.MethodPost, computeClonePreparePath, clonePrepareBody("clone-290", topology, 290, 291, source), nil)
	transaction := decodeData[computeCloneTransaction](t, prepared)
	responses := make(chan int, 2)
	var wait sync.WaitGroup
	for _, action := range []struct {
		server *Server
		path   string
	}{{firstServer, computeCloneCommitPath}, {secondServer, computeCloneAbortPath}} {
		wait.Add(1)
		go func(server *Server, path string) {
			defer wait.Done()
			responses <- request(t, server.ComputeHandler(), http.MethodPost, path, transaction, nil).Code
		}(action.server, action.path)
	}
	wait.Wait()
	close(responses)
	ok, conflict := 0, 0
	for status := range responses {
		if status == http.StatusOK {
			ok++
		} else if status == http.StatusConflict {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("concurrent terminal outcomes ok=%d conflict=%d", ok, conflict)
	}
}

func TestComputeConcurrentSameDirectionCloneCommitSerializesLocally(t *testing.T) {
	topology := newComputeTestTopology(t)
	first := topology.port(t, 292, "net0", "02:00:00:00:01:24")
	second := topology.port(t, 292, "net1", "02:00:00:00:01:25")
	firstServer := topology.server(t, topology.source.Name, false, nil)
	secondServer := topology.server(t, topology.source.Name, false, nil)
	prepared := request(t, firstServer.ComputeHandler(), http.MethodPost, computeClonePreparePath, clonePrepareBody("clone-292", topology, 292, 293, first, second), nil)
	transaction := decodeData[computeCloneTransaction](t, prepared)
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wait sync.WaitGroup
	for _, server := range []*Server{firstServer, secondServer} {
		wait.Add(1)
		go func(server *Server) {
			defer wait.Done()
			responses <- request(t, server.ComputeHandler(), http.MethodPost, computeCloneCommitPath, transaction, nil)
		}(server)
	}
	wait.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("same-direction clone commit status=%d body=%s", response.Code, response.Body.String())
		}
	}
	operation := loadComputeOperation(t, topology.store, transaction.OperationID)
	payload, err := decodeClonePayload(operation)
	if err != nil || operation.OperationStatus != model.OperationSucceeded || payload.Phase != "committed" || operation.TargetID == computeVMOperationTarget(293) {
		t.Fatalf("same-direction clone terminal=%#v payload=%#v err=%v", operation, payload, err)
	}
}

func clonePrepareBody(id string, topology *computeTestTopology, sourceVMID, targetVMID int, ports ...*model.Port) map[string]any {
	return map[string]any{"clone_id": id, "source_vmid": sourceVMID, "target_vmid": targetVMID, "source_node": topology.source.Name, "target_node": topology.target.Name, "nics": computeNICs(ports...)}
}

func resourcePrepareBody(id string, vmid int, ports ...*model.Port) map[string]any {
	return map[string]any{"lifecycle_id": id, "vmid": vmid, "nics": computeNICs(ports...)}
}

func snapshotCreateBody(id string, vmid int, snapshotID string, epoch int64, ports ...*model.Port) map[string]any {
	return map[string]any{"lifecycle_id": id, "vmid": vmid, "snapshot_id": snapshotID, "snapshot_epoch": epoch, "nics": computeNICs(ports...)}
}

func destroyCaptureBody(id string, vmid int, template bool, snapshots []computeSnapshotIdentity, ports ...*model.Port) map[string]any {
	return map[string]any{"lifecycle_id": id, "vmid": vmid, "template": template, "snapshots": snapshots, "nics": computeNICs(ports...)}
}

func computeNICs(ports ...*model.Port) []computeNIC {
	result := make([]computeNIC, 0, len(ports))
	for _, port := range ports {
		result = append(result, computeNIC{NIC: port.NIC, MACAddress: port.MACAddress})
	}
	return result
}

func createComputeSubnet(t *testing.T, topology *computeTestTopology, cidr string) *model.Subnet {
	t.Helper()
	resource, _, err := topology.store.Create(context.Background(), &model.Subnet{NetworkID: topology.network.ID, Name: "compute-subnet", CIDR: cidr, GatewayIP: "10.20.0.1", AllocationPools: []model.IPRange{{Start: "10.20.0.10", End: "10.20.0.200"}}}, "compute-subnet")
	if err != nil {
		t.Fatal(err)
	}
	return markReady(t, topology.store, resource).(*model.Subnet)
}

func attachComputeFixedIP(t *testing.T, topology *computeTestTopology, source *model.Port, subnet *model.Subnet, address string) {
	t.Helper()
	desired := clonePort(source)
	desired.Metadata = model.Metadata{ID: source.ID}
	desired.FixedIPs = []model.FixedIP{{SubnetID: subnet.ID, Address: address}}
	updated, _, err := topology.store.Update(context.Background(), desired, source.Revision, "attach-source-fixed-ip")
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, updated)
	allocation, _, err := topology.store.Create(context.Background(), &model.IPAllocation{SubnetID: subnet.ID, PortID: source.ID, Address: address, State: model.IPAllocated}, "source-allocation")
	if err != nil {
		t.Fatal(err)
	}
	markReady(t, topology.store, allocation)
}

func moveComputePort(t *testing.T, topology *computeTestTopology, source *model.Port, target *model.Node, generation int64) *model.Port {
	t.Helper()
	current := loadComputePort(t, topology.store, source.ID)
	desired := clonePort(current)
	desired.Metadata = model.Metadata{ID: current.ID}
	desired.NodeID, desired.RequestedChassis = target.ID, target.ChassisID
	desired.BindingStatus, desired.Generation = model.PortBinding, generation
	updated, _, err := topology.store.Update(context.Background(), desired, current.Revision, "move-compute-port-"+source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return markReady(t, topology.store, updated).(*model.Port)
}

func sSnapshotPayload(topology *computeTestTopology, vmid int, snapshotID string, epoch int64) (computeSnapshotPayload, error) {
	server := &Server{store: topology.store}
	return server.findSnapshotPayload(context.Background(), vmid, snapshotID, epoch)
}
