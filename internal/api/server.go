package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/controlstore"
	"github.com/pvnstack/proxmox-ovn/internal/model"
)

const maxRequestBody = 1 << 20

var ErrUnauthenticated = errors.New("PVE session is not authenticated")

type Session struct {
	User        string         `json:"user"`
	CSRFToken   string         `json:"csrf_token"`
	Permissions map[string]any `json:"permissions"`
	Cluster     string         `json:"cluster"`
}

type SessionProvider interface {
	Session(context.Context, *http.Request) (Session, error)
}

type SessionIssuer interface {
	IssueSession(context.Context, http.ResponseWriter, *http.Request) (Session, error)
}

type SessionAuthorizer interface {
	Authorize(context.Context, *http.Request, bool) (Session, error)
}

type SessionProviderFunc func(context.Context, *http.Request) (Session, error)

func (f SessionProviderFunc) Session(ctx context.Context, request *http.Request) (Session, error) {
	return f(ctx, request)
}

type Reconciler interface {
	Reconcile(context.Context, model.Kind, string) error
}

type DeletionReconciler interface {
	Delete(context.Context, model.Resource) error
}

type Options struct {
	Store            controlstore.Store
	Reconciler       Reconciler
	SessionProvider  SessionProvider
	Logger           *slog.Logger
	RequireAllNodes  bool
	NodeHeartbeatTTL time.Duration
	Clock            func() time.Time
}

type Server struct {
	store           controlstore.Store
	reconciler      Reconciler
	sessionProvider SessionProvider
	logger          *slog.Logger
	clusterGate     *clusterCapacityGate
}

type sessionContextKey struct{}

func New(options Options) (*Server, error) {
	if options.Store == nil {
		return nil, errors.New("API store is required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.NodeHeartbeatTTL <= 0 {
		options.NodeHeartbeatTTL = 2 * time.Minute
	}
	return &Server{
		store: options.Store, reconciler: options.Reconciler, sessionProvider: options.SessionProvider,
		logger: options.Logger, clusterGate: newClusterCapacityGate(options.RequireAllNodes, options.NodeHeartbeatTTL, options.Clock),
	}, nil
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if request.URL.Path == "/api/v1/health" {
		s.health(writer, request)
		return
	}
	if request.URL.Path == "/api/v1/session" {
		s.session(writer, request)
		return
	}
	if !strings.HasPrefix(request.URL.Path, "/api/v1/") {
		writeError(writer, http.StatusNotFound, "not_found", "endpoint was not found", nil)
		return
	}
	if s.sessionProvider != nil {
		var session Session
		var err error
		if authorizer, ok := s.sessionProvider.(SessionAuthorizer); ok {
			session, err = authorizer.Authorize(request.Context(), request, isUnsafe(request.Method))
		} else {
			session, err = s.sessionProvider.Session(request.Context(), request)
		}
		if errors.Is(err, ErrInvalidCSRF) {
			writeError(writer, http.StatusForbidden, "invalid_csrf", "a valid PVN CSRF token is required", nil)
			return
		}
		if err != nil {
			writeError(writer, http.StatusUnauthorized, "unauthenticated", "a valid Proxmox session is required", nil)
			return
		}
		request = request.WithContext(context.WithValue(request.Context(), sessionContextKey{}, session))
	}
	if request.URL.Path == "/api/v1/runtime/ports/resolve" {
		s.resolvePort(writer, request)
		return
	}
	if request.URL.Path == "/api/v1/ports/provision" {
		s.provisionPort(writer, request)
		return
	}
	if portID, ok := parsePortDeprovisionPath(request.URL.Path); ok {
		s.deprovisionPort(writer, request, portID)
		return
	}
	if portID, action, ok := parsePortActionPath(request.URL.Path); ok {
		s.portAction(writer, request, portID, action)
		return
	}
	s.resource(writer, request)
}

// RuntimeHandler exposes only node-runtime endpoints. It is intended to be
// mounted on a root-owned Unix socket; unlike ServeHTTP it accepts no PVE
// browser session because local pvn-agent processes do not possess one.
func (s *Server) RuntimeHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		switch {
		case request.URL.Path == "/api/v1/runtime/ports/resolve":
			s.resolvePort(writer, request)
		case isRuntimePortReportPath(request.URL.Path):
			s.reportPort(writer, request)
		case request.URL.Path == "/api/v1/runtime/nodes/heartbeat":
			s.heartbeatNode(writer, request)
		default:
			writeError(writer, http.StatusNotFound, "not_found", "endpoint was not found", nil)
		}
	})
}

