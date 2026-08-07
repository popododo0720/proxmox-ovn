package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
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

	computeMigrationAction = "compute-migration"
	computePayloadVersion  = 1
	computeIntentLifetime  = 15 * time.Minute
	computeRecoveryTimeout = 15 * time.Second
	haStabilizationDelay   = 30 * time.Second
	computeClockSkew       = 5 * time.Minute
	maxComputeWriteRetries = 8
)

type forcedReconciler interface {
	ReconcileForced(context.Context, model.Kind, string) error
}

type computeNIC struct {
	NIC        string `json:"nic"`
	MACAddress string `json:"mac_address"`
}

type computeStartRequest struct {
	LifecycleID     string       `json:"lifecycle_id,omitempty"`
	VMID            int          `json:"vmid"`
	Node            string       `json:"node"`
	NICs            []computeNIC `json:"nics"`
	MigrationSource string       `json:"migration_source,omitempty"`
	HAManaged       bool         `json:"ha_managed,omitempty"`
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
	Version      int                         `json:"version"`
	LifecycleID  string                      `json:"lifecycle_id"`
	Phase        string                      `json:"phase"`
	VMID         int                         `json:"vmid"`
	Online       bool                        `json:"online"`
	SourceNodeID string                      `json:"source_node_id"`
	SourceNode   string                      `json:"source_node"`
	Source       string                      `json:"source_chassis"`
	TargetNodeID string                      `json:"target_node_id"`
	TargetNode   string                      `json:"target_node"`
	Target       string                      `json:"target_chassis"`
	StartedAt    time.Time                   `json:"started_at"`
	ExpiresAt    time.Time                   `json:"expires_at"`
	Ports        []computeMigrationPortState `json:"ports"`
}

