package api

import (
	"fmt"

	"github.com/pvnstack/proxmox-ovn/internal/model"
)

func (s *Server) applyNetworkPolicy(resource model.Resource) error {
	switch value := resource.(type) {
	case *model.Network:
		if value.ProviderNetworkID != "" {
			return nil
		}
		if value.MTU == 0 {
			value.MTU = s.guestMTU
		}
		if value.MTU > s.guestMTU {
			return &model.ValidationError{Field: "mtu", Message: fmt.Sprintf("must not exceed configured overlay guest MTU %d", s.guestMTU)}
		}
	case *model.ProviderSegment:
		if s.physnet == "" {
			return nil
		}
		if value.PhysicalNetwork == "" {
			value.PhysicalNetwork = s.physnet
		}
		if value.PhysicalNetwork != s.physnet {
			return &model.ValidationError{Field: "physical_network", Message: fmt.Sprintf("must equal configured physnet %q", s.physnet)}
		}
	}
	return nil
}
