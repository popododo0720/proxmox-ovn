package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
	"github.com/popododo0720/proxmox-ovn/internal/reconcile"
)

func TestProjectCreateAndReplayEnsureDefaultSecurityPolicy(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	project := createAPIProject(t, server)

	assertStoredDefaultPolicy(t, store, project.ID)
	replay := request(t, server, http.MethodPost, "/api/v1/projects", map[string]any{
		"name": "tenant", "pool_id": "pool-tenant",
	}, map[string]string{"Idempotency-Key": "project-create"})
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	assertStoredDefaultPolicy(t, store, project.ID)
}

func TestGenericPortCreateUsesDefaultAfterAuthorization(t *testing.T) {
	store := controlstore.NewMemory()
	project := createAPIResource(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "project").(*model.Project)
	network := createAPIResource(t, store, &model.Network{ProjectID: project.ID, Name: "private"}, "network").(*model.Network)
	permissions := map[string]any{}
	server := testServer(t, store, SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "tenant@pve", Permissions: permissions}, nil
	}))
	body := map[string]any{
		"project_id": project.ID, "network_id": network.ID, "name": "vm100-net0",
		"mac_address": "02:00:00:00:00:10", "admin_state_up": true,
	}
	denied := request(t, server, http.MethodPost, "/api/v1/ports", body, map[string]string{"Idempotency-Key": "denied"})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied status=%d body=%s", denied.Code, denied.Body.String())
	}
	groups, _ := store.List(context.Background(), model.KindSecurityGroup, controlstore.ListOptions{ProjectID: project.ID})
	if len(groups) != 0 {
		t.Fatalf("authorization failure created %d security groups", len(groups))
	}

	permissions["/pool/pool-tenant"] = map[string]bool{"SDN.Allocate": true}
	createdResponse := request(t, server, http.MethodPost, "/api/v1/ports", body, map[string]string{"Idempotency-Key": "allowed"})
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeData[model.Port](t, createdResponse)
	wantGroup := defaultsecurity.DefaultSecurityGroupID(project.ID)
	if fmt.Sprint(created.SecurityGroupIDs) != fmt.Sprint([]string{wantGroup}) {
		t.Fatalf("port security groups=%v want [%s]", created.SecurityGroupIDs, wantGroup)
	}
	assertStoredDefaultPolicy(t, store, project.ID)
}

func TestGenericPortCreateFailsClosedWhenDefaultCannotReconcile(t *testing.T) {
	store := controlstore.NewMemory()
	project := createAPIResource(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "project").(*model.Project)
	network := createAPIResource(t, store, &model.Network{ProjectID: project.ID, Name: "private"}, "network").(*model.Network)
	want := errors.New("OVN unavailable")
	server, err := New(Options{Store: store, Reconciler: reconcileFunc(func(context.Context, model.Kind, string) error { return want })})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodPost, "/api/v1/ports", map[string]any{
		"project_id": project.ID, "network_id": network.ID, "name": "blocked",
		"mac_address": "02:00:00:00:00:11", "admin_state_up": true,
	}, map[string]string{"Idempotency-Key": "blocked"})
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "default_security_policy_unavailable") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	ports, _ := store.List(context.Background(), model.KindPort, controlstore.ListOptions{ProjectID: project.ID})
	if len(ports) != 0 {
		t.Fatalf("failed default reconciliation created %d ports", len(ports))
	}
	if _, err := store.Get(context.Background(), model.KindSecurityGroupRule, defaultsecurity.DefaultEgressRuleID(project.ID)); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("allow rule exists despite default-drop realization failure: %v", err)
	}
}

