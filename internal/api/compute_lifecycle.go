package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	computeStartPath          = "/api/v1/runtime/compute/start"
	computeMigrationBeginPath = "/api/v1/runtime/compute/migration/begin"
	computeMigrationFinalPath = "/api/v1/runtime/compute/migration/finalize"
	computeMigrationAbortPath = "/api/v1/runtime/compute/migration/abort"

	computeMigrationAction   = "compute-migration"
	computeHAAction          = "compute-ha-rebind"
	computePayloadVersion    = 1
	computeIntentLifetime    = 15 * time.Minute
	computeRecoveryTimeout   = 15 * time.Minute
	computeHAProofFreshness  = 30 * time.Second
	computeHAProofFutureSkew = 5 * time.Second
	haStabilizationDelay     = 30 * time.Second
	computeClockSkew         = 5 * time.Minute
	maxComputeWriteRetries   = 8
)

type forcedReconciler interface {
	ReconcileForced(context.Context, model.Kind, string) error
}

type computeNIC struct {
	NIC        string `json:"nic"`
	MACAddress string `json:"mac_address"`
}

type computeStartRequest struct {
	LifecycleID     string          `json:"lifecycle_id,omitempty"`
	VMID            int             `json:"vmid"`
	Node            string          `json:"node"`
	NICs            []computeNIC    `json:"nics"`
	MigrationSource string          `json:"migration_source,omitempty"`
	HAManaged       bool            `json:"ha_managed,omitempty"`
	HAProof         *computeHAProof `json:"ha_proof,omitempty"`
}

type computeHAProof struct {
	Origin         string            `json:"origin"`
	ServiceID      string            `json:"service_id"`
	ManagerEpoch   int64             `json:"manager_epoch"`
	ServiceUID     string            `json:"service_uid"`
	ServiceNode    string            `json:"service_node"`
	ServiceState   string            `json:"service_state"`
	NodeStates     map[string]string `json:"node_states"`
	LRMNode        string            `json:"lrm_node"`
	LRMEpoch       int64             `json:"lrm_epoch"`
	LRMState       string            `json:"lrm_state"`
	LRMMode        string            `json:"lrm_mode"`
	AgentLockEpoch int64             `json:"agent_lock_epoch"`
}

type computeHAPayload struct {
	Version           int                      `json:"version"`
	LifecycleID       string                   `json:"lifecycle_id"`
	Phase             string                   `json:"phase"`
	VMID              int                      `json:"vmid"`
	TargetNodeID      string                   `json:"target_node_id"`
	TargetNode        string                   `json:"target_node"`
	Target            string                   `json:"target_chassis"`
	Proof             computeHAProof           `json:"proof"`
	AuthorityHistory  []computeHAProof         `json:"authority_history,omitempty"`
	Ports             []computeHAPortState     `json:"ports"`
	MigrationRecovery *computeHAMigrationAudit `json:"migration_recovery,omitempty"`
}

type computeHAMigrationAudit struct {
	OperationID        string   `json:"operation_id"`
	HistoryOperationID string   `json:"history_operation_id"`
	LifecycleID        string   `json:"lifecycle_id"`
	Direction          string   `json:"direction"`
	OriginalPhase      string   `json:"original_phase"`
	PayloadHash        string   `json:"payload_hash"`
	Online             bool     `json:"online"`
	SourceNodeID       string   `json:"source_node_id"`
	SourceNode         string   `json:"source_node"`
	SourceChassis      string   `json:"source_chassis"`
	TargetNodeID       string   `json:"target_node_id"`
	TargetNode         string   `json:"target_node"`
	TargetChassis      string   `json:"target_chassis"`
	PortIDs            []string `json:"port_ids"`
}

type computeHAPortState struct {
	PortID           string                  `json:"port_id"`
	NIC              string                  `json:"nic"`
	Name             string                  `json:"name"`
	MACAddress       string                  `json:"mac_address"`
	NetworkID        string                  `json:"network_id"`
	FixedIPs         []model.FixedIP         `json:"fixed_ips,omitempty"`
	SecurityGroupIDs []string                `json:"security_group_ids"`
	LSPName          string                  `json:"lsp_name"`
	AdminStateUp     bool                    `json:"admin_state_up"`
	PriorNodeID      string                  `json:"prior_node_id"`
	PriorNode        string                  `json:"prior_node"`
	PriorChassis     string                  `json:"prior_chassis"`
	PriorStatus      model.PortBindingStatus `json:"prior_status"`
	PriorRevision    int64                   `json:"prior_revision"`
	PriorGeneration  int64                   `json:"prior_generation"`
	TargetRevision   int64                   `json:"target_revision"`
	TargetGeneration int64                   `json:"target_generation"`
	Predecessors     []computeHAPortPosition `json:"predecessors,omitempty"`
}

type computeHAPortPosition struct {
	NodeID        string `json:"node_id"`
	Node          string `json:"node"`
	Chassis       string `json:"chassis"`
	RevisionFloor int64  `json:"revision_floor"`
	Generation    int64  `json:"generation"`
}

type computeMigrationBeginRequest struct {
	LifecycleID   string       `json:"lifecycle_id"`
	VMID          int          `json:"vmid"`
	SourceNode    string       `json:"source_node"`
	TargetNode    string       `json:"target_node"`
	Online        bool         `json:"online"`
	SourceStopped bool         `json:"source_stopped,omitempty"`
	SourceMTU     int          `json:"source_mtu,omitempty"`
	TargetMTU     int          `json:"target_mtu,omitempty"`
	NICs          []computeNIC `json:"nics"`
}

type computeMigrationFinishRequest struct {
	LifecycleID string                      `json:"lifecycle_id"`
	VMID        int                         `json:"vmid"`
	SourceNode  string                      `json:"source_node"`
	TargetNode  string                      `json:"target_node"`
	Online      bool                        `json:"online"`
	Transaction computeMigrationTransaction `json:"transaction"`
}

type computeMigrationTransaction struct {
	OperationID string                      `json:"operation_id"`
	PayloadHash string                      `json:"payload_hash"`
	Ports       []computeMigrationPortState `json:"ports"`
}

type computeMigrationPayload struct {
	Version       int                         `json:"version"`
	LifecycleID   string                      `json:"lifecycle_id"`
	Phase         string                      `json:"phase"`
	VMID          int                         `json:"vmid"`
	Online        bool                        `json:"online"`
	SourceStopped bool                        `json:"source_stopped,omitempty"`
	SourceMTU     int                         `json:"source_mtu,omitempty"`
	TargetMTU     int                         `json:"target_mtu,omitempty"`
	SourceNodeID  string                      `json:"source_node_id"`
	SourceNode    string                      `json:"source_node"`
	Source        string                      `json:"source_chassis"`
	TargetNodeID  string                      `json:"target_node_id"`
	TargetNode    string                      `json:"target_node"`
	Target        string                      `json:"target_chassis"`
	StartedAt     time.Time                   `json:"started_at"`
	ExpiresAt     time.Time                   `json:"expires_at"`
	Ports         []computeMigrationPortState `json:"ports"`
	HARecovery    *computeMigrationHARecovery `json:"ha_recovery,omitempty"`
}

type computeMigrationHARecovery struct {
	Direction        string           `json:"direction"`
	OriginalPhase    string           `json:"original_phase"`
	MigrationHash    string           `json:"migration_payload_hash"`
	Proof            computeHAProof   `json:"proof"`
	AuthorityHistory []computeHAProof `json:"authority_history,omitempty"`
}

type computeMigrationPortState struct {
	PortID              string                  `json:"port_id"`
	NIC                 string                  `json:"nic"`
	Name                string                  `json:"name"`
	MACAddress          string                  `json:"mac_address"`
	NetworkID           string                  `json:"network_id"`
	FixedIPs            []model.FixedIP         `json:"fixed_ips,omitempty"`
	SecurityGroupIDs    []string                `json:"security_group_ids"`
	LSPName             string                  `json:"lsp_name"`
	AdminStateUp        bool                    `json:"admin_state_up"`
	SourceBindingStatus model.PortBindingStatus `json:"source_binding_status"`
	SourceRevision      int64                   `json:"source_revision"`
	PreparedRevision    int64                   `json:"prepared_revision"`
	FinalRevision       int64                   `json:"final_revision"`
	SourceGeneration    int64                   `json:"source_generation"`
	Generation          int64                   `json:"generation"`
}

type computeLifecycleError struct {
	status  int
	code    string
	message string
	details any
}

func (err *computeLifecycleError) Error() string { return err.message }

func computeError(status int, code, message string, details any) error {
	return &computeLifecycleError{status: status, code: code, message: message, details: details}
}

func (s *Server) computeLifecycle(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	switch request.URL.Path {
	case computeStartPath:
		s.computeStart(writer, request)
	case computeMigrationBeginPath:
		s.computeMigrationBegin(writer, request)
	case computeMigrationFinalPath:
		s.computeMigrationFinish(writer, request, false)
	case computeMigrationAbortPath:
		s.computeMigrationFinish(writer, request, true)
	case computeClonePreparePath:
		s.computeClonePrepare(writer, request)
	case computeCloneCommitPath:
		s.computeCloneFinish(writer, request, false)
	case computeCloneAbortPath:
		s.computeCloneFinish(writer, request, true)
	case computeTemplatePreparePath:
		s.computeTemplatePrepare(writer, request)
	case computeTemplateCommitPath:
		s.computeTemplateFinish(writer, request, false)
	case computeTemplateAbortPath:
		s.computeTemplateFinish(writer, request, true)
	case computeSnapshotCreatePath:
		s.computeSnapshotCreate(writer, request)
	case computeSnapshotPreparePath:
		s.computeSnapshotPrepare(writer, request)
	case computeSnapshotCommitPath:
		s.computeSnapshotFinish(writer, request, false)
	case computeSnapshotAbortPath:
		s.computeSnapshotFinish(writer, request, true)
	case computeSnapshotCleanupPath:
		s.computeSnapshotCleanup(writer, request)
	case computeDestroyCapturePath:
		s.computeDestroyCapture(writer, request)
	case computeDestroyCommitPath:
		s.computeDestroyFinish(writer, request, false)
	case computeDestroyAbortPath:
		s.computeDestroyFinish(writer, request, true)
	default:
		writeError(writer, http.StatusNotFound, "not_found", "endpoint was not found", nil)
	}
}

func (s *Server) computeStart(writer http.ResponseWriter, request *http.Request) {
	var input computeStartRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateComputeVM(input.VMID, input.NICs); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	target, err := s.readyLocalComputeNode(request.Context(), input.Node)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	ports, err := s.loadExactComputePorts(request.Context(), input.VMID, input.NICs)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if input.HAManaged {
		if input.MigrationSource != "" {
			s.writeComputeError(writer, computeError(http.StatusConflict, "ha_migration_conflict", "incoming migration start cannot also claim HA relocation authority", nil))
			return
		}
		ports, err = s.authorizeHAStart(request.Context(), input, target, ports)
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
			"ready": true, "vmid": input.VMID, "node": target.Name, "chassis_id": target.ChassisID,
			"ports": computeRuntimePorts(ports),
		}})
		return
	}
	if input.HAProof != nil {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "ha_proof requires ha_managed=true", nil))
		return
	}
	if input.MigrationSource == "" {
		ports, err = s.fenceOrdinaryStartOnComputeLifecycle(request.Context(), input, ports)
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
	}

	dual := false
	wrongChassis := false
	for _, port := range ports {
		requested, parseErr := model.ParseRequestedChassis(port.RequestedChassis)
		if parseErr != nil {
			s.writeComputeError(writer, parseErr)
			return
		}
		dual = dual || len(requested) == 2
		wrongChassis = wrongChassis || !model.RequestedChassisContains(port.RequestedChassis, target.ChassisID)
	}
	switch {
	case dual:
		if input.MigrationSource == "" {
			s.writeComputeError(writer, computeError(http.StatusConflict, "migration_intent_required", "dual-chassis PVN ports may start only as the target of their fresh migration transaction", nil))
			return
		}
		if err := s.authorizeMigrationTargetStart(request.Context(), input, target, ports); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	case wrongChassis:
		s.writeComputeError(writer, computeError(http.StatusConflict, "wrong_chassis", "PVN ports are assigned to another chassis and no exact HA recovery proof was supplied", nil))
		return
	case input.MigrationSource != "":
		s.writeComputeError(writer, computeError(http.StatusConflict, "migration_intent_mismatch", "migration_source was supplied for ports without a prepared online migration", nil))
		return
	}

	for index, port := range ports {
		realized, realizeErr := s.forceRealizeComputePort(request.Context(), port)
		if realizeErr != nil {
			s.writeComputeError(writer, realizeErr)
			return
		}
		ports[index] = realized
		if err := validateStartPort(realized, target); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"ready": true, "vmid": input.VMID, "node": target.Name, "chassis_id": target.ChassisID,
		"ports": computeRuntimePorts(ports),
	}})
}

func (s *Server) computeMigrationBegin(writer http.ResponseWriter, request *http.Request) {
	var input computeMigrationBeginRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateComputeMigrationBegin(input, s.guestMTU); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	pinnedSource, err := s.localComputeNode(request.Context(), input.SourceNode)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	pinnedTarget, err := s.resolveAttachmentNode(request.Context(), input.TargetNode)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if pinnedSource.ID == pinnedTarget.ID {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "source_node and target_node must identify different nodes", nil))
		return
	}
	operationID := computeMigrationOperationID(input.LifecycleID, input.VMID, pinnedSource.ID, pinnedTarget.ID)
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	if existing, getErr := s.store.Get(request.Context(), model.KindOperation, operationID); getErr == nil {
		operation := existing.(*model.Operation)
		payload, err := decodeMigrationPayload(operation)
		if err == nil {
			err = validateBeginReplayPayload(input, pinnedSource, pinnedTarget, payload)
		}
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
		if operation.OperationStatus == model.OperationRunning && payload.Phase == "compensating" {
			cause := operation.Error
			if cause == "" {
				cause = "resuming interrupted migration prepare compensation"
			}
			s.writeComputeError(writer, s.failMigrationPrepare(request.Context(), operation, payload, errors.New(cause)))
			return
		}
		if operation.OperationStatus != model.OperationRunning || (payload.Phase != "preparing" && payload.Phase != "prepared") {
			s.writeComputeError(writer, computeError(http.StatusConflict, "migration_id_terminal", "lifecycle_id already belongs to a terminal migration transaction; use a new lifecycle_id", map[string]any{"status": operation.OperationStatus, "phase": payload.Phase}))
			return
		}
		if !s.clusterGate.now().UTC().Before(payload.ExpiresAt) {
			s.writeComputeError(writer, computeError(http.StatusConflict, "migration_intent_expired", "migration transaction expired; explicitly finalize or abort it before starting the VM", map[string]any{"expires_at": payload.ExpiresAt, "transaction": migrationTransaction(operation.ID, payload)}))
			return
		}
		if _, err := s.loadExactPayloadPorts(request.Context(), payload); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		if err := s.prepareMigrationPorts(request.Context(), operation.ID, payload); err != nil {
			s.writeComputeError(writer, s.failMigrationPrepare(request.Context(), operation, payload, err))
			return
		}
		if err := s.markMigrationPrepared(request.Context(), operation.ID); err != nil {
			s.writeComputeError(writer, err)
			return
		}
		payload.Phase = "prepared"
		writer.Header().Set("Idempotency-Replayed", "true")
		s.writeMigrationBeginSuccess(writer, input, pinnedSource, pinnedTarget, operation, payload)
		return
	} else if !errors.Is(getErr, controlstore.ErrNotFound) {
		s.writeComputeError(writer, getErr)
		return
	}
	source, err := s.readyLocalComputeNode(request.Context(), input.SourceNode)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	target, err := s.readyComputeNode(request.Context(), input.TargetNode)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	ports, err := s.loadExactComputePorts(request.Context(), input.VMID, input.NICs)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	operation, payload, replayed, err := s.loadOrCreateMigrationIntent(request.Context(), input, source, target, ports, operationID)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := s.prepareMigrationPorts(request.Context(), operation.ID, payload); err != nil {
		s.writeComputeError(writer, s.failMigrationPrepare(request.Context(), operation, payload, err))
		return
	}
	if err := s.markMigrationPrepared(request.Context(), operation.ID); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	payload.Phase = "prepared"
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	s.writeMigrationBeginSuccess(writer, input, source, target, operation, payload)
}

