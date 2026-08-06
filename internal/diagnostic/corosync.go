package diagnostic

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const defaultCorosyncConfigPath = "/etc/pve/corosync.conf"

// CorosyncRuntimeCheckName is the stable doctor check name used by package
// installation and rolling-update safety gates.
const CorosyncRuntimeCheckName = "corosync-runtime-config"

const maxCorosyncConfigSize = 1024 * 1024

var (
	corosyncNodeStartPattern = regexp.MustCompile(`(?m)^[ \t]*node[ \t]*\{`)
	corosyncNodeBlockPattern = regexp.MustCompile(`(?ms)^[ \t]*node[ \t]*\{.*?^[ \t]*\}[ \t]*$`)
	corosyncVersionPattern   = regexp.MustCompile(`(?m)^[ \t]*config_version:[ \t]*(\d+)[ \t]*$`)
	corosyncNamePattern      = regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*(\S+)[ \t]*$`)
	corosyncNodeIDPattern    = regexp.MustCompile(`(?m)^[ \t]*nodeid:[ \t]*(\d+)[ \t]*$`)
	corosyncRingPattern      = regexp.MustCompile(`(?m)^[ \t]*ring(\d+)_addr:[ \t]*(\S+)[ \t]*$`)

	cmapLinePattern           = regexp.MustCompile(`^([A-Za-z0-9_.]+)[ \t]+\([^)]+\)[ \t]*=[ \t]*(.*)$`)
	cmapNodeKeyPattern        = regexp.MustCompile(`^nodelist\.node\.(\d+)\.(name|nodeid|ring(\d+)_addr)$`)
	cmapMemberKeyPattern      = regexp.MustCompile(`^runtime\.members\.(\d+)\.(config_version|ip|status)$`)
	cmapBindKeyPattern        = regexp.MustCompile(`^totem\.interface\.(\d+)\.bindnetaddr$`)
	cmapRuntimeAddressPattern = regexp.MustCompile(`r\((\d+)\)[ \t]+ip\(([^)]+)\)`)
)

type corosyncNode struct {
	Name  string
	ID    uint32
	Rings map[uint32]string
}

type corosyncConfigSnapshot struct {
	Version uint64
	Nodes   map[uint32]corosyncNode
}

type cmapNode struct {
	nameSet bool
	Name    string
	idSet   bool
	ID      uint32
	Rings   map[uint32]string
}

type cmapMember struct {
	statusSet  bool
	Status     string
	versionSet bool
	Version    uint64
	addressSet bool
	Addresses  map[uint32]string
}

type corosyncRuntimeSnapshot struct {
	versionSet       bool
	Version          uint64
	localPositionSet bool
	LocalPosition    uint32
	Nodes            map[uint32]*cmapNode
	Members          map[uint32]*cmapMember
	BindAddresses    map[uint32]string
}

func corosyncRuntimeCheck(ctx context.Context, runner Runner, path string) Check {
	const name = CorosyncRuntimeCheckName

	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Check{Name: name, Status: Pass, Message: "standalone node: corosync.conf is absent"}
	}
	if err != nil {
		return Check{Name: name, Status: Fail, Message: fmt.Sprintf("inspect %s: %v", path, err)}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Check{Name: name, Status: Fail, Message: path + " is not a regular non-symlink file"}
	}
	if info.Size() > maxCorosyncConfigSize {
		return Check{Name: name, Status: Fail, Message: fmt.Sprintf("%s exceeds %d bytes", path, maxCorosyncConfigSize)}
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return Check{Name: name, Status: Fail, Message: fmt.Sprintf("read %s: %v", path, err)}
	}
	persisted, err := parseCorosyncConfig(string(content))
	if err != nil {
		return Check{Name: name, Status: Fail, Message: fmt.Sprintf("parse %s: %v", path, err)}
	}

	output, err := runner.Run(ctx, "corosync-cmapctl")
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return Check{Name: name, Status: Fail, Message: "read live Corosync cmap: " + message}
	}
	runtime, err := parseCorosyncRuntime(string(output))
	if err != nil {
		return Check{Name: name, Status: Fail, Message: "parse live Corosync cmap: " + err.Error()}
	}
	if err := compareCorosyncSnapshots(persisted, runtime); err != nil {
		return Check{Name: name, Status: Fail, Message: err.Error()}
	}

	return Check{
		Name:    name,
		Status:  Pass,
		Message: fmt.Sprintf("config_version %d; %d configured nodes and joined runtime members match", persisted.Version, len(persisted.Nodes)),
	}
}

// CorosyncRuntimeCheck compares the persisted PVE Corosync configuration with
// the currently joined runtime without requiring PVN to be configured or
// active. Package orchestration uses it before any post-upgrade service
// restart and between rolling node mutations.
func CorosyncRuntimeCheck(ctx context.Context, runner Runner) Check {
	return corosyncRuntimeCheck(ctx, runner, defaultCorosyncConfigPath)
}

func parseCorosyncConfig(text string) (corosyncConfigSnapshot, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if len(text) > maxCorosyncConfigSize {
		return corosyncConfigSnapshot{}, fmt.Errorf("configuration exceeds %d bytes", maxCorosyncConfigSize)
	}

	versions := corosyncVersionPattern.FindAllStringSubmatch(text, -1)
	if len(versions) != 1 {
		return corosyncConfigSnapshot{}, fmt.Errorf("expected exactly one config_version, found %d", len(versions))
	}
	version, err := parseUint64(versions[0][1], "config_version")
	if err != nil {
		return corosyncConfigSnapshot{}, err
	}

	blocks := corosyncNodeBlockPattern.FindAllString(text, -1)
	starts := corosyncNodeStartPattern.FindAllStringIndex(text, -1)
	if len(blocks) == 0 || len(blocks) != len(starts) {
		return corosyncConfigSnapshot{}, fmt.Errorf("node blocks are missing or malformed")
	}

	result := corosyncConfigSnapshot{Version: version, Nodes: make(map[uint32]corosyncNode, len(blocks))}
	names := make(map[string]bool, len(blocks))
	for _, block := range blocks {
		nameMatches := corosyncNamePattern.FindAllStringSubmatch(block, -1)
		idMatches := corosyncNodeIDPattern.FindAllStringSubmatch(block, -1)
		if len(nameMatches) != 1 || len(idMatches) != 1 {
			return corosyncConfigSnapshot{}, fmt.Errorf("each node block must contain exactly one name and nodeid")
		}
		name := nameMatches[0][1]
		if names[name] {
			return corosyncConfigSnapshot{}, fmt.Errorf("duplicate node name %q", name)
		}
		names[name] = true
		nodeID, err := parseUint32(idMatches[0][1], "node "+name+" nodeid")
		if err != nil {
			return corosyncConfigSnapshot{}, err
		}
		if nodeID == 0 {
			return corosyncConfigSnapshot{}, fmt.Errorf("node %s has invalid nodeid 0", name)
		}
		if previous, exists := result.Nodes[nodeID]; exists {
			return corosyncConfigSnapshot{}, fmt.Errorf("nodes %s and %s share nodeid %d", previous.Name, name, nodeID)
		}

		rings := make(map[uint32]string)
		for _, match := range corosyncRingPattern.FindAllStringSubmatch(block, -1) {
			ring, err := parseUint32(match[1], "node "+name+" ring number")
			if err != nil {
				return corosyncConfigSnapshot{}, err
			}
			if _, exists := rings[ring]; exists {
				return corosyncConfigSnapshot{}, fmt.Errorf("node %s repeats ring%d_addr", name, ring)
			}
			rings[ring] = match[2]
		}
		if len(rings) == 0 {
			return corosyncConfigSnapshot{}, fmt.Errorf("node %s has no ring address", name)
		}
		result.Nodes[nodeID] = corosyncNode{Name: name, ID: nodeID, Rings: rings}
	}
	return result, nil
}

func parseCorosyncRuntime(text string) (corosyncRuntimeSnapshot, error) {
	result := corosyncRuntimeSnapshot{
		Nodes:         make(map[uint32]*cmapNode),
		Members:       make(map[uint32]*cmapMember),
		BindAddresses: make(map[uint32]string),
	}
	for lineNumber, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		match := cmapLinePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key, value := match[1], unquoteCmapValue(match[2])
		switch key {
		case "totem.config_version":
			if result.versionSet {
				return corosyncRuntimeSnapshot{}, fmt.Errorf("line %d repeats totem.config_version", lineNumber+1)
			}
			parsed, err := parseUint64(value, "totem.config_version")
			if err != nil {
				return corosyncRuntimeSnapshot{}, err
			}
			result.Version, result.versionSet = parsed, true
		case "nodelist.local_node_pos":
			if result.localPositionSet {
				return corosyncRuntimeSnapshot{}, fmt.Errorf("line %d repeats nodelist.local_node_pos", lineNumber+1)
			}
			parsed, err := parseUint32(value, "nodelist.local_node_pos")
			if err != nil {
				return corosyncRuntimeSnapshot{}, err
			}
			result.LocalPosition, result.localPositionSet = parsed, true
		default:
			if nodeMatch := cmapNodeKeyPattern.FindStringSubmatch(key); nodeMatch != nil {
				if err := parseCmapNodeField(&result, nodeMatch, value); err != nil {
					return corosyncRuntimeSnapshot{}, err
				}
				continue
			}
			if memberMatch := cmapMemberKeyPattern.FindStringSubmatch(key); memberMatch != nil {
				if err := parseCmapMemberField(&result, memberMatch, value); err != nil {
					return corosyncRuntimeSnapshot{}, err
				}
				continue
			}
			if bindMatch := cmapBindKeyPattern.FindStringSubmatch(key); bindMatch != nil {
				ring, err := parseUint32(bindMatch[1], "totem interface number")
				if err != nil {
					return corosyncRuntimeSnapshot{}, err
				}
				if _, exists := result.BindAddresses[ring]; exists {
					return corosyncRuntimeSnapshot{}, fmt.Errorf("line %d repeats %s", lineNumber+1, key)
				}
				result.BindAddresses[ring] = value
			}
		}
	}

	if !result.versionSet {
		return corosyncRuntimeSnapshot{}, fmt.Errorf("totem.config_version is missing")
	}
	if !result.localPositionSet {
		return corosyncRuntimeSnapshot{}, fmt.Errorf("nodelist.local_node_pos is missing")
	}
	if len(result.Nodes) == 0 {
		return corosyncRuntimeSnapshot{}, fmt.Errorf("nodelist nodes are missing")
	}
	if len(result.Members) == 0 {
		return corosyncRuntimeSnapshot{}, fmt.Errorf("runtime members are missing")
	}
	for position, node := range result.Nodes {
		if !node.nameSet || !node.idSet || len(node.Rings) == 0 {
			return corosyncRuntimeSnapshot{}, fmt.Errorf("nodelist.node.%d is missing name, nodeid, or ring addresses", position)
		}
	}
	for nodeID, member := range result.Members {
		if !member.statusSet || !member.versionSet || !member.addressSet {
			return corosyncRuntimeSnapshot{}, fmt.Errorf("runtime member %d is missing status, config_version, or link addresses", nodeID)
		}
	}
	return result, nil
}

func parseCmapNodeField(result *corosyncRuntimeSnapshot, match []string, value string) error {
	position, err := parseUint32(match[1], "nodelist node position")
	if err != nil {
		return err
	}
	node := result.Nodes[position]
	if node == nil {
		node = &cmapNode{Rings: make(map[uint32]string)}
		result.Nodes[position] = node
	}
	switch match[2] {
	case "name":
		if node.nameSet {
			return fmt.Errorf("nodelist.node.%d repeats name", position)
		}
		if value == "" {
			return fmt.Errorf("nodelist.node.%d has an empty name", position)
		}
		node.Name, node.nameSet = value, true
	case "nodeid":
		if node.idSet {
			return fmt.Errorf("nodelist.node.%d repeats nodeid", position)
		}
		parsed, err := parseUint32(value, fmt.Sprintf("nodelist.node.%d.nodeid", position))
		if err != nil {
			return err
		}
		if parsed == 0 {
			return fmt.Errorf("nodelist.node.%d has invalid nodeid 0", position)
		}
		node.ID, node.idSet = parsed, true
	default:
		ring, err := parseUint32(match[3], fmt.Sprintf("nodelist.node.%d ring number", position))
		if err != nil {
			return err
		}
		if _, exists := node.Rings[ring]; exists {
			return fmt.Errorf("nodelist.node.%d repeats ring%d_addr", position, ring)
		}
		node.Rings[ring] = value
	}
	return nil
}

func parseCmapMemberField(result *corosyncRuntimeSnapshot, match []string, value string) error {
	nodeID, err := parseUint32(match[1], "runtime member nodeid")
	if err != nil {
		return err
	}
	if nodeID == 0 {
		return fmt.Errorf("runtime member has invalid nodeid 0")
	}
	member := result.Members[nodeID]
	if member == nil {
		member = &cmapMember{Addresses: make(map[uint32]string)}
		result.Members[nodeID] = member
	}
	switch match[2] {
	case "status":
		if member.statusSet {
			return fmt.Errorf("runtime member %d repeats status", nodeID)
		}
		member.Status, member.statusSet = value, true
	case "config_version":
		if member.versionSet {
			return fmt.Errorf("runtime member %d repeats config_version", nodeID)
		}
		parsed, err := parseUint64(value, fmt.Sprintf("runtime member %d config_version", nodeID))
		if err != nil {
			return err
		}
		member.Version, member.versionSet = parsed, true
	case "ip":
		if member.addressSet {
			return fmt.Errorf("runtime member %d repeats link addresses", nodeID)
		}
		matches := cmapRuntimeAddressPattern.FindAllStringSubmatch(value, -1)
		if len(matches) == 0 || strings.TrimSpace(cmapRuntimeAddressPattern.ReplaceAllString(value, "")) != "" {
			return fmt.Errorf("runtime member %d has malformed link addresses %q", nodeID, value)
		}
		for _, addressMatch := range matches {
			ring, err := parseUint32(addressMatch[1], fmt.Sprintf("runtime member %d ring number", nodeID))
			if err != nil {
				return err
			}
			if _, exists := member.Addresses[ring]; exists {
				return fmt.Errorf("runtime member %d repeats ring%d address", nodeID, ring)
			}
			member.Addresses[ring] = strings.TrimSpace(addressMatch[2])
		}
		member.addressSet = true
	}
	return nil
}

func compareCorosyncSnapshots(persisted corosyncConfigSnapshot, runtime corosyncRuntimeSnapshot) error {
	if persisted.Version != runtime.Version {
		return fmt.Errorf("persisted config_version %d does not match live totem.config_version %d", persisted.Version, runtime.Version)
	}

	liveNodes := make(map[uint32]corosyncNode, len(runtime.Nodes))
	for _, candidate := range runtime.Nodes {
		if previous, exists := liveNodes[candidate.ID]; exists {
			return fmt.Errorf("live nodelist nodes %s and %s share nodeid %d", previous.Name, candidate.Name, candidate.ID)
		}
		liveNodes[candidate.ID] = corosyncNode{Name: candidate.Name, ID: candidate.ID, Rings: candidate.Rings}
	}
	if err := compareConfiguredNodes(persisted.Nodes, liveNodes); err != nil {
		return err
	}

	if expected, actual := sortedNodeIDs(persisted.Nodes), sortedMemberIDs(runtime.Members); !equalNodeIDs(expected, actual) {
		return fmt.Errorf("joined runtime membership %s does not match persisted nodes %s", describeMemberIDs(actual, persisted.Nodes), describeMemberIDs(expected, persisted.Nodes))
	}
	for nodeID, expectedNode := range persisted.Nodes {
		member := runtime.Members[nodeID]
		if member.Status != "joined" {
			return fmt.Errorf("runtime member %s(id=%d) status is %q, expected joined", expectedNode.Name, nodeID, member.Status)
		}
		if member.Version != persisted.Version {
			return fmt.Errorf("runtime member %s(id=%d) config_version %d does not match persisted %d", expectedNode.Name, nodeID, member.Version, persisted.Version)
		}
		if err := compareRuntimeAddresses(expectedNode, member.Addresses); err != nil {
			return err
		}
	}

	localNode := runtime.Nodes[runtime.LocalPosition]
	if localNode == nil {
		return fmt.Errorf("live local node position %d does not exist in nodelist", runtime.LocalPosition)
	}
	expectedLocal := persisted.Nodes[localNode.ID]
	for ring, configured := range expectedLocal.Rings {
		expectedAddress, numeric := parseNumericAddress(configured)
		if !numeric {
			continue
		}
		actual, exists := runtime.BindAddresses[ring]
		if !exists {
			return fmt.Errorf("local node %s(id=%d) has no live bind address for ring%d", expectedLocal.Name, expectedLocal.ID, ring)
		}
		actualAddress, valid := parseNumericAddress(actual)
		if !valid || actualAddress != expectedAddress {
			return fmt.Errorf("local node %s(id=%d) ring%d persisted address %q does not match live bind address %q", expectedLocal.Name, expectedLocal.ID, ring, configured, actual)
		}
	}
	return nil
}

func compareConfiguredNodes(expected, actual map[uint32]corosyncNode) error {
	if expectedIDs, actualIDs := sortedNodeIDs(expected), sortedNodeIDs(actual); !equalNodeIDs(expectedIDs, actualIDs) {
		return fmt.Errorf("live nodelist %s does not match persisted nodes %s", describeMemberIDs(actualIDs, expected), describeMemberIDs(expectedIDs, expected))
	}
	for nodeID, expectedNode := range expected {
		actualNode := actual[nodeID]
		if actualNode.Name != expectedNode.Name {
			return fmt.Errorf("nodeid %d persisted name %q does not match live nodelist name %q", nodeID, expectedNode.Name, actualNode.Name)
		}
		rings := make(map[uint32]bool, len(expectedNode.Rings)+len(actualNode.Rings))
		for ring := range expectedNode.Rings {
			rings[ring] = true
		}
		for ring := range actualNode.Rings {
			rings[ring] = true
		}
		ordered := make([]int, 0, len(rings))
		for ring := range rings {
			ordered = append(ordered, int(ring))
		}
		sort.Ints(ordered)
		for _, rawRing := range ordered {
			ring := uint32(rawRing)
			persistedAddress, persistedExists := expectedNode.Rings[ring]
			liveAddress, liveExists := actualNode.Rings[ring]
			if !persistedExists || !liveExists || persistedAddress != liveAddress {
				return fmt.Errorf("node %s(id=%d) ring%d persisted address %q does not match live nodelist address %q", expectedNode.Name, nodeID, ring, persistedAddress, liveAddress)
			}
		}
	}
	return nil
}

func compareRuntimeAddresses(node corosyncNode, actual map[uint32]string) error {
	if len(node.Rings) != len(actual) {
		return fmt.Errorf("runtime member %s(id=%d) reports %d links, persisted nodelist has %d", node.Name, node.ID, len(actual), len(node.Rings))
	}
	for ring, configured := range node.Rings {
		live, exists := actual[ring]
		if !exists {
			return fmt.Errorf("runtime member %s(id=%d) is missing ring%d", node.Name, node.ID, ring)
		}
		expectedAddress, numeric := parseNumericAddress(configured)
		if !numeric {
			continue
		}
		liveAddress, valid := parseNumericAddress(live)
		if !valid || liveAddress != expectedAddress {
			return fmt.Errorf("runtime member %s(id=%d) ring%d persisted address %q does not match joined address %q", node.Name, node.ID, ring, configured, live)
		}
	}
	return nil
}

func parseNumericAddress(value string) (netip.Addr, bool) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func unquoteCmapValue(value string) string {
	value = strings.TrimSpace(value)
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return value
}

func parseUint32(value, label string) (uint32, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %q", label, value)
	}
	return uint32(parsed), nil
}

func parseUint64(value, label string) (uint64, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %q", label, value)
	}
	return parsed, nil
}

func sortedNodeIDs(nodes map[uint32]corosyncNode) []uint32 {
	result := make([]uint32, 0, len(nodes))
	for nodeID := range nodes {
		result = append(result, nodeID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func sortedMemberIDs(nodes map[uint32]*cmapMember) []uint32 {
	result := make([]uint32, 0, len(nodes))
	for nodeID := range nodes {
		result = append(result, nodeID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func equalNodeIDs(left, right []uint32) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func describeMemberIDs(ids []uint32, names map[uint32]corosyncNode) string {
	items := make([]string, 0, len(ids))
	for _, nodeID := range ids {
		if node, exists := names[nodeID]; exists {
			items = append(items, fmt.Sprintf("%s(id=%d)", node.Name, nodeID))
		} else {
			items = append(items, fmt.Sprintf("id=%d", nodeID))
		}
	}
	return "[" + strings.Join(items, ", ") + "]"
}
