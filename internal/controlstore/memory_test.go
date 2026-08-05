package controlstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func deterministicStore() *Memory {
	var sequence atomic.Int64
	return NewMemory(
		WithClock(func() time.Time { return time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC) }),
		WithIDGenerator(func() string { return fmt.Sprintf("id-%03d", sequence.Add(1)) }),
	)
}

func mustCreate(t *testing.T, store Store, resource model.Resource, key string) model.Resource {
	t.Helper()
	created, _, err := store.Create(context.Background(), resource, key)
	if err != nil {
		t.Fatalf("Create(%s): %v", resource.ResourceKind(), err)
	}
	return created
}

func baseTopology(t *testing.T, store Store) (*model.Project, *model.Network, *model.Subnet) {
	t.Helper()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "project").(*model.Project)
	network := mustCreate(t, store, &model.Network{ProjectID: project.ID, Name: "private"}, "network").(*model.Network)
	subnet := mustCreate(t, store, &model.Subnet{ProjectID: project.ID, NetworkID: network.ID, Name: "private-v4", CIDR: "10.0.0.0/24", EnableDHCP: true}, "subnet").(*model.Subnet)
	return project, network, subnet
}

func TestMemoryCreateReplayAndIsolation(t *testing.T) {
	store := deterministicStore()
	request := &model.Project{Name: "tenant", PoolID: "pool-a"}
	createdResource, replayed, err := store.Create(context.Background(), request, "create-tenant")
	if err != nil || replayed {
		t.Fatalf("first Create() replayed=%v err=%v", replayed, err)
	}
	created := createdResource.(*model.Project)
	if created.ID == "" || created.Revision != 1 || created.State != model.ResourcePending {
		t.Fatalf("created metadata = %#v", created.Metadata)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("server metadata was not populated")
	}
	created.Name = "caller-mutated"
	stored, err := store.Get(context.Background(), model.KindProject, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.(*model.Project).Name != "tenant" {
		t.Fatal("caller mutated stored object")
	}

	replayedResource, replayed, err := store.Create(context.Background(), request, "create-tenant")
	if err != nil || !replayed {
		t.Fatalf("replay Create() replayed=%v err=%v", replayed, err)
	}
	if replayedResource.GetMetadata().ID != created.ID {
		t.Fatal("replay returned another resource")
	}
	_, _, err = store.Create(context.Background(), &model.Project{Name: "different", PoolID: "pool-b"}, "create-tenant")
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different replay error = %v", err)
	}
}

func TestMemoryOptimisticUpdateAndDelete(t *testing.T) {
	store := deterministicStore()
	created := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-a"}, "create").(*model.Project)
	created.Description = "updated"
	updatedResource, replayed, err := store.Update(context.Background(), created, 1, "update")
	if err != nil || replayed {
		t.Fatalf("Update() replayed=%v err=%v", replayed, err)
	}
	updated := updatedResource.(*model.Project)
	if updated.Revision != 2 || updated.Description != "updated" || updated.State != model.ResourcePending {
		t.Fatalf("updated = %#v", updated)
	}
	_, _, err = store.Update(context.Background(), created, 1, "stale")
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale update error = %v", err)
	}
	if _, err = store.Delete(context.Background(), model.KindProject, updated.ID, 1, "delete-stale"); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale delete error = %v", err)
	}
	replayed, err = store.Delete(context.Background(), model.KindProject, updated.ID, 2, "delete")
	if err != nil || replayed {
		t.Fatalf("Delete() replayed=%v err=%v", replayed, err)
	}
	replayed, err = store.Delete(context.Background(), model.KindProject, updated.ID, 2, "delete")
	if err != nil || !replayed {
		t.Fatalf("Delete() replay replayed=%v err=%v", replayed, err)
	}
}

