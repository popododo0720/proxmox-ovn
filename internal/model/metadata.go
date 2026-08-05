package model

import "time"

type ResourceState string

const (
	ResourcePending  ResourceState = "pending"
	ResourceReady    ResourceState = "ready"
	ResourceError    ResourceState = "error"
	ResourceDeleting ResourceState = "deleting"
)

// Metadata contains server-managed resource fields. Revision changes only when
// desired state changes; AppliedRevision records the last rendered revision.
type Metadata struct {
	ID              string        `json:"id"`
	Revision        int64         `json:"revision"`
	AppliedRevision int64         `json:"applied_revision"`
	State           ResourceState `json:"state"`
	LastError       string        `json:"last_error,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

type Resource interface {
	ResourceKind() Kind
	GetMetadata() *Metadata
	Validate() error
}

type NamedResource interface {
	Resource
	ResourceName() string
}

func (m *Metadata) GetMetadata() *Metadata { return m }