func (s *Server) computeMigrationFinish(writer http.ResponseWriter, request *http.Request, abort bool) {
	var input computeMigrationFinishRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := validateMigrationFinishRequest(input); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	unlock := lockComputeLifecycle("compute-vm:" + fmt.Sprint(input.VMID))
	defer unlock()
	source, err := s.localComputeNode(request.Context(), input.SourceNode)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	target, err := s.resolveAttachmentNode(request.Context(), input.TargetNode)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	expectedOperationID := computeMigrationOperationID(input.LifecycleID, input.VMID, source.ID, target.ID)
	if input.Transaction.OperationID != expectedOperationID {
		s.writeComputeError(writer, computeError(http.StatusConflict, "migration_transaction_mismatch", "operation_id does not match the migration identity", nil))
		return
	}
	operation, payload, err := s.loadMigrationOperation(request.Context(), expectedOperationID)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := validateMigrationTransaction(input, operation, payload); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if source.ID != payload.SourceNodeID || source.ChassisID != payload.Source || target.ID != payload.TargetNodeID || target.ChassisID != payload.Target {
		s.writeComputeError(writer, computeError(http.StatusConflict, "migration_node_drift", "migration source or target node identity changed after prepare", nil))
		return
	}
	wantedPhase, claimPhase := "finalized", "committing"
	if abort {
		wantedPhase, claimPhase = "aborted", "aborting"
	}
	if operation.OperationStatus != model.OperationRunning {
		if operation.OperationStatus == model.OperationSucceeded && payload.Phase == wantedPhase {
			writer.Header().Set("Idempotency-Replayed", "true")
			writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"lifecycle_id": input.LifecycleID, "state": wantedPhase}})
			return
		}
		s.writeComputeError(writer, computeError(http.StatusConflict, "migration_transaction_terminal", "migration transaction is already terminal in a different phase", map[string]any{"phase": payload.Phase, "status": operation.OperationStatus}))
		return
	}
	if payload.Phase != "prepared" && payload.Phase != claimPhase {
		s.writeComputeError(writer, computeError(http.StatusConflict, "migration_transaction_claimed", "migration transaction is claimed by a different finish action", map[string]any{"phase": payload.Phase}))
		return
	}
	if err := s.claimMigrationOperation(request.Context(), operation.ID, []string{"prepared"}, claimPhase); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	payload.Phase = claimPhase
	if err := s.finishMigrationPorts(request.Context(), operation.ID, payload, abort); err != nil {
		_ = s.recordComputeClaimError(request.Context(), operation.ID, computeMigrationAction, claimPhase, err.Error())
		s.writeComputeError(writer, computeError(http.StatusServiceUnavailable, "migration_finish_failed", err.Error(), map[string]any{
			"operation_id": operation.ID, "recovery_required": true, "transaction": input.Transaction,
		}))
		return
	}
	if err := s.terminalizeClaimedMigrationOperation(request.Context(), operation.ID, claimPhase, wantedPhase, model.OperationSucceeded, ""); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"lifecycle_id": input.LifecycleID, "state": wantedPhase}})
}

func (s *Server) writeMigrationBeginSuccess(writer http.ResponseWriter, input computeMigrationBeginRequest, source, target *model.Node, operation *model.Operation, payload computeMigrationPayload) {
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"lifecycle_id": input.LifecycleID, "vmid": input.VMID, "source_node": source.Name, "target_node": target.Name, "online": input.Online,
		"transaction": migrationTransaction(operation.ID, payload),
	}})
}

func (s *Server) failMigrationPrepare(ctx context.Context, operation *model.Operation, payload computeMigrationPayload, cause error) error {
	recoveryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), computeRecoveryTimeout)
	defer cancel()
	if err := s.claimMigrationOperation(recoveryContext, operation.ID, []string{"preparing", "prepared"}, "compensating"); err != nil {
		return err
	}
	payload.Phase = "compensating"
	compensationErrors := s.compensateMigrationPorts(recoveryContext, operation.ID, payload)
	recoveryRequired := len(compensationErrors) != 0
	if recoveryRequired {
		if err := s.recordComputeClaimError(recoveryContext, operation.ID, computeMigrationAction, "compensating", cause.Error()); err != nil {
			compensationErrors = append(compensationErrors, "record recovery intent: "+err.Error())
		}
	} else if err := s.terminalizeClaimedMigrationOperation(recoveryContext, operation.ID, "compensating", "compensated", model.OperationFailed, cause.Error()); err != nil {
		compensationErrors = append(compensationErrors, "record terminal intent: "+err.Error())
		recoveryRequired = true
	}
	return computeError(http.StatusServiceUnavailable, "migration_prepare_failed", cause.Error(), map[string]any{
		"operation_id": operation.ID, "transaction": migrationTransaction(operation.ID, payload),
		"recovery_required": recoveryRequired, "compensation_errors": compensationErrors,
	})
}

func (s *Server) markMigrationPrepared(ctx context.Context, id string) error {
	return s.claimMigrationOperation(ctx, id, []string{"preparing"}, "prepared")
}

func (s *Server) claimMigrationOperation(ctx context.Context, id string, fromPhases []string, claimPhase string) error {
	return s.claimComputeOperation(ctx, id, computeMigrationAction, fromPhases, claimPhase, func(operation *model.Operation) (any, error) {
		payload, err := decodeMigrationPayload(operation)
		payload.Phase = claimPhase
		return payload, err
	})
}

func (s *Server) terminalizeClaimedMigrationOperation(ctx context.Context, id, claimPhase, terminalPhase string, status model.OperationStatus, failure string) error {
	return s.terminalizeClaimedComputeOperation(ctx, id, computeMigrationAction, claimPhase, terminalPhase, status, failure, func(operation *model.Operation) (any, error) {
		payload, err := decodeMigrationPayload(operation)
		payload.Phase = terminalPhase
		return payload, err
	})
}

func validateBeginReplayPayload(input computeMigrationBeginRequest, source, target *model.Node, payload computeMigrationPayload) error {
	if payload.Version != computePayloadVersion || payload.LifecycleID != input.LifecycleID || payload.VMID != input.VMID || payload.Online != input.Online ||
		payload.SourceStopped != input.SourceStopped || payload.SourceMTU != input.SourceMTU || payload.TargetMTU != input.TargetMTU ||
		payload.SourceNodeID != source.ID || payload.SourceNode != source.Name || payload.Source != source.ChassisID ||
		payload.TargetNodeID != target.ID || payload.TargetNode != target.Name || payload.Target != target.ChassisID {
		return computeError(http.StatusConflict, "migration_id_conflict", "lifecycle_id was reused with different migration parameters", nil)
	}
	if len(input.NICs) != len(payload.Ports) {
		return computeError(http.StatusConflict, "migration_port_set_drift", "migration NIC set differs from the durable transaction", nil)
	}
	byNIC := make(map[string]computeMigrationPortState, len(payload.Ports))
	for _, intent := range payload.Ports {
		byNIC[intent.NIC] = intent
	}
	for _, nic := range input.NICs {
		intent, ok := byNIC[nic.NIC]
		if !ok || !strings.EqualFold(intent.MACAddress, nic.MACAddress) {
			return computeError(http.StatusConflict, "migration_port_set_drift", "migration NIC identity differs from the durable transaction", map[string]any{"nic": nic.NIC})
		}
	}
	return nil
}

func validateComputeVM(vmid int, nics []computeNIC) error {
	if vmid < 1 {
		return computeError(http.StatusBadRequest, "invalid_request", "vmid must be positive", nil)
	}
	if len(nics) == 0 {
		return computeError(http.StatusBadRequest, "invalid_request", "at least one br-int PVN NIC is required", nil)
	}
	seen := make(map[string]bool, len(nics))
	for _, nic := range nics {
		probe := &model.Port{NetworkID: "network", Name: "probe", MACAddress: nic.MACAddress, NIC: nic.NIC}
		if nic.NIC == "" || probe.Validate() != nil {
			return computeError(http.StatusBadRequest, "invalid_request", "every NIC must contain a valid netN name and 48-bit MAC address", nil)
		}
		if seen[nic.NIC] {
			return computeError(http.StatusBadRequest, "invalid_request", "PVN NIC names must be unique", map[string]any{"nic": nic.NIC})
		}
		seen[nic.NIC] = true
	}
	return nil
}

func validateComputeMigrationBegin(input computeMigrationBeginRequest, guestMTU int) error {
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil {
		return err
	}
	if err := validateComputeVM(input.VMID, input.NICs); err != nil {
		return err
	}
	if strings.TrimSpace(input.SourceNode) == "" || strings.TrimSpace(input.TargetNode) == "" {
		return computeError(http.StatusBadRequest, "invalid_request", "source_node and target_node are required", nil)
	}
	if input.Online {
		if input.SourceMTU < guestMTU || input.TargetMTU < guestMTU || input.SourceMTU != input.TargetMTU {
			return computeError(http.StatusConflict, "migration_mtu_mismatch", "online migration requires equal source and target br-int MTUs at least as large as the configured guest MTU", map[string]any{
				"guest_mtu": guestMTU, "source_mtu": input.SourceMTU, "target_mtu": input.TargetMTU,
			})
		}
	} else if !input.SourceStopped {
		return computeError(http.StatusConflict, "source_not_stopped", "offline migration requires proof that the source VM is stopped", nil)
	}
	return nil
}

func validateMigrationFinishRequest(input computeMigrationFinishRequest) error {
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil {
		return err
	}
	if input.VMID < 1 || input.SourceNode == "" || input.TargetNode == "" || input.Transaction.OperationID == "" || input.Transaction.PayloadHash == "" || len(input.Transaction.Ports) == 0 {
		return computeError(http.StatusBadRequest, "invalid_request", "complete migration identity and transaction echo are required", nil)
	}
	return nil
}

func validateLifecycleID(field, value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 200 || strings.ContainsAny(value, "\x00\r\n") {
		return computeError(http.StatusBadRequest, "invalid_request", field+" must be a non-empty clean value no longer than 200 bytes", nil)
	}
	return nil
}

func (s *Server) readyLocalComputeNode(ctx context.Context, identity string) (*model.Node, error) {
	node, err := s.readyComputeNode(ctx, identity)
	if err != nil {
		return nil, err
	}
	local, err := s.localComputeNode(ctx, identity)
	if err != nil {
		return nil, err
	}
	if local.ID != node.ID {
		return nil, computeError(http.StatusConflict, "wrong_compute_node", "root compute listener may authorize work only for its local PVE node", nil)
	}
	if s.computeProbe == nil {
		return nil, computeError(http.StatusServiceUnavailable, "compute_agent_unhealthy", "local pvn-agent watcher health probe is unavailable", nil)
	}
	probeContext, cancel := context.WithTimeout(ctx, s.healthTimeout)
	defer cancel()
	if err := s.computeProbe.Probe(probeContext); err != nil {
		return nil, computeError(http.StatusServiceUnavailable, "compute_agent_unhealthy", "local pvn-agent watcher or OVS health is not ready", map[string]any{"error": err.Error()})
	}
	return node, nil
}

func (s *Server) localComputeNode(ctx context.Context, identity string) (*model.Node, error) {
	node, err := s.resolveAttachmentNode(ctx, identity)
	if err != nil {
		return nil, err
	}
	if s.computeNode == "" {
		return nil, computeError(http.StatusServiceUnavailable, "compute_identity_unavailable", "manager has no configured local compute-node identity", nil)
	}
	local, err := s.resolveAttachmentNode(ctx, s.computeNode)
	if err != nil || local.ID != node.ID {
		return nil, computeError(http.StatusConflict, "wrong_compute_node", "root compute listener may authorize starts only for its local PVE node", map[string]any{"requested_node": node.Name, "local_node": s.computeNode})
	}
	return node, nil
}

func (s *Server) readyComputeNode(ctx context.Context, identity string) (*model.Node, error) {
	node, err := s.resolveAttachmentNode(ctx, identity)
	if err != nil {
		return nil, err
	}
	if !node.Enabled || !slices.Contains(node.Roles, model.NodeRoleCompute) {
		return nil, computeError(http.StatusConflict, "node_unavailable", "node is not an enabled PVN compute node", map[string]any{"node": node.Name})
	}
	now := s.clusterGate.now().UTC()
	if node.State != model.ResourceReady || node.AppliedRevision != node.Revision || node.LastSeenAt == nil ||
		node.LastSeenAt.UTC().After(now.Add(computeClockSkew)) || now.Sub(node.LastSeenAt.UTC()) > s.clusterGate.ttl {
		return nil, computeError(http.StatusServiceUnavailable, "node_not_ready", "node does not have a fresh realized PVN heartbeat", map[string]any{
			"node": node.Name, "state": node.State, "revision": node.Revision, "applied_revision": node.AppliedRevision, "last_seen_at": node.LastSeenAt,
		})
	}
	return node, nil
}

func (s *Server) loadExactComputePorts(ctx context.Context, vmid int, nics []computeNIC) ([]*model.Port, error) {
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: vmid})
	if err != nil {
		return nil, err
	}
	if len(resources) != len(nics) {
		return nil, computeError(http.StatusConflict, "pvn_nic_set_mismatch", "PVE br-int NIC set does not exactly match all PVN-managed VM ports", map[string]any{"vmid": vmid, "configured": len(nics), "managed": len(resources)})
	}
	byNIC := make(map[string]*model.Port, len(resources))
	for _, resource := range resources {
		port := resource.(*model.Port)
		if port.NIC == "" || byNIC[port.NIC] != nil {
			return nil, computeError(http.StatusConflict, "pvn_nic_set_ambiguous", "PVN has an unassigned or duplicate VM NIC port", computePortDetails(port))
		}
		byNIC[port.NIC] = port
	}
	ports := make([]*model.Port, 0, len(nics))
	for _, nic := range nics {
		port := byNIC[nic.NIC]
		if port == nil {
			return nil, computeError(http.StatusConflict, "pvn_nic_set_mismatch", "PVE br-int NIC has no exact PVN port", map[string]any{"vmid": vmid, "nic": nic.NIC})
		}
		if !strings.EqualFold(port.MACAddress, nic.MACAddress) {
			return nil, computeError(http.StatusConflict, "pvn_mac_mismatch", "PVE NIC MAC does not match its PVN port", map[string]any{"vmid": vmid, "nic": nic.NIC, "configured_mac": strings.ToLower(nic.MACAddress), "pvn_mac": strings.ToLower(port.MACAddress)})
		}
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].NIC < ports[j].NIC })
	return ports, nil
}

func (s *Server) loadExactPayloadPorts(ctx context.Context, payload computeMigrationPayload) ([]*model.Port, error) {
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: payload.VMID})
	if err != nil {
		return nil, err
	}
	if len(resources) != len(payload.Ports) {
		return nil, computeError(http.StatusConflict, "migration_port_set_drift", "managed VM port set changed after migration prepare", map[string]any{"expected": len(payload.Ports), "current": len(resources)})
	}
	byID := make(map[string]*model.Port, len(resources))
	for _, resource := range resources {
		port := resource.(*model.Port)
		byID[port.ID] = port
	}
	ports := make([]*model.Port, 0, len(payload.Ports))
	seen := make(map[string]bool, len(payload.Ports))
	for _, intent := range payload.Ports {
		if seen[intent.PortID] || intent.NIC == "" {
			return nil, computeError(http.StatusConflict, "migration_port_set_invalid", "durable migration manifest contains duplicate or invalid ports", nil)
		}
		seen[intent.PortID] = true
		port := byID[intent.PortID]
		if port == nil || port.NIC != intent.NIC || !strings.EqualFold(port.MACAddress, intent.MACAddress) {
			return nil, computeError(http.StatusConflict, "migration_port_set_drift", "managed VM port identity changed after migration prepare", map[string]any{"port_id": intent.PortID, "nic": intent.NIC})
		}
		ports = append(ports, port)
	}
	return ports, nil
}

