package diagnostic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/popododo0720/proxmox-ovn/internal/config"
)

type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

type Check struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func Run(ctx context.Context, cfg config.Config, runner Runner) []Check {
	checks := []Check{{Name: "config", Status: Pass, Message: "configuration is valid"}}
	if err := cfg.Validate(); err != nil {
		checks[0] = Check{Name: "config", Status: Fail, Message: err.Error()}
		return checks
	}

	checks = append(checks, commandCheck(ctx, runner, "pve-version", "pveversion", func(output string) error {
		pattern := regexp.MustCompile(`(?m)(?:pve-manager/|pve-manager:\s*)9(?:\.|-)`)
		if !pattern.MatchString(output) {
			return fmt.Errorf("expected Proxmox VE 9, got %q", firstLine(output))
		}
		return nil
	}, "--verbose"))

	checks = append(checks, commandCheck(ctx, runner, "integration-bridge", "ovs-vsctl", nil, "br-exists", cfg.Agent.Bridge))
	checks = append(checks, commandCheck(ctx, runner, "provider-bridge", "ovs-vsctl", nil, "br-exists", cfg.Networking.ProviderBridge))
	checks = append(checks, commandCheck(ctx, runner, "ovn-controller", "ovn-appctl", nil, "-t", "ovn-controller", "version"))
	checks = append(checks, commandCheck(ctx, runner, "ovn-southbound", "ovn-appctl", func(output string) error {
		if strings.TrimSpace(output) != "connected" {
			return fmt.Errorf("ovn-controller is not connected to Southbound (status %q)", strings.TrimSpace(output))
		}
		return nil
	}, "-t", "ovn-controller", "connection-status"))
	checks = append(checks, commandCheck(ctx, runner, "chassis-system-id", "ovs-vsctl", nonEmptyOVSValue,
		"--if-exists", "get", "Open_vSwitch", ".", "external_ids:system-id"))
	checks = append(checks, commandCheck(ctx, runner, "provider-bridge-mapping", "ovs-vsctl", func(output string) error {
		mapping := cfg.Networking.Physnet + ":" + cfg.Networking.ProviderBridge
		for _, candidate := range strings.Split(unquoteOVSValue(output), ",") {
			if strings.TrimSpace(candidate) == mapping {
				return nil
			}
		}
		return fmt.Errorf("OVS bridge mappings do not contain %q", mapping)
	}, "--if-exists", "get", "Open_vSwitch", ".", "external_ids:ovn-bridge-mappings"))
	checks = append(checks, commandCheck(ctx, runner, "ovn-southbound-remotes", "ovs-vsctl", func(output string) error {
		got := splitSet(unquoteOVSValue(output))
		for _, endpoint := range cfg.OVN.Southbound {
			if !got[endpoint] {
				return fmt.Errorf("OVS ovn-remote is missing configured endpoint %q", endpoint)
			}
		}
		return nil
	}, "--if-exists", "get", "Open_vSwitch", ".", "external_ids:ovn-remote"))

	for _, item := range []struct {
		name string
		path string
	}{
		{"tls-certificate", cfg.Manager.TLSCert},
		{"tls-private-key", cfg.Manager.TLSKey},
	} {
		if _, err := os.Stat(item.path); err != nil {
			checks = append(checks, Check{Name: item.name, Status: Fail, Message: err.Error()})
		} else {
			checks = append(checks, Check{Name: item.name, Status: Pass, Message: item.path})
		}
	}
	if strings.TrimSpace(cfg.Networking.EncapIP) == "" {
		checks = append(checks, Check{Name: "encap-ip", Status: Fail, Message: "networking.encap_ip is required on every node"})
	} else {
		checks = append(checks, Check{Name: "encap-ip", Status: Pass, Message: cfg.Networking.EncapIP})
		checks = append(checks, commandCheck(ctx, runner, "encap-underlay-mtu", "ip", func(output string) error {
			return validateEncapMTU(output, cfg.Networking.EncapIP, cfg.Networking.GuestMTU+100)
		}, "-j", "address", "show"))
	}
	checks = append(checks, socketCheck("manager-runtime-socket", cfg.Manager.UnixSocket))
	return checks
}

func Healthy(checks []Check) bool {
	for _, check := range checks {
		if check.Status == Fail {
			return false
		}
	}
	return true
}

func commandCheck(ctx context.Context, runner Runner, label, command string, validate func(string) error, args ...string) Check {
	output, err := runner.Run(ctx, command, args...)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return Check{Name: label, Status: Fail, Message: message}
	}
	message := strings.TrimSpace(string(output))
	if validate != nil {
		if err := validate(message); err != nil {
			return Check{Name: label, Status: Fail, Message: err.Error()}
		}
	}
	if message == "" {
		message = "ok"
	}
	return Check{Name: label, Status: Pass, Message: message}
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	return value
}

func nonEmptyOVSValue(output string) error {
	value := strings.TrimSpace(unquoteOVSValue(output))
	if value == "" || value == "[]" || value == "{}" {
		return fmt.Errorf("OVS value is not configured")
	}
	return nil
}

func unquoteOVSValue(value string) string {
	value = strings.TrimSpace(value)
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return value
}

func splitSet(value string) map[string]bool {
	result := make(map[string]bool)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = true
		}
	}
	return result
}

func validateEncapMTU(output, encapIP string, required int) error {
	var links []struct {
		Name      string `json:"ifname"`
		MTU       int    `json:"mtu"`
		Addresses []struct {
			Family string `json:"family"`
			Local  string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(output), &links); err != nil {
		return fmt.Errorf("decode ip address inventory: %w", err)
	}
	for _, link := range links {
		for _, address := range link.Addresses {
			if address.Family != "inet" || address.Local != encapIP {
				continue
			}
			if link.MTU < required {
				return fmt.Errorf("encapsulation interface %s MTU %d is below required %d", link.Name, link.MTU, required)
			}
			return nil
		}
	}
	return fmt.Errorf("encapsulation IPv4 address %s was not found on a local interface", encapIP)
}

func socketCheck(name, path string) Check {
	info, err := os.Stat(path)
	if err != nil {
		return Check{Name: name, Status: Fail, Message: err.Error()}
	}
	if info.Mode()&os.ModeSocket == 0 {
		return Check{Name: name, Status: Fail, Message: path + " is not a Unix socket"}
	}
	return Check{Name: name, Status: Pass, Message: path}
}
