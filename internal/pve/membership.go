package pve

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

const DefaultMembershipFile = "/etc/pve/.members"

// ClusterMembership is the local pmxcfs view of the PVE cluster. PVN uses
// only online node names and quorum state; addresses from .members are never
// trusted as network configuration.
type ClusterMembership struct {
	Reporter    string
	OnlineNodes []string
	Quorate     bool
}

type membershipFile struct {
	NodeName string `json:"nodename"`
	Cluster  struct {
		Quorate int `json:"quorate"`
	} `json:"cluster"`
	NodeList map[string]struct {
		Online int `json:"online"`
	} `json:"nodelist"`
}

// ReadClusterMembership reads the pmxcfs-generated .members snapshot.
func ReadClusterMembership(path string) (ClusterMembership, error) {
	if strings.TrimSpace(path) == "" {
		return ClusterMembership{}, fmt.Errorf("PVE membership file is required")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return ClusterMembership{}, fmt.Errorf("read PVE membership %q: %w", path, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return ClusterMembership{}, fmt.Errorf("decode PVE membership %q: %w", path, err)
	}
	if fields == nil {
		return ClusterMembership{}, fmt.Errorf("PVE membership %q must contain a JSON object", path)
	}
	_, hasCluster := fields["cluster"]
	_, hasNodeList := fields["nodelist"]
	if !hasCluster && !hasNodeList {
		return readStandaloneMembership(path, fields)
	}

	var document membershipFile
	if err := json.Unmarshal(payload, &document); err != nil {
		return ClusterMembership{}, fmt.Errorf("decode PVE membership %q: %w", path, err)
	}
	document.NodeName = strings.TrimSpace(document.NodeName)
	if document.NodeName == "" {
		return ClusterMembership{}, fmt.Errorf("PVE membership %q has no local nodename", path)
	}
	online := make([]string, 0, len(document.NodeList))
	for rawName, node := range document.NodeList {
		name := strings.TrimSpace(rawName)
		if name == "" {
			return ClusterMembership{}, fmt.Errorf("PVE membership %q contains an empty node name", path)
		}
		if node.Online != 0 {
			online = append(online, name)
		}
	}
	sort.Strings(online)
	if len(online) == 0 {
		return ClusterMembership{}, fmt.Errorf("PVE membership %q reports no online nodes", path)
	}
	return ClusterMembership{Reporter: document.NodeName, OnlineNodes: online, Quorate: document.Cluster.Quorate != 0}, nil
}

func readStandaloneMembership(path string, fields map[string]json.RawMessage) (ClusterMembership, error) {
	if len(fields) != 2 || fields["nodename"] == nil || fields["version"] == nil {
		return ClusterMembership{}, fmt.Errorf("standalone PVE membership %q must contain exactly nodename and version", path)
	}
	var nodeName string
	if err := json.Unmarshal(fields["nodename"], &nodeName); err != nil {
		return ClusterMembership{}, fmt.Errorf("standalone PVE membership %q has an invalid nodename", path)
	}
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return ClusterMembership{}, fmt.Errorf("PVE membership %q has no local nodename", path)
	}
	var version int64
	if err := json.Unmarshal(fields["version"], &version); err != nil || version < 0 {
		return ClusterMembership{}, fmt.Errorf("standalone PVE membership %q has an invalid version", path)
	}
	return ClusterMembership{
		Reporter: nodeName, OnlineNodes: []string{nodeName}, Quorate: true,
	}, nil
}
