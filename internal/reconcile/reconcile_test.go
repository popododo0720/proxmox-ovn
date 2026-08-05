package reconcile

import (
	"context"
	"errors"
	"sync"
	"testing"

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

func createProject(t *testing.T, store controlstore.Store) *model.Project {
	t.Helper()
	resource, _, err := store.Create(context.Background(), &model.Project{Name: "tenant", PoolID: "pool"}, "create-project")
	if err != nil {
		t.Fatal(err)
	}
	return resource.(*model.Project)
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
	want := []model.Kind{model.KindProject, model.KindProviderNetwork, model.KindProviderSegment, model.KindNetwork, model.KindSubnet, model.KindSecurityGroup, model.KindSecurityGroupRule, model.KindNode}
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
