package pve

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNICAlreadyExists = errors.New("VM NIC already exists")
	ErrNICNotFound      = errors.New("VM NIC does not exist")
)

type HotplugState string

const (
	StatePreparingPort  HotplugState = "preparing-port"
	StateStagingNIC     HotplugState = "staging-nic-link-down"
	StateWaitingBinding HotplugState = "waiting-for-binding"
	StateEnablingNIC    HotplugState = "enabling-nic"
	StateAttached       HotplugState = "attached"
	StateDisablingNIC   HotplugState = "disabling-nic"
	StateDisablingPort  HotplugState = "disabling-port"
	StateDeletingNIC    HotplugState = "deleting-nic"
	StateReleasingPort  HotplugState = "releasing-port"
	StateDetached       HotplugState = "detached"
	StateRollingBack    HotplugState = "rolling-back"
)

type Attachment struct {
	Node      string
	VMID      int
	NICIndex  int
	Property  NetProperty
	PVNPortID string
}

// BindingLifecycle connects the PVE config transaction to the manager-side
// logical port lifecycle. Implementations must make these methods idempotent;
// rollback deliberately retries them after uncertain failures.
type BindingLifecycle interface {
	Prepare(context.Context, Attachment) error
	WaitBound(context.Context, Attachment) error
	Disable(context.Context, Attachment) error
	Release(context.Context, Attachment) error
}

type StateObserver func(Attachment, HotplugState)

type Hotplugger struct {
	PVE      VMNetworkClient
	Bindings BindingLifecycle
	Observe  StateObserver
}

type HotplugError struct {
	Stage       HotplugState
	Err         error
	RollbackErr error
}

func (err *HotplugError) Error() string {
	if err.RollbackErr == nil {
		return fmt.Sprintf("hotplug failed in state %s: %v", err.Stage, err.Err)
	}
	return fmt.Sprintf("hotplug failed in state %s: %v (rollback also failed: %v)", err.Stage, err.Err, err.RollbackErr)
}

func (err *HotplugError) Unwrap() error { return err.Err }

func (hotplugger *Hotplugger) Attach(ctx context.Context, attachment Attachment) error {
	if err := hotplugger.validate(); err != nil {
		return err
	}
	if err := validateAttachment(attachment); err != nil {
		return err
	}
	config, err := hotplugger.PVE.GetVMConfig(ctx, attachment.Node, attachment.VMID)
	if err != nil {
		return &HotplugError{Stage: StatePreparingPort, Err: err}
	}
	if _, exists := config.Networks[attachment.NICIndex]; exists {
		return &HotplugError{Stage: StatePreparingPort, Err: ErrNICAlreadyExists}
	}

	hotplugger.observe(attachment, StatePreparingPort)
	if err := hotplugger.Bindings.Prepare(ctx, attachment); err != nil {
		return &HotplugError{Stage: StatePreparingPort, Err: err}
	}

	staged := attachment.Property.Clone()
	if err := staged.SetLinkDown(true); err != nil {
		rollbackErr := hotplugger.Bindings.Release(ctx, attachment)
		return &HotplugError{Stage: StateStagingNIC, Err: err, RollbackErr: rollbackErr}
	}
	hotplugger.observe(attachment, StateStagingNIC)
	upid, err := hotplugger.PVE.SetVMNetwork(ctx, attachment.Node, attachment.VMID, attachment.NICIndex, staged, config.Digest)
	if err == nil {
		err = hotplugger.PVE.WaitUPID(ctx, attachment.Node, upid)
	}
	if err != nil {
		return hotplugger.attachFailure(ctx, attachment, StateStagingNIC, err)
	}

	hotplugger.observe(attachment, StateWaitingBinding)
	if err := hotplugger.Bindings.WaitBound(ctx, attachment); err != nil {
		return hotplugger.attachFailure(ctx, attachment, StateWaitingBinding, err)
	}

	hotplugger.observe(attachment, StateEnablingNIC)
	config, err = hotplugger.PVE.GetVMConfig(ctx, attachment.Node, attachment.VMID)
	if err != nil {
		return hotplugger.attachFailure(ctx, attachment, StateEnablingNIC, err)
	}
	current, exists := config.Networks[attachment.NICIndex]
	if !exists {
		return hotplugger.attachFailure(ctx, attachment, StateEnablingNIC, ErrNICNotFound)
	}
	if err := current.SetLinkDown(false); err != nil {
		return hotplugger.attachFailure(ctx, attachment, StateEnablingNIC, err)
	}
	upid, err = hotplugger.PVE.SetVMNetwork(ctx, attachment.Node, attachment.VMID, attachment.NICIndex, current, config.Digest)
	if err == nil {
		err = hotplugger.PVE.WaitUPID(ctx, attachment.Node, upid)
	}
	if err != nil {
		return hotplugger.attachFailure(ctx, attachment, StateEnablingNIC, err)
	}
	hotplugger.observe(attachment, StateAttached)
	return nil
}

