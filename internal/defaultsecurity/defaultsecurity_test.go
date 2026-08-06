package defaultsecurity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestDeterministicIDsAreStableUUIDs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "group", got: DefaultSecurityGroupID("project-a"), want: "a97a5a10-0ebc-5f72-b8a1-b7c33eb71208"},
		{name: "egress", got: DefaultEgressRuleID("project-a"), want: "82ec7b3f-960d-51b3-be53-475772cee46a"},
		{name: "ingress", got: DefaultIngressRuleID("project-a"), want: "0be19954-2dc1-5cfc-b867-2c81489bde36"},
	}
	for _, test := range tests {
		if test.got != test.want {
			t.Fatalf("%s ID=%q want %q", test.name, test.got, test.want)
		}
	}
	if DefaultSecurityGroupID("project-a") == DefaultSecurityGroupID("project-b") {
		t.Fatal("different projects received the same default security group ID")
	}
}

func TestEnsureCreatesBaselineAndIsIdempotent(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	manager := New(store, nil)

	first, err := manager.Ensure(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Ensure(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.ID != DefaultSecurityGroupID(project.ID) {
		t.Fatalf("default group IDs first=%q second=%q", first.ID, second.ID)
	}
	if first.Name != DefaultSecurityGroupName || first.Description != DefaultSecurityGroupDescription || !first.Stateful {
		t.Fatalf("default group=%+v", first)
	}

	rules, err := store.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules=%d want 2", len(rules))
	}
	assertBaselineRules(t, project.ID, rules)
}

func TestInspectIsReadOnlyAndReportsMissingThenReady(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	manager := New(store, nil)

	inspection, err := manager.Inspect(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantMissing := []string{DefaultSecurityGroupID(project.ID), DefaultEgressRuleID(project.ID), DefaultIngressRuleID(project.ID)}
	if inspection.Ready || inspection.BlockedReason != BlockedNone || fmt.Sprint(inspection.MissingResourceIDs) != fmt.Sprint(wantMissing) {
		t.Fatalf("initial inspection=%+v", inspection)
	}
	groups, _ := store.List(context.Background(), model.KindSecurityGroup, controlstore.ListOptions{ProjectID: project.ID})
	if len(groups) != 0 {
		t.Fatalf("Inspect created %d groups", len(groups))
	}

	reconciler := &recordingReconciler{store: store}
	manager = New(store, reconciler)
	if _, err := manager.Ensure(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	inspection, err = manager.Inspect(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Ready || inspection.BlockedReason != BlockedNone || len(inspection.MissingResourceIDs) != 0 {
		t.Fatalf("ready inspection=%+v", inspection)
	}
}

func TestEnsureIsSafeAcrossConcurrentManagers(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)

	const workers = 32
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := New(store, nil).Ensure(context.Background(), project.ID)
			errorsByWorker <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatal(err)
		}
	}
	groups, _ := store.List(context.Background(), model.KindSecurityGroup, controlstore.ListOptions{ProjectID: project.ID})
	rules, _ := store.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{ProjectID: project.ID})
	if len(groups) != 1 || len(rules) != 2 {
		t.Fatalf("groups=%d rules=%d", len(groups), len(rules))
	}
}