func TestPortUpdatePreservesLegacyEmptyPolicyBoundary(t *testing.T) {
	store := controlstore.NewMemory()
	project := createAPIResource(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "project").(*model.Project)
	network := createAPIResource(t, store, &model.Network{ProjectID: project.ID, Name: "private"}, "network").(*model.Network)
	legacy := createAPIResource(t, store, &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "legacy", MACAddress: "02:00:00:00:00:12", AdminStateUp: true,
	}, "legacy-port").(*model.Port)
	server := testServer(t, store, nil)

	legacy.Name = "unrelated-change"
	blocked := request(t, server, http.MethodPut, "/api/v1/ports/"+legacy.ID, legacy, map[string]string{
		"Idempotency-Key": "legacy-update", "If-Match": `"1"`,
	})
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), "legacy_port_security_unset") {
		t.Fatalf("legacy update status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	stored, _ := store.Get(context.Background(), model.KindPort, legacy.ID)
	if stored.(*model.Port).Name != "legacy" || len(stored.(*model.Port).SecurityGroupIDs) != 0 {
		t.Fatalf("blocked update changed port=%+v", stored)
	}

	group, err := defaultsecurity.New(store, nil).Ensure(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	secured := createAPIResource(t, store, &model.Port{
		ProjectID: project.ID, NetworkID: network.ID, Name: "secured", MACAddress: "02:00:00:00:00:13",
		AdminStateUp: true, SecurityGroupIDs: []string{group.ID},
	}, "secured-port").(*model.Port)
	secured.Name = "still-secured"
	secured.SecurityGroupIDs = nil
	updatedResponse := request(t, server, http.MethodPut, "/api/v1/ports/"+secured.ID, secured, map[string]string{
		"Idempotency-Key": "secured-update", "If-Match": `"1"`,
	})
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("secured update status=%d body=%s", updatedResponse.Code, updatedResponse.Body.String())
	}
	updated := decodeData[model.Port](t, updatedResponse)
	if fmt.Sprint(updated.SecurityGroupIDs) != fmt.Sprint([]string{group.ID}) {
		t.Fatalf("updated security groups=%v", updated.SecurityGroupIDs)
	}
	replayedResponse := request(t, server, http.MethodPut, "/api/v1/ports/"+secured.ID, secured, map[string]string{
		"Idempotency-Key": "secured-update", "If-Match": `"1"`,
	})
	if replayedResponse.Code != http.StatusOK || replayedResponse.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("secured replay status=%d headers=%v body=%s", replayedResponse.Code, replayedResponse.Header(), replayedResponse.Body.String())
	}
}

func TestReservedDefaultPolicyRejectsGenericMutation(t *testing.T) {
	store := controlstore.NewMemory()
	project := createAPIResource(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "project").(*model.Project)
	group, err := defaultsecurity.New(store, nil).Ensure(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := testServer(t, store, nil)
	group.Description = "weakened"
	update := request(t, server, http.MethodPut, "/api/v1/security-groups/"+group.ID, group, map[string]string{
		"Idempotency-Key": "update-default", "If-Match": `"1"`,
	})
	if update.Code != http.StatusConflict || !strings.Contains(update.Body.String(), "reserved_default_security_policy") {
		t.Fatalf("group update status=%d body=%s", update.Code, update.Body.String())
	}
	deleteGroup := request(t, server, http.MethodDelete, "/api/v1/security-groups/"+group.ID, nil, map[string]string{
		"Idempotency-Key": "delete-default", "If-Match": `"1"`,
	})
	if deleteGroup.Code != http.StatusConflict {
		t.Fatalf("group delete status=%d body=%s", deleteGroup.Code, deleteGroup.Body.String())
	}

	ruleResource, _ := store.Get(context.Background(), model.KindSecurityGroupRule, defaultsecurity.DefaultEgressRuleID(project.ID))
	rule := ruleResource.(*model.SecurityGroupRule)
	rule.ProjectID = "different-project"
	updateRule := request(t, server, http.MethodPut, "/api/v1/security-group-rules/"+rule.ID, rule, map[string]string{
		"Idempotency-Key": "move-default-rule", "If-Match": `"1"`,
	})
	if updateRule.Code != http.StatusConflict || !strings.Contains(updateRule.Body.String(), "reserved_default_security_policy") {
		t.Fatalf("rule update status=%d body=%s", updateRule.Code, updateRule.Body.String())
	}
	deleteRule := request(t, server, http.MethodDelete, "/api/v1/security-group-rules/"+rule.ID, nil, map[string]string{
		"Idempotency-Key": "delete-default-rule", "If-Match": `"1"`,
	})
	if deleteRule.Code != http.StatusConflict {
		t.Fatalf("rule delete status=%d body=%s", deleteRule.Code, deleteRule.Body.String())
	}
}

func TestProjectDeleteCleansOnlyBaselineAndPreservesReplay(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	project := createAPIProject(t, server)

	deleted := request(t, server, http.MethodDelete, "/api/v1/projects/"+project.ID, nil, map[string]string{
		"Idempotency-Key": "delete-project", "If-Match": `"1"`,
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	assertDefaultPolicyMissing(t, store, project.ID)
	if _, err := store.Get(context.Background(), model.KindProject, project.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("project still exists: %v", err)
	}
	replayed := request(t, server, http.MethodDelete, "/api/v1/projects/"+project.ID, nil, map[string]string{
		"Idempotency-Key": "delete-project", "If-Match": `"1"`,
	})
	if replayed.Code != http.StatusNoContent || replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("delete replay status=%d headers=%v body=%s", replayed.Code, replayed.Header(), replayed.Body.String())
	}
}

func TestProjectDeleteReplayFromDeletingTombstoneAndDifferentKey(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		requestKey string
		wantStatus int
		wantReplay bool
	}{
		{name: "same key", requestKey: "lost-response", wantStatus: http.StatusNoContent, wantReplay: true},
		{name: "different key", requestKey: "different", wantStatus: http.StatusPreconditionFailed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := controlstore.NewMemory()
			server := testServer(t, store, nil)
			project := createAPIProject(t, server)
			if err := server.cleanupProjectDefaultSecurity(context.Background(), project.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "lost-response"); err != nil {
				t.Fatal(err)
			}
			response := request(t, server, http.MethodDelete, "/api/v1/projects/"+project.ID, nil, map[string]string{
				"Idempotency-Key": testCase.requestKey, "If-Match": `"1"`,
			})
			if response.Code != testCase.wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Idempotency-Replayed") == "true"; got != testCase.wantReplay {
				t.Fatalf("replay=%v want %v", got, testCase.wantReplay)
			}
		})
	}
}

