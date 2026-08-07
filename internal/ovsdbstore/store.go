// Package ovsdbstore persists PVN desired state in the clustered PVN_Control
// OVSDB database.
package ovsdbstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	maxWriteAttempts                = 64
	maxOperationPruneBatch          = 256
	maxExpiredOperationRecoverBatch = 256
	expiredOperationError           = "operation lease expired before completion"
	supersededQueuedOperationError  = "operation target revision was superseded before claim"
)

// Config defines the secure OVSDB connection. Only unix: and ssl: endpoints
// are accepted. SSL endpoints require certificate authentication and a CA.
type Config struct {
	Endpoints []string
	TLSConfig *tls.Config
}

type Option func(*Store)

func WithClock(clock func() time.Time) Option {
	return func(store *Store) {
		if clock != nil {
			store.now = clock
		}
	}
}

func WithIDGenerator(generator func() string) Option {
	return func(store *Store) {
		if generator != nil {
			store.newID = generator
		}
	}
}

// Store is a production controlstore.Store backed by PVN_Control. Every write
// is serialized through a durable database row, allowing validation and
// idempotency decisions to be shared safely by all manager nodes.
type Store struct {
	database database
	now      func() time.Time
	newID    func() string
}

var _ controlstore.Store = (*Store)(nil)

// Open connects to the current OVSDB leader and starts a full monitor before
// returning. Any connection or monitor failure is fatal.
func Open(ctx context.Context, cfg Config, options ...Option) (*Store, error) {
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}
	store := newStore(database, options...)
	if _, err := store.load(ctx); err != nil {
		database.close()
		return nil, err
	}
	return store, nil
}

func newStore(database database, options ...Option) *Store {
	store := &Store{database: database, now: time.Now, newID: randomID}
	for _, option := range options {
		option(store)
	}
	return store
}

func (s *Store) Close() {
	if s != nil && s.database != nil {
		s.database.close()
	}
}

func (s *Store) load(ctx context.Context) (*snapshot, error) {
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		raw, err := s.database.load(ctx)
		if err != nil {
			return nil, err
		}
		current, err := decodeDatabase(raw)
		if err != nil {
			return nil, fmt.Errorf("decode PVN control database: %w", err)
		}
		if current.hasLock {
			if current.epoch == math.MaxInt64 {
				return nil, errors.New("PVN control store transaction epoch is exhausted")
			}
			return current, nil
		}
		if err := s.database.initialize(ctx, encodeLockRow(s.now().UTC())); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return nil, err
		}
	}
	return nil, storeError(controlstore.ErrConflict, "could not initialize the PVN control store after concurrent changes")
}

