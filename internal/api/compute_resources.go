package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	computeClonePreparePath    = "/api/v1/runtime/compute/clone/prepare"
	computeCloneCommitPath     = "/api/v1/runtime/compute/clone/commit"
	computeCloneAbortPath      = "/api/v1/runtime/compute/clone/abort"
	computeTemplatePreparePath = "/api/v1/runtime/compute/template/prepare"
	computeTemplateCommitPath  = "/api/v1/runtime/compute/template/commit"
	computeTemplateAbortPath   = "/api/v1/runtime/compute/template/abort"
	computeSnapshotCreatePath  = "/api/v1/runtime/compute/snapshot/create"
	computeSnapshotPreparePath = "/api/v1/runtime/compute/snapshot/prepare"
	computeSnapshotCommitPath  = "/api/v1/runtime/compute/snapshot/commit"
	computeSnapshotAbortPath   = "/api/v1/runtime/compute/snapshot/abort"
	computeSnapshotCleanupPath = "/api/v1/runtime/compute/snapshot/cleanup"
	computeDestroyCapturePath  = "/api/v1/runtime/compute/destroy/capture"
	computeDestroyCommitPath   = "/api/v1/runtime/compute/destroy/commit"
	computeDestroyAbortPath    = "/api/v1/runtime/compute/destroy/abort"

	computeCloneAction            = "compute-clone"
	computeTemplateAction         = "compute-template"
	computeSnapshotAction         = "compute-snapshot"
	computeSnapshotMutationAction = "compute-snapshot-mutation"
	computeDestroyAction          = "compute-destroy"
)

type computePortBlueprint struct {
	NIC              string   `json:"nic"`
	SourceMACAddress string   `json:"source_mac_address"`
	NetworkID        string   `json:"network_id"`
	SubnetID         string   `json:"subnet_id,omitempty"`
	SecurityGroupIDs []string `json:"security_group_ids"`
}

type computeOwnedPort struct {
	PortID             string          `json:"port_id"`
	NIC                string          `json:"nic"`
	MACAddress         string          `json:"mac_address"`
	FixedIPs           []model.FixedIP `json:"fixed_ips,omitempty"`
	NetworkID          string          `json:"network_id"`
	SecurityGroupIDs   []string        `json:"security_group_ids"`
	LSPName            string          `json:"lsp_name"`
	Revision           int64           `json:"revision"`
	Generation         int64           `json:"generation"`
	DetachedRevision   int64           `json:"detached_revision"`
	DetachedGeneration int64           `json:"detached_generation"`
	AllocationID       string          `json:"allocation_id,omitempty"`
	AllocationRevision int64           `json:"allocation_revision,omitempty"`
	OwnershipDigest    string          `json:"ownership_digest"`
}

type computeSourcePort struct {
	PortID              string                  `json:"port_id"`
	NIC                 string                  `json:"nic"`
	MACAddress          string                  `json:"mac_address"`
	FixedIPs            []model.FixedIP         `json:"fixed_ips,omitempty"`
	NetworkID           string                  `json:"network_id"`
	SecurityGroupIDs    []string                `json:"security_group_ids"`
	AllocationIDs       []string                `json:"allocation_ids,omitempty"`
	AllocationRevisions []int64                 `json:"allocation_revisions,omitempty"`
	LSPName             string                  `json:"lsp_name"`
	SourceNodeID        string                  `json:"source_node_id"`
	SourceChassis       string                  `json:"source_chassis"`
	SourceStatus        model.PortBindingStatus `json:"source_status"`
	SourceRevision      int64                   `json:"source_revision"`
	SourceGeneration    int64                   `json:"source_generation"`
	DetachedRevision    int64                   `json:"detached_revision"`
	DetachedGeneration  int64                   `json:"detached_generation"`
	RestoredRevision    int64                   `json:"restored_revision"`
	RestoredGeneration  int64                   `json:"restored_generation"`
}

type computeTemplatePrepareRequest struct {
	LifecycleID string       `json:"lifecycle_id"`
	VMID        int          `json:"vmid"`
	NICs        []computeNIC `json:"nics"`
}

type computeTemplatePayload struct {
	Version     int                    `json:"version"`
	LifecycleID string                 `json:"lifecycle_id"`
	Phase       string                 `json:"phase"`
	VMID        int                    `json:"vmid"`
	NodeID      string                 `json:"node_id"`
	Node        string                 `json:"node"`
	Chassis     string                 `json:"chassis"`
	Blueprints  []computePortBlueprint `json:"blueprints"`
	Ports       []computeSourcePort    `json:"ports"`
}

type computeTemplateTransaction struct {
	LifecycleID string              `json:"lifecycle_id"`
	VMID        int                 `json:"vmid"`
	Node        string              `json:"node"`
	OperationID string              `json:"operation_id"`
	PayloadHash string              `json:"payload_hash"`
	Ports       []computeSourcePort `json:"ports"`
}

type computeSnapshotCreateRequest struct {
	LifecycleID   string       `json:"lifecycle_id"`
	VMID          int          `json:"vmid"`
	SnapshotID    string       `json:"snapshot_id"`
	SnapshotEpoch int64        `json:"snapshot_epoch"`
	NICs          []computeNIC `json:"nics"`
}

type computeSnapshotLookupRequest struct {
	VMID          int          `json:"vmid"`
	SnapshotID    string       `json:"snapshot_id"`
	SnapshotEpoch int64        `json:"snapshot_epoch"`
	Purpose       string       `json:"purpose,omitempty"`
	NICs          []computeNIC `json:"nics,omitempty"`
}

type computeSnapshotPort struct {
	PortID              string                  `json:"port_id"`
	NIC                 string                  `json:"nic"`
	MACAddress          string                  `json:"mac_address"`
	NetworkID           string                  `json:"network_id"`
	FixedIPs            []model.FixedIP         `json:"fixed_ips,omitempty"`
	SecurityGroupIDs    []string                `json:"security_group_ids"`
	AllocationIDs       []string                `json:"allocation_ids,omitempty"`
	AllocationRevisions []int64                 `json:"allocation_revisions,omitempty"`
	LSPName             string                  `json:"lsp_name"`
	NodeID              string                  `json:"node_id"`
	RequestedChassis    string                  `json:"requested_chassis"`
	BindingStatus       model.PortBindingStatus `json:"binding_status"`
	AdminStateUp        bool                    `json:"admin_state_up"`
	Revision            int64                   `json:"revision"`
	Generation          int64                   `json:"generation"`
}

type computeSnapshotPayload struct {
	Version       int                    `json:"version"`
	LifecycleID   string                 `json:"lifecycle_id"`
	Phase         string                 `json:"phase"`
	VMID          int                    `json:"vmid"`
	SnapshotID    string                 `json:"snapshot_id"`
	SnapshotEpoch int64                  `json:"snapshot_epoch"`
	NodeID        string                 `json:"node_id"`
	Node          string                 `json:"node"`
	Chassis       string                 `json:"chassis"`
	Blueprints    []computePortBlueprint `json:"blueprints"`
	Ports         []computeSnapshotPort  `json:"ports"`
}

type computeSnapshotCleanupRequest struct {
	VMID          int    `json:"vmid"`
	SnapshotID    string `json:"snapshot_id"`
	SnapshotEpoch int64  `json:"snapshot_epoch"`
}

type computeSnapshotMutationRequest struct {
	LifecycleID   string       `json:"lifecycle_id"`
	Action        string       `json:"action"`
	VMID          int          `json:"vmid"`
	SnapshotID    string       `json:"snapshot_id"`
	SnapshotEpoch int64        `json:"snapshot_epoch"`
	NICs          []computeNIC `json:"nics,omitempty"`
}

type computeSnapshotMutationTransaction struct {
	LifecycleID   string                `json:"lifecycle_id"`
	Action        string                `json:"action"`
	VMID          int                   `json:"vmid"`
	SnapshotID    string                `json:"snapshot_id"`
	SnapshotEpoch int64                 `json:"snapshot_epoch"`
	OperationID   string                `json:"operation_id"`
	PayloadHash   string                `json:"payload_hash"`
	Ports         []computeSnapshotPort `json:"ports,omitempty"`
}

type computeSnapshotMutationPayload struct {
	Version             int                   `json:"version"`
	LifecycleID         string                `json:"lifecycle_id"`
	Phase               string                `json:"phase"`
	Action              string                `json:"action"`
	VMID                int                   `json:"vmid"`
	SnapshotID          string                `json:"snapshot_id"`
	SnapshotEpoch       int64                 `json:"snapshot_epoch"`
	ManifestOperationID string                `json:"manifest_operation_id"`
	ManifestHash        string                `json:"manifest_hash"`
	NodeID              string                `json:"node_id"`
	Node                string                `json:"node"`
	Chassis             string                `json:"chassis"`
	Ports               []computeSnapshotPort `json:"ports,omitempty"`
}

type computeSnapshotTransaction struct {
	LifecycleID   string                `json:"lifecycle_id"`
	VMID          int                   `json:"vmid"`
	SnapshotID    string                `json:"snapshot_id"`
	SnapshotEpoch int64                 `json:"snapshot_epoch"`
	OperationID   string                `json:"operation_id"`
	PayloadHash   string                `json:"payload_hash"`
	Ports         []computeSnapshotPort `json:"ports"`
}

type computeDestroyCaptureRequest struct {
	LifecycleID string                    `json:"lifecycle_id"`
	VMID        int                       `json:"vmid"`
	NICs        []computeNIC              `json:"nics"`
	Template    bool                      `json:"template,omitempty"`
	Snapshots   []computeSnapshotIdentity `json:"snapshots,omitempty"`
}

type computeSnapshotIdentity struct {
	SnapshotID    string `json:"snapshot_id"`
	SnapshotEpoch int64  `json:"snapshot_epoch"`
}

type computeSnapshotRef struct {
	SnapshotID    string `json:"snapshot_id"`
	SnapshotEpoch int64  `json:"snapshot_epoch"`
	OperationID   string `json:"operation_id"`
	PayloadHash   string `json:"payload_hash"`
}

type computeDestroyPayload struct {
	Version             int                    `json:"version"`
	LifecycleID         string                 `json:"lifecycle_id"`
	Phase               string                 `json:"phase"`
	VMID                int                    `json:"vmid"`
	Template            bool                   `json:"template,omitempty"`
	NodeID              string                 `json:"node_id"`
	Node                string                 `json:"node"`
	Chassis             string                 `json:"chassis"`
	TemplateOperationID string                 `json:"template_operation_id,omitempty"`
	SnapshotRefs        []computeSnapshotRef   `json:"snapshot_refs,omitempty"`
	Blueprints          []computePortBlueprint `json:"blueprints"`
	Ports               []computeSourcePort    `json:"ports,omitempty"`
}

type computeDestroyTransaction struct {
	LifecycleID string              `json:"lifecycle_id"`
	VMID        int                 `json:"vmid"`
	Template    bool                `json:"template,omitempty"`
	Node        string              `json:"node"`
	OperationID string              `json:"operation_id"`
	PayloadHash string              `json:"payload_hash"`
	Ports       []computeSourcePort `json:"ports,omitempty"`
}

type computeClonePrepareRequest struct {
	CloneID        string       `json:"clone_id"`
	SourceVMID     int          `json:"source_vmid"`
	TargetVMID     int          `json:"target_vmid"`
	SourceNode     string       `json:"source_node"`
	TargetNode     string       `json:"target_node"`
	SnapshotID     string       `json:"snapshot_id,omitempty"`
	SnapshotEpoch  int64        `json:"snapshot_epoch,omitempty"`
	SourceTemplate bool         `json:"source_template,omitempty"`
	NICs           []computeNIC `json:"nics"`
}

type computeClonePayload struct {
	Version        int                    `json:"version"`
	CloneID        string                 `json:"clone_id"`
	Phase          string                 `json:"phase"`
	SourceVMID     int                    `json:"source_vmid"`
	TargetVMID     int                    `json:"target_vmid"`
	SourceNodeID   string                 `json:"source_node_id"`
	SourceNode     string                 `json:"source_node"`
	Source         string                 `json:"source_chassis"`
	TargetNodeID   string                 `json:"target_node_id"`
	TargetNode     string                 `json:"target_node"`
	Target         string                 `json:"target_chassis"`
	SnapshotID     string                 `json:"snapshot_id,omitempty"`
	SnapshotEpoch  int64                  `json:"snapshot_epoch,omitempty"`
	SourceTemplate bool                   `json:"source_template,omitempty"`
	Blueprints     []computePortBlueprint `json:"blueprints"`
	Ports          []computeOwnedPort     `json:"ports"`
}

type computeCloneTransaction struct {
	CloneID        string             `json:"clone_id"`
	SourceVMID     int                `json:"source_vmid"`
	TargetVMID     int                `json:"target_vmid"`
	SourceNode     string             `json:"source_node"`
	TargetNode     string             `json:"target_node"`
	SnapshotID     string             `json:"snapshot_id,omitempty"`
	SnapshotEpoch  int64              `json:"snapshot_epoch,omitempty"`
	SourceTemplate bool               `json:"source_template,omitempty"`
	OperationID    string             `json:"operation_id"`
	PayloadHash    string             `json:"payload_hash"`
	Ports          []computeOwnedPort `json:"ports"`
}

func (s *Server) computeClonePrepare(writer http.ResponseWriter, request *http.Request) {
	var input computeClonePrepareRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateLifecycleID("clone_id", input.CloneID); err != nil || input.SourceVMID < 1 || input.TargetVMID < 1 || input.SourceVMID == input.TargetVMID {
		if err == nil {
			err = computeError(http.StatusBadRequest, "invalid_request", "source_vmid and a distinct positive target_vmid are required", nil)
		}
		s.writeComputeError(writer, err)
		return
	}
	if err := validateComputeVM(input.SourceVMID, input.NICs); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if (input.SnapshotID == "") != (input.SnapshotEpoch == 0) || input.SnapshotEpoch < 0 {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "snapshot_id and positive snapshot_epoch must be supplied together", nil))
		return
	}
	source, err := s.localComputeNode(request.Context(), input.SourceNode)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	operationID := computeResourceOperationID(computeCloneAction, input.CloneID, input.SourceVMID, input.TargetVMID)
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.TargetVMID))
	defer unlock()
	if existing, getErr := s.store.Get(request.Context(), model.KindOperation, operationID); getErr == nil {
		operation := existing.(*model.Operation)
		payload, err := decodeClonePayload(operation)
		var replayTarget *model.Node
		if err == nil {
			replayTarget, err = s.resolveAttachmentNode(request.Context(), input.TargetNode)
		}
		if err != nil || !cloneReplayRequestMatches(input, source, replayTarget, payload) {
			if err == nil {
				err = computeError(http.StatusConflict, "clone_id_conflict", "clone_id was reused with different parameters", nil)
			}
			s.writeComputeError(writer, err)
			return
		}
		if operation.OperationStatus == model.OperationRunning && payload.Phase == "compensating" {
			cause := operation.Error
			if cause == "" {
				cause = "resuming interrupted clone prepare compensation"
			}
			s.writeComputeError(writer, s.failClonePrepare(request.Context(), operation, payload, errors.New(cause)))
			return
		}
		if operation.OperationStatus != model.OperationRunning || (payload.Phase != "preparing" && payload.Phase != "prepared") {
			s.writeComputeError(writer, computeError(http.StatusConflict, "clone_transaction_terminal", "clone transaction is already terminal", map[string]any{"phase": payload.Phase}))
			return
		}
		if payload.Phase == "preparing" {
			payload, err = s.prepareClonePorts(request.Context(), payload)
			if err == nil {
				operation, payload, err = s.persistClonePrepared(request.Context(), operation, payload)
			}
		} else {
			err = s.verifyClonePorts(request.Context(), payload)
		}
		if err != nil {
			s.writeComputeError(writer, s.failClonePrepare(request.Context(), operation, payload, err))
			return
		}
		writer.Header().Set("Idempotency-Replayed", "true")
		writeJSON(writer, http.StatusOK, map[string]any{"data": cloneTransaction(operation.ID, payload)})
		return
	} else if !errors.Is(getErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, getErr)
		return
	}
	target, err := s.readyComputeNode(request.Context(), input.TargetNode)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	blueprints, err := s.cloneSourceBlueprints(request.Context(), input, source)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	payload, err := newClonePayload(input, source, target, blueprints)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if targetPorts, listErr := s.store.List(request.Context(), model.KindPort, controlstore.ListOptions{VMID: input.TargetVMID}); listErr != nil {
		s.writeComputeError(writer, listErr)
		return
	} else if len(targetPorts) != 0 {
		s.writeComputeError(writer, computeError(http.StatusConflict, "clone_target_not_empty", "target VM already has PVN-managed ports", map[string]any{"vmid": input.TargetVMID, "ports": len(targetPorts)}))
		return
	}
	for _, expected := range payload.Ports {
		if _, getErr := s.store.Get(request.Context(), model.KindPort, expected.PortID); getErr == nil {
			s.writeComputeError(writer, computeError(http.StatusConflict, "clone_port_conflict", "deterministic clone port already exists without its parent transaction", map[string]any{"port_id": expected.PortID}))
			return
		} else if !errors.Is(getErr, controlstore.ErrNotFound) {
			s.writeComputeError(writer, getErr)
			return
		}
	}
	operation, err := s.createComputeOperation(request.Context(), operationID, computeCloneAction, input.TargetVMID, payload)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	payload, err = s.prepareClonePorts(request.Context(), payload)
	if err == nil {
		operation, payload, err = s.persistClonePrepared(request.Context(), operation, payload)
	}
	if err != nil {
		s.writeComputeError(writer, s.failClonePrepare(request.Context(), operation, payload, err))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": cloneTransaction(operation.ID, payload)})
}

func (s *Server) computeCloneFinish(writer http.ResponseWriter, request *http.Request, abort bool) {
	var input computeCloneTransaction
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateLifecycleID("clone_id", input.CloneID); err != nil || input.SourceVMID < 1 || input.TargetVMID < 1 || input.OperationID == "" || input.PayloadHash == "" || len(input.Ports) == 0 {
		if err == nil {
			err = computeError(http.StatusBadRequest, "invalid_request", "complete clone transaction echo is required", nil)
		}
		s.writeComputeError(writer, err)
		return
	}
	if input.OperationID != computeResourceOperationID(computeCloneAction, input.CloneID, input.SourceVMID, input.TargetVMID) {
		s.writeComputeError(writer, computeError(http.StatusConflict, "clone_transaction_mismatch", "clone operation_id does not match its identity", nil))
		return
	}
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.TargetVMID))
	defer unlock()
	operation, payload, err := s.loadCloneOperation(request.Context(), input.OperationID)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	expected := cloneTransaction(operation.ID, payload)
	if !reflect.DeepEqual(input, expected) {
		s.writeComputeError(writer, computeError(http.StatusConflict, "clone_transaction_mismatch", "clone transaction echo is incomplete, reordered, or stale", nil))
		return
	}
	local, err := s.localComputeNode(request.Context(), payload.SourceNode)
	if err != nil || local.ID != payload.SourceNodeID || local.ChassisID != payload.Source {
		if err == nil {
			err = computeError(http.StatusConflict, "clone_source_drift", "clone source node identity changed after prepare", nil)
		}
		s.writeComputeError(writer, err)
		return
	}
	phase, claimPhase := "committed", "committing"
	if abort {
		phase, claimPhase = "aborted", "aborting"
	}
	if operation.OperationStatus != model.OperationRunning {
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == phase {
			writer.Header().Set("Idempotency-Replayed", "true")
			writeJSON(writer, http.StatusOK, map[string]any{"data": input})
			return
		}
		s.writeComputeError(writer, computeError(http.StatusConflict, "clone_transaction_terminal", "clone transaction is terminal in a different phase", map[string]any{"phase": payload.Phase}))
		return
	}
	if payload.Phase != "prepared" && payload.Phase != claimPhase {
		s.writeComputeError(writer, computeError(http.StatusConflict, "clone_transaction_terminal", "clone transaction is claimed by a different action", map[string]any{"phase": payload.Phase}))
		return
	}
	if !abort {
		target, err := s.resolveAttachmentNode(request.Context(), payload.TargetNode)
		if err != nil || target.ID != payload.TargetNodeID || target.ChassisID != payload.Target {
			if err == nil {
				err = computeError(http.StatusConflict, "clone_target_drift", "clone target node identity changed after prepare", nil)
			}
			s.writeComputeError(writer, err)
			return
		}
	}
	if err := s.claimCloneOperation(request.Context(), operation.ID, "prepared", claimPhase); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if abort {
		if err := s.verifyCloneAbortPortSet(request.Context(), payload); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		failures := s.deleteClonePorts(request.Context(), payload)
		if len(failures) != 0 {
			s.writeComputeError(writer, computeError(http.StatusServiceUnavailable, "clone_abort_failed", "clone ports require recovery", map[string]any{"recovery_required": true, "errors": failures}))
			return
		}
	} else if err := s.verifyClonePorts(request.Context(), payload); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := s.terminalizeClaimedCloneOperation(request.Context(), operation.ID, claimPhase, phase); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": input})
}

