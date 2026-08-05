package diagnostic

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/pvnstack/proxmox-ovn/internal/config"
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
	}
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
