package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

// Renderer materializes one immutable desired resource revision. Implementations
// must treat repeated calls for the same kind, id, and revision as idempotent
// and stop issuing external writes after the context is cancelled.
type Renderer interface {
	Render(context.Context, model.Resource) error
	Delete(context.Context, model.Resource) error
}

type Controller struct {
	store             controlstore.Store
	renderer          Renderer
	locksMu           sync.Mutex
	locks             map[string]*sync.Mutex
	lease             time.Duration
	heartbeat         time.Duration
	completionTimeout time.Duration
	now               func() time.Time
	newOwner          func() string
	retention         operationRetention
}

// ErrReconcileLeaseActive means deletion was durably recorded but cleanup must
// wait for a manager that is still allowed to change the target's realized
// state. Callers must leave the tombstone in place and retry later.
var ErrReconcileLeaseActive = errors.New("target has an active reconcile lease")

const (
	operationLease             = 5 * time.Minute
	operationCompletionTimeout = 5 * time.Second
	maxConvergencePasses       = 8
	maxHeartbeatInterval       = 30 * time.Second
	defaultOperationKeep       = 1000
	defaultOperationAge        = 24 * time.Hour
	operationRecoveryBatch     = 256
)

type operationRetention struct {
	keep int
	age  time.Duration
}

type Option func(*Controller)

// WithLeaseDuration uses the cluster orphan grace as the maximum interval
// between durable writer heartbeats.
func WithLeaseDuration(duration time.Duration) Option {
	return func(controller *Controller) {
		if duration > 0 {
			controller.lease = duration
			controller.heartbeat = heartbeatInterval(duration)
		}
	}
}

// WithOperationRetention configures the minimum number and age of terminal,
// superseded reconcile audit records retained by periodic reconciliation.
func WithOperationRetention(keep int, age time.Duration) Option {
	return func(controller *Controller) {
		if keep >= 0 && age > 0 {
			controller.retention = operationRetention{keep: keep, age: age}
		}
	}
}

func NewController(store controlstore.Store, renderer Renderer, options ...Option) *Controller {
	controller := &Controller{
		store: store, renderer: renderer, locks: make(map[string]*sync.Mutex),
		lease: operationLease, heartbeat: heartbeatInterval(operationLease), completionTimeout: operationCompletionTimeout,
		now: time.Now, newOwner: randomLeaseOwner,
		retention: operationRetention{keep: defaultOperationKeep, age: defaultOperationAge},
	}
	for _, option := range options {
		option(controller)
	}
	return controller
}

func (c *Controller) Reconcile(ctx context.Context, kind model.Kind, id string) error {
	return c.reconcile(ctx, kind, id, false, time.Time{})
}

