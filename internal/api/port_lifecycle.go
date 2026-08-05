package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	portActionAttach = "attach"
	portActionDetach = "detach"
)

type portAttachRequest struct {
	NodeID           string `json:"node_id"`
	VMID             int    `json:"vmid"`
	NIC              string `json:"nic"`
	RequestedChassis string `json:"requested_chassis,omitempty"`
	Generation       int64  `json:"generation"`
	ExpectedRevision int64  `json:"revision,omitempty"`
}

type portDetachRequest struct {
	Generation       int64 `json:"generation"`
	ExpectedRevision int64 `json:"revision,omitempty"`
}

type portRuntimeReport struct {
	Generation int64                   `json:"generation"`
	Status     model.PortBindingStatus `json:"status"`
}

func parsePortActionPath(path string) (string, string, bool) {
	remainder := strings.TrimPrefix(path, "/api/v1/ports/")
	if remainder == path {
		return "", "", false
	}
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 || parts[0] == "" || (parts[1] != portActionAttach && parts[1] != portActionDetach) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func isRuntimePortReportPath(path string) bool {
	_, ok := runtimePortReportID(path)
	return ok
}

func runtimePortReportID(path string) (string, bool) {
	remainder := strings.TrimPrefix(path, "/api/v1/runtime/ports/")
	if remainder == path {
		return "", false
	}
	parts := strings.Split(remainder, "/")
	returnValue := ""
	if len(parts) == 2 && parts[0] != "" && parts[1] == "report" {
		returnValue = parts[0]
	}
	return returnValue, returnValue != ""
}

func (s *Server) portAction(writer http.ResponseWriter, request *http.Request, portID, action string) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	session, ok := request.Context().Value(sessionContextKey{}).(Session)
	if !ok || session.User == "" {
		writeError(writer, http.StatusUnauthorized, "unauthenticated", "a valid Proxmox session is required", nil)
		return
	}
	key, ok := idempotencyKey(writer, request)
	if !ok {
		return
	}
	switch action {
	case portActionAttach:
		s.attachPort(writer, request, portID, key)
	case portActionDetach:
		s.detachPort(writer, request, portID, key)
	default:
		writeError(writer, http.StatusNotFound, "not_found", "endpoint was not found", nil)
	}
}

