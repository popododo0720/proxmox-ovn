package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const maxHeartbeatWriteAttempts = 8

type nodeHeartbeatRequest struct {
	Name        string            `json:"name"`
	ChassisID   string            `json:"chassis_id"`
	Roles       *[]model.NodeRole `json:"roles,omitempty"`
	OnlineNodes *[]string         `json:"online_nodes,omitempty"`
	Quorate     *bool             `json:"quorate,omitempty"`
}

func (s *Server) heartbeatNode(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	var heartbeat nodeHeartbeatRequest
	if !decodeActionBody(writer, request, &heartbeat) {
		return
	}
	if s.clusterGate.required && (heartbeat.OnlineNodes == nil || heartbeat.Quorate == nil) {
		writeError(writer, http.StatusBadRequest, "membership_required", "online_nodes and quorate are required when cluster.require_all_nodes is enabled", nil)
		return
	}
	explicitRoles, err := canonicalHeartbeatRoles(heartbeat.Roles)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	observedAt := s.clusterGate.now().UTC()
	for attempt := 0; attempt < maxHeartbeatWriteAttempts; attempt++ {
		current, err := s.findHeartbeatNode(request.Context(), heartbeat.Name, heartbeat.ChassisID)
		if err != nil {
			s.storeError(writer, err)
			return
		}
		if current == nil {
			roles := explicitRoles
			if heartbeat.Roles == nil {
				roles = []model.NodeRole{model.NodeRoleCompute}
			}
			candidate := &model.Node{
				Name: heartbeat.Name, ChassisID: heartbeat.ChassisID, Roles: roles,
				Enabled: true, LastSeenAt: &observedAt,
			}
			if err := candidate.Validate(); err != nil {
				s.storeError(writer, err)
				return
			}
			created, replayed, err := s.store.Create(request.Context(), candidate, heartbeatCreateKey(heartbeat.Name, heartbeat.ChassisID, observedAt))
			if errors.Is(err, controlstore.ErrAlreadyExists) || errors.Is(err, controlstore.ErrPrecondition) {
				continue
			}
			if err != nil {
				s.storeError(writer, err)
				return
			}
			ready := s.markHeartbeatNodeReady(request.Context(), created.(*model.Node))
			if !s.recordHeartbeatMembership(writer, heartbeat, observedAt) {
				return
			}
			setETag(writer, ready.Revision)
			if replayed {
				writer.Header().Set("Idempotency-Replayed", "true")
			}
			writeJSON(writer, http.StatusOK, map[string]any{"data": ready})
			return
		}

		candidateResource, err := model.Clone(current)
		if err != nil {
			s.storeError(writer, err)
			return
		}
		candidate := candidateResource.(*model.Node)
		candidate.LastSeenAt = &observedAt
		if heartbeat.Roles != nil {
			candidate.Roles = explicitRoles
		}
		updated, _, err := s.store.Update(request.Context(), candidate, current.Revision, "")
		if errors.Is(err, controlstore.ErrPrecondition) {
			continue
		}
		if err != nil {
			s.storeError(writer, err)
			return
		}
		ready := s.markHeartbeatNodeReady(request.Context(), updated.(*model.Node))
		if !s.recordHeartbeatMembership(writer, heartbeat, observedAt) {
			return
		}
		setETag(writer, ready.Revision)
		writeJSON(writer, http.StatusOK, map[string]any{"data": ready})
		return
	}
	writeError(writer, http.StatusConflict, "heartbeat_conflict", "node heartbeat could not be serialized after concurrent updates", nil)
}

func (s *Server) recordHeartbeatMembership(writer http.ResponseWriter, heartbeat nodeHeartbeatRequest, observedAt time.Time) bool {
	if heartbeat.OnlineNodes == nil || heartbeat.Quorate == nil {
		return true
	}
	if err := s.clusterGate.report(heartbeat.Name, *heartbeat.OnlineNodes, *heartbeat.Quorate, observedAt); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_membership", err.Error(), nil)
		return false
	}
	return true
}

func (s *Server) findHeartbeatNode(ctx context.Context, name, chassisID string) (*model.Node, error) {
	probe := &model.Node{Name: name, ChassisID: chassisID, Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}
	if err := probe.Validate(); err != nil {
		return nil, err
	}
	resources, err := s.store.List(ctx, model.KindNode, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	var byName, byChassis *model.Node
	for _, resource := range resources {
		node := resource.(*model.Node)
		if node.Name == name {
			byName = node
		}
		if node.ChassisID == chassisID {
			byChassis = node
		}
	}
	if byName == nil && byChassis == nil {
		return nil, nil
	}
	if byName == nil || byChassis == nil || byName.ID != byChassis.ID {
		return nil, &controlstore.Error{Kind: controlstore.ErrConflict, Message: "node name or chassis ID is already registered to another identity"}
	}
	return byName, nil
}

func canonicalHeartbeatRoles(input *[]model.NodeRole) ([]model.NodeRole, error) {
	if input == nil {
		return nil, nil
	}
	if len(*input) == 0 {
		return nil, &model.ValidationError{Field: "roles", Message: "must contain at least one role when supplied"}
	}
	seen := make(map[model.NodeRole]bool, len(*input))
	for _, role := range *input {
		if role != model.NodeRoleCompute && role != model.NodeRoleGateway && role != model.NodeRoleCentral {
			return nil, &model.ValidationError{Field: "roles", Message: "contains an unknown role " + string(role)}
		}
		if seen[role] {
			return nil, &model.ValidationError{Field: "roles", Message: "contains a duplicate role " + string(role)}
		}
		seen[role] = true
	}
	ordered := make([]model.NodeRole, 0, len(seen))
	for _, role := range []model.NodeRole{model.NodeRoleCompute, model.NodeRoleGateway, model.NodeRoleCentral} {
		if seen[role] {
			ordered = append(ordered, role)
		}
	}
	return ordered, nil
}

func heartbeatCreateKey(name, chassisID string, observedAt time.Time) string {
	digest := sha256.Sum256([]byte(name + "\x00" + chassisID + "\x00" + observedAt.Format(time.RFC3339Nano)))
	return "runtime-node-heartbeat-" + hex.EncodeToString(digest[:])
}

func (s *Server) markHeartbeatNodeReady(ctx context.Context, node *model.Node) *model.Node {
	ready, err := s.store.MarkReconciled(ctx, model.KindNode, node.ID, node.Revision, nil)
	if err != nil {
		s.logger.Error("mark heartbeat node ready", "node", node.ID, "revision", node.Revision, "error", err)
		return node
	}
	return ready.(*model.Node)
}