func validateStartPort(port *model.Port, node *model.Node) error {
	if !port.AdminStateUp || (port.BindingStatus != model.PortBinding && port.BindingStatus != model.PortBound) {
		return computeError(http.StatusConflict, "port_disconnected", "PVN NIC is administratively down or disconnected", computePortDetails(port))
	}
	if port.State != model.ResourceReady || port.AppliedRevision != port.Revision || port.LSPName == "" || port.Generation < 1 {
		return computeError(http.StatusConflict, "port_not_realized", "PVN NIC has not been realized in OVN", computePortDetails(port))
	}
	if !model.RequestedChassisContains(port.RequestedChassis, node.ChassisID) {
		return computeError(http.StatusConflict, "wrong_chassis", "PVN NIC is not assigned to the local chassis", computePortDetails(port))
	}
	return nil
}

func computePortDetails(port *model.Port) map[string]any {
	return map[string]any{"port_id": port.ID, "vmid": port.VMID, "nic": port.NIC, "status": port.BindingStatus, "state": port.State, "revision": port.Revision, "applied_revision": port.AppliedRevision, "generation": port.Generation, "requested_chassis": port.RequestedChassis}
}

func computeRuntimePorts(ports []*model.Port) []map[string]any {
	result := make([]map[string]any, 0, len(ports))
	for _, port := range ports {
		result = append(result, map[string]any{"port_id": port.ID, "nic": port.NIC, "generation": port.Generation, "status": port.BindingStatus, "requested_chassis": port.RequestedChassis})
	}
	return result
}

func (s *Server) forceRealizeComputePort(ctx context.Context, port *model.Port) (*model.Port, error) {
	reconciler, ok := s.reconciler.(forcedReconciler)
	if !ok {
		return nil, computeError(http.StatusServiceUnavailable, "reconciler_unavailable", "forced OVN drift reconciliation is unavailable for VM start", computePortDetails(port))
	}
	if err := reconciler.ReconcileForced(ctx, model.KindPort, port.ID); err != nil {
		return nil, computeError(http.StatusServiceUnavailable, "reconcile_failed", "PVN could not verify or repair the port in OVN", map[string]any{"port_id": port.ID, "error": err.Error()})
	}
	latest, err := s.loadPort(ctx, port.ID)
	if err != nil {
		return nil, err
	}
	if latest.State != model.ResourceReady || latest.AppliedRevision != latest.Revision {
		return nil, computeError(http.StatusServiceUnavailable, "port_not_realized", "forced OVN reconciliation did not realize the exact port revision", computePortDetails(latest))
	}
	return latest, nil
}

func (s *Server) loadOrCreateMigrationIntent(ctx context.Context, input computeMigrationBeginRequest, source, target *model.Node, ports []*model.Port, operationID string) (*model.Operation, computeMigrationPayload, bool, error) {
	if existing, err := s.store.Get(ctx, model.KindOperation, operationID); err == nil {
		operation := existing.(*model.Operation)
		payload, loadErr := decodeMigrationPayload(operation)
		if loadErr != nil {
			return nil, computeMigrationPayload{}, false, loadErr
		}
		if operation.Action != computeMigrationAction || operation.OperationStatus != model.OperationRunning || (payload.Phase != "preparing" && payload.Phase != "prepared") {
			return nil, computeMigrationPayload{}, false, computeError(http.StatusConflict, "migration_id_terminal", "lifecycle_id already belongs to a terminal or invalid migration transaction; use a new lifecycle_id", map[string]any{"status": operation.OperationStatus, "phase": payload.Phase})
		}
		if err := validateBeginReplay(input, source, target, ports, payload); err != nil {
			return nil, computeMigrationPayload{}, false, err
		}
		if !s.clusterGate.now().UTC().Before(payload.ExpiresAt) {
			return nil, computeMigrationPayload{}, false, computeError(http.StatusConflict, "migration_intent_expired", "migration transaction expired; abort it and use a new lifecycle_id", map[string]any{"expires_at": payload.ExpiresAt})
		}
		return operation, payload, true, nil
	} else if !errors.Is(err, controlstore.ErrNotFound) {
		return nil, computeMigrationPayload{}, false, err
	}
	payload, err := newMigrationPayload(s.clusterGate.now().UTC(), input, source, target, ports)
	if err != nil {
		return nil, computeMigrationPayload{}, false, err
	}
	created, err := s.createComputeOperation(ctx, operationID, computeMigrationAction, input.VMID, payload)
	if err != nil {
		var lifecycle *computeLifecycleError
		if errors.As(err, &lifecycle) && lifecycle.code == "compute_vm_busy" {
			return nil, computeMigrationPayload{}, false, computeError(http.StatusConflict, "migration_already_active", "VM already has an active compute lifecycle transaction", map[string]any{"vmid": input.VMID})
		}
		return nil, computeMigrationPayload{}, false, err
	}
	return created, payload, false, nil
}

func newMigrationPayload(now time.Time, input computeMigrationBeginRequest, source, target *model.Node, ports []*model.Port) (computeMigrationPayload, error) {
	payload := computeMigrationPayload{
		Version: computePayloadVersion, LifecycleID: input.LifecycleID, Phase: "preparing", VMID: input.VMID, Online: input.Online,
		SourceStopped: input.SourceStopped, SourceMTU: input.SourceMTU, TargetMTU: input.TargetMTU,
		SourceNodeID: source.ID, SourceNode: source.Name, Source: source.ChassisID,
		TargetNodeID: target.ID, TargetNode: target.Name, Target: target.ChassisID,
		StartedAt: now, ExpiresAt: now.Add(computeIntentLifetime),
		Ports: make([]computeMigrationPortState, 0, len(ports)),
	}
	for _, port := range ports {
		if port.Generation < 1 || port.Generation == math.MaxInt64 || port.Revision < 1 || port.Revision > math.MaxInt64-2 {
			return computeMigrationPayload{}, computeError(http.StatusConflict, "generation_exhausted", "PVN port has no usable migration generation", computePortDetails(port))
		}
		if port.NodeID != source.ID || port.RequestedChassis != source.ChassisID || !migrationBindingReady(port.BindingStatus) || !port.AdminStateUp || port.State != model.ResourceReady || port.AppliedRevision != port.Revision {
			return computeMigrationPayload{}, computeError(http.StatusConflict, "migration_source_mismatch", "every PVN port must be connected and exclusively assigned to the declared source", computePortDetails(port))
		}
		groups := append([]string(nil), port.SecurityGroupIDs...)
		sort.Strings(groups)
		payload.Ports = append(payload.Ports, computeMigrationPortState{
			PortID: port.ID, NIC: port.NIC, Name: port.Name, MACAddress: strings.ToLower(port.MACAddress), NetworkID: port.NetworkID,
			FixedIPs: append([]model.FixedIP(nil), port.FixedIPs...), SecurityGroupIDs: groups, LSPName: port.LSPName, AdminStateUp: port.AdminStateUp, SourceBindingStatus: port.BindingStatus,
			SourceRevision:   port.Revision,
			PreparedRevision: port.Revision + 1, FinalRevision: port.Revision + 2, SourceGeneration: port.Generation, Generation: port.Generation + 1,
		})
	}
	sort.Slice(payload.Ports, func(i, j int) bool { return payload.Ports[i].NIC < payload.Ports[j].NIC })
	return payload, nil
}

func validateBeginReplay(input computeMigrationBeginRequest, source, target *model.Node, ports []*model.Port, payload computeMigrationPayload) error {
	if payload.Version != computePayloadVersion || payload.LifecycleID != input.LifecycleID || payload.VMID != input.VMID || payload.Online != input.Online ||
		payload.SourceStopped != input.SourceStopped || payload.SourceMTU != input.SourceMTU || payload.TargetMTU != input.TargetMTU ||
		payload.SourceNodeID != source.ID || payload.SourceNode != source.Name || payload.Source != source.ChassisID ||
		payload.TargetNodeID != target.ID || payload.TargetNode != target.Name || payload.Target != target.ChassisID {
		return computeError(http.StatusConflict, "migration_id_conflict", "lifecycle_id was reused with different migration parameters", nil)
	}
	if len(payload.Ports) != len(ports) {
		return computeError(http.StatusConflict, "migration_port_set_drift", "managed VM port set changed during migration prepare", nil)
	}
	for index, port := range ports {
		intent := payload.Ports[index]
		if intent.PortID != port.ID || intent.NIC != port.NIC || !strings.EqualFold(intent.MACAddress, port.MACAddress) {
			return computeError(http.StatusConflict, "migration_port_set_drift", "managed VM port identity changed during migration prepare", computePortDetails(port))
		}
	}
	return nil
}

func (s *Server) ensureNoActiveMigration(ctx context.Context, vmid int, ownID string) error {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.ID == ownID || operation.Action != computeMigrationAction || operation.OperationStatus != model.OperationRunning {
			continue
		}
		payload, decodeErr := decodeMigrationPayload(operation)
		if decodeErr != nil {
			return decodeErr
		}
		if payload.VMID == vmid {
			return computeError(http.StatusConflict, "migration_already_active", "VM already has an active or abandoned migration transaction", map[string]any{"operation_id": operation.ID, "lifecycle_id": payload.LifecycleID, "expires_at": payload.ExpiresAt})
		}
	}
	return nil
}

