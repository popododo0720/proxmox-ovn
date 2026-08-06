package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestDefaultSecurityGroupBackfillAuthorizationAndConfirmation(t *testing.T) {
	store := controlstore.NewMemory()
	permissions := map[string]any{}
	server := newDefaultSecurityGroupBackfillTestServer(t, store, nil, permissions)

	response := request(t, server, http.MethodGet, defaultSecurityGroupBackfillPlanPath, nil, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("GET without global audit status=%d body=%s", response.Code, response.Body.String())
	}

	permissions[globalPath] = map[string]bool{"SDN.Audit": true}
	response = request(t, server, http.MethodGet, defaultSecurityGroupBackfillPlanPath, nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET with global audit status=%d body=%s", response.Code, response.Body.String())
	}
	response = request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{}, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("POST without allocate status=%d body=%s", response.Code, response.Body.String())
	}

	permissions[globalPath] = map[string]bool{"SDN.Audit": true, "SDN.Allocate": true}
	response = request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{}, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("POST without Sys.Modify status=%d body=%s", response.Code, response.Body.String())
	}

	permissions[globalPath] = map[string]bool{"SDN.Audit": true, "Sys.Modify": true}
	response = request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{}, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("POST without allocate status=%d body=%s", response.Code, response.Body.String())
	}

	permissions[globalPath] = map[string]bool{"SDN.Audit": true, "SDN.Allocate": true, "Sys.Modify": true}
	response = request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("default dry run status=%d body=%s", response.Code, response.Body.String())
	}
	if report := decodeData[defaultSecurityGroupBackfillApplyData](t, response); !report.DryRun {
		t.Fatal("omitted dry_run performed an actual apply")
	}

	for _, confirmation := range []string{"", "cluster-other", " cluster-a "} {
		response = request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{"dry_run": false, "confirm": confirmation}, nil)
		if response.Code != http.StatusBadRequest || apiErrorCode(t, response) != "confirmation_required" {
			t.Fatalf("confirm=%q status=%d body=%s", confirmation, response.Code, response.Body.String())
		}
	}

	response = request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{"unknown": true}, nil)
	if response.Code != http.StatusBadRequest || apiErrorCode(t, response) != "invalid_json" {
		t.Fatalf("unknown field status=%d body=%s", response.Code, response.Body.String())
	}
	response = request(t, server, http.MethodDelete, defaultSecurityGroupBackfillPlanPath, nil, nil)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("plan method status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDefaultSecurityGroupBackfillPlanIsReadOnlyAndHumanReadable(t *testing.T) {
	store := controlstore.NewMemory()
	reconciler := &defaultSecurityGroupBackfillReadyReconciler{store: store}
	alpha, alphaNetwork := createDefaultSecurityGroupBackfillProject(t, store, "alpha", "pool-alpha")
	zeta, zetaNetwork := createDefaultSecurityGroupBackfillProject(t, store, "zeta", "pool-zeta")
	node := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Node{
		Name: "pve-a", ChassisID: "chassis-a", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true,
	}).(*model.Node)

	if _, err := defaultsecurity.New(store, reconciler).Ensure(context.Background(), alpha.ID); err != nil {
		t.Fatal(err)
	}
	custom := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.SecurityGroup{
		ProjectID: alpha.ID, Name: "web", Stateful: true,
	}).(*model.SecurityGroup)
	attached := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Port{
		ProjectID: alpha.ID, NetworkID: alphaNetwork.ID, Name: "vm101-net0", MACAddress: "02:00:00:00:10:01",
		AdminStateUp: true, BindingStatus: model.PortBound, NodeID: node.ID, VMID: 101, NIC: "net0", RequestedChassis: node.ChassisID,
	}).(*model.Port)
	explicit := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Port{
		ProjectID: alpha.ID, NetworkID: alphaNetwork.ID, Name: "explicit", MACAddress: "02:00:00:00:10:02",
		AdminStateUp: true, SecurityGroupIDs: []string{custom.ID},
	}).(*model.Port)
	missingPolicy := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Port{
		ProjectID: zeta.ID, NetworkID: zetaNetwork.ID, Name: "legacy", MACAddress: "02:00:00:00:20:01", AdminStateUp: true,
	}).(*model.Port)
	reconciler.reset()

	permissions := map[string]any{globalPath: map[string]bool{"SDN.Audit": true}}
	server := newDefaultSecurityGroupBackfillTestServer(t, store, reconciler, permissions)
	response := request(t, server, http.MethodGet, defaultSecurityGroupBackfillPlanPath, nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", response.Code, response.Body.String())
	}
	plan := decodeData[defaultSecurityGroupBackfillPlanData](t, response)
	if plan.Cluster != "cluster-a" || plan.GeneratedAt != "2026-08-06T01:02:03Z" {
		t.Fatalf("cluster=%q generated_at=%q", plan.Cluster, plan.GeneratedAt)
	}
	if plan.Warning != defaultSecurityGroupBackfillWarning || !plan.CanApply || plan.TotalLegacyPorts != 2 || plan.TotalAttachedPorts != 1 {
		t.Fatalf("plan summary=%+v", plan)
	}
	if len(plan.Projects) != 2 || plan.Projects[0].ProjectName != "alpha" || plan.Projects[1].ProjectName != "zeta" {
		t.Fatalf("projects=%+v", plan.Projects)
	}
	alphaPlan, zetaPlan := plan.Projects[0], plan.Projects[1]
	if !alphaPlan.DefaultReady || alphaPlan.DefaultSecurityGroupName != defaultsecurity.DefaultSecurityGroupName || len(alphaPlan.LegacyPorts) != 1 {
		t.Fatalf("alpha plan=%+v", alphaPlan)
	}
	if alphaPlan.LegacyPorts[0].PortID != attached.ID || !alphaPlan.LegacyPorts[0].Attached || alphaPlan.LegacyPorts[0].NodeName != node.Name || alphaPlan.LegacyPorts[0].VMID != 101 || alphaPlan.LegacyPorts[0].NIC != "net0" {
		t.Fatalf("attached plan=%+v", alphaPlan.LegacyPorts[0])
	}
	if zetaPlan.DefaultReady || zetaPlan.BlockedReason != defaultsecurity.BlockedNone || len(zetaPlan.MissingResourceIDs) != 3 || len(zetaPlan.LegacyPorts) != 1 || zetaPlan.LegacyPorts[0].PortID != missingPolicy.ID {
		t.Fatalf("zeta plan=%+v", zetaPlan)
	}
	if zetaPlan.DefaultSecurityGroupID != defaultsecurity.DefaultSecurityGroupID(zeta.ID) {
		t.Fatalf("default ID=%q", zetaPlan.DefaultSecurityGroupID)
	}
	if reconciler.callCount() != 0 {
		t.Fatalf("read-only plan reconciled %d resources", reconciler.callCount())
	}
	assertDefaultSecurityGroupBackfillPort(t, store, attached.ID, attached.Revision, nil)
	assertDefaultSecurityGroupBackfillPort(t, store, explicit.ID, explicit.Revision, []string{custom.ID})
	assertDefaultSecurityGroupBackfillPort(t, store, missingPolicy.ID, missingPolicy.Revision, nil)
	if _, err := store.Get(context.Background(), model.KindSecurityGroup, defaultsecurity.DefaultSecurityGroupID(zeta.ID)); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("plan created zeta default policy: %v", err)
	}
}