func TestMemoryObserveNodeHeartbeatDoesNotChangeDesiredMetadata(t *testing.T) {
	store := deterministicStore()
	node := mustCreate(t, store, &model.Node{Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "node").(*model.Node)
	readyResource, err := store.MarkReconciled(context.Background(), model.KindNode, node.ID, node.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready := readyResource.(*model.Node)
	observedAt := ready.UpdatedAt.Add(time.Minute)
	observed, err := store.ObserveNodeHeartbeat(context.Background(), ready.ID, ready.Revision, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Revision != ready.Revision || observed.AppliedRevision != ready.AppliedRevision || observed.State != ready.State || !observed.UpdatedAt.Equal(ready.UpdatedAt) {
		t.Fatalf("observation changed desired metadata: ready=%#v observed=%#v", ready.Metadata, observed.Metadata)
	}
	if observed.LastSeenAt == nil || !observed.LastSeenAt.Equal(observedAt) {
		t.Fatalf("last_seen_at=%v want %v", observed.LastSeenAt, observedAt)
	}
	older, err := store.ObserveNodeHeartbeat(context.Background(), ready.ID, ready.Revision, observedAt.Add(-time.Second))
	if err != nil || older.LastSeenAt == nil || !older.LastSeenAt.Equal(observedAt) {
		t.Fatalf("older observation regressed liveness: node=%#v err=%v", older, err)
	}
	if _, err := store.ObserveNodeHeartbeat(context.Background(), ready.ID, ready.Revision+1, observedAt.Add(time.Minute)); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale desired revision error=%v", err)
	}
}

func TestMemoryListRecentFirstAndLimit(t *testing.T) {
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	var sequence atomic.Int64
	store := NewMemory(WithClock(func() time.Time { return now }), WithIDGenerator(func() string { return fmt.Sprintf("operation-%d", sequence.Add(1)) }))
	for revision := int64(1); revision <= 4; revision++ {
		mustCreate(t, store, &model.Operation{Action: "bind", TargetKind: model.KindPort, TargetID: "port-a", TargetRevision: revision}, fmt.Sprintf("operation-%d", revision))
		now = now.Add(time.Second)
	}
	resources, err := store.List(context.Background(), model.KindOperation, ListOptions{RecentFirst: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 || resources[0].GetMetadata().ID != "operation-4" || resources[1].GetMetadata().ID != "operation-3" {
		t.Fatalf("recent limited operations=%#v", resources)
	}
	if _, err := store.List(context.Background(), model.KindOperation, ListOptions{Limit: -1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("negative limit error=%v", err)
	}
}

func TestMemoryPrunesOnlyOldSupersededReconcileAudits(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	store := NewMemory(WithClock(func() time.Time { return now }))
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-a"}, "project").(*model.Project)
	for revision := int64(2); revision <= 5; revision++ {
		project.Description = fmt.Sprintf("revision-%d", revision)
		updated, _, err := store.Update(context.Background(), project, project.Revision, fmt.Sprintf("project-%d", revision))
		if err != nil {
			t.Fatal(err)
		}
		project = updated.(*model.Project)
	}
	for revision := int64(1); revision <= 4; revision++ {
		now = now.Add(time.Hour)
		operation := mustCreate(t, store, &model.Operation{
			Action: "reconcile", TargetKind: model.KindProject, TargetID: project.ID, TargetRevision: revision,
			OperationStatus: model.OperationQueued,
		}, fmt.Sprintf("reconcile:%s:%d", project.ID, revision)).(*model.Operation)
		completed := now
		operation.CompletedAt = &completed
		operation.OperationStatus = model.OperationSucceeded
		if _, _, err := store.Update(context.Background(), operation, operation.Revision, ""); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(48 * time.Hour)
	pruned, err := store.PruneOperations(context.Background(), now.Add(-24*time.Hour), 1)
	if err != nil || pruned != 3 {
		t.Fatalf("PruneOperations() pruned=%d err=%v", pruned, err)
	}
	operations, err := store.List(context.Background(), model.KindOperation, ListOptions{})
	if err != nil || len(operations) != 1 || operations[0].(*model.Operation).TargetRevision != 4 {
		t.Fatalf("retained operations=%#v err=%v", operations, err)
	}

	// Even beyond the age/count thresholds, the operation for the active
	// desired revision is retained so periodic forced audits can replay it.
	current := mustCreate(t, store, &model.Operation{
		Action: "reconcile", TargetKind: model.KindProject, TargetID: project.ID, TargetRevision: project.Revision,
		OperationStatus: model.OperationQueued,
	}, fmt.Sprintf("reconcile:%s:%d", project.ID, project.Revision)).(*model.Operation)
	completed := now.Add(-48 * time.Hour)
	current.CompletedAt = &completed
	current.OperationStatus = model.OperationSucceeded
	if _, _, err := store.Update(context.Background(), current, current.Revision, ""); err != nil {
		t.Fatal(err)
	}
	pruned, err = store.PruneOperations(context.Background(), now.Add(-24*time.Hour), 0)
	if err != nil || pruned != 1 {
		// Revision 4 is now outside the keep set and superseded; revision 5 is
		// active and must remain.
		t.Fatalf("second PruneOperations() pruned=%d err=%v", pruned, err)
	}
	operations, err = store.List(context.Background(), model.KindOperation, ListOptions{})
	if err != nil || len(operations) != 1 || operations[0].(*model.Operation).TargetRevision != project.Revision {
		t.Fatalf("active revision operation was not retained: %#v err=%v", operations, err)
	}
}

func TestMemoryReferencesUniquenessAndFiltering(t *testing.T) {
	store := deterministicStore()
	project, network, _ := baseTopology(t, store)
	_, _, err := store.Create(context.Background(), &model.Network{ProjectID: "missing", Name: "bad"}, "missing-ref")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("missing reference error = %v", err)
	}
	_, _, err = store.Create(context.Background(), &model.Network{ProjectID: project.ID, Name: network.Name}, "duplicate")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := store.Delete(context.Background(), model.KindProject, project.ID, project.Revision, "delete-parent"); !errors.Is(err, ErrConflict) {
		t.Fatalf("referenced delete error = %v", err)
	}
	otherProject := mustCreate(t, store, &model.Project{Name: "other", PoolID: "pool-other"}, "other-project").(*model.Project)
	mustCreate(t, store, &model.Network{ProjectID: otherProject.ID, Name: "private"}, "other-network")
	resources, err := store.List(context.Background(), model.KindNetwork, ListOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].GetMetadata().ID != network.ID {
		t.Fatalf("filtered resources = %#v", resources)
	}
}

func TestMemoryRouterExternalGatewayReferences(t *testing.T) {
	store := deterministicStore()
	project, privateNetwork, privateSubnet := baseTopology(t, store)
	provider := mustCreate(t, store, &model.ProviderNetwork{Name: "public", Shared: true}, "provider").(*model.ProviderNetwork)
	externalNetwork := mustCreate(t, store, &model.Network{
		ProjectID:         project.ID,
		Name:              "public",
		External:          true,
		ProviderNetworkID: provider.ID,
	}, "external-network").(*model.Network)
	externalSubnet := mustCreate(t, store, &model.Subnet{
		ProjectID: project.ID,
		NetworkID: externalNetwork.ID,
		Name:      "public-v4",
		CIDR:      "192.0.2.0/24",
		GatewayIP: "192.0.2.1",
	}, "external-subnet").(*model.Subnet)

	valid := &model.Router{
		ProjectID:         project.ID,
		Name:              "edge",
		ExternalNetworkID: externalNetwork.ID,
		ExternalSubnetID:  externalSubnet.ID,
		ExternalIPAddress: "192.0.2.10",
		EnableSNAT:        true,
	}
	router := mustCreate(t, store, valid, "router").(*model.Router)
	if _, err := store.Delete(context.Background(), model.KindSubnet, externalSubnet.ID, externalSubnet.Revision, "delete-external-subnet"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete referenced external subnet error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*model.Router)
	}{
		{"internal network", func(candidate *model.Router) {
			candidate.ExternalNetworkID = privateNetwork.ID
			candidate.ExternalSubnetID = privateSubnet.ID
			candidate.ExternalIPAddress = "10.0.0.10"
		}},
		{"subnet on another network", func(candidate *model.Router) { candidate.ExternalSubnetID = privateSubnet.ID }},
		{"address outside subnet", func(candidate *model.Router) { candidate.ExternalIPAddress = "198.51.100.10" }},
		{"gateway address", func(candidate *model.Router) { candidate.ExternalIPAddress = externalSubnet.GatewayIP }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := *router
			candidate.ID = ""
			candidate.Name = "invalid-" + strings.ReplaceAll(test.name, " ", "-")
			test.mutate(&candidate)
			if _, _, err := store.Create(context.Background(), &candidate, "invalid-"+test.name); !errors.Is(err, ErrConflict) {
				t.Fatalf("Create() error = %v", err)
			}
		})
	}
}

func TestMemoryConcurrentUniqueAllocation(t *testing.T) {
	store := deterministicStore()
	project, _, subnet := baseTopology(t, store)
	const workers = 32
	var successes atomic.Int64
	var duplicateErrors atomic.Int64
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			_, _, err := store.Create(context.Background(), &model.IPAllocation{ProjectID: project.ID, SubnetID: subnet.ID, Address: "10.0.0.10", State: model.IPReserved}, fmt.Sprintf("allocation-%d", i))
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrAlreadyExists):
				duplicateErrors.Add(1)
			default:
				t.Errorf("Create allocation: %v", err)
			}
		}(i)
	}
	wait.Wait()
	if successes.Load() != 1 || duplicateErrors.Load() != workers-1 {
		t.Fatalf("success=%d duplicate=%d", successes.Load(), duplicateErrors.Load())
	}
}

