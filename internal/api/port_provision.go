package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/pvnstack/proxmox-ovn/internal/controlstore"
	"github.com/pvnstack/proxmox-ovn/internal/model"
)

type portProvisionRequest struct {
	ProjectID        string   `json:"project_id"`
	NetworkID        string   `json:"network_id"`
	SubnetID         string   `json:"subnet_id,omitempty"`
	Name             string   `json:"name,omitempty"`
	MACAddress       string   `json:"mac_address,omitempty"`
	FixedIPAddress   string   `json:"fixed_ip_address,omitempty"`
	SecurityGroupIDs []string `json:"security_group_ids,omitempty"`
}

type provisionIdentity struct {
	digest       [sha256.Size]byte
	portID       string
	allocationID string
}

type deprovisionReconcileError struct {
	resource model.Resource
	err      error
}

func (err *deprovisionReconcileError) Error() string { return err.err.Error() }
func (err *deprovisionReconcileError) Unwrap() error { return err.err }

func parsePortDeprovisionPath(path string) (string, bool) {
	remainder := strings.TrimPrefix(path, "/api/v1/ports/")
	if remainder == path {
		return "", false
	}
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "deprovision" {
		return "", false
	}
	return parts[0], true
}

func (s *Server) provisionPort(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	session, authenticated := request.Context().Value(sessionContextKey{}).(Session)
	if !authenticated || session.User == "" {
		writeError(writer, http.StatusUnauthorized, "unauthenticated", "a valid Proxmox session is required", nil)
		return
	}
	key, ok := idempotencyKey(writer, request)
	if !ok {
		return
	}

	var input portProvisionRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	if err := normalizeProvisionRequest(&input); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, "validation_error", err.Error(), err)
		return
	}
	identity, err := newProvisionIdentity(key, input)
	if err != nil {
		s.storeError(writer, err)
		return
	}

	port := provisionPortResource(identity, input)
	if err := port.Validate(); err != nil {
		s.storeError(writer, err)
		return
	}
	if err := s.authorizeWrite(request.Context(), port, nil); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	_, network, subnet, err := s.loadProvisionTopology(request.Context(), input)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if network.External || network.ProviderNetworkID != "" {
		writeError(writer, http.StatusConflict, "provider_network_port", "tenant VM ports cannot be provisioned on an external or provider-backed network", nil)
		return
	}
	operation, replayed, err := s.beginPortProvision(request.Context(), key, identity, port.ID)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if replayed && operation.OperationStatus == model.OperationSucceeded {
		current, getErr := s.store.Get(request.Context(), model.KindPort, port.ID)
		if getErr != nil {
			s.storeError(writer, getErr)
			return
		}
		s.writeProvisionedPort(writer, current.(*model.Port), true, false)
		return
	}

	var allocation *model.IPAllocation
	if subnet != nil {
		allocation, err = s.reserveProvisionAddress(request.Context(), input, identity, subnet, port.ID)
		if err != nil {
			s.failPortProvision(request.Context(), operation, err)
			s.storeError(writer, err)
			return
		}
		port.FixedIPs = []model.FixedIP{{SubnetID: subnet.ID, Address: allocation.Address}}
		if err := s.authorizeWrite(request.Context(), allocation, nil); err != nil {
			s.rollbackProvisionReservation(request.Context(), allocation, port.ID)
			s.failPortProvision(request.Context(), operation, err)
			writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
			return
		}
	}

	created, portReplayed, err := s.createProvisionPort(request.Context(), port)
	if err != nil {
		if rollbackProvisionError(err) {
			s.rollbackProvisionReservation(request.Context(), allocation, port.ID)
		}
		s.failPortProvision(request.Context(), operation, err)
		s.storeError(writer, err)
		return
	}
	if allocation != nil {
		if err := s.allocateProvisionAddress(request.Context(), allocation.ID, created); err != nil {
			s.failPortProvision(request.Context(), operation, err)
			s.storeError(writer, err)
			return
		}
	}
	if err := s.completePortProvision(request.Context(), operation); err != nil {
		s.storeError(writer, err)
		return
	}

	created = s.reconcileAndReload(request.Context(), created).(*model.Port)
	s.writeProvisionedPort(writer, created, replayed || portReplayed, !(replayed || portReplayed))
}

