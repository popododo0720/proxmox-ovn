package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type clusterCapacityGate struct {
	required bool
	ttl      time.Duration
	clock    func() time.Time

	mu       sync.RWMutex
	reported time.Time
	reporter string
	quorate  bool
	online   []string
}

type clusterCapacityStatus struct {
	Ready       bool     `json:"ready"`
	Reason      string   `json:"reason,omitempty"`
	Reporter    string   `json:"reporter,omitempty"`
	OnlineNodes []string `json:"online_nodes,omitempty"`
	Missing     []string `json:"missing_nodes,omitempty"`
	Stale       []string `json:"stale_nodes,omitempty"`
}

func (status clusterCapacityStatus) Label() string {
	if status.Ready {
		return "ok"
	}
	return "degraded"
}

func newClusterCapacityGate(required bool, ttl time.Duration, clock func() time.Time) *clusterCapacityGate {
	return &clusterCapacityGate{required: required, ttl: ttl, clock: clock}
}

func (gate *clusterCapacityGate) now() time.Time {
	if gate == nil || gate.clock == nil {
		return time.Now()
	}
	return gate.clock()
}

func (gate *clusterCapacityGate) report(reporter string, online []string, quorate bool, observedAt time.Time) error {
	if gate == nil || !gate.required {
		return nil
	}
	reporter = strings.TrimSpace(reporter)
	if reporter == "" {
		return fmt.Errorf("membership reporter is required")
	}
	canonical := make([]string, 0, len(online))
	seen := make(map[string]bool, len(online))
	reporterOnline := false
	for _, rawName := range online {
		name := strings.TrimSpace(rawName)
		probe := &model.Node{Name: name, ChassisID: "membership", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}
		if name == "" || probe.Validate() != nil {
			return fmt.Errorf("online_nodes contains an invalid PVE node name %q", rawName)
		}
		if seen[name] {
			return fmt.Errorf("online_nodes contains duplicate PVE node %q", name)
		}
		seen[name] = true
		reporterOnline = reporterOnline || name == reporter
		canonical = append(canonical, name)
	}
	if len(canonical) == 0 {
		return fmt.Errorf("online_nodes must contain at least one PVE node")
	}
	if !reporterOnline {
		return fmt.Errorf("membership reporter %q is not listed as online", reporter)
	}
	sort.Strings(canonical)
	gate.mu.Lock()
	gate.reported = observedAt.UTC()
	gate.reporter = reporter
	gate.quorate = quorate
	gate.online = canonical
	gate.mu.Unlock()
	return nil
}

func (gate *clusterCapacityGate) status(ctx context.Context, store controlstore.Store) clusterCapacityStatus {
	if gate == nil || !gate.required {
		return clusterCapacityStatus{Ready: true}
	}
	gate.mu.RLock()
	reported, reporter, quorate := gate.reported, gate.reporter, gate.quorate
	online := append([]string(nil), gate.online...)
	gate.mu.RUnlock()
	now := gate.now().UTC()
	status := clusterCapacityStatus{Reporter: reporter, OnlineNodes: online}
	if reported.IsZero() || now.Sub(reported) > gate.ttl {
		status.Reason = "fresh PVE membership report is unavailable"
		return status
	}
	if !quorate {
		status.Reason = "PVE cluster is not quorate"
		return status
	}
	resources, err := store.List(ctx, model.KindNode, controlstore.ListOptions{})
	if err != nil {
		status.Reason = "PVN node registry is unavailable"
		return status
	}
	registered := make(map[string]*model.Node, len(resources))
	for _, resource := range resources {
		node := resource.(*model.Node)
		registered[node.Name] = node
	}
	for _, name := range online {
		node := registered[name]
		if node == nil || !node.Enabled || node.State != model.ResourceReady || node.LastSeenAt == nil {
			status.Missing = append(status.Missing, name)
			continue
		}
		if now.Sub(node.LastSeenAt.UTC()) > gate.ttl {
			status.Stale = append(status.Stale, name)
		}
	}
	if len(status.Missing) != 0 || len(status.Stale) != 0 {
		status.Reason = "PVN is not ready on every online PVE node"
		return status
	}
	status.Ready = true
	return status
}

func (s *Server) requireClusterCapacity(writer http.ResponseWriter, request *http.Request) bool {
	status := s.clusterGate.status(request.Context(), s.store)
	if status.Ready {
		return true
	}
	writeError(writer, http.StatusServiceUnavailable, "cluster_incomplete", status.Reason, status)
	return false
}
