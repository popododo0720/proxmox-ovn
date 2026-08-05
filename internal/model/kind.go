package model

import (
	"fmt"
	"sort"
	"strings"
)

// Kind identifies a PVN control-plane resource type.
type Kind string

const (
	KindProject           Kind = "project"
	KindNetwork           Kind = "network"
	KindSubnet            Kind = "subnet"
	KindPort              Kind = "port"
	KindIPAllocation      Kind = "ip-allocation"
	KindRouter            Kind = "router"
	KindRouterInterface   Kind = "router-interface"
	KindFloatingIP        Kind = "floating-ip"
	KindProviderNetwork   Kind = "provider-network"
	KindProviderSegment   Kind = "provider-segment"
	KindSecurityGroup     Kind = "security-group"
	KindSecurityGroupRule Kind = "security-group-rule"
	KindNode              Kind = "node"
	KindOperation         Kind = "operation"
)

var kindToCollection = map[Kind]string{
	KindProject:           "projects",
	KindNetwork:           "networks",
	KindSubnet:            "subnets",
	KindPort:              "ports",
	KindIPAllocation:      "ip-allocations",
	KindRouter:            "routers",
	KindRouterInterface:   "router-interfaces",
	KindFloatingIP:        "floating-ips",
	KindProviderNetwork:   "provider-networks",
	KindProviderSegment:   "provider-segments",
	KindSecurityGroup:     "security-groups",
	KindSecurityGroupRule: "security-group-rules",
	KindNode:              "nodes",
	KindOperation:         "operations",
}

var collectionToKind = func() map[string]Kind {
	result := make(map[string]Kind, len(kindToCollection))
	for kind, collection := range kindToCollection {
		result[collection] = kind
	}
	return result
}()

func (k Kind) String() string { return string(k) }

func (k Kind) Collection() string { return kindToCollection[k] }

func (k Kind) Valid() bool {
	_, ok := kindToCollection[k]
	return ok
}

func ParseCollection(value string) (Kind, error) {
	kind, ok := collectionToKind[strings.ToLower(value)]
	if !ok {
		return "", fmt.Errorf("unknown resource collection %q", value)
	}
	return kind, nil
}

func Kinds() []Kind {
	result := make([]Kind, 0, len(kindToCollection))
	for kind := range kindToCollection {
		result = append(result, kind)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