func (s *Store) Create(ctx context.Context, resource model.Resource, key string) (model.Resource, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if resource == nil || !resource.ResourceKind().Valid() {
		return nil, false, storeError(controlstore.ErrConflict, "invalid resource kind")
	}
	fingerprint, err := resourceFingerprint(resource, "")
	if err != nil {
		return nil, false, err
	}
	scope := idempotencyScope("create", resource.ResourceKind(), "", key)
	candidate, err := model.Clone(resource)
	if err != nil {
		return nil, false, err
	}
	if candidate.GetMetadata().ID == "" {
		candidate.GetMetadata().ID = s.newID()
	}
	if isReservedID(candidate.GetMetadata().ID) {
		return nil, false, storeError(controlstore.ErrConflict, "resource id %q is reserved", candidate.GetMetadata().ID)
	}
	if operation, ok := candidate.(*model.Operation); ok && operation.IdempotencyKey == "" {
		operation.IdempotencyKey = key
		if operation.IdempotencyKey == "" {
			return nil, false, storeError(controlstore.ErrConflict, "idempotency_key is required for an operation")
		}
	}
	if operation, ok := candidate.(*model.Operation); ok && leaseProtectedAction(operation.Action) && operation.OperationStatus != "" && operation.OperationStatus != model.OperationQueued {
		return nil, false, storeError(controlstore.ErrConflict, "%s operations must be created in queued state", operation.Action)
	}
	if operation, ok := candidate.(*model.Operation); ok && (operation.IdempotencyKey == storeLockID || strings.HasPrefix(operation.IdempotencyKey, internalIDPrefix)) {
		return nil, false, storeError(controlstore.ErrConflict, "operation idempotency_key %q is reserved", operation.IdempotencyKey)
	}
	model.SetDefaults(candidate)
	if floatingIP, ok := candidate.(*model.FloatingIP); ok {
		floatingIP.MarkPending()
	}
	if err := candidate.Validate(); err != nil {
		return nil, false, err
	}
	now := s.now().UTC()
	meta := candidate.GetMetadata()
	meta.Revision = 1
	meta.AppliedRevision = 0
	meta.State = model.ResourcePending
	meta.LastError = ""
	meta.CreatedAt = now
	meta.UpdatedAt = now
	if port, ok := candidate.(*model.Port); ok && port.LSPName == "" {
		port.LSPName = "pvn-" + port.ID
	}

	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return nil, false, err
		}
		if replay, replayed, err := replay(current, scope, fingerprint); replayed || err != nil {
			return replay, replayed, err
		}
		if _, exists := current.resources[candidate.ResourceKind()][meta.ID]; exists {
			return nil, false, storeError(controlstore.ErrAlreadyExists, "%s %q already exists", candidate.ResourceKind(), meta.ID)
		}
		if err := validateReferences(current, candidate); err != nil {
			return nil, false, err
		}
		if err := validateNewReferenceStates(current, candidate, nil); err != nil {
			return nil, false, err
		}
		if err := validateUnique(current, candidate, ""); err != nil {
			return nil, false, err
		}
		row, err := encodeResource(candidate, current)
		if err != nil {
			return nil, false, err
		}
		table, _ := tableForKind(candidate.ResourceKind())
		changes := []change{{type_: changeInsert, table: table, id: meta.ID, row: row}}
		changes, err = appendIdempotency(changes, scope, fingerprint, candidate, false, now)
		if err != nil {
			return nil, false, err
		}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(now)); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			if isReferenceConstraint(err) {
				return nil, false, storeError(controlstore.ErrConflict, "%s references a resource that changed concurrently", candidate.ResourceKind())
			}
			if isDatabaseConstraint(err) {
				return nil, false, storeError(controlstore.ErrAlreadyExists, "%s conflicts with an existing resource", candidate.ResourceKind())
			}
			return nil, false, err
		}
		result, err := model.Clone(candidate)
		return result, false, err
	}
	return nil, false, storeError(controlstore.ErrConflict, "create could not be serialized after concurrent changes")
}

func (s *Store) Get(ctx context.Context, kind model.Kind, id string) (model.Resource, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if !kind.Valid() {
		return nil, storeError(controlstore.ErrNotFound, "unknown resource kind %q", kind)
	}
	current, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	entry, exists := current.resources[kind][id]
	if !exists {
		return nil, storeError(controlstore.ErrNotFound, "%s %q was not found", kind, id)
	}
	return model.Clone(entry.resource)
}

func (s *Store) List(ctx context.Context, kind model.Kind, options controlstore.ListOptions) ([]model.Resource, error) {
	snapshot, err := s.Snapshot(ctx, []model.Kind{kind}, options)
	if err != nil {
		return nil, err
	}
	return snapshot[kind], nil
}

