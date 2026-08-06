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
	"sync"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/buildinfo"
	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	maxRequestBody         = 1 << 20
	defaultOperationsLimit = 100
	maximumOperationsLimit = 500
	defaultHealthTimeout   = 5 * time.Second
)

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

// PoolValidator verifies project pool references against the Proxmox API.
// The incoming request is provided so production implementations can forward
// the already authenticated PVE browser ticket without introducing a second
// credential store.
type PoolValidator interface {
	PoolExists(context.Context, *http.Request, string) (bool, error)
}

type PoolValidatorFunc func(context.Context, *http.Request, string) (bool, error)

func (f PoolValidatorFunc) PoolExists(ctx context.Context, request *http.Request, poolID string) (bool, error) {
	return f(ctx, request, poolID)
}

type Reconciler interface {
	Reconcile(context.Context, model.Kind, string) error
}

// HealthProber performs a read-only liveness check of one manager dependency.
// Implementations must honor context cancellation and must not repair or
// otherwise mutate the dependency while serving the health endpoint.
type HealthProber interface {
	Probe(context.Context) error
}

type HealthProbeFunc func(context.Context) error

func (probe HealthProbeFunc) Probe(ctx context.Context) error { return probe(ctx) }

type DeletionReconciler interface {
	Delete(context.Context, model.Resource) error
}

type Options struct {
	Store            controlstore.Store
	Reconciler       Reconciler
	SessionProvider  SessionProvider
	PoolValidator    PoolValidator
	Logger           *slog.Logger
	RequireAllNodes  bool
	NodeHeartbeatTTL time.Duration
	Clock            func() time.Time
	GuestMTU         int
	Physnet          string
	ClusterName      string
	NorthboundProbe  HealthProber
	SouthboundProbe  HealthProber
	ReconcilerProbe  HealthProber
	HealthTimeout    time.Duration
}

type Server struct {
	store           controlstore.Store
	reconciler      Reconciler
	defaultSecurity *defaultsecurity.Manager
	sessionProvider SessionProvider
	poolValidator   PoolValidator
	logger          *slog.Logger
	clusterGate     *clusterCapacityGate
	guestMTU        int
	physnet         string
	clusterName     string
	northboundProbe HealthProber
	southboundProbe HealthProber
	reconcilerProbe HealthProber
	healthTimeout   time.Duration
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
	if options.GuestMTU == 0 {
		options.GuestMTU = 1400
	}
	if options.GuestMTU < 576 || options.GuestMTU > 9000 {
		return nil, errors.New("API guest MTU must be between 576 and 9000")
	}
	if options.HealthTimeout <= 0 {
		options.HealthTimeout = defaultHealthTimeout
	}
	return &Server{
		store: options.Store, reconciler: options.Reconciler, defaultSecurity: defaultsecurity.New(options.Store, options.Reconciler), sessionProvider: options.SessionProvider, poolValidator: options.PoolValidator,
		logger: options.Logger, clusterGate: newClusterCapacityGate(options.RequireAllNodes, options.NodeHeartbeatTTL, options.Clock),
		guestMTU: options.GuestMTU, physnet: strings.TrimSpace(options.Physnet), clusterName: strings.TrimSpace(options.ClusterName),
		northboundProbe: options.NorthboundProbe, southboundProbe: options.SouthboundProbe,
		reconcilerProbe: options.ReconcilerProbe, healthTimeout: options.HealthTimeout,
	}, nil
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
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
	if request.URL.Path == "/api/v1/health" {
		s.health(writer, request)
		return
	}
	if request.URL.Path == defaultSecurityGroupBackfillPlanPath {
		s.defaultSecurityGroupBackfillPlan(writer, request)
		return
	}
	if request.URL.Path == defaultSecurityGroupBackfillApplyPath {
		s.defaultSecurityGroupBackfillApply(writer, request)
		return
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
	capacity := s.clusterGate.status(request.Context(), s.store)
	components := s.componentHealth(request.Context())
	status := capacity.Label()
	for _, component := range components {
		if component != "ready" {
			status = "degraded"
			break
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]any{
		"status": status, "time": s.clusterGate.now().UTC(), "cluster": s.clusterName,
		"version": buildinfo.Version, "capacity": capacity,
		"database": components["database"], "ovn_northbound": components["ovn_northbound"],
		"ovn_southbound": components["ovn_southbound"], "reconciler": components["reconciler"],
		"default_security_policy": components["default_security_policy"],
	}})
}