func (s *Server) attachPort(writer http.ResponseWriter, request *http.Request, portID, key string) {
	var input portAttachRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	expected, err := expectedRevision(request, input.ExpectedRevision)
	if err != nil {
		writeError(writer, http.StatusPreconditionRequired, "precondition_required", err.Error(), nil)
		return
	}
	if input.Generation < 1 {
		writeError(writer, http.StatusBadRequest, "invalid_request", "generation must contain the current positive port generation", nil)
		return
	}
	if input.Generation == math.MaxInt64 {
		writeError(writer, http.StatusConflict, "generation_exhausted", "the port generation cannot be incremented", nil)
		return
	}
	current, err := s.loadPort(request.Context(), portID)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	node, err := s.resolveAttachmentNode(request.Context(), input.NodeID)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if !node.Enabled {
		writeError(writer, http.StatusConflict, "node_disabled", "the target PVN node is disabled", nil)
		return
	}
	if input.RequestedChassis != "" && input.RequestedChassis != node.ChassisID {
		writeError(writer, http.StatusBadRequest, "invalid_request", "requested_chassis must match the target node chassis", nil)
		return
	}

	desired := clonePort(current)
	desired.Metadata = model.Metadata{ID: current.ID}
	desired.NodeID = node.ID
	desired.VMID = input.VMID
	desired.NIC = input.NIC
	desired.RequestedChassis = node.ChassisID
	desired.BindingStatus = model.PortBinding
	desired.Generation = input.Generation + 1
	if !current.AdminStateUp {
		writeError(writer, http.StatusConflict, "port_disabled", "an administratively disabled port cannot be attached", nil)
		return
	}
	if err := desired.Validate(); err != nil {
		s.storeError(writer, err)
		return
	}

	fresh := current.Revision == expected && current.Generation == input.Generation && current.BindingStatus == model.PortUnbound
	replayCandidate := current.Revision > expected && current.Generation == input.Generation+1 && sameAttachment(current, desired) &&
		(current.BindingStatus == model.PortBinding || current.BindingStatus == model.PortBound)
	if !fresh && !replayCandidate {
		if current.Revision != expected {
			s.storeError(writer, &controlstore.Error{Kind: controlstore.ErrPrecondition, Message: fmt.Sprintf("expected revision %d but current revision is %d", expected, current.Revision)})
		} else if current.Generation != input.Generation {
			writeError(writer, http.StatusConflict, "stale_generation", "the supplied port generation is stale", map[string]any{"current_generation": current.Generation})
		} else {
			writeError(writer, http.StatusConflict, "invalid_port_transition", "only an unbound port can be attached", map[string]any{"status": current.BindingStatus})
		}
		return
	}
	if err := s.authorizeWrite(request.Context(), desired, current); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	if !s.requireClusterCapacity(writer, request) {
		return
	}

	op, replayed, err := s.beginPortAction(request.Context(), portActionAttach, current.ID, expected, key, input)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if replayed && op.OperationStatus == model.OperationSucceeded {
		s.writeCurrentPort(writer, request.Context(), current.ID, true)
		return
	}
	updated, updateReplayed, err := s.store.Update(request.Context(), desired, expected, "port-action-"+op.ID)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	updated = s.reconcileAndReload(request.Context(), updated)
	if err := s.completePortAction(request.Context(), op); err != nil {
		s.logger.Error("complete port attach operation", "port", current.ID, "operation", op.ID, "error", err)
	}
	setETag(writer, updated.GetMetadata().Revision)
	if replayed || updateReplayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": updated})
}

func (s *Server) detachPort(writer http.ResponseWriter, request *http.Request, portID, key string) {
	var input portDetachRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	expected, err := expectedRevision(request, input.ExpectedRevision)
	if err != nil {
		writeError(writer, http.StatusPreconditionRequired, "precondition_required", err.Error(), nil)
		return
	}
	if input.Generation < 1 {
		writeError(writer, http.StatusBadRequest, "invalid_request", "generation must contain the current positive port generation", nil)
		return
	}
	current, err := s.loadPort(request.Context(), portID)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	desired := clonePort(current)
	desired.Metadata = model.Metadata{ID: current.ID}
	desired.BindingStatus = model.PortDetaching

	freshStatus := current.BindingStatus == model.PortBinding || current.BindingStatus == model.PortBound || current.BindingStatus == model.PortBindingError
	fresh := current.Revision == expected && current.Generation == input.Generation && freshStatus && portAttached(current)
	replayCandidate := current.Revision > expected && current.Generation == input.Generation &&
		(current.BindingStatus == model.PortDetaching || current.BindingStatus == model.PortUnbound)
	if !fresh && !replayCandidate {
		if current.Revision != expected {
			s.storeError(writer, &controlstore.Error{Kind: controlstore.ErrPrecondition, Message: fmt.Sprintf("expected revision %d but current revision is %d", expected, current.Revision)})
		} else if current.Generation != input.Generation {
			writeError(writer, http.StatusConflict, "stale_generation", "the supplied port generation is stale", map[string]any{"current_generation": current.Generation})
		} else {
			writeError(writer, http.StatusConflict, "invalid_port_transition", "only an attached port can begin detaching", map[string]any{"status": current.BindingStatus})
		}
		return
	}
	// A completed detach clears the VM identity needed for VM.Config.Network
	// evaluation. Permit only an exact, durable replay after ordinary project
	// allocation authorization; the original transition was fully authorized before
	// its operation could be marked successful.
	if replayCandidate && current.BindingStatus == model.PortUnbound {
		if err := s.authorizeWrite(request.Context(), current, current); err != nil {
			writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
			return
		}
		op, replayed, err := s.beginPortAction(request.Context(), portActionDetach, current.ID, expected, key, input)
		if err != nil {
			s.storeError(writer, err)
			return
		}
		if replayed && op.OperationStatus == model.OperationSucceeded {
			s.writeCurrentPort(writer, request.Context(), current.ID, true)
			return
		}
		writeError(writer, http.StatusPreconditionFailed, "revision_conflict", "the detach request does not match a completed operation", nil)
		return
	}
	if err := s.authorizeWrite(request.Context(), desired, current); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	op, replayed, err := s.beginPortAction(request.Context(), portActionDetach, current.ID, expected, key, input)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if replayed && op.OperationStatus == model.OperationSucceeded {
		s.writeCurrentPort(writer, request.Context(), current.ID, true)
		return
	}
	updated, updateReplayed, err := s.store.Update(request.Context(), desired, expected, "port-action-"+op.ID)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	updated = s.reconcileAndReload(request.Context(), updated)
	if err := s.completePortAction(request.Context(), op); err != nil {
		s.logger.Error("complete port detach operation", "port", current.ID, "operation", op.ID, "error", err)
	}
	setETag(writer, updated.GetMetadata().Revision)
	if replayed || updateReplayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": updated})
}