func TestMemoryUpdateRejectsBrokenExistingReferences(t *testing.T) {
	store := deterministicStore()
	project, network, _ := baseTopology(t, store)
	otherProject := mustCreate(t, store, &model.Project{Name: "other", PoolID: "pool-other"}, "other-project").(*model.Project)
	network.ProjectID = otherProject.ID
	if _, _, err := store.Update(context.Background(), network, network.Revision, "move-network"); !errors.Is(err, ErrConflict) {
		t.Fatalf("network move error = %v", err)
	}
	stored, err := store.Get(context.Background(), model.KindNetwork, network.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.(*model.Network).ProjectID != project.ID {
		t.Fatalf("failed update changed stored network: %#v", stored)
	}
}

func TestMemoryMarkReconciledHonorsDesiredRevision(t *testing.T) {
	store := deterministicStore()
	created := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool"}, "create").(*model.Project)
	ready, err := store.MarkReconciled(context.Background(), model.KindProject, created.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ready.GetMetadata().State != model.ResourceReady || ready.GetMetadata().AppliedRevision != 1 {
		t.Fatalf("ready metadata = %#v", ready.GetMetadata())
	}
	project := ready.(*model.Project)
	project.Description = "revision two"
	updated, _, err := store.Update(context.Background(), project, 1, "update")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.MarkReconciled(context.Background(), model.KindProject, created.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stale.GetMetadata().State != model.ResourcePending || stale.GetMetadata().AppliedRevision != 1 || updated.GetMetadata().Revision != 2 {
		t.Fatalf("stale mark metadata = %#v", stale.GetMetadata())
	}
	failed, err := store.MarkReconciled(context.Background(), model.KindProject, created.ID, 2, errors.New("OVN unavailable"))
	if err != nil {
		t.Fatal(err)
	}
	if failed.GetMetadata().State != model.ResourceError || failed.GetMetadata().LastError != "OVN unavailable" {
		t.Fatalf("failed metadata = %#v", failed.GetMetadata())
	}
}

func TestMemoryContextCancellation(t *testing.T) {
	store := deterministicStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.List(ctx, model.KindProject, ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("List error = %v", err)
	}
}

func TestMemoryOperationUsesRequestIdempotencyKey(t *testing.T) {
	store := deterministicStore()
	operation := &model.Operation{Action: "reconcile", TargetKind: model.KindNetwork, TargetID: "network-id", TargetRevision: 1}
	created := mustCreate(t, store, operation, "reconcile:network:network-id:1").(*model.Operation)
	if created.IdempotencyKey != "reconcile:network:network-id:1" {
		t.Fatalf("idempotency key = %q", created.IdempotencyKey)
	}
	if _, _, err := store.Create(context.Background(), &model.Operation{Action: "retry", TargetKind: model.KindNetwork, TargetID: "network-id", TargetRevision: 1}, "retry-key"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate target revision error = %v", err)
	}
}

func TestMemoryDeleteTombstoneMustBePurged(t *testing.T) {
	store := deterministicStore()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool"}, "create").(*model.Project)
	tombstone, replayed, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, 1, "delete")
	if err != nil || replayed {
		t.Fatalf("BeginDelete replayed=%v err=%v", replayed, err)
	}
	if tombstone.GetMetadata().State != model.ResourceDeleting || tombstone.GetMetadata().Revision != 2 {
		t.Fatalf("tombstone metadata = %#v", tombstone.GetMetadata())
	}
	stored, err := store.Get(context.Background(), model.KindProject, project.ID)
	if err != nil || stored.GetMetadata().State != model.ResourceDeleting {
		t.Fatalf("stored tombstone=%#v err=%v", stored, err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, 1); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale Purge error=%v", err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), model.KindProject, project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after purge error=%v", err)
	}
	replayedTombstone, replayed, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, 1, "delete")
	if err != nil || !replayed || replayedTombstone.GetMetadata().Revision != 2 {
		t.Fatalf("tombstone replay=%#v replayed=%v err=%v", replayedTombstone, replayed, err)
	}
}

