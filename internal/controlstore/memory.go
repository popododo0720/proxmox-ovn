package controlstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/model"
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
	meta := copyResource.GetMetadata()
	if meta.ID == "" {
		meta.ID = s.newID()
	}
	if _, exists := s.resources[copyResource.ResourceKind()][meta.ID]; exists {
		return nil, false, storeError(ErrAlreadyExists, "%s %q already exists", copyResource.ResourceKind(), meta.ID)
	}
	model.SetDefaults(copyResource)
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
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if !kind.Valid() {
		return nil, storeError(ErrNotFound, "unknown resource kind %q", kind)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.resources[kind]))
	for id, resource := range s.resources[kind] {
		if matches(resource, options) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]model.Resource, 0, len(ids))
	for _, id := range ids {
		copyResource, err := model.Clone(s.resources[kind][id])
		if err != nil {
			return nil, err
		}
		result = append(result, copyResource)
	}
	return result, nil
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
	if err := copyResource.Validate(); err != nil {
		return nil, false, err
	}
	if err := s.validateReferencesLocked(copyResource); err != nil {
		return nil, false, err
	}
	if err := s.validateUniqueLocked(copyResource, id); err != nil {
		return nil, false, err
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
		}
	} else {
		if revision > meta.AppliedRevision {
			meta.AppliedRevision = revision
		}
		if revision == meta.Revision {
			meta.State = model.ResourceReady
			meta.LastError = ""
			meta.UpdatedAt = s.now().UTC()
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
	case *model.Project, *model.ProviderNetwork, *model.Node, *model.Operation:
		return nil
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
		if network.(*model.Network).ProjectID != value.ProjectID {
			return storeError(ErrConflict, "network belongs to a different project")
		}
		for _, fixed := range value.FixedIPs {
			subnet, subErr := s.requireLocked(model.KindSubnet, fixed.SubnetID, "fixed_ips.subnet_id")
			if subErr != nil {
				return subErr
			}
			if subnet.(*model.Subnet).NetworkID != value.NetworkID {
				return storeError(ErrConflict, "fixed IP subnet belongs to a different network")
			}
		}
		for _, groupID := range value.SecurityGroupIDs {
			if _, err := s.requireLocked(model.KindSecurityGroup, groupID, "security_group_ids"); err != nil {
				return err
			}
		}
	case *model.IPAllocation:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if _, err := s.requireLocked(model.KindSubnet, value.SubnetID, "subnet_id"); err != nil {
			return err
		}
		if value.PortID != "" {
			if _, err := s.requireLocked(model.KindPort, value.PortID, "port_id"); err != nil {
				return err
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
			subnetResource, err := s.requireLocked(model.KindSubnet, value.ExternalSubnetID, "external_subnet_id")
			if err != nil {
				return err
			}
			subnet := subnetResource.(*model.Subnet)
			if subnet.NetworkID != network.ID {
				return storeError(ErrConflict, "external subnet belongs to a different network")
			}
			prefix, prefixErr := netip.ParsePrefix(subnet.CIDR)
			address, addressErr := netip.ParseAddr(value.ExternalIPAddress)
			if prefixErr != nil || addressErr != nil || !prefix.Contains(address) {
				return storeError(ErrConflict, "external_ip_address must be inside the external subnet")
			}
			if subnet.GatewayIP != "" && address.String() == subnet.GatewayIP {
				return storeError(ErrConflict, "external_ip_address must not equal the external subnet gateway")
			}
		}
	case *model.RouterInterface:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if _, err := s.requireLocked(model.KindRouter, value.RouterID, "router_id"); err != nil {
			return err
		}
		if _, err := s.requireLocked(model.KindSubnet, value.SubnetID, "subnet_id"); err != nil {
			return err
		}
		if value.PortID != "" {
			if _, err := s.requireLocked(model.KindPort, value.PortID, "port_id"); err != nil {
				return err
			}
		}
	case *model.FloatingIP:
		if err := project(value.ProjectID, "project_id"); err != nil {
			return err
		}
		if _, err := s.requireLocked(model.KindProviderNetwork, value.ProviderNetworkID, "provider_network_id"); err != nil {
			return err
		}
		if value.PortID != "" {
			if _, err := s.requireLocked(model.KindPort, value.PortID, "port_id"); err != nil {
				return err
			}
		}
		if value.RouterID != "" {
			if _, err := s.requireLocked(model.KindRouter, value.RouterID, "router_id"); err != nil {
				return err
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
		if _, err := s.requireLocked(model.KindSecurityGroup, value.SecurityGroupID, "security_group_id"); err != nil {
			return err
		}
		if value.RemoteGroupID != "" {
			if _, err := s.requireLocked(model.KindSecurityGroup, value.RemoteGroupID, "remote_group_id"); err != nil {
				return err
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
	return nil
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
		if left.NetworkID == right.NetworkID && left.CIDR == right.CIDR {
			return "network_id,cidr"
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
	return ""
}

func references(resource model.Resource, kind model.Kind, id string) bool {
	switch value := resource.(type) {
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
