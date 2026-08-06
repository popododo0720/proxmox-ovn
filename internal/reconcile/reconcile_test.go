package reconcile

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
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
	renewed         chan time.Time
	fail            error
	cancelOnFailure context.CancelFunc
}

type cancelingOperationUpdateStore struct {
	controlstore.Store
	cancel context.CancelFunc
	once   sync.Once
}

type blockingOperationUpdateStore struct {
	controlstore.Store
	started chan struct{}
	once    sync.Once
}

type retentionObservingStore struct {
	controlstore.Store
	mu     sync.Mutex
	calls  int
	before time.Time
	keep   int
}

type recoveryObservingStore struct {
	controlstore.Store
	mu          sync.Mutex
	calls       int
	leaseCutoff time.Time
	recoveredAt time.Time
	limit       int
}

type snapshotObservingStore struct {
	controlstore.Store
	mu            sync.Mutex
	snapshotCalls int
	listCalls     int
	kinds         [][]model.Kind
}

func (store *snapshotObservingStore) List(ctx context.Context, kind model.Kind, options controlstore.ListOptions) ([]model.Resource, error) {
	store.mu.Lock()
	store.listCalls++
	store.mu.Unlock()
	return store.Store.List(ctx, kind, options)
}

func (store *snapshotObservingStore) Snapshot(ctx context.Context, kinds []model.Kind, options controlstore.ListOptions) (controlstore.ResourceSnapshot, error) {
	store.mu.Lock()
	store.snapshotCalls++
	store.kinds = append(store.kinds, append([]model.Kind(nil), kinds...))
	store.mu.Unlock()
	return store.Store.Snapshot(ctx, kinds, options)
}

func (store *recoveryObservingStore) RecoverExpiredOperations(ctx context.Context, leaseCutoff, recoveredAt time.Time, limit int) (int, error) {
	store.mu.Lock()
	store.calls++
	store.leaseCutoff = leaseCutoff
	store.recoveredAt = recoveredAt
	store.limit = limit
	store.mu.Unlock()
	return store.Store.RecoverExpiredOperations(ctx, leaseCutoff, recoveredAt, limit)
}

func (store *retentionObservingStore) PruneOperations(ctx context.Context, before time.Time, keep int) (int, error) {
	store.mu.Lock()
	store.calls++
	store.before = before
	store.keep = keep
	store.mu.Unlock()
	return store.Store.PruneOperations(ctx, before, keep)
}