func (hotplugger *Hotplugger) Detach(ctx context.Context, attachment Attachment) error {
	if err := hotplugger.validate(); err != nil {
		return err
	}
	if err := validateAttachment(attachment); err != nil {
		return err
	}
	config, err := hotplugger.PVE.GetVMConfig(ctx, attachment.Node, attachment.VMID)
	if err != nil {
		return &HotplugError{Stage: StateDisablingNIC, Err: err}
	}
	original, exists := config.Networks[attachment.NICIndex]
	if !exists {
		return &HotplugError{Stage: StateDisablingNIC, Err: ErrNICNotFound}
	}
	attachment.Property = original.Clone()
	staged := original.Clone()
	if err := staged.SetLinkDown(true); err != nil {
		return &HotplugError{Stage: StateDisablingNIC, Err: err}
	}

	hotplugger.observe(attachment, StateDisablingNIC)
	upid, err := hotplugger.PVE.SetVMNetwork(ctx, attachment.Node, attachment.VMID, attachment.NICIndex, staged, config.Digest)
	if err == nil {
		err = hotplugger.PVE.WaitUPID(ctx, attachment.Node, upid)
	}
	if err != nil {
		return hotplugger.detachFailure(ctx, attachment, StateDisablingNIC, err)
	}

	hotplugger.observe(attachment, StateDisablingPort)
	if err := hotplugger.Bindings.Disable(ctx, attachment); err != nil {
		return hotplugger.detachFailure(ctx, attachment, StateDisablingPort, err)
	}

	hotplugger.observe(attachment, StateDeletingNIC)
	config, err = hotplugger.PVE.GetVMConfig(ctx, attachment.Node, attachment.VMID)
	if err != nil {
		return hotplugger.detachFailure(ctx, attachment, StateDeletingNIC, err)
	}
	upid, err = hotplugger.PVE.DeleteVMNetwork(ctx, attachment.Node, attachment.VMID, attachment.NICIndex, config.Digest)
	if err == nil {
		err = hotplugger.PVE.WaitUPID(ctx, attachment.Node, upid)
	}
	if err != nil {
		return hotplugger.detachFailure(ctx, attachment, StateDeletingNIC, err)
	}

	hotplugger.observe(attachment, StateReleasingPort)
	if err := hotplugger.Bindings.Release(ctx, attachment); err != nil {
		return hotplugger.detachFailure(ctx, attachment, StateReleasingPort, err)
	}
	hotplugger.observe(attachment, StateDetached)
	return nil
}

func (hotplugger *Hotplugger) attachFailure(ctx context.Context, attachment Attachment, state HotplugState, cause error) error {
	hotplugger.observe(attachment, StateRollingBack)
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var rollbackErrors []error
	config, err := hotplugger.PVE.GetVMConfig(rollbackCtx, attachment.Node, attachment.VMID)
	if err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("read VM config: %w", err))
	} else if _, exists := config.Networks[attachment.NICIndex]; exists {
		upid, deleteErr := hotplugger.PVE.DeleteVMNetwork(rollbackCtx, attachment.Node, attachment.VMID, attachment.NICIndex, config.Digest)
		if deleteErr == nil {
			deleteErr = hotplugger.PVE.WaitUPID(rollbackCtx, attachment.Node, upid)
		}
		if deleteErr != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove staged NIC: %w", deleteErr))
		}
	}
	if err := hotplugger.Bindings.Release(rollbackCtx, attachment); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("release logical port: %w", err))
	}
	return &HotplugError{Stage: state, Err: cause, RollbackErr: errors.Join(rollbackErrors...)}
}

func (hotplugger *Hotplugger) detachFailure(ctx context.Context, attachment Attachment, state HotplugState, cause error) error {
	hotplugger.observe(attachment, StateRollingBack)
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var rollbackErrors []error
	config, err := hotplugger.PVE.GetVMConfig(rollbackCtx, attachment.Node, attachment.VMID)
	if err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("read VM config: %w", err))
	} else {
		upid, restoreErr := hotplugger.PVE.SetVMNetwork(rollbackCtx, attachment.Node, attachment.VMID, attachment.NICIndex, attachment.Property, config.Digest)
		if restoreErr == nil {
			restoreErr = hotplugger.PVE.WaitUPID(rollbackCtx, attachment.Node, upid)
		}
		if restoreErr != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore VM NIC: %w", restoreErr))
		}
	}
	if err := hotplugger.Bindings.Prepare(rollbackCtx, attachment); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore logical port: %w", err))
	} else if err := hotplugger.Bindings.WaitBound(rollbackCtx, attachment); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("wait for restored binding: %w", err))
	}
	return &HotplugError{Stage: state, Err: cause, RollbackErr: errors.Join(rollbackErrors...)}
}

func (hotplugger *Hotplugger) validate() error {
	if hotplugger.PVE == nil || hotplugger.Bindings == nil {
		return errors.New("PVE client and binding lifecycle are required")
	}
	return nil
}

func validateAttachment(attachment Attachment) error {
	if err := validateNodeAndVM(attachment.Node, attachment.VMID); err != nil {
		return err
	}
	if attachment.NICIndex < 0 {
		return fmt.Errorf("invalid NIC index %d", attachment.NICIndex)
	}
	return nil
}

func (hotplugger *Hotplugger) observe(attachment Attachment, state HotplugState) {
	if hotplugger.Observe != nil {
		hotplugger.Observe(attachment, state)
	}
}
