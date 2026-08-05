package controlstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/model"
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
