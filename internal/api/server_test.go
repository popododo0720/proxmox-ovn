package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func testServer(t *testing.T, store controlstore.Store, provider SessionProvider) *Server {
	t.Helper()
	server, err := New(Options{Store: store, SessionProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func request(t *testing.T, handler http.Handler, method, target string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeData[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var envelope struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return envelope.Data
}

func createAPIProject(t *testing.T, server *Server) model.Project {
	t.Helper()
	recorder := request(t, server, http.MethodPost, "/api/v1/projects", map[string]any{"name": "tenant", "pool_id": "pool-tenant"}, map[string]string{"Idempotency-Key": "project-create"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST project status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	return decodeData[model.Project](t, recorder)
}

func TestHealthAndSession(t *testing.T) {
	provider := SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "root@pam", CSRFToken: "csrf", Permissions: map[string]any{"/": map[string]bool{"SDN.Audit": true}}, Cluster: "lab"}, nil
	})
	server := testServer(t, controlstore.NewMemory(), provider)
	health := request(t, server, http.MethodGet, "/api/v1/health", nil, nil)
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d", health.Code)
	}
	session := request(t, server, http.MethodGet, "/api/v1/session", nil, nil)
	if session.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", session.Code, session.Body.String())
	}
	data := decodeData[Session](t, session)
	if data.User != "root@pam" || data.CSRFToken != "csrf" || data.Cluster != "lab" {
		t.Fatalf("session data = %#v", data)
	}
	unauthenticated := testServer(t, controlstore.NewMemory(), SessionProviderFunc(func(context.Context, *http.Request) (Session, error) { return Session{}, ErrUnauthenticated }))
	response := request(t, unauthenticated, http.MethodGet, "/api/v1/session", nil, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated session status=%d", response.Code)
	}
	health = request(t, unauthenticated, http.MethodGet, "/api/v1/health", nil, nil)
	if health.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated health status=%d body=%s", health.Code, health.Body.String())
	}
}