func (s *Server) prepareMigrationPorts(ctx context.Context, operationID string, payload computeMigrationPayload) error {
	ports, err := s.loadExactPayloadPorts(ctx, payload)
	if err != nil {
		return err
	}
	for index, current := range ports {
		if err := s.assertMigrationOperationOwner(ctx, operationID, payload); err != nil {
			return err
		}
		intent := payload.Ports[index]
		if !migrationPortConfigMatches(current, payload.VMID, intent) {
			return computeError(http.StatusConflict, "migration_port_drift", "PVN port immutable configuration changed during migration", computePortDetails(current))
		}
		desiredNodeID, desiredChassis := payload.TargetNodeID, payload.Target
		if payload.Online {
			desiredNodeID, desiredChassis = payload.SourceNodeID, payload.Source+","+payload.Target
		}
		if current.Generation == intent.Generation && current.NodeID == desiredNodeID && current.RequestedChassis == desiredChassis && migrationBindingReady(current.BindingStatus) {
			if !migrationPhaseRevisionMatches(current, intent.PreparedRevision) {
				return computeError(http.StatusConflict, "migration_revision_drift", "prepared PVN port revision differs from its durable transaction", computePortDetails(current))
			}
			if _, err := s.forceRealizeComputePort(ctx, current); err != nil {
				return err
			}
			continue
		}
		if current.Generation != intent.SourceGeneration || !migrationPhaseRevisionMatches(current, intent.SourceRevision) || current.NodeID != payload.SourceNodeID || current.RequestedChassis != payload.Source {
			return computeError(http.StatusConflict, "migration_intent_drift", "PVN port is neither the exact source nor prepared migration state", computePortDetails(current))
		}
		desired := clonePort(current)
		desired.Metadata = model.Metadata{ID: current.ID}
		desired.NodeID, desired.RequestedChassis = desiredNodeID, desiredChassis
		desired.BindingStatus = model.PortBinding
		desired.Generation = intent.Generation
		updated, _, err := s.store.Update(ctx, desired, current.Revision, "compute-migration-prepare-"+payload.LifecycleID+"-"+current.ID)
		if err != nil {
			return err
		}
		if updated.GetMetadata().Revision < intent.PreparedRevision {
			return computeError(http.StatusConflict, "migration_revision_drift", "prepared PVN port revision differs from its durable transaction", computePortDetails(updated.(*model.Port)))
		}
		if !migrationPortConfigMatches(updated.(*model.Port), payload.VMID, intent) {
			return computeError(http.StatusConflict, "migration_port_drift", "migration prepare changed immutable PVN port configuration", computePortDetails(updated.(*model.Port)))
		}
		if _, err := s.forceRealizeComputePort(ctx, updated.(*model.Port)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) compensateMigrationPorts(ctx context.Context, operationID string, payload computeMigrationPayload) []string {
	failures := make([]string, 0)
	for _, intent := range payload.Ports {
		if err := s.assertMigrationOperationOwner(ctx, operationID, payload); err != nil {
			failures = append(failures, "migration ownership: "+err.Error())
			break
		}
		current, err := s.loadPort(ctx, intent.PortID)
		if err != nil {
			failures = append(failures, intent.PortID+": "+err.Error())
			continue
		}
		preparedNodeID, preparedChassis := payload.TargetNodeID, payload.Target
		if payload.Online {
			preparedNodeID, preparedChassis = payload.SourceNodeID, payload.Source+","+payload.Target
		}
		switch {
		case current.NodeID == payload.SourceNodeID && current.RequestedChassis == payload.Source &&
			migrationPortConfigMatches(current, payload.VMID, intent) &&
			((current.Generation == intent.SourceGeneration && migrationPhaseRevisionMatches(current, intent.SourceRevision)) ||
				(current.Generation == intent.Generation && migrationPhaseRevisionMatches(current, intent.FinalRevision))):
			// Already source-only. The higher generation is retained as a fence.
		case current.NodeID == preparedNodeID && current.RequestedChassis == preparedChassis && current.Generation == intent.Generation &&
			migrationPhaseRevisionMatches(current, intent.PreparedRevision) && migrationPortConfigMatches(current, payload.VMID, intent):
			desired := clonePort(current)
			desired.Metadata = model.Metadata{ID: current.ID}
			desired.NodeID, desired.RequestedChassis = payload.SourceNodeID, payload.Source
			desired.BindingStatus = model.PortBinding
			updated, _, updateErr := s.store.Update(ctx, desired, current.Revision, "compute-migration-compensate-"+payload.LifecycleID+"-"+current.ID)
			if updateErr != nil {
				failures = append(failures, current.ID+": "+updateErr.Error())
				continue
			}
			current = updated.(*model.Port)
			if current.Revision < intent.FinalRevision || !migrationPortConfigMatches(current, payload.VMID, intent) {
				failures = append(failures, current.ID+": compensation produced an unexpected durable port state")
				continue
			}
		default:
			failures = append(failures, current.ID+": port changed outside the migration transaction")
			continue
		}
		if _, realizeErr := s.forceRealizeComputePort(ctx, current); realizeErr != nil {
			failures = append(failures, current.ID+": "+realizeErr.Error())
		}
	}
	return failures
}

func (s *Server) finishMigrationPorts(ctx context.Context, operationID string, payload computeMigrationPayload, abort bool) error {
	ports, err := s.loadExactPayloadPorts(ctx, payload)
	if err != nil {
		return err
	}
	for index, current := range ports {
		if err := s.assertMigrationOperationOwner(ctx, operationID, payload); err != nil {
			return err
		}
		intent := payload.Ports[index]
		if !migrationPortConfigMatches(current, payload.VMID, intent) {
			return computeError(http.StatusConflict, "migration_port_drift", "PVN port immutable configuration changed before migration completion", computePortDetails(current))
		}
		if current.Generation != intent.Generation {
			return computeError(http.StatusConflict, "stale_generation", "migration completion was fenced by a newer PVN port generation", computePortDetails(current))
		}
		desiredNodeID, desiredChassis := payload.TargetNodeID, payload.Target
		if abort {
			desiredNodeID, desiredChassis = payload.SourceNodeID, payload.Source
		}
		if current.NodeID == desiredNodeID && current.RequestedChassis == desiredChassis && migrationBindingReady(current.BindingStatus) {
			expectedRevision := intent.FinalRevision
			if !payload.Online && !abort {
				expectedRevision = intent.PreparedRevision
			}
			if !migrationPhaseRevisionMatches(current, expectedRevision) {
				return computeError(http.StatusConflict, "migration_revision_drift", "completed PVN port revision differs from its durable transaction", computePortDetails(current))
			}
			if _, err := s.forceRealizeComputePort(ctx, current); err != nil {
				return err
			}
			continue
		}
		preparedNodeID, preparedChassis := payload.TargetNodeID, payload.Target
		if payload.Online {
			preparedNodeID, preparedChassis = payload.SourceNodeID, payload.Source+","+payload.Target
		}
		if current.NodeID != preparedNodeID || current.RequestedChassis != preparedChassis || !migrationPhaseRevisionMatches(current, intent.PreparedRevision) {
			return computeError(http.StatusConflict, "migration_intent_drift", "PVN port is outside the exact prepared migration transaction", computePortDetails(current))
		}
		desired := clonePort(current)
		desired.Metadata = model.Metadata{ID: current.ID}
		desired.NodeID, desired.RequestedChassis = desiredNodeID, desiredChassis
		desired.BindingStatus = model.PortBinding
		phase := "finalize"
		if abort {
			phase = "abort"
		}
		updated, _, err := s.store.Update(ctx, desired, current.Revision, "compute-migration-"+phase+"-"+payload.LifecycleID+"-"+current.ID)
		if err != nil {
			return err
		}
		if _, err := s.forceRealizeComputePort(ctx, updated.(*model.Port)); err != nil {
			return err
		}
		if updated.GetMetadata().Revision < intent.FinalRevision {
			return computeError(http.StatusConflict, "migration_revision_drift", "completed PVN port revision differs from its durable transaction", computePortDetails(updated.(*model.Port)))
		}
		if !migrationPortConfigMatches(updated.(*model.Port), payload.VMID, intent) {
			return computeError(http.StatusConflict, "migration_port_drift", "migration completion changed immutable PVN port configuration", computePortDetails(updated.(*model.Port)))
		}
	}
	return nil
}

func (s *Server) authorizeMigrationTargetStart(ctx context.Context, input computeStartRequest, target *model.Node, ports []*model.Port) error {
	source, err := s.resolveAttachmentNode(ctx, input.MigrationSource)
	if err != nil {
		return computeError(http.StatusConflict, "migration_intent_mismatch", "migration_source is not a registered PVN node", nil)
	}
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	type candidate struct {
		operation *model.Operation
		payload   computeMigrationPayload
	}
	candidates := make([]candidate, 0, 1)
	var expired *computeMigrationPayload
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != computeMigrationAction || operation.OperationStatus != model.OperationRunning {
			continue
		}
		payload, decodeErr := decodeMigrationPayload(operation)
		if decodeErr != nil {
			return decodeErr
		}
		if payload.Phase != "prepared" || !payload.Online || payload.VMID != input.VMID || payload.SourceNodeID != source.ID || payload.TargetNodeID != target.ID ||
			(input.LifecycleID != "" && payload.LifecycleID != input.LifecycleID) {
			continue
		}
		if !s.clusterGate.now().UTC().Before(payload.ExpiresAt) {
			copyPayload := payload
			expired = &copyPayload
			continue
		}
		if migrationPortsMatchPrepared(payload, ports) {
			candidates = append(candidates, candidate{operation: operation, payload: payload})
		}
	}
	if len(candidates) == 0 {
		if expired != nil {
			return computeError(http.StatusConflict, "migration_intent_expired", "online migration target-start authorization expired", map[string]any{"expires_at": expired.ExpiresAt})
		}
		return computeError(http.StatusConflict, "migration_intent_mismatch", "no exact fresh online migration transaction authorizes this target start", nil)
	}
	if len(candidates) != 1 {
		ids := make([]string, 0, len(candidates))
		for _, item := range candidates {
			ids = append(ids, item.operation.ID)
		}
		return computeError(http.StatusConflict, "migration_intent_ambiguous", "more than one active migration transaction matches target start", map[string]any{"operation_ids": ids})
	}
	return nil
}

func migrationPortsMatchPrepared(payload computeMigrationPayload, ports []*model.Port) bool {
	if len(ports) != len(payload.Ports) {
		return false
	}
	for index, port := range ports {
		intent := payload.Ports[index]
		if !migrationPortConfigMatches(port, payload.VMID, intent) ||
			port.Generation != intent.Generation || !migrationPhaseRevisionMatches(port, intent.PreparedRevision) || port.NodeID != payload.SourceNodeID || port.RequestedChassis != payload.Source+","+payload.Target {
			return false
		}
	}
	return true
}

func migrationPortConfigMatches(port *model.Port, vmid int, intent computeMigrationPortState) bool {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	return port.ID == intent.PortID && port.VMID == vmid && port.NIC == intent.NIC && port.Name == intent.Name &&
		strings.EqualFold(port.MACAddress, intent.MACAddress) && port.NetworkID == intent.NetworkID && reflect.DeepEqual(port.FixedIPs, intent.FixedIPs) &&
		slices.Equal(groups, intent.SecurityGroupIDs) && port.LSPName == intent.LSPName && port.AdminStateUp == intent.AdminStateUp
}

func migrationBindingReady(status model.PortBindingStatus) bool {
	return status == model.PortBinding || status == model.PortBound
}

func migrationPhaseRevisionMatches(port *model.Port, floor int64) bool {
	return port.Revision >= floor && migrationBindingReady(port.BindingStatus)
}

func (s *Server) fenceOrdinaryStartOnComputeLifecycle(ctx context.Context, input computeStartRequest, ports []*model.Port) ([]*model.Port, error) {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	candidates := make([]*model.Operation, 0, 1)
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.OperationStatus != model.OperationRunning || operation.TargetKind != model.KindNode || operation.TargetID != computeVMOperationTarget(input.VMID) || operation.TargetRevision != 1 {
			continue
		}
		candidates = append(candidates, operation)
	}
	if len(candidates) == 0 {
		return ports, nil
	}
	if len(candidates) != 1 {
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.ID)
		}
		return nil, computeError(http.StatusConflict, "compute_lifecycle_ambiguous", "multiple active compute lifecycle transactions fence ordinary VM start", map[string]any{"operation_ids": ids})
	}
	selected := candidates[0]
	if selected.Action == computeMigrationAction {
		payload, decodeErr := decodeMigrationPayload(selected)
		if decodeErr != nil {
			return nil, decodeErr
		}
		expired := !s.clusterGate.now().UTC().Before(payload.ExpiresAt)
		return nil, computeError(http.StatusConflict, "migration_intent_required", "an active migration transaction fences ordinary VM start; explicitly finalize or abort it", map[string]any{
			"operation_id": selected.ID, "lifecycle_id": payload.LifecycleID, "source_node": payload.SourceNode, "target_node": payload.TargetNode,
			"phase": payload.Phase, "expires_at": payload.ExpiresAt, "expired": expired, "transaction": migrationTransaction(selected.ID, payload),
		})
	}
	phase, phaseErr := computeOperationPhase(selected)
	if phaseErr != nil {
		return nil, phaseErr
	}
	return nil, computeError(http.StatusConflict, "compute_lifecycle_active", "an active compute lifecycle transaction fences ordinary VM start", map[string]any{
		"operation_id": selected.ID, "action": selected.Action, "phase": phase,
	})
}

func migrationPortIdentitiesMatch(payload computeMigrationPayload, ports []*model.Port) bool {
	if len(payload.Ports) != len(ports) {
		return false
	}
	for index, port := range ports {
		intent := payload.Ports[index]
		if port.ID != intent.PortID || port.NIC != intent.NIC || !strings.EqualFold(port.MACAddress, intent.MACAddress) {
			return false
		}
	}
	return true
}

