package api

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/pvnstack/proxmox-ovn/internal/controlstore"
	"github.com/pvnstack/proxmox-ovn/internal/model"
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
	if preserved.Enabled || !reflect.DeepEqual(preserved.Roles, admin.Roles) || preserved.Revision <= admin.Revision {
		t.Fatalf("preserved node=%#v admin=%#v", preserved, admin)
	}

	explicitResponse := request(t, server.RuntimeHandler(), http.MethodPost, path, map[string]any{
		"name": "pve01", "chassis_id": "chassis-01", "roles": []string{"central", "compute", "gateway"},
	}, nil)
	if explicitResponse.Code != http.StatusOK {
		t.Fatalf("explicit roles status=%d body=%s", explicitResponse.Code, explicitResponse.Body.String())
	}
	explicit := decodeData[model.Node](t, explicitResponse)
	wantRoles := []model.NodeRole{model.NodeRoleCompute, model.NodeRoleGateway, model.NodeRoleCentral}
	if explicit.Enabled || !reflect.DeepEqual(explicit.Roles, wantRoles) {
		t.Fatalf("explicit node=%#v", explicit)
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