type computeMigrationPortState struct {
	PortID           string `json:"port_id"`
	NIC              string `json:"nic"`
	MACAddress       string `json:"mac_address"`
	SourceRevision   int64  `json:"source_revision"`
	PreparedRevision int64  `json:"prepared_revision"`
	SourceGeneration int64  `json:"source_generation"`
	Generation       int64  `json:"generation"`
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
		if input.HAManaged || input.MigrationSource == "" {
			s.writeComputeError(writer, computeError(http.StatusConflict, "migration_intent_required", "dual-chassis PVN ports may start only as the target of their fresh migration transaction", nil))
			return
		}
		if err := s.authorizeMigrationTargetStart(request.Context(), input, target, ports); err != nil {
			s.writeComputeError(writer, err)
			return
		}
	case wrongChassis:
		if !input.HAManaged || input.MigrationSource != "" || input.LifecycleID == "" {
			s.writeComputeError(writer, computeError(http.StatusConflict, "wrong_chassis", "PVN ports are assigned to another chassis and no exact HA recovery proof was supplied", nil))
			return
		}
		ports, err = s.rebindHAPorts(request.Context(), input, target, ports)
		if err != nil {
			s.writeComputeError(writer, err)
			return
		}
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
	if source.ID == target.ID {
		s.writeComputeError(writer, computeError(http.StatusBadRequest, "invalid_request", "source_node and target_node must identify different nodes", nil))
		return
	}
	ports, err := s.loadExactComputePorts(request.Context(), input.VMID, input.NICs)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	operationID := computeMigrationOperationID(input.LifecycleID, input.VMID, source.ID, target.ID)
	operation, payload, replayed, err := s.loadOrCreateMigrationIntent(request.Context(), input, source, target, ports, operationID)
	if err != nil {
		s.writeComputeError(writer, err)
		return
	}
	if err := s.prepareMigrationPorts(request.Context(), payload); err != nil {
		recoveryContext, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), computeRecoveryTimeout)
		compensationErrors := s.compensateMigrationPorts(recoveryContext, payload)
		recoveryRequired := len(compensationErrors) != 0
		phase := "compensated"
		if recoveryRequired {
			phase = "recovery-required"
		}
		terminalErr := s.terminalizeMigrationOperation(recoveryContext, operation.ID, phase, model.OperationFailed, err.Error())
		cancel()
		if terminalErr != nil {
			compensationErrors = append(compensationErrors, "record terminal intent: "+terminalErr.Error())
			recoveryRequired = true
		}
		details := map[string]any{
			"operation_id": operation.ID, "transaction": migrationTransaction(operation.ID, payload),
			"recovery_required": recoveryRequired, "compensation_errors": compensationErrors,
		}
		s.writeComputeError(writer, computeError(http.StatusServiceUnavailable, "migration_prepare_failed", err.Error(), details))
		return
	}
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"lifecycle_id": input.LifecycleID, "vmid": input.VMID, "source_node": source.Name, "target_node": target.Name, "online": input.Online,
		"transaction": migrationTransaction(operation.ID, payload),
	}})
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
	wantedPhase := "finalized"
	if abort {
		wantedPhase = "aborted"
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
	if err := s.finishMigrationPorts(request.Context(), payload, abort); err != nil {
		s.writeComputeError(writer, computeError(http.StatusServiceUnavailable, "migration_finish_failed", err.Error(), map[string]any{
			"operation_id": operation.ID, "recovery_required": true, "transaction": input.Transaction,
		}))
		return
	}
	if err := s.terminalizeMigrationOperation(request.Context(), operation.ID, wantedPhase, model.OperationSucceeded, ""); err != nil {
		s.writeComputeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{"lifecycle_id": input.LifecycleID, "state": wantedPhase}})
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
		if operation.Action != computeMigrationAction || operation.OperationStatus != model.OperationRunning || payload.Phase != "prepared" {
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
	if err := s.ensureNoActiveMigration(ctx, input.VMID, operationID); err != nil {
		return nil, computeMigrationPayload{}, false, err
	}
	payload, err := newMigrationPayload(s.clusterGate.now().UTC(), input, source, target, ports)
	if err != nil {
		return nil, computeMigrationPayload{}, false, err
	}
	encoded, err := model.MarshalOperationPayload(payload)
	if err != nil {
		return nil, computeMigrationPayload{}, false, err
	}
	digest := sha256.Sum256([]byte(operationID))
	operation := &model.Operation{
		Metadata: model.Metadata{ID: operationID}, Action: computeMigrationAction,
		TargetKind: model.KindPort, TargetID: payload.Ports[0].PortID,
		TargetRevision:  math.MaxInt64 - int64(binary.BigEndian.Uint64(digest[:8])&((1<<60)-1)),
		OperationStatus: model.OperationRunning, IdempotencyKey: "compute-migration-" + hex.EncodeToString(digest[:]),
		LeaseOwner: "compute-lifecycle", StartedAt: &payload.StartedAt, Payload: encoded,
	}
	created, replayed, err := s.store.Create(ctx, operation, operation.IdempotencyKey)
	if err != nil {
		return nil, computeMigrationPayload{}, false, err
	}
	return created.(*model.Operation), payload, replayed, nil
}

func newMigrationPayload(now time.Time, input computeMigrationBeginRequest, source, target *model.Node, ports []*model.Port) (computeMigrationPayload, error) {
	payload := computeMigrationPayload{
		Version: computePayloadVersion, LifecycleID: input.LifecycleID, Phase: "prepared", VMID: input.VMID, Online: input.Online,
		SourceNodeID: source.ID, SourceNode: source.Name, Source: source.ChassisID,
		TargetNodeID: target.ID, TargetNode: target.Name, Target: target.ChassisID,
		StartedAt: now, ExpiresAt: now.Add(computeIntentLifetime),
		Ports: make([]computeMigrationPortState, 0, len(ports)),
	}
	for _, port := range ports {
		if port.Generation < 1 || port.Generation == math.MaxInt64 {
			return computeMigrationPayload{}, computeError(http.StatusConflict, "generation_exhausted", "PVN port has no usable migration generation", computePortDetails(port))
		}
		if port.NodeID != source.ID || port.RequestedChassis != source.ChassisID || (port.BindingStatus != model.PortBinding && port.BindingStatus != model.PortBound) {
			return computeMigrationPayload{}, computeError(http.StatusConflict, "migration_source_mismatch", "every PVN port must be connected and exclusively assigned to the declared source", computePortDetails(port))
		}
		payload.Ports = append(payload.Ports, computeMigrationPortState{
			PortID: port.ID, NIC: port.NIC, MACAddress: strings.ToLower(port.MACAddress), SourceRevision: port.Revision,
			PreparedRevision: port.Revision + 1, SourceGeneration: port.Generation, Generation: port.Generation + 1,
		})
	}
	sort.Slice(payload.Ports, func(i, j int) bool { return payload.Ports[i].NIC < payload.Ports[j].NIC })
	return payload, nil
}