func (s *Server) verifyCloneAbortPortSet(ctx context.Context, payload computeClonePayload) error {
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: payload.TargetVMID})
	if err != nil {
		return err
	}
	owned := make(map[string]bool, len(payload.Ports))
	for _, expected := range payload.Ports {
		owned[expected.PortID] = true
	}
	for _, resource := range resources {
		if !owned[resource.GetMetadata().ID] {
			return computeError(http.StatusConflict, "clone_port_set_drift", "target VM has a port outside the abort transaction", map[string]any{"port_id": resource.GetMetadata().ID})
		}
	}
	return nil
}

func (s *Server) cloneSourceBlueprints(ctx context.Context, input computeClonePrepareRequest, source *model.Node) ([]computePortBlueprint, error) {
	if input.SnapshotID != "" {
		payload, err := s.findSnapshotPayload(ctx, input.SourceVMID, input.SnapshotID, input.SnapshotEpoch)
		if err != nil {
			return nil, err
		}
		if err := blueprintNICsMatch(input.NICs, payload.Blueprints); err != nil {
			return nil, err
		}
		if err := s.validateCloneBlueprintResources(ctx, payload.Blueprints); err != nil {
			return nil, err
		}
		return cloneBlueprints(payload.Blueprints), nil
	}
	if !input.SourceTemplate {
		ports, err := s.loadExactComputePorts(ctx, input.SourceVMID, input.NICs)
		if err != nil {
			return nil, err
		}
		for _, port := range ports {
			if port.NodeID != source.ID || port.RequestedChassis != source.ChassisID || (port.BindingStatus != model.PortBinding && port.BindingStatus != model.PortBound) || port.State != model.ResourceReady || port.AppliedRevision != port.Revision {
				return nil, computeError(http.StatusConflict, "clone_source_not_local", "clone source port is not ready on the local source node", computePortDetails(port))
			}
		}
		blueprints, err := blueprintsFromPorts(ports)
		if err != nil {
			return nil, err
		}
		if err := s.validateCloneBlueprintResources(ctx, blueprints); err != nil {
			return nil, err
		}
		return blueprints, nil
	}
	if err := s.requireVMPortSetEmpty(ctx, input.SourceVMID, "template clone source validation"); err != nil {
		return nil, err
	}
	payload, err := s.findCommittedTemplate(ctx, input.SourceVMID, true)
	if err != nil {
		return nil, err
	}
	if err := blueprintNICsMatch(input.NICs, payload.Blueprints); err != nil {
		return nil, err
	}
	if err := s.validateCloneBlueprintResources(ctx, payload.Blueprints); err != nil {
		return nil, err
	}
	return cloneBlueprints(payload.Blueprints), nil
}

func (s *Server) validateCloneBlueprintResources(ctx context.Context, blueprints []computePortBlueprint) error {
	for _, blueprint := range blueprints {
		if _, err := s.store.Get(ctx, model.KindNetwork, blueprint.NetworkID); err != nil {
			return computeError(http.StatusConflict, "clone_network_unavailable", "clone blueprint network is unavailable", map[string]any{"network_id": blueprint.NetworkID})
		}
		if blueprint.SubnetID != "" {
			resource, err := s.store.Get(ctx, model.KindSubnet, blueprint.SubnetID)
			if err != nil {
				return computeError(http.StatusConflict, "clone_subnet_unavailable", "clone blueprint subnet is unavailable", map[string]any{"subnet_id": blueprint.SubnetID})
			}
			if resource.(*model.Subnet).NetworkID != blueprint.NetworkID {
				return computeError(http.StatusConflict, "clone_subnet_drift", "clone blueprint subnet no longer belongs to its network", map[string]any{"subnet_id": blueprint.SubnetID, "network_id": blueprint.NetworkID})
			}
		}
		for _, groupID := range blueprint.SecurityGroupIDs {
			if _, err := s.store.Get(ctx, model.KindSecurityGroup, groupID); err != nil {
				return computeError(http.StatusConflict, "clone_security_group_unavailable", "clone blueprint security group is unavailable", map[string]any{"security_group_id": groupID})
			}
		}
	}
	return nil
}

func blueprintsFromPorts(ports []*model.Port) ([]computePortBlueprint, error) {
	result := make([]computePortBlueprint, 0, len(ports))
	for _, port := range ports {
		if len(port.FixedIPs) > 1 {
			return nil, computeError(http.StatusConflict, "multi_ip_clone_unsupported", "automatic clone requires at most one fixed-IP subnet per PVN NIC", computePortDetails(port))
		}
		blueprint := computePortBlueprint{NIC: port.NIC, SourceMACAddress: strings.ToLower(port.MACAddress), NetworkID: port.NetworkID, SecurityGroupIDs: append([]string(nil), port.SecurityGroupIDs...)}
		if len(port.FixedIPs) == 1 {
			blueprint.SubnetID = port.FixedIPs[0].SubnetID
		}
		sort.Strings(blueprint.SecurityGroupIDs)
		result = append(result, blueprint)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NIC < result[j].NIC })
	return result, nil
}

func newClonePayload(input computeClonePrepareRequest, source, target *model.Node, blueprints []computePortBlueprint) (computeClonePayload, error) {
	payload := computeClonePayload{Version: computePayloadVersion, CloneID: input.CloneID, Phase: "preparing", SourceVMID: input.SourceVMID, TargetVMID: input.TargetVMID, SourceNodeID: source.ID, SourceNode: source.Name, Source: source.ChassisID, TargetNodeID: target.ID, TargetNode: target.Name, Target: target.ChassisID, SnapshotID: input.SnapshotID, SnapshotEpoch: input.SnapshotEpoch, SourceTemplate: input.SourceTemplate, Blueprints: cloneBlueprints(blueprints)}
	for _, blueprint := range blueprints {
		provision := cloneProvisionRequest(input.TargetVMID, blueprint)
		key, ownership := clonePortOwnership(input.CloneID, input.SourceVMID, input.TargetVMID, input.SnapshotID, input.SnapshotEpoch, blueprint)
		identity, err := newProvisionIdentity(key, provision)
		if err != nil {
			return payload, err
		}
		allocationID := ""
		allocationRevision := int64(0)
		if blueprint.SubnetID != "" {
			allocationID = identity.allocationID
			allocationRevision = 2
		}
		payload.Ports = append(payload.Ports, computeOwnedPort{PortID: identity.portID, NIC: blueprint.NIC, MACAddress: deterministicProvisionMAC(identity.digest), NetworkID: blueprint.NetworkID, SecurityGroupIDs: append([]string(nil), blueprint.SecurityGroupIDs...), LSPName: "pvn-" + identity.portID, Revision: 2, Generation: 2, DetachedRevision: 3, DetachedGeneration: 3, AllocationID: allocationID, AllocationRevision: allocationRevision, OwnershipDigest: ownership})
	}
	return payload, nil
}

func cloneProvisionRequest(targetVMID int, blueprint computePortBlueprint) portProvisionRequest {
	return portProvisionRequest{NetworkID: blueprint.NetworkID, SubnetID: blueprint.SubnetID, Name: fmt.Sprintf("vm%d-%s", targetVMID, blueprint.NIC), SecurityGroupIDs: append([]string(nil), blueprint.SecurityGroupIDs...)}
}

func clonePortOwnership(cloneID string, sourceVMID, targetVMID int, snapshotID string, snapshotEpoch int64, blueprint computePortBlueprint) (string, string) {
	encoded, _ := model.MarshalOperationPayload(struct {
		CloneID       string               `json:"clone_id"`
		SourceVMID    int                  `json:"source_vmid"`
		TargetVMID    int                  `json:"target_vmid"`
		SnapshotID    string               `json:"snapshot_id,omitempty"`
		SnapshotEpoch int64                `json:"snapshot_epoch,omitempty"`
		Blueprint     computePortBlueprint `json:"blueprint"`
	}{cloneID, sourceVMID, targetVMID, snapshotID, snapshotEpoch, blueprint})
	digest := sha256.Sum256([]byte(encoded))
	hexDigest := hex.EncodeToString(digest[:])
	return "compute-clone-port-" + hexDigest, hexDigest
}

func (s *Server) prepareClonePorts(ctx context.Context, payload computeClonePayload) (computeClonePayload, error) {
	for index, blueprint := range payload.Blueprints {
		expected := payload.Ports[index]
		provision := cloneProvisionRequest(payload.TargetVMID, blueprint)
		key, ownership := clonePortOwnership(payload.CloneID, payload.SourceVMID, payload.TargetVMID, payload.SnapshotID, payload.SnapshotEpoch, blueprint)
		identity, err := newProvisionIdentity(key, provision)
		if err != nil || identity.portID != expected.PortID || ownership != expected.OwnershipDigest {
			return payload, computeError(http.StatusConflict, "clone_payload_invalid", "clone port ownership does not match its durable payload", map[string]any{"nic": blueprint.NIC})
		}
		port, err := s.provisionResourcePort(ctx, key, identity, provision)
		if err != nil {
			return payload, err
		}
		if port.VMID == payload.TargetVMID && port.NIC == blueprint.NIC && port.NodeID == payload.TargetNodeID && port.RequestedChassis == payload.Target && port.Generation == expected.Generation {
			if err := s.validatePreparingClonePort(ctx, port, blueprint, expected); err != nil {
				return payload, err
			}
			realized, err := s.forceRealizeComputePort(ctx, port)
			if err != nil {
				return payload, err
			}
			payload.Ports[index] = ownedPortManifest(realized, ownership, expected.AllocationID, expected.AllocationRevision)
			continue
		}
		if !portCanBeDeprovisioned(port) || port.Revision != 1 || port.Generation != 1 || !strings.EqualFold(port.MACAddress, expected.MACAddress) {
			return payload, computeError(http.StatusConflict, "clone_port_conflict", "clone-owned port changed before attachment", computePortDetails(port))
		}
		desired := clonePort(port)
		desired.Metadata = model.Metadata{ID: port.ID}
		desired.NodeID, desired.VMID, desired.NIC = payload.TargetNodeID, payload.TargetVMID, blueprint.NIC
		desired.RequestedChassis, desired.BindingStatus, desired.Generation = payload.Target, model.PortBinding, expected.Generation
		updated, _, err := s.store.Update(ctx, desired, port.Revision, "compute-clone-attach-"+expected.OwnershipDigest)
		if err != nil {
			return payload, err
		}
		if updated.GetMetadata().Revision != expected.Revision {
			return payload, computeError(http.StatusConflict, "clone_revision_drift", "clone port revision differs from its durable transaction", computePortDetails(updated.(*model.Port)))
		}
		realized, err := s.forceRealizeComputePort(ctx, updated.(*model.Port))
		if err != nil {
			return payload, err
		}
		payload.Ports[index] = ownedPortManifest(realized, ownership, expected.AllocationID, expected.AllocationRevision)
	}
	if err := s.verifyClonePorts(ctx, payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func (s *Server) provisionResourcePort(ctx context.Context, key string, identity provisionIdentity, input portProvisionRequest) (*model.Port, error) {
	port := provisionPortResource(identity, input)
	if err := port.Validate(); err != nil {
		return nil, err
	}
	network, subnet, err := s.loadProvisionTopology(ctx, input)
	if err != nil {
		return nil, err
	}
	if network.External || network.ProviderNetworkID != "" {
		return nil, computeError(http.StatusConflict, "provider_network_port", "tenant VM ports cannot use an external or provider-backed network", nil)
	}
	operation, replayed, err := s.beginPortProvision(ctx, key, identity, port.ID)
	if err != nil {
		return nil, err
	}
	if replayed && operation.OperationStatus == model.OperationSucceeded {
		return s.loadPort(ctx, port.ID)
	}
	var allocation *model.IPAllocation
	if subnet != nil {
		allocation, err = s.reserveProvisionAddress(ctx, input, identity, subnet, port.ID)
		if err != nil {
			s.failPortProvision(ctx, operation, err)
			return nil, err
		}
		port.FixedIPs = []model.FixedIP{{SubnetID: subnet.ID, Address: allocation.Address}}
	}
	created, _, err := s.createProvisionPort(ctx, port)
	if err != nil {
		if rollbackProvisionError(err) {
			s.rollbackProvisionReservation(ctx, allocation, port.ID)
		}
		s.failPortProvision(ctx, operation, err)
		return nil, err
	}
	if allocation != nil {
		if err := s.allocateProvisionAddress(ctx, allocation.ID, created); err != nil {
			s.failPortProvision(ctx, operation, err)
			return nil, err
		}
	}
	if err := s.completePortProvision(ctx, operation); err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Server) verifyClonePorts(ctx context.Context, payload computeClonePayload) error {
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: payload.TargetVMID})
	if err != nil {
		return err
	}
	if len(resources) != len(payload.Ports) {
		return computeError(http.StatusConflict, "clone_port_set_drift", "target VM port set differs from the exact clone transaction", map[string]any{"expected": len(payload.Ports), "current": len(resources)})
	}
	owned := make(map[string]bool, len(payload.Ports))
	for _, expected := range payload.Ports {
		owned[expected.PortID] = true
	}
	for _, resource := range resources {
		if !owned[resource.GetMetadata().ID] {
			return computeError(http.StatusConflict, "clone_port_set_drift", "target VM has a port outside the exact clone transaction", map[string]any{"port_id": resource.GetMetadata().ID})
		}
	}
	for _, expected := range payload.Ports {
		port, err := s.loadPort(ctx, expected.PortID)
		if err != nil {
			return err
		}
		if !clonePortOwnedByPayload(port, payload, expected) {
			return computeError(http.StatusConflict, "clone_port_drift", "clone port changed before commit", computePortDetails(port))
		}
		if err := s.validateCloneAllocation(ctx, port, expected); err != nil {
			return err
		}
		if _, err := s.forceRealizeComputePort(ctx, port); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) deleteClonePorts(ctx context.Context, payload computeClonePayload) []string {
	failures := make([]string, 0)
	for index, expected := range payload.Ports {
		// The deterministic prepare cleanup recognizes provisional, attached,
		// detached, and tombstone states, so it remains resumable after the
		// durable phase moves to compensating or aborting.
		err := s.deletePreparingClonePort(ctx, payload, payload.Blueprints[index], expected)
		if err != nil {
			failures = append(failures, expected.PortID+": "+err.Error())
		}
	}
	return failures
}

func (s *Server) failClonePrepare(ctx context.Context, operation *model.Operation, payload computeClonePayload, cause error) error {
	recoveryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), computeRecoveryTimeout)
	defer cancel()
	if err := s.claimCloneOperationFrom(recoveryContext, operation.ID, []string{"preparing", "prepared"}, "compensating"); err != nil {
		return err
	}
	failures := s.deleteClonePorts(recoveryContext, payload)
	if len(failures) != 0 {
		if err := s.recordComputeClaimError(recoveryContext, operation.ID, computeCloneAction, "compensating", cause.Error()); err != nil {
			failures = append(failures, err.Error())
		}
		return computeError(http.StatusServiceUnavailable, "clone_prepare_failed", cause.Error(), map[string]any{"operation_id": operation.ID, "recovery_required": true, "compensation_errors": failures})
	}
	if err := s.terminalizeClaimedCloneOperationWithStatus(recoveryContext, operation.ID, "compensating", "compensated", model.OperationFailed, cause.Error()); err != nil {
		failures = append(failures, err.Error())
	}
	return computeError(http.StatusServiceUnavailable, "clone_prepare_failed", cause.Error(), map[string]any{"operation_id": operation.ID, "recovery_required": len(failures) != 0, "compensation_errors": failures})
}

func (s *Server) deletePreparingClonePort(ctx context.Context, payload computeClonePayload, blueprint computePortBlueprint, expected computeOwnedPort) error {
	provision := cloneProvisionRequest(payload.TargetVMID, blueprint)
	key, ownership := clonePortOwnership(payload.CloneID, payload.SourceVMID, payload.TargetVMID, payload.SnapshotID, payload.SnapshotEpoch, blueprint)
	identity, identityErr := newProvisionIdentity(key, provision)
	expectedAllocationID := ""
	if blueprint.SubnetID != "" {
		expectedAllocationID = identity.allocationID
	}
	if identityErr != nil || identity.portID != expected.PortID || ownership != expected.OwnershipDigest || expected.AllocationID != expectedAllocationID ||
		!strings.EqualFold(expected.MACAddress, deterministicProvisionMAC(identity.digest)) {
		return computeError(http.StatusConflict, "clone_payload_invalid", "clone cleanup ownership does not match its deterministic identity", map[string]any{"port_id": expected.PortID})
	}
	port, err := s.loadPort(ctx, expected.PortID)
	if errors.Is(err, controlstore.ErrNotFound) {
		return s.deletePreparingCloneAllocation(ctx, nil, blueprint, expected)
	}
	if err != nil {
		return err
	}
	if err := s.ensureNoPortDeprovisionDependents(ctx, port.ID); err != nil {
		return err
	}
	manifest := expected
	if blueprint.SubnetID != "" && len(manifest.FixedIPs) == 0 {
		if len(port.FixedIPs) != 1 || port.FixedIPs[0].SubnetID != blueprint.SubnetID || port.FixedIPs[0].Address == "" {
			return computeError(http.StatusConflict, "clone_allocation_drift", "clone cleanup cannot reconstruct its deterministic fixed-IP ownership", computePortDetails(port))
		}
		manifest.FixedIPs = append([]model.FixedIP(nil), port.FixedIPs...)
	}
	if clonePortAttached(port, payload.TargetVMID, payload.TargetNodeID, payload.Target, manifest) || clonePortDetached(port, manifest) || clonePortDeletionTombstone(port, manifest) {
		if clonePortAttached(port, payload.TargetVMID, payload.TargetNodeID, payload.Target, manifest) {
			if err := s.validatePreparingClonePort(ctx, port, blueprint, manifest); err != nil {
				return err
			}
		}
		return s.deleteOwnedComputePort(ctx, payload.TargetVMID, payload.TargetNodeID, payload.Target, manifest, "compute-clone-prepare-delete-"+expected.OwnershipDigest)
	}
	if (port.Metadata.State == model.ResourceDeleting || port.Metadata.State == model.ResourceError) && port.Revision != 1 {
		if !preparingClonePortShape(port, blueprint, expected) || port.Revision != 2 {
			return computeError(http.StatusConflict, "clone_port_ownership_mismatch", "refusing to resume provisional clone port deletion outside its deterministic state", computePortDetails(port))
		}
		if err := s.deletePreparingCloneAllocation(ctx, port, blueprint, expected); err != nil {
			return err
		}
		if err := s.reconcileDeprovisionDelete(ctx, port); err != nil {
			return err
		}
		return s.store.Purge(ctx, model.KindPort, port.ID, port.Revision)
	}
	if !preparingClonePortShape(port, blueprint, expected) || port.Revision != 1 {
		return computeError(http.StatusConflict, "clone_port_ownership_mismatch", "refusing to delete a provisional port outside its deterministic clone state", computePortDetails(port))
	}
	if err := s.deletePreparingCloneAllocation(ctx, port, blueprint, expected); err != nil {
		return err
	}
	tombstone, _, err := s.store.BeginDelete(ctx, model.KindPort, port.ID, port.Revision, "compute-clone-provisional-delete-"+expected.OwnershipDigest)
	if err != nil {
		return err
	}
	if err := s.reconcileDeprovisionDelete(ctx, tombstone); err != nil {
		return err
	}
	return s.store.Purge(ctx, model.KindPort, port.ID, tombstone.GetMetadata().Revision)
}