func (store *renewalObservingStore) RenewOperationLease(ctx context.Context, operationID string, expectedRevision int64, leaseOwner string, renewedAt time.Time) (*model.Operation, error) {
	if store.fail != nil {
		if store.cancelOnFailure != nil {
			store.cancelOnFailure()
		}
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

func (store *cancelingOperationUpdateStore) Update(ctx context.Context, resource model.Resource, expectedRevision int64, key string) (model.Resource, bool, error) {
	if operation, ok := resource.(*model.Operation); ok && (operation.OperationStatus == model.OperationSucceeded || operation.OperationStatus == model.OperationFailed) {
		store.once.Do(store.cancel)
	}
	return store.Store.Update(ctx, resource, expectedRevision, key)
}

func (store *blockingOperationUpdateStore) Update(ctx context.Context, resource model.Resource, expectedRevision int64, key string) (model.Resource, bool, error) {
	if operation, ok := resource.(*model.Operation); ok && (operation.OperationStatus == model.OperationSucceeded || operation.OperationStatus == model.OperationFailed) {
		store.once.Do(func() { close(store.started) })
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	return store.Store.Update(ctx, resource, expectedRevision, key)
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

func createAssociatedFloatingIP(t *testing.T, store controlstore.Store) *model.FloatingIP {
	t.Helper()
	project := createProject(t, store)
	provider, _, err := store.Create(context.Background(), &model.ProviderNetwork{Name: "public"}, "fip-provider")
	if err != nil {
		t.Fatal(err)
	}
	externalNetwork, _, err := store.Create(context.Background(), &model.Network{
		ProjectID: project.ID, Name: "public", External: true, ProviderNetworkID: provider.GetMetadata().ID,
	}, "fip-external-network")
	if err != nil {
		t.Fatal(err)
	}
	externalSubnet, _, err := store.Create(context.Background(), &model.Subnet{
		ProjectID: project.ID, NetworkID: externalNetwork.GetMetadata().ID, Name: "public-v4",
		CIDR: "192.0.2.0/24", GatewayIP: "192.0.2.1", AllocationPools: []model.IPRange{{Start: "192.0.2.2", End: "192.0.2.200"}},
	}, "fip-external-subnet")
	if err != nil {
		t.Fatal(err)
	}
	privateNetwork, _, err := store.Create(context.Background(), &model.Network{ProjectID: project.ID, Name: "private"}, "fip-private-network")
	if err != nil {
		t.Fatal(err)
	}
	privateSubnet, _, err := store.Create(context.Background(), &model.Subnet{
		ProjectID: project.ID, NetworkID: privateNetwork.GetMetadata().ID, Name: "private-v4", CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1",
	}, "fip-private-subnet")
	if err != nil {
		t.Fatal(err)
	}
	port, _, err := store.Create(context.Background(), &model.Port{
		ProjectID: project.ID, NetworkID: privateNetwork.GetMetadata().ID, Name: "vm-port", MACAddress: "02:00:00:00:00:10",
		FixedIPs: []model.FixedIP{{SubnetID: privateSubnet.GetMetadata().ID, Address: "10.0.0.10"}},
	}, "fip-port")
	if err != nil {
		t.Fatal(err)
	}
	router, _, err := store.Create(context.Background(), &model.Router{
		ProjectID: project.ID, Name: "router", ExternalNetworkID: externalNetwork.GetMetadata().ID,
		ExternalSubnetID: externalSubnet.GetMetadata().ID, ExternalIPAddress: "192.0.2.2", EnableSNAT: true,
	}, "fip-router")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(context.Background(), &model.RouterInterface{
		ProjectID: project.ID, RouterID: router.GetMetadata().ID, SubnetID: privateSubnet.GetMetadata().ID,
	}, "fip-router-interface"); err != nil {
		t.Fatal(err)
	}
	floatingIP, _, err := store.Create(context.Background(), &model.FloatingIP{
		ProjectID: project.ID, ProviderNetworkID: provider.GetMetadata().ID, Address: "192.0.2.10",
		RouterID: router.GetMetadata().ID, PortID: port.GetMetadata().ID, FixedIPAddress: "10.0.0.10",
	}, "fip-create")
	if err != nil {
		t.Fatal(err)
	}
	return floatingIP.(*model.FloatingIP)
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

func TestControllerTracksFloatingIPRealizedLifecycle(t *testing.T) {
	store := controlstore.NewMemory()
	floatingIP := createAssociatedFloatingIP(t, store)
	if floatingIP.State != model.ResourcePending || floatingIP.FloatingStatus != model.FloatingIPDown {
		t.Fatalf("created floating IP state=%s status=%s", floatingIP.State, floatingIP.FloatingStatus)
	}

	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	if err := controller.Reconcile(context.Background(), model.KindFloatingIP, floatingIP.ID); err != nil {
		t.Fatal(err)
	}
	realizedResource, err := store.Get(context.Background(), model.KindFloatingIP, floatingIP.ID)
	if err != nil {
		t.Fatal(err)
	}
	realized := realizedResource.(*model.FloatingIP)
	if realized.State != model.ResourceReady || realized.FloatingStatus != model.FloatingIPActive {
		t.Fatalf("realized floating IP state=%s status=%s", realized.State, realized.FloatingStatus)
	}

	routerID, portID, fixedIPAddress := realized.RouterID, realized.PortID, realized.FixedIPAddress
	realized.FixedIPAddress = ""
	realized.PortID = ""
	reservedResource, _, err := store.Update(context.Background(), realized, realized.Revision, "fip-reserve")
	if err != nil {
		t.Fatal(err)
	}
	reserved := reservedResource.(*model.FloatingIP)
	if reserved.State != model.ResourcePending || reserved.FloatingStatus != model.FloatingIPDown {
		t.Fatalf("pending reserved floating IP state=%s status=%s", reserved.State, reserved.FloatingStatus)
	}
	if err := controller.Reconcile(context.Background(), model.KindFloatingIP, floatingIP.ID); err != nil {
		t.Fatal(err)
	}
	reservedResource, err = store.Get(context.Background(), model.KindFloatingIP, floatingIP.ID)
	if err != nil {
		t.Fatal(err)
	}
	reserved = reservedResource.(*model.FloatingIP)
	if reserved.State != model.ResourceReady || reserved.FloatingStatus != model.FloatingIPDown {
		t.Fatalf("realized reserved floating IP state=%s status=%s", reserved.State, reserved.FloatingStatus)
	}

	reserved.RouterID = routerID
	reserved.PortID = portID
	reserved.FixedIPAddress = fixedIPAddress
	pendingResource, _, err := store.Update(context.Background(), reserved, reserved.Revision, "fip-reassociate")
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingResource.(*model.FloatingIP)
	renderer.SetFailure(model.KindFloatingIP, floatingIP.ID, errors.New("OVN unavailable"))
	if err := controller.Reconcile(context.Background(), model.KindFloatingIP, floatingIP.ID); err == nil {
		t.Fatal("floating IP render failure was not returned")
	}
	failedResource, err := store.Get(context.Background(), model.KindFloatingIP, floatingIP.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := failedResource.(*model.FloatingIP)
	if failed.State != model.ResourceError || failed.FloatingStatus != model.FloatingIPError || failed.AppliedRevision != reserved.Revision {
		t.Fatalf("failed floating IP=%#v", failed)
	}

	renderer.SetFailure(model.KindFloatingIP, floatingIP.ID, nil)
	if err := controller.Reconcile(context.Background(), model.KindFloatingIP, floatingIP.ID); err != nil {
		t.Fatal(err)
	}
	recoveredResource, err := store.Get(context.Background(), model.KindFloatingIP, floatingIP.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := recoveredResource.(*model.FloatingIP)
	if recovered.State != model.ResourceReady || recovered.FloatingStatus != model.FloatingIPActive || recovered.AppliedRevision != pending.Revision {
		t.Fatalf("recovered floating IP=%#v", recovered)
	}

	tombstone, _, err := store.BeginDelete(context.Background(), model.KindFloatingIP, floatingIP.ID, recovered.Revision, "fip-delete")
	if err != nil {
		t.Fatal(err)
	}
	deleting := tombstone.(*model.FloatingIP)
	if deleting.State != model.ResourceDeleting || deleting.FloatingStatus != model.FloatingIPDown {
		t.Fatalf("deleting floating IP state=%s status=%s", deleting.State, deleting.FloatingStatus)
	}
	renderer.SetDeleteFailure(model.KindFloatingIP, floatingIP.ID, errors.New("OVN unavailable"))
	if err := controller.Delete(context.Background(), deleting); err == nil {
		t.Fatal("floating IP delete failure was not returned")
	}
	storedTombstone, err := store.Get(context.Background(), model.KindFloatingIP, floatingIP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value := storedTombstone.(*model.FloatingIP); value.State != model.ResourceDeleting || value.FloatingStatus != model.FloatingIPDown {
		t.Fatalf("retryable floating IP tombstone=%#v", value)
	}
	renderer.SetDeleteFailure(model.KindFloatingIP, floatingIP.ID, nil)
	if err := controller.Delete(context.Background(), deleting); err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(context.Background(), model.KindFloatingIP, floatingIP.ID, deleting.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), model.KindFloatingIP, floatingIP.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("deleted floating IP Get() error=%v", err)
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

func TestControllerReconcileAllRunsConservativeOperationRetention(t *testing.T) {
	base := controlstore.NewMemory()
	store := &retentionObservingStore{Store: base}
	controller := NewController(store, NewFakeRenderer(), WithOperationRetention(25, 6*time.Hour))
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	controller.now = func() time.Time { return now }
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.calls != 1 || store.keep != 25 || !store.before.Equal(now.Add(-6*time.Hour)) {
		t.Fatalf("retention calls=%d keep=%d before=%v", store.calls, store.keep, store.before)
	}
}

func TestControllerFullPassRecoversExpiredOperationsBeforeReconciling(t *testing.T) {
	base := controlstore.NewMemory()
	store := &recoveryObservingStore{Store: base}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	controller := NewController(store, NewFakeRenderer(), WithLeaseDuration(5*time.Minute))
	controller.now = func() time.Time { return now }
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := controller.ReconcilePeriodic(context.Background(), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.calls != 2 || !store.leaseCutoff.Equal(now.Add(-5*time.Minute)) || !store.recoveredAt.Equal(now) || store.limit != operationRecoveryBatch {
		t.Fatalf("recovery calls=%d cutoff=%v recovered=%v limit=%d", store.calls, store.leaseCutoff, store.recoveredAt, store.limit)
	}
}

func TestControllerFullPassRecoversSupersededQueuedReconcile(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	staleResource, _, err := store.Create(context.Background(), &model.Operation{
		Action: "reconcile", TargetKind: model.KindProject, TargetID: project.ID,
		TargetRevision: project.Revision, OperationStatus: model.OperationQueued,
	}, "stale-queued-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	stale := staleResource.(*model.Operation)
	project.Description = "new desired revision"
	updatedResource, _, err := store.Update(context.Background(), project, project.Revision, "supersede-project")
	if err != nil {
		t.Fatal(err)
	}
	updated := updatedResource.(*model.Project)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	controller := NewController(store, NewFakeRenderer(), WithLeaseDuration(5*time.Minute))
	controller.now = func() time.Time { return now }
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	staleResource, err = store.Get(context.Background(), model.KindOperation, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := staleResource.(*model.Operation)
	if failed.OperationStatus != model.OperationFailed || failed.CompletedAt == nil || !failed.CompletedAt.Equal(now) || !strings.Contains(failed.Error, "superseded before claim") {
		t.Fatalf("stale operation=%#v", failed)
	}
	ready, err := store.Get(context.Background(), model.KindProject, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.GetMetadata().Revision != updated.Revision || ready.GetMetadata().AppliedRevision != updated.Revision || ready.GetMetadata().State != model.ResourceReady {
		t.Fatalf("current project was not reconciled: %#v", ready.GetMetadata())
	}
}

func TestControllerFullPassReadsOneDependencySnapshot(t *testing.T) {
	base := controlstore.NewMemory()
	project := createProject(t, base)
	store := &snapshotObservingStore{Store: base}
	renderer := NewFakeRenderer()
	controller := NewController(store, renderer)
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := renderer.Calls(model.KindProject, project.ID); calls != 1 {
		t.Fatalf("initial renderer calls=%d want 1", calls)
	}
	if err := controller.ReconcilePeriodic(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if calls := renderer.Calls(model.KindProject, project.ID); calls != 1 {
		t.Fatalf("fresh periodic renderer calls=%d want 1", calls)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.snapshotCalls != 2 || store.listCalls != 0 {
		t.Fatalf("snapshot calls=%d list calls=%d, want 2 and 0", store.snapshotCalls, store.listCalls)
	}
	wantForced := dependencyOrder
	wantPeriodic := append([]model.Kind{model.KindOperation}, dependencyOrder...)
	for index, want := range [][]model.Kind{wantForced, wantPeriodic} {
		if len(store.kinds[index]) != len(want) {
			t.Fatalf("snapshot %d kinds=%v want=%v", index, store.kinds[index], want)
		}
		for kindIndex := range want {
			if store.kinds[index][kindIndex] != want[kindIndex] {
				t.Fatalf("snapshot %d kinds=%v want=%v", index, store.kinds[index], want)
			}
		}
	}
}

func TestControllerReconcilePeriodicUsesDurableFreshnessAcrossManagers(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := &unfencedRevisionRenderer{blockRevision: -1}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	first := NewController(store, renderer)
	second := NewController(store, renderer)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }
	if err := first.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	const freshness = 10 * time.Minute
	for pass := 0; pass < 5; pass++ {
		if err := second.ReconcilePeriodic(context.Background(), freshness); err != nil {
			t.Fatal(err)
		}
	}
	if _, calls := renderer.state(); calls != 1 {
		t.Fatalf("fresh clustered periodic passes rendered %d times, want 1", calls)
	}

	now = now.Add(freshness + time.Second)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, controller := range []*Controller{first, second} {
		go func(controller *Controller) {
			<-start
			results <- controller.ReconcilePeriodic(context.Background(), freshness)
		}(controller)
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if revision, calls := renderer.state(); revision != project.Revision || calls != 2 {
		t.Fatalf("due clustered audit revision=%d calls=%d, want revision=%d calls=2", revision, calls, project.Revision)
	}
}

func TestControllerReconcilePeriodicDoesNotHideUnreadyExactRevision(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := &unfencedRevisionRenderer{blockRevision: -1}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	controller := NewController(store, renderer)
	controller.now = func() time.Time { return now }
	if err := controller.Reconcile(context.Background(), model.KindProject, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkReconciled(context.Background(), model.KindProject, project.ID, project.Revision, errors.New("realized state lost")); err != nil {
		t.Fatal(err)
	}
	if err := controller.ReconcilePeriodic(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, calls := renderer.state(); calls != 2 {
		t.Fatalf("unready exact revision renderer calls=%d, want 2", calls)
	}
}

func TestControllerReconcilePeriodicRejectsNonPositiveFreshness(t *testing.T) {
	controller := NewController(controlstore.NewMemory(), NewFakeRenderer())
	if err := controller.ReconcilePeriodic(context.Background(), 0); err == nil {
		t.Fatal("zero periodic freshness was accepted")
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

func TestForeignCancellationLeaseFailureRemainsRunning(t *testing.T) {
	baseStore := controlstore.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &renewalObservingStore{
		Store: baseStore, renewed: make(chan time.Time, 1), fail: context.Canceled,
		cancelOnFailure: cancel,
	}
	project := createProject(t, store)
	renderer := &blockingRenderer{delegate: NewFakeRenderer(), started: make(chan struct{}), release: make(chan struct{})}
	controller := NewController(store, renderer, WithLeaseDuration(30*time.Millisecond))

	err := controller.Reconcile(ctx, model.KindProject, project.ID)
	if err == nil || !strings.Contains(err.Error(), "lost its writer lease") {
		t.Fatalf("Reconcile() error=%v, want foreign lease loss", err)
	}
	operations, listErr := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(operations) != 1 || operations[0].(*model.Operation).OperationStatus != model.OperationRunning {
		t.Fatalf("operation was completed after foreign cancellation: %#v", operations)
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
	operations, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("operations=%d, want one durable operation", len(operations))
	}
	failed := operations[0].(*model.Operation)
	if failed.OperationStatus != model.OperationFailed || failed.CompletedAt == nil || !strings.Contains(failed.Error, context.Canceled.Error()) {
		t.Fatalf("canceled operation=%#v, want immediate terminal failure", failed)
	}
	operationID := failed.ID

	close(renderer.release)
	if err := controller.ReconcilePeriodic(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	readyResource, err := store.Get(context.Background(), model.KindProject, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := readyResource.(*model.Project)
	if ready.State != model.ResourceReady || ready.AppliedRevision != ready.Revision {
		t.Fatalf("project after periodic retry=%#v", ready)
	}
	operations, err = store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("operations after retry=%d, want idempotent reuse", len(operations))
	}
	succeeded := operations[0].(*model.Operation)
	if succeeded.ID != operationID || succeeded.OperationStatus != model.OperationSucceeded || succeeded.CompletedAt == nil {
		t.Fatalf("retried operation=%#v, want same successful operation", succeeded)
	}
}

func TestLateParentCancellationStillPersistsSuccessfulOperation(t *testing.T) {
	baseStore := controlstore.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	store := &cancelingOperationUpdateStore{Store: baseStore, cancel: cancel}
	project := createProject(t, store)
	controller := NewController(store, NewFakeRenderer())

	if err := controller.Reconcile(ctx, model.KindProject, project.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() error=%v, want late context cancellation", err)
	}
	resource, err := store.Get(context.Background(), model.KindProject, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := resource.(*model.Project)
	if ready.State != model.ResourceReady || ready.AppliedRevision != ready.Revision {
		t.Fatalf("project=%#v, want realized revision preserved", ready)
	}
	operations, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].(*model.Operation).OperationStatus != model.OperationSucceeded {
		t.Fatalf("operations=%#v, want successful terminal bookkeeping", operations)
	}
}

func TestParentDeadlineImmediatelyTerminalizesOperation(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	renderer := &blockingRenderer{delegate: NewFakeRenderer(), started: make(chan struct{}), release: make(chan struct{})}
	controller := NewController(store, renderer)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := controller.Reconcile(ctx, model.KindProject, project.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile() error=%v, want parent deadline", err)
	}
	operations, listErr := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(operations) != 1 {
		t.Fatalf("operations=%d, want one", len(operations))
	}
	failed := operations[0].(*model.Operation)
	if failed.OperationStatus != model.OperationFailed || failed.CompletedAt == nil || !strings.Contains(failed.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("deadline operation=%#v, want terminal failure", failed)
	}
}

func TestOperationCompletionTimeoutIsBounded(t *testing.T) {
	baseStore := controlstore.NewMemory()
	store := &blockingOperationUpdateStore{Store: baseStore, started: make(chan struct{})}
	project := createProject(t, store)
	controller := NewController(store, NewFakeRenderer())
	controller.completionTimeout = 20 * time.Millisecond

	started := time.Now()
	err := controller.Reconcile(context.Background(), model.KindProject, project.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile() error=%v, want bounded completion deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("operation completion took %v, want bounded return", elapsed)
	}
	select {
	case <-store.started:
	default:
		t.Fatal("terminal operation update was not attempted")
	}
	operations, listErr := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(operations) != 1 || operations[0].(*model.Operation).OperationStatus != model.OperationRunning {
		t.Fatalf("operations=%#v, want lease-protected fallback for failed bookkeeping", operations)
	}
}

func TestRenderAndOperationCompletionErrorsAreBothReturned(t *testing.T) {
	baseStore := controlstore.NewMemory()
	store := &blockingOperationUpdateStore{Store: baseStore, started: make(chan struct{})}
	project := createProject(t, store)
	renderErr := errors.New("render rejected")
	renderer := NewFakeRenderer()
	renderer.SetFailure(model.KindProject, project.ID, renderErr)
	controller := NewController(store, renderer)
	controller.completionTimeout = 20 * time.Millisecond

	err := controller.Reconcile(context.Background(), model.KindProject, project.ID)
	if !errors.Is(err, renderErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reconcile() error=%v, want render and completion errors", err)
	}
}

func TestDeleteCancellationImmediatelyTerminalizesAndRetriesSameOperation(t *testing.T) {
	store := controlstore.NewMemory()
	project := createProject(t, store)
	tombstoneResource, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete-cancel")
	if err != nil {
		t.Fatal(err)
	}
	tombstone := tombstoneResource.(*model.Project)
	renderer := &blockingDeleteRenderer{delegate: NewFakeRenderer(), started: make(chan struct{}), release: make(chan struct{})}
	controller := NewController(store, renderer, WithLeaseDuration(30*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Delete(ctx, tombstone) }()
	<-renderer.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete() error=%v, want context canceled", err)
	}
	operations, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("delete operations=%d, want one", len(operations))
	}
	failed := operations[0].(*model.Operation)
	if failed.OperationStatus != model.OperationFailed || failed.CompletedAt == nil || !strings.Contains(failed.Error, context.Canceled.Error()) {
		t.Fatalf("canceled delete operation=%#v", failed)
	}
	operationID := failed.ID

	close(renderer.release)
	if err := controller.Delete(context.Background(), tombstone); err != nil {
		t.Fatal(err)
	}
	operations, err = store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("delete operations after retry=%d, want idempotent reuse", len(operations))
	}
	succeeded := operations[0].(*model.Operation)
	if succeeded.ID != operationID || succeeded.OperationStatus != model.OperationSucceeded || succeeded.CompletedAt == nil {
		t.Fatalf("retried delete operation=%#v", succeeded)
	}
}