func TestOperationsListIsRecentAndBounded(t *testing.T) {
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	store := controlstore.NewMemory(controlstore.WithClock(func() time.Time { return now }))
	for revision := int64(1); revision <= 105; revision++ {
		_, _, err := store.Create(context.Background(), &model.Operation{
			Action: "bind", TargetKind: model.KindPort, TargetID: "port-a", TargetRevision: revision,
		}, fmt.Sprintf("operation-%03d", revision))
		if err != nil {
			t.Fatalf("create operation %d: %v", revision, err)
		}
		now = now.Add(time.Second)
	}
	server := testServer(t, store, nil)

	defaultResponse := request(t, server, http.MethodGet, "/api/v1/operations", nil, nil)
	if defaultResponse.Code != http.StatusOK {
		t.Fatalf("default list status=%d body=%s", defaultResponse.Code, defaultResponse.Body.String())
	}
	defaults := decodeData[[]model.Operation](t, defaultResponse)
	if len(defaults) != defaultOperationsLimit || defaults[0].TargetRevision != 105 || defaults[len(defaults)-1].TargetRevision != 6 {
		t.Fatalf("default recent operations len=%d first=%d last=%d", len(defaults), defaults[0].TargetRevision, defaults[len(defaults)-1].TargetRevision)
	}

	limitedResponse := request(t, server, http.MethodGet, "/api/v1/operations?limit=2", nil, nil)
	if limitedResponse.Code != http.StatusOK {
		t.Fatalf("limited list status=%d body=%s", limitedResponse.Code, limitedResponse.Body.String())
	}
	limited := decodeData[[]model.Operation](t, limitedResponse)
	if len(limited) != 2 || limited[0].TargetRevision != 105 || limited[1].TargetRevision != 104 {
		t.Fatalf("limited recent operations=%#v", limited)
	}

	for _, query := range []string{"0", "501", "invalid", "", "1&limit=2"} {
		response := request(t, server, http.MethodGet, "/api/v1/operations?limit="+query, nil, nil)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("limit %q status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
}

func TestCRUDRevisionAndIdempotency(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	project := createAPIProject(t, server)
	if project.Revision != 1 {
		t.Fatalf("project revision = %d", project.Revision)
	}

	replay := request(t, server, http.MethodPost, "/api/v1/projects", map[string]any{"name": "tenant", "pool_id": "pool-tenant"}, map[string]string{"Idempotency-Key": "project-create"})
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}

	project.Description = "changed"
	updatedResponse := request(t, server, http.MethodPut, "/api/v1/projects/"+project.ID, project, map[string]string{"Idempotency-Key": "project-update", "If-Match": `"1"`})
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", updatedResponse.Code, updatedResponse.Body.String())
	}
	updated := decodeData[model.Project](t, updatedResponse)
	if updated.Revision != 2 || updated.Description != "changed" || updatedResponse.Header().Get("ETag") != `"2"` {
		t.Fatalf("updated=%#v etag=%s", updated, updatedResponse.Header().Get("ETag"))
	}
	updateReplay := request(t, server, http.MethodPut, "/api/v1/projects/"+project.ID, project, map[string]string{"Idempotency-Key": "project-update", "If-Match": `"1"`})
	if updateReplay.Code != http.StatusOK || updateReplay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("update replay status=%d headers=%v body=%s", updateReplay.Code, updateReplay.Header(), updateReplay.Body.String())
	}

	stale := request(t, server, http.MethodPut, "/api/v1/projects/"+project.ID, project, map[string]string{"Idempotency-Key": "project-update-stale", "If-Match": `"1"`})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
	missingPrecondition := request(t, server, http.MethodDelete, "/api/v1/projects/"+project.ID, nil, map[string]string{"Idempotency-Key": "project-delete"})
	if missingPrecondition.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing precondition status=%d", missingPrecondition.Code)
	}
	deleted := request(t, server, http.MethodDelete, "/api/v1/projects/"+project.ID, nil, map[string]string{"Idempotency-Key": "project-delete", "If-Match": `"2"`})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	deleteReplay := request(t, server, http.MethodDelete, "/api/v1/projects/"+project.ID, nil, map[string]string{"Idempotency-Key": "project-delete", "If-Match": `"2"`})
	if deleteReplay.Code != http.StatusNoContent || deleteReplay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("delete replay status=%d headers=%v", deleteReplay.Code, deleteReplay.Header())
	}
}

func TestCRUDValidationReferencesAndListFilter(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	missingKey := request(t, server, http.MethodPost, "/api/v1/projects", map[string]any{"name": "tenant", "pool_id": "pool"}, nil)
	if missingKey.Code != http.StatusBadRequest {
		t.Fatalf("missing key status=%d", missingKey.Code)
	}
	unknownField := request(t, server, http.MethodPost, "/api/v1/projects", map[string]any{"name": "tenant", "pool_id": "pool", "surprise": true}, map[string]string{"Idempotency-Key": "bad-json"})
	if unknownField.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", unknownField.Code, unknownField.Body.String())
	}
	invalid := request(t, server, http.MethodPost, "/api/v1/projects", map[string]any{"name": "bad name", "pool_id": "pool"}, map[string]string{"Idempotency-Key": "invalid"})
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validation status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	project := createAPIProject(t, server)
	networkResponse := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"project_id": project.ID, "name": "private"}, map[string]string{"Idempotency-Key": "network"})
	if networkResponse.Code != http.StatusCreated {
		t.Fatalf("network status=%d body=%s", networkResponse.Code, networkResponse.Body.String())
	}
	missingProject := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"project_id": "missing", "name": "orphan"}, map[string]string{"Idempotency-Key": "orphan"})
	if missingProject.Code != http.StatusConflict {
		t.Fatalf("missing ref status=%d body=%s", missingProject.Code, missingProject.Body.String())
	}
	list := request(t, server, http.MethodGet, "/api/v1/networks?project_id="+project.ID, nil, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d", list.Code)
	}
	data := decodeData[[]model.Network](t, list)
	if len(data) != 1 || data[0].MTU != 1400 {
		t.Fatalf("networks = %#v", data)
	}
	operationsWrite := request(t, server, http.MethodPost, "/api/v1/operations", map[string]any{}, map[string]string{"Idempotency-Key": "operation"})
	if operationsWrite.Code != http.StatusMethodNotAllowed {
		t.Fatalf("operation write status=%d", operationsWrite.Code)
	}
}

