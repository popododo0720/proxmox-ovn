package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type provisionSessionProvider struct {
	mu            sync.RWMutex
	authenticated bool
	csrf          string
	permissions   map[string]any
}

func (provider *provisionSessionProvider) Session(context.Context, *http.Request) (Session, error) {
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	if !provider.authenticated {
		return Session{}, ErrUnauthenticated
	}
	return Session{User: "tenant@pve", Permissions: provider.permissions}, nil
}

func (provider *provisionSessionProvider) Authorize(_ context.Context, request *http.Request, unsafe bool) (Session, error) {
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	if !provider.authenticated {
		return Session{}, ErrUnauthenticated
	}
	if unsafe && request.Header.Get(PVNCSRFHeader) != provider.csrf {
		return Session{}, ErrInvalidCSRF
	}
	return Session{User: "tenant@pve", Permissions: provider.permissions}, nil
}

func (provider *provisionSessionProvider) setAuthenticated(value bool) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.authenticated = value
}

func (provider *provisionSessionProvider) setPermissions(value map[string]any) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.permissions = value
}

type provisionTopology struct {
	network *model.Network
	subnet  *model.Subnet
}

func seedProvisionTopology(t *testing.T, store controlstore.Store, cidr string, pools []model.IPRange) provisionTopology {
	t.Helper()
	networkResource, _, err := store.Create(context.Background(), &model.Network{Name: "private"}, "seed-network")
	if err != nil {
		t.Fatal(err)
	}
	network := networkResource.(*model.Network)
	subnetResource, _, err := store.Create(context.Background(), &model.Subnet{
		NetworkID: network.ID, Name: "private-v4", CIDR: cidr,
		GatewayIP: "10.0.0.1", EnableDHCP: true, AllocationPools: pools,
	}, "seed-subnet")
	if err != nil {
		t.Fatal(err)
	}
	return provisionTopology{network: network, subnet: subnetResource.(*model.Subnet)}
}

func provisionRequestBody(topology provisionTopology, name string) map[string]any {
	return map[string]any{
		"network_id": topology.network.ID,
		"subnet_id":  topology.subnet.ID,
		"name":       name,
	}
}

func provisionHeaders(key, csrf string) map[string]string {
	return map[string]string{"Idempotency-Key": key, PVNCSRFHeader: csrf}
}

type recordingProvisionReconciler struct {
	mu    sync.Mutex
	store controlstore.Store
	calls []string
}

func (reconciler *recordingProvisionReconciler) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	if reconciler.store != nil {
		resource, err := reconciler.store.Get(ctx, kind, id)
		if err != nil {
			return err
		}
		if _, err := reconciler.store.MarkReconciled(ctx, kind, id, resource.GetMetadata().Revision, nil); err != nil {
			return err
		}
	}
	if kind != model.KindPort {
		return nil
	}
	reconciler.mu.Lock()
	defer reconciler.mu.Unlock()
	reconciler.calls = append(reconciler.calls, kind.String()+"/"+id)
	return nil
}

func (reconciler *recordingProvisionReconciler) count() int {
	reconciler.mu.Lock()
	defer reconciler.mu.Unlock()
	return len(reconciler.calls)
}

func TestPortProvisionRequiresSessionCSRFAndAllocatePrivilege(t *testing.T) {
	store := controlstore.NewMemory()
	topology := seedProvisionTopology(t, store, "10.0.0.0/29", []model.IPRange{{Start: "10.0.0.2", End: "10.0.0.5"}})
	provider := &provisionSessionProvider{csrf: "csrf-good"}
	server := testServer(t, store, provider)
	body := provisionRequestBody(topology, "vm100-net0")
	headers := provisionHeaders("provision-vm100", "csrf-good")

	unauthenticated := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	provider.setAuthenticated(true)
	withoutCSRF := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, map[string]string{"Idempotency-Key": "provision-vm100"})
	if withoutCSRF.Code != http.StatusForbidden || provisionErrorCode(t, withoutCSRF) != "invalid_csrf" {
		t.Fatalf("without CSRF status=%d body=%s", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	denied := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("without SDN.Allocate status=%d body=%s", denied.Code, denied.Body.String())
	}
	provider.setPermissions(map[string]any{"/": map[string]bool{"SDN.Allocate": true}})
	allowed := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if allowed.Code != http.StatusCreated {
		t.Fatalf("allowed status=%d body=%s", allowed.Code, allowed.Body.String())
	}
}

