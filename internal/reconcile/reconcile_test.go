package reconcile

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/controlstore"
	"github.com/pvnstack/proxmox-ovn/internal/model"
)

type recordingRenderer struct {
	mu    sync.Mutex
	kinds []model.Kind
}

func (r *recordingRenderer) Render(_ context.Context, resource model.Resource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, resource.ResourceKind())
	return nil
}

func (*recordingRenderer) Delete(context.Context, model.Resource) error { return nil }

type blockingRenderer struct {
	delegate *FakeRenderer
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

type blockingDeleteRenderer struct {
	delegate *FakeRenderer
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (renderer *blockingDeleteRenderer) Render(ctx context.Context, resource model.Resource) error {
	return renderer.delegate.Render(ctx, resource)
}

func (renderer *blockingDeleteRenderer) Delete(ctx context.Context, resource model.Resource) error {
	renderer.once.Do(func() { close(renderer.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-renderer.release:
		return renderer.delegate.Delete(ctx, resource)
	}
}

type renewalObservingStore struct {
	controlstore.Store
	renewed chan time.Time
	fail    error
}

func (store *renewalObservingStore) RenewOperationLease(ctx context.Context, operationID string, expectedRevision int64, leaseOwner string, renewedAt time.Time) (*model.Operation, error) {
	if store.fail != nil {
		select {
		case store.renewed <- renewedAt:
		default:
		}
		return nil, store.fail
	}
	operation, err := store.Store.RenewOperationLease(ctx, operationID, expectedRevision, leaseOwner, renewedAt)
	if err == nil {
		select {
		case store.renewed <- renewedAt:
		default:
		}
	}
	return operation, err
}

func (r *blockingRenderer) Render(ctx context.Context, resource model.Resource) error {
	r.once.Do(func() { close(r.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return r.delegate.Render(ctx, resource)
	}
}

func (r *blockingRenderer) Delete(ctx context.Context, resource model.Resource) error {
	return r.delegate.Delete(ctx, resource)
}

// unfencedRevisionRenderer deliberately lets an older render overwrite a
// newer one, matching the failure mode the controller must correct across
// independent manager processes.
type unfencedRevisionRenderer struct {
	mu            sync.Mutex
	blockRevision int64
	started       chan struct{}
	release       chan struct{}
	once          sync.Once
	revision      int64
	calls         int
}

func (r *unfencedRevisionRenderer) Render(ctx context.Context, resource model.Resource) error {
	if resource.GetMetadata().Revision == r.blockRevision {
		r.once.Do(func() { close(r.started) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.release:
		}
	}
	r.mu.Lock()
	r.revision = resource.GetMetadata().Revision
	r.calls++
	r.mu.Unlock()
	return nil
}

func (r *unfencedRevisionRenderer) Delete(context.Context, model.Resource) error {
	r.mu.Lock()
	r.revision = 0
	r.mu.Unlock()
	return nil
}

func (r *unfencedRevisionRenderer) state() (int64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revision, r.calls
}

func (r *unfencedRevisionRenderer) drift(revision int64) {
	r.mu.Lock()
	r.revision = revision
	r.mu.Unlock()
}

func createProject(t *testing.T, store controlstore.Store) *model.Project {
	t.Helper()
	resource, _, err := store.Create(context.Background(), &model.Project{Name: "tenant", PoolID: "pool"}, "create-project")
	if err != nil {
		t.Fatal(err)
	}
	return resource.(*model.Project)
}

func waitForRenewalSpan(t *testing.T, renewed <-chan time.Time, span time.Duration) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	var first time.Time
	for {
		select {
		case timestamp := <-renewed:
			if first.IsZero() {
				first = timestamp
			}
			if timestamp.Sub(first) >= span {
				return
			}
		case <-deadline.C:
			t.Fatalf("writer lease was not renewed across %s", span)
		}
	}
}

func TestControllerRendersRevisionOnce(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	if calls := renderer.Calls(model.KindProject, project.ID); calls != 1 {
		t.Fatalf("renderer calls = %d", calls)
	}
	ready, err := store.Get(context.Background(), model.KindProject, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.GetMetadata().State != model.ResourceReady || ready.GetMetadata().AppliedRevision != 1 {
		t.Fatalf("metadata = %#v", ready.GetMetadata())
	}
	operations, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].(*model.Operation).OperationStatus != model.OperationSucceeded {
		t.Fatalf("operations = %#v", operations)
	}
}

func TestControllerSerializesConcurrentReconcile(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	const workers = 24
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
				t.Errorf("Reconcile: %v", err)
			}
		}()
	}
	wait.Wait()
	if calls := renderer.Calls(model.KindProject, project.ID); calls != 1 {
		t.Fatalf("renderer calls = %d", calls)
	}
}

func TestControllerFailureCanBeRetried(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := NewFakeRenderer()
	renderer.SetFailure(model.KindProject, project.ID, errors.New("northbound unavailable"))
	controller := NewController(store, renderer)
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err == nil {
		t.Fatal("render failure was not returned")
	}
	failed, err := store.Get(context.Background(), model.KindProject, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.GetMetadata().State != model.ResourceError || failed.GetMetadata().AppliedRevision != 0 {
		t.Fatalf("failed metadata = %#v", failed.GetMetadata())
	}

	renderer.SetFailure(model.KindProject, project.ID, nil)
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	ready, err := store.Get(context.Background(), model.KindProject, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.GetMetadata().State != model.ResourceReady || ready.GetMetadata().AppliedRevision != 1 {
		t.Fatalf("ready metadata = %#v", ready.GetMetadata())
	}
	operations, _ := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if len(operations) != 1 || operations[0].(*model.Operation).OperationStatus != model.OperationSucceeded {
		t.Fatalf("retry operation = %#v", operations)
	}
}

func TestControllerRendersNewDesiredRevision(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(context.Background(), model.KindProject, project.ID)
	updatedProject := current.(*model.Project)
	updatedProject.Description = "changed"
	updated, _, err := store.Update(context.Background(), updatedProject, updatedProject.Revision, "update")
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Reconcile(context.Background(), model.KindProject, updated.GetMetadata().ID); err != nil {
		t.Fatal(err)
	}
	if calls := renderer.Calls(model.KindProject, project.ID); calls != 2 {
		t.Fatalf("renderer calls = %d", calls)
	}
	rendered, ok := renderer.Rendered(model.KindProject, project.ID)
	if !ok || rendered.GetMetadata().Revision != 2 {
		t.Fatalf("rendered = %#v, ok=%v", rendered, ok)
	}
}

func TestControllerReconcileAll(t *testing.T) {
	store := controlstore.NewMemory()
	createProject(t, store)
	_, _, err := store.Create(context.Background(), &model.ProviderNetwork{Name: "provider"}, "provider")
	if err != nil {
		t.Fatal(err)
	}
	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	projects, _ := store.List(context.Background(), model.KindProject, controlstore.ListOptions{})
	providers, _ := store.List(context.Background(), model.KindProviderNetwork, controlstore.ListOptions{})
	if projects[0].GetMetadata().State != model.ResourceReady || providers[0].GetMetadata().State != model.ResourceReady {
		t.Fatal("ReconcileAll did not render all resources")
	}
}

func TestControllerReconcileAllUsesDependencyOrder(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	provider, _, err := store.Create(context.Background(), &model.ProviderNetwork{Name: "provider"}, "provider")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Create(context.Background(), &model.ProviderSegment{ProviderNetworkID: provider.GetMetadata().ID, Name: "flat", PhysicalNetwork: "provider", NetworkType: model.ProviderFlat}, "segment")
	if err != nil {
		t.Fatal(err)
	}
	network, _, err := store.Create(context.Background(), &model.Network{ProjectID: project.ID, Name: "private"}, "network")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Create(context.Background(), &model.Subnet{ProjectID: project.ID, NetworkID: network.GetMetadata().ID, Name: "v4", CIDR: "10.0.0.0/24"}, "subnet")
	if err != nil {
		t.Fatal(err)
	}
	group, _, err := store.Create(context.Background(), &model.SecurityGroup{ProjectID: project.ID, Name: "default"}, "group")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Create(context.Background(), &model.SecurityGroupRule{ProjectID: project.ID, SecurityGroupID: group.GetMetadata().ID, Direction: model.DirectionIngress, EtherType: model.EtherTypeIPv4, Action: model.ActionAllow}, "rule")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Create(context.Background(), &model.Node{Name: "pve01", ChassisID: "chassis"}, "node")
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingRenderer{}
	if err := NewController(store, recorder).ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []model.Kind{model.KindProject, model.KindProviderNetwork, model.KindNetwork, model.KindProviderSegment, model.KindSubnet, model.KindSecurityGroup, model.KindSecurityGroupRule, model.KindNode}
	if len(recorder.kinds) != len(want) {
		t.Fatalf("render order=%v want=%v", recorder.kinds, want)
	}
	for i := range want {
		if recorder.kinds[i] != want[i] {
			t.Fatalf("render order=%v want=%v", recorder.kinds, want)
		}
	}
}

func TestControllerDeleteCleansRendererBeforePurge(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, 1, "delete")
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Delete(context.Background(), tombstone); err != nil {
		t.Fatal(err)
	}
	if renderer.DeleteCalls(model.KindProject, project.ID) != 1 {
		t.Fatalf("delete calls=%d", renderer.DeleteCalls(model.KindProject, project.ID))
	}
	if _, ok := renderer.Rendered(model.KindProject, project.ID); ok {
		t.Fatal("renderer still contains deleted resource")
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); err != nil {
		t.Fatal(err)
	}
	operations, _ := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	found := false
	for _, resource := range operations {
		op := resource.(*model.Operation)
		if op.Action == "delete" && op.OperationStatus == model.OperationSucceeded {
			found = true
		}
	}
	if !found {
		t.Fatalf("delete operation not recorded: %#v", operations)
	}
}

func TestControllerDeleteFailureKeepsRetryableTombstone(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete-with-retry")
	if err != nil {
		t.Fatal(err)
	}
	renderer.SetDeleteFailure(model.KindProject, project.ID, errors.New("OVN unavailable"))
	if err := controller.Delete(context.Background(), tombstone); err == nil {
		t.Fatal("Delete() unexpectedly succeeded")
	}
	stored, err := store.Get(context.Background(), model.KindProject, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetMetadata().State != model.ResourceDeleting {
		t.Fatalf("failed delete changed tombstone state to %s", stored.GetMetadata().State)
	}
	renderer.SetDeleteFailure(model.KindProject, project.ID, nil)
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), model.KindProject, project.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("Get() error=%v, want tombstone purged after retry", err)
	}
}

func TestControllerReconcileRecoversPersistedTombstone(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete"); err != nil {
		t.Fatal(err)
	}
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), model.KindProject, project.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("Get() error=%v, want not found", err)
	}
	if _, ok := renderer.Rendered(model.KindProject, project.ID); ok {
		t.Fatal("renderer still contains recovered tombstone")
	}
}