func setupPort(t *testing.T, store controlstore.Store, nodeID string, vmid int, nic, mac, requested string, status model.PortBindingStatus, adminUp bool) *model.Port {
	t.Helper()
	ensureAPINode(t, store, nodeID, requested)
	projects, _ := store.List(context.Background(), model.KindProject, controlstore.ListOptions{})
	var project *model.Project
	var network *model.Network
	if len(projects) == 0 {
		created, _, err := store.Create(context.Background(), &model.Project{Name: "tenant", PoolID: "pool"}, "runtime-project")
		if err != nil {
			t.Fatal(err)
		}
		project = created.(*model.Project)
		created, _, err = store.Create(context.Background(), &model.Network{ProjectID: project.ID, Name: "private"}, "runtime-network")
		if err != nil {
			t.Fatal(err)
		}
		network = created.(*model.Network)
	} else {
		project = projects[0].(*model.Project)
		networks, _ := store.List(context.Background(), model.KindNetwork, controlstore.ListOptions{ProjectID: project.ID})
		network = networks[0].(*model.Network)
	}
	created, _, err := store.Create(context.Background(), &model.Port{ProjectID: project.ID, NetworkID: network.ID, Name: fmt.Sprintf("vm-%d-%s-%s", vmid, nic, nodeID), MACAddress: mac, AdminStateUp: adminUp, BindingStatus: status, NodeID: nodeID, VMID: vmid, NIC: nic, LSPName: "lsp-" + nodeID, Generation: 7, RequestedChassis: requested}, "port-"+nodeID+nic)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.MarkReconciled(context.Background(), model.KindPort, created.GetMetadata().ID, created.GetMetadata().Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ready.(*model.Port)
}

func ensureAPINode(t *testing.T, store controlstore.Store, id string, requestedChassis ...string) *model.Node {
	t.Helper()
	resource, err := store.Get(context.Background(), model.KindNode, id)
	if err == nil {
		return resource.(*model.Node)
	}
	if !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatal(err)
	}
	chassisID := "chassis-" + id
	if len(requestedChassis) > 0 && requestedChassis[0] != "" {
		chassisID = requestedChassis[0]
	}
	created, _, err := store.Create(context.Background(), &model.Node{
		Metadata: model.Metadata{ID: id}, Name: id, ChassisID: chassisID,
		Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true,
	}, "test-node-"+id)
	if err != nil {
		t.Fatal(err)
	}
	return created.(*model.Node)
}