func TestPortProvisionAllocatesAndDurablyReplays(t *testing.T) {
	store := controlstore.NewMemory()
	topology := seedProvisionTopology(t, store, "10.0.0.0/29", []model.IPRange{{Start: "10.0.0.1", End: "10.0.0.4"}})
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	reconciler := &recordingProvisionReconciler{store: store}
	server, err := New(Options{Store: store, SessionProvider: provider, Reconciler: reconciler})
	if err != nil {
		t.Fatal(err)
	}
	body := provisionRequestBody(topology, "vm100-net0")
	headers := provisionHeaders("provision-vm100", "csrf")

	createdResponse := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeData[model.Port](t, createdResponse)
	mac, err := net.ParseMAC(created.MACAddress)
	if err != nil || mac[0]&0x03 != 0x02 {
		t.Fatalf("generated MAC %q is not locally-administered unicast", created.MACAddress)
	}
	if created.BindingStatus != model.PortUnbound || !created.AdminStateUp || created.NodeID != "" || created.VMID != 0 || created.NIC != "" {
		t.Fatalf("created port is not admin-up and unbound: %#v", created)
	}
	if len(created.FixedIPs) != 1 || created.FixedIPs[0].Address != "10.0.0.2" {
		t.Fatalf("fixed IPs = %#v", created.FixedIPs)
	}
	if fmt.Sprint(created.SecurityGroupIDs) != fmt.Sprint([]string{defaultsecurity.DefaultSecurityGroupID()}) {
		t.Fatalf("security groups=%v", created.SecurityGroupIDs)
	}
	if createdResponse.Header().Get("Location") != "/api/v1/ports/"+created.ID || createdResponse.Header().Get("ETag") != `"1"` {
		t.Fatalf("headers=%v", createdResponse.Header())
	}
	allocations, err := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil || len(allocations) != 1 {
		t.Fatalf("allocations=%#v err=%v", allocations, err)
	}
	allocation := allocations[0].(*model.IPAllocation)
	if allocation.State != model.IPAllocated || allocation.PortID != created.ID || allocation.Address != "10.0.0.2" {
		t.Fatalf("allocation=%#v", allocation)
	}
	if reconciler.count() != 1 {
		t.Fatalf("reconcile calls=%d", reconciler.count())
	}

	replayedResponse := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if replayedResponse.Code != http.StatusOK || replayedResponse.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replayedResponse.Code, replayedResponse.Header(), replayedResponse.Body.String())
	}
	replayed := decodeData[model.Port](t, replayedResponse)
	if replayed.ID != created.ID || replayed.MACAddress != created.MACAddress {
		t.Fatalf("replayed=%#v created=%#v", replayed, created)
	}
	ports, _ := store.List(context.Background(), model.KindPort, controlstore.ListOptions{})
	allocations, _ = store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if len(ports) != 1 || len(allocations) != 1 || reconciler.count() != 1 {
		t.Fatalf("ports=%d allocations=%d reconcile=%d", len(ports), len(allocations), reconciler.count())
	}

	changed := provisionRequestBody(topology, "vm100-net1")
	conflict := request(t, server, http.MethodPost, "/api/v1/ports/provision", changed, headers)
	if conflict.Code != http.StatusConflict || provisionErrorCode(t, conflict) != "idempotency_conflict" {
		t.Fatalf("key mismatch status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestPortProvisionSupportsNoAddressAndManualAddress(t *testing.T) {
	store := controlstore.NewMemory()
	topology := seedProvisionTopology(t, store, "10.0.0.0/29", []model.IPRange{{Start: "10.0.0.2", End: "10.0.0.5"}})
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	server := testServer(t, store, provider)

	withoutAddress := map[string]any{"network_id": topology.network.ID}
	withoutAddressResponse := request(t, server, http.MethodPost, "/api/v1/ports/provision", withoutAddress, provisionHeaders("without-address", "csrf"))
	if withoutAddressResponse.Code != http.StatusCreated {
		t.Fatalf("without address status=%d body=%s", withoutAddressResponse.Code, withoutAddressResponse.Body.String())
	}
	portWithoutAddress := decodeData[model.Port](t, withoutAddressResponse)
	if portWithoutAddress.Name == "" || len(portWithoutAddress.FixedIPs) != 0 {
		t.Fatalf("port without address=%#v", portWithoutAddress)
	}
	allocations, _ := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if len(allocations) != 0 {
		t.Fatalf("unexpected allocations=%#v", allocations)
	}

	manual := provisionRequestBody(topology, "manual")
	manual["fixed_ip_address"] = "10.0.0.4"
	manualResponse := request(t, server, http.MethodPost, "/api/v1/ports/provision", manual, provisionHeaders("manual-address", "csrf"))
	if manualResponse.Code != http.StatusCreated {
		t.Fatalf("manual status=%d body=%s", manualResponse.Code, manualResponse.Body.String())
	}
	manualPort := decodeData[model.Port](t, manualResponse)
	if len(manualPort.FixedIPs) != 1 || manualPort.FixedIPs[0].Address != "10.0.0.4" {
		t.Fatalf("manual fixed IPs=%#v", manualPort.FixedIPs)
	}

	outsidePool := provisionRequestBody(topology, "outside-pool")
	outsidePool["fixed_ip_address"] = "10.0.0.6"
	outsideResponse := request(t, server, http.MethodPost, "/api/v1/ports/provision", outsidePool, provisionHeaders("outside-pool", "csrf"))
	if outsideResponse.Code != http.StatusUnprocessableEntity || provisionErrorCode(t, outsideResponse) != "validation_error" {
		t.Fatalf("outside pool status=%d body=%s", outsideResponse.Code, outsideResponse.Body.String())
	}
}

func TestPortProvisionRejectsProviderBackedNetwork(t *testing.T) {
	store := controlstore.NewMemory()
	providerNetworkResource, _, err := store.Create(context.Background(), &model.ProviderNetwork{Name: "public"}, "provider-network")
	if err != nil {
		t.Fatal(err)
	}
	providerNetwork := providerNetworkResource.(*model.ProviderNetwork)
	networkResource, _, err := store.Create(context.Background(), &model.Network{
		Name: "public", External: true, ProviderNetworkID: providerNetwork.ID,
	}, "external-network")
	if err != nil {
		t.Fatal(err)
	}
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	server := testServer(t, store, provider)
	body := map[string]any{"network_id": networkResource.(*model.Network).ID, "name": "external-port"}
	response := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, provisionHeaders("external-port", "csrf"))
	if response.Code != http.StatusConflict || provisionErrorCode(t, response) != "provider_network_port" {
		t.Fatalf("provider-backed status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPortProvisionConcurrentAllocationHasNoDuplicates(t *testing.T) {
	store := controlstore.NewMemory()
	topology := seedProvisionTopology(t, store, "10.0.0.0/27", []model.IPRange{{Start: "10.0.0.2", End: "10.0.0.17"}})
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	// Separate handlers sharing one serialized control store model independent
	// active pvn-manager processes racing on the same clustered OVSDB.
	servers := []*Server{testServer(t, store, provider), testServer(t, store, provider), testServer(t, store, provider)}
	const count = 12
	encodedBodies := make([][]byte, count)
	for index := 0; index < count; index++ {
		var err error
		encodedBodies[index], err = json.Marshal(provisionRequestBody(topology, fmt.Sprintf("vm%d-net0", 100+index)))
		if err != nil {
			t.Fatal(err)
		}
	}
	responses := make(chan *httptest.ResponseRecorder, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			encodedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/ports/provision", bytes.NewReader(encodedBodies[index]))
			encodedRequest.Header.Set("Idempotency-Key", fmt.Sprintf("concurrent-%d", index))
			encodedRequest.Header.Set(PVNCSRFHeader, "csrf")
			response := httptest.NewRecorder()
			servers[index%len(servers)].ServeHTTP(response, encodedRequest)
			responses <- response
		}()
	}
	wait.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("concurrent status=%d body=%s", response.Code, response.Body.String())
		}
	}

	allocations, err := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil || len(allocations) != count {
		t.Fatalf("allocation count=%d err=%v", len(allocations), err)
	}
	seen := make(map[string]bool, count)
	for _, resource := range allocations {
		allocation := resource.(*model.IPAllocation)
		if seen[allocation.Address] || allocation.State != model.IPAllocated || allocation.PortID == "" {
			t.Fatalf("duplicate or incomplete allocation=%#v", allocation)
		}
		seen[allocation.Address] = true
	}
}