func (s *Server) reportPort(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	portID, ok := runtimePortReportID(request.URL.Path)
	if !ok {
		writeError(writer, http.StatusNotFound, "not_found", "endpoint was not found", nil)
		return
	}
	var report portRuntimeReport
	if !decodeActionBody(writer, request, &report) {
		return
	}
	if report.Generation < 1 || (report.Status != model.PortBound && report.Status != model.PortUnbound) {
		writeError(writer, http.StatusBadRequest, "invalid_report", "generation must be positive and status must be bound or unbound", nil)
		return
	}
	current, err := s.loadPort(request.Context(), portID)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if current.Generation != report.Generation {
		writeError(writer, http.StatusConflict, "stale_generation", "the agent report generation is stale", map[string]any{"current_generation": current.Generation})
		return
	}
	if current.BindingStatus == report.Status {
		setETag(writer, current.Revision)
		writeJSON(writer, http.StatusOK, map[string]any{"data": current})
		return
	}

	desired := clonePort(current)
	desired.Metadata = model.Metadata{ID: current.ID}
	switch report.Status {
	case model.PortBound:
		if current.BindingStatus != model.PortBinding {
			writeError(writer, http.StatusConflict, "invalid_port_transition", "only a binding port can be reported bound", map[string]any{"status": current.BindingStatus})
			return
		}
		desired.BindingStatus = model.PortBound
	case model.PortUnbound:
		if current.BindingStatus != model.PortDetaching {
			writeError(writer, http.StatusConflict, "invalid_port_transition", "only a detaching port can be reported unbound", map[string]any{"status": current.BindingStatus})
			return
		}
		desired.BindingStatus = model.PortUnbound
		desired.NodeID = ""
		desired.VMID = 0
		desired.NIC = ""
		desired.RequestedChassis = ""
	}
	updated, _, err := s.store.Update(request.Context(), desired, current.Revision,
		fmt.Sprintf("runtime-report-%s-%d-%s", current.ID, report.Generation, report.Status))
	if err != nil {
		s.storeError(writer, err)
		return
	}
	updated = s.reconcileAndReload(request.Context(), updated)
	setETag(writer, updated.GetMetadata().Revision)
	writeJSON(writer, http.StatusOK, map[string]any{"data": updated})
}

func (s *Server) loadPort(ctx context.Context, id string) (*model.Port, error) {
	resource, err := s.store.Get(ctx, model.KindPort, id)
	if err != nil {
		return nil, err
	}
	port, ok := resource.(*model.Port)
	if !ok {
		return nil, &controlstore.Error{Kind: controlstore.ErrConflict, Message: "stored port has an invalid type"}
	}
	return port, nil
}

