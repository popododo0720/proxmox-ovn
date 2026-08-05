package ovnnb

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	// ovn-nbctl consumes these variables before its command line.  The manager
	// supplies an explicit database and options, so inheriting either variable
	// could silently redirect privileged writes or turn them into dry-runs.
	command.Env = environmentWithout(os.Environ(), "OVN_NB_DAEMON", "OVN_NBCTL_OPTIONS", "OVN_NB_DB")
	return command.CombinedOutput()
}

type ClientConfig struct {
	Runner      Runner
	Binary      string
	Database    []string
	TLSCA       string
	TLSCert     string
	TLSKey      string
	Timeout     int
	WaitForSync bool
}

type Client struct {
	runner Runner
	binary string
	base   []string
}

func NewClient(config ClientConfig) (*Client, error) {
	binary := config.Binary
	if binary == "" {
		binary = "ovn-nbctl"
	}
	if filepath.Base(binary) != "ovn-nbctl" || strings.TrimSpace(binary) != binary || strings.ContainsAny(binary, "\r\n\x00") {
		return nil, fmt.Errorf("invalid ovn-nbctl binary %q", binary)
	}
	if len(config.Database) == 0 {
		return nil, errors.New("at least one OVN Northbound endpoint is required")
	}
	for _, endpoint := range config.Database {
		if !validEndpoint(endpoint) {
			return nil, fmt.Errorf("unsafe OVN Northbound endpoint %q", endpoint)
		}
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 15
	} else if timeout < 0 || timeout > 3600 {
		return nil, errors.New("OVN command timeout must be between 1 and 3600 seconds")
	}
	// ovn-nbctl logs every mutating command, including addresses and external
	// IDs, to syslog at info level by default.  Other info-level messages also
	// cause journald to retain the process's full command line.  Keep routine
	// command payloads out of the manager journal while retaining warnings and
	// errors in the captured console output.
	base := []string{
		"--no-syslog",
		"--verbose=syslog:warn",
		"--verbose=console:warn",
		"--timeout=" + strconv.Itoa(timeout),
		"--db=" + strings.Join(config.Database, ","),
	}
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
	if config.WaitForSync {
		base = append(base, "--wait=sb")
	}
	runner := config.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Client{runner: runner, binary: binary, base: base}, nil
}

func (client *Client) run(ctx context.Context, arguments ...string) ([]byte, error) {
	args := append(append([]string(nil), client.base...), arguments...)
	output, err := client.runner.Run(ctx, client.binary, args...)
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%s: %w", detail, err)
	}
	return output, nil
}

// Probe verifies that the configured Northbound cluster is reachable. When
// WaitForSync is enabled this also waits for northd to report Southbound sync.
func (client *Client) Probe(ctx context.Context) error {
	if client == nil {
		return errors.New("OVN Northbound client is nil")
	}
	if _, err := client.run(ctx, "--bare", "--columns=name", "list", "NB_Global"); err != nil {
		return fmt.Errorf("probe OVN Northbound: %w", err)
	}
	return nil
}

func validEndpoint(endpoint string) bool {
	if strings.TrimSpace(endpoint) != endpoint || strings.ContainsAny(endpoint, "\r\n\x00,") {
		return false
	}
	if strings.HasPrefix(endpoint, "unix:") {
		return strings.HasPrefix(endpoint, "unix:/") && len(endpoint) > len("unix:/")
	}
	if !strings.HasPrefix(endpoint, "ssl:") {
		return false
	}
	remote := strings.TrimPrefix(endpoint, "ssl:")
	separator := strings.LastIndexByte(remote, ':')
	if separator <= 0 || separator == len(remote)-1 {
		return false
	}
	host, port := remote[:separator], remote[separator+1:]
	if strings.HasPrefix(host, "[") {
		if !strings.HasSuffix(host, "]") || len(host) <= 2 {
			return false
		}
		if _, err := netip.ParseAddr(host[1 : len(host)-1]); err != nil {
			return false
		}
	} else if strings.Contains(host, ":") {
		return false
	} else if !validDNSOrIP(host) {
		return false
	}
	portNumber, err := strconv.Atoi(port)
	return err == nil && portNumber >= 1 && portNumber <= 65535
}

func validDNSOrIP(host string) bool {
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Is4()
	}
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func environmentWithout(environment []string, names ...string) []string {
	discard := make(map[string]struct{}, len(names))
	for _, name := range names {
		discard[name] = struct{}{}
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, found := discard[name]; !found {
			result = append(result, entry)
		}
	}
	return result
}