func normalizeProvisionRequest(input *portProvisionRequest) error {
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.NetworkID = strings.TrimSpace(input.NetworkID)
	input.SubnetID = strings.TrimSpace(input.SubnetID)
	input.Name = strings.TrimSpace(input.Name)
	input.MACAddress = strings.TrimSpace(input.MACAddress)
	input.FixedIPAddress = strings.TrimSpace(input.FixedIPAddress)
	if input.ProjectID == "" {
		return &model.ValidationError{Field: "project_id", Message: "is required"}
	}
	if input.NetworkID == "" {
		return &model.ValidationError{Field: "network_id", Message: "is required"}
	}
	if input.FixedIPAddress != "" && input.SubnetID == "" {
		return &model.ValidationError{Field: "subnet_id", Message: "is required when fixed_ip_address is set"}
	}
	if input.MACAddress != "" {
		mac, err := net.ParseMAC(input.MACAddress)
		if err != nil || len(mac) != 6 {
			return &model.ValidationError{Field: "mac_address", Message: "must be a 48-bit MAC address"}
		}
		input.MACAddress = strings.ToLower(mac.String())
	}
	if input.FixedIPAddress != "" {
		address, err := netip.ParseAddr(input.FixedIPAddress)
		if err != nil || !address.Is4() {
			return &model.ValidationError{Field: "fixed_ip_address", Message: "must be a valid IPv4 address"}
		}
		input.FixedIPAddress = address.String()
	}
	for index := range input.SecurityGroupIDs {
		input.SecurityGroupIDs[index] = strings.TrimSpace(input.SecurityGroupIDs[index])
		if input.SecurityGroupIDs[index] == "" {
			return &model.ValidationError{Field: fmt.Sprintf("security_group_ids[%d]", index), Message: "must not be empty"}
		}
	}
	sort.Strings(input.SecurityGroupIDs)
	for index := 1; index < len(input.SecurityGroupIDs); index++ {
		if input.SecurityGroupIDs[index] == input.SecurityGroupIDs[index-1] {
			return &model.ValidationError{Field: "security_group_ids", Message: "must not contain duplicates"}
		}
	}
	return nil
}

func newProvisionIdentity(key string, input portProvisionRequest) (provisionIdentity, error) {
	encoded, err := json.Marshal(struct {
		Key   string               `json:"idempotency_key"`
		Input portProvisionRequest `json:"request"`
	}{Key: key, Input: input})
	if err != nil {
		return provisionIdentity{}, err
	}
	digest := sha256.Sum256(encoded)
	return provisionIdentity{
		digest:       digest,
		portID:       deterministicProvisionID("port", digest),
		allocationID: deterministicProvisionID("allocation", digest),
	}, nil
}