func TestControllerCleansRenderThatRacesWithDistributedDelete(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	delegate := NewFakeRenderer()
	renderer := &blockingRenderer{delegate: delegate, started: make(chan struct{}), release: make(chan struct{})}
	reconcileController := NewController(store, renderer)
	deleteController := NewController(store, renderer)

	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- reconcileController.Reconcile(context.Background(), model.KindProject, project.ID)
	}()
	<-renderer.started

	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteController.Delete(context.Background(), tombstone); !errors.Is(err, ErrReconcileLeaseActive) {
		t.Fatalf("Delete() error=%v, want active reconcile lease", err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Purge() error=%v, want running reconcile conflict", err)
	}
	close(renderer.release)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	if err := deleteController.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), model.KindProject, project.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("Get() error=%v, want purged tombstone", err)
	}
	if _, ok := delegate.Rendered(model.KindProject, project.ID); ok {
		t.Fatal("stale render survived the distributed delete")
	}
	if calls := delegate.DeleteCalls(model.KindProject, project.ID); calls != 2 {
		t.Fatalf("delete calls=%d, want distributed delete plus stale cleanup", calls)
	}
}

func TestReconcileAllRecoversCrashAfterExternalWriteBeforeDeleteCheck(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := NewFakeRenderer()
	operationResource, _, err := store.Create(context.Background(), &model.Operation{
		Action:          "reconcile",
		TargetKind:      model.KindProject,
		TargetID:        project.ID,
		TargetRevision:  project.Revision,
		OperationStatus: model.OperationQueued,
		IdempotencyKey:  operationKey(model.KindProject, project.ID, project.Revision),
	}, operationKey(model.KindProject, project.ID, project.Revision))
	if err != nil {
		t.Fatal(err)
	}
	operation := operationResource.(*model.Operation)
	started := time.Now().UTC().Add(-operationLease - time.Minute)
	if _, err := store.ClaimReconcile(context.Background(), operation.ID, operation.Revision, "lease-crashed", started, started.Add(-operationLease)); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Render(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete-after-crash")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Purge() error=%v, want crashed operation fence", err)
	}

	controller := NewController(store, renderer)
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), model.KindProject, project.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("Get() error=%v, want recovered tombstone purged", err)
	}
	if _, ok := renderer.Rendered(model.KindProject, project.ID); ok {
		t.Fatal("realized row written immediately before the crash survived recovery")
	}
	storedOperation, err := store.Get(context.Background(), model.KindOperation, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := storedOperation.(*model.Operation)
	if recovered.OperationStatus != model.OperationFailed || recovered.CompletedAt == nil || recovered.Error == "" {
		t.Fatalf("recovered operation=%#v", recovered)
	}
}

