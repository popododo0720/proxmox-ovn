package api

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
	"github.com/popododo0720/proxmox-ovn/internal/reconcile"
)

func TestRuntimeNodeHeartbeatRegistersAndPreservesAdminState(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	path := "/api/v1/runtime/nodes/heartbeat"

	registeredResponse := request(t, server.RuntimeHandler(), http.MethodPost, path, map[string]any{
		"name": "pve01", "chassis_id": "chassis-01",
	}, nil)
	if registeredResponse.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", registeredResponse.Code, registeredResponse.Body.String())
	}
	registered := decodeData[model.Node](t, registeredResponse)
	if !registered.Enabled || registered.LastSeenAt == nil || !reflect.DeepEqual(registered.Roles, []model.NodeRole{model.NodeRoleCompute}) || registered.State != model.ResourceReady {
		t.Fatalf("registered node=%#v", registered)
	}

	registered.Enabled = false
	registered.Roles = []model.NodeRole{model.NodeRoleGateway}
	adminResource, _, err := store.Update(context.Background(), &registered, registered.Revision, "admin-node-edit")
	if err != nil {
		t.Fatal(err)
	}
	admin := adminResource.(*model.Node)
	preservedResponse := request(t, server.RuntimeHandler(), http.MethodPost, path, map[string]any{
		"name": "pve01", "chassis_id": "chassis-01",
	}, nil)
	if preservedResponse.Code != http.StatusOK {
		t.Fatalf("preserve status=%d body=%s", preservedResponse.Code, preservedResponse.Body.String())
	}
	preserved := decodeData[model.Node](t, preservedResponse)
	if preserved.Enabled || !reflect.DeepEqual(preserved.Roles, admin.Roles) || preserved.Revision != admin.Revision || preserved.State != admin.State || preserved.AppliedRevision != admin.AppliedRevision || !preserved.UpdatedAt.Equal(admin.UpdatedAt) {
		t.Fatalf("preserved node=%#v admin=%#v", preserved, admin)
	}
	if preserved.LastSeenAt == nil || admin.LastSeenAt == nil || !preserved.LastSeenAt.After(*admin.LastSeenAt) {
		t.Fatalf("heartbeat observation did not advance last_seen_at: preserved=%#v admin=%#v", preserved.LastSeenAt, admin.LastSeenAt)
	}

	explicitResponse := request(t, server.RuntimeHandler(), http.MethodPost, path, map[string]any{
		"name": "pve01", "chassis_id": "chassis-01", "roles": []string{"central", "compute", "gateway"},
	}, nil)
	if explicitResponse.Code != http.StatusOK {
		t.Fatalf("explicit roles status=%d body=%s", explicitResponse.Code, explicitResponse.Body.String())
	}
	explicit := decodeData[model.Node](t, explicitResponse)
	wantRoles := []model.NodeRole{model.NodeRoleCentral, model.NodeRoleCompute, model.NodeRoleGateway}
	if explicit.Enabled || !reflect.DeepEqual(explicit.Roles, wantRoles) {
		t.Fatalf("explicit node=%#v", explicit)
	}
}