func (s *Server) resolveAttachmentNode(ctx context.Context, identity string) (*model.Node, error) {
	if strings.TrimSpace(identity) == "" {
		return nil, &model.ValidationError{Field: "node_id", Message: "is required"}
	}
	resource, err := s.store.Get(ctx, model.KindNode, identity)
	if err == nil {
		return resource.(*model.Node), nil
	}
	if !errors.Is(err, controlstore.ErrNotFound) {
		return nil, err
	}
	resources, err := s.store.List(ctx, model.KindNode, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, candidate := range resources {
		node := candidate.(*model.Node)
		if node.Name == identity || node.ChassisID == identity {
			return node, nil
		}
	}
	return nil, &controlstore.Error{Kind: controlstore.ErrNotFound, Message: fmt.Sprintf("node %q was not found", identity)}
}

func (s *Server) beginPortAction(ctx context.Context, action, portID string, expected int64, key string, payload any) (*model.Operation, bool, error) {
	encoded, err := json.Marshal(struct {
		Action   string `json:"action"`
		PortID   string `json:"port_id"`
		Expected int64  `json:"expected_revision"`
		Payload  any    `json:"payload"`
	}{Action: action, PortID: portID, Expected: expected, Payload: payload})
	if err != nil {
		return nil, false, err
	}
	digest := sha256.Sum256(encoded)
	op := &model.Operation{
		Action:     "port-" + action + ":" + hex.EncodeToString(digest[:]),
		TargetKind: model.KindPort,
		TargetID:   portID,
		// Action operations occupy the upper revision range so they cannot
		// collide with ordinary reconcile operations for a resource revision.
		TargetRevision:  math.MaxInt64 - int64(binary.BigEndian.Uint64(digest[:8])&((1<<62)-1)),
		OperationStatus: model.OperationQueued,
		IdempotencyKey:  key,
	}
	created, replayed, err := s.store.Create(ctx, op, key)
	if err != nil {
		return nil, false, err
	}
	result := created.(*model.Operation)
	if replayed {
		latest, getErr := s.store.Get(ctx, model.KindOperation, result.ID)
		if getErr != nil {
			return nil, false, getErr
		}
		result = latest.(*model.Operation)
	}
	return result, replayed, nil
}

func (s *Server) completePortAction(ctx context.Context, operation *model.Operation) error {
	latestResource, err := s.store.Get(ctx, model.KindOperation, operation.ID)
	if err != nil {
		return err
	}
	latest := latestResource.(*model.Operation)
	if latest.OperationStatus == model.OperationSucceeded {
		return nil
	}
	completed := time.Now().UTC()
	latest.OperationStatus = model.OperationSucceeded
	latest.CompletedAt = &completed
	latest.Error = ""
	_, _, err = s.store.Update(ctx, latest, latest.Revision, "complete-port-action-"+latest.ID)
	return err
}

func (s *Server) writeCurrentPort(writer http.ResponseWriter, ctx context.Context, id string, replayed bool) {
	current, err := s.loadPort(ctx, id)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	setETag(writer, current.Revision)
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": current})
}

func clonePort(source *model.Port) *model.Port {
	resource, err := model.Clone(source)
	if err != nil {
		panic("clone a valid port: " + err.Error())
	}
	return resource.(*model.Port)
}

func sameAttachment(left, right *model.Port) bool {
	return left.NodeID == right.NodeID && left.VMID == right.VMID && left.NIC == right.NIC &&
		left.RequestedChassis == right.RequestedChassis
}

func decodeActionBody(writer http.ResponseWriter, request *http.Request, destination any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_json", jsonError(err), nil)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("request body must contain one JSON object")
		}
		writeError(writer, http.StatusBadRequest, "invalid_json", jsonError(err), nil)
		return false
	}
	return true
}