func TestProjectDeleteReplayCleansBaselineCreatedAfterTombstone(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	project := createAPIProject(t, server)
	if err := server.cleanupProjectDefaultSecurity(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	tombstone, _, err := store.BeginDelete(context.Background(), model.KindProject, project.ID, project.Revision, "delete-race")
	if err != nil {
		t.Fatal(err)
	}
	groupID := defaultsecurity.DefaultSecurityGroupID(project.ID)
	createAPIResource(t, store, &model.SecurityGroup{
		Metadata: model.Metadata{ID: groupID}, ProjectID: project.ID, Name: defaultsecurity.DefaultSecurityGroupName,
		Description: defaultsecurity.DefaultSecurityGroupDescription,
	}, "")
	createAPIResource(t, store, &model.SecurityGroupRule{
		Metadata: model.Metadata{ID: defaultsecurity.DefaultEgressRuleID(project.ID)}, ProjectID: project.ID,
		SecurityGroupID: groupID, Direction: model.DirectionEgress, EtherType: model.EtherTypeIPv4,
		Action: model.ActionAllow, Description: defaultsecurity.DefaultEgressDescription,
	}, "")
	createAPIResource(t, store, &model.SecurityGroupRule{
		Metadata: model.Metadata{ID: defaultsecurity.DefaultIngressRuleID(project.ID)}, ProjectID: project.ID,
		SecurityGroupID: groupID, Direction: model.DirectionIngress, EtherType: model.EtherTypeIPv4,
		RemoteGroupID: groupID, Action: model.ActionAllow, Description: defaultsecurity.DefaultIngressDescription,
	}, "")
	if err := store.Purge(context.Background(), model.KindProject, project.ID, tombstone.GetMetadata().Revision); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("first purge error=%v want late baseline conflict", err)
	}

	response := request(t, server, http.MethodDelete, "/api/v1/projects/"+project.ID, nil, map[string]string{
		"Idempotency-Key": "delete-race", "If-Match": `"1"`,
	})
	if response.Code != http.StatusNoContent || response.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	assertDefaultPolicyMissing(t, store, project.ID)
	if _, err := store.Get(context.Background(), model.KindProject, project.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("project remains after replay cleanup: %v", err)
	}
}

func TestProjectDeleteDoesNotRemoveBaselineWhenTenantChildrenRemain(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	project := createAPIProject(t, server)
	createAPIResource(t, store, &model.Network{ProjectID: project.ID, Name: "private"}, "network")

	response := request(t, server, http.MethodDelete, "/api/v1/projects/"+project.ID, nil, map[string]string{
		"Idempotency-Key": "delete-with-child", "If-Match": `"1"`,
	})
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	assertStoredDefaultPolicy(t, store, project.ID)
}

func TestDefaultPolicyAndPortAreReadyWithController(t *testing.T) {
	store := controlstore.NewMemory()
	controller := reconcile.NewController(store, reconcile.NewFakeRenderer())
	server, err := New(Options{Store: store, Reconciler: controller})
	if err != nil {
		t.Fatal(err)
	}
	project := createAPIProject(t, server)
	for _, target := range []struct {
		kind model.Kind
		id   string
	}{
		{model.KindSecurityGroup, defaultsecurity.DefaultSecurityGroupID(project.ID)},
		{model.KindSecurityGroupRule, defaultsecurity.DefaultEgressRuleID(project.ID)},
		{model.KindSecurityGroupRule, defaultsecurity.DefaultIngressRuleID(project.ID)},
	} {
		resource, err := store.Get(context.Background(), target.kind, target.id)
		if err != nil {
			t.Fatal(err)
		}
		if resource.GetMetadata().State != model.ResourceReady || resource.GetMetadata().AppliedRevision != resource.GetMetadata().Revision {
			t.Fatalf("%s was not ready: %+v", target.kind, resource.GetMetadata())
		}
	}
	networkResponse := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{
		"project_id": project.ID, "name": "private",
	}, map[string]string{"Idempotency-Key": "ready-network"})
	if networkResponse.Code != http.StatusCreated {
		t.Fatalf("network status=%d body=%s", networkResponse.Code, networkResponse.Body.String())
	}
	network := decodeData[model.Network](t, networkResponse)
	portResponse := request(t, server, http.MethodPost, "/api/v1/ports", map[string]any{
		"project_id": project.ID, "network_id": network.ID, "name": "ready-port",
		"mac_address": "02:00:00:00:00:14", "admin_state_up": true,
	}, map[string]string{"Idempotency-Key": "ready-port"})
	if portResponse.Code != http.StatusCreated {
		t.Fatalf("port status=%d body=%s", portResponse.Code, portResponse.Body.String())
	}
	port := decodeData[model.Port](t, portResponse)
	if port.State != model.ResourceReady || fmt.Sprint(port.SecurityGroupIDs) != fmt.Sprint([]string{defaultsecurity.DefaultSecurityGroupID(project.ID)}) {
		t.Fatalf("ready port=%+v", port)
	}
}

