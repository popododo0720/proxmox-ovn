package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
	"github.com/popododo0720/proxmox-ovn/internal/reconcile"
)

func testServer(t *testing.T, store controlstore.Store, provider SessionProvider) *testAPIHandler {
	t.Helper()
	server, err := New(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return newTestAPIHandler(server, provider)
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
	if server, ok := handler.(*Server); ok {
		handler = newTestAPIHandler(server, nil)
	}
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

func createAPIResource(t *testing.T, store controlstore.Store, resource model.Resource, key string) model.Resource {
	t.Helper()
	if port, ok := resource.(*model.Port); ok && len(port.SecurityGroupIDs) == 0 {
		port.SecurityGroupIDs = []string{ensureAPITestSecurityGroup(t, store)}
	}
	created, replayed, err := store.Create(context.Background(), resource, key)
	if err != nil || replayed {
		t.Fatalf("Create(%s) replayed=%v err=%v", resource.ResourceKind(), replayed, err)
	}
	return created
}

func ensureAPITestSecurityGroup(t *testing.T, store controlstore.Store) string {
	t.Helper()
	const id = "00000000-0000-5000-8000-0000000000a1"
	if _, err := store.Get(context.Background(), model.KindSecurityGroup, id); errors.Is(err, controlstore.ErrNotFound) {
		if _, _, err := store.Create(context.Background(), &model.SecurityGroup{
			Metadata: model.Metadata{ID: id}, Name: "api-test-baseline",
		}, ""); err != nil {
			t.Fatalf("create API test security group: %v", err)
		}
	}
	return id
}

func floatingIPAPIInput(t *testing.T, store controlstore.Store) model.FloatingIP {
	t.Helper()
	provider := createAPIResource(t, store, &model.ProviderNetwork{Name: "public-fip"}, "api-fip-provider").(*model.ProviderNetwork)
	externalNetwork := createAPIResource(t, store, &model.Network{
		Name: "public-fip", External: true, ProviderNetworkID: provider.ID,
	}, "api-fip-external-network").(*model.Network)
	externalSubnet := createAPIResource(t, store, &model.Subnet{
		NetworkID: externalNetwork.ID, Name: "public-fip-v4", CIDR: "198.51.100.0/24",
		GatewayIP: "198.51.100.1", AllocationPools: []model.IPRange{{Start: "198.51.100.2", End: "198.51.100.200"}},
	}, "api-fip-external-subnet").(*model.Subnet)
	privateNetwork := createAPIResource(t, store, &model.Network{Name: "private-fip"}, "api-fip-private-network").(*model.Network)
	privateSubnet := createAPIResource(t, store, &model.Subnet{
		NetworkID: privateNetwork.ID, Name: "private-fip-v4", CIDR: "10.20.0.0/24", GatewayIP: "10.20.0.1",
	}, "api-fip-private-subnet").(*model.Subnet)
	port := createAPIResource(t, store, &model.Port{
		NetworkID: privateNetwork.ID, Name: "api-fip-port", MACAddress: "02:00:00:00:20:10",
		FixedIPs: []model.FixedIP{{SubnetID: privateSubnet.ID, Address: "10.20.0.10"}},
	}, "api-fip-port").(*model.Port)
	router := createAPIResource(t, store, &model.Router{
		Name: "api-fip-router", ExternalNetworkID: externalNetwork.ID,
		ExternalSubnetID: externalSubnet.ID, ExternalIPAddress: "198.51.100.2", EnableSNAT: true,
	}, "api-fip-router").(*model.Router)
	createAPIResource(t, store, &model.RouterInterface{
		RouterID: router.ID, SubnetID: privateSubnet.ID,
	}, "api-fip-router-interface")
	return model.FloatingIP{
		ProviderNetworkID: provider.ID, Address: "198.51.100.10",
		RouterID: router.ID, PortID: port.ID, FixedIPAddress: "10.20.0.10",
	}
}

func TestFloatingIPAPIUsesServerManagedRealizedStatus(t *testing.T) {
	store := controlstore.NewMemory()
	input := floatingIPAPIInput(t, store)
	controller := reconcile.NewController(store, reconcile.NewFakeRenderer())
	server, err := New(Options{Store: store, Reconciler: controller})
	if err != nil {
		t.Fatal(err)
	}

	spoofed := input
	spoofed.FloatingStatus = model.FloatingIPActive
	rejected := request(t, server, http.MethodPost, "/api/v1/floating-ips", spoofed, map[string]string{"Idempotency-Key": "api-fip-spoof-create"})
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "server-managed") {
		t.Fatalf("spoofed create status=%d body=%s", rejected.Code, rejected.Body.String())
	}

	createdResponse := request(t, server, http.MethodPost, "/api/v1/floating-ips", input, map[string]string{"Idempotency-Key": "api-fip-create"})
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeData[model.FloatingIP](t, createdResponse)
	if created.State != model.ResourceReady || created.FloatingStatus != model.FloatingIPActive || created.AppliedRevision != created.Revision {
		t.Fatalf("created floating IP=%#v", created)
	}

	created.PortID = ""
	created.FixedIPAddress = ""
	updatedResponse := request(t, server, http.MethodPut, "/api/v1/floating-ips/"+created.ID, created, map[string]string{
		"Idempotency-Key": "api-fip-reserve", "If-Match": `"1"`,
	})
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updatedResponse.Code, updatedResponse.Body.String())
	}
	updated := decodeData[model.FloatingIP](t, updatedResponse)
	if updated.State != model.ResourceReady || updated.FloatingStatus != model.FloatingIPDown || updated.AppliedRevision != updated.Revision {
		t.Fatalf("updated floating IP=%#v", updated)
	}

	updated.FloatingStatus = model.FloatingIPActive
	rejected = request(t, server, http.MethodPut, "/api/v1/floating-ips/"+updated.ID, updated, map[string]string{
		"Idempotency-Key": "api-fip-spoof-update", "If-Match": `"2"`,
	})
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "server-managed") {
		t.Fatalf("spoofed update status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}

func TestHealthRequiresGatewaySession(t *testing.T) {
	const deploymentName = "human-cluster"
	provider := SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "root@pam", Permissions: map[string]any{"/": map[string]bool{"SDN.Audit": true}}, Cluster: deploymentName}, nil
	})
	server, err := New(Options{Store: controlstore.NewMemory(), ClusterName: deploymentName})
	if err != nil {
		t.Fatal(err)
	}
	handler := newTestAPIHandler(server, provider)
	health := request(t, handler, http.MethodGet, "/api/v1/health", nil, nil)
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d", health.Code)
	}
	if cluster := decodeData[map[string]any](t, health)["cluster"]; cluster != deploymentName {
		t.Fatalf("health deployment name=%v", cluster)
	}
	session := request(t, handler, http.MethodGet, "/api/v1/session", nil, nil)
	if session.Code != http.StatusNotFound {
		t.Fatalf("removed session endpoint status=%d body=%s", session.Code, session.Body.String())
	}
	unauthenticated := testServer(t, controlstore.NewMemory(), SessionProviderFunc(func(context.Context, *http.Request) (Session, error) { return Session{}, ErrUnauthenticated }))
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
	createdResponse := request(t, server, http.MethodPost, "/api/v1/provider-networks", map[string]any{"name": "provider"}, map[string]string{"Idempotency-Key": "provider-create"})
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	provider := decodeData[model.ProviderNetwork](t, createdResponse)

	replay := request(t, server, http.MethodPost, "/api/v1/provider-networks", map[string]any{"name": "provider"}, map[string]string{"Idempotency-Key": "provider-create"})
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}

	provider.Description = "changed"
	updatedResponse := request(t, server, http.MethodPut, "/api/v1/provider-networks/"+provider.ID, provider, map[string]string{"Idempotency-Key": "provider-update", "If-Match": `"1"`})
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", updatedResponse.Code, updatedResponse.Body.String())
	}
	updated := decodeData[model.ProviderNetwork](t, updatedResponse)
	if updated.Revision != 2 || updated.Description != "changed" || updatedResponse.Header().Get("ETag") != `"2"` {
		t.Fatalf("updated=%#v etag=%s", updated, updatedResponse.Header().Get("ETag"))
	}
	updateReplay := request(t, server, http.MethodPut, "/api/v1/provider-networks/"+provider.ID, provider, map[string]string{"Idempotency-Key": "provider-update", "If-Match": `"1"`})
	if updateReplay.Code != http.StatusOK || updateReplay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("update replay status=%d headers=%v body=%s", updateReplay.Code, updateReplay.Header(), updateReplay.Body.String())
	}

	stale := request(t, server, http.MethodPut, "/api/v1/provider-networks/"+provider.ID, provider, map[string]string{"Idempotency-Key": "provider-update-stale", "If-Match": `"1"`})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
	missingPrecondition := request(t, server, http.MethodDelete, "/api/v1/provider-networks/"+provider.ID, nil, map[string]string{"Idempotency-Key": "provider-delete"})
	if missingPrecondition.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing precondition status=%d", missingPrecondition.Code)
	}
	deleted := request(t, server, http.MethodDelete, "/api/v1/provider-networks/"+provider.ID, nil, map[string]string{"Idempotency-Key": "provider-delete", "If-Match": `"2"`})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	deleteReplay := request(t, server, http.MethodDelete, "/api/v1/provider-networks/"+provider.ID, nil, map[string]string{"Idempotency-Key": "provider-delete", "If-Match": `"2"`})
	if deleteReplay.Code != http.StatusNoContent || deleteReplay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("delete replay status=%d headers=%v", deleteReplay.Code, deleteReplay.Header())
	}
}