func TestDefaultSecurityGroupBackfillApplyIsBoundedRerunnableAndPartial(t *testing.T) {
	store := controlstore.NewMemory()
	reconciler := &defaultSecurityGroupBackfillReadyReconciler{store: store}
	good, goodNetwork := createDefaultSecurityGroupBackfillProject(t, store, "good", "pool-good")
	blocked, blockedNetwork := createDefaultSecurityGroupBackfillProject(t, store, "blocked", "pool-blocked")
	goodA := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Port{
		ProjectID: good.ID, NetworkID: goodNetwork.ID, Name: "legacy-a", MACAddress: "02:00:00:00:30:01", AdminStateUp: true,
	}).(*model.Port)
	goodB := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Port{
		ProjectID: good.ID, NetworkID: goodNetwork.ID, Name: "legacy-b", MACAddress: "02:00:00:00:30:02", AdminStateUp: true,
	}).(*model.Port)
	custom := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.SecurityGroup{
		ProjectID: good.ID, Name: "custom", Stateful: true,
	}).(*model.SecurityGroup)
	explicit := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Port{
		ProjectID: good.ID, NetworkID: goodNetwork.ID, Name: "explicit", MACAddress: "02:00:00:00:30:03",
		AdminStateUp: true, SecurityGroupIDs: []string{custom.ID},
	}).(*model.Port)
	mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.SecurityGroup{
		ProjectID: blocked.ID, Name: defaultsecurity.DefaultSecurityGroupName, Stateful: true,
	})
	blockedPort := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Port{
		ProjectID: blocked.ID, NetworkID: blockedNetwork.ID, Name: "blocked-port", MACAddress: "02:00:00:00:40:01", AdminStateUp: true,
	}).(*model.Port)

	permissions := map[string]any{globalPath: map[string]bool{"SDN.Audit": true, "SDN.Allocate": true, "Sys.Modify": true}}
	server := newDefaultSecurityGroupBackfillTestServer(t, store, reconciler, permissions)
	dryResponse := request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{}, nil)
	if dryResponse.Code != http.StatusOK {
		t.Fatalf("dry run status=%d body=%s", dryResponse.Code, dryResponse.Body.String())
	}
	dryReport := decodeData[defaultSecurityGroupBackfillApplyData](t, dryResponse)
	if !dryReport.DryRun || dryReport.Planned != 3 || dryReport.Migrated != 0 || dryReport.Failed != 1 || len(dryReport.Results) != 3 {
		t.Fatalf("dry report=%+v", dryReport)
	}
	assertDefaultSecurityGroupBackfillPort(t, store, goodA.ID, goodA.Revision, nil)
	if _, err := store.Get(context.Background(), model.KindSecurityGroup, defaultsecurity.DefaultSecurityGroupID(good.ID)); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("dry run created policy: %v", err)
	}

	applyResponse := request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{"dry_run": false, "confirm": "cluster-a"}, nil)
	if applyResponse.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applyResponse.Code, applyResponse.Body.String())
	}
	report := decodeData[defaultSecurityGroupBackfillApplyData](t, applyResponse)
	if report.DryRun || report.Planned != 3 || report.Migrated != 2 || report.Skipped != 0 || report.Failed != 1 || len(report.Results) != 3 {
		t.Fatalf("apply report=%+v", report)
	}
	defaultID := defaultsecurity.DefaultSecurityGroupID(good.ID)
	assertDefaultSecurityGroupBackfillPort(t, store, goodA.ID, goodA.Revision+1, []string{defaultID})
	assertDefaultSecurityGroupBackfillPort(t, store, goodB.ID, goodB.Revision+1, []string{defaultID})
	assertDefaultSecurityGroupBackfillPort(t, store, explicit.ID, explicit.Revision, []string{custom.ID})
	assertDefaultSecurityGroupBackfillPort(t, store, blockedPort.ID, blockedPort.Revision, nil)
	inspection, err := defaultsecurity.New(store, reconciler).Inspect(context.Background(), good.ID)
	if err != nil || !inspection.Ready {
		t.Fatalf("good baseline=%+v err=%v", inspection, err)
	}

	rerunResponse := request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{"dry_run": false, "confirm": "cluster-a"}, nil)
	if rerunResponse.Code != http.StatusOK {
		t.Fatalf("rerun status=%d body=%s", rerunResponse.Code, rerunResponse.Body.String())
	}
	rerun := decodeData[defaultSecurityGroupBackfillApplyData](t, rerunResponse)
	if rerun.Planned != 1 || rerun.Migrated != 0 || rerun.Failed != 1 || len(rerun.Results) != 1 || rerun.Results[0].PortID != blockedPort.ID {
		t.Fatalf("rerun report=%+v", rerun)
	}
}