func (s *Store) Snapshot(ctx context.Context, kinds []model.Kind, options controlstore.ListOptions) (controlstore.ResourceSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	requested := make([]model.Kind, 0, len(kinds))
	seen := make(map[model.Kind]struct{}, len(kinds))
	for _, kind := range kinds {
		if !kind.Valid() {
			return nil, storeError(controlstore.ErrNotFound, "unknown resource kind %q", kind)
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}
		requested = append(requested, kind)
	}
	if options.Limit < 0 {
		return nil, storeError(controlstore.ErrConflict, "list limit cannot be negative")
	}
	if len(requested) == 0 {
		return controlstore.ResourceSnapshot{}, nil
	}
	current, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	result := make(controlstore.ResourceSnapshot, len(requested))
	for _, kind := range requested {
		resources := make([]model.Resource, 0, len(current.resources[kind]))
		for _, entry := range current.resources[kind] {
			if matches(entry.resource, options) {
				resources = append(resources, entry.resource)
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

// ObserveNodeHeartbeat persists runtime liveness while retaining the exact
// desired-state metadata used to fence reconcilers. The store transaction
// epoch serializes this observational write with administrative updates.
func (s *Store) ObserveNodeHeartbeat(ctx context.Context, id string, expectedRevision int64, observedAt time.Time) (*model.Node, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if id == "" || observedAt.IsZero() {
		return nil, storeError(controlstore.ErrConflict, "node id and heartbeat observation time are required")
	}
	if isReservedID(id) {
		return nil, storeError(controlstore.ErrNotFound, "node %q was not found", id)
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return nil, err
		}
		entry, exists := current.resources[model.KindNode][id]
		if !exists {
			return nil, storeError(controlstore.ErrNotFound, "node %q was not found", id)
		}
		stored := entry.resource.(*model.Node)
		if expectedRevision < 1 || stored.Revision != expectedRevision {
			return nil, storeError(controlstore.ErrPrecondition, "expected revision %d but current revision is %d", expectedRevision, stored.Revision)
		}
		if stored.LastSeenAt != nil && !stored.LastSeenAt.Before(observedAt) {
			result, err := model.Clone(stored)
			if err != nil {
				return nil, err
			}
			return result.(*model.Node), nil
		}
		candidateResource, err := model.Clone(stored)
		if err != nil {
			return nil, err
		}
		candidate := candidateResource.(*model.Node)
		timestamp := observedAt.UTC()
		candidate.LastSeenAt = &timestamp
		row, err := encodeResource(candidate, current)
		if err != nil {
			return nil, err
		}
		changes := []change{{type_: changeUpdate, table: kindTables[model.KindNode], id: id, expectedRevision: expectedRevision, row: row}}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(timestamp)); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return nil, err
		}
		result, err := model.Clone(candidate)
		if err != nil {
			return nil, err
		}
		return result.(*model.Node), nil
	}
	return nil, storeError(controlstore.ErrConflict, "node heartbeat observation could not be serialized after concurrent changes")
}

func (s *Store) PruneOperations(ctx context.Context, before time.Time, keep int) (int, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	if before.IsZero() || keep < 0 {
		return 0, storeError(controlstore.ErrConflict, "operation retention cutoff and non-negative keep count are required")
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return 0, err
		}
		operations := make([]*model.Operation, 0, len(current.resources[model.KindOperation]))
		for _, entry := range current.resources[model.KindOperation] {
			operation := entry.resource.(*model.Operation)
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
		if len(operations) <= keep {
			return 0, nil
		}
		changes := make([]change, 0, maxOperationPruneBatch*2)
		pruned := 0
		for _, operation := range operations[keep:] {
			if pruned == maxOperationPruneBatch {
				break
			}
			if !operation.CompletedAt.Before(before) || !operationSuperseded(current, operation) {
				continue
			}
			scope := idempotencyScope("create", model.KindOperation, "", operation.IdempotencyKey)
			record, exists := current.idempotency[scope]
			if !exists || record.resource.GetMetadata().ID != operation.ID {
				continue
			}
			changes = append(changes,
				change{type_: changeDelete, table: kindTables[model.KindOperation], id: operation.ID, expectedRevision: operation.Revision},
				change{type_: changeDelete, table: kindTables[model.KindOperation], id: idempotencyRowID(scope), expectedRevision: 1},
			)
			pruned++
		}
		if pruned == 0 {
			return 0, nil
		}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(s.now().UTC())); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return 0, err
		}
		return pruned, nil
	}
	return 0, storeError(controlstore.ErrConflict, "operation retention could not be serialized after concurrent changes")
}

func operationSuperseded(current *snapshot, operation *model.Operation) bool {
	target, exists := current.resources[operation.TargetKind][operation.TargetID]
	return !exists || target.resource.GetMetadata().Revision > operation.TargetRevision
}

