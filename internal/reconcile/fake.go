package reconcile

import (
	"context"
	"fmt"
	"sync"

	"github.com/popododo0720/proxmox-ovn/internal/model"
)

// FakeRenderer is an in-memory, revision-aware renderer suitable for tests and
// for manager development before the OVN northbound renderer is configured.
type FakeRenderer struct {
	mu             sync.Mutex
	rendered       map[string]model.Resource
	calls          map[string]int
	failures       map[string]error
	deleteFailures map[string]error
	deleteCalls    map[string]int
}

func NewFakeRenderer() *FakeRenderer {
	return &FakeRenderer{
		rendered:       make(map[string]model.Resource),
		calls:          make(map[string]int),
		failures:       make(map[string]error),
		deleteFailures: make(map[string]error),
		deleteCalls:    make(map[string]int),
	}
}

func (f *FakeRenderer) Delete(ctx context.Context, resource model.Resource) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if resource == nil {
		return fmt.Errorf("resource is nil")
	}
	key := rendererKey(resource.ResourceKind(), resource.GetMetadata().ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.deleteFailures[key]; err != nil {
		f.deleteCalls[key]++
		return err
	}
	delete(f.rendered, key)
	f.deleteCalls[key]++
	return nil
}

func (f *FakeRenderer) SetDeleteFailure(kind model.Kind, id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := rendererKey(kind, id)
	if err == nil {
		delete(f.deleteFailures, key)
	} else {
		f.deleteFailures[key] = err
	}
}

func (f *FakeRenderer) DeleteCalls(kind model.Kind, id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteCalls[rendererKey(kind, id)]
}

func (f *FakeRenderer) Render(ctx context.Context, resource model.Resource) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if resource == nil {
		return fmt.Errorf("resource is nil")
	}
	key := rendererKey(resource.ResourceKind(), resource.GetMetadata().ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures[key]; err != nil {
		f.calls[key]++
		return err
	}
	if previous := f.rendered[key]; previous != nil && previous.GetMetadata().Revision >= resource.GetMetadata().Revision {
		return nil
	}
	copyResource, err := model.Clone(resource)
	if err != nil {
		return err
	}
	f.rendered[key] = copyResource
	f.calls[key]++
	return nil
}

func (f *FakeRenderer) SetFailure(kind model.Kind, id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := rendererKey(kind, id)
	if err == nil {
		delete(f.failures, key)
	} else {
		f.failures[key] = err
	}
}

func (f *FakeRenderer) Calls(kind model.Kind, id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[rendererKey(kind, id)]
}

func (f *FakeRenderer) Rendered(kind model.Kind, id string) (model.Resource, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resource, ok := f.rendered[rendererKey(kind, id)]
	if !ok {
		return nil, false
	}
	copyResource, err := model.Clone(resource)
	return copyResource, err == nil
}

func rendererKey(kind model.Kind, id string) string { return kind.String() + "/" + id }
