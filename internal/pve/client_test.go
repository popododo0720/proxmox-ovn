package pve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientChecksExactPoolExistence(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api2/json/pools" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if cookie, err := request.Cookie("PVEAuthCookie"); err != nil || cookie.Value != "ticket-value" {
			t.Errorf("ticket cookie = %#v, %v", cookie, err)
		}
		switch request.URL.Query().Get("poolid") {
		case "parent/tenant":
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": []map[string]any{{"poolid": "parent/tenant"}}})
		case "missing":
			writer.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": nil, "message": "pool 'missing' does not exist\n"})
		case "broken":
			writer.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": nil, "message": "database unavailable"})
		default:
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": []map[string]any{{"poolid": "different"}}})
		}
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{BaseURL: server.URL, Auth: TicketAuth{Ticket: "ticket-value"}})
	if err != nil {
		t.Fatal(err)
	}
	exists, err := client.PoolExists(context.Background(), "parent/tenant")
	if err != nil || !exists {
		t.Fatalf("existing pool: exists=%v err=%v", exists, err)
	}
	exists, err = client.PoolExists(context.Background(), "missing")
	if err != nil || exists {
		t.Fatalf("missing pool: exists=%v err=%v", exists, err)
	}
	exists, err = client.PoolExists(context.Background(), "broken")
	if err == nil || exists {
		t.Fatalf("failed lookup: exists=%v err=%v", exists, err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError || apiErr.Message != "database unavailable" {
		t.Fatalf("unexpected API error: %#v", err)
	}
	if _, err := client.PoolExists(context.Background(), "mismatched"); err == nil {
		t.Fatal("mismatched pool response unexpectedly succeeded")
	}
}

func TestClientRejectsInvalidPoolIDBeforeRequest(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PoolExists(context.Background(), "bad\npool"); err == nil {
		t.Fatal("control character in pool ID was accepted")
	}
	if requests.Load() != 0 {
		t.Fatalf("requests = %d, want 0", requests.Load())
	}
}

func TestClientReadsAndUpdatesVMNetworkWithTicket(t *testing.T) {
	t.Parallel()

	var updateForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if cookie, err := request.Cookie("PVEAuthCookie"); err != nil || cookie.Value != "ticket-value" {
			t.Errorf("ticket cookie = %#v, %v", cookie, err)
		}
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
				"digest": "digest-1",
				"name":   "example",
				"net0":   "virtio=02:00:00:00:00:01,bridge=br-int,unknown=kept",
			}})
		case http.MethodPut:
			if got := request.Header.Get("CSRFPreventionToken"); got != "csrf-value" {
				t.Errorf("CSRF header = %q", got)
			}
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			updateForm = request.PostForm
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": "UPID:node:1"})
		}
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{
		BaseURL: server.URL,
		Auth: TicketAuth{
			Ticket:    "ticket-value",
			CSRFToken: "csrf-value",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := client.GetVMConfig(context.Background(), "pve-a", 100)
	if err != nil {
		t.Fatal(err)
	}
	if config.Digest != "digest-1" || config.Networks[0].String() != "virtio=02:00:00:00:00:01,bridge=br-int,unknown=kept" {
		t.Fatalf("unexpected config: %#v", config)
	}
	property := config.Networks[0].Clone()
	if err := property.SetLinkDown(true); err != nil {
		t.Fatal(err)
	}
	upid, err := client.SetVMNetwork(context.Background(), "pve-a", 100, 0, property, config.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if upid != "UPID:node:1" {
		t.Fatalf("UPID = %q", upid)
	}
	if got := updateForm.Get("digest"); got != "digest-1" {
		t.Fatalf("digest = %q", got)
	}
	if got := updateForm.Get("net0"); !strings.Contains(got, "unknown=kept") || !strings.Contains(got, "link_down=1") {
		t.Fatalf("net0 = %q", got)
	}
}

func TestClientAPITokenAndUPIDPolling(t *testing.T) {
	t.Parallel()

	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "PVEAPIToken=root@pam!pvn=secret" {
			t.Errorf("Authorization = %q", got)
		}
		if polls.Add(1) == 1 {
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": TaskStatus{Status: "running"}})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": TaskStatus{Status: "stopped", ExitStatus: "OK"}})
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{
		BaseURL:          server.URL,
		Auth:             APITokenAuth{TokenID: "root@pam!pvn", Secret: "secret"},
		TaskPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WaitUPID(context.Background(), "pve-a", "UPID:pve-a:1"); err != nil {
		t.Fatal(err)
	}
	if got := polls.Load(); got != 2 {
		t.Fatalf("poll count = %d, want 2", got)
	}
}

func TestDigestIsRequiredForNetworkMutation(t *testing.T) {
	t.Parallel()

	client, err := NewClient(ClientConfig{BaseURL: "https://pve.invalid:8006"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DeleteVMNetwork(context.Background(), "pve-a", 100, 0, "")
	if err == nil {
		t.Fatal("DeleteVMNetwork without digest unexpectedly succeeded")
	}
}
