package api

import (
	"net/http"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestConfiguredGuestMTUAndPhysnetAreEnforced(t *testing.T) {
	server, err := New(Options{Store: controlstore.NewMemory(), GuestMTU: 1450, Physnet: "provider"})
	if err != nil {
		t.Fatal(err)
	}
	project := createAPIProject(t, server)
	networkResponse := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{
		"project_id": project.ID, "name": "private",
	}, map[string]string{"Idempotency-Key": "configured-mtu"})
	if networkResponse.Code != http.StatusCreated {
		t.Fatalf("network create=%d body=%s", networkResponse.Code, networkResponse.Body.String())
	}
	network := decodeData[model.Network](t, networkResponse)
	if network.MTU != 1450 {
		t.Fatalf("network MTU=%d", network.MTU)
	}
	tooLarge := request(t, server, http.MethodPost, "/api/v1/networks", map[string]any{
		"project_id": project.ID, "name": "jumbo", "mtu": 1500,
	}, map[string]string{"Idempotency-Key": "oversized-mtu"})
	if tooLarge.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized MTU=%d body=%s", tooLarge.Code, tooLarge.Body.String())
	}

	providerResponse := request(t, server, http.MethodPost, "/api/v1/provider-networks", map[string]any{
		"name": "external",
	}, map[string]string{"Idempotency-Key": "provider-network"})
	if providerResponse.Code != http.StatusCreated {
		t.Fatalf("provider create=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	provider := decodeData[model.ProviderNetwork](t, providerResponse)
	segmentResponse := request(t, server, http.MethodPost, "/api/v1/provider-segments", map[string]any{
		"provider_network_id": provider.ID, "name": "flat", "network_type": "flat",
	}, map[string]string{"Idempotency-Key": "default-physnet"})
	if segmentResponse.Code != http.StatusCreated {
		t.Fatalf("segment create=%d body=%s", segmentResponse.Code, segmentResponse.Body.String())
	}
	segment := decodeData[model.ProviderSegment](t, segmentResponse)
	if segment.PhysicalNetwork != "provider" {
		t.Fatalf("segment physnet=%q", segment.PhysicalNetwork)
	}
	wrongPhysnet := request(t, server, http.MethodPost, "/api/v1/provider-segments", map[string]any{
		"provider_network_id": provider.ID, "name": "wrong", "network_type": "vlan", "vlan_id": 100, "physical_network": "other",
	}, map[string]string{"Idempotency-Key": "wrong-physnet"})
	if wrongPhysnet.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong physnet=%d body=%s", wrongPhysnet.Code, wrongPhysnet.Body.String())
	}
}
