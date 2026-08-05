package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type lifecycleSessionProvider struct {
	authenticated bool
	csrf          string
	session       Session
}

func (provider *lifecycleSessionProvider) Session(context.Context, *http.Request) (Session, error) {
	if !provider.authenticated {
		return Session{}, ErrUnauthenticated
	}
	return provider.session, nil
}

func (provider *lifecycleSessionProvider) Authorize(_ context.Context, request *http.Request, unsafe bool) (Session, error) {
	if !provider.authenticated {
		return Session{}, ErrUnauthenticated
	}
	if unsafe && request.Header.Get(PVNCSRFHeader) != provider.csrf {
		return Session{}, ErrInvalidCSRF
	}
	return provider.session, nil
}

func lifecycleTopology(t *testing.T) (controlstore.Store, *model.Project, *model.Network, *model.Node, *model.Port) {
	t.Helper()
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
	nodeResource, _, err := store.Create(context.Background(), &model.Node{Name: "pve01", ChassisID: "chassis-01", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}, "node")
	if err != nil {
		t.Fatal(err)
	}
	node := nodeResource.(*model.Node)
	portResource, _, err := store.Create(context.Background(), &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "vm100-net0",
		MACAddress: "02:00:00:00:00:10", AdminStateUp: true,
	}, "port")
	if err != nil {
		t.Fatal(err)
	}
	return store, project, network, node, portResource.(*model.Port)
}

func TestPortAttachRequiresSessionCSRFAndScopedPrivileges(t *testing.T) {
	store, _, _, node, port := lifecycleTopology(t)
	permissions := map[string]any{
		"/pool/pool-tenant": map[string]bool{"SDN.Allocate": true},
	}
	provider := &lifecycleSessionProvider{
		csrf:    "csrf-good",
		session: Session{User: "tenant@pve", Permissions: permissions},
	}
	server := testServer(t, store, provider)
	target := "/api/v1/ports/" + port.ID + "/attach"
	body := map[string]any{"node_id": node.ID, "vmid": 100, "nic": "net0", "generation": port.Generation}
	headers := map[string]string{"Idempotency-Key": "attach-port", "If-Match": `"1"`, PVNCSRFHeader: "csrf-good"}

	unauthenticated := request(t, server, http.MethodPost, target, body, headers)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	provider.authenticated = true
	withoutCSRF := request(t, server, http.MethodPost, target, body, map[string]string{"Idempotency-Key": "attach-port", "If-Match": `"1"`})
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("without CSRF status=%d body=%s", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	withoutUse := request(t, server, http.MethodPost, target, body, headers)
	if withoutUse.Code != http.StatusForbidden {
		t.Fatalf("without SDN.Use status=%d body=%s", withoutUse.Code, withoutUse.Body.String())
	}
	permissions["/pool/pool-tenant"] = map[string]bool{"SDN.Allocate": true, "SDN.Use": true}
	withoutVM := request(t, server, http.MethodPost, target, body, headers)
	if withoutVM.Code != http.StatusForbidden {
		t.Fatalf("without VM.Config.Network status=%d body=%s", withoutVM.Code, withoutVM.Body.String())
	}
	permissions["/vms/100"] = map[string]bool{"VM.Config.Network": true}
	staleGenerationBody := map[string]any{"node_id": node.ID, "vmid": 100, "nic": "net0", "generation": port.Generation + 1}
	staleGenerationHeaders := map[string]string{"Idempotency-Key": "attach-stale-generation", "If-Match": `"1"`, PVNCSRFHeader: "csrf-good"}
	staleGeneration := request(t, server, http.MethodPost, target, staleGenerationBody, staleGenerationHeaders)
	if staleGeneration.Code != http.StatusConflict || apiErrorCode(t, staleGeneration) != "stale_generation" {
		t.Fatalf("stale generation status=%d body=%s", staleGeneration.Code, staleGeneration.Body.String())
	}
	attachedResponse := request(t, server, http.MethodPost, target, body, headers)
	if attachedResponse.Code != http.StatusOK {
		t.Fatalf("attach status=%d body=%s", attachedResponse.Code, attachedResponse.Body.String())
	}
	attached := decodeData[model.Port](t, attachedResponse)
	if attached.BindingStatus != model.PortBinding || attached.NodeID != node.ID || attached.VMID != 100 || attached.NIC != "net0" || attached.RequestedChassis != node.ChassisID || attached.Generation != port.Generation+1 {
		t.Fatalf("attached port = %#v", attached)
	}
	if attachedResponse.Header().Get("ETag") != `"2"` {
		t.Fatalf("attach ETag=%q", attachedResponse.Header().Get("ETag"))
	}

	replayed := request(t, server, http.MethodPost, target, body, headers)
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("attach replay status=%d headers=%v body=%s", replayed.Code, replayed.Header(), replayed.Body.String())
	}
	staleHeaders := map[string]string{"Idempotency-Key": "attach-stale", "If-Match": `"1"`, PVNCSRFHeader: "csrf-good"}
	stale := request(t, server, http.MethodPost, target, body, staleHeaders)
	if stale.Code != http.StatusPreconditionFailed && stale.Code != http.StatusConflict {
		t.Fatalf("stale attach status=%d body=%s", stale.Code, stale.Body.String())
	}

	detachBody := map[string]any{"generation": attached.Generation}
	detachHeaders := map[string]string{"Idempotency-Key": "detach-port", "If-Match": fmt.Sprintf(`"%d"`, attached.Revision), PVNCSRFHeader: "csrf-good"}
	delete(permissions, "/vms/100")
	deniedDetach := request(t, server, http.MethodPost, "/api/v1/ports/"+port.ID+"/detach", detachBody, detachHeaders)
	if deniedDetach.Code != http.StatusForbidden {
		t.Fatalf("detach without VM.Config.Network status=%d body=%s", deniedDetach.Code, deniedDetach.Body.String())
	}
	permissions["/vms/100"] = map[string]bool{"VM.Config.Network": true}
	detachedResponse := request(t, server, http.MethodPost, "/api/v1/ports/"+port.ID+"/detach", detachBody, detachHeaders)
	if detachedResponse.Code != http.StatusOK {
		t.Fatalf("detach status=%d body=%s", detachedResponse.Code, detachedResponse.Body.String())
	}
	detaching := decodeData[model.Port](t, detachedResponse)
	if detaching.BindingStatus != model.PortDetaching || detaching.Generation != attached.Generation || detaching.NodeID != node.ID {
		t.Fatalf("detaching port = %#v", detaching)
	}
	reportPath := "/api/v1/runtime/ports/" + port.ID + "/report"
	reported := request(t, server.RuntimeHandler(), http.MethodPost, reportPath, map[string]any{"generation": detaching.Generation, "status": "unbound"}, nil)
	if reported.Code != http.StatusOK {
		t.Fatalf("unbound report status=%d body=%s", reported.Code, reported.Body.String())
	}
	replayedDetach := request(t, server, http.MethodPost, "/api/v1/ports/"+port.ID+"/detach", detachBody, detachHeaders)
	if replayedDetach.Code != http.StatusOK || replayedDetach.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("completed detach replay status=%d headers=%v body=%s", replayedDetach.Code, replayedDetach.Header(), replayedDetach.Body.String())
	}
}

