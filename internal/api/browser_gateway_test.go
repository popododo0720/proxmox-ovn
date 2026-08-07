package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
)

func gatewayHeaders(t *testing.T) map[string]string {
	t.Helper()
	permissions, err := json.Marshal(map[string]map[string]int{
		"/": {"SDN.Audit": 1, "SDN.Allocate": 1, "Sys.Modify": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		PVEAuthIDHeader:      "root@pam",
		PVEPermissionsHeader: base64.RawURLEncoding.EncodeToString(permissions),
	}
}

func TestBrowserGatewayRequiresTrustedPVEIdentity(t *testing.T) {
	server, err := New(Options{Store: controlstore.NewMemory(), ClusterName: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.BrowserHandler()

	missing := request(t, handler, http.MethodGet, "/api/v1/health", nil, nil)
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing identity status=%d body=%s", missing.Code, missing.Body.String())
	}

	headers := gatewayHeaders(t)
	headers[PVEPermissionsHeader] = "not-base64"
	malformed := request(t, handler, http.MethodGet, "/api/v1/health", nil, headers)
	if malformed.Code != http.StatusUnauthorized {
		t.Fatalf("malformed permissions status=%d body=%s", malformed.Code, malformed.Body.String())
	}

	valid := request(t, handler, http.MethodGet, "/api/v1/health", nil, gatewayHeaders(t))
	if valid.Code != http.StatusOK {
		t.Fatalf("valid gateway status=%d body=%s", valid.Code, valid.Body.String())
	}
}

func TestBrowserGatewayExposesOnlyProjectlessBrowserRoutes(t *testing.T) {
	server, err := New(Options{Store: controlstore.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.BrowserHandler()
	headers := gatewayHeaders(t)

	for _, target := range []string{
		"/api/v1/projects",
		"/api/v1/runtime/nodes/heartbeat",
		"/api/v1/runtime/ports/example/report",
		"/api/v1/admin/default-security-group-backfill/plan",
		"/api/v1/ports/not/a/real/action",
	} {
		response := request(t, handler, http.MethodGet, target, nil, headers)
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}

	query := request(t, handler, http.MethodGet, "/api/v1/networks?project_id=legacy", nil, headers)
	if query.Code != http.StatusBadRequest {
		t.Fatalf("project query status=%d body=%s", query.Code, query.Body.String())
	}
	body := request(t, handler, http.MethodPost, "/api/v1/networks", map[string]any{
		"name": "private", "project_id": "legacy",
	}, headers)
	if body.Code != http.StatusBadRequest {
		t.Fatalf("project body status=%d body=%s", body.Code, body.Body.String())
	}
}

func TestBrowserRouteContract(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/api/v1/networks", true},
		{http.MethodPost, "/api/v1/networks", true},
		{http.MethodGet, "/api/v1/networks/id-1", true},
		{http.MethodPut, "/api/v1/networks/id-1", true},
		{http.MethodDelete, "/api/v1/networks/id-1", true},
		{http.MethodPost, "/api/v1/ports/id-1/attach", true},
		{http.MethodPost, "/api/v1/ports/id-1/detach", true},
		{http.MethodDelete, "/api/v1/ports/id-1/deprovision", true},
		{http.MethodPost, "/api/v1/ports/provision", true},
		{http.MethodGet, "/api/v1/runtime/ports/resolve", true},
		{http.MethodPost, "/api/v1/operations", false},
		{http.MethodGet, "/api/v1/projects", false},
		{http.MethodGet, "/api/v1/networks/id/extra", false},
	}
	for _, test := range tests {
		if got := browserRouteAllowed(test.method, test.path); got != test.want {
			t.Errorf("browserRouteAllowed(%s, %s)=%v want=%v", test.method, test.path, got, test.want)
		}
	}
}
