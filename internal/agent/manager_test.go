package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHTTPManagerClientResolve(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/runtime/ports/resolve" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if got := request.URL.Query().Get("node"); got != "pve-a" {
			t.Errorf("node = %q", got)
		}
		if got := request.URL.Query().Get("vmid"); got != "100" {
			t.Errorf("vmid = %q", got)
		}
		if got := request.URL.Query().Get("nic"); got != "net2" {
			t.Errorf("nic = %q", got)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"port_id":           "port-uuid",
			"lsp_name":          "pvn-lsp-port-uuid",
			"mac_address":       "02:00:00:00:00:01",
			"generation":        int64(7),
			"requested_chassis": "system-id",
			"status":            "binding",
		})
	}))
	defer server.Close()

	client := managerClientForTLSServer(t, server)
	resolution, err := client.ResolveInterface(context.Background(), InterfaceRef{Node: "pve-a", VMID: 100, NICIndex: 2})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.LSPName != "pvn-lsp-port-uuid" || resolution.Generation != "7" {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func TestHTTPManagerClientLeavesUnknownAndAmbiguousUnresolved(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "unknown", status: http.StatusNotFound, want: ErrNotManaged},
		{name: "ambiguous", status: http.StatusConflict, want: ErrAmbiguous},
		{name: "not-bindable", status: http.StatusConflict, body: `{"error":{"code":"port_not_bindable"}}`, want: ErrNotBindable},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			client := managerClientForTLSServer(t, server)
			_, err := client.ResolveInterface(context.Background(), InterfaceRef{Node: "pve-a", VMID: 100, NICIndex: 0})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestHTTPManagerClientUsesUnixSocket(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "manager.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"port_id": "port-1", "lsp_name": "pvn-lsp-1", "mac_address": "02:00:00:00:00:01",
			"generation": 1, "requested_chassis": "chassis-a", "status": "binding",
		})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	client, err := NewHTTPManagerClient(HTTPManagerClientConfig{BaseURL: "unix://" + socketPath})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ResolveInterface(context.Background(), InterfaceRef{Node: "pve-a", VMID: 100, NICIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	if result.LSPName != "pvn-lsp-1" {
		t.Fatalf("result = %#v", result)
	}
}

func TestHTTPManagerClientAcceptsBoundAndCleanupResolutions(t *testing.T) {
	t.Parallel()

	for _, status := range []string{PortStatusBound, PortStatusDetaching, PortStatusUnbound, PortStatusError} {
		status := status
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"port_id": "port-1", "lsp_name": "pvn-lsp-1", "mac_address": "02:00:00:00:00:01",
					"generation": 8, "requested_chassis": "chassis-a", "status": status,
				})
			}))
			defer server.Close()
			client := managerClientForTLSServer(t, server)
			result, err := client.ResolveInterface(context.Background(), InterfaceRef{Node: "pve-a", VMID: 100, NICIndex: 0})
			if err != nil || result.Status != status || result.Generation != "8" {
				t.Fatalf("resolution=%#v err=%v", result, err)
			}
		})
	}
}

func TestHTTPManagerClientReportsOnlyOverUnixSocket(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "manager.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan map[string]any, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/runtime/ports/port-1/report" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode report: %v", err)
		}
		received <- payload
		_, _ = writer.Write([]byte(`{"data":{"status":"bound"}}`))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	client, err := NewHTTPManagerClient(HTTPManagerClientConfig{BaseURL: "unix://" + socketPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReportPort(context.Background(), PortReport{PortID: "port-1", Generation: "9", Status: PortStatusBound}); err != nil {
		t.Fatal(err)
	}
	payload := <-received
	want := map[string]any{"generation": float64(9), "status": PortStatusBound}
	if !reflect.DeepEqual(payload, want) {
		t.Fatalf("report payload=%#v want=%#v", payload, want)
	}

	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsServer.Close()
	httpsClient := managerClientForTLSServer(t, tlsServer)
	if err := httpsClient.ReportPort(context.Background(), PortReport{PortID: "port-1", Generation: "9", Status: PortStatusBound}); !errors.Is(err, ErrRuntimeUnixOnly) {
		t.Fatalf("HTTPS report error=%v", err)
	}
}

func TestHTTPManagerClientMapsStaleReportGeneration(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "manager.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"error":{"code":"stale_generation"}}`))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client, err := NewHTTPManagerClient(HTTPManagerClientConfig{BaseURL: "unix://" + socketPath})
	if err != nil {
		t.Fatal(err)
	}
	err = client.ReportPort(context.Background(), PortReport{PortID: "port-1", Generation: "4", Status: PortStatusUnbound})
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("report error=%v", err)
	}
}

func TestHTTPManagerClientSendsNodeHeartbeatRolesOnlyWhenExplicit(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "manager.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan map[string]any, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/runtime/nodes/heartbeat" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode heartbeat: %v", err)
		}
		received <- payload
		_, _ = writer.Write([]byte(`{"data":{"name":"pve-a"}}`))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client, err := NewHTTPManagerClient(HTTPManagerClientConfig{BaseURL: "unix://" + socketPath})
	if err != nil {
		t.Fatal(err)
	}

	if err := client.HeartbeatNode(context.Background(), NodeHeartbeat{Name: "pve-a", ChassisID: "chassis-a"}); err != nil {
		t.Fatal(err)
	}
	implicit := <-received
	if _, exists := implicit["roles"]; exists {
		t.Fatalf("implicit heartbeat overwrote roles: %#v", implicit)
	}
	if err := client.HeartbeatNode(context.Background(), NodeHeartbeat{
		Name: "pve-a", ChassisID: "chassis-a", Roles: []string{"compute", "gateway", "central"}, RolesExplicit: true,
	}); err != nil {
		t.Fatal(err)
	}
	explicit := <-received
	if !reflect.DeepEqual(explicit["roles"], []any{"compute", "gateway", "central"}) {
		t.Fatalf("explicit heartbeat roles=%#v", explicit["roles"])
	}
}

func TestHTTPManagerClientRejectsPlainHTTP(t *testing.T) {
	t.Parallel()
	if _, err := NewHTTPManagerClient(HTTPManagerClientConfig{BaseURL: "http://127.0.0.1:8443"}); err == nil {
		t.Fatal("plain HTTP manager URL unexpectedly accepted")
	}
}

func managerClientForTLSServer(t *testing.T, server *httptest.Server) *HTTPManagerClient {
	t.Helper()
	caPath := filepath.Join(t.TempDir(), "manager-ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewHTTPManagerClient(HTTPManagerClientConfig{BaseURL: server.URL, CAFile: caPath})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