func TestEnsureRepairsMissingBaselineRule(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	manager := New(store, nil)
	if _, err := manager.Ensure(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	ruleResource, err := store.Get(context.Background(), model.KindSecurityGroupRule, DefaultIngressRuleID(project.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete(context.Background(), model.KindSecurityGroupRule, ruleResource.GetMetadata().ID, ruleResource.GetMetadata().Revision, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	rules, _ := store.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{ProjectID: project.ID})
	if len(rules) != 2 {
		t.Fatalf("rules=%d want 2 after repair", len(rules))
	}
	assertBaselineRules(t, project.ID, rules)
}

func TestEnsureRejectsLegacyDefaultNameCollision(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	legacy := mustCreate(t, store, &model.SecurityGroup{ProjectID: project.ID, Name: DefaultSecurityGroupName}).(*model.SecurityGroup)

	_, err := New(store, nil).Ensure(context.Background(), project.ID)
	if !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Ensure() error=%v want conflict", err)
	}
	if _, getErr := store.Get(context.Background(), model.KindSecurityGroup, DefaultSecurityGroupID(project.ID)); !errors.Is(getErr, controlstore.ErrNotFound) {
		t.Fatalf("deterministic group unexpectedly exists: %v", getErr)
	}
	if legacy.ID == DefaultSecurityGroupID(project.ID) {
		t.Fatal("test legacy group unexpectedly used deterministic ID")
	}
	inspection, inspectErr := New(store, nil).Inspect(context.Background(), project.ID)
	if inspectErr != nil || inspection.BlockedReason != BlockedNameCollision {
		t.Fatalf("Inspect()=%+v error=%v", inspection, inspectErr)
	}
}

func TestEnsureRejectsDeterministicIDWithDifferentPolicy(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	mustCreate(t, store, &model.SecurityGroup{
		Metadata: model.Metadata{ID: DefaultSecurityGroupID(project.ID)}, ProjectID: project.ID,
		Name: "not-default", Description: "untrusted",
	})

	_, err := New(store, nil).Ensure(context.Background(), project.ID)
	if !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("Ensure() error=%v want conflict", err)
	}
	inspection, inspectErr := New(store, nil).Inspect(context.Background(), project.ID)
	if inspectErr != nil || inspection.BlockedReason != BlockedMalformedGroup {
		t.Fatalf("Inspect()=%+v error=%v", inspection, inspectErr)
	}
}

func TestEnsureReconcilesRestrictiveGroupBeforeRules(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	reconciler := &recordingReconciler{store: store}

	group, err := New(store, reconciler).Ensure(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("%s/%s", model.KindSecurityGroup, DefaultSecurityGroupID(project.ID)),
		fmt.Sprintf("%s/%s", model.KindSecurityGroupRule, DefaultEgressRuleID(project.ID)),
		fmt.Sprintf("%s/%s", model.KindSecurityGroupRule, DefaultIngressRuleID(project.ID)),
	}
	if fmt.Sprint(reconciler.calls) != fmt.Sprint(want) {
		t.Fatalf("reconcile order=%v want %v", reconciler.calls, want)
	}
	if group.State != model.ResourceReady || group.AppliedRevision != group.Revision {
		t.Fatalf("group was not required ready: %+v", group.Metadata)
	}
	callCount := len(reconciler.calls)
	if _, err := New(store, reconciler).Ensure(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	if len(reconciler.calls) != callCount {
		t.Fatalf("ready policy was reconciled again: before=%d after=%d", callCount, len(reconciler.calls))
	}
}

func TestEnsureRequiresSuccessfulRealization(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	want := errors.New("OVN unavailable")
	reconciler := ReconcilerFunc(func(context.Context, model.Kind, string) error { return want })

	_, err := New(store, reconciler).Ensure(context.Background(), project.ID)
	if !errors.Is(err, want) {
		t.Fatalf("Ensure() error=%v want %v", err, want)
	}
	if _, getErr := store.Get(context.Background(), model.KindSecurityGroupRule, DefaultEgressRuleID(project.ID)); !errors.Is(getErr, controlstore.ErrNotFound) {
		t.Fatalf("allow rule exists after group realization failed: %v", getErr)
	}
}

func TestEnsureAllDoesNotBackfillExistingEmptyPorts(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	network := mustCreate(t, store, &model.Network{ProjectID: project.ID, Name: "private"}).(*model.Network)
	port := mustCreate(t, store, &model.Port{ProjectID: project.ID, NetworkID: network.ID, Name: "legacy", MACAddress: "02:00:00:00:00:01", AdminStateUp: true}).(*model.Port)

	if err := New(store, nil).EnsureAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(context.Background(), model.KindPort, port.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.(*model.Port).SecurityGroupIDs) != 0 {
		t.Fatalf("legacy port security groups=%v want unchanged empty list", current.(*model.Port).SecurityGroupIDs)
	}
}

func TestProbeRequiresCompleteRealizedPolicy(t *testing.T) {
	store := controlstore.NewMemory()
	project := mustCreate(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}).(*model.Project)
	manager := New(store, nil)
	if err := manager.Probe(context.Background()); err == nil {
		t.Fatal("Probe reported a missing policy ready")
	}
	reconciler := &recordingReconciler{store: store}
	manager = New(store, reconciler)
	if _, err := manager.Ensure(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Probe(context.Background()); err != nil {
		t.Fatalf("Probe rejected realized policy: %v", err)
	}
}

func TestProbeUsesSingleSnapshotForMultipleProjects(t *testing.T) {
	store := controlstore.NewMemory()
	reconciler := &recordingReconciler{store: store}
	manager := New(store, reconciler)
	for index := 0; index < 3; index++ {
		project := mustCreate(t, store, &model.Project{Name: fmt.Sprintf("tenant-%d", index), PoolID: fmt.Sprintf("pool-%d", index)}).(*model.Project)
		if _, err := manager.Ensure(context.Background(), project.ID); err != nil {
			t.Fatal(err)
		}
	}
	observed := &snapshotOnlyStore{Store: store}
	if err := New(observed, nil).Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if observed.snapshots != 1 || observed.gets != 0 || observed.lists != 0 {
		t.Fatalf("snapshots=%d gets=%d lists=%d", observed.snapshots, observed.gets, observed.lists)
	}
	observed.snapshots, observed.gets, observed.lists = 0, 0, 0
	reconcileCalls := len(reconciler.calls)
	if err := New(observed, reconciler).EnsureAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if observed.snapshots != 1 || observed.gets != 0 || observed.lists != 0 || len(reconciler.calls) != reconcileCalls {
		t.Fatalf("EnsureAll snapshots=%d gets=%d lists=%d reconciles=%d", observed.snapshots, observed.gets, observed.lists, len(reconciler.calls)-reconcileCalls)
	}
}

func TestReservedResources(t *testing.T) {
	projectID := "project-a"
	if !IsReserved(desiredGroup(projectID)) || !IsReserved(desiredEgressRule(projectID)) || !IsReserved(desiredIngressRule(projectID)) {
		t.Fatal("baseline resources were not reserved")
	}
	if IsReserved(&model.SecurityGroup{Metadata: model.Metadata{ID: "custom"}, ProjectID: projectID, Name: "custom"}) {
		t.Fatal("custom security group was reserved")
	}
}

type ReconcilerFunc func(context.Context, model.Kind, string) error

func (function ReconcilerFunc) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	return function(ctx, kind, id)
}

type recordingReconciler struct {
	store controlstore.Store
	calls []string
}

type snapshotOnlyStore struct {
	controlstore.Store
	snapshots int
	gets      int
	lists     int
}

func (store *snapshotOnlyStore) Snapshot(ctx context.Context, kinds []model.Kind, options controlstore.ListOptions) (controlstore.ResourceSnapshot, error) {
	store.snapshots++
	return store.Store.Snapshot(ctx, kinds, options)
}

func (store *snapshotOnlyStore) Get(context.Context, model.Kind, string) (model.Resource, error) {
	store.gets++
	return nil, errors.New("Probe must not use Get")
}

func (store *snapshotOnlyStore) List(context.Context, model.Kind, controlstore.ListOptions) ([]model.Resource, error) {
	store.lists++
	return nil, errors.New("Probe must not use List")
}

func (r *recordingReconciler) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	r.calls = append(r.calls, fmt.Sprintf("%s/%s", kind, id))
	resource, err := r.store.Get(ctx, kind, id)
	if err != nil {
		return err
	}
	_, err = r.store.MarkReconciled(ctx, kind, id, resource.GetMetadata().Revision, nil)
	return err
}

