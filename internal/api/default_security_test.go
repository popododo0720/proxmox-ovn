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

func TestGenericPortCreateUsesClusterGlobalDefaultAfterAuthorization(t *testing.T) {
	store := controlstore.NewMemory()
	network := createAPIResource(t, store, &model.Network{Name: "private"}, "network").(*model.Network)
	permissions := map[string]any{}
	server := testServer(t, store, SessionProviderFunc(func(context.Context, *http.Request) (Session, error) {
		return Session{User: "tenant@pve", Permissions: permissions}, nil
	}))
	body := map[string]any{
		"network_id": network.ID, "name": "vm100-net0",
		"mac_address": "02:00:00:00:00:10", "admin_state_up": true,
	}
	denied := request(t, server, http.MethodPost, "/api/v1/ports", body, map[string]string{"Idempotency-Key": "denied"})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("denied status=%d body=%s", denied.Code, denied.Body.String())
	}
	groups, _ := store.List(context.Background(), model.KindSecurityGroup, controlstore.ListOptions{})
	if len(groups) != 0 {
		t.Fatalf("authorization failure created %d security groups", len(groups))
	}

	permissions["/"] = map[string]bool{"SDN.Allocate": true}
	createdResponse := request(t, server, http.MethodPost, "/api/v1/ports", body, map[string]string{"Idempotency-Key": "allowed"})
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeData[model.Port](t, createdResponse)
	wantGroup := defaultsecurity.DefaultSecurityGroupID()
	if fmt.Sprint(created.SecurityGroupIDs) != fmt.Sprint([]string{wantGroup}) {
		t.Fatalf("port security groups=%v want [%s]", created.SecurityGroupIDs, wantGroup)
	}
	assertStoredDefaultPolicy(t, store)
}

func TestAllAPIPortsUsingDefaultSGShareOneRoutedSelfIngressTrustDomain(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	firstNetwork := createAPIResource(t, store, &model.Network{Name: "first"}, "first-network").(*model.Network)
	secondNetwork := createAPIResource(t, store, &model.Network{Name: "second"}, "second-network").(*model.Network)

	createPort := func(key, networkID, name, mac string) model.Port {
		t.Helper()
		response := request(t, server, http.MethodPost, "/api/v1/ports", map[string]any{
			"network_id": networkID, "name": name, "mac_address": mac,
		}, map[string]string{"Idempotency-Key": key})
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s status=%d body=%s", name, response.Code, response.Body.String())
		}
		return decodeData[model.Port](t, response)
	}
	first := createPort("first-port", firstNetwork.ID, "first-port", "02:00:00:00:00:21")
	second := createPort("second-port", secondNetwork.ID, "second-port", "02:00:00:00:00:22")
	want := []string{defaultsecurity.DefaultSecurityGroupID()}
	if fmt.Sprint(first.SecurityGroupIDs) != fmt.Sprint(want) || fmt.Sprint(second.SecurityGroupIDs) != fmt.Sprint(want) {
		t.Fatalf("ports do not share the cluster-global default group: first=%v second=%v", first.SecurityGroupIDs, second.SecurityGroupIDs)
	}

	// This single self-referencing ingress rule deliberately makes every port
	// using the default SG part of one routed self-ingress trust domain.
	resource, err := store.Get(context.Background(), model.KindSecurityGroupRule, defaultsecurity.DefaultIngressRuleID())
	if err != nil {
		t.Fatal(err)
	}
	rule := resource.(*model.SecurityGroupRule)
	if rule.SecurityGroupID != want[0] || rule.RemoteGroupID != want[0] {
		t.Fatalf("default self-ingress rule=%+v", rule)
	}
}

func TestGenericPortCreateFailsClosedWhenDefaultCannotReconcile(t *testing.T) {
	store := controlstore.NewMemory()
	network := createAPIResource(t, store, &model.Network{Name: "private"}, "network").(*model.Network)
	want := errors.New("OVN unavailable")
	server, err := New(Options{Store: store, Reconciler: reconcileFunc(func(context.Context, model.Kind, string) error { return want })})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodPost, "/api/v1/ports", map[string]any{
		"network_id": network.ID, "name": "blocked",
		"mac_address": "02:00:00:00:00:11", "admin_state_up": true,
	}, map[string]string{"Idempotency-Key": "blocked"})
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "default_security_policy_unavailable") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	ports, _ := store.List(context.Background(), model.KindPort, controlstore.ListOptions{})
	if len(ports) != 0 {
		t.Fatalf("failed default reconciliation created %d ports", len(ports))
	}
	if _, err := store.Get(context.Background(), model.KindSecurityGroupRule, defaultsecurity.DefaultEgressRuleID()); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("allow rule exists despite default-drop realization failure: %v", err)
	}
}