func TestCRUDValidationReferencesAndListFilter(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	missingKey := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"name": "private"}, nil)
	if missingKey.Code != http.StatusBadRequest {
		t.Fatalf("missing key status=%d", missingKey.Code)
	}
	unknownField := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"name": "private", "surprise": true}, map[string]string{"Idempotency-Key": "bad-json"})
	if unknownField.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", unknownField.Code, unknownField.Body.String())
	}
	invalid := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"name": "bad name"}, map[string]string{"Idempotency-Key": "invalid"})
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validation status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	networkResponse := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"name": "private"}, map[string]string{"Idempotency-Key": "network"})
	if networkResponse.Code != http.StatusCreated {
		t.Fatalf("network status=%d body=%s", networkResponse.Code, networkResponse.Body.String())
	}
	missingProvider := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{"name": "orphan", "external": true, "provider_network_id": "missing"}, map[string]string{"Idempotency-Key": "orphan"})
	if missingProvider.Code != http.StatusConflict {
		t.Fatalf("missing ref status=%d body=%s", missingProvider.Code, missingProvider.Body.String())
	}
	legacyFilter := request(t, server, http.MethodGet, "/api/v1/networks?project_id=legacy", nil, nil)
	if legacyFilter.Code != http.StatusBadRequest {
		t.Fatalf("legacy project_id filter status=%d body=%s", legacyFilter.Code, legacyFilter.Body.String())
	}
	list := request(t, server, http.MethodGet, "/api/v1/networks", nil, nil)
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
	networks, _ := store.List(context.Background(), model.KindNetwork, controlstore.ListOptions{})
	var network *model.Network
	if len(networks) == 0 {
		created, _, err := store.Create(context.Background(), &model.Network{Name: "private"}, "runtime-network")
		if err != nil {
			t.Fatal(err)
		}
		network = created.(*model.Network)
	} else {
		network = networks[0].(*model.Network)
	}
	created, _, err := store.Create(context.Background(), &model.Port{
		NetworkID: network.ID, Name: fmt.Sprintf("vm-%d-%s-%s", vmid, nic, nodeID), MACAddress: mac,
		AdminStateUp: adminUp, BindingStatus: status, NodeID: nodeID, VMID: vmid, NIC: nic,
		LSPName: "lsp-" + nodeID, Generation: 7, RequestedChassis: requested,
		SecurityGroupIDs: []string{ensureAPITestSecurityGroup(t, store)},
	}, "port-"+nodeID+nic)
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

