package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/controlstore"
	"github.com/pvnstack/proxmox-ovn/internal/model"
)

// Renderer materializes one immutable desired resource revision. Implementations
// must treat repeated calls for the same kind, id, and revision as idempotent.
type Renderer interface {
	Render(context.Context, model.Resource) error
	Delete(context.Context, model.Resource) error
}

type Controller struct {
	store    controlstore.Store
	renderer Renderer
	locksMu  sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewController(store controlstore.Store, renderer Renderer) *Controller {
	return &Controller{store: store, renderer: renderer, locks: make(map[string]*sync.Mutex)}
}

func (c *Controller) Reconcile(ctx context.Context, kind model.Kind, id string) error {
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
			return err
		}
		if err := c.store.Purge(ctx, kind, id, meta.Revision); err != nil && !errors.Is(err, controlstore.ErrNotFound) {
			return fmt.Errorf("purge deleted %s %q revision %d: %w", kind, id, meta.Revision, err)
		}
		return nil
	}
	defer lock.Unlock()
	if meta.AppliedRevision >= meta.Revision && meta.State == model.ResourceReady {
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
	if op.OperationStatus == model.OperationSucceeded {
		_, markErr := c.store.MarkReconciled(ctx, kind, id, meta.Revision, nil)
		return markErr
	}

	now := time.Now().UTC()
	op.OperationStatus = model.OperationRunning
	op.StartedAt = &now
	updated, _, updateErr := c.store.Update(ctx, op, op.Revision, "")
	if updateErr != nil {
		return fmt.Errorf("start reconcile operation: %w", updateErr)
	}
	op = updated.(*model.Operation)

	renderErr := c.renderer.Render(ctx, resource)
	staleDelete := false
	latest, latestErr := c.store.Get(ctx, kind, id)
	if errors.Is(latestErr, controlstore.ErrNotFound) || (latestErr == nil && latest.GetMetadata().State == model.ResourceDeleting) {
		// Render and delete are separate systems, so the desired row can be
		// deleted while an older manager is still writing OVN. An idempotent
		// cleanup after the render closes that race and prevents orphan rows.
		staleDelete = true
		renderErr = c.renderer.Delete(ctx, resource)
	} else if latestErr != nil && renderErr == nil {
		renderErr = fmt.Errorf("reload desired state after render: %w", latestErr)
	}
	var markErr error
	if !staleDelete {
		_, markErr = c.store.MarkReconciled(ctx, kind, id, meta.Revision, renderErr)
	}
	completed := time.Now().UTC()
	op.CompletedAt = &completed
	if renderErr != nil {
		op.OperationStatus = model.OperationFailed
		op.Error = renderErr.Error()
	} else if markErr != nil {
		op.OperationStatus = model.OperationFailed
		op.Error = markErr.Error()
	} else {
		op.OperationStatus = model.OperationSucceeded
		op.Error = ""
	}
	_, _, operationErr := c.store.Update(ctx, op, op.Revision, "")
	if renderErr != nil {
		return fmt.Errorf("render %s %q revision %d: %w", kind, id, meta.Revision, renderErr)
	}
	if markErr != nil {
		return fmt.Errorf("mark %s %q reconciled: %w", kind, id, markErr)
	}
	if operationErr != nil {
		return fmt.Errorf("complete reconcile operation: %w", operationErr)
	}
	return nil
}

func (c *Controller) ReconcileAll(ctx context.Context) error {
	var failures []error
	for _, kind := range dependencyOrder {
		resources, err := c.store.List(ctx, kind, controlstore.ListOptions{})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, resource := range resources {
			if err := c.Reconcile(ctx, kind, resource.GetMetadata().ID); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
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
		if op.OperationStatus == model.OperationSucceeded {
			return nil
		}
	}
	now := time.Now().UTC()
	op.OperationStatus, op.StartedAt, op.CompletedAt, op.Error = model.OperationRunning, &now, nil, ""
	updated, _, err := c.store.Update(ctx, op, op.Revision, "")
	if err != nil {
		return fmt.Errorf("start delete operation: %w", err)
	}
	op = updated.(*model.Operation)
	renderErr := c.renderer.Delete(ctx, resource)
	completed := time.Now().UTC()
	op.CompletedAt = &completed
	if renderErr != nil {
		op.OperationStatus, op.Error = model.OperationFailed, renderErr.Error()
		_, _ = c.store.MarkReconciled(ctx, kind, id, revision, renderErr)
	} else {
		op.OperationStatus, op.Error = model.OperationSucceeded, ""
	}
	_, _, operationErr := c.store.Update(ctx, op, op.Revision, "")
	if renderErr != nil {
		return fmt.Errorf("delete rendered %s %q revision %d: %w", kind, id, revision, renderErr)
	}
	if operationErr != nil {
		return fmt.Errorf("complete delete operation: %w", operationErr)
	}
	return nil
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