func TestRuntimePortResolverUnixOnlyBypass(t *testing.T) {
	store := controlstore.NewMemory()
	nodeResource, _, err := store.Create(context.Background(), &model.Node{Name: "pve01", ChassisID: "chassis-01", Enabled: true}, "node")
	if err != nil {
		t.Fatal(err)
	}
	port := setupPort(t, store, nodeResource.GetMetadata().ID, 100, "net0", "02:00:00:00:00:01", "chassis-01", model.PortBinding, true)
	reject := SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{}, errors.New("no browser session")
	})
	server := testServer(t, store, reject)
	tcp := request(t, server, http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve01&vmid=100&nic=net0", nil, nil)
	if tcp.Code != http.StatusUnauthorized {
		t.Fatalf("TCP resolver status=%d body=%s", tcp.Code, tcp.Body.String())
	}
	unix := request(t, server.RuntimeHandler(), http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve01&vmid=100&nic=net0", nil, nil)
	if unix.Code != http.StatusOK {
		t.Fatalf("Unix resolver status=%d body=%s", unix.Code, unix.Body.String())
	}
	var resolved resolvedPort
	if err := json.Unmarshal(unix.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.PortID != port.ID || resolved.LSPName != port.LSPName || resolved.Generation != 7 || resolved.RequestedChassis != "chassis-01" {
		t.Fatalf("resolved = %#v", resolved)
	}
}

func TestRuntimePortResolverErrors(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	missing := request(t, server.RuntimeHandler(), http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve01&vmid=100&nic=net0", nil, nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d", missing.Code)
	}
	setupPort(t, store, "node-a", 100, "net0", "02:00:00:00:00:01", "pve01", model.PortBindingError, true)
	cleanup := request(t, server.RuntimeHandler(), http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve01&vmid=100&nic=net0", nil, nil)
	if cleanup.Code != http.StatusOK {
		t.Fatalf("cleanup resolution status=%d body=%s", cleanup.Code, cleanup.Body.String())
	}
	var cleanupPort resolvedPort
	if err := json.Unmarshal(cleanup.Body.Bytes(), &cleanupPort); err != nil || cleanupPort.Status != model.PortBindingError {
		t.Fatalf("cleanup resolution = %#v err=%v", cleanupPort, err)
	}
}

func TestNewRequiresStore(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted nil store")
	}
}

func TestPermissionEnforcement(t *testing.T) {
	store := controlstore.NewMemory()
	projectResource, _, err := store.Create(context.Background(), &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "project")
	if err != nil {
		t.Fatal(err)
	}
	networkResource, _, err := store.Create(context.Background(), &model.Network{ProjectID: projectResource.GetMetadata().ID, Name: "private"}, "network")
	if err != nil {
		t.Fatal(err)
	}
	permissions := map[string]any{}
	provider := SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "user@pam", Permissions: permissions}, nil
	})
	server := testServer(t, store, provider)
	filteredRead := request(t, server, http.MethodGet, "/api/v1/projects", nil, nil)
	if filteredRead.Code != http.StatusOK || len(decodeData[[]model.Project](t, filteredRead)) != 0 {
		t.Fatalf("read without audit status=%d body=%s", filteredRead.Code, filteredRead.Body.String())
	}
	permissions["/"] = map[string]bool{"SDN.Audit": true}
	allowedRead := request(t, server, http.MethodGet, "/api/v1/projects", nil, nil)
	if allowedRead.Code != http.StatusOK {
		t.Fatalf("read with audit status=%d body=%s", allowedRead.Code, allowedRead.Body.String())
	}
	deniedWrite := request(t, server, http.MethodPost, "/api/v1/provider-networks", map[string]any{"name": "provider"}, map[string]string{"Idempotency-Key": "provider"})
	if deniedWrite.Code != http.StatusForbidden {
		t.Fatalf("write without allocate status=%d", deniedWrite.Code)
	}
	permissions["/"] = map[string]bool{"SDN.Audit": true, "SDN.Allocate": true}
	deniedGlobal := request(t, server, http.MethodPost, "/api/v1/provider-networks", map[string]any{"name": "provider"}, map[string]string{"Idempotency-Key": "provider"})
	if deniedGlobal.Code != http.StatusForbidden {
		t.Fatalf("provider without Sys.Modify status=%d body=%s", deniedGlobal.Code, deniedGlobal.Body.String())
	}
	permissions["/"] = map[string]bool{"SDN.Audit": true, "SDN.Allocate": true, "Sys.Modify": true}
	allowedGlobal := request(t, server, http.MethodPost, "/api/v1/provider-networks", map[string]any{"name": "provider"}, map[string]string{"Idempotency-Key": "provider"})
	if allowedGlobal.Code != http.StatusCreated {
		t.Fatalf("provider with global permission status=%d body=%s", allowedGlobal.Code, allowedGlobal.Body.String())
	}

	permissions["/"] = map[string]bool{"SDN.Audit": true, "Sys.Modify": true}
	permissions["/pool/pool-tenant"] = map[string]bool{"SDN.Allocate": true}
	portBody := map[string]any{"project_id": projectResource.GetMetadata().ID, "network_id": networkResource.GetMetadata().ID, "name": "vm100-net0", "mac_address": "02:00:00:00:00:10", "admin_state_up": true}
	allowedPort := request(t, server, http.MethodPost, "/api/v1/ports", portBody, map[string]string{"Idempotency-Key": "port"})
	if allowedPort.Code != http.StatusCreated {
		t.Fatalf("unattached port with project allocation status=%d body=%s", allowedPort.Code, allowedPort.Body.String())
	}

	deniedSystem := request(t, server, http.MethodPost, "/api/v1/nodes", map[string]any{"name": "pve01", "chassis_id": "chassis-01", "enabled": true}, map[string]string{"Idempotency-Key": "node"})
	if deniedSystem.Code != http.StatusForbidden {
		t.Fatalf("node without global allocation status=%d body=%s", deniedSystem.Code, deniedSystem.Body.String())
	}
}