type runtimeLookupObservingStore struct {
	controlstore.Store
	ports       []*model.Port
	lookupCalls int
	listCalls   int
}

func (s *runtimeLookupObservingStore) LookupRuntimePorts(context.Context, string, int, string) ([]*model.Port, error) {
	s.lookupCalls++
	return s.ports, nil
}

func (s *runtimeLookupObservingStore) List(context.Context, model.Kind, controlstore.ListOptions) ([]model.Resource, error) {
	s.listCalls++
	return nil, errors.New("generic list path must not be used")
}

type runtimeListOnlyStore struct {
	controlstore.Store
	listed []model.Kind
}

func (s *runtimeListOnlyStore) List(ctx context.Context, kind model.Kind, options controlstore.ListOptions) ([]model.Resource, error) {
	s.listed = append(s.listed, kind)
	return s.Store.List(ctx, kind, options)
}

func TestRuntimePortResolverUsesOptionalLookupAndKeepsListFallback(t *testing.T) {
	fastBacking := controlstore.NewMemory()
	fast := &runtimeLookupObservingStore{
		Store: fastBacking,
		ports: []*model.Port{{
			Metadata:   model.Metadata{ID: "port-fast", Revision: 2, AppliedRevision: 2, State: model.ResourceReady},
			MACAddress: "02:00:00:00:00:01", AdminStateUp: true,
			BindingStatus: model.PortBinding, VMID: 100, NIC: "net0", LSPName: "lsp-fast",
			Generation: 3, RequestedChassis: "chassis-fast",
		}},
	}
	allowedProvider := SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "auditor@pam", Permissions: map[string]any{
			"/": map[string]bool{"SDN.Audit": true},
		}}, nil
	})
	fastResponse := request(t, testServer(t, fast, allowedProvider), http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve-fast&vmid=100&nic=net0", nil, nil)
	if fastResponse.Code != http.StatusOK || fast.lookupCalls != 1 || fast.listCalls != 0 {
		t.Fatalf("fast resolver status=%d lookups=%d lists=%d body=%s", fastResponse.Code, fast.lookupCalls, fast.listCalls, fastResponse.Body.String())
	}
	deniedProvider := SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "denied@pam", Permissions: map[string]any{}}, nil
	})
	deniedResponse := request(t, testServer(t, fast, deniedProvider), http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve-fast&vmid=100&nic=net0", nil, nil)
	if deniedResponse.Code != http.StatusNotFound || fast.lookupCalls != 2 || fast.listCalls != 0 {
		t.Fatalf("hidden fast resolver status=%d lookups=%d lists=%d body=%s", deniedResponse.Code, fast.lookupCalls, fast.listCalls, deniedResponse.Body.String())
	}
	second := *fast.ports[0]
	second.ID = "port-fast-2"
	fast.ports = append(fast.ports, &second)
	ambiguousResponse := request(t, testServer(t, fast, nil).RuntimeHandler(), http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve-fast&vmid=100&nic=net0", nil, nil)
	if ambiguousResponse.Code != http.StatusConflict || fast.lookupCalls != 3 || fast.listCalls != 0 {
		t.Fatalf("ambiguous fast resolver status=%d lookups=%d lists=%d body=%s", ambiguousResponse.Code, fast.lookupCalls, fast.listCalls, ambiguousResponse.Body.String())
	}

	memory := controlstore.NewMemory()
	node := ensureAPINode(t, memory, "node-fallback", "chassis-fallback")
	setupPort(t, memory, node.ID, 101, "net1", "02:00:00:00:00:02", node.ChassisID, model.PortBinding, true)
	fallback := &runtimeListOnlyStore{Store: memory}
	fallbackResponse := request(t, testServer(t, fallback, nil).RuntimeHandler(), http.MethodGet, "/api/v1/runtime/ports/resolve?node=node-fallback&vmid=101&nic=net1", nil, nil)
	if fallbackResponse.Code != http.StatusOK {
		t.Fatalf("fallback resolver status=%d body=%s", fallbackResponse.Code, fallbackResponse.Body.String())
	}
	wantKinds := []model.Kind{model.KindNode, model.KindPort}
	if fmt.Sprint(fallback.listed) != fmt.Sprint(wantKinds) {
		t.Fatalf("fallback listed kinds=%v want %v", fallback.listed, wantKinds)
	}
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
	networkResource, _, err := store.Create(context.Background(), &model.Network{Name: "private"}, "network")
	if err != nil {
		t.Fatal(err)
	}
	permissions := map[string]any{}
	provider := SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "user@pam", Permissions: permissions}, nil
	})
	server := testServer(t, store, provider)
	filteredRead := request(t, server, http.MethodGet, "/api/v1/networks", nil, nil)
	if filteredRead.Code != http.StatusOK || len(decodeData[[]model.Network](t, filteredRead)) != 0 {
		t.Fatalf("read without audit status=%d body=%s", filteredRead.Code, filteredRead.Body.String())
	}
	permissions["/"] = map[string]bool{"SDN.Audit": true}
	allowedRead := request(t, server, http.MethodGet, "/api/v1/networks", nil, nil)
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

	permissions["/"] = map[string]bool{"SDN.Audit": true, "SDN.Allocate": true}
	portBody := map[string]any{"network_id": networkResource.GetMetadata().ID, "name": "vm100-net0", "mac_address": "02:00:00:00:00:10", "admin_state_up": true}
	allowedPort := request(t, server, http.MethodPost, "/api/v1/ports", portBody, map[string]string{"Idempotency-Key": "port"})
	if allowedPort.Code != http.StatusCreated {
		t.Fatalf("unattached port with global allocation status=%d body=%s", allowedPort.Code, allowedPort.Body.String())
	}

	deniedSystem := request(t, server, http.MethodPost, "/api/v1/nodes", map[string]any{"name": "pve01", "chassis_id": "chassis-01", "enabled": true}, map[string]string{"Idempotency-Key": "node"})
	if deniedSystem.Code != http.StatusForbidden {
		t.Fatalf("node without global allocation status=%d body=%s", deniedSystem.Code, deniedSystem.Body.String())
	}
}