// RecoverExpiredOperations atomically terminalizes a bounded set of stale
// writer claims and queued reconciles for superseded target revisions. The
// PVN_Control transaction epoch and per-row revisions make the result race-safe
// with a heartbeat, claim, or target update: exactly one transaction wins, and
// the loser reloads the new state.
func (s *Store) RecoverExpiredOperations(ctx context.Context, leaseCutoff, recoveredAt time.Time, limit int) (int, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	if leaseCutoff.IsZero() || recoveredAt.IsZero() || limit < 1 {
		return 0, storeError(controlstore.ErrConflict, "lease cutoff, recovery time, and positive limit are required")
	}
	if limit > maxExpiredOperationRecoverBatch {
		limit = maxExpiredOperationRecoverBatch
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return 0, err
		}
		expired := make([]*model.Operation, 0)
		queued := make([]*model.Operation, 0)
		for _, entry := range current.resources[model.KindOperation] {
			operation := entry.resource.(*model.Operation)
			switch {
			case leaseProtectedAction(operation.Action) && operation.OperationStatus == model.OperationRunning && !reconcileLeaseIsLive(operation, leaseCutoff):
				expired = append(expired, operation)
			case operation.Action == "reconcile" && operation.OperationStatus == model.OperationQueued && operationSuperseded(current, operation):
				queued = append(queued, operation)
			}
		}
		sort.Slice(expired, func(i, j int) bool {
			left, right := expired[i], expired[j]
			if !left.UpdatedAt.Equal(right.UpdatedAt) {
				return left.UpdatedAt.Before(right.UpdatedAt)
			}
			return left.ID < right.ID
		})
		sort.Slice(queued, func(i, j int) bool {
			left, right := queued[i], queued[j]
			if !left.UpdatedAt.Equal(right.UpdatedAt) {
				return left.UpdatedAt.Before(right.UpdatedAt)
			}
			return left.ID < right.ID
		})
		candidates := expired
		if len(candidates) < limit {
			remaining := limit - len(candidates)
			if len(queued) > remaining {
				queued = queued[:remaining]
			}
			candidates = append(candidates, queued...)
		}
		if len(candidates) > limit {
			candidates = candidates[:limit]
		}
		if len(candidates) == 0 {
			return 0, nil
		}
		completed := recoveredAt.UTC()
		changes := make([]change, 0, len(candidates))
		for _, operation := range candidates {
			copyResource, err := model.Clone(operation)
			if err != nil {
				return 0, err
			}
			recovered := copyResource.(*model.Operation)
			recovered.OperationStatus = model.OperationFailed
			recovered.Error = operationRecoveryError(operation)
			recovered.CompletedAt = &completed
			recovered.Revision++
			recovered.State = model.ResourcePending
			recovered.LastError = ""
			recovered.UpdatedAt = completed
			row, err := encodeResource(recovered, current)
			if err != nil {
				return 0, err
			}
			changes = append(changes, change{type_: changeUpdate, table: kindTables[model.KindOperation], id: recovered.ID, expectedRevision: operation.Revision, row: row})
		}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(completed)); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return 0, err
		}
		return len(candidates), nil
	}
	return 0, storeError(controlstore.ErrConflict, "expired operation recovery could not be serialized after concurrent changes")
}

func operationRecoveryError(operation *model.Operation) string {
	if operation != nil && operation.OperationStatus == model.OperationQueued {
		return supersededQueuedOperationError
	}
	return expiredOperationError
}

func (s *Store) Update(ctx context.Context, resource model.Resource, expectedRevision int64, key string) (model.Resource, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if resource == nil || !resource.ResourceKind().Valid() || resource.GetMetadata().ID == "" {
		return nil, false, storeError(controlstore.ErrConflict, "resource id is required")
	}
	if isReservedID(resource.GetMetadata().ID) {
		return nil, false, storeError(controlstore.ErrConflict, "resource id %q is reserved", resource.GetMetadata().ID)
	}
	fingerprint, err := resourceFingerprint(resource, fmt.Sprintf("expected=%d", expectedRevision))
	if err != nil {
		return nil, false, err
	}
	id := resource.GetMetadata().ID
	scope := idempotencyScope("update", resource.ResourceKind(), id, key)
	requested, err := model.Clone(resource)
	if err != nil {
		return nil, false, err
	}
	if operation, ok := requested.(*model.Operation); ok && (operation.IdempotencyKey == storeLockID || strings.HasPrefix(operation.IdempotencyKey, internalIDPrefix)) {
		return nil, false, storeError(controlstore.ErrConflict, "operation idempotency_key %q is reserved", operation.IdempotencyKey)
	}
	model.SetDefaults(requested)
	if floatingIP, ok := requested.(*model.FloatingIP); ok {
		floatingIP.MarkPending()
	}
	if operation, ok := requested.(*model.Operation); ok && leaseProtectedAction(operation.Action) && operation.OperationStatus == model.OperationRunning {
		return nil, false, storeError(controlstore.ErrConflict, "%s operations must be started with a durable lease claim", operation.Action)
	}
	if err := requested.Validate(); err != nil {
		return nil, false, err
	}
	now := s.now().UTC()

	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return nil, false, err
		}
		if replay, replayed, err := replay(current, scope, fingerprint); replayed || err != nil {
			return replay, replayed, err
		}
		stored, exists := current.resources[requested.ResourceKind()][id]
		if !exists {
			return nil, false, storeError(controlstore.ErrNotFound, "%s %q was not found", requested.ResourceKind(), id)
		}
		storedMeta := stored.resource.GetMetadata()
		if expectedRevision < 1 || storedMeta.Revision != expectedRevision {
			return nil, false, storeError(controlstore.ErrPrecondition, "expected revision %d but current revision is %d", expectedRevision, storedMeta.Revision)
		}
		if requestedOperation, ok := requested.(*model.Operation); ok {
			storedOperation := stored.resource.(*model.Operation)
			if leaseProtectedAction(storedOperation.Action) && storedOperation.OperationStatus == model.OperationRunning && requestedOperation.LeaseOwner != storedOperation.LeaseOwner {
				return nil, false, storeError(controlstore.ErrConflict, "operation %q lease is owned by another manager", storedOperation.ID)
			}
		}
		candidate, err := model.Clone(requested)
		if err != nil {
			return nil, false, err
		}
		meta := candidate.GetMetadata()
		meta.ID = id
		meta.Revision = storedMeta.Revision + 1
		meta.AppliedRevision = storedMeta.AppliedRevision
		meta.State = model.ResourcePending
		meta.LastError = ""
		meta.CreatedAt = storedMeta.CreatedAt
		meta.UpdatedAt = now
		if err := validateReferences(current, candidate); err != nil {
			return nil, false, err
		}
		if err := validateNewReferenceStates(current, candidate, stored.resource); err != nil {
			return nil, false, err
		}
		if err := validateUnique(current, candidate, id); err != nil {
			return nil, false, err
		}
		if err := validateReplacement(current, candidate); err != nil {
			return nil, false, err
		}
		row, err := encodeResource(candidate, current)
		if err != nil {
			return nil, false, err
		}
		table, _ := tableForKind(candidate.ResourceKind())
		changes := []change{{type_: changeUpdate, table: table, id: id, expectedRevision: expectedRevision, row: row}}
		changes, err = appendIdempotency(changes, scope, fingerprint, candidate, false, now)
		if err != nil {
			return nil, false, err
		}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(now)); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			if isReferenceConstraint(err) {
				return nil, false, storeError(controlstore.ErrConflict, "%s references a resource that changed concurrently", candidate.ResourceKind())
			}
			if isDatabaseConstraint(err) {
				return nil, false, storeError(controlstore.ErrAlreadyExists, "%s conflicts with an existing resource", candidate.ResourceKind())
			}
			return nil, false, err
		}
		result, err := model.Clone(candidate)
		return result, false, err
	}
	return nil, false, storeError(controlstore.ErrConflict, "update could not be serialized after concurrent changes")
}