func (c *Controller) reconcile(ctx context.Context, kind model.Kind, id string, force bool, freshSince time.Time) error {
	if c == nil || c.store == nil || c.renderer == nil {
		return errors.New("reconciler is not configured")
	}
	lock := c.resourceLock(kind, id)
	lock.Lock()

	resource, err := c.store.Get(ctx, kind, id)
	if err != nil {
		lock.Unlock()
		return err
	}
	meta := resource.GetMetadata()
	if meta.State == model.ResourceDeleting {
		// A manager may have died after persisting the tombstone but before
		// removing the OVN rows. Release the local lock because Delete takes
		// the same lock, then finish the durable delete workflow. This also
		// keeps periodic reconciliation from rendering tombstones.
		lock.Unlock()
		if err := c.Delete(ctx, resource); err != nil {
			if errors.Is(err, ErrReconcileLeaseActive) {
				return nil
			}
			return err
		}
		if err := c.store.Purge(ctx, kind, id, meta.Revision); err != nil && !errors.Is(err, controlstore.ErrNotFound) {
			return fmt.Errorf("purge deleted %s %q revision %d: %w", kind, id, meta.Revision, err)
		}
		return nil
	}
	defer lock.Unlock()
	if !force && meta.AppliedRevision >= meta.Revision && meta.State == model.ResourceReady {
		return nil
	}

	operation := &model.Operation{
		Action:          "reconcile",
		TargetKind:      kind,
		TargetID:        id,
		TargetRevision:  meta.Revision,
		OperationStatus: model.OperationQueued,
		IdempotencyKey:  operationKey(kind, id, meta.Revision),
	}
	created, replayed, createErr := c.store.Create(ctx, operation, operation.IdempotencyKey)
	if createErr != nil {
		return fmt.Errorf("create reconcile operation: %w", createErr)
	}
	op := created.(*model.Operation)
	if replayed {
		latest, getErr := c.store.Get(ctx, model.KindOperation, op.ID)
		if getErr != nil {
			return fmt.Errorf("load reconcile operation: %w", getErr)
		}
		op = latest.(*model.Operation)
	}
	// Recheck durable freshness immediately before the claim. Two managers may
	// both decide that an audit is due from an older snapshot; after either one
	// completes, the other must not replay the same audit sequentially.
	if !freshSince.IsZero() && meta.State == model.ResourceReady && meta.AppliedRevision >= meta.Revision && op.OperationStatus == model.OperationSucceeded && op.CompletedAt != nil && !op.CompletedAt.Before(freshSince) {
		return nil
	}
	if op.OperationStatus == model.OperationSucceeded && !force {
		_, markErr := c.store.MarkReconciled(ctx, kind, id, meta.Revision, nil)
		return markErr
	}
	now := c.now().UTC()
	claimed, claimErr := c.store.ClaimReconcile(ctx, op.ID, op.Revision, c.newOwner(), now, now.Add(-c.lease))
	if claimErr != nil {
		if errors.Is(claimErr, controlstore.ErrPrecondition) || errors.Is(claimErr, controlstore.ErrConflict) || errors.Is(claimErr, controlstore.ErrNotFound) {
			// Another manager won the claim, or the exact desired revision
			// stopped being active before this manager could start writing.
			return nil
		}
		return fmt.Errorf("claim reconcile operation: %w", claimErr)
	}
	op = claimed

	renderContext, heartbeat := c.startHeartbeat(ctx, op)
	renderErr, markErr := c.renderUntilStable(renderContext, resource)
	op, leaseErr, leaseStoppedByParent := heartbeat.stop()
	parentErr := ctx.Err()
	if leaseErr != nil && (!leaseStoppedByParent || parentErr == nil || !errors.Is(leaseErr, parentErr)) {
		return fmt.Errorf("reconcile %s %q lost its writer lease: %w", kind, id, leaseErr)
	}
	workErr := renderErr
	if workErr == nil {
		workErr = markErr
	}
	operationErr := c.completeOperation(ctx, op, workErr)
	if parentErr == nil {
		parentErr = ctx.Err()
	}
	var resultErr error
	if renderErr != nil {
		resultErr = fmt.Errorf("render %s %q revision %d: %w", kind, id, meta.Revision, renderErr)
	} else if markErr != nil {
		resultErr = fmt.Errorf("mark %s %q reconciled: %w", kind, id, markErr)
	} else if parentErr != nil {
		resultErr = parentErr
	}
	if operationErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("complete reconcile operation: %w", operationErr))
	}
	return resultErr
}

// ReconcileAll is an explicit forced drift audit. Manager startup uses the
// freshness-aware periodic path so rolling restarts do not replay healthy OVN
// state, while tests and operator-driven repair can still force a full pass.
func (c *Controller) ReconcileAll(ctx context.Context) error {
	return c.reconcileAll(ctx, 0)
}

// ReconcilePeriodic performs a forced drift audit only when an exact desired
// revision has not completed successfully within freshness. Pending, failed,
// deleting, and superseded resources are always processed. This gives every
// manager the same durable decision and prevents clustered periodic loops from
// continuously replaying an already-healthy full audit.
func (c *Controller) ReconcilePeriodic(ctx context.Context, freshness time.Duration) error {
	if freshness <= 0 {
		return errors.New("periodic reconciliation freshness must be positive")
	}
	return c.reconcileAll(ctx, freshness)
}