func clonePortDeletionTombstone(port *model.Port, expected computeOwnedPort) bool {
	return (port.Metadata.State == model.ResourceDeleting || port.Metadata.State == model.ResourceError) &&
		clonePortDetachedShape(port, expected) && port.Revision == expected.DetachedRevision+1
}

func preparingClonePortShape(port *model.Port, blueprint computePortBlueprint, expected computeOwnedPort) bool {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	if !portCanBeDeprovisioned(port) || port.Generation != 1 || port.NetworkID != blueprint.NetworkID || port.LSPName != expected.LSPName ||
		!strings.EqualFold(port.MACAddress, expected.MACAddress) || !slices.Equal(groups, expected.SecurityGroupIDs) || !port.AdminStateUp {
		return false
	}
	if blueprint.SubnetID == "" {
		return len(port.FixedIPs) == 0 && expected.AllocationID == ""
	}
	return len(port.FixedIPs) == 1 && port.FixedIPs[0].SubnetID == blueprint.SubnetID && expected.AllocationID != ""
}

func (s *Server) deletePreparingCloneAllocation(ctx context.Context, port *model.Port, blueprint computePortBlueprint, expected computeOwnedPort) error {
	if expected.AllocationID == "" {
		if port != nil {
			return s.ensureOnlyCloneAllocation(ctx, port.ID, "")
		}
		return nil
	}
	if err := s.ensureOnlyCloneAllocation(ctx, expected.PortID, expected.AllocationID); err != nil {
		return err
	}
	resource, err := s.store.Get(ctx, model.KindIPAllocation, expected.AllocationID)
	if errors.Is(err, controlstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	allocation := resource.(*model.IPAllocation)
	portID := expected.PortID
	if port != nil {
		portID = port.ID
		if len(port.FixedIPs) != 1 || allocation.SubnetID != port.FixedIPs[0].SubnetID || allocation.Address != port.FixedIPs[0].Address {
			return computeError(http.StatusConflict, "clone_allocation_drift", "provisional clone allocation differs from its port fixed IP", map[string]any{"allocation_id": allocation.ID})
		}
	}
	if allocation.SubnetID != blueprint.SubnetID ||
		(allocation.State == model.IPReserved && allocation.PortID != "") ||
		(allocation.State == model.IPAllocated && allocation.PortID != portID) ||
		(allocation.State != model.IPReserved && allocation.State != model.IPAllocated) {
		return computeError(http.StatusConflict, "clone_allocation_drift", "provisional clone allocation is outside its deterministic ownership state", map[string]any{"allocation_id": allocation.ID})
	}
	baseRevision := int64(1)
	if allocation.State == model.IPAllocated {
		baseRevision = expected.AllocationRevision
	}
	if allocation.Metadata.State == model.ResourceDeleting || allocation.Metadata.State == model.ResourceError {
		if allocation.Revision != baseRevision+1 {
			return computeError(http.StatusConflict, "clone_allocation_drift", "provisional clone allocation tombstone revision is not owned", map[string]any{"allocation_id": allocation.ID})
		}
		if err := s.reconcileDeprovisionDelete(ctx, allocation); err != nil {
			return err
		}
		return s.store.Purge(ctx, model.KindIPAllocation, allocation.ID, allocation.Revision)
	}
	if allocation.Revision != baseRevision {
		return computeError(http.StatusConflict, "clone_allocation_drift", "provisional clone allocation revision is not owned", map[string]any{"allocation_id": allocation.ID})
	}
	tombstone, _, err := s.store.BeginDelete(ctx, model.KindIPAllocation, allocation.ID, allocation.Revision, "compute-clone-provisional-allocation-delete-"+expected.OwnershipDigest)
	if err != nil {
		return err
	}
	if err := s.reconcileDeprovisionDelete(ctx, tombstone); err != nil {
		return err
	}
	return s.store.Purge(ctx, model.KindIPAllocation, allocation.ID, tombstone.GetMetadata().Revision)
}

func (s *Server) deleteOwnedComputePort(ctx context.Context, vmid int, nodeID, chassis string, expected computeOwnedPort, key string) error {
	port, err := s.loadPort(ctx, expected.PortID)
	if errors.Is(err, controlstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.ensureNoPortDeprovisionDependents(ctx, port.ID); err != nil {
		return err
	}
	if (port.Metadata.State == model.ResourceDeleting || port.Metadata.State == model.ResourceError) &&
		!clonePortAttached(port, vmid, nodeID, chassis, expected) && !clonePortDetached(port, expected) {
		if !clonePortDetachedShape(port, expected) || port.Revision != expected.DetachedRevision+1 {
			return computeError(http.StatusConflict, "compute_port_ownership_mismatch", "refusing to resume deletion outside the exact clone transaction", computePortDetails(port))
		}
		if err := s.validateCloneAllocationForDeletion(ctx, port, expected); err != nil {
			return err
		}
		if err := s.reconcileDeprovisionDelete(ctx, port); err != nil {
			return err
		}
		return s.store.Purge(ctx, model.KindPort, port.ID, port.Revision)
	}
	if clonePortAttached(port, vmid, nodeID, chassis, expected) {
		if err := s.validateCloneAllocation(ctx, port, expected); err != nil {
			return err
		}
		desired := clonePort(port)
		desired.Metadata = model.Metadata{ID: port.ID}
		desired.NodeID, desired.VMID, desired.NIC, desired.RequestedChassis = "", 0, "", ""
		desired.BindingStatus, desired.Generation = model.PortUnbound, expected.DetachedGeneration
		updated, _, err := s.store.Update(ctx, desired, port.Revision, key+"-unbind")
		if err != nil {
			return err
		}
		port = updated.(*model.Port)
		if !clonePortDetached(port, expected) {
			return computeError(http.StatusConflict, "compute_port_ownership_mismatch", "clone port detach did not produce its exact durable state", computePortDetails(port))
		}
		port, err = s.forceRealizeComputePort(ctx, port)
		if err != nil {
			return err
		}
	} else if !clonePortDetached(port, expected) {
		return computeError(http.StatusConflict, "compute_port_ownership_mismatch", "refusing to delete a port outside its exact lifecycle transaction", computePortDetails(port))
	}
	if err := s.validateCloneAllocationForDeletion(ctx, port, expected); err != nil {
		return err
	}
	if err := s.releasePortAllocations(ctx, port, key); err != nil {
		return err
	}
	tombstone, _, err := s.store.BeginDelete(ctx, model.KindPort, port.ID, port.Revision, key)
	if err != nil {
		return err
	}
	if err := s.reconcileDeprovisionDelete(ctx, tombstone); err != nil {
		return err
	}
	return s.store.Purge(ctx, model.KindPort, port.ID, tombstone.GetMetadata().Revision)
}

func clonePortAttached(port *model.Port, vmid int, nodeID, chassis string, expected computeOwnedPort) bool {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	return port.ID == expected.PortID && port.VMID == vmid && port.NIC == expected.NIC && port.NodeID == nodeID &&
		port.RequestedChassis == chassis && port.Revision == expected.Revision && port.Generation == expected.Generation &&
		port.NetworkID == expected.NetworkID && port.LSPName == expected.LSPName && strings.EqualFold(port.MACAddress, expected.MACAddress) &&
		reflect.DeepEqual(port.FixedIPs, expected.FixedIPs) && slices.Equal(groups, expected.SecurityGroupIDs) &&
		(port.BindingStatus == model.PortBinding || port.BindingStatus == model.PortBound)
}

func clonePortDetached(port *model.Port, expected computeOwnedPort) bool {
	return clonePortDetachedShape(port, expected) && port.Revision == expected.DetachedRevision
}

func clonePortDetachedShape(port *model.Port, expected computeOwnedPort) bool {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	return port.ID == expected.PortID && portCanBeDeprovisioned(port) &&
		port.Generation == expected.DetachedGeneration && port.NetworkID == expected.NetworkID && port.LSPName == expected.LSPName &&
		strings.EqualFold(port.MACAddress, expected.MACAddress) && reflect.DeepEqual(port.FixedIPs, expected.FixedIPs) &&
		slices.Equal(groups, expected.SecurityGroupIDs)
}

func (s *Server) validateCloneAllocationForDeletion(ctx context.Context, port *model.Port, expected computeOwnedPort) error {
	if expected.AllocationID != "" {
		resource, err := s.store.Get(ctx, model.KindIPAllocation, expected.AllocationID)
		if err == nil {
			allocation := resource.(*model.IPAllocation)
			if allocation.State != model.IPAllocated || allocation.PortID != port.ID || len(port.FixedIPs) != 1 || allocation.SubnetID != port.FixedIPs[0].SubnetID || allocation.Address != port.FixedIPs[0].Address || !lifecycleAllocationRevisionMatches(allocation, expected.AllocationRevision, true) {
				return computeError(http.StatusConflict, "clone_allocation_drift", "durable clone allocation exists but no longer belongs to its port", map[string]any{"allocation_id": expected.AllocationID})
			}
		} else if !errors.Is(err, controlstore.ErrNotFound) {
			return err
		}
	}
	resources, err := s.store.List(ctx, model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	foundExpected := false
	for _, resource := range resources {
		allocation := resource.(*model.IPAllocation)
		if allocation.PortID != port.ID {
			continue
		}
		if allocation.ID != expected.AllocationID {
			return computeError(http.StatusConflict, "clone_allocation_drift", "clone port has an allocation outside its durable ownership manifest", map[string]any{"allocation_id": allocation.ID})
		}
		foundExpected = true
		if allocation.State != model.IPAllocated || len(port.FixedIPs) != 1 || allocation.SubnetID != port.FixedIPs[0].SubnetID || allocation.Address != port.FixedIPs[0].Address || !lifecycleAllocationRevisionMatches(allocation, expected.AllocationRevision, true) {
			return computeError(http.StatusConflict, "clone_allocation_drift", "clone allocation differs from its durable ownership manifest", map[string]any{"allocation_id": allocation.ID})
		}
	}
	if expected.AllocationID == "" && foundExpected {
		return computeError(http.StatusConflict, "clone_allocation_drift", "clone port unexpectedly owns an allocation", nil)
	}
	return nil
}

func (s *Server) validatePreparingClonePort(ctx context.Context, port *model.Port, blueprint computePortBlueprint, expected computeOwnedPort) error {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	if port.Revision != expected.Revision || port.Generation != expected.Generation || port.NetworkID != blueprint.NetworkID ||
		port.LSPName != expected.LSPName || !strings.EqualFold(port.MACAddress, expected.MACAddress) ||
		!slices.Equal(groups, expected.SecurityGroupIDs) || !port.AdminStateUp ||
		(port.BindingStatus != model.PortBinding && port.BindingStatus != model.PortBound) {
		return computeError(http.StatusConflict, "clone_port_drift", "prepared clone port differs from its deterministic pre-manifest state", computePortDetails(port))
	}
	if blueprint.SubnetID == "" {
		if len(port.FixedIPs) != 0 || expected.AllocationID != "" {
			return computeError(http.StatusConflict, "clone_allocation_drift", "clone port has an unexpected fixed IP allocation", computePortDetails(port))
		}
		return s.ensureOnlyCloneAllocation(ctx, port.ID, "")
	}
	if len(port.FixedIPs) != 1 || port.FixedIPs[0].SubnetID != blueprint.SubnetID || expected.AllocationID == "" {
		return computeError(http.StatusConflict, "clone_allocation_drift", "clone port fixed IP does not match its deterministic allocation", computePortDetails(port))
	}
	return s.validateCloneAllocation(ctx, port, expected)
}

func (s *Server) validateCloneAllocation(ctx context.Context, port *model.Port, expected computeOwnedPort) error {
	if expected.AllocationID == "" {
		return s.ensureOnlyCloneAllocation(ctx, port.ID, "")
	}
	resource, err := s.store.Get(ctx, model.KindIPAllocation, expected.AllocationID)
	if err != nil {
		return computeError(http.StatusConflict, "clone_allocation_drift", "clone allocation is missing from its durable ownership manifest", map[string]any{"allocation_id": expected.AllocationID})
	}
	allocation := resource.(*model.IPAllocation)
	if allocation.State != model.IPAllocated || allocation.PortID != port.ID || len(port.FixedIPs) != 1 ||
		allocation.SubnetID != port.FixedIPs[0].SubnetID || allocation.Address != port.FixedIPs[0].Address || allocation.Revision != expected.AllocationRevision {
		return computeError(http.StatusConflict, "clone_allocation_drift", "clone allocation differs from its durable ownership manifest", map[string]any{"allocation_id": expected.AllocationID})
	}
	return s.ensureOnlyCloneAllocation(ctx, port.ID, expected.AllocationID)
}

func (s *Server) ensureOnlyCloneAllocation(ctx context.Context, portID, expectedID string) error {
	resources, err := s.store.List(ctx, model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	for _, resource := range resources {
		allocation := resource.(*model.IPAllocation)
		if allocation.PortID == portID && allocation.ID != expectedID {
			return computeError(http.StatusConflict, "clone_allocation_drift", "clone port has an allocation outside its durable ownership manifest", map[string]any{"allocation_id": allocation.ID})
		}
	}
	return nil
}

func cloneRequestMatches(input computeClonePrepareRequest, source, target *model.Node, payload computeClonePayload) bool {
	if payload.Version != computePayloadVersion || payload.CloneID != input.CloneID || payload.SourceVMID != input.SourceVMID || payload.TargetVMID != input.TargetVMID || payload.SourceNodeID != source.ID || payload.SourceNode != source.Name || payload.Source != source.ChassisID || payload.TargetNodeID != target.ID || payload.TargetNode != target.Name || payload.Target != target.ChassisID || payload.SnapshotID != input.SnapshotID || payload.SnapshotEpoch != input.SnapshotEpoch || payload.SourceTemplate != input.SourceTemplate {
		return false
	}
	return blueprintNICsMatch(input.NICs, payload.Blueprints) == nil
}

func cloneReplayRequestMatches(input computeClonePrepareRequest, source, target *model.Node, payload computeClonePayload) bool {
	if target == nil || payload.Version != computePayloadVersion || payload.CloneID != input.CloneID || payload.SourceVMID != input.SourceVMID || payload.TargetVMID != input.TargetVMID || payload.SourceNodeID != source.ID || payload.SourceNode != source.Name || payload.Source != source.ChassisID || payload.TargetNodeID != target.ID || payload.TargetNode != target.Name || payload.Target != target.ChassisID || payload.SnapshotID != input.SnapshotID || payload.SnapshotEpoch != input.SnapshotEpoch || payload.SourceTemplate != input.SourceTemplate {
		return false
	}
	return blueprintNICsMatch(input.NICs, payload.Blueprints) == nil
}

func cloneTransaction(operationID string, payload computeClonePayload) computeCloneTransaction {
	return computeCloneTransaction{CloneID: payload.CloneID, SourceVMID: payload.SourceVMID, TargetVMID: payload.TargetVMID, SourceNode: payload.SourceNode, TargetNode: payload.TargetNode, SnapshotID: payload.SnapshotID, SnapshotEpoch: payload.SnapshotEpoch, SourceTemplate: payload.SourceTemplate, OperationID: operationID, PayloadHash: clonePayloadHash(payload), Ports: append([]computeOwnedPort(nil), payload.Ports...)}
}

func clonePayloadHash(payload computeClonePayload) string {
	payload.Phase = ""
	return computePayloadHash(payload)
}

func decodeClonePayload(operation *model.Operation) (computeClonePayload, error) {
	var payload computeClonePayload
	if operation.Action != computeCloneAction || model.UnmarshalOperationPayload(operation.Payload, &payload) != nil || payload.Version != computePayloadVersion || payload.CloneID == "" || payload.SourceNodeID == "" || payload.Source == "" || len(payload.Ports) == 0 || len(payload.Ports) != len(payload.Blueprints) {
		return payload, computeError(http.StatusConflict, "clone_payload_invalid", "durable clone payload is invalid", map[string]any{"operation_id": operation.ID})
	}
	return payload, nil
}

func (s *Server) loadCloneOperation(ctx context.Context, id string) (*model.Operation, computeClonePayload, error) {
	resource, err := s.store.Get(ctx, model.KindOperation, id)
	if err != nil {
		return nil, computeClonePayload{}, err
	}
	operation := resource.(*model.Operation)
	payload, err := decodeClonePayload(operation)
	return operation, payload, err
}

func (s *Server) claimCloneOperation(ctx context.Context, id, fromPhase, claimPhase string) error {
	return s.claimCloneOperationFrom(ctx, id, []string{fromPhase}, claimPhase)
}

func (s *Server) claimCloneOperationFrom(ctx context.Context, id string, fromPhases []string, claimPhase string) error {
	return s.claimComputeOperation(ctx, id, computeCloneAction, fromPhases, claimPhase, func(operation *model.Operation) (any, error) {
		payload, err := decodeClonePayload(operation)
		payload.Phase = claimPhase
		return payload, err
	})
}

func (s *Server) terminalizeClaimedCloneOperation(ctx context.Context, id, claimPhase, terminalPhase string) error {
	return s.terminalizeClaimedCloneOperationWithStatus(ctx, id, claimPhase, terminalPhase, model.OperationSucceeded, "")
}

func (s *Server) terminalizeClaimedCloneOperationWithStatus(ctx context.Context, id, claimPhase, terminalPhase string, status model.OperationStatus, failure string) error {
	return s.terminalizeClaimedComputeOperation(ctx, id, computeCloneAction, claimPhase, terminalPhase, status, failure, func(operation *model.Operation) (any, error) {
		payload, err := decodeClonePayload(operation)
		payload.Phase = terminalPhase
		return payload, err
	})
}

func ownedPortManifest(port *model.Port, ownership, allocationID string, allocationRevision int64) computeOwnedPort {
	securityGroups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(securityGroups)
	return computeOwnedPort{
		PortID: port.ID, NIC: port.NIC, MACAddress: strings.ToLower(port.MACAddress),
		FixedIPs: append([]model.FixedIP(nil), port.FixedIPs...), NetworkID: port.NetworkID,
		SecurityGroupIDs: securityGroups, LSPName: port.LSPName, Revision: port.Revision,
		Generation: port.Generation, DetachedRevision: port.Revision + 1, DetachedGeneration: port.Generation + 1,
		AllocationID: allocationID, AllocationRevision: allocationRevision, OwnershipDigest: ownership,
	}
}

func clonePortOwnedByPayload(port *model.Port, payload computeClonePayload, expected computeOwnedPort) bool {
	securityGroups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(securityGroups)
	return port.ID == expected.PortID && port.VMID == payload.TargetVMID && port.NIC == expected.NIC &&
		port.NodeID == payload.TargetNodeID && port.RequestedChassis == payload.Target &&
		port.Revision == expected.Revision && port.Generation == expected.Generation &&
		port.NetworkID == expected.NetworkID && port.LSPName == expected.LSPName &&
		strings.EqualFold(port.MACAddress, expected.MACAddress) && reflect.DeepEqual(port.FixedIPs, expected.FixedIPs) &&
		slices.Equal(securityGroups, expected.SecurityGroupIDs) &&
		(port.BindingStatus == model.PortBinding || port.BindingStatus == model.PortBound)
}

func (s *Server) persistClonePrepared(ctx context.Context, operation *model.Operation, payload computeClonePayload) (*model.Operation, computeClonePayload, error) {
	payload.Phase = "prepared"
	encoded, err := model.MarshalOperationPayload(payload)
	if err != nil {
		return operation, payload, err
	}
	copyResource, err := model.Clone(operation)
	if err != nil {
		return operation, payload, err
	}
	desired := copyResource.(*model.Operation)
	desired.Payload = encoded
	updated, _, err := s.store.Update(ctx, desired, operation.Revision, "compute-clone-prepared-"+operation.ID)
	if err == nil {
		return updated.(*model.Operation), payload, nil
	}
	if !errors.Is(err, controlstore.ErrPrecondition) {
		return operation, payload, err
	}
	current, currentPayload, loadErr := s.loadCloneOperation(ctx, operation.ID)
	if loadErr != nil || currentPayload.Phase != "prepared" || clonePayloadHash(currentPayload) != clonePayloadHash(payload) {
		return operation, payload, computeError(http.StatusConflict, "clone_prepare_conflict", "clone manifest changed concurrently", nil)
	}
	return current, currentPayload, nil
}

var computeLifecycleLocks [256]sync.Mutex

func lockComputeLifecycle(key string) func() {
	digest := sha256.Sum256([]byte(key))
	lock := &computeLifecycleLocks[int(digest[0])]
	lock.Lock()
	return lock.Unlock
}

func computeResourceOperationID(action, lifecycleID string, identities ...int) string {
	parts := make([]string, 0, len(identities)+2)
	parts = append(parts, "pvn-"+action, lifecycleID)
	for _, identity := range identities {
		parts = append(parts, fmt.Sprint(identity))
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, ":")))
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(digest[:16])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func computePayloadHash(payload any) string {
	encoded, err := model.MarshalOperationPayload(payload)
	if err != nil {
		panic("hash validated compute payload: " + err.Error())
	}
	digest := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(digest[:])
}

func (s *Server) createComputeOperation(ctx context.Context, id, action string, vmid int, payload any) (*model.Operation, error) {
	encoded, err := model.MarshalOperationPayload(payload)
	if err != nil {
		return nil, err
	}
	now := s.clusterGate.now().UTC()
	operation := &model.Operation{
		Metadata: model.Metadata{ID: id}, Action: action, TargetKind: model.KindNode, TargetID: computeVMOperationTarget(vmid),
		TargetRevision:  1,
		OperationStatus: model.OperationRunning, IdempotencyKey: action + ":" + id,
		LeaseOwner: "compute-lifecycle", StartedAt: &now, Payload: encoded,
	}
	created, replayed, err := s.store.Create(ctx, operation, operation.IdempotencyKey)
	if err != nil {
		if errors.Is(err, controlstore.ErrAlreadyExists) {
			return nil, computeError(http.StatusConflict, "compute_vm_busy", "VM already has an active compute lifecycle transaction", map[string]any{"vmid": vmid})
		}
		return nil, err
	}
	if replayed {
		return nil, computeError(http.StatusConflict, "lifecycle_operation_conflict", "lifecycle operation appeared concurrently; retry the exact request", map[string]any{"operation_id": id})
	}
	return created.(*model.Operation), nil
}

func computeVMOperationTarget(vmid int) string {
	return "compute-vm:" + fmt.Sprint(vmid)
}

func computeHistoryOperationTarget(activeTarget, id string) string {
	base := activeTarget
	if marker := strings.Index(base, ":history:"); marker >= 0 {
		base = base[:marker]
	}
	return base + ":history:" + id
}

func (s *Server) updateComputeOperation(ctx context.Context, id, action, phase string, status model.OperationStatus, failure string, update func(*model.Operation) (any, error)) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		resource, err := s.store.Get(ctx, model.KindOperation, id)
		if err != nil {
			return err
		}
		current := resource.(*model.Operation)
		if current.Action != action {
			return computeError(http.StatusConflict, "lifecycle_transaction_mismatch", "operation action does not match lifecycle transaction", nil)
		}
		currentPhase, err := computeOperationPhase(current)
		if err != nil {
			return err
		}
		if current.OperationStatus != model.OperationRunning {
			if current.OperationStatus == status && currentPhase == phase {
				return nil
			}
			return computeError(http.StatusConflict, "lifecycle_transaction_terminal", "lifecycle transaction is terminal in a different phase", map[string]any{"phase": currentPhase, "status": current.OperationStatus})
		}
		payload, err := update(current)
		if err != nil {
			return err
		}
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return err
		}
		copyResource, err := model.Clone(current)
		if err != nil {
			return err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload = encoded
		desired.OperationStatus = status
		desired.Error = failure
		if status != model.OperationRunning {
			now := s.clusterGate.now().UTC()
			desired.CompletedAt = &now
			if phase != "recovery-required" {
				desired.TargetID = computeHistoryOperationTarget(current.TargetID, id)
				desired.TargetRevision = 1
			}
		}
		_, _, err = s.store.Update(ctx, desired, current.Revision, action+"-"+id+"-"+phase)
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "lifecycle_update_conflict", "lifecycle transaction changed concurrently", nil)
}

// claimComputeOperation durably chooses one finish direction before that
// direction mutates ports or dependent lifecycle records. A retry of the same
// direction resumes the claim; the opposite direction is rejected before it
// can mutate anything, including when another manager owns that request.
func (s *Server) claimComputeOperation(ctx context.Context, id, action string, fromPhases []string, claimPhase string, update func(*model.Operation) (any, error)) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		resource, err := s.store.Get(ctx, model.KindOperation, id)
		if err != nil {
			return err
		}
		current := resource.(*model.Operation)
		if current.Action != action {
			return computeError(http.StatusConflict, "lifecycle_transaction_mismatch", "operation action does not match lifecycle transaction", nil)
		}
		currentPhase, err := computeOperationPhase(current)
		if err != nil {
			return err
		}
		if current.OperationStatus != model.OperationRunning {
			return computeError(http.StatusConflict, "lifecycle_transaction_terminal", "lifecycle transaction is already terminal", map[string]any{"phase": currentPhase, "status": current.OperationStatus})
		}
		if currentPhase == claimPhase {
			return nil
		}
		if !slices.Contains(fromPhases, currentPhase) {
			return computeError(http.StatusConflict, "lifecycle_transaction_claimed", "lifecycle transaction is claimed by a different action", map[string]any{"phase": currentPhase})
		}
		payload, err := update(current)
		if err != nil {
			return err
		}
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return err
		}
		copyResource, err := model.Clone(current)
		if err != nil {
			return err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload = encoded
		_, _, err = s.store.Update(ctx, desired, current.Revision, action+"-"+id+"-claim-"+claimPhase+"-"+fmt.Sprint(current.Revision))
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "lifecycle_claim_conflict", "lifecycle transaction claim changed concurrently", nil)
}