func (s *Server) health(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	status := s.clusterGate.status(request.Context(), s.store)
	code := http.StatusOK
	if !status.Ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(writer, code, map[string]any{"data": map[string]any{"status": status.Label(), "time": s.clusterGate.now().UTC(), "cluster": status}})
}

func (s *Server) session(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if s.sessionProvider == nil {
		writeError(writer, http.StatusUnauthorized, "unauthenticated", "a valid Proxmox session is required", nil)
		return
	}
	var session Session
	var err error
	if issuer, ok := s.sessionProvider.(SessionIssuer); ok {
		session, err = issuer.IssueSession(request.Context(), writer, request)
	} else {
		session, err = s.sessionProvider.Session(request.Context(), request)
	}
	if err != nil || session.User == "" {
		writeError(writer, http.StatusUnauthorized, "unauthenticated", "a valid Proxmox session is required", nil)
		return
	}
	if session.Permissions == nil {
		session.Permissions = map[string]any{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": session})
}

func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func (s *Server) resource(writer http.ResponseWriter, request *http.Request) {
	path := strings.Trim(strings.TrimPrefix(request.URL.Path, "/api/v1/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || len(parts) > 2 || parts[0] == "" {
		writeError(writer, http.StatusNotFound, "not_found", "endpoint was not found", nil)
		return
	}
	kind, err := model.ParseCollection(parts[0])
	if err != nil {
		writeError(writer, http.StatusNotFound, "not_found", err.Error(), nil)
		return
	}
	id := ""
	if len(parts) == 2 {
		id = parts[1]
	}
	if kind == model.KindOperation && request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	switch request.Method {
	case http.MethodGet:
		if id == "" {
			s.list(writer, request, kind)
		} else {
			s.get(writer, request, kind, id)
		}
	case http.MethodPost:
		if id != "" {
			methodNotAllowed(writer, http.MethodGet, http.MethodPut, http.MethodDelete)
			return
		}
		s.create(writer, request, kind)
	case http.MethodPut:
		if id == "" {
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
			return
		}
		s.update(writer, request, kind, id)
	case http.MethodDelete:
		if id == "" {
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
			return
		}
		s.delete(writer, request, kind, id)
	default:
		methodNotAllowed(writer, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) list(writer http.ResponseWriter, request *http.Request, kind model.Kind) {
	options := controlstore.ListOptions{
		ProjectID: request.URL.Query().Get("project_id"),
		NetworkID: request.URL.Query().Get("network_id"),
		NodeID:    request.URL.Query().Get("node_id"),
		NIC:       request.URL.Query().Get("nic"),
	}
	if rawVMID := request.URL.Query().Get("vmid"); rawVMID != "" {
		vmid, err := strconv.Atoi(rawVMID)
		if err != nil || vmid < 1 {
			writeError(writer, http.StatusBadRequest, "invalid_request", "vmid must be a positive integer", nil)
			return
		}
		options.VMID = vmid
	}
	resources, err := s.store.List(request.Context(), kind, options)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	visible := make([]model.Resource, 0, len(resources))
	for _, resource := range resources {
		if s.authorizeRead(request.Context(), resource) == nil {
			visible = append(visible, resource)
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": visible})
}

func (s *Server) get(writer http.ResponseWriter, request *http.Request, kind model.Kind, id string) {
	resource, err := s.store.Get(request.Context(), kind, id)
	if err != nil {
		if errors.Is(err, controlstore.ErrNotFound) {
			writeError(writer, http.StatusNotFound, "not_found", "resource was not found", nil)
			return
		}
		s.storeError(writer, err)
		return
	}
	if err := s.authorizeRead(request.Context(), resource); err != nil {
		// Use a consistent not-found response so callers cannot use object GETs
		// to enumerate resources outside their PVE pools.
		writeError(writer, http.StatusNotFound, "not_found", "resource was not found", nil)
		return
	}
	setETag(writer, resource.GetMetadata().Revision)
	writeJSON(writer, http.StatusOK, map[string]any{"data": resource})
}

func (s *Server) create(writer http.ResponseWriter, request *http.Request, kind model.Kind) {
	key, ok := idempotencyKey(writer, request)
	if !ok {
		return
	}
	resource, err := decodeResource(writer, request, kind)
	if err != nil {
		return
	}
	if !metadataEmpty(resource.GetMetadata()) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "server-managed metadata must be omitted when creating a resource", nil)
		return
	}
	if port, ok := resource.(*model.Port); ok {
		if portBindingFieldsSet(port) {
			writeError(writer, http.StatusBadRequest, "server_managed_field", "node_id, vmid, nic, requested_chassis, binding_status, lsp_name, and generation are server-managed", nil)
			return
		}
	}
	if err := s.authorizeWrite(request.Context(), resource, nil); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	if kind == model.KindPort && !s.requireClusterCapacity(writer, request) {
		return
	}
	created, replayed, err := s.store.Create(request.Context(), resource, key)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	created = s.reconcileAndReload(request.Context(), created)
	setETag(writer, created.GetMetadata().Revision)
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
		writeJSON(writer, http.StatusOK, map[string]any{"data": created})
		return
	}
	writer.Header().Set("Location", "/api/v1/"+kind.Collection()+"/"+created.GetMetadata().ID)
	writeJSON(writer, http.StatusCreated, map[string]any{"data": created})
}

func (s *Server) update(writer http.ResponseWriter, request *http.Request, kind model.Kind, id string) {
	key, ok := idempotencyKey(writer, request)
	if !ok {
		return
	}
	resource, err := decodeResource(writer, request, kind)
	if err != nil {
		return
	}
	meta := resource.GetMetadata()
	if meta.ID != "" && meta.ID != id {
		writeError(writer, http.StatusBadRequest, "invalid_request", "body id does not match URL", nil)
		return
	}
	meta.ID = id
	current, getErr := s.store.Get(request.Context(), kind, id)
	if getErr != nil {
		s.storeError(writer, getErr)
		return
	}
	if err := s.authorizeWrite(request.Context(), resource, current); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	expected, parseErr := expectedRevision(request, meta.Revision)
	if parseErr != nil {
		writeError(writer, http.StatusPreconditionRequired, "precondition_required", parseErr.Error(), nil)
		return
	}
	if expected == current.GetMetadata().Revision {
		if !metadataUpdateAllowed(meta, current.GetMetadata()) {
			writeError(writer, http.StatusBadRequest, "server_managed_field", "resource metadata is server-managed", nil)
			return
		}
		if port, ok := resource.(*model.Port); ok {
			oldPort := current.(*model.Port)
			if !samePortBindingFields(port, oldPort) {
				writeError(writer, http.StatusBadRequest, "server_managed_field", "node_id, vmid, nic, requested_chassis, binding_status, lsp_name, and generation are server-managed", nil)
				return
			}
		}
	}
	updated, replayed, err := s.store.Update(request.Context(), resource, expected, key)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	updated = s.reconcileAndReload(request.Context(), updated)
	setETag(writer, updated.GetMetadata().Revision)
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": updated})
}

func (s *Server) delete(writer http.ResponseWriter, request *http.Request, kind model.Kind, id string) {
	key, ok := idempotencyKey(writer, request)
	if !ok {
		return
	}
	expected, err := expectedRevision(request, 0)
	if err != nil {
		writeError(writer, http.StatusPreconditionRequired, "precondition_required", err.Error(), nil)
		return
	}
	current, getErr := s.store.Get(request.Context(), kind, id)
	if getErr != nil && !errors.Is(getErr, controlstore.ErrNotFound) {
		s.storeError(writer, getErr)
		return
	}
	if getErr == nil {
		if err := s.authorizeWrite(request.Context(), current, current); err != nil {
			writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
			return
		}
	}
	tombstone, replayed, err := s.store.BeginDelete(request.Context(), kind, id, expected, key)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if getErr != nil {
		if err := s.authorizeWrite(request.Context(), tombstone, tombstone); err != nil {
			writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
			return
		}
	}
	if deletionReconciler, ok := s.reconciler.(DeletionReconciler); ok {
		if err := deletionReconciler.Delete(request.Context(), tombstone); err != nil {
			s.logger.Error("resource deletion reconciliation failed", "kind", kind, "id", id, "revision", tombstone.GetMetadata().Revision, "error", err)
			writeError(writer, http.StatusServiceUnavailable, "reconcile_failed", err.Error(), nil)
			return
		}
	}
	if err := s.store.Purge(request.Context(), kind, id, tombstone.GetMetadata().Revision); err != nil {
		s.storeError(writer, err)
		return
	}
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Del("Content-Type")
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) reconcileAndReload(ctx context.Context, resource model.Resource) model.Resource {
	if s.reconciler == nil || resource.ResourceKind() == model.KindOperation {
		return resource
	}
	if err := s.reconciler.Reconcile(ctx, resource.ResourceKind(), resource.GetMetadata().ID); err != nil {
		s.logger.Error("resource reconciliation failed", "kind", resource.ResourceKind(), "id", resource.GetMetadata().ID, "revision", resource.GetMetadata().Revision, "error", err)
	}
	latest, err := s.store.Get(ctx, resource.ResourceKind(), resource.GetMetadata().ID)
	if err == nil {
		return latest
	}
	return resource
}

type resolvedPort struct {
	PortID           string                  `json:"port_id"`
	LSPName          string                  `json:"lsp_name"`
	MACAddress       string                  `json:"mac_address"`
	Generation       int64                   `json:"generation"`
	RequestedChassis string                  `json:"requested_chassis"`
	Status           model.PortBindingStatus `json:"status"`
}

func (s *Server) resolvePort(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	nodeName := request.URL.Query().Get("node")
	rawVMID := request.URL.Query().Get("vmid")
	nic := request.URL.Query().Get("nic")
	vmid, err := strconv.Atoi(rawVMID)
	if nodeName == "" || err != nil || vmid < 1 || nic == "" {
		writeError(writer, http.StatusBadRequest, "invalid_request", "node, positive vmid, and nic are required", nil)
		return
	}
	acceptedNodes := map[string]bool{nodeName: true}
	nodes, listNodeErr := s.store.List(request.Context(), model.KindNode, controlstore.ListOptions{})
	if listNodeErr == nil {
		for _, resource := range nodes {
			node := resource.(*model.Node)
			if node.Name == nodeName || node.ID == nodeName || node.ChassisID == nodeName {
				acceptedNodes[node.ID], acceptedNodes[node.Name], acceptedNodes[node.ChassisID] = true, true, true
			}
		}
	}
	resources, err := s.store.List(request.Context(), model.KindPort, controlstore.ListOptions{VMID: vmid, NIC: nic})
	if err != nil {
		s.storeError(writer, err)
		return
	}
	matches := make([]*model.Port, 0, 1)
	for _, resource := range resources {
		port := resource.(*model.Port)
		if acceptedNodes[port.NodeID] || acceptedNodes[port.RequestedChassis] {
			matches = append(matches, port)
		}
	}
	if len(matches) == 0 {
		writeError(writer, http.StatusNotFound, "port_not_found", "no PVN port matches this VM NIC", nil)
		return
	}
	if len(matches) > 1 {
		writeError(writer, http.StatusConflict, "ambiguous_port", "more than one PVN port matches this VM NIC", nil)
		return
	}
	port := matches[0]
	if err := s.authorizeRead(request.Context(), port); err != nil {
		writeError(writer, http.StatusNotFound, "port_not_found", "no PVN port matches this VM NIC", nil)
		return
	}
	if port.LSPName == "" || port.Generation < 1 {
		writeError(writer, http.StatusConflict, "port_not_bindable", "the matching PVN port has incomplete runtime identity", map[string]any{"status": port.BindingStatus})
		return
	}
	if (port.BindingStatus == model.PortBinding || port.BindingStatus == model.PortBound) &&
		(!port.AdminStateUp || port.MACAddress == "" || port.RequestedChassis == "") {
		writeError(writer, http.StatusConflict, "port_not_bindable", "the matching PVN port is not bindable", map[string]any{"status": port.BindingStatus})
		return
	}
	if (port.BindingStatus == model.PortBinding || port.BindingStatus == model.PortBound) &&
		(port.State != model.ResourceReady || port.AppliedRevision != port.Revision) {
		writeError(writer, http.StatusConflict, "port_not_bindable", "the matching PVN port has not been realized in OVN", map[string]any{
			"status": port.BindingStatus, "state": port.State, "revision": port.Revision, "applied_revision": port.AppliedRevision,
		})
		return
	}
	if port.BindingStatus != model.PortBinding && port.BindingStatus != model.PortBound &&
		port.BindingStatus != model.PortDetaching && port.BindingStatus != model.PortUnbound &&
		port.BindingStatus != model.PortBindingError {
		writeError(writer, http.StatusConflict, "port_not_bindable", "the matching PVN port has an unknown binding status", map[string]any{"status": port.BindingStatus})
		return
	}
	writeJSON(writer, http.StatusOK, resolvedPort{PortID: port.ID, LSPName: port.LSPName, MACAddress: port.MACAddress, Generation: port.Generation, RequestedChassis: port.RequestedChassis, Status: port.BindingStatus})
}

func decodeResource(writer http.ResponseWriter, request *http.Request, kind model.Kind) (model.Resource, error) {
	resource, err := model.New(kind)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return nil, err
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(resource); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_json", jsonError(err), nil)
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("request body must contain one JSON object")
		}
		writeError(writer, http.StatusBadRequest, "invalid_json", jsonError(err), nil)
		return nil, err
	}
	return resource, nil
}

func expectedRevision(request *http.Request, bodyRevision int64) (int64, error) {
	value := strings.TrimSpace(request.Header.Get("If-Match"))
	if value == "" {
		if bodyRevision > 0 {
			return bodyRevision, nil
		}
		return 0, errors.New("If-Match or a positive body revision is required")
	}
	if strings.HasPrefix(value, "W/") {
		value = strings.TrimPrefix(value, "W/")
	}
	value = strings.Trim(value, `"`)
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 1 {
		return 0, errors.New("If-Match must contain a positive revision")
	}
	return revision, nil
}

func idempotencyKey(writer http.ResponseWriter, request *http.Request) (string, bool) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" {
		writeError(writer, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required", nil)
		return "", false
	}
	if len(key) > 200 {
		writeError(writer, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must not exceed 200 characters", nil)
		return "", false
	}
	return key, true
}

func setETag(writer http.ResponseWriter, revision int64) {
	writer.Header().Set("ETag", fmt.Sprintf(`"%d"`, revision))
}

func (s *Server) storeError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, controlstore.ErrNotFound):
		writeError(writer, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, controlstore.ErrAlreadyExists):
		writeError(writer, http.StatusConflict, "already_exists", err.Error(), nil)
	case errors.Is(err, controlstore.ErrPrecondition):
		writeError(writer, http.StatusPreconditionFailed, "revision_conflict", err.Error(), nil)
	case errors.Is(err, controlstore.ErrIdempotencyConflict):
		writeError(writer, http.StatusConflict, "idempotency_conflict", err.Error(), nil)
	case errors.Is(err, controlstore.ErrConflict):
		writeError(writer, http.StatusConflict, "conflict", err.Error(), nil)
	default:
		var validation *model.ValidationError
		if errors.As(err, &validation) {
			writeError(writer, http.StatusUnprocessableEntity, "validation_error", validation.Error(), validation)
			return
		}
		s.logger.Error("control store request failed", "error", err)
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error", nil)
	}
}

func methodNotAllowed(writer http.ResponseWriter, methods ...string) {
	writer.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed", nil)
}

func writeError(writer http.ResponseWriter, status int, code, message string, details any) {
	errorBody := map[string]any{"code": code, "message": message}
	if details != nil {
		errorBody["details"] = details
	}
	writeJSON(writer, status, map[string]any{"error": errorBody})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func jsonError(err error) string {
	var syntax *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("malformed JSON at byte %d", syntax.Offset)
	case errors.As(err, &typeError):
		return fmt.Sprintf("invalid value for field %q", typeError.Field)
	case errors.Is(err, io.EOF):
		return "request body is empty"
	default:
		return err.Error()
	}
}
