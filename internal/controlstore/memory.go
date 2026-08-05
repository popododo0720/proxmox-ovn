package controlstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/model"
)

type idempotencyRecord struct {
	fingerprint [32]byte
	resource    model.Resource
	deleted     bool
}

type Memory struct {
	mu          sync.RWMutex
	resources   map[model.Kind]map[string]model.Resource
	idempotency map[string]idempotencyRecord
	now         func() time.Time
	newID       func() string
}

const (
	maxOperationPruneBatch          = 256
	maxExpiredOperationRecoverBatch = 256
)

type MemoryOption func(*Memory)

func WithClock(clock func() time.Time) MemoryOption {
	return func(store *Memory) { store.now = clock }
}

func WithIDGenerator(generator func() string) MemoryOption {
	return func(store *Memory) { store.newID = generator }
}

func NewMemory(options ...MemoryOption) *Memory {
	store := &Memory{
		resources:   make(map[model.Kind]map[string]model.Resource),
		idempotency: make(map[string]idempotencyRecord),
		now:         time.Now,
		newID:       randomID,
	}
	for _, option := range options {
		option(store)
	}
	for _, kind := range model.Kinds() {
		store.resources[kind] = make(map[string]model.Resource)
	}
	return store
}

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		value[6] = (value[6] & 0x0f) | 0x40
		value[8] = (value[8] & 0x3f) | 0x80
		encoded := hex.EncodeToString(value[:])
		return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
	}
	// rand.Read failure is vanishingly unlikely; the timestamp keeps the store usable.
	return fmt.Sprintf("pvn-%d", time.Now().UnixNano())
}

func (s *Memory) Create(ctx context.Context, resource model.Resource, key string) (model.Resource, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if resource == nil || !resource.ResourceKind().Valid() {
		return nil, false, storeError(ErrConflict, "invalid resource kind")
	}
	fingerprint, err := fingerprint(resource, "")
	if err != nil {
		return nil, false, err
	}
	scope := idempotencyScope("create", resource.ResourceKind(), "", key)

	s.mu.Lock()
	defer s.mu.Unlock()
	if replay, ok, replayErr := s.replayLocked(scope, fingerprint); ok || replayErr != nil {
		return replay, ok, replayErr
	}

	copyResource, err := model.Clone(resource)
	if err != nil {
		return nil, false, err
	}
	if operation, ok := copyResource.(*model.Operation); ok && operation.IdempotencyKey == "" {
		operation.IdempotencyKey = key
	}
	if operation, ok := copyResource.(*model.Operation); ok && leaseProtectedAction(operation.Action) && operation.OperationStatus != "" && operation.OperationStatus != model.OperationQueued {
		return nil, false, storeError(ErrConflict, "%s operations must be created in queued state", operation.Action)
	}
	meta := copyResource.GetMetadata()
	if meta.ID == "" {
		meta.ID = s.newID()
	}
	if _, exists := s.resources[copyResource.ResourceKind()][meta.ID]; exists {
		return nil, false, storeError(ErrAlreadyExists, "%s %q already exists", copyResource.ResourceKind(), meta.ID)
	}
	model.SetDefaults(copyResource)
	if floatingIP, ok := copyResource.(*model.FloatingIP); ok {
		floatingIP.MarkPending()
	}
	if err := copyResource.Validate(); err != nil {
		return nil, false, err
	}
	if err := s.validateReferencesLocked(copyResource); err != nil {
		return nil, false, err
	}
	if err := s.validateUniqueLocked(copyResource, ""); err != nil {
		return nil, false, err
	}
	now := s.now().UTC()
	meta.Revision = 1
	meta.AppliedRevision = 0
	meta.State = model.ResourcePending
	meta.LastError = ""
	meta.CreatedAt = now
	meta.UpdatedAt = now
	if port, ok := copyResource.(*model.Port); ok && port.LSPName == "" {
		port.LSPName = "pvn-" + port.ID
	}
	s.resources[copyResource.ResourceKind()][meta.ID] = copyResource
	result, err := model.Clone(copyResource)
	if err != nil {
		return nil, false, err
	}
	s.rememberLocked(scope, fingerprint, result, false)
	return result, false, nil
}

func (s *Memory) Get(ctx context.Context, kind model.Kind, id string) (model.Resource, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	resource, ok := s.resources[kind][id]
	if !ok {
		return nil, storeError(ErrNotFound, "%s %q was not found", kind, id)
	}
	return model.Clone(resource)
}

func (s *Memory) List(ctx context.Context, kind model.Kind, options ListOptions) ([]model.Resource, error) {
	snapshot, err := s.Snapshot(ctx, []model.Kind{kind}, options)
	if err != nil {
		return nil, err
	}
	return snapshot[kind], nil
}