func (s *Server) componentHealth(parent context.Context) map[string]string {
	ctx, cancel := context.WithTimeout(parent, s.healthTimeout)
	defer cancel()

	type componentProbe struct {
		name  string
		probe HealthProber
	}
	probes := []componentProbe{
		{name: "database", probe: HealthProbeFunc(func(ctx context.Context) error {
			_, err := s.store.List(ctx, model.KindOperation, controlstore.ListOptions{Limit: 1})
			return err
		})},
		{name: "ovn_northbound", probe: s.northboundProbe},
		{name: "ovn_southbound", probe: s.southboundProbe},
		{name: "reconciler", probe: s.reconcilerProbe},
		{name: "default_security_policy", probe: s.defaultSecurity},
	}
	statuses := make([]string, len(probes))
	var wait sync.WaitGroup
	wait.Add(len(probes))
	for index, item := range probes {
		go func() {
			defer wait.Done()
			if item.probe == nil {
				statuses[index] = "unavailable"
				return
			}
			if err := item.probe.Probe(ctx); err != nil {
				statuses[index] = "degraded"
				return
			}
			statuses[index] = "ready"
		}()
	}
	wait.Wait()
	result := make(map[string]string, len(probes))
	for index, item := range probes {
		result[item.name] = statuses[index]
	}
	return result
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
	if kind == model.KindOperation {
		options.RecentFirst = true
		options.Limit = defaultOperationsLimit
		if values, supplied := request.URL.Query()["limit"]; supplied {
			if len(values) != 1 || values[0] == "" {
				writeError(writer, http.StatusBadRequest, "invalid_request", "limit must be supplied exactly once", nil)
				return
			}
			limit, err := strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > maximumOperationsLimit {
				writeError(writer, http.StatusBadRequest, "invalid_request", fmt.Sprintf("limit must be an integer between 1 and %d", maximumOperationsLimit), nil)
				return
			}
			options.Limit = limit
		}
	}
	resources, err := s.store.List(request.Context(), kind, options)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	visible := make([]any, 0, len(resources))
	for _, resource := range resources {
		if s.authorizeRead(request.Context(), resource) == nil {
			visible = append(visible, resourceAPIView(resource))
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
	writeJSON(writer, http.StatusOK, map[string]any{"data": resourceAPIView(resource)})
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
	if err := s.applyNetworkPolicy(resource); err != nil {
		s.storeError(writer, err)
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
	if floatingIP, ok := resource.(*model.FloatingIP); ok && floatingIP.FloatingStatus != "" {
		writeError(writer, http.StatusBadRequest, "server_managed_field", "status is server-managed", nil)
		return
	}
	if err := s.authorizeWrite(request.Context(), resource, nil); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	if !s.requireExistingProjectPool(writer, request, resource) {
		return
	}
	if kind == model.KindPort && !s.requireClusterCapacity(writer, request) {
		return
	}
	if port, ok := resource.(*model.Port); ok && !s.preparePortSecurityGroups(writer, request.Context(), port, nil) {
		return
	}
	created, replayed, err := s.store.Create(request.Context(), resource, key)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if project, ok := created.(*model.Project); ok {
		if _, ensured := s.ensureDefaultSecurityGroup(writer, request.Context(), project.ID); !ensured {
			return
		}
	}
	created = s.reconcileAndReload(request.Context(), created)
	setETag(writer, created.GetMetadata().Revision)
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
		writeJSON(writer, http.StatusOK, map[string]any{"data": resourceAPIView(created)})
		return
	}
	writer.Header().Set("Location", "/api/v1/"+kind.Collection()+"/"+created.GetMetadata().ID)
	writeJSON(writer, http.StatusCreated, map[string]any{"data": resourceAPIView(created)})
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
	if err := s.applyNetworkPolicy(resource); err != nil {
		s.storeError(writer, err)
		return
	}
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
	if defaultsecurity.IsReserved(current) || defaultsecurity.IsReserved(resource) {
		writeError(writer, http.StatusConflict, "reserved_default_security_policy", "PVN managed default security policy resources cannot be updated", nil)
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
		if floatingIP, ok := resource.(*model.FloatingIP); ok {
			oldFloatingIP := current.(*model.FloatingIP)
			if floatingIP.FloatingStatus != "" && floatingIP.FloatingStatus != oldFloatingIP.FloatingStatus {
				writeError(writer, http.StatusBadRequest, "server_managed_field", "status is server-managed", nil)
				return
			}
		}
	}
	if !s.requireExistingProjectPool(writer, request, resource) {
		return
	}
	if port, ok := resource.(*model.Port); ok && len(port.SecurityGroupIDs) == 0 {
		currentPort := current.(*model.Port)
		if expected == current.GetMetadata().Revision {
			if !s.preparePortSecurityGroups(writer, request.Context(), port, currentPort) {
				return
			}
		} else if len(currentPort.SecurityGroupIDs) != 0 {
			// Canonicalize stale/replayed bodies without performing repair side
			// effects. Store.Update will decide replay versus precondition using
			// the same fingerprint recorded by the original successful request.
			port.SecurityGroupIDs = []string{defaultsecurity.DefaultSecurityGroupID(port.ProjectID)}
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
	writeJSON(writer, http.StatusOK, map[string]any{"data": resourceAPIView(updated)})
}

func (s *Server) requireExistingProjectPool(writer http.ResponseWriter, request *http.Request, resource model.Resource) bool {
	project, ok := resource.(*model.Project)
	if !ok || s.poolValidator == nil {
		return true
	}
	exists, err := s.poolValidator.PoolExists(request.Context(), request, project.PoolID)
	if err != nil {
		s.logger.Error("Proxmox pool validation failed", "pool_id", project.PoolID, "error", err)
		writeError(writer, http.StatusBadGateway, "pve_pool_validation_failed", "failed to verify the Proxmox pool", nil)
		return false
	}
	if !exists {
		s.storeError(writer, &model.ValidationError{Field: "pool_id", Message: fmt.Sprintf("Proxmox pool %q does not exist", project.PoolID)})
		return false
	}
	return true
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
		if defaultsecurity.IsReserved(current) {
			writeError(writer, http.StatusConflict, "reserved_default_security_policy", "PVN managed default security policy resources cannot be deleted", nil)
			return
		}
		if kind == model.KindProject {
			if current.GetMetadata().State != model.ResourceDeleting && expected != current.GetMetadata().Revision {
				s.storeError(writer, &controlstore.Error{Kind: controlstore.ErrPrecondition, Message: fmt.Sprintf("expected revision %d but current revision is %d", expected, current.GetMetadata().Revision)})
				return
			}
			if err := s.cleanupProjectDefaultSecurity(request.Context(), id); err != nil {
				s.writeDefaultSecurityError(writer, err)
				return
			}
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
	matches, err := s.lookupRuntimePorts(request.Context(), nodeName, vmid, nic)
	if err != nil {
		s.storeError(writer, err)
		return
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

func (s *Server) lookupRuntimePorts(ctx context.Context, nodeName string, vmid int, nic string) ([]*model.Port, error) {
	if lookup, ok := s.store.(controlstore.RuntimePortLookup); ok {
		return lookup.LookupRuntimePorts(ctx, nodeName, vmid, nic)
	}
	acceptedNodes := map[string]bool{nodeName: true}
	nodes, listNodeErr := s.store.List(ctx, model.KindNode, controlstore.ListOptions{})
	if listNodeErr == nil {
		for _, resource := range nodes {
			node := resource.(*model.Node)
			if node.Name == nodeName || node.ID == nodeName || node.ChassisID == nodeName {
				acceptedNodes[node.ID], acceptedNodes[node.Name], acceptedNodes[node.ChassisID] = true, true, true
			}
		}
	}
	resources, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{VMID: vmid, NIC: nic})
	if err != nil {
		return nil, err
	}
	matches := make([]*model.Port, 0, 1)
	for _, resource := range resources {
		port := resource.(*model.Port)
		if acceptedNodes[port.NodeID] || acceptedNodes[port.RequestedChassis] {
			matches = append(matches, port)
		}
	}
	return matches, nil
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