// ClaimReconcile atomically claims an exact active desired-state revision.
func (s *Store) ClaimReconcile(ctx context.Context, operationID string, expectedRevision int64, leaseOwner string, startedAt, leaseCutoff time.Time) (*model.Operation, error) {
	return s.claimOperation(ctx, operationID, expectedRevision, "reconcile", leaseOwner, startedAt, leaseCutoff)
}

// ClaimDelete atomically claims an exact persisted deletion tombstone.
func (s *Store) ClaimDelete(ctx context.Context, operationID string, expectedRevision int64, leaseOwner string, startedAt, leaseCutoff time.Time) (*model.Operation, error) {
	return s.claimOperation(ctx, operationID, expectedRevision, "delete", leaseOwner, startedAt, leaseCutoff)
}

func (s *Store) claimOperation(ctx context.Context, operationID string, expectedRevision int64, action, leaseOwner string, startedAt, leaseCutoff time.Time) (*model.Operation, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if !validLeaseOwner(leaseOwner) {
		return nil, storeError(controlstore.ErrConflict, "lease owner is invalid")
	}
	if isReservedID(operationID) {
		return nil, storeError(controlstore.ErrNotFound, "operation %q was not found", operationID)
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return nil, err
		}
		entry, exists := current.resources[model.KindOperation][operationID]
		if !exists {
			return nil, storeError(controlstore.ErrNotFound, "operation %q was not found", operationID)
		}
		stored := entry.resource.(*model.Operation)
		if expectedRevision < 1 || stored.Revision != expectedRevision {
			return nil, storeError(controlstore.ErrPrecondition, "expected operation revision %d but current revision is %d", expectedRevision, stored.Revision)
		}
		if stored.Action != action {
			return nil, storeError(controlstore.ErrConflict, "operation %q is not a %s operation", operationID, action)
		}
		if stored.OperationStatus == model.OperationRunning && reconcileLeaseIsLive(stored, leaseCutoff) {
			return nil, storeError(controlstore.ErrConflict, "%s operation %q still holds a live lease", action, operationID)
		}
		targetEntry, exists := current.resources[stored.TargetKind][stored.TargetID]
		if !exists {
			return nil, storeError(controlstore.ErrPrecondition, "%s %q no longer exists", stored.TargetKind, stored.TargetID)
		}
		targetMeta := targetEntry.resource.GetMetadata()
		if !operationTargetClaimable(stored, targetMeta, true) {
			return nil, storeError(controlstore.ErrPrecondition, "%s target %s %q is not at claimable revision %d", action, stored.TargetKind, stored.TargetID, stored.TargetRevision)
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
		row, err := encodeResource(claimed, current)
		if err != nil {
			return nil, err
		}
		changes := []change{{type_: changeUpdate, table: kindTables[model.KindOperation], id: operationID, expectedRevision: expectedRevision, row: row}}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(started)); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return nil, err
		}
		result, err := model.Clone(claimed)
		if err != nil {
			return nil, err
		}
		return result.(*model.Operation), nil
	}
	return nil, storeError(controlstore.ErrConflict, "%s claim could not be serialized after concurrent changes", action)
}

