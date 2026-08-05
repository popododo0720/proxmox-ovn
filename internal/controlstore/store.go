package controlstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/pvnstack/proxmox-ovn/internal/model"
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
}

type Store interface {
	Create(context.Context, model.Resource, string) (model.Resource, bool, error)
	Get(context.Context, model.Kind, string) (model.Resource, error)
	List(context.Context, model.Kind, ListOptions) ([]model.Resource, error)
	Update(context.Context, model.Resource, int64, string) (model.Resource, bool, error)
	BeginDelete(context.Context, model.Kind, string, int64, string) (model.Resource, bool, error)
	Purge(context.Context, model.Kind, string, int64) error
	Delete(context.Context, model.Kind, string, int64, string) (bool, error)
	MarkReconciled(context.Context, model.Kind, string, int64, error) (model.Resource, error)
}