func TestMemoryReconcileClaimFencesPurgeAndRecoversExpiredLease(t *testing.T) {
	store := deterministicStore()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool"}, "create-fenced-project").(*model.Project)
	operation := mustCreate(t, store, &model.Operation{
		Action:          "reconcile",
		TargetKind:      model.KindProject,
		TargetID:        project.ID,
		TargetRevision:  project.Revision,
		OperationStatus: model.OperationQueued,
	}, "reconcile-fenced-project").(*model.Operation)
	started := time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-memory", started, started.Add(-2*time.Minute))
	if err != nil || claimed.OperationStatus != model.OperationRunning || claimed.StartedAt == nil || !claimed.StartedAt.Equal(started) {
		t.Fatalf("ClaimReconcile() operation=%#v err=%v", claimed, err)
	}
	if _, err := store.RenewOperationLease(context.Background(), claimed.ID, claimed.Revision, "lease-other", started.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-owner RenewOperationLease() error=%v", err)
	}
	renewedAt := started.Add(time.Minute)
	claimed, err = store.RenewOperationLease(context.Background(), claimed.ID, claimed.Revision, "lease-memory", renewedAt)
	if err != nil || claimed.Revision != operation.Revision+2 || claimed.StartedAt == nil || !claimed.StartedAt.Equal(started) || !claimed.UpdatedAt.Equal(renewedAt) {
		t.Fatalf("RenewOperationLease() operation=%#v err=%v", claimed, err)
	}
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete-fenced-project")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("Purge() error=%v, want reconcile fence", err)
	}
	active, recovered, err := store.FenceReconciles(context.Background(), model.KindProject, project.ID, renewedAt.Add(-time.Minute), renewedAt.Add(time.Minute))
	if err != nil || !active || recovered {
		t.Fatalf("live FenceReconciles() active=%v recovered=%v err=%v", active, recovered, err)
	}
	active, recovered, err = store.FenceReconciles(context.Background(), model.KindProject, project.ID, renewedAt.Add(time.Second), renewedAt.Add(3*time.Minute))
	if err != nil || active || !recovered {
		t.Fatalf("expired FenceReconciles() active=%v recovered=%v err=%v", active, recovered, err)
	}
	stored, err := store.Get(context.Background(), model.KindOperation, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := stored.(*model.Operation)
	if failed.OperationStatus != model.OperationFailed || failed.CompletedAt == nil || failed.Error == "" {
		t.Fatalf("expired operation=%#v", failed)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryReconcileCannotStartAfterTombstone(t *testing.T) {
	store := deterministicStore()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool"}, "create-claim-project").(*model.Project)
	operation := mustCreate(t, store, &model.Operation{
		Action:          "reconcile",
		TargetKind:      model.KindProject,
		TargetID:        project.ID,
		TargetRevision:  project.Revision,
		OperationStatus: model.OperationQueued,
	}, "reconcile-claim-project").(*model.Operation)
	if _, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete-before-claim"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC)
	if _, err := store.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-memory", now, now.Add(-2*time.Minute)); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("ClaimReconcile() error=%v, want inactive target precondition", err)
	}
	stored, err := store.Get(context.Background(), model.KindOperation, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.(*model.Operation).OperationStatus != model.OperationQueued {
		t.Fatalf("operation changed despite rejected claim: %#v", stored)
	}
	queued := stored.(*model.Operation)
	queued.OperationStatus = model.OperationRunning
	if _, _, err := store.Update(context.Background(), queued, queued.Revision, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("generic running transition error=%v", err)
	}
}

func TestMemoryDeleteLeaseBlocksPurgeUntilOwnerCompletes(t *testing.T) {
	store := deterministicStore()
	project := mustCreate(t, store, &model.Project{Name: "delete-tenant", PoolID: "delete-pool"}, "delete-project").(*model.Project)
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "begin-delete")
	if err != nil {
		t.Fatal(err)
	}
	operation := mustCreate(t, store, &model.Operation{Action: "delete", TargetKind: model.KindProject, TargetID: project.ID, TargetRevision: tombstone.GetMetadata().Revision, OperationStatus: model.OperationQueued}, "delete-operation").(*model.Operation)
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimDelete(context.Background(), operation.ID, operation.Revision, "lease-delete", now, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("Purge() error=%v, want delete lease fence", err)
	}
	claimed, err = store.RenewOperationLease(context.Background(), claimed.ID, claimed.Revision, "lease-delete", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	claimed.OperationStatus = model.OperationSucceeded
	completed := now.Add(2 * time.Second)
	claimed.CompletedAt = &completed
	if _, _, err := store.Update(context.Background(), claimed, claimed.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryClaimAndDeleteAreSerialized(t *testing.T) {
	for iteration := 0; iteration < 64; iteration++ {
		store := NewMemory()
		project := mustCreate(t, store, &model.Project{Name: fmt.Sprintf("tenant-%d", iteration), PoolID: fmt.Sprintf("pool-%d", iteration)}, "project").(*model.Project)
		operation := mustCreate(t, store, &model.Operation{
			Action:          "reconcile",
			TargetKind:      model.KindProject,
			TargetID:        project.ID,
			TargetRevision:  project.Revision,
			OperationStatus: model.OperationQueued,
		}, "operation").(*model.Operation)
		start := make(chan struct{})
		claimResult := make(chan error, 1)
		deleteResult := make(chan error, 1)
		now := time.Now().UTC()
		go func() {
			<-start
			_, err := store.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-memory", now, now.Add(-2*time.Minute))
			claimResult <- err
		}()
		go func() {
			<-start
			_, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete")
			deleteResult <- err
		}()
		close(start)
		claimErr, deleteErr := <-claimResult, <-deleteResult
		if deleteErr != nil {
			t.Fatalf("iteration %d BeginDelete(): %v", iteration, deleteErr)
		}
		if claimErr != nil && !errors.Is(claimErr, ErrPrecondition) {
			t.Fatalf("iteration %d ClaimReconcile(): %v", iteration, claimErr)
		}
		stored, err := store.Get(context.Background(), model.KindOperation, operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		status := stored.(*model.Operation).OperationStatus
		if (claimErr == nil) != (status == model.OperationRunning) {
			t.Fatalf("iteration %d claimErr=%v status=%s", iteration, claimErr, status)
		}
	}
}