func mustCreate(t *testing.T, store controlstore.Store, resource model.Resource) model.Resource {
	t.Helper()
	created, replayed, err := store.Create(context.Background(), resource, "")
	if err != nil || replayed {
		t.Fatalf("Create(%s) replayed=%v: %v", resource.ResourceKind(), replayed, err)
	}
	return created
}

func assertBaselineRules(t *testing.T, projectID string, resources []model.Resource) {
	t.Helper()
	byID := make(map[string]*model.SecurityGroupRule, len(resources))
	for _, resource := range resources {
		byID[resource.GetMetadata().ID] = resource.(*model.SecurityGroupRule)
	}
	egress := byID[DefaultEgressRuleID(projectID)]
	if egress == nil || egress.Direction != model.DirectionEgress || egress.EtherType != model.EtherTypeIPv4 ||
		egress.Action != model.ActionAllow || egress.Protocol != "" || egress.RemoteCIDR != "" || egress.RemoteGroupID != "" ||
		egress.Description != DefaultEgressDescription {
		t.Fatalf("egress rule=%+v", egress)
	}
	ingress := byID[DefaultIngressRuleID(projectID)]
	if ingress == nil || ingress.Direction != model.DirectionIngress || ingress.EtherType != model.EtherTypeIPv4 ||
		ingress.Action != model.ActionAllow || ingress.RemoteGroupID != DefaultSecurityGroupID(projectID) ||
		ingress.Protocol != "" || ingress.RemoteCIDR != "" || ingress.Description != DefaultIngressDescription {
		t.Fatalf("ingress rule=%+v", ingress)
	}
}