func (s *Server) terminalizeClaimedComputeOperation(ctx context.Context, id, action, claimPhase, terminalPhase string, status model.OperationStatus, failure string, update func(*model.Operation) (any, error)) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		resource, err := s.store.Get(ctx, model.KindOperation, id)
		if err != nil {
			return err
		}
		current := resource.(*model.Operation)
		if current.Action != action {
			return computeError(http.StatusConflict, "lifecycle_transaction_mismatch", "operation action does not match lifecycle transaction", nil)
		}
		currentPhase, err := computeOperationPhase(current)
		if err != nil {
			return err
		}
		if current.OperationStatus == status && currentPhase == terminalPhase {
			return nil
		}
		if current.OperationStatus != model.OperationRunning || currentPhase != claimPhase {
			return computeError(http.StatusConflict, "lifecycle_transaction_claimed", "lifecycle transaction is not owned by this finish action", map[string]any{"phase": currentPhase, "status": current.OperationStatus})
		}
		payload, err := update(current)
		if err != nil {
			return err
		}
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return err
		}
		copyResource, err := model.Clone(current)
		if err != nil {
			return err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload = encoded
		desired.OperationStatus = status
		desired.Error = failure
		now := s.clusterGate.now().UTC()
		desired.CompletedAt = &now
		if terminalPhase != "recovery-required" {
			desired.TargetID = computeHistoryOperationTarget(current.TargetID, id)
			desired.TargetRevision = 1
		}
		_, _, err = s.store.Update(ctx, desired, current.Revision, action+"-"+id+"-terminal-"+terminalPhase+"-"+fmt.Sprint(current.Revision))
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "lifecycle_update_conflict", "lifecycle transaction changed concurrently", nil)
}

func (s *Server) recordComputeClaimError(ctx context.Context, id, action, claimPhase, failure string) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		resource, err := s.store.Get(ctx, model.KindOperation, id)
		if err != nil {
			return err
		}
		current := resource.(*model.Operation)
		currentPhase, err := computeOperationPhase(current)
		if err != nil {
			return err
		}
		if current.Action != action || current.OperationStatus != model.OperationRunning || currentPhase != claimPhase {
			return computeError(http.StatusConflict, "lifecycle_transaction_claimed", "cannot record recovery state outside its durable action claim", map[string]any{"phase": currentPhase, "status": current.OperationStatus})
		}
		copyResource, err := model.Clone(current)
		if err != nil {
			return err
		}
		desired := copyResource.(*model.Operation)
		desired.Error = failure
		_, _, err = s.store.Update(ctx, desired, current.Revision, action+"-"+id+"-recovery-error-"+fmt.Sprint(current.Revision))
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "lifecycle_update_conflict", "lifecycle recovery state changed concurrently", nil)
}

func computeOperationPhase(operation *model.Operation) (string, error) {
	var envelope map[string]any
	if err := json.Unmarshal([]byte(operation.Payload), &envelope); err != nil {
		return "", computeError(http.StatusConflict, "lifecycle_payload_invalid", "durable lifecycle payload is invalid", map[string]any{"operation_id": operation.ID})
	}
	phase, ok := envelope["phase"].(string)
	if !ok || phase == "" {
		return "", computeError(http.StatusConflict, "lifecycle_payload_invalid", "durable lifecycle payload has no phase", map[string]any{"operation_id": operation.ID})
	}
	return phase, nil
}

func cloneBlueprints(source []computePortBlueprint) []computePortBlueprint {
	result := make([]computePortBlueprint, len(source))
	for index := range source {
		result[index] = source[index]
		result[index].SecurityGroupIDs = append([]string(nil), source[index].SecurityGroupIDs...)
		sort.Strings(result[index].SecurityGroupIDs)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NIC < result[j].NIC })
	return result
}

func blueprintNICsMatch(nics []computeNIC, blueprints []computePortBlueprint) error {
	if len(nics) != len(blueprints) {
		return computeError(http.StatusConflict, "pvn_nic_set_mismatch", "PVE br-int NIC set does not match the durable lifecycle manifest", nil)
	}
	byNIC := make(map[string]computePortBlueprint, len(blueprints))
	for _, blueprint := range blueprints {
		if blueprint.NIC == "" || byNIC[blueprint.NIC].NIC != "" {
			return computeError(http.StatusConflict, "lifecycle_manifest_invalid", "durable lifecycle manifest has duplicate NICs", nil)
		}
		byNIC[blueprint.NIC] = blueprint
	}
	seen := make(map[string]bool, len(nics))
	for _, nic := range nics {
		if seen[nic.NIC] {
			return computeError(http.StatusConflict, "pvn_nic_set_mismatch", "PVE NIC set contains a duplicate interface", map[string]any{"nic": nic.NIC})
		}
		seen[nic.NIC] = true
		blueprint, ok := byNIC[nic.NIC]
		if !ok || !strings.EqualFold(blueprint.SourceMACAddress, nic.MACAddress) {
			return computeError(http.StatusConflict, "pvn_nic_set_mismatch", "PVE NIC identity does not match the durable lifecycle manifest", map[string]any{"nic": nic.NIC})
		}
	}
	return nil
}

func (s *Server) configuredComputeNode(ctx context.Context) (*model.Node, error) {
	if s.computeNode == "" {
		return nil, computeError(http.StatusServiceUnavailable, "compute_identity_unavailable", "manager has no configured local compute-node identity", nil)
	}
	return s.localComputeNode(ctx, s.computeNode)
}

func (s *Server) sourcePortManifest(ctx context.Context, port *model.Port) (computeSourcePort, error) {
	securityGroups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(securityGroups)
	allocationIDs, allocationRevisions, err := s.sourcePortAllocationManifest(ctx, port)
	if err != nil {
		return computeSourcePort{}, err
	}
	return computeSourcePort{
		PortID: port.ID, NIC: port.NIC, MACAddress: strings.ToLower(port.MACAddress),
		FixedIPs: append([]model.FixedIP(nil), port.FixedIPs...), NetworkID: port.NetworkID,
		SecurityGroupIDs: securityGroups, AllocationIDs: allocationIDs, AllocationRevisions: allocationRevisions, LSPName: port.LSPName, SourceNodeID: port.NodeID,
		SourceChassis: port.RequestedChassis, SourceStatus: port.BindingStatus,
		SourceRevision: port.Revision, SourceGeneration: port.Generation,
		DetachedRevision: port.Revision + 1, DetachedGeneration: port.Generation + 1,
		RestoredRevision: port.Revision + 2, RestoredGeneration: port.Generation + 2,
	}, nil
}

func (s *Server) sourcePortAllocationManifest(ctx context.Context, port *model.Port) ([]string, []int64, error) {
	resources, err := s.store.List(ctx, model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	type allocationVersion struct {
		id       string
		revision int64
	}
	versions := make([]allocationVersion, 0, len(port.FixedIPs))
	for _, resource := range resources {
		allocation := resource.(*model.IPAllocation)
		if allocation.PortID != port.ID {
			continue
		}
		if allocation.Metadata.State == model.ResourceDeleting || allocation.Metadata.State == model.ResourceError {
			return nil, nil, computeError(http.StatusConflict, "source_allocation_drift", "source port allocation is in a deletion tombstone state", map[string]any{"port_id": port.ID, "allocation_id": allocation.ID})
		}
		matched := false
		for _, fixed := range port.FixedIPs {
			if allocation.State == model.IPAllocated && allocation.SubnetID == fixed.SubnetID && allocation.Address == fixed.Address {
				matched = true
				break
			}
		}
		if !matched {
			return nil, nil, computeError(http.StatusConflict, "source_allocation_drift", "source port allocation does not match its fixed IP manifest", map[string]any{"port_id": port.ID, "allocation_id": allocation.ID})
		}
		versions = append(versions, allocationVersion{id: allocation.ID, revision: allocation.Revision})
	}
	if len(versions) != len(port.FixedIPs) {
		return nil, nil, computeError(http.StatusConflict, "source_allocation_drift", "source port fixed IPs do not have an exact allocation bijection", map[string]any{"port_id": port.ID})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].id < versions[j].id })
	ids, revisions := make([]string, len(versions)), make([]int64, len(versions))
	for index, version := range versions {
		ids[index], revisions[index] = version.id, version.revision
	}
	return ids, revisions, nil
}

func sourcePortImmutableMatches(port *model.Port, expected computeSourcePort) bool {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	return port.ID == expected.PortID && port.NetworkID == expected.NetworkID && port.LSPName == expected.LSPName &&
		strings.EqualFold(port.MACAddress, expected.MACAddress) && reflect.DeepEqual(port.FixedIPs, expected.FixedIPs) &&
		slices.Equal(groups, expected.SecurityGroupIDs)
}

func sourcePortAttached(port *model.Port, vmid int, expected computeSourcePort, restored bool) bool {
	revision, generation := expected.SourceRevision, expected.SourceGeneration
	if restored {
		revision, generation = expected.RestoredRevision, expected.RestoredGeneration
	}
	return sourcePortImmutableMatches(port, expected) && port.VMID == vmid && port.NIC == expected.NIC &&
		port.NodeID == expected.SourceNodeID && port.RequestedChassis == expected.SourceChassis &&
		port.BindingStatus == expected.SourceStatus && port.Revision == revision && port.Generation == generation
}

func sourcePortDetached(port *model.Port, expected computeSourcePort) bool {
	return sourcePortDetachedShape(port, expected) &&
		port.Revision == expected.DetachedRevision && port.Generation == expected.DetachedGeneration
}

func sourcePortDetachedShape(port *model.Port, expected computeSourcePort) bool {
	return sourcePortImmutableMatches(port, expected) && portCanBeDeprovisioned(port) && port.Generation == expected.DetachedGeneration
}

func validateSourcePortLifecycle(port *model.Port) error {
	if port.Revision > math.MaxInt64-2 || port.Generation > math.MaxInt64-2 {
		return computeError(http.StatusConflict, "source_port_counter_exhausted", "source port counters cannot represent lifecycle detach and restore states", computePortDetails(port))
	}
	return nil
}

func sourcePortRestored(port *model.Port, vmid int, expected computeSourcePort) bool {
	return sourcePortAttached(port, vmid, expected, true)
}

func (s *Server) detachSourcePorts(ctx context.Context, vmid int, ports []computeSourcePort, key string) error {
	for _, expected := range ports {
		port, err := s.loadPort(ctx, expected.PortID)
		if err != nil {
			return err
		}
		if sourcePortDetached(port, expected) {
			if err := s.validateSourceAllocations(ctx, port, expected, false); err != nil {
				return err
			}
			if _, err := s.forceRealizeComputePort(ctx, port); err != nil {
				return err
			}
			continue
		}
		if !sourcePortAttached(port, vmid, expected, false) {
			return computeError(http.StatusConflict, "source_port_drift", "source VM port changed outside the lifecycle transaction", computePortDetails(port))
		}
		if err := s.validateSourceAllocations(ctx, port, expected, false); err != nil {
			return err
		}
		desired := clonePort(port)
		desired.Metadata = model.Metadata{ID: port.ID}
		desired.NodeID, desired.VMID, desired.NIC, desired.RequestedChassis = "", 0, "", ""
		desired.BindingStatus, desired.Generation = model.PortUnbound, expected.DetachedGeneration
		updated, _, err := s.store.Update(ctx, desired, port.Revision, key+"-detach-"+port.ID)
		if err != nil {
			return err
		}
		if updated.GetMetadata().Revision != expected.DetachedRevision {
			return computeError(http.StatusConflict, "source_port_revision_drift", "detached port revision differs from its durable manifest", computePortDetails(updated.(*model.Port)))
		}
		if _, err := s.forceRealizeComputePort(ctx, updated.(*model.Port)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) restoreSourcePorts(ctx context.Context, vmid int, ports []computeSourcePort, key string) []string {
	failures := make([]string, 0)
	for _, expected := range ports {
		port, err := s.loadPort(ctx, expected.PortID)
		if err == nil && sourcePortAttached(port, vmid, expected, false) {
			err = s.validateSourceAllocations(ctx, port, expected, false)
			if err == nil {
				_, err = s.forceRealizeComputePort(ctx, port)
			}
		} else if err == nil && sourcePortRestored(port, vmid, expected) {
			err = s.validateSourceAllocations(ctx, port, expected, false)
			if err == nil {
				_, err = s.forceRealizeComputePort(ctx, port)
			}
		} else if err == nil && sourcePortDetached(port, expected) {
			err = s.validateSourceAllocations(ctx, port, expected, false)
			if err != nil {
				failures = append(failures, expected.PortID+": "+err.Error())
				continue
			}
			desired := clonePort(port)
			desired.Metadata = model.Metadata{ID: port.ID}
			desired.NodeID, desired.VMID, desired.NIC = expected.SourceNodeID, vmid, expected.NIC
			desired.RequestedChassis, desired.BindingStatus = expected.SourceChassis, expected.SourceStatus
			desired.Generation = expected.RestoredGeneration
			var updated model.Resource
			updated, _, err = s.store.Update(ctx, desired, port.Revision, key+"-restore-"+port.ID)
			if err == nil && updated.GetMetadata().Revision != expected.RestoredRevision {
				err = computeError(http.StatusConflict, "source_port_revision_drift", "restored port revision differs from its durable manifest", computePortDetails(updated.(*model.Port)))
			}
			if err == nil {
				_, err = s.forceRealizeComputePort(ctx, updated.(*model.Port))
			}
		} else if err == nil {
			err = computeError(http.StatusConflict, "source_port_drift", "refusing to restore a port outside its durable lifecycle manifest", computePortDetails(port))
		}
		if err != nil {
			failures = append(failures, expected.PortID+": "+err.Error())
		}
	}
	return failures
}

func (s *Server) verifySourceRestorePortSet(ctx context.Context, vmid int, ports []computeSourcePort, phase string) error {
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: vmid})
	if err != nil {
		return err
	}
	owned := make(map[string]computeSourcePort, len(ports))
	for _, expected := range ports {
		owned[expected.PortID] = expected
	}
	for _, resource := range resources {
		port := resource.(*model.Port)
		expected, ok := owned[port.ID]
		if !ok || port.NIC != expected.NIC {
			return computeError(http.StatusConflict, "lifecycle_port_set_drift", "VM acquired a PVN-managed port outside the "+phase+" transaction", map[string]any{"vmid": vmid, "port_id": port.ID})
		}
	}
	return nil
}