func TestPortAttachRejectsIdempotencyKeyReuseForAnotherRequest(t *testing.T) {
	store, _, _, node, port := lifecycleTopology(t)
	permissions := map[string]any{
		"/pool/pool-tenant": map[string]bool{"SDN.Allocate": true, "SDN.Use": true},
		"/vms/100":          map[string]bool{"VM.Config.Network": true},
		"/vms/101":          map[string]bool{"VM.Config.Network": true},
	}
	provider := &lifecycleSessionProvider{authenticated: true, csrf: "csrf", session: Session{User: "tenant@pve", Permissions: permissions}}
	server := testServer(t, store, provider)
	target := "/api/v1/ports/" + port.ID + "/attach"
	headers := map[string]string{"Idempotency-Key": "same-key", "If-Match": `"1"`, PVNCSRFHeader: "csrf"}
	first := request(t, server, http.MethodPost, target, map[string]any{"node_id": node.ID, "vmid": 100, "nic": "net0", "generation": 1}, headers)
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	changed := request(t, server, http.MethodPost, target, map[string]any{"node_id": node.ID, "vmid": 101, "nic": "net1", "generation": 1}, headers)
	if changed.Code != http.StatusPreconditionFailed && changed.Code != http.StatusConflict {
		t.Fatalf("changed idempotency request status=%d body=%s", changed.Code, changed.Body.String())
	}
}