func deterministicProvisionID(domain string, digest [sha256.Size]byte) string {
	value := sha256.Sum256(append([]byte("pvn-port-provision:"+domain+":"), digest[:]...))
	value[6] = (value[6] & 0x0f) | 0x50
	value[8] = (value[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(value[:16])
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
}

func deterministicProvisionMAC(digest [sha256.Size]byte) string {
	value := sha256.Sum256(append([]byte("pvn-port-provision:mac:"), digest[:]...))
	// 02 fixes both the locally-administered and unicast bits and makes the
	// generated address visually recognizable as a PVN-owned address.
	return net.HardwareAddr{0x02, value[0], value[1], value[2], value[3], value[4]}.String()
}

func provisionPortResource(identity provisionIdentity, input portProvisionRequest) *model.Port {
	name := input.Name
	if name == "" {
		name = "port-" + strings.ReplaceAll(identity.portID[:13], "-", "")
	}
	mac := input.MACAddress
	if mac == "" {
		mac = deterministicProvisionMAC(identity.digest)
	}
	port := &model.Port{
		Metadata:         model.Metadata{ID: identity.portID},
		ProjectID:        input.ProjectID,
		NetworkID:        input.NetworkID,
		Name:             name,
		MACAddress:       mac,
		SecurityGroupIDs: append([]string(nil), input.SecurityGroupIDs...),
		AdminStateUp:     true,
		BindingStatus:    model.PortUnbound,
		LSPName:          "pvn-" + identity.portID,
		Generation:       1,
	}
	return port
}

func (s *Server) loadProvisionTopology(ctx context.Context, input portProvisionRequest) (*model.Project, *model.Network, *model.Subnet, error) {
	projectResource, err := s.store.Get(ctx, model.KindProject, input.ProjectID)
	if err != nil {
		return nil, nil, nil, err
	}
	networkResource, err := s.store.Get(ctx, model.KindNetwork, input.NetworkID)
	if err != nil {
		return nil, nil, nil, err
	}
	project := projectResource.(*model.Project)
	network := networkResource.(*model.Network)
	if network.ProjectID != project.ID {
		return nil, nil, nil, &controlstore.Error{Kind: controlstore.ErrConflict, Message: "network belongs to a different project"}
	}
	if input.SubnetID == "" {
		return project, network, nil, nil
	}
	subnetResource, err := s.store.Get(ctx, model.KindSubnet, input.SubnetID)
	if err != nil {
		return nil, nil, nil, err
	}
	subnet := subnetResource.(*model.Subnet)
	if subnet.ProjectID != project.ID || subnet.NetworkID != network.ID {
		return nil, nil, nil, &controlstore.Error{Kind: controlstore.ErrConflict, Message: "subnet belongs to a different project or network"}
	}
	return project, network, subnet, nil
}

func (s *Server) beginPortProvision(ctx context.Context, key string, identity provisionIdentity, portID string) (*model.Operation, bool, error) {
	operation := &model.Operation{
		Metadata:        model.Metadata{ID: deterministicProvisionID("operation", identity.digest)},
		Action:          "port-provision:" + hex.EncodeToString(identity.digest[:]),
		TargetKind:      model.KindPort,
		TargetID:        portID,
		TargetRevision:  math.MaxInt64 - int64(binary.BigEndian.Uint64(identity.digest[:8])&((1<<62)-1)),
		OperationStatus: model.OperationQueued,
		IdempotencyKey:  key,
	}
	created, replayed, err := s.store.Create(ctx, operation, key)
	if err != nil {
		return nil, false, err
	}
	result := created.(*model.Operation)
	if replayed {
		latest, getErr := s.store.Get(ctx, model.KindOperation, result.ID)
		if getErr != nil {
			return nil, false, getErr
		}
		result = latest.(*model.Operation)
	}
	return result, replayed, nil
}

func (s *Server) reserveProvisionAddress(ctx context.Context, input portProvisionRequest, identity provisionIdentity, subnet *model.Subnet, portID string) (*model.IPAllocation, error) {
	if existing, err := s.store.Get(ctx, model.KindIPAllocation, identity.allocationID); err == nil {
		return validateProvisionAllocation(existing.(*model.IPAllocation), input, subnet, portID)
	} else if !errors.Is(err, controlstore.ErrNotFound) {
		return nil, err
	}

	used, err := s.usedProvisionAddresses(ctx, subnet.ID, portID)
	if err != nil {
		return nil, err
	}
	candidates, err := provisionAddressCandidates(subnet, input.FixedIPAddress)
	if err != nil {
		return nil, err
	}
	manual := input.FixedIPAddress != ""
	for candidates.next() {
		address := candidates.address()
		if used[address] {
			if manual {
				return nil, &controlstore.Error{Kind: controlstore.ErrAlreadyExists, Message: fmt.Sprintf("IP address %s is already allocated on subnet %q", address, subnet.ID)}
			}
			continue
		}
		allocation := &model.IPAllocation{
			Metadata:  model.Metadata{ID: identity.allocationID},
			ProjectID: input.ProjectID,
			SubnetID:  subnet.ID,
			Address:   address,
			State:     model.IPReserved,
		}
		created, _, createErr := s.store.Create(ctx, allocation, "")
		if createErr == nil {
			return created.(*model.IPAllocation), nil
		}
		if errors.Is(createErr, controlstore.ErrAlreadyExists) {
			if existing, getErr := s.store.Get(ctx, model.KindIPAllocation, identity.allocationID); getErr == nil {
				return validateProvisionAllocation(existing.(*model.IPAllocation), input, subnet, portID)
			}
			if manual {
				return nil, createErr
			}
			used[address] = true
			continue
		}
		return nil, createErr
	}
	return nil, &controlstore.Error{Kind: controlstore.ErrConflict, Message: fmt.Sprintf("subnet %q has no free IPv4 addresses", subnet.ID)}
}

func validateProvisionAllocation(allocation *model.IPAllocation, input portProvisionRequest, subnet *model.Subnet, portID string) (*model.IPAllocation, error) {
	if allocation.ProjectID != input.ProjectID || allocation.SubnetID != subnet.ID {
		return nil, &controlstore.Error{Kind: controlstore.ErrConflict, Message: "the provisioning reservation does not match this request"}
	}
	if input.FixedIPAddress != "" && allocation.Address != input.FixedIPAddress {
		return nil, &controlstore.Error{Kind: controlstore.ErrIdempotencyConflict, Message: "the provisioning reservation contains a different fixed IP address"}
	}
	if allocation.State == model.IPAllocated && allocation.PortID != portID {
		return nil, &controlstore.Error{Kind: controlstore.ErrConflict, Message: "the provisioning reservation belongs to another port"}
	}
	if allocation.State != model.IPReserved && allocation.State != model.IPAllocated {
		return nil, &controlstore.Error{Kind: controlstore.ErrConflict, Message: "the provisioning reservation has an invalid state"}
	}
	return allocation, nil
}

func (s *Server) usedProvisionAddresses(ctx context.Context, subnetID, ownPortID string) (map[string]bool, error) {
	used := make(map[string]bool)
	allocations, err := s.store.List(ctx, model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, resource := range allocations {
		allocation := resource.(*model.IPAllocation)
		if allocation.SubnetID == subnetID {
			used[allocation.Address] = true
		}
	}
	ports, err := s.store.List(ctx, model.KindPort, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, resource := range ports {
		port := resource.(*model.Port)
		if port.ID == ownPortID {
			continue
		}
		for _, fixed := range port.FixedIPs {
			if fixed.SubnetID == subnetID && fixed.Address != "" {
				used[fixed.Address] = true
			}
		}
	}
	return used, nil
}

type provisionCandidateIterator struct {
	ranges  [][2]uint32
	rangeAt int
	current uint32
	ready   bool
	subnet  *model.Subnet
}

func provisionAddressCandidates(subnet *model.Subnet, manual string) (*provisionCandidateIterator, error) {
	prefix, err := netip.ParsePrefix(subnet.CIDR)
	if err != nil || !prefix.Addr().Is4() {
		return nil, &model.ValidationError{Field: "subnet_id", Message: "references a subnet without a valid IPv4 CIDR"}
	}
	prefix = prefix.Masked()
	iterator := &provisionCandidateIterator{subnet: subnet, rangeAt: -1}
	if manual != "" {
		address, _ := netip.ParseAddr(manual)
		if !prefix.Contains(address) || !usableProvisionAddress(subnet, address) {
			return nil, &model.ValidationError{Field: "fixed_ip_address", Message: "must be a usable host address in the selected subnet"}
		}
		if len(subnet.AllocationPools) > 0 && !addressInProvisionPools(subnet.AllocationPools, address) {
			return nil, &model.ValidationError{Field: "fixed_ip_address", Message: "must belong to an allocation pool when the subnet defines pools"}
		}
		value := ipv4Uint32(address)
		iterator.ranges = append(iterator.ranges, [2]uint32{value, value})
		return iterator, nil
	}
	if len(subnet.AllocationPools) > 0 {
		for _, pool := range subnet.AllocationPools {
			start, _ := netip.ParseAddr(pool.Start)
			end, _ := netip.ParseAddr(pool.End)
			iterator.ranges = append(iterator.ranges, [2]uint32{ipv4Uint32(start), ipv4Uint32(end)})
		}
		return iterator, nil
	}
	network, broadcast := provisionSubnetBounds(prefix)
	if broadcast-network > 1 {
		iterator.ranges = append(iterator.ranges, [2]uint32{network + 1, broadcast - 1})
	}
	return iterator, nil
}

func (iterator *provisionCandidateIterator) next() bool {
	for {
		if !iterator.ready || iterator.current == iterator.ranges[iterator.rangeAt][1] {
			iterator.rangeAt++
			if iterator.rangeAt >= len(iterator.ranges) {
				return false
			}
			iterator.current = iterator.ranges[iterator.rangeAt][0]
			iterator.ready = true
		} else {
			iterator.current++
		}
		address := uint32IPv4(iterator.current)
		if usableProvisionAddress(iterator.subnet, address) {
			return true
		}
	}
}

func (iterator *provisionCandidateIterator) address() string {
	return uint32IPv4(iterator.current).String()
}

func usableProvisionAddress(subnet *model.Subnet, address netip.Addr) bool {
	prefix, err := netip.ParsePrefix(subnet.CIDR)
	if err != nil || !prefix.Addr().Is4() || !prefix.Contains(address) {
		return false
	}
	network, broadcast := provisionSubnetBounds(prefix.Masked())
	value := ipv4Uint32(address)
	gateway, gatewayErr := model.EffectiveIPv4Gateway(subnet)
	if value == network || value == broadcast || gatewayErr != nil || address == gateway {
		return false
	}
	return true
}

func addressInProvisionPools(pools []model.IPRange, address netip.Addr) bool {
	value := ipv4Uint32(address)
	for _, pool := range pools {
		start, startErr := netip.ParseAddr(pool.Start)
		end, endErr := netip.ParseAddr(pool.End)
		if startErr == nil && endErr == nil && value >= ipv4Uint32(start) && value <= ipv4Uint32(end) {
			return true
		}
	}
	return false
}

func provisionSubnetBounds(prefix netip.Prefix) (uint32, uint32) {
	network := ipv4Uint32(prefix.Masked().Addr())
	hostBits := 32 - prefix.Bits()
	if hostBits == 32 {
		return network, math.MaxUint32
	}
	return network, network | uint32((uint64(1)<<hostBits)-1)
}

func ipv4Uint32(address netip.Addr) uint32 {
	bytes := address.As4()
	return binary.BigEndian.Uint32(bytes[:])
}

func uint32IPv4(value uint32) netip.Addr {
	var bytes [4]byte
	binary.BigEndian.PutUint32(bytes[:], value)
	return netip.AddrFrom4(bytes)
}

func (s *Server) createProvisionPort(ctx context.Context, desired *model.Port) (*model.Port, bool, error) {
	if existing, err := s.store.Get(ctx, model.KindPort, desired.ID); err == nil {
		port := existing.(*model.Port)
		if !sameProvisionedPort(port, desired) {
			return nil, false, &controlstore.Error{Kind: controlstore.ErrConflict, Message: "the deterministic provisioning port ID contains different desired state"}
		}
		return port, true, nil
	} else if !errors.Is(err, controlstore.ErrNotFound) {
		return nil, false, err
	}
	created, _, err := s.store.Create(ctx, desired, "")
	if err == nil {
		return created.(*model.Port), false, nil
	}
	if errors.Is(err, controlstore.ErrAlreadyExists) {
		if existing, getErr := s.store.Get(ctx, model.KindPort, desired.ID); getErr == nil {
			port := existing.(*model.Port)
			if sameProvisionedPort(port, desired) {
				return port, true, nil
			}
		}
	}
	return nil, false, err
}

func sameProvisionedPort(current, desired *model.Port) bool {
	if current.ID != desired.ID || current.ProjectID != desired.ProjectID || current.NetworkID != desired.NetworkID ||
		current.Name != desired.Name || !strings.EqualFold(current.MACAddress, desired.MACAddress) ||
		!current.AdminStateUp || len(current.FixedIPs) != len(desired.FixedIPs) ||
		len(current.SecurityGroupIDs) != len(desired.SecurityGroupIDs) {
		return false
	}
	for index := range current.FixedIPs {
		if current.FixedIPs[index] != desired.FixedIPs[index] {
			return false
		}
	}
	for index := range current.SecurityGroupIDs {
		if current.SecurityGroupIDs[index] != desired.SecurityGroupIDs[index] {
			return false
		}
	}
	return true
}

func (s *Server) allocateProvisionAddress(ctx context.Context, allocationID string, port *model.Port) error {
	for attempt := 0; attempt < 8; attempt++ {
		resource, err := s.store.Get(ctx, model.KindIPAllocation, allocationID)
		if err != nil {
			return err
		}
		allocation := resource.(*model.IPAllocation)
		if allocation.State == model.IPAllocated {
			if allocation.PortID == port.ID {
				return nil
			}
			return &controlstore.Error{Kind: controlstore.ErrConflict, Message: "the reserved IP address was allocated to another port"}
		}
		if allocation.State != model.IPReserved || allocation.PortID != "" {
			return &controlstore.Error{Kind: controlstore.ErrConflict, Message: "the IP address is not in a reservable state"}
		}
		allocation.State = model.IPAllocated
		allocation.PortID = port.ID
		_, _, err = s.store.Update(ctx, allocation, allocation.Revision, "")
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return &controlstore.Error{Kind: controlstore.ErrConflict, Message: "IP allocation finalization could not be serialized"}
}

func rollbackProvisionError(err error) bool {
	var validation *model.ValidationError
	return errors.As(err, &validation) || errors.Is(err, controlstore.ErrAlreadyExists) || errors.Is(err, controlstore.ErrConflict)
}

func (s *Server) rollbackProvisionReservation(ctx context.Context, allocation *model.IPAllocation, portID string) {
	if allocation == nil {
		return
	}
	if _, err := s.store.Get(ctx, model.KindPort, portID); err == nil {
		return
	}
	current, err := s.store.Get(ctx, model.KindIPAllocation, allocation.ID)
	if err != nil {
		return
	}
	reserved := current.(*model.IPAllocation)
	if reserved.State != model.IPReserved || reserved.PortID != "" {
		return
	}
	_, _ = s.store.Delete(ctx, model.KindIPAllocation, reserved.ID, reserved.Revision, "")
}

func (s *Server) completePortProvision(ctx context.Context, operation *model.Operation) error {
	for attempt := 0; attempt < 8; attempt++ {
		resource, err := s.store.Get(ctx, model.KindOperation, operation.ID)
		if err != nil {
			return err
		}
		current := resource.(*model.Operation)
		if current.OperationStatus == model.OperationSucceeded {
			return nil
		}
		now := time.Now().UTC()
		current.OperationStatus = model.OperationSucceeded
		current.Error = ""
		current.CompletedAt = &now
		_, _, err = s.store.Update(ctx, current, current.Revision, "")
		if err == nil {
			return nil
		}
		if !errors.Is(err, controlstore.ErrPrecondition) {
			return err
		}
	}
	return &controlstore.Error{Kind: controlstore.ErrConflict, Message: "port provisioning completion could not be serialized"}
}

func (s *Server) failPortProvision(ctx context.Context, operation *model.Operation, provisionErr error) {
	resource, err := s.store.Get(ctx, model.KindOperation, operation.ID)
	if err != nil {
		return
	}
	current := resource.(*model.Operation)
	if current.OperationStatus == model.OperationSucceeded {
		return
	}
	now := time.Now().UTC()
	current.OperationStatus = model.OperationFailed
	current.Error = provisionErr.Error()
	current.CompletedAt = &now
	_, _, _ = s.store.Update(ctx, current, current.Revision, "")
}

func (s *Server) writeProvisionedPort(writer http.ResponseWriter, port *model.Port, replayed, created bool) {
	setETag(writer, port.Revision)
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	if created {
		writer.Header().Set("Location", "/api/v1/ports/"+port.ID)
		writeJSON(writer, http.StatusCreated, map[string]any{"data": port})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": port})
}

func (s *Server) deprovisionPort(writer http.ResponseWriter, request *http.Request, portID string) {
	if request.Method != http.MethodDelete {
		methodNotAllowed(writer, http.MethodDelete)
		return
	}
	session, authenticated := request.Context().Value(sessionContextKey{}).(Session)
	if !authenticated || session.User == "" {
		writeError(writer, http.StatusUnauthorized, "unauthenticated", "a valid Proxmox session is required", nil)
		return
	}
	key, ok := idempotencyKey(writer, request)
	if !ok {
		return
	}
	expected, err := expectedRevision(request, 0)
	if err != nil {
		writeError(writer, http.StatusPreconditionRequired, "precondition_required", err.Error(), nil)
		return
	}

	resource, err := s.store.Get(request.Context(), model.KindPort, portID)
	if errors.Is(err, controlstore.ErrNotFound) {
		s.replayPortDeprovision(writer, request, portID, expected, key)
		return
	}
	if err != nil {
		s.storeError(writer, err)
		return
	}
	port := resource.(*model.Port)
	if err := s.authorizeWrite(request.Context(), port, port); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	if !portCanBeDeprovisioned(port) {
		writeError(writer, http.StatusConflict, "port_attached", "the port must be unbound and unattached before deprovisioning", map[string]any{"binding_status": port.BindingStatus})
		return
	}
	deletionReplay := (port.State == model.ResourceDeleting || port.State == model.ResourceError) && expected < math.MaxInt64 && port.Revision == expected+1
	if port.Revision != expected && !deletionReplay {
		s.storeError(writer, &controlstore.Error{Kind: controlstore.ErrPrecondition, Message: fmt.Sprintf("expected revision %d but current revision is %d", expected, port.Revision)})
		return
	}
	if err := s.ensureNoPortDeprovisionDependents(request.Context(), port.ID); err != nil {
		s.storeError(writer, err)
		return
	}
	if err := s.releasePortAllocations(request.Context(), port, key); err != nil {
		var reconcileErr *deprovisionReconcileError
		if errors.As(err, &reconcileErr) {
			s.logger.Error("port allocation deprovision reconciliation failed", "kind", reconcileErr.resource.ResourceKind(), "id", reconcileErr.resource.GetMetadata().ID, "error", reconcileErr.err)
			writeError(writer, http.StatusServiceUnavailable, "reconcile_failed", reconcileErr.err.Error(), nil)
			return
		}
		s.storeError(writer, err)
		return
	}

	tombstone, replayed, err := s.store.BeginDelete(request.Context(), model.KindPort, port.ID, expected, key)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if err := s.authorizeWrite(request.Context(), tombstone, tombstone); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	if err := s.reconcileDeprovisionDelete(request.Context(), tombstone); err != nil {
		s.logger.Error("port deprovision reconciliation failed", "port_id", port.ID, "revision", tombstone.GetMetadata().Revision, "error", err)
		writeError(writer, http.StatusServiceUnavailable, "reconcile_failed", err.Error(), nil)
		return
	}
	if err := s.store.Purge(request.Context(), model.KindPort, port.ID, tombstone.GetMetadata().Revision); err != nil {
		s.storeError(writer, err)
		return
	}
	writeDeprovisioned(writer, replayed)
}

func (s *Server) replayPortDeprovision(writer http.ResponseWriter, request *http.Request, portID string, expected int64, key string) {
	tombstone, replayed, err := s.store.BeginDelete(request.Context(), model.KindPort, portID, expected, key)
	if err != nil {
		s.storeError(writer, err)
		return
	}
	if err := s.authorizeWrite(request.Context(), tombstone, tombstone); err != nil {
		writeError(writer, http.StatusForbidden, "forbidden", err.Error(), nil)
		return
	}
	if !replayed {
		// A missing live row can only produce a durable delete replay.
		writeError(writer, http.StatusConflict, "conflict", "port deletion has no replay record", nil)
		return
	}
	writeDeprovisioned(writer, true)
}

func portCanBeDeprovisioned(port *model.Port) bool {
	return port.BindingStatus == model.PortUnbound && port.NodeID == "" && port.VMID == 0 && port.NIC == "" && port.RequestedChassis == ""
}

func (s *Server) ensureNoPortDeprovisionDependents(ctx context.Context, portID string) error {
	checks := []model.Kind{model.KindRouterInterface, model.KindFloatingIP}
	for _, kind := range checks {
		resources, err := s.store.List(ctx, kind, controlstore.ListOptions{})
		if err != nil {
			return err
		}
		for _, resource := range resources {
			referencesPort := false
			switch value := resource.(type) {
			case *model.RouterInterface:
				referencesPort = value.PortID == portID
			case *model.FloatingIP:
				referencesPort = value.PortID == portID
			}
			if referencesPort {
				return &controlstore.Error{Kind: controlstore.ErrConflict, Message: fmt.Sprintf("port %q is still referenced by %s %q", portID, kind, resource.GetMetadata().ID)}
			}
		}
	}
	return nil
}

func (s *Server) releasePortAllocations(ctx context.Context, port *model.Port, key string) error {
	resources, err := s.store.List(ctx, model.KindIPAllocation, controlstore.ListOptions{})
	if err != nil {
		return err
	}
	for _, resource := range resources {
		allocation := resource.(*model.IPAllocation)
		if allocation.PortID != port.ID {
			continue
		}
		expected := allocation.Revision
		if allocation.Metadata.State == model.ResourceDeleting || allocation.Metadata.State == model.ResourceError {
			if expected < 2 {
				return &controlstore.Error{Kind: controlstore.ErrConflict, Message: "IP allocation deletion has an invalid revision"}
			}
			expected--
		}
		deleteKey := deprovisionAllocationKey(key, port.ID, allocation.ID)
		tombstone, _, err := s.store.BeginDelete(ctx, model.KindIPAllocation, allocation.ID, expected, deleteKey)
		if err != nil {
			return err
		}
		if err := s.reconcileDeprovisionDelete(ctx, tombstone); err != nil {
			return &deprovisionReconcileError{resource: tombstone, err: fmt.Errorf("delete IP allocation %q from realized state: %w", allocation.ID, err)}
		}
		if err := s.store.Purge(ctx, model.KindIPAllocation, allocation.ID, tombstone.GetMetadata().Revision); err != nil {
			return err
		}
	}
	return nil
}

func deprovisionAllocationKey(key, portID, allocationID string) string {
	digest := sha256.Sum256([]byte("pvn-port-deprovision:" + key + ":" + portID + ":" + allocationID))
	return "port-deprovision-allocation:" + hex.EncodeToString(digest[:])
}

func (s *Server) reconcileDeprovisionDelete(ctx context.Context, tombstone model.Resource) error {
	deletionReconciler, ok := s.reconciler.(DeletionReconciler)
	if !ok {
		return nil
	}
	return deletionReconciler.Delete(ctx, tombstone)
}

func writeDeprovisioned(writer http.ResponseWriter, replayed bool) {
	if replayed {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Del("Content-Type")
	writer.WriteHeader(http.StatusNoContent)
}