func (s *Server) authorizeHAStart(ctx context.Context, input computeStartRequest, target *model.Node, ports []*model.Port) ([]*model.Port, error) {
	if input.HAProof == nil {
		return nil, computeError(http.StatusConflict, "ha_proof_required", "HA-managed VM start requires a fresh PVE CRM/LRM authority proof", nil)
	}
	if err := validateHAProofIdentity(input, target, *input.HAProof, s.clusterGate.now().UTC()); err != nil {
		return nil, err
	}
	if err := s.validateHAClusterAuthority(ctx, target, *input.HAProof, nil); err != nil {
		return nil, err
	}
	if err := s.enforceHAAuthorityHistory(ctx, input, target); err != nil {
		return nil, err
	}
	operationID := computeResourceOperationID(computeHAAction, input.LifecycleID, input.VMID)
	var operation *model.Operation
	var payload computeHAPayload
	adoption, matched, err := s.recoverMigrationForHA(ctx, input, target, ports)
	if err != nil {
		return nil, err
	}
	if matched {
		operation, payload, ports = adoption.Operation, adoption.Payload, adoption.Ports
	} else {
		existing, existingPayload, findErr := s.findHAOperationForLifecycle(ctx, input, target, operationID)
		if findErr != nil {
			return nil, findErr
		}
		payload = existingPayload
		if existing != nil {
			operation = existing
			if err := validateHAReplayIdentity(input, target, payload, *input.HAProof); err != nil {
				return nil, err
			}
			if err := s.validateHAClusterAuthority(ctx, target, *input.HAProof, haPriorNodes(payload, target.Name)); err != nil {
				return nil, err
			}
			operation, payload, err = s.claimHAStartOperation(ctx, operation.ID, input, target, *input.HAProof)
			if err != nil {
				return nil, err
			}
		} else {
			var adopted bool
			operation, payload, adopted, err = s.supersedeHAOperation(ctx, input, target)
			if err != nil {
				return nil, err
			}
			if !adopted {
				payload, err = s.newHAPayload(ctx, input, target, ports)
				if err != nil {
					return nil, err
				}
				if err := s.validateHAClusterAuthority(ctx, target, payload.Proof, haPriorNodes(payload, target.Name)); err != nil {
					return nil, err
				}
				operation, err = s.createComputeOperation(ctx, operationID, computeHAAction, input.VMID, payload)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	result, err := s.applyHAPorts(ctx, operation.ID, payload)
	if err != nil {
		_ = s.recordHAClaimError(ctx, operation.ID, payload, err.Error())
		return nil, computeError(http.StatusServiceUnavailable, "ha_rebind_failed", err.Error(), map[string]any{"operation_id": operation.ID, "recovery_required": true})
	}
	if err := s.terminalizeClaimedHAOperation(ctx, operation.ID, payload, "ready", model.OperationSucceeded, ""); err != nil {
		return nil, err
	}
	return result, nil
}

type computeHAMigrationAdoption struct {
	Operation *model.Operation
	Payload   computeHAPayload
	Ports     []*model.Port
}

// recoverMigrationForHA deterministically completes the direction already
// chosen by a migration, then atomically promotes that same operation to the HA
// action without releasing its VM slot. The fresh HA proof fences every
// possible old owner before either recovery or promotion.
func (s *Server) recoverMigrationForHA(ctx context.Context, input computeStartRequest, target *model.Node, ports []*model.Port) (*computeHAMigrationAdoption, bool, error) {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return nil, false, err
	}
	var operation *model.Operation
	var payload computeMigrationPayload
	for _, resource := range resources {
		candidate := resource.(*model.Operation)
		if candidate.Action != computeMigrationAction || candidate.OperationStatus != model.OperationRunning {
			continue
		}
		candidatePayload, decodeErr := decodeMigrationPayload(candidate)
		if decodeErr != nil {
			return nil, true, decodeErr
		}
		if candidatePayload.VMID != input.VMID {
			continue
		}
		if operation != nil {
			return nil, true, computeError(http.StatusConflict, "migration_intent_ambiguous", "multiple active migration transactions prevent HA recovery", map[string]any{"vmid": input.VMID})
		}
		operation, payload = candidate, candidatePayload
	}
	if operation == nil {
		return nil, false, nil
	}
	direction, ok := migrationHARecoveryDirection(payload)
	if !ok {
		return nil, true, computeError(http.StatusConflict, "ha_migration_phase_unsafe", "HA cannot take over a migration worker that may still be mutating ports", map[string]any{
			"operation_id": operation.ID, "phase": payload.Phase,
		})
	}
	if !migrationPortIdentitiesMatch(payload, ports) {
		return nil, true, computeError(http.StatusConflict, "migration_port_set_drift", "PVE NICs do not match the abandoned migration manifest", nil)
	}
	source, err := s.resolveAttachmentNode(ctx, payload.SourceNodeID)
	if err != nil || source.Name != payload.SourceNode || source.ChassisID != payload.Source {
		return nil, true, computeError(http.StatusConflict, "migration_node_drift", "migration source identity changed before HA recovery", nil)
	}
	oldTarget, err := s.resolveAttachmentNode(ctx, payload.TargetNodeID)
	if err != nil || oldTarget.Name != payload.TargetNode || oldTarget.ChassisID != payload.Target {
		return nil, true, computeError(http.StatusConflict, "migration_node_drift", "migration target identity changed before HA recovery", nil)
	}
	prior := make([]computeHAPriorNode, 0, 2)
	for _, node := range []*model.Node{source, oldTarget} {
		if node.ID != target.ID {
			prior = append(prior, computeHAPriorNode{ID: node.ID, Name: node.Name, Chassis: node.ChassisID})
		}
	}
	if err := s.validateHAClusterAuthority(ctx, target, *input.HAProof, prior); err != nil {
		return nil, true, err
	}
	operation, payload, err = s.claimHAMigrationRecovery(ctx, operation.ID, direction, *input.HAProof)
	if err != nil {
		return nil, true, err
	}
	claimPhase := "ha-recovering-" + direction
	if direction == "source" {
		failures := s.compensateMigrationPorts(ctx, operation.ID, payload)
		if len(failures) != 0 {
			cause := errors.New(strings.Join(failures, "; "))
			_ = s.recordComputeClaimError(ctx, operation.ID, computeMigrationAction, claimPhase, cause.Error())
			return nil, true, computeError(http.StatusServiceUnavailable, "ha_migration_recovery_failed", cause.Error(), map[string]any{"operation_id": operation.ID, "phase": claimPhase, "recovery_required": true})
		}
	} else if err := s.finishMigrationPorts(ctx, operation.ID, payload, false); err != nil {
		_ = s.recordComputeClaimError(ctx, operation.ID, computeMigrationAction, claimPhase, err.Error())
		return nil, true, computeError(http.StatusServiceUnavailable, "ha_migration_recovery_failed", err.Error(), map[string]any{"operation_id": operation.ID, "phase": claimPhase, "recovery_required": true})
	}
	recovered, err := s.loadExactPayloadPorts(ctx, payload)
	if err != nil {
		return nil, true, err
	}
	if err := validateRecoveredMigrationPorts(payload, direction, recovered); err != nil {
		return nil, true, err
	}
	promoted, haPayload, err := s.promoteMigrationRecoveryToHA(ctx, operation, payload, input, target, recovered)
	if err != nil {
		return nil, true, err
	}
	return &computeHAMigrationAdoption{Operation: promoted, Payload: haPayload, Ports: recovered}, true, nil
}

func validateRecoveredMigrationPorts(payload computeMigrationPayload, direction string, ports []*model.Port) error {
	if len(ports) != len(payload.Ports) {
		return computeError(http.StatusConflict, "migration_port_set_drift", "recovered migration port set differs from its durable manifest", nil)
	}
	for index, port := range ports {
		intent := payload.Ports[index]
		if !migrationPortConfigMatches(port, payload.VMID, intent) || !migrationBindingReady(port.BindingStatus) {
			return computeError(http.StatusConflict, "migration_port_drift", "recovered migration port immutable configuration changed before HA promotion", computePortDetails(port))
		}
		valid := false
		if direction == "source" && port.NodeID == payload.SourceNodeID && port.RequestedChassis == payload.Source {
			valid = (port.Generation == intent.SourceGeneration && migrationPhaseRevisionMatches(port, intent.SourceRevision)) ||
				(port.Generation == intent.Generation && migrationPhaseRevisionMatches(port, intent.FinalRevision))
		} else if direction == "target" && port.NodeID == payload.TargetNodeID && port.RequestedChassis == payload.Target && port.Generation == intent.Generation {
			floor := intent.FinalRevision
			if !payload.Online {
				floor = intent.PreparedRevision
			}
			valid = migrationPhaseRevisionMatches(port, floor)
		}
		if !valid {
			return computeError(http.StatusConflict, "migration_intent_drift", "recovered migration port is outside its exact directional state before HA promotion", computePortDetails(port))
		}
	}
	return nil
}

func (s *Server) promoteMigrationRecoveryToHA(ctx context.Context, operation *model.Operation, payload computeMigrationPayload, input computeStartRequest, target *model.Node, ports []*model.Port) (*model.Operation, computeHAPayload, error) {
	claimPhase := "ha-recovering-" + payload.HARecovery.Direction
	if operation.Action != computeMigrationAction || operation.OperationStatus != model.OperationRunning || operation.TargetID != computeVMOperationTarget(payload.VMID) ||
		payload.Phase != claimPhase || !reflect.DeepEqual(payload.HARecovery.Proof, *input.HAProof) {
		return nil, computeHAPayload{}, computeError(http.StatusConflict, "ha_migration_promotion_conflict", "migration recovery ownership changed before HA promotion", nil)
	}
	original := payload
	original.Phase, original.HARecovery = payload.HARecovery.OriginalPhase, nil
	if payload.HARecovery.MigrationHash != migrationPayloadHash(original) {
		return nil, computeHAPayload{}, computeError(http.StatusConflict, "migration_payload_invalid", "migration recovery audit no longer matches the original transaction", nil)
	}
	historyID, err := s.archiveMigrationHAAudit(ctx, operation, original, payload.HARecovery.Direction, input.LifecycleID)
	if err != nil {
		return nil, computeHAPayload{}, err
	}
	haPayload, err := s.newHAPayload(ctx, input, target, ports)
	if err != nil {
		return nil, computeHAPayload{}, err
	}
	if payload.HARecovery.Direction == "source" {
		if err := addMigrationPreparedPredecessors(&haPayload, original); err != nil {
			return nil, computeHAPayload{}, err
		}
	}
	for _, proof := range payload.HARecovery.AuthorityHistory {
		haPayload.AuthorityHistory = append(haPayload.AuthorityHistory, cloneHAProof(proof))
	}
	portIDs := make([]string, 0, len(original.Ports))
	for _, port := range original.Ports {
		portIDs = append(portIDs, port.PortID)
	}
	sort.Strings(portIDs)
	haPayload.MigrationRecovery = &computeHAMigrationAudit{
		OperationID: operation.ID, HistoryOperationID: historyID, LifecycleID: original.LifecycleID,
		Direction: payload.HARecovery.Direction, OriginalPhase: original.Phase, PayloadHash: payload.HARecovery.MigrationHash,
		Online: original.Online, SourceNodeID: original.SourceNodeID, SourceNode: original.SourceNode, SourceChassis: original.Source,
		TargetNodeID: original.TargetNodeID, TargetNode: original.TargetNode, TargetChassis: original.Target, PortIDs: portIDs,
	}
	if err := s.validateHAClusterAuthority(ctx, target, haPayload.Proof, haPriorNodes(haPayload, target.Name)); err != nil {
		return nil, computeHAPayload{}, err
	}
	encoded, err := model.MarshalOperationPayload(haPayload)
	if err != nil {
		return nil, computeHAPayload{}, err
	}
	copyResource, err := model.Clone(operation)
	if err != nil {
		return nil, computeHAPayload{}, err
	}
	desired := copyResource.(*model.Operation)
	desired.Action, desired.Payload, desired.Error = computeHAAction, encoded, ""
	desired.OperationStatus, desired.CompletedAt = model.OperationRunning, nil
	updated, _, err := s.store.Update(ctx, desired, operation.Revision, "compute-migration-ha-promote-"+operation.ID+"-"+input.LifecycleID)
	if err == nil {
		return updated.(*model.Operation), haPayload, nil
	}
	if !errors.Is(err, controlstore.ErrPrecondition) {
		return nil, computeHAPayload{}, err
	}
	currentResource, getErr := s.store.Get(ctx, model.KindOperation, operation.ID)
	if getErr == nil {
		current := currentResource.(*model.Operation)
		currentPayload, decodeErr := decodeHAPayload(current)
		if decodeErr == nil {
			decodeErr = s.validateHAMigrationAudit(ctx, current, currentPayload)
		}
		if decodeErr == nil && current.OperationStatus == model.OperationRunning && current.TargetID == operation.TargetID && current.TargetRevision == operation.TargetRevision &&
			reflect.DeepEqual(currentPayload, haPayload) {
			return current, currentPayload, nil
		}
	}
	return nil, computeHAPayload{}, computeError(http.StatusConflict, "ha_migration_promotion_conflict", "migration recovery changed while ownership was promoted to HA", nil)
}

func addMigrationPreparedPredecessors(payload *computeHAPayload, migration computeMigrationPayload) error {
	byID := make(map[string]computeMigrationPortState, len(migration.Ports))
	for _, intent := range migration.Ports {
		byID[intent.PortID] = intent
	}
	for index := range payload.Ports {
		intent := &payload.Ports[index]
		migrationIntent, ok := byID[intent.PortID]
		if !ok {
			return computeError(http.StatusConflict, "migration_port_set_drift", "promoted HA port is absent from the original migration manifest", map[string]any{"port_id": intent.PortID})
		}
		prepared := computeHAPortPosition{
			NodeID: migration.TargetNodeID, Node: migration.TargetNode, Chassis: migration.Target,
			RevisionFloor: migrationIntent.PreparedRevision, Generation: migrationIntent.Generation,
		}
		if migration.Online {
			prepared.NodeID, prepared.Node, prepared.Chassis = migration.SourceNodeID, migration.SourceNode, migration.Source+","+migration.Target
		}
		intent.Predecessors = normalizeHAPortPositions(append(intent.Predecessors, prepared))
		maxGeneration := intent.PriorGeneration
		allAtTarget := intent.PriorNodeID == payload.TargetNodeID && intent.PriorChassis == payload.Target
		for _, position := range intent.Predecessors {
			if position.Generation > maxGeneration {
				maxGeneration = position.Generation
			}
			if position.NodeID != payload.TargetNodeID || position.Chassis != payload.Target {
				allAtTarget = false
			}
		}
		if !allAtTarget {
			if maxGeneration == math.MaxInt64 {
				return computeError(http.StatusConflict, "generation_exhausted", "PVN port has no usable HA migration takeover fence", map[string]any{"port_id": intent.PortID})
			}
			intent.TargetGeneration = maxGeneration + 1
		}
	}
	return nil
}

func (s *Server) archiveMigrationHAAudit(ctx context.Context, active *model.Operation, original computeMigrationPayload, direction, haLifecycleID string) (string, error) {
	historyID := computeMigrationHAHistoryOperationID(active.ID, direction, haLifecycleID)
	if resource, err := s.store.Get(ctx, model.KindOperation, historyID); err == nil {
		existing := resource.(*model.Operation)
		existingPayload, decodeErr := decodeMigrationPayload(existing)
		if decodeErr == nil && existing.OperationStatus == model.OperationFailed && existing.TargetID == computeHistoryOperationTarget(computeVMOperationTarget(original.VMID), historyID) && reflect.DeepEqual(existingPayload, original) {
			return historyID, nil
		}
		return "", computeError(http.StatusConflict, "ha_migration_audit_conflict", "durable migration takeover audit differs from the original transaction", nil)
	} else if !errors.Is(err, controlstore.ErrNotFound) {
		return "", err
	}
	encoded, err := model.MarshalOperationPayload(original)
	if err != nil {
		return "", err
	}
	now := s.clusterGate.now().UTC()
	started := active.StartedAt
	if started == nil {
		started = &now
	}
	history := &model.Operation{
		Metadata: model.Metadata{ID: historyID}, Action: computeMigrationAction, TargetKind: model.KindNode,
		TargetID: computeHistoryOperationTarget(computeVMOperationTarget(original.VMID), historyID), TargetRevision: 1,
		OperationStatus: model.OperationFailed, IdempotencyKey: "compute-migration-ha-history:" + historyID,
		Error: "migration ownership was promoted to PVE HA lifecycle " + haLifecycleID, LeaseOwner: "compute-lifecycle",
		StartedAt: started, CompletedAt: &now, Payload: encoded,
	}
	if _, _, err := s.store.Create(ctx, history, history.IdempotencyKey); err != nil {
		return "", err
	}
	return historyID, nil
}

func computeMigrationHAHistoryOperationID(activeID, direction, lifecycleID string) string {
	digest := sha256.Sum256([]byte("pvn-compute-migration-ha-history:" + activeID + ":" + direction + ":" + lifecycleID))
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(digest[:16])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func migrationHARecoveryDirection(payload computeMigrationPayload) (string, bool) {
	switch payload.Phase {
	case "preparing", "compensating", "aborting", "ha-recovering-source":
		return "source", true
	case "prepared", "committing", "ha-recovering-target":
		return "target", true
	default:
		return "", false
	}
}

func (s *Server) claimHAMigrationRecovery(ctx context.Context, id, direction string, proof computeHAProof) (*model.Operation, computeMigrationPayload, error) {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		operation, payload, err := s.loadMigrationOperation(ctx, id)
		if err != nil {
			return nil, payload, err
		}
		if operation.OperationStatus != model.OperationRunning || operation.TargetID != computeVMOperationTarget(payload.VMID) {
			return nil, payload, computeError(http.StatusConflict, "ha_migration_transaction_terminal", "abandoned migration no longer owns the VM lifecycle slot", nil)
		}
		currentDirection, ok := migrationHARecoveryDirection(payload)
		if !ok || currentDirection != direction {
			return nil, payload, computeError(http.StatusConflict, "ha_migration_phase_unsafe", "migration is claimed by another recovery action", map[string]any{"phase": payload.Phase})
		}
		if payload.HARecovery != nil {
			if payload.HARecovery.Direction != direction {
				return nil, payload, computeError(http.StatusConflict, "ha_migration_recovery_conflict", "durable migration recovery direction changed", nil)
			}
			if payload.HARecovery.Proof.ServiceUID == proof.ServiceUID &&
				(payload.HARecovery.Proof.ServiceID != proof.ServiceID || payload.HARecovery.Proof.ServiceNode != proof.ServiceNode || payload.HARecovery.Proof.LRMNode != proof.LRMNode) {
				return nil, payload, computeError(http.StatusConflict, "ha_lifecycle_conflict", "PVE HA service UID changed VM or target during migration recovery", nil)
			}
			if payload.HARecovery.Proof.ServiceUID == proof.ServiceUID &&
				(proof.ManagerEpoch < payload.HARecovery.Proof.ManagerEpoch || proof.LRMEpoch < payload.HARecovery.Proof.LRMEpoch || proof.AgentLockEpoch < payload.HARecovery.Proof.AgentLockEpoch) {
				return nil, payload, computeError(http.StatusConflict, "ha_proof_regressed", "PVE HA authority epochs regressed during migration recovery", nil)
			}
			if payload.HARecovery.Proof.ServiceUID == proof.ServiceUID && proof.ManagerEpoch == payload.HARecovery.Proof.ManagerEpoch &&
				proof.LRMEpoch == payload.HARecovery.Proof.LRMEpoch && proof.AgentLockEpoch == payload.HARecovery.Proof.AgentLockEpoch &&
				!reflect.DeepEqual(proof, payload.HARecovery.Proof) {
				return nil, payload, computeError(http.StatusConflict, "ha_proof_conflict", "PVE HA proof changed without a newer authority epoch", nil)
			}
			if payload.HARecovery.Proof.ServiceUID != proof.ServiceUID && proof.ManagerEpoch <= payload.HARecovery.Proof.ManagerEpoch {
				return nil, payload, computeError(http.StatusConflict, "ha_assignment_not_newer", "new PVE HA service generation is not newer than the recovery owner", nil)
			}
			if payload.HARecovery.Proof.ServiceUID != proof.ServiceUID {
				if len(payload.HARecovery.AuthorityHistory) >= 32 {
					return nil, payload, computeError(http.StatusConflict, "ha_history_exhausted", "too many interrupted HA migration recovery assignments require operator recovery", map[string]any{"vmid": payload.VMID})
				}
				payload.HARecovery.AuthorityHistory = append(payload.HARecovery.AuthorityHistory, cloneHAProof(payload.HARecovery.Proof))
			}
		} else if strings.HasPrefix(payload.Phase, "ha-recovering-") {
			return nil, payload, computeError(http.StatusConflict, "migration_payload_invalid", "HA migration recovery phase has no durable authority payload", nil)
		}
		if payload.HARecovery == nil {
			payload.HARecovery = &computeMigrationHARecovery{Direction: direction, OriginalPhase: payload.Phase, MigrationHash: migrationPayloadHash(payload), Proof: cloneHAProof(proof)}
		} else {
			payload.HARecovery.Proof = cloneHAProof(proof)
		}
		payload.Phase = "ha-recovering-" + direction
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return nil, payload, err
		}
		copyResource, err := model.Clone(operation)
		if err != nil {
			return nil, payload, err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload, desired.Error = encoded, ""
		updated, _, err := s.store.Update(ctx, desired, operation.Revision, "compute-migration-ha-claim-"+id+"-"+fmt.Sprint(operation.Revision))
		if err == nil {
			return updated.(*model.Operation), payload, nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return nil, payload, err
		}
		return nil, payload, computeError(http.StatusConflict, "ha_migration_claim_conflict", "HA migration recovery changed concurrently; retry against the durable owner", nil)
	}
	return nil, computeMigrationPayload{}, computeError(http.StatusConflict, "ha_migration_claim_conflict", "HA migration recovery changed concurrently", nil)
}

func (s *Server) enforceHAAuthorityHistory(ctx context.Context, input computeStartRequest, target *model.Node) error {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	baselines := make([]computeHAProof, 0)
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		var vmid int
		switch operation.Action {
		case computeHAAction:
			payload, decodeErr := decodeHAPayload(operation)
			if decodeErr != nil {
				return decodeErr
			}
			if err := s.validateHAMigrationAudit(ctx, operation, payload); err != nil {
				return err
			}
			vmid = payload.VMID
			if vmid == input.VMID {
				for _, proof := range payload.AuthorityHistory {
					baselines = append(baselines, cloneHAProof(proof))
				}
				baselines = append(baselines, cloneHAProof(payload.Proof))
			}
		case computeMigrationAction:
			payload, decodeErr := decodeMigrationPayload(operation)
			if decodeErr != nil {
				return decodeErr
			}
			vmid = payload.VMID
			if vmid == input.VMID && payload.HARecovery != nil {
				for _, proof := range payload.HARecovery.AuthorityHistory {
					baselines = append(baselines, cloneHAProof(proof))
				}
				baselines = append(baselines, cloneHAProof(payload.HARecovery.Proof))
			}
		default:
			continue
		}
		_ = vmid
	}
	maxSameUID := int64(-1)
	for _, baseline := range baselines {
		if baseline.ServiceUID == input.HAProof.ServiceUID && baseline.ManagerEpoch > maxSameUID {
			maxSameUID = baseline.ManagerEpoch
		}
	}
	if maxSameUID >= 0 {
		for _, baseline := range baselines {
			if baseline.ServiceUID != input.HAProof.ServiceUID && baseline.ManagerEpoch > maxSameUID {
				return computeError(http.StatusConflict, "ha_service_uid_reused", "a superseded PVE HA service UID cannot be resurrected after another assignment", map[string]any{"service_uid": input.HAProof.ServiceUID})
			}
		}
	}
	for _, baseline := range baselines {
		if err := validateHAAuthoritySuccessor(input, target, *input.HAProof, baseline); err != nil {
			return err
		}
	}
	return nil
}

func validateHAAuthoritySuccessor(input computeStartRequest, target *model.Node, proof, baseline computeHAProof) error {
	if proof.ServiceUID == baseline.ServiceUID {
		if proof.ServiceID != baseline.ServiceID || baseline.ServiceNode != target.Name || baseline.LRMNode != target.Name {
			return computeError(http.StatusConflict, "ha_lifecycle_conflict", "PVE HA service UID is pinned to a different VM or target assignment", nil)
		}
		if proof.ManagerEpoch < baseline.ManagerEpoch || proof.LRMEpoch < baseline.LRMEpoch || proof.AgentLockEpoch < baseline.AgentLockEpoch {
			return computeError(http.StatusConflict, "ha_proof_regressed", "PVE HA authority epochs regressed against durable history", nil)
		}
		if proof.ManagerEpoch == baseline.ManagerEpoch && proof.LRMEpoch == baseline.LRMEpoch && proof.AgentLockEpoch == baseline.AgentLockEpoch && !reflect.DeepEqual(proof, baseline) {
			return computeError(http.StatusConflict, "ha_proof_conflict", "PVE HA proof changed without a newer authority epoch", nil)
		}
		if input.LifecycleID != computeHALifecycleID(input.VMID, target.Name, proof.ServiceUID) {
			return computeError(http.StatusConflict, "ha_lifecycle_mismatch", "HA lifecycle_id differs from its durable authority baseline", nil)
		}
		return nil
	}
	if proof.ManagerEpoch <= baseline.ManagerEpoch {
		return computeError(http.StatusConflict, "ha_assignment_not_newer", "new PVE HA service generation is not newer than durable assignment history", map[string]any{"manager_epoch": proof.ManagerEpoch, "required_after": baseline.ManagerEpoch})
	}
	return nil
}

func validateHAProofIdentity(input computeStartRequest, target *model.Node, proof computeHAProof, now time.Time) error {
	if input.LifecycleID != computeHALifecycleID(input.VMID, target.Name, proof.ServiceUID) {
		return computeError(http.StatusConflict, "ha_lifecycle_mismatch", "HA lifecycle_id does not match the canonical PVE service generation", nil)
	}
	if proof.Origin != "ha" || proof.ServiceID != "vm:"+fmt.Sprint(input.VMID) || proof.ServiceNode != target.Name || proof.ServiceState != "started" ||
		proof.LRMNode != target.Name || proof.LRMState != "active" || proof.LRMMode != "active" || !validHAServiceUID(proof.ServiceUID) || len(proof.NodeStates) == 0 {
		return computeError(http.StatusConflict, "ha_proof_invalid", "PVE HA proof does not authorize this VM and target node", nil)
	}
	if proof.NodeStates[target.Name] != "online" {
		return computeError(http.StatusConflict, "ha_target_not_authorized", "PVE HA proof does not mark the assigned target online", map[string]any{"target": target.Name})
	}
	for name, state := range proof.NodeStates {
		probe := &model.Node{Name: name, ChassisID: "proof", Roles: []model.NodeRole{model.NodeRoleCompute}, Enabled: true}
		if probe.Validate() != nil || (state != "online" && state != "maintenance" && state != "unknown" && state != "fence" && state != "gone") {
			return computeError(http.StatusConflict, "ha_proof_invalid", "PVE HA proof contains an invalid node state", map[string]any{"node": name, "state": state})
		}
	}
	for field, epoch := range map[string]int64{"manager_epoch": proof.ManagerEpoch, "lrm_epoch": proof.LRMEpoch, "agent_lock_epoch": proof.AgentLockEpoch} {
		observed := time.Unix(epoch, 0).UTC()
		if epoch < 1 || observed.After(now.Add(computeHAProofFutureSkew)) || now.Sub(observed) > computeHAProofFreshness {
			return computeError(http.StatusConflict, "ha_proof_stale", "PVE HA authority proof is stale or from the future", map[string]any{"field": field, "epoch": epoch})
		}
	}
	return nil
}

func validHAServiceUID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("+/_-", character) {
			continue
		}
		return false
	}
	return true
}