func TestPortUpdateKeepsClusterGlobalDefaultWhenPolicyIsOmitted(t *testing.T) {
	store := controlstore.NewMemory()
	network := createAPIResource(t, store, &model.Network{Name: "private"}, "network").(*model.Network)
	server := testServer(t, store, nil)
	createdResponse := request(t, server, http.MethodPost, "/api/v1/ports", map[string]any{
		"network_id": network.ID, "name": "secured", "mac_address": "02:00:00:00:00:13",
	}, map[string]string{"Idempotency-Key": "secured-create"})
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	secured := decodeData[model.Port](t, createdResponse)
	secured.Name = "still-secured"
	secured.SecurityGroupIDs = nil
	updatedResponse := request(t, server, http.MethodPut, "/api/v1/ports/"+secured.ID, secured, map[string]string{
		"Idempotency-Key": "secured-update", "If-Match": `"1"`,
	})
	if updatedResponse.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updatedResponse.Code, updatedResponse.Body.String())
	}
	updated := decodeData[model.Port](t, updatedResponse)
	if fmt.Sprint(updated.SecurityGroupIDs) != fmt.Sprint([]string{defaultsecurity.DefaultSecurityGroupID()}) {
		t.Fatalf("updated security groups=%v", updated.SecurityGroupIDs)
	}
	replayed := request(t, server, http.MethodPut, "/api/v1/ports/"+secured.ID, secured, map[string]string{
		"Idempotency-Key": "secured-update", "If-Match": `"1"`,
	})
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replayed.Code, replayed.Header(), replayed.Body.String())
	}
}

func TestReservedDefaultPolicyRejectsGenericMutation(t *testing.T) {
	store := controlstore.NewMemory()
	group, err := defaultsecurity.New(store, nil).Ensure(context.Background())
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

	ruleResource, err := store.Get(context.Background(), model.KindSecurityGroupRule, defaultsecurity.DefaultEgressRuleID())
	if err != nil {
		t.Fatal(err)
	}
	rule := ruleResource.(*model.SecurityGroupRule)
	rule.Description = "weakened"
	updateRule := request(t, server, http.MethodPut, "/api/v1/security-group-rules/"+rule.ID, rule, map[string]string{
		"Idempotency-Key": "update-default-rule", "If-Match": `"1"`,
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

func TestDefaultPolicyAndPortAreReadyWithController(t *testing.T) {
	store := controlstore.NewMemory()
	controller := reconcile.NewController(store, reconcile.NewFakeRenderer())
	server, err := New(Options{Store: store, Reconciler: controller})
	if err != nil {
		t.Fatal(err)
	}
	networkResponse := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{
		"name": "private",
	}, map[string]string{"Idempotency-Key": "ready-network"})
	if networkResponse.Code != http.StatusCreated {
		t.Fatalf("network status=%d body=%s", networkResponse.Code, networkResponse.Body.String())
	}
	network := decodeData[model.Network](t, networkResponse)
	portResponse := request(t, server, http.MethodPost, "/api/v1/ports", map[string]any{
		"network_id": network.ID, "name": "ready-port",
		"mac_address": "02:00:00:00:00:14", "admin_state_up": true,
	}, map[string]string{"Idempotency-Key": "ready-port"})
	if portResponse.Code != http.StatusCreated {
		t.Fatalf("port status=%d body=%s", portResponse.Code, portResponse.Body.String())
	}
	port := decodeData[model.Port](t, portResponse)
	if port.State != model.ResourceReady || fmt.Sprint(port.SecurityGroupIDs) != fmt.Sprint([]string{defaultsecurity.DefaultSecurityGroupID()}) {
		t.Fatalf("ready port=%+v", port)
	}
	for _, target := range []struct {
		kind model.Kind
		id   string
	}{
		{model.KindSecurityGroup, defaultsecurity.DefaultSecurityGroupID()},
		{model.KindSecurityGroupRule, defaultsecurity.DefaultEgressRuleID()},
		{model.KindSecurityGroupRule, defaultsecurity.DefaultIngressRuleID()},
	} {
		resource, err := store.Get(context.Background(), target.kind, target.id)
		if err != nil {
			t.Fatal(err)
		}
		if resource.GetMetadata().State != model.ResourceReady || resource.GetMetadata().AppliedRevision != resource.GetMetadata().Revision {
			t.Fatalf("%s was not ready: %+v", target.kind, resource.GetMetadata())
		}
	}
}

type reconcileFunc func(context.Context, model.Kind, string) error

func (function reconcileFunc) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	return function(ctx, kind, id)
}

func assertStoredDefaultPolicy(t *testing.T, store controlstore.Store) {
	t.Helper()
	group, err := store.Get(context.Background(), model.KindSecurityGroup, defaultsecurity.DefaultSecurityGroupID())
	if err != nil {
		t.Fatal(err)
	}
	if group.(*model.SecurityGroup).Name != defaultsecurity.DefaultSecurityGroupName {
		t.Fatalf("group=%+v", group)
	}
	rules, err := store.List(context.Background(), model.KindSecurityGroupRule, controlstore.ListOptions{})
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules=%d error=%v", len(rules), err)
	}
}