func (s *Server) deleteDetachedSourcePorts(ctx context.Context, ports []computeSourcePort, key string) []string {
	failures := make([]string, 0)
	for _, expected := range ports {
		port, err := s.loadPort(ctx, expected.PortID)
		if errors.Is(err, controlstore.ErrNotFound) {
			continue
		}
		if err == nil {
			err = s.ensureNoPortDeprovisionDependents(ctx, port.ID)
		}
		if err == nil && (port.Metadata.State == model.ResourceDeleting || port.Metadata.State == model.ResourceError) && !sourcePortDetached(port, expected) {
			if !sourcePortDetachedShape(port, expected) || port.Revision != expected.DetachedRevision+1 {
				err = computeError(http.StatusConflict, "source_port_ownership_mismatch", "refusing to resume deletion outside its durable lifecycle manifest", computePortDetails(port))
			} else {
				err = s.validateSourceAllocations(ctx, port, expected, true)
				if err == nil {
					err = s.reconcileDeprovisionDelete(ctx, port)
				}
				if err == nil {
					err = s.store.Purge(ctx, model.KindPort, port.ID, port.Revision)
				}
			}
			if err != nil {
				failures = append(failures, expected.PortID+": "+err.Error())
			}
			continue
		}
		if err == nil && !sourcePortDetached(port, expected) {
			err = computeError(http.StatusConflict, "source_port_ownership_mismatch", "refusing to delete a port outside its durable lifecycle manifest", computePortDetails(port))
		}
		if err == nil {
			err = s.validateSourceAllocations(ctx, port, expected, true)
		}
		if err == nil {
			err = s.releasePortAllocations(ctx, port, key)
		}
		if err == nil {
			var tombstone model.Resource
			tombstone, _, err = s.store.BeginDelete(ctx, model.KindPort, port.ID, port.Revision, key+"-delete-"+port.ID)
			if err == nil {
				err = s.reconcileDeprovisionDelete(ctx, tombstone)
			}
			if err == nil {
				err = s.store.Purge(ctx, model.KindPort, port.ID, tombstone.GetMetadata().Revision)
			}
		}
		if err != nil {
			failures = append(failures, expected.PortID+": "+err.Error())
		}
	}
	return failures
}

func (s *Server) validateSourceAllocations(ctx context.Context, port *model.Port, expected computeSourcePort, allowMissing bool) error {
	if len(expected.AllocationIDs) != len(expected.AllocationRevisions) {
		return computeError(http.StatusConflict, "source_allocation_drift", "durable source allocation revisions are incomplete", map[string]any{"port_id": port.ID})
	}
	wantedRevision := make(map[string]int64, len(expected.AllocationIDs))
	for index, id := range expected.AllocationIDs {
		wantedRevision[id] = expected.AllocationRevisions[index]
		resource, err := s.store.Get(ctx, model.KindIPAllocation, id)
		if errors.Is(err, controlstore.ErrNotFound) && allowMissing {
			continue
		}
		if err != nil {
			return computeError(http.StatusConflict, "source_allocation_drift", "source lifecycle allocation is missing", map[string]any{"port_id": port.ID, "allocation_id": id})
		}
		allocation := resource.(*model.IPAllocation)
		matched := false
		for _, fixed := range expected.FixedIPs {
			if allocation.State == model.IPAllocated && allocation.PortID == port.ID && allocation.SubnetID == fixed.SubnetID && allocation.Address == fixed.Address {
				matched = true
				break
			}
		}
		if !matched || !lifecycleAllocationRevisionMatches(allocation, expected.AllocationRevisions[index], allowMissing) {
			return computeError(http.StatusConflict, "source_allocation_drift", "durable source allocation exists but no longer belongs to its port", map[string]any{"port_id": port.ID, "allocation_id": id})
		}
	}
	resources, err := s.store.List(ctx, model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(expected.AllocationIDs))
	for _, id := range expected.AllocationIDs {
		wanted[id] = true
	}
	found := make(map[string]bool, len(wanted))
	for _, resource := range resources {
		allocation := resource.(*model.IPAllocation)
		if allocation.PortID != port.ID {
			continue
		}
		if !wanted[allocation.ID] {
			return computeError(http.StatusConflict, "source_allocation_drift", "source port has an allocation outside its durable lifecycle manifest", map[string]any{"allocation_id": allocation.ID})
		}
		if !lifecycleAllocationRevisionMatches(allocation, wantedRevision[allocation.ID], allowMissing) {
			return computeError(http.StatusConflict, "source_allocation_drift", "source allocation revision differs from its durable lifecycle manifest", map[string]any{"allocation_id": allocation.ID})
		}
		matched := false
		for _, fixed := range expected.FixedIPs {
			if allocation.State == model.IPAllocated && allocation.SubnetID == fixed.SubnetID && allocation.Address == fixed.Address {
				matched = true
				break
			}
		}
		if !matched {
			return computeError(http.StatusConflict, "source_allocation_drift", "source allocation differs from its durable lifecycle manifest", map[string]any{"allocation_id": allocation.ID})
		}
		found[allocation.ID] = true
	}
	if !allowMissing && len(found) != len(wanted) {
		return computeError(http.StatusConflict, "source_allocation_drift", "source lifecycle allocation is missing", map[string]any{"port_id": port.ID})
	}
	return nil
}

func lifecycleAllocationRevisionMatches(allocation *model.IPAllocation, expected int64, allowDeleting bool) bool {
	if allocation.Metadata.State == model.ResourceDeleting || allocation.Metadata.State == model.ResourceError {
		return allowDeleting && allocation.Revision == expected+1
	}
	return allocation.Revision == expected
}

func (s *Server) failSourcePrepare(ctx context.Context, operation *model.Operation, vmid int, ports []computeSourcePort, action, lifecycleID string, cause error) error {
	recoveryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), computeRecoveryTimeout)
	defer cancel()
	var claimErr error
	switch action {
	case computeTemplateAction:
		claimErr = s.claimTemplateOperationFrom(recoveryContext, operation.ID, []string{"prepared"}, "compensating")
	case computeDestroyAction:
		claimErr = s.claimDestroyOperationFrom(recoveryContext, operation.ID, []string{"captured"}, "compensating")
	default:
		claimErr = fmt.Errorf("unsupported source prepare action %q", action)
	}
	if claimErr != nil {
		return claimErr
	}
	failures := s.restoreSourcePorts(recoveryContext, vmid, ports, action+"-compensate-"+lifecycleID)
	if len(failures) != 0 {
		if err := s.recordComputeClaimError(recoveryContext, operation.ID, action, "compensating", cause.Error()); err != nil {
			failures = append(failures, err.Error())
		}
		return computeError(http.StatusServiceUnavailable, action+"_prepare_failed", cause.Error(), map[string]any{"operation_id": operation.ID, "recovery_required": true, "compensation_errors": failures})
	}
	var terminalErr error
	switch action {
	case computeTemplateAction:
		terminalErr = s.terminalizeClaimedTemplateOperationWithStatus(recoveryContext, operation.ID, "compensating", "compensated", model.OperationFailed, cause.Error())
	case computeDestroyAction:
		terminalErr = s.terminalizeClaimedDestroyOperationWithStatus(recoveryContext, operation.ID, "compensating", "compensated", model.OperationFailed, cause.Error())
	default:
		terminalErr = fmt.Errorf("unsupported source prepare action %q", action)
	}
	if terminalErr != nil {
		failures = append(failures, terminalErr.Error())
	}
	return computeError(http.StatusServiceUnavailable, action+"_prepare_failed", cause.Error(), map[string]any{"operation_id": operation.ID, "recovery_required": len(failures) != 0, "compensation_errors": failures})
}

func (s *Server) computeTemplatePrepare(writer http.ResponseWriter, request *http.Request) {
	var input computeTemplatePrepareRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := validateComputeVM(input.VMID, input.NICs); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	node, err := s.configuredComputeNode(request.Context())
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	operationID := computeResourceOperationID(computeTemplateAction, input.LifecycleID, input.VMID)
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	if resource, getErr := s.store.Get(request.Context(), model.KindOperation, operationID); getErr == nil {
		operation := resource.(*model.Operation)
		payload, err := decodeTemplatePayload(operation)
		if err != nil || !templateRequestMatches(input, node, payload) {
			if err == nil {
				err = computeError(http.StatusConflict, "template_id_conflict", "lifecycle_id was reused with different template parameters", nil)
			}
			s.writeComputeError(writer, err)
			return
		}
		if operation.OperationStatus == model.OperationRunning && payload.Phase == "compensating" {
			cause := operation.Error
			if cause == "" {
				cause = "resuming interrupted template prepare compensation"
			}
			s.writeComputeError(writer, s.failSourcePrepare(request.Context(), operation, payload.VMID, payload.Ports, computeTemplateAction, payload.LifecycleID, errors.New(cause)))
			return
		}
		if operation.OperationStatus != model.OperationRunning || payload.Phase != "prepared" {
			s.writeComputeError(writer, computeError(http.StatusConflict, "template_transaction_terminal", "template transaction is already terminal", map[string]any{"phase": payload.Phase}))
			return
		}
		if err := s.detachSourcePorts(request.Context(), payload.VMID, payload.Ports, "compute-template-"+payload.LifecycleID); err != nil {
			s.writeComputeError(writer, s.failSourcePrepare(request.Context(), operation, payload.VMID, payload.Ports, computeTemplateAction, payload.LifecycleID, err))
			return
		}
		writer.Header().Set("Idempotency-Replayed", "true")
		writeJSON(writer, http.StatusOK, map[string]any{"data": templateTransaction(operation.ID, payload)})
		return
	} else if !errors.Is(getErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, getErr)
		return
	}
	if existing, _, findErr := s.findTemplateRecord(request.Context(), input.VMID, false); findErr == nil && existing != nil {
		s.writeComputeError(writer, computeError(http.StatusConflict, "template_manifest_exists", "VM already has a durable template lifecycle manifest", map[string]any{"vmid": input.VMID}))
		return
	} else if findErr != nil && !errors.Is(findErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, findErr)
		return
	}
	ports, err := s.loadExactComputePorts(request.Context(), input.VMID, input.NICs)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	for _, port := range ports {
		if err := validateSourcePortLifecycle(port); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		if err := s.ensureNoPortDeprovisionDependents(request.Context(), port.ID); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		if port.NodeID != node.ID || port.RequestedChassis != node.ChassisID || (port.BindingStatus != model.PortBinding && port.BindingStatus != model.PortBound) {
			s.writeComputeError(writer, computeError(http.StatusConflict, "template_source_not_local", "template source port is not attached to the local compute node", computePortDetails(port)))
			return
		}
	}
	blueprints, err := blueprintsFromPorts(ports)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	payload := computeTemplatePayload{Version: computePayloadVersion, LifecycleID: input.LifecycleID, Phase: "prepared", VMID: input.VMID, NodeID: node.ID, Node: node.Name, Chassis: node.ChassisID, Blueprints: blueprints}
	for _, port := range ports {
		manifest, err := s.sourcePortManifest(request.Context(), port)
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
		payload.Ports = append(payload.Ports, manifest)
	}
	operation, err := s.createComputeOperation(request.Context(), operationID, computeTemplateAction, input.VMID, payload)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := s.detachSourcePorts(request.Context(), payload.VMID, payload.Ports, "compute-template-"+payload.LifecycleID); err != nil {
		s.writeComputeError(writer, s.failSourcePrepare(request.Context(), operation, payload.VMID, payload.Ports, computeTemplateAction, payload.LifecycleID, err))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": templateTransaction(operation.ID, payload)})
}

func (s *Server) computeTemplateFinish(writer http.ResponseWriter, request *http.Request, abort bool) {
	var input computeTemplateTransaction
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil || input.VMID < 1 || input.OperationID == "" || input.PayloadHash == "" || len(input.Ports) == 0 {
		if err == nil {
			err = computeError(http.StatusBadRequest, "invalid_request", "complete template transaction echo is required", nil)
		}
		s.writeComputeError(writer, err)
		return
	}
	if input.OperationID != computeResourceOperationID(computeTemplateAction, input.LifecycleID, input.VMID) {
		s.writeComputeError(writer, computeError(http.StatusConflict, "template_transaction_mismatch", "template operation_id does not match its identity", nil))
		return
	}
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	operation, payload, err := s.loadTemplateOperation(request.Context(), input.OperationID)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if !reflect.DeepEqual(input, templateTransaction(operation.ID, payload)) {
		s.writeComputeError(writer, computeError(http.StatusConflict, "template_transaction_mismatch", "template transaction echo is incomplete, reordered, or stale", nil))
		return
	}
	local, err := s.localComputeNode(request.Context(), payload.Node)
	if err != nil || local.ID != payload.NodeID || local.ChassisID != payload.Chassis {
		if err == nil {
			err = computeError(http.StatusConflict, "template_source_drift", "template source node identity changed after prepare", nil)
		}
		s.writeComputeError(writer, err)
		return
	}
	phase, claimPhase := "committed", "committing"
	if abort {
		phase, claimPhase = "aborted", "aborting"
	}
	if operation.OperationStatus != model.OperationRunning {
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == phase {
			writer.Header().Set("Idempotency-Replayed", "true")
			writeJSON(writer, http.StatusOK, map[string]any{"data": input})
			return
		}
		s.writeComputeError(writer, computeError(http.StatusConflict, "template_transaction_terminal", "template transaction is terminal in a different phase", map[string]any{"phase": payload.Phase}))
		return
	}
	if payload.Phase != "prepared" && payload.Phase != claimPhase {
		s.writeComputeError(writer, computeError(http.StatusConflict, "template_transaction_terminal", "template transaction is claimed by a different action", map[string]any{"phase": payload.Phase}))
		return
	}
	if err := s.claimTemplateOperation(request.Context(), operation.ID, "prepared", claimPhase); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if abort {
		if err := s.verifySourceRestorePortSet(request.Context(), payload.VMID, payload.Ports, "template abort"); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		if failures := s.restoreSourcePorts(request.Context(), payload.VMID, payload.Ports, "compute-template-abort-"+payload.LifecycleID); len(failures) != 0 {
			s.writeComputeError(writer, computeError(http.StatusServiceUnavailable, "template_abort_failed", "template ports require recovery", map[string]any{"recovery_required": true, "errors": failures}))
			return
		}
	} else if err := s.requireVMPortSetEmpty(request.Context(), payload.VMID, "template commit"); err != nil {
		s.writeComputeError(writer, err)
		return
	} else if failures := s.deleteDetachedSourcePorts(request.Context(), payload.Ports, "compute-template-commit-"+payload.LifecycleID); len(failures) != 0 {
		s.writeComputeError(writer, computeError(http.StatusServiceUnavailable, "template_commit_failed", "template ports require recovery", map[string]any{"recovery_required": true, "errors": failures}))
		return
	}
	if err := s.terminalizeClaimedTemplateOperation(request.Context(), operation.ID, claimPhase, phase); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": input})
}

func templateRequestMatches(input computeTemplatePrepareRequest, node *model.Node, payload computeTemplatePayload) bool {
	return payload.Version == computePayloadVersion && payload.LifecycleID == input.LifecycleID && payload.VMID == input.VMID &&
		payload.NodeID == node.ID && payload.Node == node.Name && payload.Chassis == node.ChassisID && blueprintNICsMatch(input.NICs, payload.Blueprints) == nil
}

func templateTransaction(operationID string, payload computeTemplatePayload) computeTemplateTransaction {
	return computeTemplateTransaction{LifecycleID: payload.LifecycleID, VMID: payload.VMID, Node: payload.Node, OperationID: operationID, PayloadHash: templatePayloadHash(payload), Ports: append([]computeSourcePort(nil), payload.Ports...)}
}

func templatePayloadHash(payload computeTemplatePayload) string {
	payload.Phase = ""
	return computePayloadHash(payload)
}

func decodeTemplatePayload(operation *model.Operation) (computeTemplatePayload, error) {
	var payload computeTemplatePayload
	if operation.Action != computeTemplateAction || model.UnmarshalOperationPayload(operation.Payload, &payload) != nil || payload.Version != computePayloadVersion || payload.LifecycleID == "" || payload.VMID < 1 || payload.NodeID == "" || payload.Chassis == "" || len(payload.Ports) == 0 || len(payload.Ports) != len(payload.Blueprints) {
		return payload, computeError(http.StatusConflict, "template_payload_invalid", "durable template payload is invalid", map[string]any{"operation_id": operation.ID})
	}
	return payload, nil
}

func (s *Server) loadTemplateOperation(ctx context.Context, id string) (*model.Operation, computeTemplatePayload, error) {
	resource, err := s.store.Get(ctx, model.KindOperation, id)
	if err != nil {
		return nil, computeTemplatePayload{}, err
	}
	operation := resource.(*model.Operation)
	payload, err := decodeTemplatePayload(operation)
	return operation, payload, err
}

func (s *Server) claimTemplateOperation(ctx context.Context, id, fromPhase, claimPhase string) error {
	return s.claimTemplateOperationFrom(ctx, id, []string{fromPhase}, claimPhase)
}

func (s *Server) claimTemplateOperationFrom(ctx context.Context, id string, fromPhases []string, claimPhase string) error {
	return s.claimComputeOperation(ctx, id, computeTemplateAction, fromPhases, claimPhase, func(operation *model.Operation) (any, error) {
		payload, err := decodeTemplatePayload(operation)
		payload.Phase = claimPhase
		return payload, err
	})
}

func (s *Server) terminalizeClaimedTemplateOperation(ctx context.Context, id, claimPhase, terminalPhase string) error {
	return s.terminalizeClaimedTemplateOperationWithStatus(ctx, id, claimPhase, terminalPhase, model.OperationSucceeded, "")
}

func (s *Server) terminalizeClaimedTemplateOperationWithStatus(ctx context.Context, id, claimPhase, terminalPhase string, status model.OperationStatus, failure string) error {
	return s.terminalizeClaimedComputeOperation(ctx, id, computeTemplateAction, claimPhase, terminalPhase, status, failure, func(operation *model.Operation) (any, error) {
		payload, err := decodeTemplatePayload(operation)
		payload.Phase = terminalPhase
		return payload, err
	})
}