func TestScopedReadFilteringAndCrossProjectIsolation(t *testing.T) {
	store := controlstore.NewMemory()
	projectAResource, _, err := store.Create(context.Background(), &model.Project{Name: "tenant-a", PoolID: "pool-a"}, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	projectBResource, _, err := store.Create(context.Background(), &model.Project{Name: "tenant-b", PoolID: "pool-b"}, "project-b")
	if err != nil {
		t.Fatal(err)
	}
	projectA := projectAResource.(*model.Project)
	projectB := projectBResource.(*model.Project)
	networkAResource, _, err := store.Create(context.Background(), &model.Network{ProjectID: projectA.ID, Name: "private-a"}, "network-a")
	if err != nil {
		t.Fatal(err)
	}
	networkBResource, _, err := store.Create(context.Background(), &model.Network{ProjectID: projectB.ID, Name: "private-b"}, "network-b")
	if err != nil {
		t.Fatal(err)
	}
	networkA := networkAResource.(*model.Network)
	networkB := networkBResource.(*model.Network)

	permissions := map[string]any{
		"/pool/pool-a": map[string]bool{"SDN.Audit": true, "SDN.Allocate": true},
	}
	server := testServer(t, store, SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "tenant-a@pve", Permissions: permissions}, nil
	}))

	projects := request(t, server, http.MethodGet, "/api/v1/projects", nil, nil)
	visibleProjects := decodeData[[]model.Project](t, projects)
	if projects.Code != http.StatusOK || len(visibleProjects) != 1 || visibleProjects[0].ID != projectA.ID {
		t.Fatalf("visible projects status=%d data=%#v", projects.Code, visibleProjects)
	}
	networks := request(t, server, http.MethodGet, "/api/v1/networks", nil, nil)
	visibleNetworks := decodeData[[]model.Network](t, networks)
	if networks.Code != http.StatusOK || len(visibleNetworks) != 1 || visibleNetworks[0].ID != networkA.ID {
		t.Fatalf("visible networks status=%d data=%#v", networks.Code, visibleNetworks)
	}
	hiddenGet := request(t, server, http.MethodGet, "/api/v1/networks/"+networkB.ID, nil, nil)
	if hiddenGet.Code != http.StatusNotFound {
		t.Fatalf("cross-project GET status=%d body=%s", hiddenGet.Code, hiddenGet.Body.String())
	}
	missingGet := request(t, server, http.MethodGet, "/api/v1/networks/does-not-exist", nil, nil)
	if missingGet.Code != http.StatusNotFound || hiddenGet.Body.String() != missingGet.Body.String() {
		t.Fatalf("hidden and missing GETs differ: hidden=%q missing=%q", hiddenGet.Body.String(), missingGet.Body.String())
	}

	createdA := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"project_id": projectA.ID, "name": "private-a-2"}, map[string]string{"Idempotency-Key": "create-a"})
	if createdA.Code != http.StatusCreated {
		t.Fatalf("create in allowed pool status=%d body=%s", createdA.Code, createdA.Body.String())
	}
	createdB := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"project_id": projectB.ID, "name": "private-b-2"}, map[string]string{"Idempotency-Key": "create-b"})
	if createdB.Code != http.StatusForbidden {
		t.Fatalf("create in denied pool status=%d body=%s", createdB.Code, createdB.Body.String())
	}

	networkB.ProjectID = projectA.ID
	steal := request(t, server, http.MethodPut, "/api/v1/networks/"+networkB.ID, networkB, map[string]string{"Idempotency-Key": "steal-b", "If-Match": `"1"`})
	if steal.Code != http.StatusForbidden {
		t.Fatalf("cross-project move status=%d body=%s", steal.Code, steal.Body.String())
	}
	networkA.ProjectID = projectB.ID
	moveOut := request(t, server, http.MethodPut, "/api/v1/networks/"+networkA.ID, networkA, map[string]string{"Idempotency-Key": "move-a", "If-Match": `"1"`})
	if moveOut.Code != http.StatusForbidden {
		t.Fatalf("move into denied pool status=%d body=%s", moveOut.Code, moveOut.Body.String())
	}
}

