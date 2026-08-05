package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type healthTestStore struct {
	controlstore.Store
	err error
}

func (store healthTestStore) List(ctx context.Context, kind model.Kind, options controlstore.ListOptions) ([]model.Resource, error) {
	if kind == model.KindOperation && store.err != nil {
		return nil, store.err
	}
	return store.Store.List(ctx, kind, options)
}

func TestHealthReportsLiveOperationalComponents(t *testing.T) {
	ready := HealthProbeFunc(func(context.Context) error { return nil })
	server, err := New(Options{
		Store: controlstore.NewMemory(), NorthboundProbe: ready,
		SouthboundProbe: ready, ReconcilerProbe: ready,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodGet, "/api/v1/health", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", response.Code, response.Body.String())
	}
	data := decodeData[struct {
		Status     string `json:"status"`
		Database   string `json:"database"`
		Northbound string `json:"ovn_northbound"`
		Southbound string `json:"ovn_southbound"`
		Reconciler string `json:"reconciler"`
	}](t, response)
	if data.Status != "ok" || data.Database != "ready" || data.Northbound != "ready" || data.Southbound != "ready" || data.Reconciler != "ready" {
		t.Fatalf("health data=%+v", data)
	}
}

func TestHealthDegradesOnlyFailedOrUnavailableComponents(t *testing.T) {
	ready := HealthProbeFunc(func(context.Context) error { return nil })
	failed := HealthProbeFunc(func(context.Context) error { return errors.New("unreachable") })
	server, err := New(Options{
		Store:           healthTestStore{Store: controlstore.NewMemory(), err: errors.New("database unavailable")},
		NorthboundProbe: failed, SouthboundProbe: ready,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodGet, "/api/v1/health", nil, nil)
	data := decodeData[map[string]any](t, response)
	if data["status"] != "degraded" || data["database"] != "degraded" || data["ovn_northbound"] != "degraded" || data["ovn_southbound"] != "ready" || data["reconciler"] != "unavailable" {
		t.Fatalf("health data=%+v", data)
	}
}

func TestHealthProbeTimeoutFailsClosed(t *testing.T) {
	waitForCancellation := HealthProbeFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ready := HealthProbeFunc(func(context.Context) error { return nil })
	server, err := New(Options{
		Store: controlstore.NewMemory(), NorthboundProbe: waitForCancellation,
		SouthboundProbe: ready, ReconcilerProbe: ready, HealthTimeout: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodGet, "/api/v1/health", nil, nil)
	data := decodeData[map[string]any](t, response)
	if data["status"] != "degraded" || data["ovn_northbound"] != "degraded" {
		t.Fatalf("health data=%+v", data)
	}
}
