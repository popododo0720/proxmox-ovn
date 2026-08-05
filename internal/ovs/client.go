package ovs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultBinary  = "ovs-vsctl"
	defaultTimeout = 5
	ManagedByPVN   = "pvn"
)

type Interface struct {
	Name        string
	ExternalIDs map[string]string
}

type ManagedBinding struct {
	LSPName    string
	Generation string
	MACAddress string
}

type InterfaceSource interface {
	ListInterfaces(context.Context, string) ([]Interface, error)
}

type InterfaceBinder interface {
	SetManagedBinding(context.Context, string, ManagedBinding) error
	ClearManagedBinding(context.Context, string) error
}

type Client struct {
	runner  Runner
	binary  string
	timeout int
}

type ClientConfig struct {
	Runner         Runner
	Binary         string
	TimeoutSeconds int
}

func NewClient(config ClientConfig) (*Client, error) {
	runner := config.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	binary := config.Binary
	if binary == "" {
		binary = defaultBinary
	}
	if strings.TrimSpace(binary) != binary || strings.ContainsAny(binary, "\r\n\x00") || filepath.Base(binary) != defaultBinary {
		return nil, fmt.Errorf("invalid ovs-vsctl binary %q", binary)
	}
	timeout := config.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{runner: runner, binary: binary, timeout: timeout}, nil
}

// ListInterfaces obtains external IDs in OVSDB JSON format and intersects the
// result with list-ifaces for the requested bridge. This prevents a TAP on a
// non-PVN bridge from being accidentally claimed.
func (client *Client) ListInterfaces(ctx context.Context, bridge string) ([]Interface, error) {
	if err := validateOVSAtom("bridge", bridge); err != nil {
		return nil, err
	}
	jsonOutput, err := client.run(ctx, "--format=json", "--columns=name,external_ids", "list", "Interface")
	if err != nil {
		return nil, fmt.Errorf("query OVS interfaces: %w", err)
	}
	all, err := decodeInterfaces(jsonOutput)
	if err != nil {
		return nil, err
	}
	bridgeOutput, err := client.run(ctx, "list-ifaces", bridge)
	if err != nil {
		return nil, fmt.Errorf("list interfaces on bridge %s: %w", bridge, err)
	}

	onBridge := make(map[string]struct{})
	for _, name := range strings.Fields(string(bridgeOutput)) {
		onBridge[name] = struct{}{}
	}
	interfaces := make([]Interface, 0, len(onBridge))
	for _, ovsInterface := range all {
		if _, ok := onBridge[ovsInterface.Name]; ok {
			interfaces = append(interfaces, ovsInterface)
		}
	}
	sort.Slice(interfaces, func(left, right int) bool { return interfaces[left].Name < interfaces[right].Name })
	return interfaces, nil
}

// SetManagedBinding writes all OVN binding metadata in one ovs-vsctl
// transaction. Arguments are validated and passed without shell expansion.
func (client *Client) SetManagedBinding(ctx context.Context, interfaceName string, binding ManagedBinding) error {
	if err := validateTapInterface(interfaceName); err != nil {
		return err
	}
	if err := validateOVSAtom("LSP name", binding.LSPName); err != nil {
		return err
	}
	if err := validateOVSAtom("generation", binding.Generation); err != nil {
		return err
	}
	if err := validateMAC(binding.MACAddress); err != nil {
		return err
	}
	_, err := client.run(ctx,
		"--if-exists", "set", "Interface", interfaceName,
		"external_ids:iface-id="+binding.LSPName,
		"external_ids:iface-id-ver="+binding.Generation,
		"external_ids:attached-mac="+strings.ToLower(binding.MACAddress),
		"external_ids:managed-by="+ManagedByPVN,
	)
	if err != nil {
		return fmt.Errorf("bind OVS interface %s: %w", interfaceName, err)
	}
	return nil
}