func TestServerManagedMetadataAndPortBindingFields(t *testing.T) {
	store := controlstore.NewMemory()
	projectResource, _, err := store.Create(context.Background(), &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "project")
	if err != nil {
		t.Fatal(err)
	}
	project := projectResource.(*model.Project)
	networkResource, _, err := store.Create(context.Background(), &model.Network{ProjectID: project.ID, Name: "private"}, "network")
	if err != nil {
		t.Fatal(err)
	}
	network := networkResource.(*model.Network)
	ensureAPINode(t, store, "pve01", "chassis-01")
	permissions := map[string]any{
		"/pool/pool-tenant": map[string]bool{"SDN.Audit": true, "SDN.Allocate": true, "SDN.Use": true},
		"/vms/100":          map[string]bool{"VM.Config.Network": true},
	}
	server := testServer(t, store, SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "tenant@pve", Permissions: permissions}, nil
	}))

	metadataBody := map[string]any{"id": "chosen", "project_id": project.ID, "name": "bad-metadata"}
	metadataCreate := request(t, server, http.MethodPost, "/api/v1/networks", metadataBody, map[string]string{"Idempotency-Key": "metadata"})
	if metadataCreate.Code != http.StatusBadRequest {
		t.Fatalf("metadata create status=%d body=%s", metadataCreate.Code, metadataCreate.Body.String())
	}

	basePort := map[string]any{
		"project_id": project.ID, "network_id": network.ID, "name": "vm100-net0",
		"mac_address": "02:00:00:00:00:10", "admin_state_up": true,
	}
	bindingFields := map[string]any{
		"node_id": "pve01", "vmid": 100, "nic": "net0", "requested_chassis": "chassis-01",
		"binding_status": "binding", "lsp_name": "pvn-client", "generation": 2,
	}
	for field, value := range bindingFields {
		body := make(map[string]any, len(basePort)+1)
		for name, original := range basePort {
			body[name] = original
		}
		body[field] = value
		response := request(t, server, http.MethodPost, "/api/v1/ports", body, map[string]string{"Idempotency-Key": "binding-" + field})
		if response.Code != http.StatusBadRequest {
			t.Errorf("create with %s status=%d body=%s", field, response.Code, response.Body.String())
		}
	}

	attachedResource, _, err := store.Create(context.Background(), &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "attached", MACAddress: "02:00:00:00:00:11",
		AdminStateUp: true, NodeID: "pve01", VMID: 100, NIC: "net0", RequestedChassis: "chassis-01",
		BindingStatus: model.PortBound, LSPName: "pvn-attached", Generation: 3,
	}, "attached")
	if err != nil {
		t.Fatal(err)
	}
	attached := attachedResource.(*model.Port)
	attached.NodeID = "pve02"
	immutableUpdate := request(t, server, http.MethodPut, "/api/v1/ports/"+attached.ID, attached, map[string]string{"Idempotency-Key": "change-node", "If-Match": `"1"`})
	if immutableUpdate.Code != http.StatusBadRequest {
		t.Fatalf("binding update status=%d body=%s", immutableUpdate.Code, immutableUpdate.Body.String())
	}
	attached.NodeID = "pve01"
	attached.State = model.ResourceError
	metadataUpdate := request(t, server, http.MethodPut, "/api/v1/ports/"+attached.ID, attached, map[string]string{"Idempotency-Key": "change-state", "If-Match": `"1"`})
	if metadataUpdate.Code != http.StatusBadRequest {
		t.Fatalf("metadata update status=%d body=%s", metadataUpdate.Code, metadataUpdate.Body.String())
	}
}