func TestDefaultSecurityGroupBackfillRevisionCASCanBeRetried(t *testing.T) {
	base := controlstore.NewMemory()
	project, network := createDefaultSecurityGroupBackfillProject(t, base, "retry", "pool-retry")
	failPort := mustCreateDefaultSecurityGroupBackfillResource(t, base, &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "a-fail-once", MACAddress: "02:00:00:00:50:01", AdminStateUp: true,
	}).(*model.Port)
	okPort := mustCreateDefaultSecurityGroupBackfillResource(t, base, &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "b-succeeds", MACAddress: "02:00:00:00:50:02", AdminStateUp: true,
	}).(*model.Port)
	store := &failOnceDefaultSecurityGroupBackfillStore{Store: base, portID: failPort.ID}
	reconciler := &defaultSecurityGroupBackfillReadyReconciler{store: store}
	permissions := map[string]any{globalPath: map[string]bool{"SDN.Audit": true, "SDN.Allocate": true, "Sys.Modify": true}}
	server := newDefaultSecurityGroupBackfillTestServer(t, store, reconciler, permissions)

	response := request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{"dry_run": false, "confirm": "cluster-a"}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("first apply status=%d body=%s", response.Code, response.Body.String())
	}
	first := decodeData[defaultSecurityGroupBackfillApplyData](t, response)
	if first.Planned != 2 || first.Migrated != 1 || first.Failed != 1 || first.Skipped != 0 {
		t.Fatalf("first report=%+v", first)
	}
	defaultID := defaultsecurity.DefaultSecurityGroupID(project.ID)
	assertDefaultSecurityGroupBackfillPort(t, store, failPort.ID, failPort.Revision, nil)
	assertDefaultSecurityGroupBackfillPort(t, store, okPort.ID, okPort.Revision+1, []string{defaultID})

	response = request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{"dry_run": false, "confirm": "cluster-a"}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", response.Code, response.Body.String())
	}
	retry := decodeData[defaultSecurityGroupBackfillApplyData](t, response)
	if retry.Planned != 1 || retry.Migrated != 1 || retry.Failed != 0 || len(retry.Results) != 1 || retry.Results[0].PortID != failPort.ID {
		t.Fatalf("retry report=%+v", retry)
	}
	assertDefaultSecurityGroupBackfillPort(t, store, failPort.ID, failPort.Revision+1, []string{defaultID})
}

