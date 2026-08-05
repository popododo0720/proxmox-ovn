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

	"github.com/pvnstack/proxmox-ovn/internal/controlstore"
	"github.com/pvnstack/proxmox-ovn/internal/model"
)

const maxWriteAttempts = 64

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
	ids := make([]string, 0, len(current.resources[kind]))
	for id, entry := range current.resources[kind] {
		if matches(entry.resource, options) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]model.Resource, 0, len(ids))
	for _, id := range ids {
		copyResource, err := model.Clone(current.resources[kind][id].resource)
		if err != nil {
			return nil, err
		}
		result = append(result, copyResource)
	}
	return result, nil
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
