package api

import (
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

// managedSecurityGroupView adds computed API-only metadata without persisting
// it in PVN_Control or accepting it in resource create/update payloads.
type managedSecurityGroupView struct {
	*model.SecurityGroup
	Managed  bool `json:"managed"`
	ReadOnly bool `json:"read_only"`
}

type managedSecurityGroupRuleView struct {
	*model.SecurityGroupRule
	Managed  bool `json:"managed"`
	ReadOnly bool `json:"read_only"`
}

func resourceAPIView(resource model.Resource) any {
	switch value := resource.(type) {
	case *model.SecurityGroup:
		managed := defaultsecurity.IsReserved(value)
		return managedSecurityGroupView{SecurityGroup: value, Managed: managed, ReadOnly: managed}
	case *model.SecurityGroupRule:
		managed := defaultsecurity.IsReserved(value)
		return managedSecurityGroupRuleView{SecurityGroupRule: value, Managed: managed, ReadOnly: managed}
	default:
		return resource
	}
}
