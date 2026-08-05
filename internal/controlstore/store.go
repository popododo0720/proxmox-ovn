package controlstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/model"
)

var (
	ErrNotFound            = errors.New("resource not found")
	ErrAlreadyExists       = errors.New("resource already exists")
	ErrConflict            = errors.New("resource conflict")
	ErrPrecondition        = errors.New("revision precondition failed")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different request")
)

type Error struct {
	Kind    error
	Message string
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Kind }

func storeError(kind error, format string, args ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

type ListOptions struct {
	ProjectID string
	NetworkID string
	NodeID    string
	VMID      int
	NIC       string
	// RecentFirst orders matching resources by creation time descending, with
	// the resource ID as a deterministic tie breaker. Limit is applied after
	// filtering and ordering; zero means no store-level limit.
	RecentFirst bool
	Limit       int
}

// ResourceSnapshot is an atomic, read-only view of one or more resource
// kinds. Each requested kind is filtered, ordered, and limited independently
// using the same semantics as List.
type ResourceSnapshot map[model.Kind][]model.Resource

type Store interface {
	Create(context.Context, model.Resource, string) (model.Resource, bool, error)
	Get(context.Context, model.Kind, string) (model.Resource, error)
	List(context.Context, model.Kind, ListOptions) ([]model.Resource, error)
	// Snapshot reads and clones all requested kinds from one store view. This is
	// intended for control-plane passes that must avoid reloading the complete
	// durable database once per kind. Duplicate kinds are collapsed.
	Snapshot(context.Context, []model.Kind, ListOptions) (ResourceSnapshot, error)
	Update(context.Context, model.Resource, int64, string) (model.Resource, bool, error)
	// ObserveNodeHeartbeat records runtime liveness without changing the
	// desired-state revision or reconciliation status of the node.
	ObserveNodeHeartbeat(context.Context, string, int64, time.Time) (*model.Node, error)
	// PruneOperations removes a bounded batch of old terminal reconcile audit
	// records whose target revision has been superseded (or target was purged),
	// while always retaining at least keep most-recent terminal reconciles.
	PruneOperations(context.Context, time.Time, int) (int, error)
	// RecoverExpiredOperations atomically fails a bounded batch of abandoned
	// running reconcile/delete operations. A concurrent heartbeat either wins
	// before selection or loses its revision CAS after recovery.
	RecoverExpiredOperations(context.Context, time.Time, time.Time, int) (int, error)
	ClaimReconcile(context.Context, string, int64, string, time.Time, time.Time) (*model.Operation, error)
	ClaimDelete(context.Context, string, int64, string, time.Time, time.Time) (*model.Operation, error)
	RenewOperationLease(context.Context, string, int64, string, time.Time) (*model.Operation, error)
	FenceReconciles(context.Context, model.Kind, string, time.Time, time.Time) (bool, bool, error)
	BeginDelete(context.Context, model.Kind, string, int64, string) (model.Resource, bool, error)
	Purge(context.Context, model.Kind, string, int64) error
	Delete(context.Context, model.Kind, string, int64, string) (bool, error)
	MarkReconciled(context.Context, model.Kind, string, int64, error) (model.Resource, error)
}