func TestControllersCorrectOutOfOrderDesiredRevisions(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := &unfencedRevisionRenderer{
		blockRevision: project.Revision,
		started:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	oldController := NewController(store, renderer)
	newController := NewController(store, renderer)

	oldDone := make(chan error, 1)
	go func() {
		oldDone <- oldController.Reconcile(context.Background(), model.KindProject, project.ID)
	}()
	<-renderer.started

	project.Description = "new desired revision"
	updated, _, err := store.Update(context.Background(), project, project.Revision, "update-while-rendering")
	if err != nil {
		t.Fatal(err)
	}
	if err := newController.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	if revision, _ := renderer.state(); revision != updated.GetMetadata().Revision {
		t.Fatalf("new manager rendered revision %d, want %d", revision, updated.GetMetadata().Revision)
	}

	close(renderer.release)
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if revision, calls := renderer.state(); revision != updated.GetMetadata().Revision || calls != 3 {
		t.Fatalf("final renderer revision=%d calls=%d, want revision=%d and old/new/correction", revision, calls, updated.GetMetadata().Revision)
	}
}

func TestReconcileAllRepairsReadyResourceDrift(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := &unfencedRevisionRenderer{blockRevision: -1}
	controller := NewController(store, renderer)
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	renderer.drift(0)
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if revision, calls := renderer.state(); revision != project.Revision || calls != 2 {
		t.Fatalf("periodic audit revision=%d calls=%d, want revision=%d calls=2", revision, calls, project.Revision)
	}
}

func TestControllersShareRunningOperationLease(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := &unfencedRevisionRenderer{
		blockRevision: project.Revision,
		started:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	first := NewController(store, renderer)
	second := NewController(store, renderer)
	done := make(chan error, 1)
	go func() { done <- first.Reconcile(context.Background(), model.KindProject, project.ID) }()
	<-renderer.started
	if err := second.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	close(renderer.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, calls := renderer.state(); calls != 1 {
		t.Fatalf("renderer calls=%d, want one durable operation owner", calls)
	}
}

func TestHeartbeatKeepsBlockedRenderFencedPastLease(t *testing.T) {
	const lease = 45 * time.Millisecond
	baseStore := controlstore.NewMemory()
	store := &renewalObservingStore{Store: baseStore, renewed: make(chan time.Time, 64)}
	project := createProject(t, store)
	delegate := NewFakeRenderer()
	renderer := &blockingRenderer{delegate: delegate, started: make(chan struct{}), release: make(chan struct{})}
	reconcileController := NewController(store, renderer, WithLeaseDuration(lease))
	deleteController := NewController(store, renderer, WithLeaseDuration(lease))
	done := make(chan error, 1)
	go func() { done <- reconcileController.Reconcile(context.Background(), model.KindProject, project.ID) }()
	<-renderer.started
	waitForRenewalSpan(t, store.renewed, lease)

	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete-after-long-render")
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteController.Delete(context.Background(), tombstone); !errors.Is(err, ErrReconcileLeaseActive) {
		t.Fatalf("Delete() error=%v, want renewed writer lease", err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Purge() error=%v, want renewed writer fence", err)
	}
	close(renderer.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := deleteController.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), model.KindProject, project.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("Get() error=%v, want purged tombstone", err)
	}
}

func TestHeartbeatKeepsBlockedDeleteFencedPastLease(t *testing.T) {
	const lease = 45 * time.Millisecond
	baseStore := controlstore.NewMemory()
	store := &renewalObservingStore{Store: baseStore, renewed: make(chan time.Time, 64)}
	project := createProject(t, store)
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "begin-long-delete")
	if err != nil {
		t.Fatal(err)
	}
	delegate := NewFakeRenderer()
	renderer := &blockingDeleteRenderer{delegate: delegate, started: make(chan struct{}), release: make(chan struct{})}
	first := NewController(store, renderer, WithLeaseDuration(lease))
	second := NewController(store, renderer, WithLeaseDuration(lease))
	done := make(chan error, 1)
	go func() { done <- first.Delete(context.Background(), tombstone) }()
	<-renderer.started
	waitForRenewalSpan(t, store.renewed, lease)
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Purge() error=%v, want running delete fence", err)
	}
	if err := second.Delete(context.Background(), tombstone); !errors.Is(err, ErrReconcileLeaseActive) {
		t.Fatalf("concurrent Delete() error=%v, want active delete lease", err)
	}
	close(renderer.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatLossCancelsBlockedRendererWithoutCompletingOperation(t *testing.T) {
	baseStore := controlstore.NewMemory()
	store := &renewalObservingStore{Store: baseStore, renewed: make(chan time.Time, 1), fail: controlstore.ErrPrecondition}
	project := createProject(t, store)
	renderer := &blockingRenderer{delegate: NewFakeRenderer(), started: make(chan struct{}), release: make(chan struct{})}
	controller := NewController(store, renderer, WithLeaseDuration(30*time.Millisecond))
	done := make(chan error, 1)
	go func() { done <- controller.Reconcile(context.Background(), model.KindProject, project.ID) }()
	<-renderer.started
	select {
	case <-store.renewed:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat did not attempt renewal")
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "lost its writer lease") {
		t.Fatalf("Reconcile() error=%v, want writer lease loss", err)
	}
	operations, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].(*model.Operation).OperationStatus != model.OperationRunning {
		t.Fatalf("operation was completed after lease loss: %#v", operations)
	}
}

func TestParentCancellationStopsHeartbeatAndReturnsContextError(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := &blockingRenderer{delegate: NewFakeRenderer(), started: make(chan struct{}), release: make(chan struct{})}
	controller := NewController(store, renderer, WithLeaseDuration(30*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Reconcile(ctx, model.KindProject, project.ID) }()
	<-renderer.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() error=%v, want context canceled", err)
	}
}