func (s *Server) computeSnapshotCreate(writer http.ResponseWriter, request *http.Request) {
	var input computeSnapshotCreateRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := validateLifecycleID("snapshot_id", input.SnapshotID); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if input.SnapshotEpoch < 1 {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "positive snapshot_epoch is required", nil))
		return
	}
	if err := validateComputeVM(input.VMID, input.NICs); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	node, err := s.configuredComputeNode(request.Context())
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	operationID := computeSnapshotOperationID(input.VMID, input.SnapshotID, input.SnapshotEpoch)
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	if resource, getErr := s.store.Get(request.Context(), model.KindOperation, operationID); getErr == nil {
		operation := resource.(*model.Operation)
		payload, err := decodeSnapshotPayload(operation)
		if err == nil && payload.VMID == input.VMID && payload.SnapshotID == input.SnapshotID && payload.SnapshotEpoch == input.SnapshotEpoch && payload.LifecycleID != input.LifecycleID {
			s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_epoch_conflict", "snapshot name and epoch were already used by another lifecycle", map[string]any{"vmid": input.VMID, "snapshot_id": input.SnapshotID, "snapshot_epoch": input.SnapshotEpoch}))
			return
		}
		if err != nil || !snapshotCreateMatches(input, node, payload) {
			if err == nil {
				err = computeError(http.StatusConflict, "snapshot_id_conflict", "lifecycle_id was reused with different snapshot parameters", nil)
			}
			s.writeComputeError(writer, err)
			return
		}
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "created" {
			writer.Header().Set("Idempotency-Replayed", "true")
			writeJSON(writer, http.StatusOK, map[string]any{"data": snapshotTransaction(operation.ID, payload)})
			return
		}
		if operation.OperationStatus != model.OperationRunning || payload.Phase != "creating" {
			s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_transaction_terminal", "snapshot lifecycle is terminal in a different phase", map[string]any{"phase": payload.Phase}))
			return
		}
		if err := s.verifySnapshotPorts(request.Context(), payload); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		if err := s.terminalizeClaimedSnapshotOperation(request.Context(), operation.ID, "creating", "created", model.OperationSucceeded, ""); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		payload.Phase = "created"
		writer.Header().Set("Idempotency-Replayed", "true")
		writeJSON(writer, http.StatusOK, map[string]any{"data": snapshotTransaction(operation.ID, payload)})
		return
	} else if !errors.Is(getErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, getErr)
		return
	}
	if existing, _, findErr := s.findSnapshotRecord(request.Context(), input.VMID, input.SnapshotID, input.SnapshotEpoch); findErr == nil && existing != nil {
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_epoch_conflict", "snapshot name and epoch were already used; retry after PVE assigns a later epoch", map[string]any{"vmid": input.VMID, "snapshot_id": input.SnapshotID, "snapshot_epoch": input.SnapshotEpoch}))
		return
	} else if findErr != nil && !errors.Is(findErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, findErr)
		return
	}
	if existing, _, findErr := s.findActiveSnapshotByName(request.Context(), input.VMID, input.SnapshotID); findErr == nil && existing != nil {
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_manifest_exists", "snapshot already has a durable manifest", map[string]any{"vmid": input.VMID, "snapshot_id": input.SnapshotID}))
		return
	} else if findErr != nil && !errors.Is(findErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, findErr)
		return
	}
	ports, err := s.loadExactComputePorts(request.Context(), input.VMID, input.NICs)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	payload := computeSnapshotPayload{Version: computePayloadVersion, LifecycleID: input.LifecycleID, Phase: "creating", VMID: input.VMID, SnapshotID: input.SnapshotID, SnapshotEpoch: input.SnapshotEpoch, NodeID: node.ID, Node: node.Name, Chassis: node.ChassisID}
	for _, port := range ports {
		if port.NodeID != node.ID || port.RequestedChassis != node.ChassisID {
			s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_source_not_local", "snapshot port is not attached to the local compute node", computePortDetails(port)))
			return
		}
		manifest, err := s.snapshotPortManifest(request.Context(), port)
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
		payload.Ports = append(payload.Ports, manifest)
	}
	payload.Blueprints, err = blueprintsFromSnapshot(payload.Ports)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	operation, err := s.createComputeOperation(request.Context(), operationID, computeSnapshotAction, input.VMID, payload)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if existing, _, findErr := s.findActiveSnapshotByNameExcept(request.Context(), input.VMID, input.SnapshotID, operation.ID); findErr == nil && existing != nil {
		conflict := computeError(http.StatusConflict, "snapshot_manifest_exists", "snapshot name already has an active durable manifest", map[string]any{"vmid": input.VMID, "snapshot_id": input.SnapshotID})
		if terminalErr := s.terminalizeClaimedSnapshotOperation(request.Context(), operation.ID, "creating", "conflicted", model.OperationFailed, conflict.Error()); terminalErr != nil {
			s.writeComputeError(writer, terminalErr)
			return
		}
		s.writeComputeError(writer, conflict)
		return
	} else if findErr != nil && !errors.Is(findErr, controlstore.ErrNotFound) {
		_ = s.recordComputeClaimError(request.Context(), operation.ID, computeSnapshotAction, "creating", findErr.Error())
		s.writeComputeError(writer, findErr)
		return
	}
	if err := s.verifySnapshotPorts(request.Context(), payload); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := s.terminalizeClaimedSnapshotOperation(request.Context(), operation.ID, "creating", "created", model.OperationSucceeded, ""); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	payload.Phase = "created"
	writeJSON(writer, http.StatusOK, map[string]any{"data": snapshotTransaction(operation.ID, payload)})
}

// computeSnapshotCleanup fences an exact PVE snapshot generation after PVE
// has rolled back a failed/ambiguous post-create callback and removed the
// physical snapshot. It never inspects or mutates VM ports and cannot affect a
// later epoch with the same user-visible snapshot name.
func (s *Server) computeSnapshotCleanup(writer http.ResponseWriter, request *http.Request) {
	var input computeSnapshotCleanupRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if input.VMID < 1 || validateLifecycleID("snapshot_id", input.SnapshotID) != nil || input.SnapshotEpoch < 1 {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "vmid, snapshot_id, and positive snapshot_epoch are required", nil))
		return
	}
	local, err := s.configuredComputeNode(request.Context())
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	operationID := computeSnapshotOperationID(input.VMID, input.SnapshotID, input.SnapshotEpoch)
	operation, payload, err := s.loadSnapshotOperation(request.Context(), operationID)
	if errors.Is(err, controlstore.ErrNotFound) {
		payload = computeSnapshotPayload{Version: computePayloadVersion, LifecycleID: "cleanup-fence", Phase: "cleaning", VMID: input.VMID, SnapshotID: input.SnapshotID, SnapshotEpoch: input.SnapshotEpoch, NodeID: local.ID, Node: local.Name, Chassis: local.ChassisID}
		operation, err = s.createComputeOperation(request.Context(), operationID, computeSnapshotAction, input.VMID, payload)
	}
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if payload.VMID != input.VMID || payload.SnapshotID != input.SnapshotID || payload.SnapshotEpoch != input.SnapshotEpoch {
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_cleanup_mismatch", "snapshot cleanup identity does not match its durable manifest", nil))
		return
	}
	if payload.NodeID != local.ID || payload.Node != local.Name || payload.Chassis != local.ChassisID {
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_cleanup_wrong_node", "snapshot cleanup may run only on the node that issued the post-create callback", map[string]any{"snapshot_node": payload.Node, "local_node": local.Name}))
		return
	}
	if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "deleted" {
		writer.Header().Set("Idempotency-Replayed", "true")
		writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"vmid": input.VMID, "snapshot_id": input.SnapshotID, "snapshot_epoch": input.SnapshotEpoch, "state": "deleted"}})
		return
	}
	if operation.OperationStatus != model.OperationRunning && !(operation.OperationStatus == model.OperationSucceeded && payload.Phase == "created") && !(operation.OperationStatus == model.OperationFailed && payload.Phase == "conflicted") {
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_cleanup_unavailable", "snapshot manifest cannot be cleaned from its current phase", map[string]any{"phase": payload.Phase, "status": operation.OperationStatus}))
		return
	}
	if payload.Phase != "cleaning" {
		if err := s.claimSnapshotOperation(request.Context(), operation.ID, input.VMID, []string{"creating", "created", "conflicted"}, "cleaning"); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	}
	if err := s.terminalizeClaimedSnapshotOperation(request.Context(), operation.ID, "cleaning", "deleted", model.OperationSucceeded, ""); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"vmid": input.VMID, "snapshot_id": input.SnapshotID, "snapshot_epoch": input.SnapshotEpoch, "state": "deleted"}})
}

func (s *Server) computeSnapshotPrepare(writer http.ResponseWriter, request *http.Request) {
	var input computeSnapshotMutationRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if input.VMID < 1 || validateLifecycleID("snapshot_id", input.SnapshotID) != nil || input.SnapshotEpoch < 1 || (input.Action != "rollback" && input.Action != "delete") {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "lifecycle_id, action, vmid, snapshot_id, and snapshot_epoch are required", nil))
		return
	}
	if input.Action == "rollback" {
		if err := validateComputeVM(input.VMID, input.NICs); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	} else if len(input.NICs) != 0 {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "snapshot delete prepare must not include live NIC state", nil))
		return
	}
	node, err := s.configuredComputeNode(request.Context())
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	operationID := computeSnapshotMutationOperationID(input)
	if existing, getErr := s.store.Get(request.Context(), model.KindOperation, operationID); getErr == nil {
		operation := existing.(*model.Operation)
		payload, err := decodeSnapshotMutationPayload(operation)
		if err != nil || !snapshotMutationRequestMatches(input, node, payload) {
			if err == nil {
				err = computeError(http.StatusConflict, "snapshot_transaction_conflict", "snapshot lifecycle_id was reused with different parameters", nil)
			}
			s.writeComputeError(writer, err)
			return
		}
		if operation.OperationStatus != model.OperationRunning || payload.Phase != "prepared" {
			s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_transaction_terminal", "snapshot transition is already terminal or claimed", map[string]any{"phase": payload.Phase}))
			return
		}
		if err := s.verifySnapshotMutationManifest(request.Context(), payload); err != nil {
			_ = s.terminalizeClaimedSnapshotMutation(request.Context(), operation.ID, "prepared", "compensated", model.OperationFailed, err.Error())
			s.writeComputeError(writer, err)
			return
		}
		if input.Action == "rollback" {
			_, manifestPayload, loadErr := s.loadSnapshotOperation(request.Context(), payload.ManifestOperationID)
			if loadErr != nil {
				s.writeComputeError(writer, loadErr)
				return
			}
			if err := blueprintNICsMatch(input.NICs, manifestPayload.Blueprints); err != nil {
				_ = s.terminalizeClaimedSnapshotMutation(request.Context(), operation.ID, "prepared", "compensated", model.OperationFailed, err.Error())
				s.writeComputeError(writer, err)
				return
			}
			if err := s.verifySnapshotPorts(request.Context(), manifestPayload); err != nil {
				_ = s.terminalizeClaimedSnapshotMutation(request.Context(), operation.ID, "prepared", "compensated", model.OperationFailed, err.Error())
				s.writeComputeError(writer, err)
				return
			}
		}
		writer.Header().Set("Idempotency-Replayed", "true")
		writeJSON(writer, http.StatusOK, map[string]any{"data": snapshotMutationTransaction(operation.ID, payload)})
		return
	} else if !errors.Is(getErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, getErr)
		return
	}
	manifestOperation, manifest, err := s.findSnapshotRecord(request.Context(), input.VMID, input.SnapshotID, input.SnapshotEpoch)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if manifestOperation.OperationStatus != model.OperationSucceeded || manifest.Phase != "created" {
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_manifest_unavailable", "snapshot manifest is not active", map[string]any{"phase": manifest.Phase}))
		return
	}
	if input.Action == "rollback" {
		if err := blueprintNICsMatch(input.NICs, manifest.Blueprints); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	}
	payload := computeSnapshotMutationPayload{Version: computePayloadVersion, LifecycleID: input.LifecycleID, Phase: "prepared", Action: input.Action,
		VMID: input.VMID, SnapshotID: input.SnapshotID, SnapshotEpoch: input.SnapshotEpoch, ManifestOperationID: manifestOperation.ID,
		ManifestHash: snapshotPayloadHash(manifest), NodeID: node.ID, Node: node.Name, Chassis: node.ChassisID, Ports: append([]computeSnapshotPort(nil), manifest.Ports...)}
	operation, err := s.createComputeOperation(request.Context(), operationID, computeSnapshotMutationAction, input.VMID, payload)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := s.verifySnapshotMutationManifest(request.Context(), payload); err != nil {
		_ = s.terminalizeClaimedSnapshotMutation(request.Context(), operation.ID, "prepared", "compensated", model.OperationFailed, err.Error())
		s.writeComputeError(writer, err)
		return
	}
	if input.Action == "rollback" {
		if err := s.verifySnapshotPorts(request.Context(), manifest); err != nil {
			_ = s.terminalizeClaimedSnapshotMutation(request.Context(), operation.ID, "prepared", "compensated", model.OperationFailed, err.Error())
			s.writeComputeError(writer, err)
			return
		}
	}
	transaction := snapshotMutationTransaction(operation.ID, payload)
	writeJSON(writer, http.StatusOK, map[string]any{"data": transaction})
}

func (s *Server) computeSnapshotFinish(writer http.ResponseWriter, request *http.Request, abort bool) {
	var input computeSnapshotMutationTransaction
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if validateLifecycleID("lifecycle_id", input.LifecycleID) != nil || input.VMID < 1 || validateLifecycleID("snapshot_id", input.SnapshotID) != nil || input.SnapshotEpoch < 1 ||
		(input.Action != "rollback" && input.Action != "delete") || input.OperationID != computeSnapshotMutationOperationID(computeSnapshotMutationRequest{LifecycleID: input.LifecycleID, Action: input.Action, VMID: input.VMID, SnapshotID: input.SnapshotID, SnapshotEpoch: input.SnapshotEpoch}) || input.PayloadHash == "" {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "complete snapshot transition transaction echo is required", nil))
		return
	}
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	operation, payload, err := s.loadSnapshotMutationOperation(request.Context(), input.OperationID)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if !reflect.DeepEqual(input, snapshotMutationTransaction(operation.ID, payload)) {
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_transaction_mismatch", "snapshot transaction echo is incomplete, reordered, or stale", nil))
		return
	}
	local, err := s.localComputeNode(request.Context(), payload.Node)
	if err != nil || local.ID != payload.NodeID || local.ChassisID != payload.Chassis {
		if err == nil {
			err = computeError(http.StatusConflict, "snapshot_worker_drift", "snapshot transition worker identity changed after prepare", nil)
		}
		s.writeComputeError(writer, err)
		return
	}
	claimPhase, terminalPhase := "committing", "committed"
	if abort {
		claimPhase, terminalPhase = "aborting", "aborted"
	}
	if operation.OperationStatus != model.OperationRunning {
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == terminalPhase {
			writer.Header().Set("Idempotency-Replayed", "true")
			writeJSON(writer, http.StatusOK, map[string]any{"data": input})
			return
		}
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_transaction_terminal", "snapshot transition is terminal in a different phase", map[string]any{"phase": payload.Phase}))
		return
	}
	if payload.Phase != "prepared" && payload.Phase != claimPhase {
		s.writeComputeError(writer, computeError(http.StatusConflict, "snapshot_transaction_claimed", "snapshot transition is claimed by a different action", map[string]any{"phase": payload.Phase}))
		return
	}
	if err := s.claimSnapshotMutationOperation(request.Context(), operation.ID, []string{"prepared"}, claimPhase); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if !abort {
		if err := s.verifySnapshotMutationManifest(request.Context(), payload); err != nil {
			_ = s.recordComputeClaimError(request.Context(), operation.ID, computeSnapshotMutationAction, claimPhase, err.Error())
			s.writeComputeError(writer, err)
			return
		}
		if input.Action == "delete" {
			if err := s.deleteSnapshotManifestExact(request.Context(), payload); err != nil {
				_ = s.recordComputeClaimError(request.Context(), operation.ID, computeSnapshotMutationAction, claimPhase, err.Error())
				s.writeComputeError(writer, err)
				return
			}
		} else {
			_, manifest, err := s.loadSnapshotOperation(request.Context(), payload.ManifestOperationID)
			if err == nil {
				err = s.verifySnapshotPorts(request.Context(), manifest)
			}
			if err != nil {
				_ = s.recordComputeClaimError(request.Context(), operation.ID, computeSnapshotMutationAction, claimPhase, err.Error())
				s.writeComputeError(writer, err)
				return
			}
		}
	}
	if err := s.terminalizeClaimedSnapshotMutation(request.Context(), operation.ID, claimPhase, terminalPhase, model.OperationSucceeded, ""); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": input})
}

func (s *Server) snapshotPortManifest(ctx context.Context, port *model.Port) (computeSnapshotPort, error) {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	allocationIDs, allocationRevisions, err := s.sourcePortAllocationManifest(ctx, port)
	if err != nil {
		return computeSnapshotPort{}, err
	}
	return computeSnapshotPort{PortID: port.ID, NIC: port.NIC, MACAddress: strings.ToLower(port.MACAddress), NetworkID: port.NetworkID,
		FixedIPs: append([]model.FixedIP(nil), port.FixedIPs...), SecurityGroupIDs: groups, AllocationIDs: allocationIDs, AllocationRevisions: allocationRevisions, LSPName: port.LSPName,
		NodeID: port.NodeID, RequestedChassis: port.RequestedChassis, BindingStatus: port.BindingStatus,
		AdminStateUp: port.AdminStateUp, Revision: port.Revision, Generation: port.Generation}, nil
}

func blueprintsFromSnapshot(ports []computeSnapshotPort) ([]computePortBlueprint, error) {
	result := make([]computePortBlueprint, 0, len(ports))
	for _, port := range ports {
		if len(port.FixedIPs) > 1 {
			return nil, computeError(http.StatusConflict, "multi_ip_clone_unsupported", "automatic clone requires at most one fixed-IP subnet per PVN NIC", nil)
		}
		blueprint := computePortBlueprint{NIC: port.NIC, SourceMACAddress: strings.ToLower(port.MACAddress), NetworkID: port.NetworkID, SecurityGroupIDs: append([]string(nil), port.SecurityGroupIDs...)}
		if len(port.FixedIPs) == 1 {
			blueprint.SubnetID = port.FixedIPs[0].SubnetID
		}
		result = append(result, blueprint)
	}
	return cloneBlueprints(result), nil
}

func snapshotCreateMatches(input computeSnapshotCreateRequest, node *model.Node, payload computeSnapshotPayload) bool {
	return payload.Version == computePayloadVersion && payload.LifecycleID == input.LifecycleID && payload.VMID == input.VMID && payload.SnapshotID == input.SnapshotID && payload.SnapshotEpoch == input.SnapshotEpoch &&
		payload.NodeID == node.ID && payload.Node == node.Name && payload.Chassis == node.ChassisID && blueprintNICsMatch(input.NICs, payload.Blueprints) == nil
}

func snapshotTransaction(operationID string, payload computeSnapshotPayload) computeSnapshotTransaction {
	return computeSnapshotTransaction{LifecycleID: payload.LifecycleID, VMID: payload.VMID, SnapshotID: payload.SnapshotID, SnapshotEpoch: payload.SnapshotEpoch, OperationID: operationID, PayloadHash: snapshotPayloadHash(payload), Ports: append([]computeSnapshotPort(nil), payload.Ports...)}
}

func snapshotPayloadHash(payload computeSnapshotPayload) string {
	payload.Phase = ""
	return computePayloadHash(payload)
}

func computeSnapshotMutationOperationID(input computeSnapshotMutationRequest) string {
	return computeResourceOperationID(computeSnapshotMutationAction, input.LifecycleID+":"+input.Action+":"+input.SnapshotID+":"+fmt.Sprint(input.SnapshotEpoch), input.VMID)
}

func snapshotMutationPayloadHash(payload computeSnapshotMutationPayload) string {
	payload.Phase = ""
	return computePayloadHash(payload)
}

func snapshotMutationTransaction(operationID string, payload computeSnapshotMutationPayload) computeSnapshotMutationTransaction {
	return computeSnapshotMutationTransaction{LifecycleID: payload.LifecycleID, Action: payload.Action, VMID: payload.VMID,
		SnapshotID: payload.SnapshotID, SnapshotEpoch: payload.SnapshotEpoch, OperationID: operationID,
		PayloadHash: snapshotMutationPayloadHash(payload), Ports: append([]computeSnapshotPort(nil), payload.Ports...)}
}

func snapshotMutationRequestMatches(input computeSnapshotMutationRequest, node *model.Node, payload computeSnapshotMutationPayload) bool {
	return payload.Version == computePayloadVersion && payload.LifecycleID == input.LifecycleID && payload.Action == input.Action && payload.VMID == input.VMID &&
		payload.SnapshotID == input.SnapshotID && payload.SnapshotEpoch == input.SnapshotEpoch && payload.NodeID == node.ID && payload.Node == node.Name && payload.Chassis == node.ChassisID
}