// RenewOperationLease atomically proves both owner identity and the latest
// operation revision before extending the writer lease.
func (s *Store) RenewOperationLease(ctx context.Context, operationID string, expectedRevision int64, leaseOwner string, renewedAt time.Time) (*model.Operation, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if isReservedID(operationID) {
		return nil, storeError(controlstore.ErrNotFound, "operation %q was not found", operationID)
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return nil, err
		}
		entry, exists := current.resources[model.KindOperation][operationID]
		if !exists {
			return nil, storeError(controlstore.ErrNotFound, "operation %q was not found", operationID)
		}
		stored := entry.resource.(*model.Operation)
		if expectedRevision < 1 || stored.Revision != expectedRevision {
			return nil, storeError(controlstore.ErrPrecondition, "expected operation revision %d but current revision is %d", expectedRevision, stored.Revision)
		}
		if !leaseProtectedAction(stored.Action) || stored.OperationStatus != model.OperationRunning || leaseOwner == "" || stored.LeaseOwner != leaseOwner {
			return nil, storeError(controlstore.ErrConflict, "operation %q lease is not owned by this manager", operationID)
		}
		targetEntry, exists := current.resources[stored.TargetKind][stored.TargetID]
		if !exists || !operationTargetClaimable(stored, targetEntry.resource.GetMetadata(), false) {
			return nil, storeError(controlstore.ErrPrecondition, "%s target %s %q is no longer writable", stored.Action, stored.TargetKind, stored.TargetID)
		}
		copyResource, err := model.Clone(stored)
		if err != nil {
			return nil, err
		}
		renewed := copyResource.(*model.Operation)
		timestamp := renewedAt.UTC()
		renewed.Revision++
		renewed.UpdatedAt = timestamp
		row, err := encodeResource(renewed, current)
		if err != nil {
			return nil, err
		}
		changes := []change{{type_: changeUpdate, table: kindTables[model.KindOperation], id: operationID, expectedRevision: expectedRevision, row: row}}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(timestamp)); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return nil, err
		}
		result, err := model.Clone(renewed)
		if err != nil {
			return nil, err
		}
		return result.(*model.Operation), nil
	}
	return nil, storeError(controlstore.ErrConflict, "operation lease renewal could not be serialized after concurrent changes")
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

// FenceReconciles atomically expires abandoned claims for a target and reports
// whether at least one unexpired reconcile lease still prevents cleanup.
func (s *Store) FenceReconciles(ctx context.Context, kind model.Kind, id string, leaseCutoff, recoveredAt time.Time) (bool, bool, error) {
	if err := contextError(ctx); err != nil {
		return false, false, err
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return false, false, err
		}
		active, recovered := false, false
		changes := make([]change, 0)
		for operationID, entry := range current.resources[model.KindOperation] {
			operation := entry.resource.(*model.Operation)
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
			row, err := encodeResource(recoveredOperation, current)
			if err != nil {
				return false, false, err
			}
			changes = append(changes, change{type_: changeUpdate, table: kindTables[model.KindOperation], id: operationID, expectedRevision: operation.Revision, row: row})
			recovered = true
		}
		if len(changes) == 0 {
			return active, false, nil
		}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(recoveredAt.UTC())); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return false, false, err
		}
		return active, recovered, nil
	}
	return false, false, storeError(controlstore.ErrConflict, "reconcile fencing could not be serialized after concurrent changes")
}

func reconcileLeaseIsLive(operation *model.Operation, leaseCutoff time.Time) bool {
	return operation != nil && operation.StartedAt != nil && operation.UpdatedAt.UTC().After(leaseCutoff.UTC())
}