func TestDefaultSecurityGroupBackfillRequiresReadyBaseline(t *testing.T) {
	store := controlstore.NewMemory()
	project, network := createDefaultSecurityGroupBackfillProject(t, store, "not-ready", "pool-not-ready")
	port := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "legacy", MACAddress: "02:00:00:00:60:01", AdminStateUp: true,
	}).(*model.Port)
	reconciler := &defaultSecurityGroupBackfillReadyReconciler{store: store, failKind: model.KindSecurityGroup}
	permissions := map[string]any{globalPath: map[string]bool{"SDN.Audit": true, "SDN.Allocate": true, "Sys.Modify": true}}
	server := newDefaultSecurityGroupBackfillTestServer(t, store, reconciler, permissions)

	response := request(t, server, http.MethodPost, defaultSecurityGroupBackfillApplyPath, map[string]any{"dry_run": false, "confirm": "cluster-a"}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", response.Code, response.Body.String())
	}
	report := decodeData[defaultSecurityGroupBackfillApplyData](t, response)
	if report.Planned != 1 || report.Migrated != 0 || report.Failed != 1 || len(report.Results) != 1 || !strings.Contains(report.Results[0].Error, "OVN unavailable") {
		t.Fatalf("report=%+v", report)
	}
	assertDefaultSecurityGroupBackfillPort(t, store, port.ID, port.Revision, nil)
}