func TestRuntimeNodeHeartbeatDoesNotRewriteLexicallyDecodedRoles(t *testing.T) {
	store := controlstore.NewMemory()
	createdResource, _, err := store.Create(context.Background(), &model.Node{
		Name:      "pve01",
		ChassisID: "chassis-01",
		Roles: []model.NodeRole{
			model.NodeRoleCentral,
			model.NodeRoleCompute,
			model.NodeRoleGateway,
		},
		Enabled: true,
	}, "lexically-ordered-node")
	if err != nil {
		t.Fatal(err)
	}
	created := createdResource.(*model.Node)
	server := testServer(t, store, nil)

	response := request(t, server.RuntimeHandler(), http.MethodPost, "/api/v1/runtime/nodes/heartbeat", map[string]any{
		"name": "pve01", "chassis_id": "chassis-01", "roles": []string{"gateway", "compute", "central"},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("heartbeat status=%d body=%s", response.Code, response.Body.String())
	}
	observed := decodeData[model.Node](t, response)
	if observed.Revision != created.Revision {
		t.Fatalf("unchanged lexical roles advanced revision: got %d want %d", observed.Revision, created.Revision)
	}
	wantRoles := []model.NodeRole{model.NodeRoleCentral, model.NodeRoleCompute, model.NodeRoleGateway}
	if !reflect.DeepEqual(observed.Roles, wantRoles) {
		t.Fatalf("roles=%#v want %#v", observed.Roles, wantRoles)
	}
}

func TestRuntimeNodeHeartbeatsDoNotAmplifyDesiredRevisionsOrOperations(t *testing.T) {
	store := controlstore.NewMemory()
	renderer := reconcile.NewFakeRenderer()
	controller := reconcile.NewController(store, renderer)
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	server, err := New(Options{Store: store, Reconciler: controller, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/runtime/nodes/heartbeat"
	body := map[string]any{"name": "pve01", "chassis_id": "chassis-01", "roles": []string{"compute"}}
	registered := request(t, server.RuntimeHandler(), http.MethodPost, path, body, nil)
	if registered.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", registered.Code, registered.Body.String())
	}
	initial := decodeData[model.Node](t, registered)
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	for range 6 {
		now = now.Add(30 * time.Second)
		response := request(t, server.RuntimeHandler(), http.MethodPost, path, body, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("heartbeat status=%d body=%s", response.Code, response.Body.String())
		}
		observed := decodeData[model.Node](t, response)
		if observed.Revision != initial.Revision || observed.AppliedRevision != initial.AppliedRevision {
			t.Fatalf("observation changed desired metadata: initial=%#v observed=%#v", initial.Metadata, observed.Metadata)
		}
		if err := controller.ReconcileAll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	operations, err := store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("unchanged heartbeats created %d operations, want 1: %#v", len(operations), operations)
	}

	// A real desired role change still advances the revision and receives its
	// own reconcile audit record.
	now = now.Add(30 * time.Second)
	changed := request(t, server.RuntimeHandler(), http.MethodPost, path, map[string]any{
		"name": "pve01", "chassis_id": "chassis-01", "roles": []string{"compute", "gateway"},
	}, nil)
	if changed.Code != http.StatusOK {
		t.Fatalf("role update status=%d body=%s", changed.Code, changed.Body.String())
	}
	changedNode := decodeData[model.Node](t, changed)
	if changedNode.Revision != initial.Revision+1 {
		t.Fatalf("role update revision=%d want %d", changedNode.Revision, initial.Revision+1)
	}
	if err := controller.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	operations, err = store.List(context.Background(), model.KindOperation, controlstore.ListOptions{})
	if err != nil || len(operations) != 2 {
		t.Fatalf("real change operations=%d err=%v: %#v", len(operations), err, operations)
	}
}

func TestRuntimeNodeHeartbeatRejectsIdentityCollisionAndInvalidRoles(t *testing.T) {
	store := controlstore.NewMemory()
	_, _, err := store.Create(context.Background(), &model.Node{
		Name: "pve01", ChassisID: "chassis-original", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true,
	}, "existing-node")
	if err != nil {
		t.Fatal(err)
	}
	server := testServer(t, store, nil)
	path := "/api/v1/runtime/nodes/heartbeat"

	collision := request(t, server.RuntimeHandler(), http.MethodPost, path, map[string]any{
		"name": "pve01", "chassis_id": "chassis-other",
	}, nil)
	if collision.Code != http.StatusConflict {
		t.Fatalf("collision status=%d body=%s", collision.Code, collision.Body.String())
	}
	invalid := request(t, server.RuntimeHandler(), http.MethodPost, path, map[string]any{
		"name": "pve02", "chassis_id": "chassis-02", "roles": []string{"compute", "invalid"},
	}, nil)
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid roles status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	empty := request(t, server.RuntimeHandler(), http.MethodPost, path, map[string]any{
		"name": "pve02", "chassis_id": "chassis-02", "roles": []string{},
	}, nil)
	if empty.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty roles status=%d body=%s", empty.Code, empty.Body.String())
	}
}

func TestRuntimeNodeHeartbeatIsNotExposedOnTCP(t *testing.T) {
	store := controlstore.NewMemory()
	provider := &lifecycleSessionProvider{authenticated: true, csrf: "csrf", session: Session{User: "root@pam", Permissions: map[string]any{"/": map[string]bool{"Sys.Modify": true}}}}
	server := testServer(t, store, provider)
	response := request(t, server, http.MethodPost, "/api/v1/runtime/nodes/heartbeat", map[string]any{
		"name": "pve01", "chassis_id": "chassis-01",
	}, map[string]string{PVNCSRFHeader: "csrf"})
	if response.Code != http.StatusNotFound {
		t.Fatalf("TCP heartbeat status=%d body=%s", response.Code, response.Body.String())
	}
}
