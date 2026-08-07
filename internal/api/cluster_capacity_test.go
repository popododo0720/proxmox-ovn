package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func TestRequireAllNodesGatesNewPortCapacity(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	store := controlstore.NewMemory(controlstore.WithClock(func() time.Time { return now }))
	server, err := New(Options{
		Store: store, RequireAllNodes: true, NodeHeartbeatTTL: 2 * time.Minute,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	health := request(t, server, http.MethodGet, "/api/v1/health", nil, nil)
	if health.Code != http.StatusOK || !responseHealthStatus(t, health, "degraded") {
		t.Fatalf("initial health=%d body=%s", health.Code, health.Body.String())
	}

	heartbeat := func(name, chassis string) *httptest.ResponseRecorder {
		t.Helper()
		return request(t, server.RuntimeHandler(), http.MethodPost, "/api/v1/runtime/nodes/heartbeat", map[string]any{
			"name": name, "chassis_id": chassis, "online_nodes": []string{"pve-a", "pve-b"}, "quorate": true,
			"roles": []string{"compute", "gateway", "central"},
		}, nil)
	}
	first := heartbeat("pve-a", "chassis-a")
	if first.Code != http.StatusOK {
		t.Fatalf("first heartbeat=%d body=%s", first.Code, first.Body.String())
	}
	health = request(t, server, http.MethodGet, "/api/v1/health", nil, nil)
	if health.Code != http.StatusOK || !responseDetailsContain(t, health, "missing_nodes", "pve-b") {
		t.Fatalf("partial health=%d body=%s", health.Code, health.Body.String())
	}
	second := heartbeat("pve-b", "chassis-b")
	if second.Code != http.StatusOK {
		t.Fatalf("second heartbeat=%d body=%s", second.Code, second.Body.String())
	}
	health = request(t, server, http.MethodGet, "/api/v1/health", nil, nil)
	if health.Code != http.StatusOK {
		t.Fatalf("ready health=%d body=%s", health.Code, health.Body.String())
	}

	networkResource, _, err := store.Create(context.Background(), &model.Network{Name: "private"}, "cluster-gate-network")
	if err != nil {
		t.Fatal(err)
	}
	network := networkResource.(*model.Network)
	created := request(t, server, http.MethodPost, "/api/v1/ports", map[string]any{
		"network_id": network.ID, "name": "healthy", "mac_address": "02:00:00:00:00:01",
	}, map[string]string{"Idempotency-Key": "cluster-gate-port-healthy"})
	if created.Code != http.StatusCreated {
		t.Fatalf("healthy port create=%d body=%s", created.Code, created.Body.String())
	}

	now = now.Add(3 * time.Minute)
	blocked := request(t, server, http.MethodPost, "/api/v1/ports", map[string]any{
		"network_id": network.ID, "name": "blocked", "mac_address": "02:00:00:00:00:02",
	}, map[string]string{"Idempotency-Key": "cluster-gate-port-blocked"})
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale cluster port create=%d body=%s", blocked.Code, blocked.Body.String())
	}
}

func TestRequireAllNodesRejectsLegacyHeartbeatWithoutMembership(t *testing.T) {
	server, err := New(Options{Store: controlstore.NewMemory(), RequireAllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server.RuntimeHandler(), http.MethodPost, "/api/v1/runtime/nodes/heartbeat", map[string]any{
		"name": "pve-a", "chassis_id": "chassis-a",
	}, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("legacy heartbeat=%d body=%s", response.Code, response.Body.String())
	}
}

func responseDetailsContain(t *testing.T, response *httptest.ResponseRecorder, field, value string) bool {
	t.Helper()
	var envelope struct {
		Data struct {
			Capacity map[string]json.RawMessage `json:"capacity"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var values []string
	if err := json.Unmarshal(envelope.Data.Capacity[field], &values); err != nil {
		return false
	}
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func responseHealthStatus(t *testing.T, response *httptest.ResponseRecorder, expected string) bool {
	t.Helper()
	var envelope struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data.Status == expected
}