func computeHALifecycleID(vmid int, target, serviceUID string) string {
	digest := sha256.Sum256([]byte(fmt.Sprint(vmid) + "\x00" + target + "\x00" + serviceUID))
	return "pve-ha-" + hex.EncodeToString(digest[:])
}

func validateHAReplayIdentity(input computeStartRequest, target *model.Node, payload computeHAPayload, proof computeHAProof) error {
	if payload.Version != computePayloadVersion || payload.LifecycleID != input.LifecycleID || payload.VMID != input.VMID || payload.TargetNodeID != target.ID ||
		payload.TargetNode != target.Name || payload.Target != target.ChassisID || payload.Proof.ServiceUID != proof.ServiceUID || payload.Proof.ServiceID != proof.ServiceID {
		return computeError(http.StatusConflict, "ha_lifecycle_conflict", "HA lifecycle_id was reused for a different VM, target, or service generation", nil)
	}
	if proof.ManagerEpoch < payload.Proof.ManagerEpoch || proof.LRMEpoch < payload.Proof.LRMEpoch || proof.AgentLockEpoch < payload.Proof.AgentLockEpoch {
		return computeError(http.StatusConflict, "ha_proof_regressed", "PVE HA authority epochs regressed during replay", nil)
	}
	if proof.ManagerEpoch == payload.Proof.ManagerEpoch && proof.LRMEpoch == payload.Proof.LRMEpoch && proof.AgentLockEpoch == payload.Proof.AgentLockEpoch && !reflect.DeepEqual(proof, payload.Proof) {
		return computeError(http.StatusConflict, "ha_proof_conflict", "PVE HA proof changed without a newer authority epoch", nil)
	}
	return nil
}

func (s *Server) validateHAClusterAuthority(ctx context.Context, target *model.Node, proof computeHAProof, priorNodes []computeHAPriorNode) error {
	gate := s.clusterGate
	if gate == nil || !gate.required {
		return computeError(http.StatusConflict, "ha_fence_unavailable", "HA start requires fresh quorate cluster membership and PVE CRM proof", nil)
	}
	gate.mu.RLock()
	reported, quorate := gate.reported, gate.quorate
	online := append([]string(nil), gate.online...)
	gate.mu.RUnlock()
	now := gate.now().UTC()
	if reported.IsZero() || reported.After(now.Add(computeClockSkew)) || now.Sub(reported) > gate.ttl || !quorate {
		return computeError(http.StatusServiceUnavailable, "membership_stale", "fresh quorate PVE membership is required for HA start", nil)
	}
	if !slices.Contains(online, target.Name) {
		return computeError(http.StatusConflict, "ha_target_offline", "HA target is absent from fresh PVE membership", map[string]any{"target": target.Name})
	}
	seen := make(map[string]bool, len(priorNodes))
	for _, prior := range priorNodes {
		if prior.Name == target.Name || seen[prior.ID] {
			continue
		}
		seen[prior.ID] = true
		state := proof.NodeStates[prior.Name]
		if state != "unknown" && state != "gone" {
			return computeError(http.StatusConflict, "ha_source_not_fenced", "PVE CRM proof does not fence a prior VM node", map[string]any{"source": prior.Name, "state": state})
		}
		if slices.Contains(online, prior.Name) {
			return computeError(http.StatusConflict, "ha_source_online", "a prior HA VM node remains online in fresh membership", map[string]any{"source": prior.Name})
		}
		resource, err := s.store.Get(ctx, model.KindNode, prior.ID)
		if err != nil {
			return err
		}
		node := resource.(*model.Node)
		if node.Name != prior.Name || node.ChassisID != prior.Chassis || node.LastSeenAt == nil || node.LastSeenAt.UTC().After(now.Add(computeClockSkew)) || now.Sub(node.LastSeenAt.UTC()) <= gate.ttl+haStabilizationDelay {
			return computeError(http.StatusConflict, "ha_source_not_stale", "prior HA node identity or PVN heartbeat is not safely fenced", map[string]any{"source": prior.Name, "last_seen_at": node.LastSeenAt})
		}
	}
	return nil
}

type computeHAPriorNode struct {
	ID      string
	Name    string
	Chassis string
}

func haPriorNodes(payload computeHAPayload, targetName string) []computeHAPriorNode {
	result := make([]computeHAPriorNode, 0)
	seen := make(map[string]bool)
	for _, port := range payload.Ports {
		for _, predecessor := range haPortPredecessors(port) {
			if predecessor.Node != targetName && !seen[predecessor.NodeID] {
				seen[predecessor.NodeID] = true
				result = append(result, computeHAPriorNode{ID: predecessor.NodeID, Name: predecessor.Node, Chassis: predecessor.Chassis})
			}
		}
	}
	return result
}

func haPortPredecessors(intent computeHAPortState) []computeHAPortPosition {
	positions := make([]computeHAPortPosition, 0, len(intent.Predecessors)+1)
	positions = append(positions, computeHAPortPosition{
		NodeID: intent.PriorNodeID, Node: intent.PriorNode, Chassis: intent.PriorChassis,
		RevisionFloor: intent.PriorRevision, Generation: intent.PriorGeneration,
	})
	for _, candidate := range intent.Predecessors {
		duplicate := false
		for _, existing := range positions {
			if candidate.NodeID == existing.NodeID && candidate.Chassis == existing.Chassis && candidate.Generation == existing.Generation && candidate.RevisionFloor == existing.RevisionFloor {
				duplicate = true
				break
			}
		}
		if !duplicate {
			positions = append(positions, candidate)
		}
	}
	return positions
}

func haPortMatchesPosition(port *model.Port, position computeHAPortPosition) bool {
	return port.NodeID == position.NodeID && port.RequestedChassis == position.Chassis && port.Generation == position.Generation && migrationPhaseRevisionMatches(port, position.RevisionFloor)
}

func (s *Server) newHAPayload(ctx context.Context, input computeStartRequest, target *model.Node, ports []*model.Port) (computeHAPayload, error) {
	payload := computeHAPayload{Version: computePayloadVersion, LifecycleID: input.LifecycleID, Phase: "rebinding", VMID: input.VMID,
		TargetNodeID: target.ID, TargetNode: target.Name, Target: target.ChassisID, Proof: cloneHAProof(*input.HAProof)}
	for _, port := range ports {
		requested, err := model.ParseRequestedChassis(port.RequestedChassis)
		if err != nil || len(requested) != 1 || port.NodeID == "" || !migrationBindingReady(port.BindingStatus) || !port.AdminStateUp || port.Generation < 1 || port.Revision < 1 {
			return payload, computeError(http.StatusConflict, "ha_port_state_invalid", "HA start requires every PVN port in one exact attached chassis state", computePortDetails(port))
		}
		prior, err := s.resolveAttachmentNode(ctx, port.NodeID)
		if err != nil || prior.ChassisID != requested[0] {
			return payload, computeError(http.StatusConflict, "ha_source_mismatch", "HA port node and requested chassis identities disagree", computePortDetails(port))
		}
		groups := append([]string(nil), port.SecurityGroupIDs...)
		sort.Strings(groups)
		targetRevision, targetGeneration := port.Revision, port.Generation
		if prior.ID != target.ID {
			if port.Generation == math.MaxInt64 || port.Revision == math.MaxInt64 {
				return payload, computeError(http.StatusConflict, "generation_exhausted", "PVN port has no usable HA fencing generation", computePortDetails(port))
			}
			targetRevision, targetGeneration = port.Revision+1, port.Generation+1
		}
		payload.Ports = append(payload.Ports, computeHAPortState{PortID: port.ID, NIC: port.NIC, Name: port.Name, MACAddress: strings.ToLower(port.MACAddress), NetworkID: port.NetworkID,
			FixedIPs: append([]model.FixedIP(nil), port.FixedIPs...), SecurityGroupIDs: groups, LSPName: port.LSPName, AdminStateUp: port.AdminStateUp,
			PriorNodeID: prior.ID, PriorNode: prior.Name, PriorChassis: prior.ChassisID, PriorStatus: port.BindingStatus, PriorRevision: port.Revision, PriorGeneration: port.Generation,
			TargetRevision: targetRevision, TargetGeneration: targetGeneration})
	}
	sort.Slice(payload.Ports, func(i, j int) bool { return payload.Ports[i].NIC < payload.Ports[j].NIC })
	return payload, nil
}

func (s *Server) newHASupersessionPayload(ctx context.Context, input computeStartRequest, target *model.Node, ports []*model.Port, previous computeHAPayload) (computeHAPayload, error) {
	replacement, err := s.newHAPayload(ctx, input, target, ports)
	if err != nil {
		return replacement, err
	}
	if len(previous.AuthorityHistory) >= 32 {
		return replacement, computeError(http.StatusConflict, "ha_history_exhausted", "too many interrupted HA assignments require operator recovery", map[string]any{"vmid": input.VMID})
	}
	replacement.AuthorityHistory = make([]computeHAProof, 0, len(previous.AuthorityHistory)+1)
	for _, proof := range previous.AuthorityHistory {
		replacement.AuthorityHistory = append(replacement.AuthorityHistory, cloneHAProof(proof))
	}
	replacement.AuthorityHistory = append(replacement.AuthorityHistory, cloneHAProof(previous.Proof))
	if previous.MigrationRecovery != nil {
		audit := *previous.MigrationRecovery
		audit.PortIDs = append([]string(nil), previous.MigrationRecovery.PortIDs...)
		replacement.MigrationRecovery = &audit
	}
	oldByID := make(map[string]computeHAPortState, len(previous.Ports))
	for _, intent := range previous.Ports {
		oldByID[intent.PortID] = intent
	}
	for index := range replacement.Ports {
		intent := &replacement.Ports[index]
		old, ok := oldByID[intent.PortID]
		if !ok {
			return replacement, computeError(http.StatusConflict, "ha_port_set_drift", "HA supersession lost an existing port manifest", map[string]any{"port_id": intent.PortID})
		}
		positions := haPortPredecessors(old)
		positions = append(positions, computeHAPortPosition{
			NodeID: previous.TargetNodeID, Node: previous.TargetNode, Chassis: previous.Target,
			RevisionFloor: old.TargetRevision, Generation: old.TargetGeneration,
		})
		positions = append(positions, computeHAPortPosition{
			NodeID: intent.PriorNodeID, Node: intent.PriorNode, Chassis: intent.PriorChassis,
			RevisionFloor: intent.PriorRevision, Generation: intent.PriorGeneration,
		})
		positions = normalizeHAPortPositions(positions)
		intent.Predecessors = positions
		allAtTarget := true
		maxGeneration := int64(0)
		for _, position := range positions {
			if position.Generation > maxGeneration {
				maxGeneration = position.Generation
			}
			if position.NodeID != target.ID || position.Chassis != target.ChassisID {
				allAtTarget = false
			}
		}
		if allAtTarget {
			intent.TargetGeneration = maxGeneration
			continue
		}
		if maxGeneration == math.MaxInt64 {
			return replacement, computeError(http.StatusConflict, "generation_exhausted", "PVN port has no usable HA supersession fence", map[string]any{"port_id": intent.PortID})
		}
		intent.TargetGeneration = maxGeneration + 1
	}
	return replacement, nil
}

func normalizeHAPortPositions(positions []computeHAPortPosition) []computeHAPortPosition {
	result := make([]computeHAPortPosition, 0, len(positions))
	for _, candidate := range positions {
		matched := false
		for index := range result {
			existing := &result[index]
			if candidate.NodeID == existing.NodeID && candidate.Node == existing.Node && candidate.Chassis == existing.Chassis && candidate.Generation == existing.Generation {
				if candidate.RevisionFloor < existing.RevisionFloor {
					existing.RevisionFloor = candidate.RevisionFloor
				}
				matched = true
				break
			}
		}
		if !matched {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NodeID != result[j].NodeID {
			return result[i].NodeID < result[j].NodeID
		}
		if result[i].Generation != result[j].Generation {
			return result[i].Generation < result[j].Generation
		}
		return result[i].RevisionFloor < result[j].RevisionFloor
	})
	return result
}