func (s *Memory) Snapshot(ctx context.Context, kinds []model.Kind, options ListOptions) (ResourceSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	requested := make([]model.Kind, 0, len(kinds))
	seen := make(map[model.Kind]struct{}, len(kinds))
	for _, kind := range kinds {
		if !kind.Valid() {
			return nil, storeError(ErrNotFound, "unknown resource kind %q", kind)
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}
		requested = append(requested, kind)
	}
	if options.Limit < 0 {
		return nil, storeError(ErrConflict, "list limit cannot be negative")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(ResourceSnapshot, len(requested))
	for _, kind := range requested {
		resources := make([]model.Resource, 0, len(s.resources[kind]))
		for _, resource := range s.resources[kind] {
			if matches(resource, options) {
				resources = append(resources, resource)
			}
		}
		sort.Slice(resources, func(i, j int) bool {
			left, right := resources[i].GetMetadata(), resources[j].GetMetadata()
			if options.RecentFirst && !left.CreatedAt.Equal(right.CreatedAt) {
				return left.CreatedAt.After(right.CreatedAt)
			}
			return left.ID < right.ID
		})
		if options.Limit > 0 && len(resources) > options.Limit {
			resources = resources[:options.Limit]
		}
		clones := make([]model.Resource, 0, len(resources))
		for _, resource := range resources {
			copyResource, err := model.Clone(resource)
			if err != nil {
				return nil, err
			}
			clones = append(clones, copyResource)
		}
		result[kind] = clones
	}
	return result, nil
}

func (s *Memory) ObserveNodeHeartbeat(ctx context.Context, id string, expectedRevision int64, observedAt time.Time) (*model.Node, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if id == "" || observedAt.IsZero() {
		return nil, storeError(ErrConflict, "node id and heartbeat observation time are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, exists := s.resources[model.KindNode][id]
	if !exists {
		return nil, storeError(ErrNotFound, "node %q was not found", id)
	}
	stored := resource.(*model.Node)
	if expectedRevision < 1 || stored.Revision != expectedRevision {
		return nil, storeError(ErrPrecondition, "expected revision %d but current revision is %d", expectedRevision, stored.Revision)
	}
	if stored.LastSeenAt != nil && !stored.LastSeenAt.Before(observedAt) {
		result, err := model.Clone(stored)
		if err != nil {
			return nil, err
		}
		return result.(*model.Node), nil
	}
	copyResource, err := model.Clone(stored)
	if err != nil {
		return nil, err
	}
	observed := copyResource.(*model.Node)
	timestamp := observedAt.UTC()
	observed.LastSeenAt = &timestamp
	// LastSeenAt is observed runtime state. In particular, do not advance the
	// desired revision, reset an error, or make a pending node ready here.
	s.resources[model.KindNode][id] = observed
	result, err := model.Clone(observed)
	if err != nil {
		return nil, err
	}
	return result.(*model.Node), nil
}

func (s *Memory) PruneOperations(ctx context.Context, before time.Time, keep int) (int, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	if before.IsZero() || keep < 0 {
		return 0, storeError(ErrConflict, "operation retention cutoff and non-negative keep count are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates := terminalReconcileOperations(s.resources[model.KindOperation])
	if len(candidates) <= keep {
		return 0, nil
	}
	pruned := 0
	for _, operation := range candidates[keep:] {
		if pruned == maxOperationPruneBatch {
			break
		}
		if operation.CompletedAt == nil || !operation.CompletedAt.Before(before) || !s.operationSupersededLocked(operation) {
			continue
		}
		scope := idempotencyScope("create", model.KindOperation, "", operation.IdempotencyKey)
		record, exists := s.idempotency[scope]
		if !exists || record.resource.GetMetadata().ID != operation.ID {
			// The operation and its replay token are one retention unit. Leaving
			// either half behind would break deterministic reconcile replay.
			continue
		}
		delete(s.resources[model.KindOperation], operation.ID)
		delete(s.idempotency, scope)
		pruned++
	}
	return pruned, nil
}

// RecoverExpiredOperations turns abandoned durable writer claims into
// terminal failures. The store lock makes selection and updates atomic with
// lease heartbeats, claims, target updates, and purges.
func (s *Memory) RecoverExpiredOperations(ctx context.Context, leaseCutoff, recoveredAt time.Time, limit int) (int, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	if leaseCutoff.IsZero() || recoveredAt.IsZero() || limit < 1 {
		return 0, storeError(ErrConflict, "lease cutoff, recovery time, and positive limit are required")
	}
	if limit > maxExpiredOperationRecoverBatch {
		limit = maxExpiredOperationRecoverBatch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates := expiredRunningOperations(s.resources[model.KindOperation], leaseCutoff)
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	completed := recoveredAt.UTC()
	recoveredOperations := make([]*model.Operation, 0, len(candidates))
	for _, operation := range candidates {
		copyResource, err := model.Clone(operation)
		if err != nil {
			return 0, err
		}
		recovered := copyResource.(*model.Operation)
		recovered.OperationStatus = model.OperationFailed
		recovered.Error = "operation lease expired before completion"
		recovered.CompletedAt = &completed
		recovered.Revision++
		recovered.State = model.ResourcePending
		recovered.LastError = ""
		recovered.UpdatedAt = completed
		recoveredOperations = append(recoveredOperations, recovered)
	}
	for _, recovered := range recoveredOperations {
		s.resources[model.KindOperation][recovered.ID] = recovered
	}
	return len(candidates), nil
}

func expiredRunningOperations(resources map[string]model.Resource, leaseCutoff time.Time) []*model.Operation {
	operations := make([]*model.Operation, 0)
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if !leaseProtectedAction(operation.Action) || operation.OperationStatus != model.OperationRunning || reconcileLeaseIsLive(operation, leaseCutoff) {
			continue
		}
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(i, j int) bool {
		left, right := operations[i], operations[j]
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.Before(right.UpdatedAt)
		}
		return left.ID < right.ID
	})
	return operations
}

func terminalReconcileOperations(resources map[string]model.Resource) []*model.Operation {
	operations := make([]*model.Operation, 0, len(resources))
	for _, resource := range resources {
		operation := resource.(*model.Operation)
		if operation.Action != "reconcile" || operation.CompletedAt == nil || (operation.OperationStatus != model.OperationSucceeded && operation.OperationStatus != model.OperationFailed) {
			continue
		}
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(i, j int) bool {
		left, right := operations[i], operations[j]
		if !left.CompletedAt.Equal(*right.CompletedAt) {
			return left.CompletedAt.After(*right.CompletedAt)
		}
		return left.ID < right.ID
	})
	return operations
}

func (s *Memory) operationSupersededLocked(operation *model.Operation) bool {
	target, exists := s.resources[operation.TargetKind][operation.TargetID]
	return !exists || target.GetMetadata().Revision > operation.TargetRevision
}

func (s *Memory) Update(ctx context.Context, resource model.Resource, expectedRevision int64, key string) (model.Resource, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if resource == nil || resource.GetMetadata().ID == "" {
		return nil, false, storeError(ErrConflict, "resource id is required")
	}
	fingerprint, err := fingerprint(resource, fmt.Sprintf("expected=%d", expectedRevision))
	if err != nil {
		return nil, false, err
	}
	id := resource.GetMetadata().ID
	scope := idempotencyScope("update", resource.ResourceKind(), id, key)

	s.mu.Lock()
	defer s.mu.Unlock()
	if replay, ok, replayErr := s.replayLocked(scope, fingerprint); ok || replayErr != nil {
		return replay, ok, replayErr
	}
	current, exists := s.resources[resource.ResourceKind()][id]
	if !exists {
		return nil, false, storeError(ErrNotFound, "%s %q was not found", resource.ResourceKind(), id)
	}
	if expectedRevision < 1 || current.GetMetadata().Revision != expectedRevision {
		return nil, false, storeError(ErrPrecondition, "expected revision %d but current revision is %d", expectedRevision, current.GetMetadata().Revision)
	}
	copyResource, err := model.Clone(resource)
	if err != nil {
		return nil, false, err
	}
	model.SetDefaults(copyResource)
	if floatingIP, ok := copyResource.(*model.FloatingIP); ok {
		floatingIP.MarkPending()
	}
	if requestedOperation, ok := copyResource.(*model.Operation); ok {
		storedOperation := current.(*model.Operation)
		if leaseProtectedAction(storedOperation.Action) && storedOperation.OperationStatus == model.OperationRunning && requestedOperation.LeaseOwner != storedOperation.LeaseOwner {
			return nil, false, storeError(ErrConflict, "operation %q lease is owned by another manager", storedOperation.ID)
		}
	}
	if operation, ok := copyResource.(*model.Operation); ok && leaseProtectedAction(operation.Action) && operation.OperationStatus == model.OperationRunning {
		return nil, false, storeError(ErrConflict, "%s operations must be started with a durable lease claim", operation.Action)
	}
	if err := copyResource.Validate(); err != nil {
		return nil, false, err
	}
	if err := s.validateReferencesLocked(copyResource); err != nil {
		return nil, false, err
	}
	if err := s.validateUniqueLocked(copyResource, id); err != nil {
		return nil, false, err
	}
	// Parent fields can invalidate resources that already point at this row.
	// Validate the whole prospective graph before making the replacement
	// visible, matching the transactional PVN_Control store behavior.
	s.resources[copyResource.ResourceKind()][id] = copyResource
	graphErr := s.validateAllReferencesLocked()
	if graphErr == nil {
		graphErr = s.validateAllUniqueLocked()
	}
	s.resources[copyResource.ResourceKind()][id] = current
	if graphErr != nil {
		return nil, false, graphErr
	}
	meta := copyResource.GetMetadata()
	currentMeta := current.GetMetadata()
	meta.ID = id
	meta.Revision = currentMeta.Revision + 1
	meta.AppliedRevision = currentMeta.AppliedRevision
	meta.State = model.ResourcePending
	meta.LastError = ""
	meta.CreatedAt = currentMeta.CreatedAt
	meta.UpdatedAt = s.now().UTC()
	s.resources[copyResource.ResourceKind()][id] = copyResource
	result, err := model.Clone(copyResource)
	if err != nil {
		return nil, false, err
	}
	s.rememberLocked(scope, fingerprint, result, false)
	return result, false, nil
}

// ClaimReconcile is the only supported transition to a running reconcile.
func (s *Memory) ClaimReconcile(ctx context.Context, operationID string, expectedRevision int64, leaseOwner string, startedAt, leaseCutoff time.Time) (*model.Operation, error) {
	return s.claimOperation(ctx, operationID, expectedRevision, "reconcile", leaseOwner, startedAt, leaseCutoff)
}

// ClaimDelete is the only supported transition to a running realized-state delete.
func (s *Memory) ClaimDelete(ctx context.Context, operationID string, expectedRevision int64, leaseOwner string, startedAt, leaseCutoff time.Time) (*model.Operation, error) {
	return s.claimOperation(ctx, operationID, expectedRevision, "delete", leaseOwner, startedAt, leaseCutoff)
}

func (s *Memory) claimOperation(ctx context.Context, operationID string, expectedRevision int64, action, leaseOwner string, startedAt, leaseCutoff time.Time) (*model.Operation, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if !validLeaseOwner(leaseOwner) {
		return nil, storeError(ErrConflict, "lease owner is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	storedResource, exists := s.resources[model.KindOperation][operationID]
	if !exists {
		return nil, storeError(ErrNotFound, "operation %q was not found", operationID)
	}
	stored := storedResource.(*model.Operation)
	if expectedRevision < 1 || stored.Revision != expectedRevision {
		return nil, storeError(ErrPrecondition, "expected operation revision %d but current revision is %d", expectedRevision, stored.Revision)
	}
	if stored.Action != action {
		return nil, storeError(ErrConflict, "operation %q is not a %s operation", operationID, action)
	}
	if stored.OperationStatus == model.OperationRunning && reconcileLeaseIsLive(stored, leaseCutoff) {
		return nil, storeError(ErrConflict, "%s operation %q still holds a live lease", action, operationID)
	}
	target, exists := s.resources[stored.TargetKind][stored.TargetID]
	if !exists {
		return nil, storeError(ErrPrecondition, "%s %q no longer exists", stored.TargetKind, stored.TargetID)
	}
	targetMeta := target.GetMetadata()
	if !operationTargetClaimable(stored, targetMeta, true) {
		return nil, storeError(ErrPrecondition, "%s target %s %q is not at claimable revision %d", action, stored.TargetKind, stored.TargetID, stored.TargetRevision)
	}
	claimedResource, err := model.Clone(stored)
	if err != nil {
		return nil, err
	}
	claimed := claimedResource.(*model.Operation)
	claimed.OperationStatus = model.OperationRunning
	claimed.LeaseOwner = leaseOwner
	started := startedAt.UTC()
	claimed.StartedAt = &started
	claimed.CompletedAt = nil
	claimed.Error = ""
	claimed.Revision++
	claimed.State = model.ResourcePending
	claimed.LastError = ""
	claimed.UpdatedAt = started
	s.resources[model.KindOperation][operationID] = claimed
	result, err := model.Clone(claimed)
	if err != nil {
		return nil, err
	}
	return result.(*model.Operation), nil
}

// RenewOperationLease uses both the opaque owner and operation revision as a
// CAS token. A manager that loses either can no longer keep a writer live.
func (s *Memory) RenewOperationLease(ctx context.Context, operationID string, expectedRevision int64, leaseOwner string, renewedAt time.Time) (*model.Operation, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, exists := s.resources[model.KindOperation][operationID]
	if !exists {
		return nil, storeError(ErrNotFound, "operation %q was not found", operationID)
	}
	stored := resource.(*model.Operation)
	if expectedRevision < 1 || stored.Revision != expectedRevision {
		return nil, storeError(ErrPrecondition, "expected operation revision %d but current revision is %d", expectedRevision, stored.Revision)
	}
	if !leaseProtectedAction(stored.Action) || stored.OperationStatus != model.OperationRunning || leaseOwner == "" || stored.LeaseOwner != leaseOwner {
		return nil, storeError(ErrConflict, "operation %q lease is not owned by this manager", operationID)
	}
	target, exists := s.resources[stored.TargetKind][stored.TargetID]
	if !exists || !operationTargetClaimable(stored, target.GetMetadata(), false) {
		return nil, storeError(ErrPrecondition, "%s target %s %q is no longer writable", stored.Action, stored.TargetKind, stored.TargetID)
	}
	copyResource, err := model.Clone(stored)
	if err != nil {
		return nil, err
	}
	renewed := copyResource.(*model.Operation)
	timestamp := renewedAt.UTC()
	renewed.Revision++
	renewed.UpdatedAt = timestamp
	s.resources[model.KindOperation][operationID] = renewed
	result, err := model.Clone(renewed)
	if err != nil {
		return nil, err
	}
	return result.(*model.Operation), nil
}

func operationTargetClaimable(operation *model.Operation, target *model.Metadata, exact bool) bool {
	if operation == nil || target == nil {
		return false
	}
	switch operation.Action {
	case "reconcile":
		return !exact || (target.State != model.ResourceDeleting && target.Revision == operation.TargetRevision)
	case "delete":
		return target.Revision == operation.TargetRevision && (target.State == model.ResourceDeleting || target.State == model.ResourceError)
	default:
		return false
	}
}

func leaseProtectedAction(action string) bool { return action == "reconcile" || action == "delete" }

func validLeaseOwner(value string) bool {
	if len(value) < 1 || len(value) > 127 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || (index > 0 && strings.ContainsRune("_.:-", character)) {
			continue
		}
		return false
	}
	return true
}

// FenceReconciles expires abandoned reconcile claims for a deleting target and
// reports whether a live claim still protects it. Both the inspection and all
// expired-operation updates are atomic with respect to ClaimReconcile/Purge.
func (s *Memory) FenceReconciles(ctx context.Context, kind model.Kind, id string, leaseCutoff, recoveredAt time.Time) (bool, bool, error) {
	if err := contextError(ctx); err != nil {
		return false, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active, recovered := false, false
	for operationID, resource := range s.resources[model.KindOperation] {
		operation := resource.(*model.Operation)
		if operation.Action != "reconcile" || operation.TargetKind != kind || operation.TargetID != id || operation.OperationStatus != model.OperationRunning {
			continue
		}
		if reconcileLeaseIsLive(operation, leaseCutoff) {
			active = true
			continue
		}
		copyResource, err := model.Clone(operation)
		if err != nil {
			return false, false, err
		}
		recoveredOperation := copyResource.(*model.Operation)
		recoveredOperation.OperationStatus = model.OperationFailed
		recoveredOperation.Error = "reconcile lease expired while target was deleting"
		completed := recoveredAt.UTC()
		recoveredOperation.CompletedAt = &completed
		recoveredOperation.Revision++
		recoveredOperation.State = model.ResourcePending
		recoveredOperation.LastError = ""
		recoveredOperation.UpdatedAt = completed
		s.resources[model.KindOperation][operationID] = recoveredOperation
		recovered = true
	}
	return active, recovered, nil
}

func reconcileLeaseIsLive(operation *model.Operation, leaseCutoff time.Time) bool {
	return operation != nil && operation.StartedAt != nil && operation.UpdatedAt.UTC().After(leaseCutoff.UTC())
}

func (s *Memory) Delete(ctx context.Context, kind model.Kind, id string, expectedRevision int64, key string) (bool, error) {
	tombstone, replayed, err := s.BeginDelete(ctx, kind, id, expectedRevision, key)
	if err != nil {
		return false, err
	}
	if err := s.Purge(ctx, kind, id, tombstone.GetMetadata().Revision); err != nil {
		return false, err
	}
	return replayed, nil
}

func (s *Memory) BeginDelete(ctx context.Context, kind model.Kind, id string, expectedRevision int64, key string) (model.Resource, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", id, expectedRevision)))
	scope := idempotencyScope("delete", kind, id, key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if replay, replayed, replayErr := s.replayLocked(scope, fingerprint); replayed || replayErr != nil {
		return replay, replayed, replayErr
	}
	current, exists := s.resources[kind][id]
	if !exists {
		return nil, false, storeError(ErrNotFound, "%s %q was not found", kind, id)
	}
	if expectedRevision < 1 || current.GetMetadata().Revision != expectedRevision {
		return nil, false, storeError(ErrPrecondition, "expected revision %d but current revision is %d", expectedRevision, current.GetMetadata().Revision)
	}
	if referrer := s.firstReferenceLocked(kind, id); referrer != "" {
		return nil, false, storeError(ErrConflict, "%s %q is still referenced by %s", kind, id, referrer)
	}
	tombstone, err := model.Clone(current)
	if err != nil {
		return nil, false, err
	}
	meta := tombstone.GetMetadata()
	meta.Revision++
	meta.State = model.ResourceDeleting
	meta.LastError = ""
	meta.UpdatedAt = s.now().UTC()
	if floatingIP, ok := tombstone.(*model.FloatingIP); ok {
		floatingIP.MarkPending()
	}
	s.resources[kind][id] = tombstone
	result, err := model.Clone(tombstone)
	if err != nil {
		return nil, false, err
	}
	s.rememberLocked(scope, fingerprint, result, true)
	return result, false, nil
}

func (s *Memory) Purge(ctx context.Context, kind model.Kind, id string, deletionRevision int64) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.resources[kind][id]
	if !exists {
		return nil
	}
	meta := current.GetMetadata()
	if meta.Revision != deletionRevision {
		return storeError(ErrPrecondition, "delete revision %d does not match current revision %d", deletionRevision, meta.Revision)
	}
	if meta.State != model.ResourceDeleting && meta.State != model.ResourceError {
		return storeError(ErrConflict, "%s %q is not marked for deletion", kind, id)
	}
	for _, resource := range s.resources[model.KindOperation] {
		operation := resource.(*model.Operation)
		if operation.TargetKind == kind && operation.TargetID == id && operation.OperationStatus == model.OperationRunning {
			return storeError(ErrConflict, "%s %q is protected by running reconcile operation %q", kind, id, operation.ID)
		}
	}
	delete(s.resources[kind], id)
	return nil
}

func (s *Memory) MarkReconciled(ctx context.Context, kind model.Kind, id string, revision int64, renderErr error) (model.Resource, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, exists := s.resources[kind][id]
	if !exists {
		return nil, storeError(ErrNotFound, "%s %q was not found", kind, id)
	}
	meta := resource.GetMetadata()
	if revision > meta.Revision {
		return nil, storeError(ErrPrecondition, "rendered revision %d is newer than desired revision %d", revision, meta.Revision)
	}
	if renderErr != nil {
		if revision == meta.Revision {
			meta.State = model.ResourceError
			meta.LastError = renderErr.Error()
			meta.UpdatedAt = s.now().UTC()
			if floatingIP, ok := resource.(*model.FloatingIP); ok {
				floatingIP.MarkReconciled(renderErr)
			}
		}
	} else {
		if revision > meta.AppliedRevision {
			meta.AppliedRevision = revision
		}
		if revision == meta.Revision {
			meta.State = model.ResourceReady
			meta.LastError = ""
			meta.UpdatedAt = s.now().UTC()
			if floatingIP, ok := resource.(*model.FloatingIP); ok {
				floatingIP.MarkReconciled(nil)
			}
		}
	}
	return model.Clone(resource)
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func fingerprint(resource model.Resource, qualifier string) ([32]byte, error) {
	encoded, err := json.Marshal(resource)
	if err != nil {
		return [32]byte{}, err
	}
	encoded = append(encoded, qualifier...)
	return sha256.Sum256(encoded), nil
}

func idempotencyScope(action string, kind model.Kind, id, key string) string {
	if key == "" {
		return ""
	}
	return strings.Join([]string{action, kind.String(), id, key}, "/")
}

func (s *Memory) replayLocked(scope string, fingerprint [32]byte) (model.Resource, bool, error) {
	if scope == "" {
		return nil, false, nil
	}
	record, exists := s.idempotency[scope]
	if !exists {
		return nil, false, nil
	}
	if record.fingerprint != fingerprint {
		return nil, false, storeError(ErrIdempotencyConflict, "idempotency key was already used for a different request")
	}
	if record.resource == nil {
		return nil, true, nil
	}
	resource, err := model.Clone(record.resource)
	return resource, true, err
}

func (s *Memory) rememberLocked(scope string, fingerprint [32]byte, resource model.Resource, deleted bool) {
	if scope == "" {
		return
	}
	var copyResource model.Resource
	if resource != nil {
		copyResource, _ = model.Clone(resource)
	}
	s.idempotency[scope] = idempotencyRecord{fingerprint: fingerprint, resource: copyResource, deleted: deleted}
}

func matches(resource model.Resource, options ListOptions) bool {
	projectID, networkID, nodeID, vmid, nic := resourceFields(resource)
	if options.ProjectID != "" && projectID != options.ProjectID {
		return false
	}
	if options.NetworkID != "" && networkID != options.NetworkID {
		return false
	}
	if options.NodeID != "" && nodeID != options.NodeID {
		return false
	}
	if options.VMID != 0 && vmid != options.VMID {
		return false
	}
	if options.NIC != "" && nic != options.NIC {
		return false
	}
	return true
}

func resourceFields(resource model.Resource) (projectID, networkID, nodeID string, vmid int, nic string) {
	switch value := resource.(type) {
	case *model.Network:
		projectID = value.ProjectID
	case *model.Subnet:
		projectID, networkID = value.ProjectID, value.NetworkID
	case *model.Port:
		projectID, networkID, nodeID, vmid, nic = value.ProjectID, value.NetworkID, value.NodeID, value.VMID, value.NIC
	case *model.IPAllocation:
		projectID = value.ProjectID
	case *model.Router:
		projectID = value.ProjectID
	case *model.RouterInterface:
		projectID = value.ProjectID
	case *model.FloatingIP:
		projectID = value.ProjectID
	case *model.SecurityGroup:
		projectID = value.ProjectID
	case *model.SecurityGroupRule:
		projectID = value.ProjectID
	}
	return
}

func (s *Memory) requireLocked(kind model.Kind, id, field string) (model.Resource, error) {
	resource, exists := s.resources[kind][id]
	if !exists {
		return nil, storeError(ErrConflict, "%s references missing %s %q", field, kind, id)
	}
	return resource, nil
}

func (s *Memory) validateReferencesLocked(resource model.Resource) error {
	project := func(id, field string) error { _, err := s.requireLocked(model.KindProject, id, field); return err }
	switch value := resource.(type) {
	case *model.Project, *model.Node, *model.Operation:
		return nil
	case *model.ProviderNetwork:
		if value.DefaultSegmentID == "" {
			return nil
		}
		segmentResource, err := s.requireLocked(model.KindProviderSegment, value.DefaultSegmentID, "default_segment_id")
		if err != nil {
			return err
		}
		if segmentResource.(*model.ProviderSegment).ProviderNetworkID != value.ID {
			return storeError(ErrConflict, "default segment belongs to a different provider network")
		}
	case *model.Network:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if value.ProviderNetworkID != "" {
			_, err := s.requireLocked(model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id")
			return err
		}
	case *model.Subnet:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		network, err := s.requireLocked(model.KindNetwork, value.NetworkID, "network_id")
		if err != nil {
			return err
		}
		if network.(*model.Network).ProjectID != value.ProjectID {
			return storeError(ErrConflict, "network belongs to a different project")
		}
	case *model.Port:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		network, err := s.requireLocked(model.KindNetwork, value.NetworkID, "network_id")
		if err != nil {
			return err
		}
		portNetwork := network.(*model.Network)
		if portNetwork.ProjectID != value.ProjectID {
			return storeError(ErrConflict, "network belongs to a different project")
		}
		if portNetwork.External || portNetwork.ProviderNetworkID != "" {
			return storeError(ErrConflict, "tenant ports cannot use an external or provider-backed network")
		}
		for _, fixed := range value.FixedIPs {
			subnetResource, subErr := s.requireLocked(model.KindSubnet, fixed.SubnetID, "fixed_ips.subnet_id")
			if subErr != nil {
				return subErr
			}
			subnet := subnetResource.(*model.Subnet)
			if subnet.NetworkID != value.NetworkID {
				return storeError(ErrConflict, "fixed IP subnet belongs to a different network")
			}
			if subnet.ProjectID != value.ProjectID {
				return storeError(ErrConflict, "fixed IP subnet belongs to a different project")
			}
			if fixed.Address != "" {
				if addressErr := model.ValidateIPv4AllocationAddress(subnet, fixed.Address); addressErr != nil {
					return storeError(ErrConflict, "fixed IP address is not allocatable on its subnet: %v", addressErr)
				}
			}
		}
		for _, groupID := range value.SecurityGroupIDs {
			groupResource, err := s.requireLocked(model.KindSecurityGroup, groupID, "security_group_ids")
			if err != nil {
				return err
			}
			if groupResource.(*model.SecurityGroup).ProjectID != value.ProjectID {
				return storeError(ErrConflict, "security group belongs to a different project")
			}
		}
		if value.NodeID != "" {
			nodeResource, err := s.requireLocked(model.KindNode, value.NodeID, "node_id")
			if err != nil {
				return err
			}
			if value.RequestedChassis != "" && nodeResource.(*model.Node).ChassisID != value.RequestedChassis {
				return storeError(ErrConflict, "requested chassis does not match the selected node")
			}
		} else if value.RequestedChassis != "" {
			return storeError(ErrConflict, "requested chassis requires a selected node")
		}
	case *model.IPAllocation:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		subnetResource, err := s.requireLocked(model.KindSubnet, value.SubnetID, "subnet_id")
		if err != nil {
			return err
		}
		subnet := subnetResource.(*model.Subnet)
		if subnet.ProjectID != value.ProjectID {
			return storeError(ErrConflict, "subnet belongs to a different project")
		}
		if addressErr := model.ValidateIPv4AllocationAddress(subnet, value.Address); addressErr != nil {
			return storeError(ErrConflict, "allocated address is not allocatable on its subnet: %v", addressErr)
		}
		if value.PortID != "" {
			portResource, err := s.requireLocked(model.KindPort, value.PortID, "port_id")
			if err != nil {
				return err
			}
			port := portResource.(*model.Port)
			if port.ProjectID != value.ProjectID {
				return storeError(ErrConflict, "port belongs to a different project")
			}
			if port.NetworkID != subnet.NetworkID {
				return storeError(ErrConflict, "port belongs to a different network than the allocation subnet")
			}
			if !portHasFixedIP(port, value.SubnetID, value.Address) {
				return storeError(ErrConflict, "allocated address is not assigned to the port on this subnet")
			}
		}
	case *model.Router:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if value.ExternalNetworkID != "" {
			networkResource, err := s.requireLocked(model.KindNetwork, value.ExternalNetworkID, "external_network_id")
			if err != nil {
				return err
			}
			network := networkResource.(*model.Network)
			if !network.External || network.ProviderNetworkID == "" {
				return storeError(ErrConflict, "external_network_id must reference a provider-backed external network")
			}
			providerResource, err := s.requireLocked(model.KindProviderNetwork, network.ProviderNetworkID, "external_network_id.provider_network_id")
			if err != nil {
				return err
			}
			if network.ProjectID != value.ProjectID && !providerResource.(*model.ProviderNetwork).Shared {
				return storeError(ErrConflict, "external network belongs to another project and is not shared")
			}
			subnetResource, err := s.requireLocked(model.KindSubnet, value.ExternalSubnetID, "external_subnet_id")
			if err != nil {
				return err
			}
			subnet := subnetResource.(*model.Subnet)
			if subnet.NetworkID != network.ID || subnet.ProjectID != network.ProjectID {
				return storeError(ErrConflict, "external subnet belongs to a different network")
			}
			if addressErr := model.ValidateIPv4AllocationAddress(subnet, value.ExternalIPAddress); addressErr != nil {
				return storeError(ErrConflict, "external_ip_address is not allocatable on the external subnet: %v", addressErr)
			}
		}
	case *model.RouterInterface:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		routerResource, err := s.requireLocked(model.KindRouter, value.RouterID, "router_id")
		if err != nil {
			return err
		}
		if routerResource.(*model.Router).ProjectID != value.ProjectID {
			return storeError(ErrConflict, "router belongs to a different project")
		}
		subnetResource, err := s.requireLocked(model.KindSubnet, value.SubnetID, "subnet_id")
		if err != nil {
			return err
		}
		subnet := subnetResource.(*model.Subnet)
		if subnet.ProjectID != value.ProjectID {
			return storeError(ErrConflict, "subnet belongs to a different project")
		}
		networkResource, err := s.requireLocked(model.KindNetwork, subnet.NetworkID, "subnet_id.network_id")
		if err != nil {
			return err
		}
		network := networkResource.(*model.Network)
		if network.ProjectID != value.ProjectID {
			return storeError(ErrConflict, "router interface network belongs to a different project")
		}
		if network.External {
			return storeError(ErrConflict, "router interfaces can only use internal networks")
		}
		if value.PortID != "" {
			portResource, err := s.requireLocked(model.KindPort, value.PortID, "port_id")
			if err != nil {
				return err
			}
			port := portResource.(*model.Port)
			if port.ProjectID != value.ProjectID || port.NetworkID != subnet.NetworkID {
				return storeError(ErrConflict, "router interface port belongs to a different project or network")
			}
			if !portHasSubnet(port, value.SubnetID) {
				return storeError(ErrConflict, "router interface port has no fixed IP on the interface subnet")
			}
		}
	case *model.FloatingIP:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		providerResource, err := s.requireLocked(model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id")
		if err != nil {
			return err
		}
		if !s.providerHasAllocatableAddressLocked(value.ProviderNetworkID, value.Address, "") {
			return storeError(ErrConflict, "floating IP address is not allocatable on an external subnet for its provider network")
		}
		if (value.PortID == "") != (value.FixedIPAddress == "") {
			return storeError(ErrConflict, "port and fixed IP address must be configured together")
		}
		if value.PortID != "" && value.RouterID == "" {
			return storeError(ErrConflict, "an associated floating IP requires a router")
		}
		if value.RouterID != "" {
			routerResource, err := s.requireLocked(model.KindRouter, value.RouterID, "router_id")
			if err != nil {
				return err
			}
			router := routerResource.(*model.Router)
			if router.ProjectID != value.ProjectID {
				return storeError(ErrConflict, "router belongs to a different project")
			}
			if router.ExternalNetworkID == "" || router.ExternalSubnetID == "" {
				return storeError(ErrConflict, "router has no external gateway")
			}
			externalNetworkResource, err := s.requireLocked(model.KindNetwork, router.ExternalNetworkID, "router_id.external_network_id")
			if err != nil {
				return err
			}
			externalNetwork := externalNetworkResource.(*model.Network)
			if !externalNetwork.External || externalNetwork.ProviderNetworkID != value.ProviderNetworkID {
				return storeError(ErrConflict, "floating IP provider does not match the router external network")
			}
			if externalNetwork.ProjectID != value.ProjectID && !providerResource.(*model.ProviderNetwork).Shared {
				return storeError(ErrConflict, "router external network belongs to another project and is not shared")
			}
			externalSubnetResource, err := s.requireLocked(model.KindSubnet, router.ExternalSubnetID, "router_id.external_subnet_id")
			if err != nil {
				return err
			}
			externalSubnet := externalSubnetResource.(*model.Subnet)
			if externalSubnet.NetworkID != externalNetwork.ID || externalSubnet.ProjectID != externalNetwork.ProjectID {
				return storeError(ErrConflict, "router external subnet does not belong to its external network")
			}
			if addressErr := model.ValidateIPv4AllocationAddress(externalSubnet, value.Address); addressErr != nil {
				return storeError(ErrConflict, "floating IP address is not allocatable on the router external subnet: %v", addressErr)
			}
		}
		if value.PortID != "" {
			portResource, err := s.requireLocked(model.KindPort, value.PortID, "port_id")
			if err != nil {
				return err
			}
			port := portResource.(*model.Port)
			if port.ProjectID != value.ProjectID {
				return storeError(ErrConflict, "port belongs to a different project")
			}
			if !portHasAddress(port, value.FixedIPAddress) {
				return storeError(ErrConflict, "fixed IP address is not assigned to the port")
			}
			if !s.routerReachesPortAddressLocked(value.RouterID, port, value.FixedIPAddress) {
				return storeError(ErrConflict, "router has no interface on the floating IP port subnet")
			}
		}
	case *model.ProviderSegment:
		if _, err := s.requireLocked(model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id"); err != nil {
			return err
		}
	case *model.SecurityGroup:
		return project(value.ProjectID, "project_id")
	case *model.SecurityGroupRule:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		groupResource, err := s.requireLocked(model.KindSecurityGroup, value.SecurityGroupID, "security_group_id")
		if err != nil {
			return err
		}
		if groupResource.(*model.SecurityGroup).ProjectID != value.ProjectID {
			return storeError(ErrConflict, "security group belongs to a different project")
		}
		if value.RemoteGroupID != "" {
			remoteResource, err := s.requireLocked(model.KindSecurityGroup, value.RemoteGroupID, "remote_group_id")
			if err != nil {
				return err
			}
			if remoteResource.(*model.SecurityGroup).ProjectID != value.ProjectID {
				return storeError(ErrConflict, "remote security group belongs to a different project")
			}
		}
	}
	return nil
}

func (s *Memory) providerHasAllocatableAddressLocked(providerID, address, ignoredSubnetID string) bool {
	for _, networkResource := range s.resources[model.KindNetwork] {
		network := networkResource.(*model.Network)
		if network.State == model.ResourceDeleting || !network.External || network.ProviderNetworkID != providerID {
			continue
		}
		for subnetID, subnetResource := range s.resources[model.KindSubnet] {
			if subnetID == ignoredSubnetID {
				continue
			}
			subnet := subnetResource.(*model.Subnet)
			if subnet.State != model.ResourceDeleting && subnet.NetworkID == network.ID && model.ValidateIPv4AllocationAddress(subnet, address) == nil {
				return true
			}
		}
	}
	return false
}

func portHasFixedIP(port *model.Port, subnetID, address string) bool {
	for _, fixed := range port.FixedIPs {
		if fixed.SubnetID == subnetID && fixed.Address == address {
			return true
		}
	}
	return false
}

func portHasSubnet(port *model.Port, subnetID string) bool {
	for _, fixed := range port.FixedIPs {
		if fixed.SubnetID == subnetID {
			return true
		}
	}
	return false
}

func portHasAddress(port *model.Port, address string) bool {
	for _, fixed := range port.FixedIPs {
		if fixed.Address == address {
			return true
		}
	}
	return false
}

func (s *Memory) routerReachesPortAddressLocked(routerID string, port *model.Port, address string) bool {
	for _, resource := range s.resources[model.KindRouterInterface] {
		routerInterface := resource.(*model.RouterInterface)
		if routerInterface.RouterID != routerID {
			continue
		}
		if portHasFixedIP(port, routerInterface.SubnetID, address) {
			return true
		}
	}
	return false
}

func (s *Memory) validateAllReferencesLocked() error {
	for _, kind := range model.Kinds() {
		ids := make([]string, 0, len(s.resources[kind]))
		for id := range s.resources[kind] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if err := s.validateReferencesLocked(s.resources[kind][id]); err != nil {
				return storeError(ErrConflict, "%s %q would become invalid: %v", kind, id, err)
			}
		}
	}
	return nil
}

func (s *Memory) validateUniqueLocked(candidate model.Resource, ignoredID string) error {
	for id, existing := range s.resources[candidate.ResourceKind()] {
		if id == ignoredID {
			continue
		}
		if conflictField(candidate, existing) != "" {
			return storeError(ErrAlreadyExists, "%s conflicts with existing %s %q on %s", candidate.ResourceKind(), existing.ResourceKind(), id, conflictField(candidate, existing))
		}
	}
	for _, candidateClaim := range s.externalAddressClaimsLocked(candidate) {
		for _, kind := range []model.Kind{model.KindSubnet, model.KindRouter, model.KindFloatingIP} {
			for id, existing := range s.resources[kind] {
				if kind == candidate.ResourceKind() && id == ignoredID {
					continue
				}
				for _, existingClaim := range s.externalAddressClaimsLocked(existing) {
					if candidateClaim == existingClaim {
						return storeError(ErrAlreadyExists, "%s conflicts with existing %s %q on provider network address", candidate.ResourceKind(), kind, id)
					}
				}
			}
		}
	}
	return nil
}

func (s *Memory) validateAllUniqueLocked() error {
	for _, kind := range model.Kinds() {
		for id, resource := range s.resources[kind] {
			if err := s.validateUniqueLocked(resource, id); err != nil {
				return storeError(ErrConflict, "%s %q would violate uniqueness: %v", kind, id, err)
			}
		}
	}
	return nil
}

func (s *Memory) externalAddressClaimsLocked(resource model.Resource) []string {
	providerID, address := "", ""
	switch value := resource.(type) {
	case *model.Subnet:
		if networkResource, exists := s.resources[model.KindNetwork][value.NetworkID]; exists {
			providerID = networkResource.(*model.Network).ProviderNetworkID
			if gateway, err := model.EffectiveIPv4Gateway(value); err == nil {
				address = gateway.String()
			}
		}
	case *model.Router:
		if networkResource, exists := s.resources[model.KindNetwork][value.ExternalNetworkID]; exists {
			providerID = networkResource.(*model.Network).ProviderNetworkID
			address = value.ExternalIPAddress
		}
	case *model.FloatingIP:
		providerID, address = value.ProviderNetworkID, value.Address
	}
	if providerID == "" || address == "" {
		return nil
	}
	return []string{providerID + "\x00" + address}
}

func conflictField(candidate, existing model.Resource) string {
	switch left := candidate.(type) {
	case *model.Project:
		right := existing.(*model.Project)
		if left.PoolID == right.PoolID {
			return "pool_id"
		}
		if left.Name == right.Name {
			return "name"
		}
	case *model.Network:
		right := existing.(*model.Network)
		if left.ProjectID == right.ProjectID && left.Name == right.Name {
			return "project_id,name"
		}
	case *model.Subnet:
		right := existing.(*model.Subnet)
		if left.NetworkID == right.NetworkID && model.IPv4PrefixesOverlap(left.CIDR, right.CIDR) {
			return "network_id,cidr-overlap"
		}
		if left.NetworkID == right.NetworkID && left.Name == right.Name {
			return "network_id,name"
		}
	case *model.Port:
		right := existing.(*model.Port)
		if left.MACAddress != "" && strings.EqualFold(left.MACAddress, right.MACAddress) {
			return "mac_address"
		}
		if left.LSPName != "" && left.LSPName == right.LSPName {
			return "lsp_name"
		}
		if left.NodeID != "" && left.VMID > 0 && left.NIC != "" && left.NodeID == right.NodeID && left.VMID == right.VMID && left.NIC == right.NIC {
			return "node_id,vmid,nic"
		}
		for _, leftIP := range left.FixedIPs {
			if leftIP.Address == "" {
				continue
			}
			for _, rightIP := range right.FixedIPs {
				if leftIP.SubnetID == rightIP.SubnetID && leftIP.Address == rightIP.Address {
					return "fixed_ips.subnet_id,address"
				}
			}
		}
	case *model.IPAllocation:
		right := existing.(*model.IPAllocation)
		if left.SubnetID == right.SubnetID && left.Address == right.Address {
			return "subnet_id,address"
		}
	case *model.Router:
		right := existing.(*model.Router)
		if left.ProjectID == right.ProjectID && left.Name == right.Name {
			return "project_id,name"
		}
	case *model.RouterInterface:
		right := existing.(*model.RouterInterface)
		if left.RouterID == right.RouterID && left.SubnetID == right.SubnetID {
			return "router_id,subnet_id"
		}
	case *model.FloatingIP:
		right := existing.(*model.FloatingIP)
		if left.ProviderNetworkID == right.ProviderNetworkID && left.Address == right.Address {
			return "provider_network_id,address"
		}
	case *model.ProviderNetwork:
		right := existing.(*model.ProviderNetwork)
		if left.Name == right.Name {
			return "name"
		}
	case *model.ProviderSegment:
		right := existing.(*model.ProviderSegment)
		if left.ProviderNetworkID == right.ProviderNetworkID && left.Name == right.Name {
			return "provider_network_id,name"
		}
		if left.PhysicalNetwork == right.PhysicalNetwork && left.NetworkType == right.NetworkType && left.VLANID == right.VLANID {
			return "physical_network,network_type,vlan_id"
		}
	case *model.SecurityGroup:
		right := existing.(*model.SecurityGroup)
		if left.ProjectID == right.ProjectID && left.Name == right.Name {
			return "project_id,name"
		}
	case *model.Operation:
		right := existing.(*model.Operation)
		if left.IdempotencyKey == right.IdempotencyKey {
			return "idempotency_key"
		}
		if left.TargetKind == right.TargetKind && left.TargetID == right.TargetID && left.TargetRevision == right.TargetRevision {
			return "target_kind,target_id,target_revision"
		}
	case *model.Node:
		right := existing.(*model.Node)
		if left.Name == right.Name {
			return "name"
		}
		if left.ChassisID == right.ChassisID {
			return "chassis_id"
		}
	}
	return ""
}

func (s *Memory) firstReferenceLocked(kind model.Kind, id string) string {
	for candidateKind, entries := range s.resources {
		if candidateKind == kind && len(entries) == 1 { /* still inspect other kinds */
		}
		for candidateID, resource := range entries {
			if candidateKind == kind && candidateID == id {
				continue
			}
			if references(resource, kind, id) {
				return fmt.Sprintf("%s %q", candidateKind, candidateID)
			}
		}
	}
	if kind == model.KindProviderSegment {
		segment, exists := s.resources[kind][id].(*model.ProviderSegment)
		if exists {
			providerResource, providerExists := s.resources[model.KindProviderNetwork][segment.ProviderNetworkID]
			if providerExists && providerResource.(*model.ProviderNetwork).DefaultSegmentID == "" {
				matching := 0
				for _, resource := range s.resources[model.KindProviderSegment] {
					if resource.(*model.ProviderSegment).ProviderNetworkID == segment.ProviderNetworkID {
						matching++
					}
				}
				if matching == 1 {
					for networkID, resource := range s.resources[model.KindNetwork] {
						if resource.(*model.Network).ProviderNetworkID == segment.ProviderNetworkID {
							return fmt.Sprintf("%s %q", model.KindNetwork, networkID)
						}
					}
				}
			}
		}
	}
	if kind == model.KindRouterInterface {
		routerInterface, ok := s.resources[kind][id].(*model.RouterInterface)
		if ok {
			for floatingID, resource := range s.resources[model.KindFloatingIP] {
				floating := resource.(*model.FloatingIP)
				if floating.RouterID != routerInterface.RouterID || floating.PortID == "" {
					continue
				}
				portResource, exists := s.resources[model.KindPort][floating.PortID]
				if exists && portHasFixedIP(portResource.(*model.Port), routerInterface.SubnetID, floating.FixedIPAddress) {
					return fmt.Sprintf("%s %q", model.KindFloatingIP, floatingID)
				}
			}
		}
	}
	if kind == model.KindSubnet {
		subnet, exists := s.resources[kind][id].(*model.Subnet)
		if exists {
			networkResource, networkExists := s.resources[model.KindNetwork][subnet.NetworkID]
			if networkExists {
				network := networkResource.(*model.Network)
				if network.External && network.ProviderNetworkID != "" {
					for floatingID, resource := range s.resources[model.KindFloatingIP] {
						floating := resource.(*model.FloatingIP)
						if floating.ProviderNetworkID == network.ProviderNetworkID && model.ValidateIPv4AllocationAddress(subnet, floating.Address) == nil && !s.providerHasAllocatableAddressLocked(network.ProviderNetworkID, floating.Address, id) {
							return fmt.Sprintf("%s %q", model.KindFloatingIP, floatingID)
						}
					}
				}
			}
		}
	}
	return ""
}

func references(resource model.Resource, kind model.Kind, id string) bool {
	switch value := resource.(type) {
	case *model.ProviderNetwork:
		return kind == model.KindProviderSegment && value.DefaultSegmentID == id
	case *model.Network:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindProviderNetwork && value.ProviderNetworkID == id)
	case *model.Subnet:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindNetwork && value.NetworkID == id)
	case *model.Port:
		if (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindNetwork && value.NetworkID == id) {
			return true
		}
		if kind == model.KindSubnet {
			for _, fixed := range value.FixedIPs {
				if fixed.SubnetID == id {
					return true
				}
			}
		}
		if kind == model.KindSecurityGroup {
			for _, groupID := range value.SecurityGroupIDs {
				if groupID == id {
					return true
				}
			}
		}
	case *model.IPAllocation:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindSubnet && value.SubnetID == id) || (kind == model.KindPort && value.PortID == id)
	case *model.Router:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindNetwork && value.ExternalNetworkID == id) || (kind == model.KindSubnet && value.ExternalSubnetID == id)
	case *model.RouterInterface:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindRouter && value.RouterID == id) || (kind == model.KindSubnet && value.SubnetID == id) || (kind == model.KindPort && value.PortID == id)
	case *model.FloatingIP:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindProviderNetwork && value.ProviderNetworkID == id) || (kind == model.KindPort && value.PortID == id) || (kind == model.KindRouter && value.RouterID == id)
	case *model.ProviderSegment:
		return kind == model.KindProviderNetwork && value.ProviderNetworkID == id
	case *model.SecurityGroup:
		return kind == model.KindProject && value.ProjectID == id
	case *model.SecurityGroupRule:
		return (kind == model.KindProject && value.ProjectID == id) || (kind == model.KindSecurityGroup && (value.SecurityGroupID == id || value.RemoteGroupID == id))
	}
	return false
}