func TestPortProvisionReportsSubnetExhaustion(t *testing.T) {
	store := controlstore.NewMemory()
	topology := seedProvisionTopology(t, store, "10.0.0.0/29", []model.IPRange{{Start: "10.0.0.2", End: "10.0.0.2"}})
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	server := testServer(t, store, provider)
	first := request(t, server, http.MethodPost, "/api/v1/ports/provision", provisionRequestBody(topology, "first"), provisionHeaders("first", "csrf"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	second := request(t, server, http.MethodPost, "/api/v1/ports/provision", provisionRequestBody(topology, "second"), provisionHeaders("second", "csrf"))
	if second.Code != http.StatusConflict || provisionErrorCode(t, second) != "conflict" {
		t.Fatalf("exhausted status=%d body=%s", second.Code, second.Body.String())
	}
	ports, _ := store.List(context.Background(), model.KindPort, controlstore.ListOptions{})
	allocations, _ := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if len(ports) != 1 || len(allocations) != 1 {
		t.Fatalf("ports=%d allocations=%d", len(ports), len(allocations))
	}
}

func TestPortProvisionReservesImplicitGateway(t *testing.T) {
	store := controlstore.NewMemory()
	topology := seedProvisionTopology(t, store, "10.0.0.0/29", nil)
	topology.subnet.GatewayIP = ""
	updated, _, err := store.Update(context.Background(), topology.subnet, topology.subnet.Revision, "use-implicit-gateway")
	if err != nil {
		t.Fatal(err)
	}
	topology.subnet = updated.(*model.Subnet)
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	server := testServer(t, store, provider)
	response := request(t, server, http.MethodPost, "/api/v1/ports/provision", provisionRequestBody(topology, "implicit-gateway"), provisionHeaders("implicit-gateway", "csrf"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	port := decodeData[model.Port](t, response)
	if len(port.FixedIPs) != 1 || port.FixedIPs[0].Address != "10.0.0.2" {
		t.Fatalf("fixed IPs=%v, implicit gateway .1 must be reserved", port.FixedIPs)
	}
}

func TestPortProvisionRollsBackReservationAndCanRetry(t *testing.T) {
	store := controlstore.NewMemory()
	topology := seedProvisionTopology(t, store, "10.0.0.0/29", []model.IPRange{{Start: "10.0.0.2", End: "10.0.0.4"}})
	defaultGroup, err := defaultsecurity.New(store, nil).Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conflictingResource, _, err := store.Create(context.Background(), &model.Port{
		NetworkID: topology.network.ID, Name: "existing",
		MACAddress: "02:00:00:00:00:aa", AdminStateUp: true, SecurityGroupIDs: []string{defaultGroup.ID},
	}, "existing-port")
	if err != nil {
		t.Fatal(err)
	}
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	server := testServer(t, store, provider)
	body := provisionRequestBody(topology, "retryable")
	body["mac_address"] = "02:00:00:00:00:aa"
	headers := provisionHeaders("rollback-retry", "csrf")

	failed := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if failed.Code != http.StatusConflict || provisionErrorCode(t, failed) != "already_exists" {
		t.Fatalf("conflicting status=%d body=%s", failed.Code, failed.Body.String())
	}
	allocations, _ := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if len(allocations) != 0 {
		t.Fatalf("reservation was not rolled back: %#v", allocations)
	}
	conflicting := conflictingResource.(*model.Port)
	if _, err := store.Delete(context.Background(), model.KindPort, conflicting.ID, conflicting.Revision, ""); err != nil {
		t.Fatal(err)
	}
	retried := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if retried.Code != http.StatusOK || retried.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retry status=%d headers=%v body=%s", retried.Code, retried.Header(), retried.Body.String())
	}
	allocations, _ = store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if len(allocations) != 1 || allocations[0].(*model.IPAllocation).State != model.IPAllocated {
		t.Fatalf("retried allocations=%#v", allocations)
	}
}

type failAllocationUpdateOnceStore struct {
	controlstore.Store
	mu     sync.Mutex
	failed bool
}

func (store *failAllocationUpdateOnceStore) Update(ctx context.Context, resource model.Resource, revision int64, key string) (model.Resource, bool, error) {
	store.mu.Lock()
	if resource.ResourceKind() == model.KindIPAllocation && !store.failed {
		store.failed = true
		store.mu.Unlock()
		return nil, false, errors.New("injected allocation finalization failure")
	}
	store.mu.Unlock()
	return store.Store.Update(ctx, resource, revision, key)
}

func TestPortProvisionRecoversPortCreatedAllocationReserved(t *testing.T) {
	base := controlstore.NewMemory()
	store := &failAllocationUpdateOnceStore{Store: base}
	topology := seedProvisionTopology(t, store, "10.0.0.0/29", []model.IPRange{{Start: "10.0.0.2", End: "10.0.0.4"}})
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	server := testServer(t, store, provider)
	body := provisionRequestBody(topology, "partial")
	headers := provisionHeaders("partial-retry", "csrf")

	partial := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if partial.Code != http.StatusInternalServerError {
		t.Fatalf("partial status=%d body=%s", partial.Code, partial.Body.String())
	}
	ports, _ := store.List(context.Background(), model.KindPort, controlstore.ListOptions{})
	allocations, _ := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if len(ports) != 1 || len(allocations) != 1 || allocations[0].(*model.IPAllocation).State != model.IPReserved {
		t.Fatalf("partial ports=%#v allocations=%#v", ports, allocations)
	}

	retried := request(t, server, http.MethodPost, "/api/v1/ports/provision", body, headers)
	if retried.Code != http.StatusOK || retried.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retry status=%d headers=%v body=%s", retried.Code, retried.Header(), retried.Body.String())
	}
	allocation := mustOnlyAllocation(t, store)
	port := decodeData[model.Port](t, retried)
	if allocation.State != model.IPAllocated || allocation.PortID != port.ID {
		t.Fatalf("final allocation=%#v port=%#v", allocation, port)
	}
}

type deprovisionTestReconciler struct {
	mu        sync.Mutex
	store     controlstore.Store
	failKind  model.Kind
	failCount int
	deletes   []model.Kind
}

func (reconciler *deprovisionTestReconciler) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	if reconciler.store == nil {
		return nil
	}
	resource, err := reconciler.store.Get(ctx, kind, id)
	if err != nil {
		return err
	}
	_, err = reconciler.store.MarkReconciled(ctx, kind, id, resource.GetMetadata().Revision, nil)
	return err
}

func (reconciler *deprovisionTestReconciler) Delete(_ context.Context, resource model.Resource) error {
	reconciler.mu.Lock()
	defer reconciler.mu.Unlock()
	reconciler.deletes = append(reconciler.deletes, resource.ResourceKind())
	if resource.ResourceKind() == reconciler.failKind && reconciler.failCount > 0 {
		reconciler.failCount--
		failure := errors.New("injected deletion reconciliation failure")
		if reconciler.store != nil {
			_, _ = reconciler.store.MarkReconciled(context.Background(), resource.ResourceKind(), resource.GetMetadata().ID, resource.GetMetadata().Revision, failure)
		}
		return failure
	}
	return nil
}

func (reconciler *deprovisionTestReconciler) deletedKinds() []model.Kind {
	reconciler.mu.Lock()
	defer reconciler.mu.Unlock()
	return append([]model.Kind(nil), reconciler.deletes...)
}

func provisionForDelete(t *testing.T, store controlstore.Store, provider SessionProvider, reconciler Reconciler) (*Server, model.Port) {
	t.Helper()
	topology := seedProvisionTopology(t, store, "10.0.0.0/29", []model.IPRange{{Start: "10.0.0.2", End: "10.0.0.5"}})
	server, err := New(Options{Store: store, SessionProvider: provider, Reconciler: reconciler})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodPost, "/api/v1/ports/provision", provisionRequestBody(topology, "delete-me"), provisionHeaders("create-delete-me", "csrf"))
	if response.Code != http.StatusCreated {
		t.Fatalf("provision status=%d body=%s", response.Code, response.Body.String())
	}
	return server, decodeData[model.Port](t, response)
}