func cloneHAProof(proof computeHAProof) computeHAProof {
	proof.NodeStates = maps.Clone(proof.NodeStates)
	return proof
}

func (s *Server) applyHAPorts(ctx context.Context, operationID string, payload computeHAPayload) ([]*model.Port, error) {
	if err := s.assertHAOperationOwner(ctx, operationID, payload); err != nil {
		return nil, err
	}
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: payload.VMID})
	if err != nil {
		return nil, err
	}
	if len(resources) != len(payload.Ports) {
		return nil, computeError(http.StatusConflict, "ha_port_set_drift", "VM PVN port set changed during HA relocation", nil)
	}
	byID := make(map[string]*model.Port, len(resources))
	for _, resource := range resources {
		byID[resource.GetMetadata().ID] = resource.(*model.Port)
	}
	result := make([]*model.Port, 0, len(payload.Ports))
	for _, intent := range payload.Ports {
		if err := s.assertHAOperationOwner(ctx, operationID, payload); err != nil {
			return nil, err
		}
		current := byID[intent.PortID]
		if current == nil || !haPortConfigMatches(current, payload.VMID, intent) {
			return nil, computeError(http.StatusConflict, "ha_port_drift", "PVN port changed outside its durable HA transaction", map[string]any{"port_id": intent.PortID})
		}
		if current.NodeID == payload.TargetNodeID && current.RequestedChassis == payload.Target && migrationPhaseRevisionMatches(current, intent.TargetRevision) && current.Generation == intent.TargetGeneration {
			realized, err := s.forceRealizeComputePort(ctx, current)
			if err != nil {
				return nil, err
			}
			if err := validateStartPort(realized, &model.Node{Metadata: model.Metadata{ID: payload.TargetNodeID}, Name: payload.TargetNode, ChassisID: payload.Target}); err != nil {
				return nil, err
			}
			result = append(result, realized)
			continue
		}
		prior := false
		for _, predecessor := range haPortPredecessors(intent) {
			if haPortMatchesPosition(current, predecessor) {
				prior = true
				break
			}
		}
		if !prior {
			return nil, computeError(http.StatusConflict, "ha_port_drift", "PVN port is outside the exact prior/target HA states", computePortDetails(current))
		}
		desired := clonePort(current)
		desired.Metadata = model.Metadata{ID: current.ID}
		desired.NodeID, desired.RequestedChassis = payload.TargetNodeID, payload.Target
		desired.BindingStatus, desired.Generation = model.PortBinding, intent.TargetGeneration
		updated, _, err := s.store.Update(ctx, desired, current.Revision, "compute-ha-rebind-"+payload.LifecycleID+"-"+current.ID)
		if err != nil {
			return nil, err
		}
		current = updated.(*model.Port)
		if current.Revision < intent.TargetRevision || !haPortConfigMatches(current, payload.VMID, intent) {
			return nil, computeError(http.StatusConflict, "ha_port_drift", "HA rebind produced an unexpected durable port state", computePortDetails(current))
		}
		realized, err := s.forceRealizeComputePort(ctx, current)
		if err != nil {
			return nil, err
		}
		if err := validateStartPort(realized, &model.Node{Metadata: model.Metadata{ID: payload.TargetNodeID}, Name: payload.TargetNode, ChassisID: payload.Target}); err != nil {
			return nil, err
		}
		result = append(result, realized)
	}
	return result, nil
}

func haPortConfigMatches(port *model.Port, vmid int, intent computeHAPortState) bool {
	groups := append([]string(nil), port.SecurityGroupIDs...)
	sort.Strings(groups)
	return port.ID == intent.PortID && port.VMID == vmid && port.NIC == intent.NIC && port.Name == intent.Name && strings.EqualFold(port.MACAddress, intent.MACAddress) &&
		port.NetworkID == intent.NetworkID && reflect.DeepEqual(port.FixedIPs, intent.FixedIPs) && slices.Equal(groups, intent.SecurityGroupIDs) &&
		port.LSPName == intent.LSPName && port.AdminStateUp == intent.AdminStateUp
}

func (s *Server) findHAOperationForLifecycle(ctx context.Context, input computeStartRequest, target *model.Node, canonicalID string) (*model.Operation, computeHAPayload, error) {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return nil, computeHAPayload{}, err
	}
	type match struct {
		operation *model.Operation
		payload   computeHAPayload
	}
	matches := make([]match, 0, 2)
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != computeHAAction {
			continue
		}
		candidate, decodeErr := decodeHAPayload(operation)
		if decodeErr != nil {
			return nil, computeHAPayload{}, decodeErr
		}
		if err := s.validateHAMigrationAudit(ctx, operation, candidate); err != nil {
			return nil, computeHAPayload{}, err
		}
		if candidate.VMID != input.VMID || candidate.LifecycleID != input.LifecycleID || candidate.TargetNodeID != target.ID || candidate.TargetNode != target.Name || candidate.Target != target.ChassisID ||
			candidate.Proof.ServiceUID != input.HAProof.ServiceUID || candidate.Proof.ServiceID != input.HAProof.ServiceID {
			continue
		}
		active := operation.OperationStatus == model.OperationRunning && operation.TargetID == computeVMOperationTarget(input.VMID) && candidate.Phase == "rebinding"
		terminal := operation.OperationStatus == model.OperationSucceeded && candidate.Phase == "ready"
		if active || terminal {
			matches = append(matches, match{operation: operation, payload: candidate})
		}
	}
	if len(matches) == 0 {
		return nil, computeHAPayload{}, nil
	}
	var active *match
	for index := range matches {
		candidate := &matches[index]
		if candidate.operation.OperationStatus == model.OperationRunning {
			if active != nil {
				return nil, computeHAPayload{}, computeError(http.StatusConflict, "ha_transaction_ambiguous", "multiple active HA transactions match the same service assignment", nil)
			}
			active = candidate
		}
	}
	if active != nil {
		return active.operation, active.payload, nil
	}
	best := matches[0]
	for _, candidate := range matches[1:] {
		switch compareHAProofEpochs(candidate.payload.Proof, best.payload.Proof) {
		case 1:
			best = candidate
		case 0:
			candidatePromoted := candidate.payload.MigrationRecovery != nil
			bestPromoted := best.payload.MigrationRecovery != nil
			if candidatePromoted != bestPromoted {
				if candidatePromoted {
					best = candidate
				}
				continue
			}
			if candidate.operation.ID == canonicalID && best.operation.ID != canonicalID {
				best = candidate
				continue
			}
			return nil, computeHAPayload{}, computeError(http.StatusConflict, "ha_transaction_ambiguous", "multiple terminal HA transactions match the same authority generation", nil)
		case -2:
			return nil, computeHAPayload{}, computeError(http.StatusConflict, "ha_transaction_ambiguous", "HA authority epochs are incomparable across matching transactions", nil)
		}
	}
	return best.operation, best.payload, nil
}

// compareHAProofEpochs returns 1 when left strictly dominates right, -1 when
// right dominates left, 0 when all epochs are equal, and -2 when incomparable.
func compareHAProofEpochs(left, right computeHAProof) int {
	leftGE := left.ManagerEpoch >= right.ManagerEpoch && left.LRMEpoch >= right.LRMEpoch && left.AgentLockEpoch >= right.AgentLockEpoch
	rightGE := right.ManagerEpoch >= left.ManagerEpoch && right.LRMEpoch >= left.LRMEpoch && right.AgentLockEpoch >= left.AgentLockEpoch
	switch {
	case leftGE && rightGE:
		return 0
	case leftGE:
		return 1
	case rightGE:
		return -1
	default:
		return -2
	}
}

func (s *Server) claimHAStartOperation(ctx context.Context, id string, input computeStartRequest, target *model.Node, proof computeHAProof) (*model.Operation, computeHAPayload, error) {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		operation, payload, err := s.loadHAOperation(ctx, id)
		if err != nil {
			return nil, payload, err
		}
		if err := validateHAReplayIdentity(input, target, payload, proof); err != nil {
			return nil, payload, err
		}
		if err := s.enforceHAAuthorityHistory(ctx, input, target); err != nil {
			return nil, payload, err
		}
		if proof.ManagerEpoch < payload.Proof.ManagerEpoch || proof.LRMEpoch < payload.Proof.LRMEpoch || proof.AgentLockEpoch < payload.Proof.AgentLockEpoch {
			return nil, payload, computeError(http.StatusConflict, "ha_proof_regressed", "PVE HA authority epochs regressed during retry", nil)
		}
		if operation.OperationStatus == model.OperationFailed && payload.Phase == "superseded" {
			return nil, payload, computeError(http.StatusConflict, "ha_transaction_superseded", "a newer PVE HA service assignment superseded this relocation", nil)
		}
		if operation.OperationStatus != model.OperationRunning && !(operation.OperationStatus == model.OperationSucceeded && payload.Phase == "ready") {
			return nil, payload, computeError(http.StatusConflict, "ha_transaction_terminal", "HA transaction is terminal in a different phase", map[string]any{"phase": payload.Phase})
		}
		if operation.OperationStatus == model.OperationRunning && payload.Phase != "rebinding" {
			return nil, payload, computeError(http.StatusConflict, "ha_transaction_claimed", "HA transaction is claimed in an unexpected phase", map[string]any{"phase": payload.Phase})
		}
		unchanged := reflect.DeepEqual(payload.Proof, proof) && operation.OperationStatus == model.OperationRunning && operation.TargetID == computeVMOperationTarget(input.VMID)
		if unchanged {
			return operation, payload, nil
		}
		payload.Proof, payload.Phase = cloneHAProof(proof), "rebinding"
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return nil, payload, err
		}
		copyResource, err := model.Clone(operation)
		if err != nil {
			return nil, payload, err
		}
		desired := copyResource.(*model.Operation)
		desired.Payload, desired.OperationStatus = encoded, model.OperationRunning
		desired.TargetID, desired.TargetRevision = computeVMOperationTarget(input.VMID), 1
		desired.Error, desired.CompletedAt = "", nil
		updated, _, err := s.store.Update(ctx, desired, operation.Revision, "compute-ha-claim-"+id+"-"+fmt.Sprint(operation.Revision))
		if err == nil {
			return updated.(*model.Operation), payload, nil
		}
		if errors.Is(err, controlstore.ErrAlreadyExists) {
			return nil, payload, computeError(http.StatusConflict, "compute_vm_busy", "VM already has an active compute lifecycle transaction", map[string]any{"vmid": input.VMID})
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return nil, payload, err
		}
	}
	return nil, computeHAPayload{}, computeError(http.StatusConflict, "ha_claim_conflict", "HA transaction changed concurrently", nil)
}

func (s *Server) supersedeHAOperation(ctx context.Context, input computeStartRequest, target *model.Node) (*model.Operation, computeHAPayload, bool, error) {
	resources, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{})
	if err != nil {
		return nil, computeHAPayload{}, false, err
	}
	var active *model.Operation
	var payload computeHAPayload
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != computeHAAction || operation.OperationStatus != model.OperationRunning {
			continue
		}
		candidate, decodeErr := decodeHAPayload(operation)
		if decodeErr != nil {
			return nil, computeHAPayload{}, false, decodeErr
		}
		if err := s.validateHAMigrationAudit(ctx, operation, candidate); err != nil {
			return nil, computeHAPayload{}, false, err
		}
		if candidate.VMID != input.VMID {
			continue
		}
		if active != nil {
			return nil, computeHAPayload{}, false, computeError(http.StatusConflict, "ha_transaction_ambiguous", "multiple active HA transactions exist for the VM", nil)
		}
		active, payload = operation, candidate
	}
	if active == nil {
		return nil, computeHAPayload{}, false, nil
	}
	proof := *input.HAProof
	if proof.ManagerEpoch <= payload.Proof.ManagerEpoch || proof.ServiceUID == payload.Proof.ServiceUID {
		return nil, computeHAPayload{}, false, computeError(http.StatusConflict, "ha_transaction_active", "an existing HA relocation owns the VM lifecycle slot", map[string]any{"operation_id": active.ID, "target": payload.TargetNode})
	}
	prior := haPriorNodes(payload, target.Name)
	if payload.TargetNode != target.Name {
		prior = append(prior, computeHAPriorNode{ID: payload.TargetNodeID, Name: payload.TargetNode, Chassis: payload.Target})
	}
	if err := s.validateHAClusterAuthority(ctx, target, proof, prior); err != nil {
		return nil, computeHAPayload{}, false, err
	}
	currentPorts, err := s.loadHAOperationPortStates(ctx, payload)
	if err != nil {
		return nil, computeHAPayload{}, false, err
	}
	replacement, err := s.newHASupersessionPayload(ctx, input, target, currentPorts, payload)
	if err != nil {
		return nil, computeHAPayload{}, false, err
	}
	if err := s.validateHAClusterAuthority(ctx, target, replacement.Proof, haPriorNodes(replacement, target.Name)); err != nil {
		return nil, computeHAPayload{}, false, err
	}
	encoded, err := model.MarshalOperationPayload(replacement)
	if err != nil {
		return nil, computeHAPayload{}, false, err
	}
	copyResource, err := model.Clone(active)
	if err != nil {
		return nil, computeHAPayload{}, false, err
	}
	desired := copyResource.(*model.Operation)
	desired.Payload, desired.Error = encoded, ""
	desired.OperationStatus, desired.CompletedAt = model.OperationRunning, nil
	desired.TargetID, desired.TargetRevision = computeVMOperationTarget(input.VMID), 1
	updated, _, err := s.store.Update(ctx, desired, active.Revision, "compute-ha-supersede-"+active.ID+"-"+input.LifecycleID)
	if err != nil {
		if errors.Is(err, controlstore.ErrPrecondition) {
			return nil, computeHAPayload{}, false, computeError(http.StatusConflict, "ha_claim_conflict", "HA transaction changed during supersession", nil)
		}
		return nil, computeHAPayload{}, false, err
	}
	// Supersession safety history is carried in the replacement payload and was
	// committed by the same CAS. External audit rows are intentionally not part
	// of the ownership protocol, so a crash cannot create a false history fence.
	return updated.(*model.Operation), replacement, true, nil
}

func (s *Server) loadHAOperationPortStates(ctx context.Context, payload computeHAPayload) ([]*model.Port, error) {
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: payload.VMID})
	if err != nil {
		return nil, err
	}
	if len(resources) != len(payload.Ports) {
		return nil, computeError(http.StatusConflict, "ha_port_set_drift", "VM PVN port set changed during HA transaction", nil)
	}
	byID := make(map[string]*model.Port, len(resources))
	for _, resource := range resources {
		byID[resource.GetMetadata().ID] = resource.(*model.Port)
	}
	for _, intent := range payload.Ports {
		port := byID[intent.PortID]
		if port == nil || !haPortConfigMatches(port, payload.VMID, intent) {
			return nil, computeError(http.StatusConflict, "ha_port_drift", "PVN port changed outside its HA transaction", map[string]any{"port_id": intent.PortID})
		}
		prior := false
		for _, predecessor := range haPortPredecessors(intent) {
			if haPortMatchesPosition(port, predecessor) {
				prior = true
				break
			}
		}
		target := port.NodeID == payload.TargetNodeID && port.RequestedChassis == payload.Target && migrationPhaseRevisionMatches(port, intent.TargetRevision) && port.Generation == intent.TargetGeneration
		if (!prior && !target) || !migrationBindingReady(port.BindingStatus) {
			return nil, computeError(http.StatusConflict, "ha_port_drift", "PVN port is outside its exact HA prior/target states", computePortDetails(port))
		}
	}
	result := make([]*model.Port, 0, len(payload.Ports))
	for _, intent := range payload.Ports {
		result = append(result, byID[intent.PortID])
	}
	return result, nil
}

