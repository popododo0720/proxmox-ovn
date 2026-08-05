package ovs

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes a binary directly, never through a shell. It is deliberately
// small so tests can replace host command execution with a fake.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
	if strings.TrimSpace(binary) == "" || strings.ContainsAny(binary, "\r\n\x00") {
		return nil, fmt.Errorf("invalid command binary %q", binary)
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return nil, fmt.Errorf("run %s: %w", binary, err)
		}
		return nil, fmt.Errorf("run %s: %w: %s", binary, err, message)
	}
	return output, nil
}