func TestServerManagedMetadataAndPortBindingFields(t *testing.T) {
	store := controlstore.NewMemory()
	networkResource, _, err := store.Create(context.Background(), &model.Network{Name: "private"}, "network")
	if err != nil {
		t.Fatal(err)
	}
	network := networkResource.(*model.Network)
	ensureAPINode(t, store, "pve01", "chassis-01")
	permissions := map[string]any{
		"/":        map[string]bool{"SDN.Audit": true, "SDN.Allocate": true, "SDN.Use": true},
		"/vms/100": map[string]bool{"VM.Config.Network": true},
	}
	server := testServer(t, store, SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "tenant@pve", Permissions: permissions}, nil
	}))

	metadataBody := map[string]any{"id": "chosen", "name": "bad-metadata"}
	metadataCreate := request(t, server, http.MethodPost, "/api/v1/networks", metadataBody, map[string]string{"Idempotency-Key": "metadata"})
	if metadataCreate.Code != http.StatusBadRequest {
		t.Fatalf("metadata create status=%d body=%s", metadataCreate.Code, metadataCreate.Body.String())
	}

	basePort := map[string]any{
		"network_id": network.ID, "name": "vm100-net0",
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
		NetworkID: network.ID, Name: "attached", MACAddress: "02:00:00:00:00:11",
		AdminStateUp: true, NodeID: "pve01", VMID: 100, NIC: "net0", RequestedChassis: "chassis-01",
		BindingStatus: model.PortBound, LSPName: "pvn-attached", Generation: 3,
		SecurityGroupIDs: []string{ensureAPITestSecurityGroup(t, store)},
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
	networkResource, _, err := store.Create(context.Background(), &model.Network{Name: "private"}, "network")
	if err != nil {
		t.Fatal(err)
	}
	network := networkResource.(*model.Network)
	ensureAPINode(t, store, "pve01", "chassis-01")
	groupResource, _, err := store.Create(context.Background(), &model.SecurityGroup{Name: "attached-policy"}, "attached-policy")
	if err != nil {
		t.Fatal(err)
	}
	portResource, _, err := store.Create(context.Background(), &model.Port{
		NetworkID: network.ID, Name: "attached", MACAddress: "02:00:00:00:00:12",
		AdminStateUp: true, NodeID: "pve01", VMID: 100, NIC: "net0", RequestedChassis: "chassis-01",
		BindingStatus: model.PortBound, LSPName: "pvn-attached", Generation: 2, SecurityGroupIDs: []string{groupResource.GetMetadata().ID},
	}, "port")
	if err != nil {
		t.Fatal(err)
	}
	port := portResource.(*model.Port)
	port.AdminStateUp = false
	permissions := map[string]any{
		"/": map[string]bool{"SDN.Audit": true, "SDN.Allocate": true},
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
