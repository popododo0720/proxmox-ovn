package ovnnb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SouthboundProbeConfig configures a read-only ovn-sbctl liveness probe.
type SouthboundProbeConfig struct {
	Runner   Runner
	Binary   string
	Database []string
	TLSCA    string
	TLSCert  string
	TLSKey   string
	Timeout  int
}

// SouthboundProbe verifies that the configured OVN Southbound database is
// reachable without mutating it.
type SouthboundProbe struct {
	runner Runner
	binary string
	base   []string
}

type southboundExecRunner struct{}

func (southboundExecRunner) Run(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = environmentWithout(os.Environ(), "OVN_SB_DAEMON", "OVN_SBCTL_OPTIONS", "OVN_SB_DB")
	return command.CombinedOutput()
}

func NewSouthboundProbe(config SouthboundProbeConfig) (*SouthboundProbe, error) {
	binary := config.Binary
	if binary == "" {
		binary = "ovn-sbctl"
	}
	if filepath.Base(binary) != "ovn-sbctl" || strings.TrimSpace(binary) != binary || strings.ContainsAny(binary, "\r\n\x00") {
		return nil, fmt.Errorf("invalid ovn-sbctl binary %q", binary)
	}
	if len(config.Database) == 0 {
		return nil, errors.New("at least one OVN Southbound endpoint is required")
	}
	for _, endpoint := range config.Database {
		if !validEndpoint(endpoint) {
			return nil, fmt.Errorf("unsafe OVN Southbound endpoint %q", endpoint)
		}
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 15
	} else if timeout < 0 || timeout > 3600 {
		return nil, errors.New("OVN command timeout must be between 1 and 3600 seconds")
	}
	base := []string{"--timeout=" + strconv.Itoa(timeout), "--db=" + strings.Join(config.Database, ",")}
	usesTLS := false
	for _, endpoint := range config.Database {
		usesTLS = usesTLS || strings.HasPrefix(endpoint, "ssl:")
	}
	if usesTLS {
		if config.TLSCA == "" || config.TLSCert == "" || config.TLSKey == "" {
			return nil, errors.New("OVN SSL endpoints require CA, certificate, and private key paths")
		}
		for label, path := range map[string]string{"CA certificate": config.TLSCA, "client certificate": config.TLSCert, "private key": config.TLSKey} {
			if !filepath.IsAbs(path) || strings.TrimSpace(path) != path || strings.ContainsAny(path, "\r\n\x00") {
				return nil, fmt.Errorf("OVN %s path must be an absolute, clean path", label)
			}
		}
		base = append(base,
			"--ca-cert="+config.TLSCA,
			"--certificate="+config.TLSCert,
			"--private-key="+config.TLSKey,
		)
	}
	runner := config.Runner
	if runner == nil {
		runner = southboundExecRunner{}
	}
	return &SouthboundProbe{runner: runner, binary: binary, base: base}, nil
}

func (probe *SouthboundProbe) Probe(ctx context.Context) error {
	if probe == nil {
		return errors.New("OVN Southbound probe is nil")
	}
	arguments := append(append([]string(nil), probe.base...), "--bare", "--columns=nb_cfg", "list", "SB_Global")
	output, err := probe.runner.Run(ctx, probe.binary, arguments...)
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return fmt.Errorf("probe OVN Southbound: %w", err)
		}
		return fmt.Errorf("probe OVN Southbound: %s: %w", detail, err)
	}
	return nil
}