func (s *Store) Delete(ctx context.Context, kind model.Kind, id string, expectedRevision int64, key string) (bool, error) {
	tombstone, replayed, err := s.BeginDelete(ctx, kind, id, expectedRevision, key)
	if err != nil {
		return false, err
	}
	if err := s.Purge(ctx, kind, id, tombstone.GetMetadata().Revision); err != nil {
		return false, err
	}
	return replayed, nil
}

func (s *Store) BeginDelete(ctx context.Context, kind model.Kind, id string, expectedRevision int64, key string) (model.Resource, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if !kind.Valid() || isReservedID(id) {
		return nil, false, storeError(controlstore.ErrNotFound, "%s %q was not found", kind, id)
	}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", id, expectedRevision)))
	scope := idempotencyScope("delete", kind, id, key)
	now := s.now().UTC()

	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return nil, false, err
		}
		if replay, replayed, err := replay(current, scope, fingerprint); replayed || err != nil {
			return replay, replayed, err
		}
		stored, exists := current.resources[kind][id]
		if !exists {
			return nil, false, storeError(controlstore.ErrNotFound, "%s %q was not found", kind, id)
		}
		if expectedRevision < 1 || stored.resource.GetMetadata().Revision != expectedRevision {
			return nil, false, storeError(controlstore.ErrPrecondition, "expected revision %d but current revision is %d", expectedRevision, stored.resource.GetMetadata().Revision)
		}
		if referrer := firstReference(current, kind, id); referrer != "" {
			return nil, false, storeError(controlstore.ErrConflict, "%s %q is still referenced by %s", kind, id, referrer)
		}
		tombstone, err := model.Clone(stored.resource)
		if err != nil {
			return nil, false, err
		}
		meta := tombstone.GetMetadata()
		meta.Revision++
		meta.State = model.ResourceDeleting
		meta.LastError = ""
		meta.UpdatedAt = now
		if floatingIP, ok := tombstone.(*model.FloatingIP); ok {
			floatingIP.MarkPending()
		}
		row, err := encodeResource(tombstone, current)
		if err != nil {
			return nil, false, err
		}
		table, _ := tableForKind(kind)
		changes := []change{{type_: changeUpdate, table: table, id: id, expectedRevision: expectedRevision, row: row}}
		changes, err = appendIdempotency(changes, scope, fingerprint, tombstone, true, now)
		if err != nil {
			return nil, false, err
		}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(now)); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return nil, false, err
		}
		result, err := model.Clone(tombstone)
		return result, false, err
	}
	return nil, false, storeError(controlstore.ErrConflict, "delete could not be serialized after concurrent changes")
}

func (s *Store) Purge(ctx context.Context, kind model.Kind, id string, deletionRevision int64) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return err
		}
		stored, exists := current.resources[kind][id]
		if !exists {
			return nil
		}
		meta := stored.resource.GetMetadata()
		if meta.Revision != deletionRevision {
			return storeError(controlstore.ErrPrecondition, "delete revision %d does not match current revision %d", deletionRevision, meta.Revision)
		}
		if meta.State != model.ResourceDeleting && meta.State != model.ResourceError {
			return storeError(controlstore.ErrConflict, "%s %q is not marked for deletion", kind, id)
		}
		if referrer := firstReference(current, kind, id); referrer != "" {
			return storeError(controlstore.ErrConflict, "%s %q is still referenced by %s", kind, id, referrer)
		}
		for _, entry := range current.resources[model.KindOperation] {
			operation := entry.resource.(*model.Operation)
			if operation.TargetKind == kind && operation.TargetID == id && operation.OperationStatus == model.OperationRunning {
				return storeError(controlstore.ErrConflict, "%s %q is protected by running operation %q", kind, id, operation.ID)
			}
		}
		table, err := tableForKind(kind)
		if err != nil {
			return err
		}
		changes := []change{{type_: changeDelete, table: table, id: id, expectedRevision: deletionRevision}}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(s.now().UTC())); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			if isDatabaseConstraint(err) {
				return storeError(controlstore.ErrConflict, "%s %q is still referenced", kind, id)
			}
			return err
		}
		return nil
	}
	return storeError(controlstore.ErrConflict, "purge could not be serialized after concurrent changes")
}