func TestInterruptedProvisionAdoptsDefaultButSucceededLegacyReplayDoesNot(t *testing.T) {
	for _, succeeded := range []bool{false, true} {
		name := "interrupted"
		if succeeded {
			name = "succeeded"
		}
		t.Run(name, func(t *testing.T) {
			store := controlstore.NewMemory()
			topology := seedProvisionTopology(t, store, "10.0.0.0/29", nil)
			provider := &provisionSessionProvider{
				authenticated: true,
				permissions:   map[string]any{"/pool/pool-tenant": map[string]bool{"SDN.Allocate": true}},
			}
			server := testServer(t, store, provider)
			key := "upgrade-replay-" + name
			input := portProvisionRequest{ProjectID: topology.project.ID, NetworkID: topology.network.ID, Name: "upgrade-port"}
			if err := normalizeProvisionRequest(&input); err != nil {
				t.Fatal(err)
			}
			identity, err := newProvisionIdentity(key, input)
			if err != nil {
				t.Fatal(err)
			}
			legacy := provisionPortResource(identity, input)
			operation, _, err := server.beginPortProvision(context.Background(), key, identity, legacy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Create(context.Background(), legacy, ""); err != nil {
				t.Fatal(err)
			}
			if succeeded {
				if err := server.completePortProvision(context.Background(), operation); err != nil {
					t.Fatal(err)
				}
			}
			response := request(t, server, http.MethodPost, "/api/v1/ports/provision", map[string]any{
				"project_id": topology.project.ID, "network_id": topology.network.ID, "name": "upgrade-port",
			}, provisionHeaders(key))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			port := decodeData[model.Port](t, response)
			if succeeded {
				if len(port.SecurityGroupIDs) != 0 || port.Revision != 1 {
					t.Fatalf("succeeded legacy replay was changed: %+v", port)
				}
				groups, _ := store.List(context.Background(), model.KindSecurityGroup, controlstore.ListOptions{ProjectID: topology.project.ID})
				if len(groups) != 0 {
					t.Fatalf("succeeded legacy replay unexpectedly repaired policy: %+v", groups)
				}
				return
			}
			if fmt.Sprint(port.SecurityGroupIDs) != fmt.Sprint([]string{defaultsecurity.DefaultSecurityGroupID(topology.project.ID)}) || port.Revision != 2 {
				t.Fatalf("interrupted port was not safely migrated: %+v", port)
			}
		})
	}
}

type reconcileFunc func(context.Context, model.Kind, string) error

func (function reconcileFunc) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	return function(ctx, kind, id)
}

func assertStoredDefaultPolicy(t *testing.T, store controlstore.Store, projectID string) {
	t.Helper()
	group, err := store.Get(context.Background(), model.KindSecurityGroup, defaultsecurity.DefaultSecurityGroupID(projectID))
	if err != nil {
		t.Fatal(err)
	}
	if group.(*model.SecurityGroup).Name != defaultsecurity.DefaultSecurityGroupName {
		t.Fatalf("group=%+v", group)
	}
	rules, err := store.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{ProjectID: projectID})
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules=%d error=%v", len(rules), err)
	}
}

func assertDefaultPolicyMissing(t *testing.T, store controlstore.Store, projectID string) {
	t.Helper()
	for _, target := range []struct {
		kind model.Kind
		id   string
	}{
		{model.KindSecurityGroup, defaultsecurity.DefaultSecurityGroupID(projectID)},
		{model.KindSecurityGroupRule, defaultsecurity.DefaultEgressRuleID(projectID)},
		{model.KindSecurityGroupRule, defaultsecurity.DefaultIngressRuleID(projectID)},
	} {
		if _, err := store.Get(context.Background(), target.kind, target.id); !errors.Is(err, controlstore.ErrNotFound) {
			t.Fatalf("%s still exists: %v", target.kind, err)
		}
	}
}
