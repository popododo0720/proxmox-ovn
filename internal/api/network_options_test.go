package api

import (
	"net/http"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestNetworkDNSAndStaticRoutesRoundTripThroughAPI(t *testing.T) {
	store := controlstore.NewMemory()
	server := testServer(t, store, nil)
	network := createAPIResource(t, store, &model.Network{Name: "api-route-network"}, "api-route-network").(*model.Network)

	subnetResponse := request(t, server, http.MethodPost, "/api/v1/subnets", map[string]any{
		"network_id":         network.ID,
		"name":               "api-route-subnet",
		"cidr":               "10.42.0.0/24",
		"gateway_ip":         "10.42.0.1",
		"enable_dhcp":        true,
		"dns_nameservers":    []string{"1.1.1.1", "8.8.8.8"},
		"dns_domain":         "Guest.Example.",
		"dns_search_domains": []string{"Guest.Example.", "Svc.Example"},
	}, map[string]string{"Idempotency-Key": "api-route-subnet"})
	if subnetResponse.Code != http.StatusCreated {
		t.Fatalf("create subnet status=%d body=%s", subnetResponse.Code, subnetResponse.Body.String())
	}
	subnet := decodeData[model.Subnet](t, subnetResponse)
	if subnet.DNSDomain != "guest.example" || len(subnet.DNSNameservers) != 2 || len(subnet.DNSSearchDomains) != 2 || subnet.DNSSearchDomains[1] != "svc.example" {
		t.Fatalf("subnet DNS response=%#v", subnet)
	}

	router := createAPIResource(t, store, &model.Router{Name: "api-route-router"}, "api-route-router").(*model.Router)
	createAPIResource(t, store, &model.RouterInterface{RouterID: router.ID, SubnetID: subnet.ID}, "api-route-interface")
	router.StaticRoutes = []model.StaticRoute{{Destination: "10.60.0.0/16", NextHop: "10.42.0.2"}}
	routerResponse := request(t, server, http.MethodPut, "/api/v1/routers/"+router.ID, router, map[string]string{
		"Idempotency-Key": "api-route-update", "If-Match": `"1"`,
	})
	if routerResponse.Code != http.StatusOK {
		t.Fatalf("update router status=%d body=%s", routerResponse.Code, routerResponse.Body.String())
	}
	updated := decodeData[model.Router](t, routerResponse)
	if len(updated.StaticRoutes) != 1 || updated.StaticRoutes[0].Destination != "10.60.0.0/16" || updated.StaticRoutes[0].NextHop != "10.42.0.2" {
		t.Fatalf("router static route response=%#v", updated.StaticRoutes)
	}

	getResponse := request(t, server, http.MethodGet, "/api/v1/routers/"+router.ID, nil, nil)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get router status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	loaded := decodeData[model.Router](t, getResponse)
	if len(loaded.StaticRoutes) != 1 || loaded.StaticRoutes[0] != updated.StaticRoutes[0] {
		t.Fatalf("loaded router static routes=%#v", loaded.StaticRoutes)
	}
}