func deprovisionHeaders(key string, revision int64) map[string]string {
	return map[string]string{
		"Idempotency-Key": key,
		"If-Match":        fmt.Sprintf(`"%d"`, revision),
		PVNCSRFHeader:     "csrf",
	}
}

func TestPortDeprovisionReleasesAllocationAndDurablyReplays(t *testing.T) {
	store := controlstore.NewMemory()
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
	}
	reconciler := &deprovisionTestReconciler{store: store}
	server, port := provisionForDelete(t, store, provider, reconciler)

	generic := request(t, server, http.MethodDelete, "/api/v1/ports/"+port.ID, nil, deprovisionHeaders("generic-delete", port.Revision))
	if generic.Code != http.StatusConflict {
		t.Fatalf("generic delete status=%d body=%s", generic.Code, generic.Body.String())
	}
	stale := request(t, server, http.MethodDelete, "/api/v1/ports/"+port.ID+"/deprovision", nil, deprovisionHeaders("stale-deprovision", port.Revision+1))
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
	if allocation := mustOnlyAllocation(t, store); allocation.PortID != port.ID {
		t.Fatalf("stale request changed allocation=%#v", allocation)
	}

	headers := deprovisionHeaders("deprovision-port", port.Revision)
	deleted := request(t, server, http.MethodDelete, "/api/v1/ports/"+port.ID+"/deprovision", nil, headers)
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 {
		t.Fatalf("deprovision status=%d headers=%v body=%s", deleted.Code, deleted.Header(), deleted.Body.String())
	}
	if _, err := store.Get(context.Background(), model.KindPort, port.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("port still exists: %v", err)
	}
	allocations, err := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil || len(allocations) != 0 {
		t.Fatalf("allocations=%#v err=%v", allocations, err)
	}
	deletedKinds := reconciler.deletedKinds()
	if len(deletedKinds) != 2 || deletedKinds[0] != model.KindIPAllocation || deletedKinds[1] != model.KindPort {
		t.Fatalf("deletion reconcile order=%v", deletedKinds)
	}

	replayed := request(t, server, http.MethodDelete, "/api/v1/ports/"+port.ID+"/deprovision", nil, headers)
	if replayed.Code != http.StatusNoContent || replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replayed.Code, replayed.Header(), replayed.Body.String())
	}
}