func validateBeginReplay(input computeMigrationBeginRequest, source, target *model.Node, ports []*model.Port, payload computeMigrationPayload) error {
	if payload.Version != computePayloadVersion || payload.LifecycleID != input.LifecycleID || payload.VMID != input.VMID || payload.Online != input.Online ||
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

func (s *Server) prepareMigrationPorts(ctx context.Context, payload computeMigrationPayload) error {
	ports, err := s.loadExactPayloadPorts(ctx, payload)
	if err != nil {
		return err
	}
	for index, current := range ports {
		intent := payload.Ports[index]
		desiredNodeID, desiredChassis := payload.TargetNodeID, payload.Target
		if payload.Online {
			desiredNodeID, desiredChassis = payload.SourceNodeID, payload.Source+","+payload.Target
		}
		if current.Generation == intent.Generation && current.NodeID == desiredNodeID && current.RequestedChassis == desiredChassis {
			if _, err := s.forceRealizeComputePort(ctx, current); err != nil {
				return err
			}
			continue
		}
		if current.Generation != intent.SourceGeneration || current.Revision != intent.SourceRevision || current.NodeID != payload.SourceNodeID || current.RequestedChassis != payload.Source {
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
		if updated.GetMetadata().Revision != intent.PreparedRevision {
			return computeError(http.StatusConflict, "migration_revision_drift", "prepared PVN port revision differs from its durable transaction", computePortDetails(updated.(*model.Port)))
		}
		if _, err := s.forceRealizeComputePort(ctx, updated.(*model.Port)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) compensateMigrationPorts(ctx context.Context, payload computeMigrationPayload) []string {
	failures := make([]string, 0)
	for _, intent := range payload.Ports {
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
			(current.Generation == intent.SourceGeneration || current.Generation == intent.Generation):
			// Already source-only. The higher generation is retained as a fence.
		case current.NodeID == preparedNodeID && current.RequestedChassis == preparedChassis && current.Generation == intent.Generation:
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

func (s *Server) finishMigrationPorts(ctx context.Context, payload computeMigrationPayload, abort bool) error {
	ports, err := s.loadExactPayloadPorts(ctx, payload)
	if err != nil {
		return err
	}
	for index, current := range ports {
		intent := payload.Ports[index]
		if current.Generation != intent.Generation {
			return computeError(http.StatusConflict, "stale_generation", "migration completion was fenced by a newer PVN port generation", computePortDetails(current))
		}
		desiredNodeID, desiredChassis := payload.TargetNodeID, payload.Target
		if abort {
			desiredNodeID, desiredChassis = payload.SourceNodeID, payload.Source
		}
		if current.NodeID == desiredNodeID && current.RequestedChassis == desiredChassis {
			if _, err := s.forceRealizeComputePort(ctx, current); err != nil {
				return err
			}
			continue
		}
		preparedNodeID, preparedChassis := payload.TargetNodeID, payload.Target
		if payload.Online {
			preparedNodeID, preparedChassis = payload.SourceNodeID, payload.Source+","+payload.Target
		}
		if current.NodeID != preparedNodeID || current.RequestedChassis != preparedChassis {
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
		if port.ID != intent.PortID || port.NIC != intent.NIC || !strings.EqualFold(port.MACAddress, intent.MACAddress) ||
			port.Generation != intent.Generation || port.NodeID != payload.SourceNodeID || port.RequestedChassis != payload.Source+","+payload.Target {
			return false
		}
	}
	return true
}

func (s *Server) rebindHAPorts(ctx context.Context, input computeStartRequest, target *model.Node, ports []*model.Port) ([]*model.Port, error) {
	if err := validateLifecycleID("lifecycle_id", input.LifecycleID); err != nil {
		return nil, err
	}
	var source *model.Node
	for _, port := range ports {
		requested, err := model.ParseRequestedChassis(port.RequestedChassis)
		if err != nil || len(requested) != 1 || port.NodeID == "" {
			return nil, computeError(http.StatusConflict, "ha_source_mismatch", "HA recovery requires ports exclusively assigned to one former chassis", computePortDetails(port))
		}
		node, err := s.resolveAttachmentNode(ctx, port.NodeID)
		if err != nil || node.ChassisID != requested[0] {
			return nil, computeError(http.StatusConflict, "ha_source_mismatch", "HA recovery source ownership is inconsistent", computePortDetails(port))
		}
		if source == nil {
			source = node
		} else if source.ID != node.ID {
			return nil, computeError(http.StatusConflict, "ha_source_mismatch", "HA recovery ports span multiple former nodes", nil)
		}
	}
	if source == nil || source.ID == target.ID {
		return nil, computeError(http.StatusConflict, "ha_source_mismatch", "HA recovery has no distinct former source node", nil)
	}
	if err := s.requireHAFence(source, target); err != nil {
		return nil, err
	}
	result := append([]*model.Port(nil), ports...)
	changed := make([]*model.Port, 0, len(ports))
	for index, current := range ports {
		if current.Generation == math.MaxInt64 {
			return nil, computeError(http.StatusConflict, "generation_exhausted", "PVN port generation is exhausted", computePortDetails(current))
		}
		desired := clonePort(current)
		desired.Metadata = model.Metadata{ID: current.ID}
		desired.NodeID, desired.RequestedChassis = target.ID, target.ChassisID
		desired.BindingStatus = model.PortBinding
		desired.Generation++
		updated, _, err := s.store.Update(ctx, desired, current.Revision, "compute-ha-rebind-"+input.LifecycleID+"-"+current.ID)
		if err != nil {
			return nil, s.compensateHAFailure(ctx, source, changed, err)
		}
		prepared := updated.(*model.Port)
		changed = append(changed, prepared)
		realized, err := s.forceRealizeComputePort(ctx, prepared)
		if err != nil {
			return nil, s.compensateHAFailure(ctx, source, changed, err)
		}
		result[index] = realized
	}
	return result, nil
}

func (s *Server) compensateHAFailure(ctx context.Context, source *model.Node, changed []*model.Port, cause error) error {
	recoveryContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), computeRecoveryTimeout)
	defer cancel()
	failures := make([]string, 0)
	for _, changedPort := range changed {
		current, err := s.loadPort(recoveryContext, changedPort.ID)
		if err != nil || current.Generation != changedPort.Generation {
			failures = append(failures, changedPort.ID+": changed during HA compensation")
			continue
		}
		desired := clonePort(current)
		desired.Metadata = model.Metadata{ID: current.ID}
		desired.NodeID, desired.RequestedChassis = source.ID, source.ChassisID
		desired.BindingStatus = model.PortBinding
		updated, _, err := s.store.Update(recoveryContext, desired, current.Revision, "compute-ha-compensate-"+current.ID+"-"+fmt.Sprint(current.Generation))
		if err == nil {
			_, err = s.forceRealizeComputePort(recoveryContext, updated.(*model.Port))
		}
		if err != nil {
			failures = append(failures, current.ID+": "+err.Error())
		}
	}
	return computeError(http.StatusServiceUnavailable, "ha_rebind_failed", cause.Error(), map[string]any{"recovery_required": len(failures) != 0, "compensation_errors": failures})
}

func (s *Server) requireHAFence(source, target *model.Node) error {
	gate := s.clusterGate
	if gate == nil || !gate.required {
		return computeError(http.StatusConflict, "ha_fence_unavailable", "HA rebind requires fresh quorate PVE membership fencing", nil)
	}
	gate.mu.RLock()
	reported, quorate := gate.reported, gate.quorate
	online := append([]string(nil), gate.online...)
	gate.mu.RUnlock()
	now := gate.now().UTC()
	if reported.IsZero() || now.Sub(reported) > gate.ttl || !quorate {
		return computeError(http.StatusServiceUnavailable, "membership_stale", "fresh quorate PVE membership is required for HA rebind", nil)
	}
	if !slices.Contains(online, target.Name) {
		return computeError(http.StatusConflict, "ha_target_offline", "HA target is absent from fresh PVE membership", map[string]any{"target": target.Name})
	}
	if slices.Contains(online, source.Name) {
		return computeError(http.StatusConflict, "ha_source_online", "former HA source is still online; refusing split-brain port rebind", map[string]any{"source": source.Name})
	}
	if source.LastSeenAt == nil || now.Sub(source.LastSeenAt.UTC()) <= gate.ttl+haStabilizationDelay {
		return computeError(http.StatusConflict, "ha_source_not_stale", "former HA source heartbeat has not passed the fencing stabilization interval", map[string]any{"source": source.Name, "last_seen_at": source.LastSeenAt})
	}
	return nil
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

func decodeMigrationPayload(operation *model.Operation) (computeMigrationPayload, error) {
	var payload computeMigrationPayload
	if err := model.UnmarshalOperationPayload(operation.Payload, &payload); err != nil {
		return payload, computeError(http.StatusConflict, "migration_payload_invalid", "durable migration transaction payload is invalid", map[string]any{"operation_id": operation.ID, "error": err.Error()})
	}
	if payload.Version != computePayloadVersion || payload.LifecycleID == "" || payload.VMID < 1 || len(payload.Ports) == 0 {
		return payload, computeError(http.StatusConflict, "migration_payload_invalid", "durable migration transaction payload is incomplete", map[string]any{"operation_id": operation.ID})
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

func (s *Server) terminalizeMigrationOperation(ctx context.Context, id, phase string, status model.OperationStatus, failure string) error {
	for attempt := 0; attempt < maxComputeWriteRetries; attempt++ {
		resource, err := s.store.Get(ctx, model.KindOperation, id)
		if err != nil {
			return err
		}
		operation := resource.(*model.Operation)
		payload, err := decodeMigrationPayload(operation)
		if err != nil {
			return err
		}
		if operation.OperationStatus != model.OperationRunning {
			if operation.OperationStatus == status && payload.Phase == phase {
				return nil
			}
			return computeError(http.StatusConflict, "migration_transaction_terminal", "migration transaction is already terminal", map[string]any{"phase": payload.Phase, "status": operation.OperationStatus})
		}
		payload.Phase = phase
		encoded, err := model.MarshalOperationPayload(payload)
		if err != nil {
			return err
		}
		now := s.clusterGate.now().UTC()
		operation.Payload = encoded
		operation.OperationStatus = status
		operation.CompletedAt = &now
		operation.Error = failure
		_, _, err = s.store.Update(ctx, operation, operation.Revision, "compute-migration-terminal-"+id+"-"+phase)
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return computeError(http.StatusConflict, "migration_terminal_conflict", "migration transaction could not be terminalized after concurrent updates", nil)
}

func (s *Server) writeComputeError(writer http.ResponseWriter, err error) {
	var lifecycle *computeLifecycleError
	if errors.As(err, &lifecycle) {
		writeError(writer, lifecycle.status, lifecycle.code, lifecycle.message, lifecycle.details)
		return
	}
	s.storeError(writer, err)
}
