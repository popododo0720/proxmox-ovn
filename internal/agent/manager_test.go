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