func (c *Controller) reconcileAll(ctx context.Context, freshness time.Duration) error {
	if c == nil || c.store == nil || c.renderer == nil {
		return errors.New("reconciler is not configured")
	}
	var failures []error
	now := c.now().UTC()
	if _, err := c.store.RecoverExpiredOperations(ctx, now.Add(-c.lease), now, operationRecoveryBatch); err != nil {
		failures = append(failures, fmt.Errorf("recover expired operation leases: %w", err))
	}
	kinds := append([]model.Kind(nil), dependencyOrder...)
	if freshness > 0 {
		kinds = append([]model.Kind{model.KindOperation}, kinds...)
	}
	snapshot, err := c.store.Snapshot(ctx, kinds, controlstore.ListOptions{})
	if err != nil {
		failures = append(failures, fmt.Errorf("read reconciliation snapshot: %w", err))
	}
	recent := make(map[string]struct{})
	if err == nil && freshness > 0 {
		operations := snapshot[model.KindOperation]
		if operations != nil {
			cutoff := now.Add(-freshness)
			for _, resource := range operations {
				operation := resource.(*model.Operation)
				if operation.Action != "reconcile" || operation.OperationStatus != model.OperationSucceeded || operation.CompletedAt == nil || operation.CompletedAt.Before(cutoff) {
					continue
				}
				recent[operationKey(operation.TargetKind, operation.TargetID, operation.TargetRevision)] = struct{}{}
			}
		}
	}
	if err == nil {
		for _, kind := range dependencyOrder {
			resources := snapshot[kind]
			for _, resource := range resources {
				meta := resource.GetMetadata()
				if freshness > 0 && meta.State == model.ResourceReady && meta.AppliedRevision >= meta.Revision {
					if _, ok := recent[operationKey(kind, meta.ID, meta.Revision)]; ok {
						continue
					}
				}
				// A forced pass repairs OVN drift left by a manager that died
				// between an external write and desired-state confirmation.
				freshSince := time.Time{}
				if freshness > 0 {
					freshSince = now.Add(-freshness)
				}
				if err := c.reconcile(ctx, kind, meta.ID, true, freshSince); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	if _, err := c.store.PruneOperations(ctx, now.Add(-c.retention.age), c.retention.keep); err != nil {
		failures = append(failures, fmt.Errorf("prune operation audit records: %w", err))
	}
	return errors.Join(failures...)
}

func (c *Controller) renderUntilStable(ctx context.Context, initial model.Resource) (error, error) {
	desired := initial
	for pass := 0; pass < maxConvergencePasses; pass++ {
		renderErr := c.renderer.Render(ctx, desired)
		latest, latestErr := c.store.Get(ctx, desired.ResourceKind(), desired.GetMetadata().ID)
		if errors.Is(latestErr, controlstore.ErrNotFound) || (latestErr == nil && latest.GetMetadata().State == model.ResourceDeleting) {
			// Desired state can be removed while an older manager is still
			// writing OVN. Idempotent cleanup prevents a persistent orphan.
			return c.renderer.Delete(ctx, desired), nil
		}
		if latestErr != nil {
			if renderErr != nil {
				return renderErr, latestErr
			}
			return nil, fmt.Errorf("reload desired state after render: %w", latestErr)
		}
		if latest.GetMetadata().Revision != desired.GetMetadata().Revision {
			// A newer revision won while this manager was rendering. Apply it
			// before allowing the older writer to finish last.
			desired = latest
			continue
		}
		_, markErr := c.store.MarkReconciled(ctx, desired.ResourceKind(), desired.GetMetadata().ID, desired.GetMetadata().Revision, renderErr)
		if renderErr != nil || markErr != nil {
			return renderErr, markErr
		}
		confirmed, confirmErr := c.store.Get(ctx, desired.ResourceKind(), desired.GetMetadata().ID)
		if errors.Is(confirmErr, controlstore.ErrNotFound) || (confirmErr == nil && confirmed.GetMetadata().State == model.ResourceDeleting) {
			return c.renderer.Delete(ctx, desired), nil
		}
		if confirmErr != nil {
			return nil, fmt.Errorf("confirm desired state after render: %w", confirmErr)
		}
		if confirmed.GetMetadata().Revision != desired.GetMetadata().Revision {
			desired = confirmed
			continue
		}
		return nil, nil
	}
	err := fmt.Errorf("%s %q changed during %d consecutive render passes", desired.ResourceKind(), desired.GetMetadata().ID, maxConvergencePasses)
	_, markErr := c.store.MarkReconciled(ctx, desired.ResourceKind(), desired.GetMetadata().ID, desired.GetMetadata().Revision, err)
	return err, markErr
}

var dependencyOrder = []model.Kind{
	model.KindProject,
	model.KindProviderNetwork,
	model.KindNetwork,
	model.KindProviderSegment,
	model.KindSubnet,
	model.KindSecurityGroup,
	model.KindSecurityGroupRule,
	model.KindRouter,
	model.KindRouterInterface,
	model.KindPort,
	model.KindIPAllocation,
	model.KindFloatingIP,
	model.KindNode,
}

// Delete removes the rendered form of a tombstone. The caller purges desired
// state only after this method succeeds.
func (c *Controller) Delete(ctx context.Context, resource model.Resource) error {
	if c == nil || c.store == nil || c.renderer == nil {
		return errors.New("reconciler is not configured")
	}
	if resource == nil || resource.GetMetadata().State != model.ResourceDeleting {
		return errors.New("delete reconciliation requires a deleting resource")
	}
	kind, id, revision := resource.ResourceKind(), resource.GetMetadata().ID, resource.GetMetadata().Revision
	lock := c.resourceLock(kind, id)
	lock.Lock()
	defer lock.Unlock()
	now := c.now().UTC()
	active, recovered, err := c.store.FenceReconciles(ctx, kind, id, now.Add(-c.lease), now)
	if err != nil {
		return fmt.Errorf("fence reconcile operations for deleting %s %q: %w", kind, id, err)
	}
	if active {
		return fmt.Errorf("%w for %s %q", ErrReconcileLeaseActive, kind, id)
	}

	operation := &model.Operation{Action: "delete", TargetKind: kind, TargetID: id, TargetRevision: revision, OperationStatus: model.OperationQueued, IdempotencyKey: deleteOperationKey(kind, id, revision)}
	created, replayed, err := c.store.Create(ctx, operation, operation.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("create delete operation: %w", err)
	}
	op := created.(*model.Operation)
	if replayed {
		latest, getErr := c.store.Get(ctx, model.KindOperation, op.ID)
		if getErr != nil {
			return fmt.Errorf("load delete operation: %w", getErr)
		}
		op = latest.(*model.Operation)
		if op.OperationStatus == model.OperationSucceeded && !recovered {
			return nil
		}
	}
	now = c.now().UTC()
	claimed, claimErr := c.store.ClaimDelete(ctx, op.ID, op.Revision, c.newOwner(), now, now.Add(-c.lease))
	if claimErr != nil {
		if errors.Is(claimErr, controlstore.ErrPrecondition) || errors.Is(claimErr, controlstore.ErrConflict) {
			return fmt.Errorf("%w for deleting %s %q", ErrReconcileLeaseActive, kind, id)
		}
		return fmt.Errorf("claim delete operation: %w", claimErr)
	}
	op = claimed
	deleteContext, heartbeat := c.startHeartbeat(ctx, op)
	renderErr := c.renderer.Delete(deleteContext, resource)
	op, leaseErr, leaseStoppedByParent := heartbeat.stop()
	parentErr := ctx.Err()
	if leaseErr != nil && (!leaseStoppedByParent || parentErr == nil || !errors.Is(leaseErr, parentErr)) {
		return fmt.Errorf("delete %s %q lost its writer lease: %w", kind, id, leaseErr)
	}
	operationErr := c.completeOperation(ctx, op, renderErr)
	if parentErr == nil {
		parentErr = ctx.Err()
	}
	var resultErr error
	if renderErr != nil {
		resultErr = fmt.Errorf("delete rendered %s %q revision %d: %w", kind, id, revision, renderErr)
	} else if parentErr != nil {
		resultErr = parentErr
	}
	if operationErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("complete delete operation: %w", operationErr))
	}
	return resultErr
}

// completeOperation persists only terminal bookkeeping after this writer has
// stopped issuing external writes and its heartbeat has stopped. Request
// cancellation must not strand a claim until orphan recovery, but the detached
// store transaction is bounded so shutdown and a failed control database do
// not block indefinitely. The operation revision and lease owner still fence
// this update against a different manager.
func (c *Controller) completeOperation(parent context.Context, operation *model.Operation, workErr error) error {
	completed := c.now().UTC()
	operation.CompletedAt = &completed
	if workErr != nil {
		operation.OperationStatus = model.OperationFailed
		operation.Error = workErr.Error()
	} else {
		operation.OperationStatus = model.OperationSucceeded
		operation.Error = ""
	}
	timeout := c.completionTimeout
	if timeout <= 0 {
		timeout = operationCompletionTimeout
	}
	completionContext, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	_, _, err := c.store.Update(completionContext, operation, operation.Revision, "")
	return err
}

func (c *Controller) resourceLock(kind model.Kind, id string) *sync.Mutex {
	key := kind.String() + "/" + id
	c.locksMu.Lock()
	defer c.locksMu.Unlock()
	if c.locks[key] == nil {
		c.locks[key] = &sync.Mutex{}
	}
	return c.locks[key]
}

func operationKey(kind model.Kind, id string, revision int64) string {
	return fmt.Sprintf("reconcile:%s:%s:%d", kind, id, revision)
}

func deleteOperationKey(kind model.Kind, id string, revision int64) string {
	return fmt.Sprintf("delete:%s:%s:%d", kind, id, revision)
}

type operationHeartbeat struct {
	cancel        context.CancelFunc
	stopOnce      sync.Once
	stopCh        chan struct{}
	done          chan struct{}
	mu            sync.Mutex
	operation     *model.Operation
	err           error
	parentStopped bool
}

func (c *Controller) startHeartbeat(parent context.Context, operation *model.Operation) (context.Context, *operationHeartbeat) {
	workContext, cancel := context.WithCancel(parent)
	heartbeat := &operationHeartbeat{
		cancel: cancel, stopCh: make(chan struct{}), done: make(chan struct{}), operation: operation,
	}
	go func() {
		defer close(heartbeat.done)
		ticker := time.NewTicker(c.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeat.stopCh:
				return
			case <-workContext.Done():
				heartbeat.fail(workContext.Err(), true)
				return
			case <-ticker.C:
				current := heartbeat.current()
				renewed, err := c.store.RenewOperationLease(workContext, current.ID, current.Revision, current.LeaseOwner, c.now().UTC())
				if err != nil {
					heartbeat.fail(err, false)
					cancel()
					return
				}
				heartbeat.setOperation(renewed)
			}
		}
	}()
	return workContext, heartbeat
}

func (heartbeat *operationHeartbeat) current() *model.Operation {
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	return heartbeat.operation
}

func (heartbeat *operationHeartbeat) setOperation(operation *model.Operation) {
	heartbeat.mu.Lock()
	heartbeat.operation = operation
	heartbeat.mu.Unlock()
}

func (heartbeat *operationHeartbeat) fail(err error, parentStopped bool) {
	heartbeat.mu.Lock()
	if heartbeat.err == nil {
		heartbeat.err = err
		heartbeat.parentStopped = parentStopped
	}
	heartbeat.mu.Unlock()
}

func (heartbeat *operationHeartbeat) stop() (*model.Operation, error, bool) {
	heartbeat.stopOnce.Do(func() { close(heartbeat.stopCh) })
	<-heartbeat.done
	heartbeat.cancel()
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	return heartbeat.operation, heartbeat.err, heartbeat.parentStopped
}

func heartbeatInterval(lease time.Duration) time.Duration {
	interval := lease / 3
	if interval <= 0 {
		interval = time.Nanosecond
	}
	if interval > maxHeartbeatInterval {
		interval = maxHeartbeatInterval
	}
	return interval
}

func randomLeaseOwner() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "lease-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("lease-%d", time.Now().UnixNano())
}
