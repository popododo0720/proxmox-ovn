package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type managedPolicyAPIResource struct {
	ID       string `json:"id"`
	Managed  bool   `json:"managed"`
	ReadOnly bool   `json:"read_only"`
}

func TestSecurityPolicyResponsesExposeComputedManagementMetadata(t *testing.T) {
	store := controlstore.NewMemory()
	project := createAPIResource(t, store, &model.Project{Name: "tenant", PoolID: "pool-tenant"}, "managed-project").(*model.Project)
	if _, err := defaultsecurity.New(store, nil).Ensure(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	tenantGroup := createAPIResource(t, store, &model.SecurityGroup{
		ProjectID: project.ID, Name: "web", Stateful: true,
	}, "tenant-group").(*model.SecurityGroup)
	tenantRule := createAPIResource(t, store, &model.SecurityGroupRule{
		ProjectID: project.ID, SecurityGroupID: defaultsecurity.DefaultSecurityGroupID(project.ID),
		Direction: model.DirectionIngress, EtherType: model.EtherTypeIPv4, Action: model.ActionAllow,
		Protocol: "tcp", PortRangeMin: 443, PortRangeMax: 443,
	}, "tenant-rule").(*model.SecurityGroupRule)
	server := testServer(t, store, nil)

	groupsResponse := request(t, server, http.MethodGet, "/api/v1/security-groups", nil, nil)
	groups := decodeData[[]managedPolicyAPIResource](t, groupsResponse)
	assertManagedPolicyMetadata(t, groups, defaultsecurity.DefaultSecurityGroupID(project.ID), true)
	assertManagedPolicyMetadata(t, groups, tenantGroup.ID, false)

	rulesResponse := request(t, server, http.MethodGet, "/api/v1/security-group-rules", nil, nil)
	rules := decodeData[[]managedPolicyAPIResource](t, rulesResponse)
	assertManagedPolicyMetadata(t, rules, defaultsecurity.DefaultEgressRuleID(project.ID), true)
	assertManagedPolicyMetadata(t, rules, defaultsecurity.DefaultIngressRuleID(project.ID), true)
	assertManagedPolicyMetadata(t, rules, tenantRule.ID, false)

	getResponse := request(t, server, http.MethodGet, "/api/v1/security-groups/"+defaultsecurity.DefaultSecurityGroupID(project.ID), nil, nil)
	managed := decodeData[managedPolicyAPIResource](t, getResponse)
	if !managed.Managed || !managed.ReadOnly {
		t.Fatalf("default group metadata=%+v", managed)
	}
}

func assertManagedPolicyMetadata(t *testing.T, resources []managedPolicyAPIResource, id string, want bool) {
	t.Helper()
	for _, resource := range resources {
		if resource.ID != id {
			continue
		}
		if resource.Managed != want || resource.ReadOnly != want {
			t.Fatalf("resource %s managed=%v read_only=%v want %v", id, resource.Managed, resource.ReadOnly, want)
		}
		return
	}
	t.Fatalf("resource %s was not returned", id)
}