// ClearManagedBinding removes only keys owned by PVN. Callers must verify the
// managed-by marker before invoking this method.
func (client *Client) ClearManagedBinding(ctx context.Context, interfaceName string) error {
	if err := validateTapInterface(interfaceName); err != nil {
		return err
	}
	_, err := client.run(ctx,
		"--if-exists", "remove", "Interface", interfaceName, "external_ids",
		"iface-id", "iface-id-ver", "attached-mac", "managed-by",
	)
	if err != nil {
		return fmt.Errorf("unbind OVS interface %s: %w", interfaceName, err)
	}
	return nil
}

func (client *Client) run(ctx context.Context, arguments ...string) ([]byte, error) {
	arguments = append([]string{"--timeout=" + strconv.Itoa(client.timeout)}, arguments...)
	return client.runner.Run(ctx, client.binary, arguments...)
}

type ovsTable struct {
	Headings []string            `json:"headings"`
	Data     [][]json.RawMessage `json:"data"`
}

func decodeInterfaces(payload []byte) ([]Interface, error) {
	var table ovsTable
	if err := json.Unmarshal(payload, &table); err != nil {
		return nil, fmt.Errorf("decode ovs-vsctl JSON: %w", err)
	}
	nameColumn, externalIDsColumn := -1, -1
	for index, heading := range table.Headings {
		switch heading {
		case "name":
			nameColumn = index
		case "external_ids":
			externalIDsColumn = index
		}
	}
	if nameColumn < 0 || externalIDsColumn < 0 {
		return nil, errors.New("ovs-vsctl JSON is missing name or external_ids")
	}

	interfaces := make([]Interface, 0, len(table.Data))
	for _, row := range table.Data {
		if nameColumn >= len(row) || externalIDsColumn >= len(row) {
			return nil, errors.New("ovs-vsctl JSON row has too few columns")
		}
		var name string
		if err := json.Unmarshal(row[nameColumn], &name); err != nil {
			return nil, fmt.Errorf("decode OVS interface name: %w", err)
		}
		externalIDs, err := decodeOVSMap(row[externalIDsColumn])
		if err != nil {
			return nil, fmt.Errorf("decode external IDs for %s: %w", name, err)
		}
		interfaces = append(interfaces, Interface{Name: name, ExternalIDs: externalIDs})
	}
	return interfaces, nil
}

func decodeOVSMap(encoded json.RawMessage) (map[string]string, error) {
	var tagged []json.RawMessage
	if err := json.Unmarshal(encoded, &tagged); err != nil {
		return nil, err
	}
	if len(tagged) != 2 {
		return nil, errors.New("invalid OVSDB map")
	}
	var tag string
	if err := json.Unmarshal(tagged[0], &tag); err != nil || tag != "map" {
		return nil, errors.New("invalid OVSDB map tag")
	}
	var pairs [][]string
	if err := json.Unmarshal(tagged[1], &pairs); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		if len(pair) != 2 {
			return nil, errors.New("invalid OVSDB map pair")
		}
		result[pair[0]] = pair[1]
	}
	return result, nil
}

func validateTapInterface(value string) error {
	if !strings.HasPrefix(value, "tap") || len(value) <= 4 {
		return fmt.Errorf("refusing non-TAP OVS interface %q", value)
	}
	body := strings.TrimPrefix(value, "tap")
	separator := strings.IndexByte(body, 'i')
	if separator <= 0 || separator == len(body)-1 || !allDigits(body[:separator]) || !allDigits(body[separator+1:]) || strings.TrimLeft(body[:separator], "0") == "" {
		return fmt.Errorf("refusing non-PVE TAP interface %q", value)
	}
	return validateOVSAtom("interface", value)
}

func validateOVSAtom(kind, value string) error {
	if value == "" || strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s is required", kind)
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' ||
			character == '.' || character == ':' || character == '@' {
			continue
		}
		return fmt.Errorf("invalid %s %q", kind, value)
	}
	return nil
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validateMAC(value string) error {
	parts := strings.Split(value, ":")
	if len(parts) != 6 {
		return fmt.Errorf("invalid MAC address %q", value)
	}
	for _, part := range parts {
		if len(part) != 2 {
			return fmt.Errorf("invalid MAC address %q", value)
		}
		if _, err := strconv.ParseUint(part, 16, 8); err != nil {
			return fmt.Errorf("invalid MAC address %q", value)
		}
	}
	return nil
}