func (s *Store) MarkReconciled(ctx context.Context, kind model.Kind, id string, revision int64, renderErr error) (model.Resource, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < maxWriteAttempts; attempt++ {
		current, err := s.load(ctx)
		if err != nil {
			return nil, err
		}
		stored, exists := current.resources[kind][id]
		if !exists {
			return nil, storeError(controlstore.ErrNotFound, "%s %q was not found", kind, id)
		}
		resource, err := model.Clone(stored.resource)
		if err != nil {
			return nil, err
		}
		meta := resource.GetMetadata()
		if revision > meta.Revision {
			return nil, storeError(controlstore.ErrPrecondition, "rendered revision %d is newer than desired revision %d", revision, meta.Revision)
		}
		changed := false
		if renderErr != nil {
			if revision == meta.Revision {
				meta.State = model.ResourceError
				meta.LastError = truncateError(renderErr.Error())
				meta.UpdatedAt = s.now().UTC()
				if floatingIP, ok := resource.(*model.FloatingIP); ok {
					floatingIP.MarkReconciled(renderErr)
				}
				changed = true
			}
		} else {
			if revision > meta.AppliedRevision {
				meta.AppliedRevision = revision
				changed = true
			}
			if revision == meta.Revision {
				meta.State = model.ResourceReady
				meta.LastError = ""
				meta.UpdatedAt = s.now().UTC()
				if floatingIP, ok := resource.(*model.FloatingIP); ok {
					floatingIP.MarkReconciled(nil)
				}
				changed = true
			}
		}
		if !changed {
			return resource, nil
		}
		row, err := encodeResource(resource, current)
		if err != nil {
			return nil, err
		}
		table, err := tableForKind(kind)
		if err != nil {
			return nil, err
		}
		changes := []change{{type_: changeUpdate, table: table, id: id, expectedRevision: meta.Revision, row: row}}
		if err := s.database.commit(ctx, current.epoch, changes, formatTime(s.now().UTC())); err != nil {
			if errors.Is(err, errSerialization) {
				continue
			}
			return nil, err
		}
		return resource, nil
	}
	return nil, storeError(controlstore.ErrConflict, "reconcile status could not be serialized after concurrent changes")
}

func appendIdempotency(changes []change, scope string, fingerprint [sha256.Size]byte, resource model.Resource, deleted bool, now time.Time) ([]change, error) {
	if scope == "" {
		return changes, nil
	}
	row, err := encodeIdempotencyRow(scope, fingerprint, resource, deleted, now)
	if err != nil {
		return nil, err
	}
	return append(changes, change{type_: changeInsert, table: kindTables[model.KindOperation], id: row["id"].(string), row: row}), nil
}

func replay(current *snapshot, scope string, fingerprint [sha256.Size]byte) (model.Resource, bool, error) {
	if scope == "" {
		return nil, false, nil
	}
	record, exists := current.idempotency[scope]
	if !exists {
		return nil, false, nil
	}
	if record.fingerprint != fingerprint {
		return nil, false, storeError(controlstore.ErrIdempotencyConflict, "idempotency key was already used for a different request")
	}
	resource, err := model.Clone(record.resource)
	return resource, true, err
}

func resourceFingerprint(resource model.Resource, qualifier string) ([sha256.Size]byte, error) {
	encoded, err := json.Marshal(resource)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	encoded = append(encoded, qualifier...)
	if operation, ok := resource.(*model.Operation); ok && operation.Payload != "" {
		// Operation payloads are private API state but remain part of the
		// idempotent write identity.
		encoded = append(encoded, 0)
		encoded = append(encoded, operation.Payload...)
	}
	return sha256.Sum256(encoded), nil
}

func idempotencyScope(action string, kind model.Kind, id, key string) string {
	if key == "" {
		return ""
	}
	return strings.Join([]string{action, kind.String(), id, key}, "/")
}

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		value[6] = (value[6] & 0x0f) | 0x40
		value[8] = (value[8] & 0x3f) | 0x80
		encoded := hex.EncodeToString(value[:])
		return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
	}
	return fmt.Sprintf("pvn-%d", time.Now().UnixNano())
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func storeError(kind error, format string, args ...any) error {
	return &controlstore.Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func isReservedID(id string) bool {
	return id == storeLockID || strings.HasPrefix(id, internalIDPrefix)
}

func isDatabaseConstraint(err error) bool {
	var constraint *constraintError
	return errors.As(err, &constraint)
}

func isReferenceConstraint(err error) bool {
	var constraint *constraintError
	return errors.As(err, &constraint) && constraint.reference
}

func truncateError(value string) string {
	if len(value) <= maxStoredErrorLen {
		return value
	}
	return value[:maxStoredErrorLen]
}