func newDefaultSecurityGroupBackfillTestServer(t *testing.T, store controlstore.Store, reconciler Reconciler, permissions map[string]any) *Server {
	t.Helper()
	server, err := New(Options{
		Store: store, Reconciler: reconciler, ClusterName: "cluster-a",
		Clock: func() time.Time { return time.Date(2026, 8, 6, 1, 2, 3, 0, time.UTC) },
		SessionProvider: SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
			return Session{User: "root@pam", Permissions: permissions, Cluster: "cluster-a"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func createDefaultSecurityGroupBackfillProject(t *testing.T, store controlstore.Store, name, pool string) (*model.Project, *model.Network) {
	t.Helper()
	project := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Project{Name: name, PoolID: pool}).(*model.Project)
	network := mustCreateDefaultSecurityGroupBackfillResource(t, store, &model.Network{ProjectID: project.ID, Name: name + "-network"}).(*model.Network)
	return project, network
}

func mustCreateDefaultSecurityGroupBackfillResource(t *testing.T, store controlstore.Store, resource model.Resource) model.Resource {
	t.Helper()
	created, replayed, err := store.Create(context.Background(), resource, "")
	if err != nil || replayed {
		t.Fatalf("Create(%s) replayed=%v err=%v", resource.ResourceKind(), replayed, err)
	}
	return created
}

func assertDefaultSecurityGroupBackfillPort(t *testing.T, store controlstore.Store, id string, revision int64, groupIDs []string) {
	t.Helper()
	resource, err := store.Get(context.Background(), model.KindPort, id)
	if err != nil {
		t.Fatal(err)
	}
	port := resource.(*model.Port)
	if port.Revision != revision || len(port.SecurityGroupIDs) != len(groupIDs) {
		t.Fatalf("port=%s revision=%d groups=%v want revision=%d groups=%v", id, port.Revision, port.SecurityGroupIDs, revision, groupIDs)
	}
	for index := range groupIDs {
		if port.SecurityGroupIDs[index] != groupIDs[index] {
			t.Fatalf("port=%s groups=%v want %v", id, port.SecurityGroupIDs, groupIDs)
		}
	}
}

type defaultSecurityGroupBackfillReadyReconciler struct {
	store    controlstore.Store
	failKind model.Kind
	mu       sync.Mutex
	calls    int
}

func (r *defaultSecurityGroupBackfillReadyReconciler) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if kind == r.failKind {
		return errors.New("OVN unavailable")
	}
	resource, err := r.store.Get(ctx, kind, id)
	if err != nil {
		return err
	}
	_, err = r.store.MarkReconciled(ctx, kind, id, resource.GetMetadata().Revision, nil)
	return err
}

func (r *defaultSecurityGroupBackfillReadyReconciler) reset() {
	r.mu.Lock()
	r.calls = 0
	r.mu.Unlock()
}

func (r *defaultSecurityGroupBackfillReadyReconciler) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type failOnceDefaultSecurityGroupBackfillStore struct {
	controlstore.Store
	portID string
	mu     sync.Mutex
	failed bool
}

func (s *failOnceDefaultSecurityGroupBackfillStore) Update(ctx context.Context, resource model.Resource, expected int64, key string) (model.Resource, bool, error) {
	s.mu.Lock()
	if resource.ResourceKind() == model.KindPort && resource.GetMetadata().ID == s.portID && !s.failed {
		s.failed = true
		s.mu.Unlock()
		return nil, false, &controlstore.Error{Kind: controlstore.ErrPrecondition, Message: "simulated concurrent revision change"}
	}
	s.mu.Unlock()
	return s.Store.Update(ctx, resource, expected, key)
}