func (s *Server) loadHAOperation(ctx context.Context, id string) (*model.Operation, computeHAPayload, error) {
	resource, err := s.store.Get(ctx, model.KindOperation, id)
	if err != nil {
		return nil, computeHAPayload{}, err
	}
	operation := resource.(*model.Operation)
	payload, err := decodeHAPayload(operation)
	if err == nil {
		err = s.validateHAMigrationAudit(ctx, operation, payload)
	}
	return operation, payload, err
}

func (s *Server) validateHAMigrationAudit(ctx context.Context, operation *model.Operation, payload computeHAPayload) error {
	audit := payload.MigrationRecovery
	if audit == nil {
		return nil
	}
	resource, err := s.store.Get(ctx, model.KindOperation, audit.HistoryOperationID)
	if err != nil {
		return computeError(http.StatusConflict, "ha_migration_audit_missing", "referenced migration takeover audit is unavailable", map[string]any{"operation_id": operation.ID, "history_operation_id": audit.HistoryOperationID})
	}
	history := resource.(*model.Operation)
	original, err := decodeMigrationPayload(history)
	if err != nil || history.Action != computeMigrationAction || history.OperationStatus != model.OperationFailed ||
		history.TargetID != computeHistoryOperationTarget(computeVMOperationTarget(payload.VMID), history.ID) ||
		original.HARecovery != nil || original.LifecycleID != audit.LifecycleID || original.Phase != audit.OriginalPhase || original.VMID != payload.VMID || original.Online != audit.Online ||
		original.SourceNodeID != audit.SourceNodeID || original.SourceNode != audit.SourceNode || original.Source != audit.SourceChassis ||
		original.TargetNodeID != audit.TargetNodeID || original.TargetNode != audit.TargetNode || original.Target != audit.TargetChassis || migrationPayloadHash(original) != audit.PayloadHash {
		return computeError(http.StatusConflict, "ha_migration_audit_invalid", "referenced migration takeover audit differs from the immutable original transaction", map[string]any{"operation_id": operation.ID, "history_operation_id": audit.HistoryOperationID})
	}
	direction, ok := migrationHARecoveryDirection(original)
	if !ok || direction != audit.Direction || len(original.Ports) != len(audit.PortIDs) {
		return computeError(http.StatusConflict, "ha_migration_audit_invalid", "referenced migration takeover direction or port set is invalid", map[string]any{"operation_id": operation.ID})
	}
	for _, intent := range original.Ports {
		if _, found := slices.BinarySearch(audit.PortIDs, intent.PortID); !found {
			return computeError(http.StatusConflict, "ha_migration_audit_invalid", "referenced migration takeover port set differs from the original transaction", map[string]any{"operation_id": operation.ID})
		}
	}
	return nil
}

func decodeHAPayload(operation *model.Operation) (computeHAPayload, error) {
	var payload computeHAPayload
	if operation.Action != computeHAAction || model.UnmarshalOperationPayload(operation.Payload, &payload) != nil || payload.Version != computePayloadVersion || payload.LifecycleID == "" || payload.VMID < 1 ||
		payload.TargetNodeID == "" || payload.TargetNode == "" || payload.Target == "" || len(payload.Ports) == 0 || !validHAServiceUID(payload.Proof.ServiceUID) {
		return payload, computeError(http.StatusConflict, "ha_payload_invalid", "durable HA transaction payload is invalid", map[string]any{"operation_id": operation.ID})
	}
	seen := make(map[string]bool, len(payload.Ports))
	for _, port := range payload.Ports {
		if port.PortID == "" || port.NIC == "" || port.NetworkID == "" || port.PriorNodeID == "" || port.PriorNode == "" || port.PriorChassis == "" || port.PriorRevision < 1 ||
			port.PriorGeneration < 1 || port.TargetRevision < 1 || port.TargetGeneration < 1 || seen[port.PortID] {
			return payload, computeError(http.StatusConflict, "ha_payload_invalid", "durable HA port manifest is incomplete or ambiguous", map[string]any{"operation_id": operation.ID})
		}
		for _, predecessor := range port.Predecessors {
			if predecessor.NodeID == "" || predecessor.Node == "" || predecessor.Chassis == "" || predecessor.RevisionFloor < 1 || predecessor.Generation < 1 {
				return payload, computeError(http.StatusConflict, "ha_payload_invalid", "durable HA predecessor manifest is incomplete", map[string]any{"operation_id": operation.ID, "port_id": port.PortID})
			}
		}
		seen[port.PortID] = true
	}
	if len(payload.AuthorityHistory) > 32 {
		return payload, computeError(http.StatusConflict, "ha_payload_invalid", "durable HA authority history exceeds its safety bound", map[string]any{"operation_id": operation.ID})
	}
	for _, proof := range payload.AuthorityHistory {
		if proof.ServiceID != "vm:"+fmt.Sprint(payload.VMID) || !validHAServiceUID(proof.ServiceUID) || proof.ServiceNode == "" || proof.LRMNode == "" || proof.ManagerEpoch < 1 || proof.LRMEpoch < 1 || proof.AgentLockEpoch < 1 {
			return payload, computeError(http.StatusConflict, "ha_payload_invalid", "durable HA authority history is incomplete", map[string]any{"operation_id": operation.ID})
		}
	}
	if audit := payload.MigrationRecovery; audit != nil {
		if audit.OperationID != operation.ID || audit.HistoryOperationID == "" || audit.LifecycleID == "" ||
			(audit.Direction != "source" && audit.Direction != "target") || audit.OriginalPhase == "" || len(audit.PayloadHash) != sha256.Size*2 ||
			audit.SourceNodeID == "" || audit.SourceNode == "" || audit.SourceChassis == "" || audit.TargetNodeID == "" || audit.TargetNode == "" || audit.TargetChassis == "" ||
			len(audit.PortIDs) != len(payload.Ports) || !sort.StringsAreSorted(audit.PortIDs) {
			return payload, computeError(http.StatusConflict, "ha_payload_invalid", "durable HA migration recovery audit is incomplete", map[string]any{"operation_id": operation.ID})
		}
		for _, port := range payload.Ports {
			if _, found := slices.BinarySearch(audit.PortIDs, port.PortID); !found {
				return payload, computeError(http.StatusConflict, "ha_payload_invalid", "durable HA migration recovery port set differs from HA ownership", map[string]any{"operation_id": operation.ID})
			}
		}
	}
	return payload, nil
}

func (s *Server) assertHAOperationOwner(ctx context.Context, id string, expected computeHAPayload) error {
	operation, payload, err := s.loadHAOperation(ctx, id)
	if err != nil {
		return err
	}
	if operation.OperationStatus != model.OperationRunning || operation.TargetID != computeVMOperationTarget(expected.VMID) || operation.TargetRevision != 1 || !reflect.DeepEqual(payload, expected) {
		return computeError(http.StatusConflict, "ha_claim_conflict", "HA lifecycle ownership changed while ports were being rebound", map[string]any{"operation_id": id})
	}
	return nil
}

func (s *Server) recordHAClaimError(ctx context.Context, id string, expected computeHAPayload, failure string) error {
	operation, payload, err := s.loadHAOperation(ctx, id)
	if err != nil {
		return err
	}
	if operation.OperationStatus != model.OperationRunning || operation.TargetID != computeVMOperationTarget(expected.VMID) || !reflect.DeepEqual(payload, expected) {
		return computeError(http.StatusConflict, "ha_claim_conflict", "HA lifecycle ownership changed before its recovery error could be recorded", map[string]any{"operation_id": id})
	}
	copyResource, err := model.Clone(operation)
	if err != nil {
		return err
	}
	desired := copyResource.(*model.Operation)
	desired.Error = failure
	_, _, err = s.store.Update(ctx, desired, operation.Revision, "compute-ha-error-"+id+"-"+fmt.Sprint(operation.Revision))
	return err
}

func (s *Server) terminalizeClaimedHAOperation(ctx context.Context, id string, expected computeHAPayload, terminalPhase string, status model.OperationStatus, failure string) error {
	operation, payload, err := s.loadHAOperation(ctx, id)
	if err != nil {
		return err
	}
	terminalPayload := expected
	terminalPayload.Phase = terminalPhase
	if operation.OperationStatus == status && reflect.DeepEqual(payload, terminalPayload) && operation.TargetID == computeHistoryOperationTarget(computeVMOperationTarget(expected.VMID), id) {
		return nil
	}
	if operation.OperationStatus != model.OperationRunning || operation.TargetID != computeVMOperationTarget(expected.VMID) || operation.TargetRevision != 1 || !reflect.DeepEqual(payload, expected) {
		return computeError(http.StatusConflict, "ha_claim_conflict", "HA lifecycle ownership changed before terminal completion", map[string]any{"operation_id": id})
	}
	encoded, err := model.MarshalOperationPayload(terminalPayload)
	if err != nil {
		return err
	}
	copyResource, err := model.Clone(operation)
	if err != nil {
		return err
	}
	desired := copyResource.(*model.Operation)
	desired.Payload, desired.OperationStatus, desired.Error = encoded, status, failure
	desired.TargetID = computeHistoryOperationTarget(computeVMOperationTarget(expected.VMID), id)
	now := s.clusterGate.now().UTC()
	desired.CompletedAt = &now
	_, _, err = s.store.Update(ctx, desired, operation.Revision, "compute-ha-terminal-"+id+"-"+terminalPhase+"-"+fmt.Sprint(operation.Revision))
	if errors.Is(err, controlstore.ErrPrecondition) {
		return computeError(http.StatusConflict, "ha_claim_conflict", "HA lifecycle ownership changed during terminal completion", map[string]any{"operation_id": id})
	}
	return err
}

func computeMigrationOperationID(lifecycleID string, vmid int, sourceID, targetID string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("pvn-compute-migration:%s:%d:%s:%s", lifecycleID, vmid, sourceID, targetID)))
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(digest[:16])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func migrationTransaction(operationID string, payload computeMigrationPayload) computeMigrationTransaction {
	return computeMigrationTransaction{OperationID: operationID, PayloadHash: migrationPayloadHash(payload), Ports: append([]computeMigrationPortState(nil), payload.Ports...)}
}

func migrationPayloadHash(payload computeMigrationPayload) string {
	payload.Phase = ""
	encoded, err := model.MarshalOperationPayload(payload)
	if err != nil {
		panic("hash validated migration payload: " + err.Error())
	}
	digest := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(digest[:])
}

func (s *Server) loadMigrationOperation(ctx context.Context, id string) (*model.Operation, computeMigrationPayload, error) {
	resource, err := s.store.Get(ctx, model.KindOperation, id)
	if err != nil {
		return nil, computeMigrationPayload{}, err
	}
	operation := resource.(*model.Operation)
	if operation.Action != computeMigrationAction {
		return nil, computeMigrationPayload{}, computeError(http.StatusConflict, "migration_transaction_mismatch", "operation is not a compute migration transaction", nil)
	}
	payload, err := decodeMigrationPayload(operation)
	return operation, payload, err
}

func (s *Server) assertMigrationOperationOwner(ctx context.Context, id string, expected computeMigrationPayload) error {
	operation, payload, err := s.loadMigrationOperation(ctx, id)
	if err != nil {
		return err
	}
	if operation.OperationStatus != model.OperationRunning || operation.TargetID != computeVMOperationTarget(expected.VMID) || operation.TargetRevision != 1 || !reflect.DeepEqual(payload, expected) {
		return computeError(http.StatusConflict, "migration_claim_conflict", "migration lifecycle ownership changed while ports were being updated", map[string]any{"operation_id": id})
	}
	return nil
}

func decodeMigrationPayload(operation *model.Operation) (computeMigrationPayload, error) {
	var payload computeMigrationPayload
	if err := model.UnmarshalOperationPayload(operation.Payload, &payload); err != nil {
		return payload, computeError(http.StatusConflict, "migration_payload_invalid", "durable migration transaction payload is invalid", map[string]any{"operation_id": operation.ID, "error": err.Error()})
	}
	if payload.Version != computePayloadVersion || payload.LifecycleID == "" || payload.VMID < 1 || len(payload.Ports) == 0 ||
		payload.SourceNodeID == "" || payload.SourceNode == "" || payload.Source == "" || payload.TargetNodeID == "" || payload.TargetNode == "" || payload.Target == "" {
		return payload, computeError(http.StatusConflict, "migration_payload_invalid", "durable migration transaction payload is incomplete", map[string]any{"operation_id": operation.ID})
	}
	seenPorts := make(map[string]bool, len(payload.Ports))
	seenNICs := make(map[string]bool, len(payload.Ports))
	for _, intent := range payload.Ports {
		if intent.PortID == "" || intent.NIC == "" || intent.Name == "" || intent.MACAddress == "" || intent.NetworkID == "" || intent.LSPName == "" ||
			intent.SourceRevision < 1 || intent.PreparedRevision != intent.SourceRevision+1 || intent.FinalRevision != intent.PreparedRevision+1 ||
			intent.SourceGeneration < 1 || intent.Generation != intent.SourceGeneration+1 || !migrationBindingReady(intent.SourceBindingStatus) ||
			seenPorts[intent.PortID] || seenNICs[intent.NIC] || !sort.StringsAreSorted(intent.SecurityGroupIDs) {
			return payload, computeError(http.StatusConflict, "migration_payload_invalid", "durable migration port manifest is incomplete or ambiguous", map[string]any{"operation_id": operation.ID, "port_id": intent.PortID})
		}
		seenPorts[intent.PortID], seenNICs[intent.NIC] = true, true
	}
	recoveryPhase := strings.HasPrefix(payload.Phase, "ha-recovering-") || strings.HasPrefix(payload.Phase, "ha-recovered-")
	if recoveryPhase != (payload.HARecovery != nil) {
		return payload, computeError(http.StatusConflict, "migration_payload_invalid", "durable HA migration recovery payload and phase disagree", map[string]any{"operation_id": operation.ID})
	}
	if payload.HARecovery != nil {
		recovery := payload.HARecovery
		basePayload := payload
		basePayload.HARecovery = nil
		originalPayload := basePayload
		originalPayload.Phase = recovery.OriginalPhase
		originalDirection, directionOK := migrationHARecoveryDirection(originalPayload)
		if (recovery.Direction != "source" && recovery.Direction != "target") || recovery.OriginalPhase == "" || !directionOK || originalDirection != recovery.Direction ||
			recovery.MigrationHash != migrationPayloadHash(basePayload) ||
			recovery.Proof.ServiceID != "vm:"+fmt.Sprint(payload.VMID) || recovery.Proof.ServiceUID == "" {
			return payload, computeError(http.StatusConflict, "migration_payload_invalid", "durable HA migration recovery authority is incomplete", map[string]any{"operation_id": operation.ID})
		}
	}
	return payload, nil
}

func validateMigrationTransaction(input computeMigrationFinishRequest, operation *model.Operation, payload computeMigrationPayload) error {
	if payload.LifecycleID != input.LifecycleID || payload.VMID != input.VMID || payload.SourceNode != input.SourceNode || payload.TargetNode != input.TargetNode || payload.Online != input.Online {
		return computeError(http.StatusConflict, "migration_transaction_mismatch", "migration request differs from the durable transaction", nil)
	}
	expected := migrationTransaction(operation.ID, payload)
	if input.Transaction.PayloadHash != expected.PayloadHash || !reflect.DeepEqual(input.Transaction.Ports, expected.Ports) {
		return computeError(http.StatusConflict, "migration_transaction_mismatch", "migration transaction echo is incomplete, reordered, duplicated, or stale", nil)
	}
	return nil
}

func (s *Server) writeComputeError(writer http.ResponseWriter, err error) {
	var lifecycle *computeLifecycleError
	if errors.As(err, &lifecycle) {
		writeError(writer, lifecycle.status, lifecycle.code, lifecycle.message, lifecycle.details)
		return
	}
	s.storeError(writer, err)
}
