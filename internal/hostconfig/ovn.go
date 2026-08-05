// Package hostconfig applies the small, node-local OVS settings consumed by
// ovn-controller. It never creates bridges or changes physical interfaces.
package hostconfig

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type Config struct {
	IntegrationBridge string
	ProviderBridge    string
	PhysicalNetwork   string
	EncapType         string
	EncapIP           string
	Southbound        []string
}

func ApplyOVN(ctx context.Context, runner Runner, config Config) error {
	if runner == nil {
		return errors.New("command runner is required")
	}
	if config.IntegrationBridge == "" || config.ProviderBridge == "" || config.PhysicalNetwork == "" || config.EncapType == "" || config.EncapIP == "" || len(config.Southbound) == 0 {
		return errors.New("integration bridge, provider bridge, physical network, encapsulation, and Southbound endpoints are required")
	}
	if config.IntegrationBridge == config.ProviderBridge {
		return errors.New("integration and provider bridges must be distinct")
	}
	for _, bridge := range []string{config.IntegrationBridge, config.ProviderBridge} {
		if output, err := runner.Run(ctx, "ovs-vsctl", "--timeout=10", "br-exists", bridge); err != nil {
			return commandError("verify OVS bridge "+bridge, output, err)
		}
	}
	output, err := runner.Run(ctx, "ovs-vsctl", "--timeout=10", "get", "Open_vSwitch", ".", "external_ids:ovn-bridge-mappings")
	if err != nil {
		return commandError("read OVN bridge mappings", output, err)
	}
	mappings, err := parseMappings(string(output))
	if err != nil {
		return err
	}
	if current, found := mappings[config.PhysicalNetwork]; found && current != config.ProviderBridge {
		return fmt.Errorf("physical network %q is already mapped to OVS bridge %q", config.PhysicalNetwork, current)
	}
	mappings[config.PhysicalNetwork] = config.ProviderBridge

	assignments := []string{
		"external_ids:ovn-remote=" + strings.Join(config.Southbound, ","),
		"external_ids:ovn-encap-type=" + config.EncapType,
		"external_ids:ovn-encap-ip=" + config.EncapIP,
		"external_ids:ovn-remote-probe-interval=10000",
		"external_ids:ovn-bridge-mappings=" + formatMappings(mappings),
	}
	arguments := append([]string{"--timeout=10", "set", "Open_vSwitch", "."}, assignments...)
	if output, err := runner.Run(ctx, "ovs-vsctl", arguments...); err != nil {
		return commandError("configure local OVN controller", output, err)
	}
	return nil
}

func parseMappings(raw string) (map[string]string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || value == "[]" {
		return make(map[string]string), nil
	}
	if strings.HasPrefix(value, "\"") {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return nil, fmt.Errorf("decode existing OVN bridge mappings: %w", err)
		}
		value = decoded
	}
	result := make(map[string]string)
	for _, entry := range strings.Split(value, ",") {
		physicalNetwork, bridge, found := strings.Cut(strings.TrimSpace(entry), ":")
		if !found || physicalNetwork == "" || bridge == "" || strings.Contains(bridge, ":") {
			return nil, fmt.Errorf("invalid existing OVN bridge mapping %q", entry)
		}
		if previous, duplicate := result[physicalNetwork]; duplicate && previous != bridge {
			return nil, fmt.Errorf("physical network %q has conflicting existing bridge mappings", physicalNetwork)
		}
		result[physicalNetwork] = bridge
	}
	return result, nil
}

func formatMappings(mappings map[string]string) string {
	entries := make([]string, 0, len(mappings))
	for physicalNetwork, bridge := range mappings {
		entries = append(entries, physicalNetwork+":"+bridge)
	}
	sort.Strings(entries)
	return strings.Join(entries, ",")
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %s: %w", action, detail, err)
}