func decodeSnapshotMutationPayload(operation *model.Operation) (computeSnapshotMutationPayload, error) {
	var payload computeSnapshotMutationPayload
	if operation.Action != computeSnapshotMutationAction || model.UnmarshalOperationPayload(operation.Payload, &payload) != nil || payload.Version != computePayloadVersion ||
		payload.LifecycleID == "" || (payload.Action != "rollback" && payload.Action != "delete") || payload.VMID < 1 || payload.SnapshotID == "" || payload.SnapshotEpoch < 1 ||
		payload.ManifestOperationID == "" || payload.ManifestHash == "" || payload.NodeID == "" || payload.Node == "" || payload.Chassis == "" {
		return payload, computeError(http.StatusConflict, "snapshot_mutation_payload_invalid", "durable snapshot transition payload is invalid", map[string]any{"operation_id": operation.ID})
	}
	return payload, nil
}

func (s *Server) loadSnapshotMutationOperation(ctx context.Context, id string) (*model.Operation, computeSnapshotMutationPayload, error) {
	resource, err := s.store.Get(ctx, model.KindOperation, id)
	if err != nil {
		return nil, computeSnapshotMutationPayload{}, err
	}
	operation := resource.(*model.Operation)
	payload, err := decodeSnapshotMutationPayload(operation)
	return operation, payload, err
}

func (s *Server) claimSnapshotMutationOperation(ctx context.Context, id string, fromPhases []string, claimPhase string) error {
	return s.claimComputeOperation(ctx, id, computeSnapshotMutationAction, fromPhases, claimPhase, func(operation *model.Operation) (any, error) {
		payload, err := decodeSnapshotMutationPayload(operation)
		payload.Phase = claimPhase
		return payload, err
	})
}

func (s *Server) terminalizeClaimedSnapshotMutation(ctx context.Context, id, claimPhase, terminalPhase string, status model.OperationStatus, failure string) error {
	return s.terminalizeClaimedComputeOperation(ctx, id, computeSnapshotMutationAction, claimPhase, terminalPhase, status, failure, func(operation *model.Operation) (any, error) {
		payload, err := decodeSnapshotMutationPayload(operation)
		payload.Phase = terminalPhase
		return payload, err
	})
}

func (s *Server) verifySnapshotMutationManifest(ctx context.Context, mutation computeSnapshotMutationPayload) error {
	operation, payload, err := s.loadSnapshotOperation(ctx, mutation.ManifestOperationID)
	if err != nil {
		return err
	}
	if payload.VMID != mutation.VMID || payload.SnapshotID != mutation.SnapshotID || payload.SnapshotEpoch != mutation.SnapshotEpoch || snapshotPayloadHash(payload) != mutation.ManifestHash {
		return computeError(http.StatusConflict, "snapshot_manifest_drift", "snapshot transition does not own the exact immutable manifest", nil)
	}
	if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "created" {
		return nil
	}
	if mutation.Action == "delete" && operation.OperationStatus == model.OperationSucceeded && payload.Phase == "deleted" {
		return nil
	}
	return computeError(http.StatusConflict, "snapshot_manifest_unavailable", "snapshot manifest changed during its transition", map[string]any{"phase": payload.Phase, "status": operation.OperationStatus})
}

func (s *Server) deleteSnapshotManifestExact(ctx context.Context, mutation computeSnapshotMutationPayload) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		operation, payload, err := s.loadSnapshotOperation(ctx, mutation.ManifestOperationID)
		if err != nil {
			return err
		}
		if payload.VMID != mutation.VMID || payload.SnapshotID != mutation.SnapshotID || payload.SnapshotEpoch != mutation.SnapshotEpoch || snapshotPayloadHash(payload) != mutation.ManifestHash {
			return computeError(http.StatusConflict, "snapshot_manifest_drift", "snapshot delete transition lost immutable manifest ownership", nil)
		}
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "deleted" {
			return nil
		}
		if operation.OperationStatus != model.OperationSucceeded || payload.Phase != "created" {
			return computeError(http.StatusConflict, "snapshot_manifest_unavailable", "snapshot manifest cannot be deleted from its current phase", map[string]any{"phase": payload.Phase})
		}
		payload.Phase = "deleted"
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return err
		}
		copyResource, err := model.Clone(operation)
		if err != nil {
			return err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload = encoded
		now := s.clusterGate.now().UTC()
		desired.CompletedAt = &now
		_, _, err = s.store.Update(ctx, desired, operation.Revision, "compute-snapshot-manifest-delete-"+fmt.Sprint(operation.Revision))
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "snapshot_manifest_conflict", "snapshot manifest changed concurrently", nil)
}

func computeSnapshotOperationID(vmid int, snapshotID string, snapshotEpoch int64) string {
	return computeResourceOperationID(computeSnapshotAction, snapshotID+":"+fmt.Sprint(snapshotEpoch), vmid)
}

func decodeSnapshotPayload(operation *model.Operation) (computeSnapshotPayload, error) {
	var payload computeSnapshotPayload
	decodeErr := model.UnmarshalOperationPayload(operation.Payload, &payload)
	fence := payload.LifecycleID == "cleanup-fence" && len(payload.Ports) == 0 && len(payload.Blueprints) == 0 && (payload.Phase == "cleaning" || payload.Phase == "deleted")
	if operation.Action != computeSnapshotAction || decodeErr != nil || payload.Version != computePayloadVersion || payload.LifecycleID == "" || payload.VMID < 1 || payload.SnapshotID == "" || payload.SnapshotEpoch < 1 || payload.NodeID == "" || payload.Node == "" || payload.Chassis == "" || (!fence && (len(payload.Ports) == 0 || len(payload.Ports) != len(payload.Blueprints))) {
		return payload, computeError(http.StatusConflict, "snapshot_payload_invalid", "durable snapshot payload is invalid", map[string]any{"operation_id": operation.ID})
	}
	return payload, nil
}

func (s *Server) loadSnapshotOperation(ctx context.Context, id string) (*model.Operation, computeSnapshotPayload, error) {
	resource, err := s.store.Get(ctx, model.KindOperation, id)
	if err != nil {
		return nil, computeSnapshotPayload{}, err
	}
	operation := resource.(*model.Operation)
	payload, err := decodeSnapshotPayload(operation)
	return operation, payload, err
}

func (s *Server) claimSnapshotOperation(ctx context.Context, id string, vmid int, fromPhases []string, claimPhase string) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		operation, payload, err := s.loadSnapshotOperation(ctx, id)
		if err != nil {
			return err
		}
		if operation.OperationStatus == model.OperationRunning && payload.Phase == claimPhase && operation.TargetID == computeVMOperationTarget(vmid) {
			return nil
		}
		validSource := (payload.Phase == "created" && operation.OperationStatus == model.OperationSucceeded) ||
			(payload.Phase == "creating" && operation.OperationStatus == model.OperationRunning) ||
			(payload.Phase == "conflicted" && operation.OperationStatus == model.OperationFailed)
		if !slices.Contains(fromPhases, payload.Phase) || !validSource {
			return computeError(http.StatusConflict, "snapshot_transaction_claimed", "snapshot lifecycle is claimed by a different action", map[string]any{"phase": payload.Phase, "status": operation.OperationStatus})
		}
		payload.Phase = claimPhase
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return err
		}
		copyResource, err := model.Clone(operation)
		if err != nil {
			return err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload, desired.OperationStatus = encoded, model.OperationRunning
		desired.TargetID, desired.TargetRevision = computeVMOperationTarget(vmid), 1
		desired.Error, desired.CompletedAt = "", nil
		_, _, err = s.store.Update(ctx, desired, operation.Revision, "compute-snapshot-claim-"+claimPhase+"-"+fmt.Sprint(operation.Revision))
		if err == nil {
			return nil
		}
		if errors.Is(err, controlstore.ErrAlreadyExists) {
			return computeError(http.StatusConflict, "compute_vm_busy", "VM already has an active compute lifecycle transaction", map[string]any{"vmid": vmid})
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "snapshot_claim_conflict", "snapshot lifecycle changed concurrently", nil)
}

func (s *Server) terminalizeClaimedSnapshotOperation(ctx context.Context, id, claimPhase, terminalPhase string, status model.OperationStatus, failure string) error {
	return s.terminalizeClaimedComputeOperation(ctx, id, computeSnapshotAction, claimPhase, terminalPhase, status, failure, func(operation *model.Operation) (any, error) {
		payload, err := decodeSnapshotPayload(operation)
		payload.Phase = terminalPhase
		return payload, err
	})
}

func (s *Server) deleteSnapshotOperation(ctx context.Context, id string) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		operation, payload, err := s.loadSnapshotOperation(ctx, id)
		if err != nil {
			return err
		}
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "deleted" {
			return nil
		}
		if (operation.OperationStatus != model.OperationSucceeded && operation.OperationStatus != model.OperationRunning) || payload.Phase != "created" {
			return computeError(http.StatusConflict, "snapshot_transaction_terminal", "snapshot manifest cannot transition to deleted", map[string]any{"phase": payload.Phase, "status": operation.OperationStatus})
		}
		payload.Phase = "deleted"
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return err
		}
		copyResource, err := model.Clone(operation)
		if err != nil {
			return err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload = encoded
		desired.OperationStatus = model.OperationSucceeded
		desired.Error = ""
		now := s.clusterGate.now().UTC()
		desired.CompletedAt = &now
		_, _, err = s.store.Update(ctx, desired, operation.Revision, "compute-snapshot-deleted-"+operation.ID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "snapshot_manifest_conflict", "snapshot manifest changed concurrently", nil)
}

func (s *Server) verifySnapshotPorts(ctx context.Context, payload computeSnapshotPayload) error {
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: payload.VMID})
	if err != nil {
		return err
	}
	if len(resources) != len(payload.Ports) {
		return computeError(http.StatusConflict, "snapshot_port_set_drift", "VM port set differs from immutable snapshot manifest", nil)
	}
	byID := make(map[string]*model.Port, len(resources))
	for _, resource := range resources {
		byID[resource.GetMetadata().ID] = resource.(*model.Port)
	}
	for _, expected := range payload.Ports {
		port := byID[expected.PortID]
		if port == nil || !snapshotPortConfigMatches(port, expected) {
			return computeError(http.StatusConflict, "snapshot_port_drift", "current VM port differs from immutable snapshot manifest", map[string]any{"port_id": expected.PortID})
		}
		if err := s.validateSnapshotAllocations(ctx, port, expected); err != nil {
			return err
		}
		if _, err := s.forceRealizeComputePort(ctx, port); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) validateSnapshotAllocations(ctx context.Context, port *model.Port, expected computeSnapshotPort) error {
	source := computeSourcePort{PortID: expected.PortID, FixedIPs: expected.FixedIPs, AllocationIDs: expected.AllocationIDs, AllocationRevisions: expected.AllocationRevisions}
	return s.validateSourceAllocations(ctx, port, source, false)
}

func snapshotPortConfigMatches(port *model.Port, expected computeSnapshotPort) bool {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	return port.ID == expected.PortID && port.NIC == expected.NIC && strings.EqualFold(port.MACAddress, expected.MACAddress) &&
		port.NetworkID == expected.NetworkID && reflect.DeepEqual(port.FixedIPs, expected.FixedIPs) &&
		slices.Equal(groups, expected.SecurityGroupIDs) && port.LSPName == expected.LSPName && port.AdminStateUp == expected.AdminStateUp
}

func (s *Server) findSnapshotRecord(ctx context.Context, vmid int, snapshotID string, snapshotEpoch int64) (*model.Operation, computeSnapshotPayload, error) {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return nil, computeSnapshotPayload{}, err
	}
	var found *model.Operation
	var foundPayload computeSnapshotPayload
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != computeSnapshotAction {
			continue
		}
		payload, decodeErr := decodeSnapshotPayload(operation)
		if decodeErr != nil {
			return nil, computeSnapshotPayload{}, decodeErr
		}
		if payload.VMID != vmid || payload.SnapshotID != snapshotID || payload.SnapshotEpoch != snapshotEpoch {
			continue
		}
		if found != nil {
			return nil, computeSnapshotPayload{}, computeError(http.StatusConflict, "snapshot_manifest_ambiguous", "multiple durable manifests exist for the snapshot", map[string]any{"vmid": vmid, "snapshot_id": snapshotID})
		}
		found, foundPayload = operation, payload
	}
	if found == nil {
		return nil, computeSnapshotPayload{}, controlstore.ErrNotFound
	}
	return found, foundPayload, nil
}

func (s *Server) findActiveSnapshotByName(ctx context.Context, vmid int, snapshotID string) (*model.Operation, computeSnapshotPayload, error) {
	return s.findActiveSnapshotByNameExcept(ctx, vmid, snapshotID, "")
}

func (s *Server) findActiveSnapshotByNameExcept(ctx context.Context, vmid int, snapshotID, excludedOperationID string) (*model.Operation, computeSnapshotPayload, error) {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return nil, computeSnapshotPayload{}, err
	}
	var found *model.Operation
	var foundPayload computeSnapshotPayload
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != computeSnapshotAction || operation.ID == excludedOperationID {
			continue
		}
		payload, decodeErr := decodeSnapshotPayload(operation)
		if decodeErr != nil {
			return nil, computeSnapshotPayload{}, decodeErr
		}
		if payload.VMID != vmid || payload.SnapshotID != snapshotID || payload.Phase == "deleted" || payload.Phase == "conflicted" || operation.OperationStatus == model.OperationFailed {
			continue
		}
		if found != nil {
			return nil, computeSnapshotPayload{}, computeError(http.StatusConflict, "snapshot_manifest_ambiguous", "multiple active manifests exist for the snapshot name", map[string]any{"vmid": vmid, "snapshot_id": snapshotID})
		}
		found, foundPayload = operation, payload
	}
	if found == nil {
		return nil, computeSnapshotPayload{}, controlstore.ErrNotFound
	}
	return found, foundPayload, nil
}

func (s *Server) findSnapshotPayload(ctx context.Context, vmid int, snapshotID string, snapshotEpoch int64) (computeSnapshotPayload, error) {
	operation, payload, err := s.findSnapshotRecord(ctx, vmid, snapshotID, snapshotEpoch)
	if err != nil {
		return payload, err
	}
	if operation.OperationStatus != model.OperationSucceeded || payload.Phase != "created" {
		return payload, computeError(http.StatusConflict, "snapshot_manifest_unavailable", "snapshot does not have an active committed network manifest", map[string]any{"phase": payload.Phase})
	}
	return payload, nil
}

func (s *Server) findTemplateRecord(ctx context.Context, vmid int, includeDestroyed bool) (*model.Operation, computeTemplatePayload, error) {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return nil, computeTemplatePayload{}, err
	}
	var found *model.Operation
	var foundPayload computeTemplatePayload
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != computeTemplateAction {
			continue
		}
		payload, decodeErr := decodeTemplatePayload(operation)
		if decodeErr != nil {
			return nil, computeTemplatePayload{}, decodeErr
		}
		if payload.VMID != vmid || payload.Phase == "aborted" || payload.Phase == "compensated" || payload.Phase == "recovery-required" || (!includeDestroyed && payload.Phase == "destroyed") {
			continue
		}
		if found != nil {
			return nil, computeTemplatePayload{}, computeError(http.StatusConflict, "template_manifest_ambiguous", "multiple durable template manifests exist for the VM", map[string]any{"vmid": vmid})
		}
		found, foundPayload = operation, payload
	}
	if found == nil {
		return nil, computeTemplatePayload{}, controlstore.ErrNotFound
	}
	return found, foundPayload, nil
}

func (s *Server) findCommittedTemplate(ctx context.Context, vmid int, recoverPrepared bool) (computeTemplatePayload, error) {
	operation, payload, err := s.findTemplateRecord(ctx, vmid, false)
	if err != nil {
		return payload, err
	}
	if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "committed" {
		return payload, nil
	}
	if !recoverPrepared || operation.OperationStatus != model.OperationRunning || (payload.Phase != "prepared" && payload.Phase != "committing") {
		return payload, computeError(http.StatusConflict, "template_manifest_unavailable", "template does not have a committed network blueprint", map[string]any{"phase": payload.Phase})
	}
	local, err := s.localComputeNode(ctx, payload.Node)
	if err != nil || local.ID != payload.NodeID || local.ChassisID != payload.Chassis {
		if err == nil {
			err = computeError(http.StatusConflict, "template_source_drift", "template source node identity changed before recovery", nil)
		}
		return payload, err
	}
	operation, payload, err = s.loadTemplateOperation(ctx, operation.ID)
	if err != nil {
		return payload, err
	}
	if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "committed" {
		return payload, nil
	}
	if operation.OperationStatus != model.OperationRunning || (payload.Phase != "prepared" && payload.Phase != "committing") {
		return payload, computeError(http.StatusConflict, "template_manifest_unavailable", "template recovery transaction changed phase", map[string]any{"phase": payload.Phase})
	}
	if payload.Phase == "prepared" {
		if err := s.claimTemplateOperation(ctx, operation.ID, "prepared", "committing"); err != nil {
			return payload, err
		}
	}
	if err := s.requireVMPortSetEmpty(ctx, payload.VMID, "template recovery"); err != nil {
		return payload, err
	}
	if failures := s.deleteDetachedSourcePorts(ctx, payload.Ports, "compute-template-recover-"+payload.LifecycleID); len(failures) != 0 {
		return payload, computeError(http.StatusServiceUnavailable, "template_recovery_failed", "template blueprint recovery requires cleanup", map[string]any{"recovery_required": true, "errors": failures})
	}
	if err := s.terminalizeClaimedTemplateOperation(ctx, operation.ID, "committing", "committed"); err != nil {
		return payload, err
	}
	payload.Phase = "committed"
	return payload, nil
}

func (s *Server) requireVMPortSetEmpty(ctx context.Context, vmid int, phase string) error {
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: vmid})
	if err != nil {
		return err
	}
	if len(resources) != 0 {
		return computeError(http.StatusConflict, "lifecycle_port_set_drift", "VM acquired PVN-managed ports after "+phase, map[string]any{"vmid": vmid, "ports": len(resources)})
	}
	return nil
}

