package centraldb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDatabase = "/var/lib/pvn/control-db/pvn_control.db"
	DefaultSchema   = "/usr/share/pvn/schema/pvn-control.ovsschema"
	SchemaName      = "PVN_Control"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type InitOptions struct {
	Database string
	Schema   string
	Mode     string
	Local    string
	Remotes  []string
}

func Init(ctx context.Context, runner Runner, options InitOptions) error {
	options.defaults()
	if _, err := os.Stat(options.Database); err == nil {
		return fmt.Errorf("database %q already exists", options.Database)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect database: %w", err)
	}
	if _, err := os.Stat(options.Schema); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(options.Database), 0o750); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}

	var args []string
	switch options.Mode {
	case "standalone":
		if options.Local != "" || len(options.Remotes) != 0 {
			return errors.New("standalone initialization does not accept local or remote Raft addresses")
		}
		args = []string{"create", options.Database, options.Schema}
	case "raft":
		if err := validateClusterAddress(options.Local); err != nil {
			return fmt.Errorf("local Raft address: %w", err)
		}
		for _, remote := range options.Remotes {
			if err := validateClusterAddress(remote); err != nil {
				return fmt.Errorf("remote Raft address: %w", err)
			}
		}
		if len(options.Remotes) == 0 {
			args = []string{"create-cluster", options.Database, options.Schema, options.Local}
		} else {
			args = append([]string{"join-cluster", options.Database, SchemaName, options.Local}, options.Remotes...)
		}
	default:
		return fmt.Errorf("unsupported database mode %q", options.Mode)
	}

	if output, err := runner.Run(ctx, "ovsdb-tool", args...); err != nil {
		return commandError("initialize PVN control database", output, err)
	}
	if err := os.Chmod(options.Database, 0o600); err != nil {
		return fmt.Errorf("secure database: %w", err)
	}
	return nil
}

func (o *InitOptions) defaults() {
	if o.Database == "" {
		o.Database = DefaultDatabase
	}
	if o.Schema == "" {
		o.Schema = DefaultSchema
	}
	if o.Mode == "" {
		o.Mode = "standalone"
	}
}

type PromoteOptions struct {
	Database string
	Local    string
	Now      func() time.Time
}

func Promote(ctx context.Context, runner Runner, options PromoteOptions) (string, error) {
	if options.Database == "" {
		options.Database = DefaultDatabase
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if err := validateClusterAddress(options.Local); err != nil {
		return "", fmt.Errorf("local Raft address: %w", err)
	}
	if _, err := os.Stat(options.Database); err != nil {
		return "", fmt.Errorf("inspect database: %w", err)
	}

	if output, err := runner.Run(ctx, "systemctl", "is-active", "--quiet", "pvn-control-db.service"); err == nil {
		return "", errors.New("pvn-control-db.service must be stopped before promotion")
	} else if len(output) > 0 {
		return "", commandError("check pvn-control-db.service", output, err)
	}
	if output, err := runner.Run(ctx, "ovsdb-tool", "db-is-standalone", options.Database); err != nil {
		return "", commandError("verify standalone database", output, err)
	}

	timestamp := options.Now().UTC().Format("20060102T150405Z")
	backup := options.Database + ".standalone." + timestamp
	if err := copyFile(options.Database, backup); err != nil {
		return "", fmt.Errorf("back up standalone database: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(options.Database), ".pvn-control-raft-*")
	if err != nil {
		return backup, fmt.Errorf("create promotion path: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return backup, fmt.Errorf("close promotion path: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return backup, fmt.Errorf("prepare promotion path: %w", err)
	}
	defer os.Remove(temporaryPath)

	if output, err := runner.Run(ctx, "ovsdb-tool", "create-cluster", temporaryPath, options.Database, options.Local); err != nil {
		return backup, commandError("create clustered database", output, err)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return backup, fmt.Errorf("secure clustered database: %w", err)
	}
	if err := os.Rename(temporaryPath, options.Database); err != nil {
		return backup, fmt.Errorf("activate clustered database: %w", err)
	}
	return backup, nil
}

func validateClusterAddress(address string) error {
	parts := strings.SplitN(address, ":", 2)
	if len(parts) != 2 || (parts[0] != "ssl" && parts[0] != "tcp") {
		return errors.New("must use ssl:IP:PORT or tcp:IP:PORT syntax")
	}
	host, port, err := net.SplitHostPort(parts[1])
	if err != nil {
		return err
	}
	if net.ParseIP(host) == nil {
		return errors.New("host must be an IP address")
	}
	numericPort, err := strconv.Atoi(port)
	if err != nil || numericPort < 1 || numericPort > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func copyFile(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %s: %w", action, detail, err)
}