func TestRuntimePortReportsUseGenerationCASAndAreUnixOnly(t *testing.T) {
	store, _, _, node, port := lifecycleTopology(t)
	port.NodeID, port.VMID, port.NIC, port.RequestedChassis = node.ID, 100, "net0", node.ChassisID
	port.BindingStatus, port.Generation = model.PortBinding, 2
	_, _, err := store.Update(context.Background(), port, port.Revision, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	provider := &lifecycleSessionProvider{authenticated: true, csrf: "csrf", session: Session{User: "root@pam", Permissions: map[string]any{"/": map[string]bool{"SDN.Audit": true, "SDN.Allocate": true, "SDN.Use": true, "VM.Config.Network": true}}}}
	server := testServer(t, store, provider)
	reportPath := "/api/v1/runtime/ports/" + port.ID + "/report"

	tcp := request(t, server, http.MethodPost, reportPath, map[string]any{"generation": 2, "status": "bound"}, map[string]string{PVNCSRFHeader: "csrf"})
	if tcp.Code != http.StatusNotFound {
		t.Fatalf("TCP report status=%d body=%s", tcp.Code, tcp.Body.String())
	}
	boundResponse := request(t, server.RuntimeHandler(), http.MethodPost, reportPath, map[string]any{"generation": 2, "status": "bound"}, nil)
	if boundResponse.Code != http.StatusOK {
		t.Fatalf("bound report status=%d body=%s", boundResponse.Code, boundResponse.Body.String())
	}
	bound := decodeData[model.Port](t, boundResponse)
	if bound.BindingStatus != model.PortBound {
		t.Fatalf("bound port = %#v", bound)
	}
	readyResource, err := store.MarkReconciled(context.Background(), model.KindPort, bound.ID, bound.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	bound = *readyResource.(*model.Port)
	resolved := request(t, server.RuntimeHandler(), http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve01&vmid=100&nic=net0", nil, nil)
	if resolved.Code != http.StatusOK {
		t.Fatalf("bound resolver status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	var resolution resolvedPort
	if err := decodePlainJSON(resolved.Body.Bytes(), &resolution); err != nil || resolution.Status != model.PortBound {
		t.Fatalf("bound resolution=%#v err=%v", resolution, err)
	}
	stale := request(t, server.RuntimeHandler(), http.MethodPost, reportPath, map[string]any{"generation": 1, "status": "bound"}, nil)
	if stale.Code != http.StatusConflict || apiErrorCode(t, stale) != "stale_generation" {
		t.Fatalf("stale report status=%d body=%s", stale.Code, stale.Body.String())
	}

	bound.BindingStatus = model.PortDetaching
	detachingResource, _, err := store.Update(context.Background(), &bound, bound.Revision, "detach")
	if err != nil {
		t.Fatal(err)
	}
	detaching := detachingResource.(*model.Port)
	detachingResolution := request(t, server.RuntimeHandler(), http.MethodGet, "/api/v1/runtime/ports/resolve?node=pve01&vmid=100&nic=net0", nil, nil)
	if detachingResolution.Code != http.StatusOK {
		t.Fatalf("detaching resolver status=%d body=%s", detachingResolution.Code, detachingResolution.Body.String())
	}
	unboundResponse := request(t, server.RuntimeHandler(), http.MethodPost, reportPath, map[string]any{"generation": 2, "status": "unbound"}, nil)
	if unboundResponse.Code != http.StatusOK {
		t.Fatalf("unbound report status=%d body=%s", unboundResponse.Code, unboundResponse.Body.String())
	}
	unbound := decodeData[model.Port](t, unboundResponse)
	if unbound.BindingStatus != model.PortUnbound || unbound.NodeID != "" || unbound.VMID != 0 || unbound.NIC != "" || unbound.RequestedChassis != "" || unbound.Generation != detaching.Generation {
		t.Fatalf("unbound port = %#v", unbound)
	}
}

func TestRuntimePortResolverFailsClosedUntilOVNRevisionIsApplied(t *testing.T) {
	store, _, _, node, port := lifecycleTopology(t)
	port.NodeID, port.VMID, port.NIC, port.RequestedChassis = node.ID, 100, "net0", node.ChassisID
	port.BindingStatus, port.Generation = model.PortBinding, 2
	pendingResource, _, err := store.Update(context.Background(), port, port.Revision, "prepare-pending")
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingResource.(*model.Port)
	server := testServer(t, store, nil)
	path := "/api/v1/runtime/ports/resolve?node=pve01&vmid=100&nic=net0"

	response := request(t, server.RuntimeHandler(), http.MethodGet, path, nil, nil)
	if response.Code != http.StatusConflict || apiErrorCode(t, response) != "port_not_bindable" {
		t.Fatalf("pending resolution status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := store.MarkReconciled(context.Background(), model.KindPort, pending.ID, pending.Revision, nil); err != nil {
		t.Fatal(err)
	}
	response = request(t, server.RuntimeHandler(), http.MethodGet, path, nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("ready resolution status=%d body=%s", response.Code, response.Body.String())
	}
}

func decodePlainJSON(payload []byte, destination any) error {
	return json.Unmarshal(payload, destination)
}

func apiErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
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