func (s *Server) computeDestroyCapture(writer http.ResponseWriter, request *http.Request) {
	var input computeDestroyCaptureRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if input.VMID < 1 || (len(input.NICs) == 0 && len(input.Snapshots) == 0) {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "positive vmid and at least one live or snapshot PVN identity are required", nil))
		return
	}
	if len(input.NICs) != 0 {
		if err := validateComputeVM(input.VMID, input.NICs); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	}
	if err := validateSnapshotIdentities(input.Snapshots); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	node, err := s.configuredComputeNode(request.Context())
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	operationID := computeResourceOperationID(computeDestroyAction, input.LifecycleID, input.VMID)
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	if resource, getErr := s.store.Get(request.Context(), model.KindOperation, operationID); getErr == nil {
		operation := resource.(*model.Operation)
		payload, err := decodeDestroyPayload(operation)
		if err != nil || !destroyRequestMatches(input, node, payload) {
			if err == nil {
				err = computeError(http.StatusConflict, "destroy_id_conflict", "lifecycle_id was reused with different destroy parameters", nil)
			}
			s.writeComputeError(writer, err)
			return
		}
		if operation.OperationStatus == model.OperationRunning && payload.Phase == "compensating" {
			cause := operation.Error
			if cause == "" {
				cause = "resuming interrupted destroy capture compensation"
			}
			s.writeComputeError(writer, s.failSourcePrepare(request.Context(), operation, payload.VMID, payload.Ports, computeDestroyAction, payload.LifecycleID, errors.New(cause)))
			return
		}
		if operation.OperationStatus != model.OperationRunning || payload.Phase != "captured" {
			s.writeComputeError(writer, computeError(http.StatusConflict, "destroy_transaction_terminal", "destroy transaction is already terminal", map[string]any{"phase": payload.Phase}))
			return
		}
		if !payload.Template {
			if err := s.detachSourcePorts(request.Context(), payload.VMID, payload.Ports, "compute-destroy-"+payload.LifecycleID); err != nil {
				s.writeComputeError(writer, s.failSourcePrepare(request.Context(), operation, payload.VMID, payload.Ports, computeDestroyAction, payload.LifecycleID, err))
				return
			}
		}
		writer.Header().Set("Idempotency-Replayed", "true")
		writeJSON(writer, http.StatusOK, map[string]any{"data": destroyTransaction(operation.ID, payload)})
		return
	} else if !errors.Is(getErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, getErr)
		return
	}
	var livePorts []*model.Port
	if input.Template {
		if err := s.requireVMPortSetEmpty(request.Context(), input.VMID, "template destroy capture"); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	} else {
		livePorts, err = s.loadExactComputePorts(request.Context(), input.VMID, input.NICs)
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
	}
	payload := computeDestroyPayload{Version: computePayloadVersion, LifecycleID: input.LifecycleID, Phase: "captured", VMID: input.VMID, Template: input.Template, NodeID: node.ID, Node: node.Name, Chassis: node.ChassisID}
	payload.SnapshotRefs, err = s.captureSnapshotRefs(request.Context(), input.VMID, input.Snapshots)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if input.Template {
		templatePayload, findErr := s.findCommittedTemplate(request.Context(), input.VMID, true)
		if findErr != nil {
			s.writeComputeError(writer, findErr)
			return
		}
		templateOperation, durableTemplate, findErr := s.findTemplateRecord(request.Context(), input.VMID, false)
		if findErr != nil {
			s.writeComputeError(writer, findErr)
			return
		}
		if templateOperation.OperationStatus != model.OperationSucceeded || durableTemplate.Phase != "committed" || computePayloadHash(templatePayload) != computePayloadHash(durableTemplate) {
			s.writeComputeError(writer, computeError(http.StatusConflict, "template_manifest_unavailable", "destroy requires a committed template blueprint", nil))
			return
		}
		if err := blueprintNICsMatch(input.NICs, durableTemplate.Blueprints); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		payload.TemplateOperationID, payload.Blueprints = templateOperation.ID, cloneBlueprints(durableTemplate.Blueprints)
	} else {
		for _, port := range livePorts {
			if validateErr := validateSourcePortLifecycle(port); validateErr != nil {
				s.writeComputeError(writer, validateErr)
				return
			}
			if dependentErr := s.ensureNoPortDeprovisionDependents(request.Context(), port.ID); dependentErr != nil {
				s.writeComputeError(writer, dependentErr)
				return
			}
			if port.NodeID != node.ID || port.RequestedChassis != node.ChassisID || (port.BindingStatus != model.PortBinding && port.BindingStatus != model.PortBound) {
				s.writeComputeError(writer, computeError(http.StatusConflict, "destroy_source_not_local", "destroy source port is not attached to the local compute node", computePortDetails(port)))
				return
			}
			manifest, manifestErr := s.sourcePortManifest(request.Context(), port)
			if manifestErr != nil {
				s.writeComputeError(writer, manifestErr)
				return
			}
			payload.Ports = append(payload.Ports, manifest)
		}
		payload.Blueprints, err = blueprintsFromPorts(livePorts)
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
	}
	operation, err := s.createComputeOperation(request.Context(), operationID, computeDestroyAction, input.VMID, payload)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if !payload.Template {
		if err := s.detachSourcePorts(request.Context(), payload.VMID, payload.Ports, "compute-destroy-"+payload.LifecycleID); err != nil {
			s.writeComputeError(writer, s.failSourcePrepare(request.Context(), operation, payload.VMID, payload.Ports, computeDestroyAction, payload.LifecycleID, err))
			return
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": destroyTransaction(operation.ID, payload)})
}

func (s *Server) computeDestroyFinish(writer http.ResponseWriter, request *http.Request, abort bool) {
	var input computeDestroyTransaction
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil || input.VMID < 1 || input.OperationID == "" || input.PayloadHash == "" {
		if err == nil {
			err = computeError(http.StatusBadRequest, "invalid_request", "complete destroy transaction echo is required", nil)
		}
		s.writeComputeError(writer, err)
		return
	}
	if input.OperationID != computeResourceOperationID(computeDestroyAction, input.LifecycleID, input.VMID) {
		s.writeComputeError(writer, computeError(http.StatusConflict, "destroy_transaction_mismatch", "destroy operation_id does not match its identity", nil))
		return
	}
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	operation, payload, err := s.loadDestroyOperation(request.Context(), input.OperationID)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if !reflect.DeepEqual(input, destroyTransaction(operation.ID, payload)) {
		s.writeComputeError(writer, computeError(http.StatusConflict, "destroy_transaction_mismatch", "destroy transaction echo is incomplete, reordered, or stale", nil))
		return
	}
	local, err := s.localComputeNode(request.Context(), payload.Node)
	if err != nil || local.ID != payload.NodeID || local.ChassisID != payload.Chassis {
		if err == nil {
			err = computeError(http.StatusConflict, "destroy_source_drift", "destroy source node identity changed after capture", nil)
		}
		s.writeComputeError(writer, err)
		return
	}
	phase, claimPhase := "committed", "committing"
	if abort {
		phase, claimPhase = "aborted", "aborting"
	}
	if operation.OperationStatus != model.OperationRunning {
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == phase {
			writer.Header().Set("Idempotency-Replayed", "true")
			writeJSON(writer, http.StatusOK, map[string]any{"data": input})
			return
		}
		s.writeComputeError(writer, computeError(http.StatusConflict, "destroy_transaction_terminal", "destroy transaction is terminal in a different phase", map[string]any{"phase": payload.Phase}))
		return
	}
	if payload.Phase != "captured" && payload.Phase != claimPhase {
		s.writeComputeError(writer, computeError(http.StatusConflict, "destroy_transaction_terminal", "destroy transaction is claimed by a different action", map[string]any{"phase": payload.Phase}))
		return
	}
	if err := s.claimDestroyOperation(request.Context(), operation.ID, "captured", claimPhase); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if !abort {
		if err := s.verifyCapturedSnapshotSet(request.Context(), payload); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		resources, err := s.store.List(request.Context(), model.KindPort, controlstore.ListOptions{VMID: payload.VMID})
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
		if len(resources) != 0 {
			s.writeComputeError(writer, computeError(http.StatusConflict, "destroy_port_set_drift", "VM acquired PVN-managed ports after destroy capture", map[string]any{"vmid": payload.VMID, "ports": len(resources)}))
			return
		}
	}
	if abort {
		if err := s.verifyDestroyAbortResources(request.Context(), payload); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		if !payload.Template {
			if err := s.verifySourceRestorePortSet(request.Context(), payload.VMID, payload.Ports, "destroy abort"); err != nil {
				s.writeComputeError(writer, err)
				return
			}
			if failures := s.restoreSourcePorts(request.Context(), payload.VMID, payload.Ports, "compute-destroy-abort-"+payload.LifecycleID); len(failures) != 0 {
				s.writeComputeError(writer, computeError(http.StatusServiceUnavailable, "destroy_abort_failed", "destroy ports require recovery", map[string]any{"recovery_required": true, "errors": failures}))
				return
			}
		}
	} else if payload.Template {
		if err := s.consumeTemplateBlueprint(request.Context(), payload); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	} else if failures := s.deleteDetachedSourcePorts(request.Context(), payload.Ports, "compute-destroy-commit-"+payload.LifecycleID); len(failures) != 0 {
		s.writeComputeError(writer, computeError(http.StatusServiceUnavailable, "destroy_commit_failed", "destroy ports require recovery", map[string]any{"recovery_required": true, "errors": failures}))
		return
	}
	if !abort {
		if err := s.deleteCapturedSnapshots(request.Context(), payload); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	}
	if err := s.terminalizeClaimedDestroyOperation(request.Context(), operation.ID, claimPhase, phase); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": input})
}

func (s *Server) verifyDestroyAbortResources(ctx context.Context, destroy computeDestroyPayload) error {
	for _, ref := range destroy.SnapshotRefs {
		operation, payload, err := s.loadSnapshotOperation(ctx, ref.OperationID)
		if err != nil {
			return err
		}
		if operation.OperationStatus != model.OperationSucceeded || payload.Phase != "created" || payload.VMID != destroy.VMID ||
			payload.SnapshotID != ref.SnapshotID || payload.SnapshotEpoch != ref.SnapshotEpoch || snapshotPayloadHash(payload) != ref.PayloadHash {
			return computeError(http.StatusConflict, "snapshot_set_drift", "captured snapshot manifest changed before destroy abort", map[string]any{"operation_id": ref.OperationID})
		}
	}
	if !destroy.Template {
		return nil
	}
	operation, payload, err := s.loadTemplateOperation(ctx, destroy.TemplateOperationID)
	if err != nil {
		return err
	}
	if operation.OperationStatus != model.OperationSucceeded || payload.Phase != "committed" || payload.VMID != destroy.VMID || !reflect.DeepEqual(payload.Blueprints, destroy.Blueprints) {
		return computeError(http.StatusConflict, "template_manifest_drift", "template blueprint changed before destroy abort", nil)
	}
	return nil
}

func (s *Server) verifyCapturedSnapshotSet(ctx context.Context, destroy computeDestroyPayload) error {
	captured := make(map[string]computeSnapshotRef, len(destroy.SnapshotRefs))
	for _, ref := range destroy.SnapshotRefs {
		captured[ref.OperationID] = ref
	}
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != computeSnapshotAction {
			continue
		}
		payload, err := decodeSnapshotPayload(operation)
		if err != nil {
			return err
		}
		if payload.VMID != destroy.VMID || payload.Phase != "created" || operation.OperationStatus == model.OperationFailed {
			continue
		}
		ref, ok := captured[operation.ID]
		if !ok || ref.SnapshotID != payload.SnapshotID || ref.SnapshotEpoch != payload.SnapshotEpoch || ref.PayloadHash != snapshotPayloadHash(payload) {
			return computeError(http.StatusConflict, "snapshot_set_drift", "active snapshot manifest was not captured by destroy transaction", map[string]any{"operation_id": operation.ID})
		}
	}
	for _, ref := range destroy.SnapshotRefs {
		operation, payload, err := s.loadSnapshotOperation(ctx, ref.OperationID)
		if err != nil {
			return err
		}
		if payload.VMID != destroy.VMID || payload.SnapshotID != ref.SnapshotID || payload.SnapshotEpoch != ref.SnapshotEpoch || snapshotPayloadHash(payload) != ref.PayloadHash ||
			(payload.Phase != "created" && payload.Phase != "deleted") || (operation.OperationStatus != model.OperationRunning && operation.OperationStatus != model.OperationSucceeded) {
			return computeError(http.StatusConflict, "snapshot_set_drift", "captured snapshot manifest changed before destroy commit", map[string]any{"operation_id": ref.OperationID})
		}
	}
	return nil
}

func destroyRequestMatches(input computeDestroyCaptureRequest, node *model.Node, payload computeDestroyPayload) bool {
	return payload.Version == computePayloadVersion && payload.LifecycleID == input.LifecycleID && payload.VMID == input.VMID && payload.Template == input.Template &&
		payload.NodeID == node.ID && payload.Node == node.Name && payload.Chassis == node.ChassisID && blueprintNICsMatch(input.NICs, payload.Blueprints) == nil && snapshotRefsMatch(input.Snapshots, payload.SnapshotRefs)
}

func destroyTransaction(operationID string, payload computeDestroyPayload) computeDestroyTransaction {
	return computeDestroyTransaction{LifecycleID: payload.LifecycleID, VMID: payload.VMID, Template: payload.Template, Node: payload.Node, OperationID: operationID, PayloadHash: destroyPayloadHash(payload), Ports: append([]computeSourcePort(nil), payload.Ports...)}
}

func destroyPayloadHash(payload computeDestroyPayload) string {
	payload.Phase = ""
	return computePayloadHash(payload)
}

func decodeDestroyPayload(operation *model.Operation) (computeDestroyPayload, error) {
	var payload computeDestroyPayload
	if operation.Action != computeDestroyAction || model.UnmarshalOperationPayload(operation.Payload, &payload) != nil || payload.Version != computePayloadVersion || payload.LifecycleID == "" || payload.VMID < 1 || payload.NodeID == "" || payload.Chassis == "" || (len(payload.Blueprints) == 0 && len(payload.SnapshotRefs) == 0) || (payload.Template && payload.TemplateOperationID == "") || (!payload.Template && len(payload.Ports) != len(payload.Blueprints)) {
		return payload, computeError(http.StatusConflict, "destroy_payload_invalid", "durable destroy payload is invalid", map[string]any{"operation_id": operation.ID})
	}
	return payload, nil
}

func (s *Server) loadDestroyOperation(ctx context.Context, id string) (*model.Operation, computeDestroyPayload, error) {
	resource, err := s.store.Get(ctx, model.KindOperation, id)
	if err != nil {
		return nil, computeDestroyPayload{}, err
	}
	operation := resource.(*model.Operation)
	payload, err := decodeDestroyPayload(operation)
	return operation, payload, err
}

func (s *Server) claimDestroyOperation(ctx context.Context, id, fromPhase, claimPhase string) error {
	return s.claimDestroyOperationFrom(ctx, id, []string{fromPhase}, claimPhase)
}

func (s *Server) claimDestroyOperationFrom(ctx context.Context, id string, fromPhases []string, claimPhase string) error {
	return s.claimComputeOperation(ctx, id, computeDestroyAction, fromPhases, claimPhase, func(operation *model.Operation) (any, error) {
		payload, err := decodeDestroyPayload(operation)
		payload.Phase = claimPhase
		return payload, err
	})
}

func (s *Server) terminalizeClaimedDestroyOperation(ctx context.Context, id, claimPhase, terminalPhase string) error {
	return s.terminalizeClaimedDestroyOperationWithStatus(ctx, id, claimPhase, terminalPhase, model.OperationSucceeded, "")
}

func (s *Server) terminalizeClaimedDestroyOperationWithStatus(ctx context.Context, id, claimPhase, terminalPhase string, status model.OperationStatus, failure string) error {
	return s.terminalizeClaimedComputeOperation(ctx, id, computeDestroyAction, claimPhase, terminalPhase, status, failure, func(operation *model.Operation) (any, error) {
		payload, err := decodeDestroyPayload(operation)
		payload.Phase = terminalPhase
		return payload, err
	})
}

func (s *Server) consumeTemplateBlueprint(ctx context.Context, destroy computeDestroyPayload) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		operation, payload, err := s.loadTemplateOperation(ctx, destroy.TemplateOperationID)
		if err != nil {
			return err
		}
		if payload.VMID != destroy.VMID || !reflect.DeepEqual(payload.Blueprints, destroy.Blueprints) {
			return computeError(http.StatusConflict, "template_manifest_drift", "destroy transaction does not own the template blueprint", nil)
		}
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "destroyed" {
			return nil
		}
		if operation.OperationStatus != model.OperationSucceeded || payload.Phase != "committed" {
			return computeError(http.StatusConflict, "template_manifest_unavailable", "template blueprint cannot be consumed from its current phase", map[string]any{"phase": payload.Phase})
		}
		payload.Phase = "destroyed"
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return err
		}
		copyResource, err := model.Clone(operation)
		if err != nil {
			return err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload = encoded
		_, _, err = s.store.Update(ctx, desired, operation.Revision, "compute-template-destroyed-"+operation.ID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "template_manifest_conflict", "template blueprint changed concurrently", nil)
}

func validateSnapshotIdentities(snapshots []computeSnapshotIdentity) error {
	seen := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		if err := validateLifecycleID("snapshot_id", snapshot.SnapshotID); err != nil || snapshot.SnapshotEpoch < 1 {
			return computeError(http.StatusBadRequest, "invalid_request", "destroy snapshot identities require a name and positive epoch", nil)
		}
		key := snapshot.SnapshotID + "\x00" + fmt.Sprint(snapshot.SnapshotEpoch)
		if seen[key] {
			return computeError(http.StatusBadRequest, "invalid_request", "destroy snapshot identities must be unique", nil)
		}
		seen[key] = true
	}
	return nil
}

func sortSnapshotIdentities(snapshots []computeSnapshotIdentity) []computeSnapshotIdentity {
	result := append([]computeSnapshotIdentity(nil), snapshots...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].SnapshotID != result[j].SnapshotID {
			return result[i].SnapshotID < result[j].SnapshotID
		}
		return result[i].SnapshotEpoch < result[j].SnapshotEpoch
	})
	return result
}

func snapshotRefsMatch(input []computeSnapshotIdentity, refs []computeSnapshotRef) bool {
	identities := make([]computeSnapshotIdentity, 0, len(refs))
	for _, ref := range refs {
		identities = append(identities, computeSnapshotIdentity{SnapshotID: ref.SnapshotID, SnapshotEpoch: ref.SnapshotEpoch})
	}
	return reflect.DeepEqual(sortSnapshotIdentities(input), sortSnapshotIdentities(identities))
}

func (s *Server) captureSnapshotRefs(ctx context.Context, vmid int, input []computeSnapshotIdentity) ([]computeSnapshotRef, error) {
	input = sortSnapshotIdentities(input)
	refs := make([]computeSnapshotRef, 0, len(input))
	for _, identity := range input {
		operation, payload, err := s.findSnapshotRecord(ctx, vmid, identity.SnapshotID, identity.SnapshotEpoch)
		if err != nil {
			return nil, err
		}
		if operation.OperationStatus == model.OperationRunning && (payload.Phase == "creating" || payload.Phase == "verifying") {
			claimPhase := payload.Phase
			if err := s.verifySnapshotPorts(ctx, payload); err != nil {
				return nil, err
			}
			if err := s.terminalizeClaimedSnapshotOperation(ctx, operation.ID, claimPhase, "created", model.OperationSucceeded, ""); err != nil {
				return nil, err
			}
			operation, payload, err = s.loadSnapshotOperation(ctx, operation.ID)
			if err != nil {
				return nil, err
			}
		}
		if operation == nil || operation.OperationStatus != model.OperationSucceeded || payload.Phase != "created" {
			return nil, computeError(http.StatusConflict, "snapshot_manifest_unavailable", "destroy snapshot identity is not an active durable manifest", map[string]any{"snapshot_id": identity.SnapshotID, "snapshot_epoch": identity.SnapshotEpoch})
		}
		refs = append(refs, computeSnapshotRef{SnapshotID: identity.SnapshotID, SnapshotEpoch: identity.SnapshotEpoch, OperationID: operation.ID, PayloadHash: snapshotPayloadHash(payload)})
	}
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	active := make([]computeSnapshotIdentity, 0)
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != computeSnapshotAction {
			continue
		}
		payload, err := decodeSnapshotPayload(operation)
		if err != nil {
			return nil, err
		}
		if payload.VMID == vmid && payload.Phase == "created" && operation.OperationStatus != model.OperationFailed {
			active = append(active, computeSnapshotIdentity{SnapshotID: payload.SnapshotID, SnapshotEpoch: payload.SnapshotEpoch})
		}
	}
	if !reflect.DeepEqual(sortSnapshotIdentities(active), input) {
		return nil, computeError(http.StatusConflict, "snapshot_set_mismatch", "PVE snapshot identities do not exactly match all active durable PVN manifests", map[string]any{"vmid": vmid})
	}
	return refs, nil
}

func (s *Server) deleteCapturedSnapshots(ctx context.Context, destroy computeDestroyPayload) error {
	for _, ref := range destroy.SnapshotRefs {
		operation, payload, err := s.loadSnapshotOperation(ctx, ref.OperationID)
		if err != nil {
			return err
		}
		if payload.VMID != destroy.VMID || payload.SnapshotID != ref.SnapshotID || payload.SnapshotEpoch != ref.SnapshotEpoch || snapshotPayloadHash(payload) != ref.PayloadHash {
			return computeError(http.StatusConflict, "snapshot_manifest_drift", "destroy transaction does not own the exact snapshot manifest", map[string]any{"operation_id": ref.OperationID})
		}
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == "deleted" {
			continue
		}
		if err := s.deleteSnapshotOperation(ctx, operation.ID); err != nil {
			return err
		}
	}
	return nil
}