func TestAttachedPortRequiresUseAndVMNetworkPrivileges(t *testing.T) {
	store := controlstore.NewMemory()
	projectResource, _, err := store.Create(context.Background(), &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "project")
	if err != nil {
		t.Fatal(err)
	}
	project := projectResource.(*model.Project)
	networkResource, _, err := store.Create(context.Background(), &model.Network{ProjectID: project.ID, Name: "private"}, "network")
	if err != nil {
		t.Fatal(err)
	}
	network := networkResource.(*model.Network)
	ensureAPINode(t, store, "pve01", "chassis-01")
	portResource, _, err := store.Create(context.Background(), &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "attached", MACAddress: "02:00:00:00:00:12",
		AdminStateUp: true, NodeID: "pve01", VMID: 100, NIC: "net0", RequestedChassis: "chassis-01",
		BindingStatus: model.PortBound, LSPName: "pvn-attached", Generation: 2,
	}, "port")
	if err != nil {
		t.Fatal(err)
	}
	port := portResource.(*model.Port)
	port.AdminStateUp = false
	permissions := map[string]any{
		"/pool/pool-tenant": map[string]bool{"SDN.Audit": true, "SDN.Allocate": true},
	}
	server := testServer(t, store, SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "tenant@pve", Permissions: permissions}, nil
	}))

	deniedUse := request(t, server, http.MethodPut, "/api/v1/ports/"+port.ID, port, map[string]string{"Idempotency-Key": "no-use", "If-Match": `"1"`})
	if deniedUse.Code != http.StatusForbidden {
		t.Fatalf("attached update without SDN.Use status=%d body=%s", deniedUse.Code, deniedUse.Body.String())
	}
	permissions[networkPathPrefix+network.ID] = map[string]bool{"SDN.Use": true}
	deniedVM := request(t, server, http.MethodPut, "/api/v1/ports/"+port.ID, port, map[string]string{"Idempotency-Key": "no-vm", "If-Match": `"1"`})
	if deniedVM.Code != http.StatusForbidden {
		t.Fatalf("attached update without VM.Config.Network status=%d body=%s", deniedVM.Code, deniedVM.Body.String())
	}
	permissions["/vms/100"] = map[string]bool{"VM.Config.Network": true}
	allowed := request(t, server, http.MethodPut, "/api/v1/ports/"+port.ID, port, map[string]string{"Idempotency-Key": "allowed", "If-Match": `"1"`})
	if allowed.Code != http.StatusOK {
		t.Fatalf("attached update with scoped privileges status=%d body=%s", allowed.Code, allowed.Body.String())
	}
	updated := decodeData[model.Port](t, allowed)
	delete(permissions, "/vms/100")
	deniedDelete := request(t, server, http.MethodDelete, "/api/v1/ports/"+port.ID, nil, map[string]string{"Idempotency-Key": "delete-no-vm", "If-Match": fmt.Sprintf(`"%d"`, updated.Revision)})
	if deniedDelete.Code != http.StatusForbidden {
		t.Fatalf("attached delete without VM.Config.Network status=%d body=%s", deniedDelete.Code, deniedDelete.Body.String())
	}
}