func TestPortDeprovisionRejectsAttachedPortWithoutReleasingAddress(t *testing.T) {
	store := controlstore.NewMemory()
	provider := &provisionSessionProvider{
		authenticated: true,
		csrf:          "csrf",
		permissions: map[string]any{
			"/":        map[string]bool{"SDN.Allocate": true, "SDN.Use": true},
			"/vms/100": map[string]bool{"VM.Config.Network": true},
		},
	}
	server, port := provisionForDelete(t, store, provider, nil)
	nodeResource, _, err := store.Create(context.Background(), &model.Node{Name: "pve01", ChassisID: "chassis-01", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "delete-node")
	if err != nil {
		t.Fatal(err)
	}
	port.NodeID = nodeResource.(*model.Node).ID
	port.RequestedChassis = nodeResource.(*model.Node).ChassisID
	port.VMID = 100
	port.NIC = "net0"
	port.BindingStatus = model.PortBound
	updatedResource, _, err := store.Update(context.Background(), &port, port.Revision, "attach-for-delete-test")
	if err != nil {
		t.Fatal(err)
	}
	updated := updatedResource.(*model.Port)

	response := request(t, server, http.MethodDelete, "/api/v1/ports/"+updated.ID+"/deprovision", nil, deprovisionHeaders("attached-delete", updated.Revision))
	if response.Code != http.StatusConflict || provisionErrorCode(t, response) != "port_attached" {
		t.Fatalf("attached status=%d body=%s", response.Code, response.Body.String())
	}
	if allocation := mustOnlyAllocation(t, store); allocation.PortID != updated.ID || allocation.State != model.IPAllocated {
		t.Fatalf("attached delete changed allocation=%#v", allocation)
	}
}

func TestPortDeprovisionRecoversReconciliationFailures(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		failKind model.Kind
		replayed bool
	}{
		{name: "allocation", failKind: model.KindIPAllocation, replayed: false},
		{name: "port", failKind: model.KindPort, replayed: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := controlstore.NewMemory()
			provider := &provisionSessionProvider{
				authenticated: true,
				csrf:          "csrf",
				permissions:   map[string]any{"/": map[string]bool{"SDN.Allocate": true}},
			}
			reconciler := &deprovisionTestReconciler{store: store, failKind: testCase.failKind, failCount: 1}
			server, port := provisionForDelete(t, store, provider, reconciler)
			headers := deprovisionHeaders("retry-delete-"+testCase.name, port.Revision)

			failed := request(t, server, http.MethodDelete, "/api/v1/ports/"+port.ID+"/deprovision", nil, headers)
			if failed.Code != http.StatusServiceUnavailable || provisionErrorCode(t, failed) != "reconcile_failed" {
				t.Fatalf("failed status=%d body=%s", failed.Code, failed.Body.String())
			}
			retried := request(t, server, http.MethodDelete, "/api/v1/ports/"+port.ID+"/deprovision", nil, headers)
			if retried.Code != http.StatusNoContent {
				t.Fatalf("retry status=%d headers=%v body=%s", retried.Code, retried.Header(), retried.Body.String())
			}
			if got := retried.Header().Get("Idempotency-Replayed") == "true"; got != testCase.replayed {
				t.Fatalf("replay header=%v want=%v headers=%v", got, testCase.replayed, retried.Header())
			}
			if _, err := store.Get(context.Background(), model.KindPort, port.ID); !errors.Is(err, controlstore.ErrNotFound) {
				t.Fatalf("port remains after retry: %v", err)
			}
			allocations, _ := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
			if len(allocations) != 0 {
				t.Fatalf("allocations remain after retry=%#v", allocations)
			}
		})
	}
}

func mustOnlyAllocation(t *testing.T, store controlstore.Store) *model.IPAllocation {
	t.Helper()
	resources, err := store.List(context.Background(), model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil || len(resources) != 1 {
		t.Fatalf("allocations=%#v err=%v", resources, err)
	}
	return resources[0].(*model.IPAllocation)
}

func provisionErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Error.Code
}
