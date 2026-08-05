// Package raftstatus inspects the local OVSDB Raft membership exposed through
// ovsdb-server's unixctl socket.
package raftstatus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultAppctlPath    = "/usr/bin/ovs-appctl"
	DefaultControlSocket = "/run/pvn-control/pvn-control-db.ctl"
	DefaultNBSocket      = "/run/ovn/ovnnb_db.ctl"
	DefaultSBSocket      = "/run/ovn/ovnsb_db.ctl"
	DefaultTimeout       = 5 * time.Second
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	serverPattern = regexp.MustCompile(`^[ \t]+([^[:space:]()]+) \(([0-9a-fA-F]{4}) at ((?:ssl|tcp|unix):[^)]+)\)(.*)$`)
)

// Runner executes ovs-appctl. It is intentionally small so status collection
// can be tested without a running OVSDB server.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, binary string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, binary, arguments...).CombinedOutput()
}

// Target identifies one database and the exact local unixctl socket of the
// ovsdb-server process that serves it.
type Target struct {
	Component     string `json:"component"`
	Database      string `json:"database"`
	ControlSocket string `json:"control_socket"`
}

// Status is the machine-readable interpretation of one cluster/status reply.
// Available only means unixctl answered successfully; Healthy includes local
// membership, leader, role, and observable quorum checks.
type Status struct {
	Target
	Available        bool     `json:"available"`
	Healthy          bool     `json:"healthy"`
	Name             string   `json:"name,omitempty"`
	ClusterID        string   `json:"cluster_id,omitempty"`
	ServerID         string   `json:"server_id,omitempty"`
	Address          string   `json:"address,omitempty"`
	MembershipStatus string   `json:"membership_status,omitempty"`
	Role             string   `json:"role,omitempty"`
	Term             uint64   `json:"term,omitempty"`
	Leader           string   `json:"leader,omitempty"`
	MemberCount      int      `json:"member_count,omitempty"`
	ConnectedMembers int      `json:"connected_members,omitempty"`
	QuorumSize       int      `json:"quorum_size,omitempty"`
	MembershipChange bool     `json:"membership_change"`
	Issues           []string `json:"issues,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type Report struct {
	Healthy   bool     `json:"healthy"`
	Databases []Status `json:"databases"`
}

// Inspect queries every target even when another target is unavailable. This
// gives an operator one complete report for the three independent PVN/OVN Raft
// clusters.
func Inspect(ctx context.Context, runner Runner, appctlPath string, timeout time.Duration, targets []Target) Report {
	report := Report{Healthy: true, Databases: make([]Status, 0, len(targets))}
	if runner == nil {
		runner = ExecRunner{}
	}
	if appctlPath == "" {
		appctlPath = DefaultAppctlPath
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	for _, target := range targets {
		status := inspectOne(ctx, runner, appctlPath, timeout, target)
		report.Databases = append(report.Databases, status)
		if !status.Healthy {
			report.Healthy = false
		}
	}
	return report
}

func inspectOne(parent context.Context, runner Runner, appctlPath string, timeout time.Duration, target Target) Status {
	status := Status{Target: target}
	if err := validateTarget(target); err != nil {
		status.Error = err.Error()
		return status
	}

	seconds := int64((timeout + time.Second - 1) / time.Second)
	ctx, cancel := context.WithTimeout(parent, timeout+time.Second)
	defer cancel()
	output, err := runner.Run(ctx, appctlPath,
		"--timeout="+strconv.FormatInt(seconds, 10),
		"--target="+target.ControlSocket,
		"cluster/status", target.Database,
	)
	if err != nil {
		status.Error = commandError(output, err)
		return status
	}

	status.Available = true
	parsed, parseErr := Parse(output)
	if parseErr != nil {
		status.Error = "parse cluster/status: " + parseErr.Error()
		return status
	}
	parsed.Target = target
	parsed.Available = true
	if parsed.Name != target.Database {
		parsed.Issues = append(parsed.Issues,
			fmt.Sprintf("database name mismatch: got %q, expected %q", parsed.Name, target.Database))
	}
	parsed.Healthy = len(parsed.Issues) == 0
	return parsed
}

func validateTarget(target Target) error {
	if strings.TrimSpace(target.Component) == "" {
		return errors.New("component is required")
	}
	if strings.TrimSpace(target.Database) == "" {
		return errors.New("database is required")
	}
	if !filepath.IsAbs(target.ControlSocket) || filepath.Clean(target.ControlSocket) != target.ControlSocket {
		return errors.New("control socket must be a clean absolute path")
	}
	return nil
}

func commandError(output []byte, err error) string {
	detail := strings.TrimSpace(string(output))
	if len(detail) > 4096 {
		detail = detail[:4096] + "..."
	}
	if detail == "" {
		return err.Error()
	}
	return detail + ": " + err.Error()
}

// Parse interprets the stable text fields emitted by OVSDB's cluster/status
// unixctl command. Unknown fields are ignored so newer OVS releases remain
// compatible; missing safety-critical fields make the result unhealthy.
func Parse(output []byte) (Status, error) {
	var status Status
	members := make(map[string]struct{})
	connected := make(map[string]struct{})
	inServers := false
	selfSeen := false
	membershipChange := false

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Adding server ") || strings.HasPrefix(line, "Removing server ") {
			membershipChange = true
		}
		if inServers {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if len(line) == 0 || (line[0] != ' ' && line[0] != '\t') {
				inServers = false
			} else {
				match := serverPattern.FindStringSubmatch(line)
				if match == nil {
					return Status{}, fmt.Errorf("malformed server line %q", line)
				}
				nickname := match[1]
				members[nickname] = struct{}{}
				if strings.Contains(match[4], "(self)") {
					selfSeen = true
				}
				suffix := strings.ReplaceAll(match[4], "(self)", "")
				for {
					start := strings.Index(suffix, "(voted for ")
					if start < 0 {
						break
					}
					end := strings.IndexByte(suffix[start:], ')')
					if end < 0 {
						break
					}
					suffix = suffix[:start] + suffix[start+end+1:]
				}
				if strings.Contains(suffix, "(") {
					membershipChange = true
				}
				continue
			}
		}

		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			status.Name = value
		case "Cluster ID":
			status.ClusterID = parenthesizedUUID(value)
		case "Server ID":
			status.ServerID = parenthesizedUUID(value)
		case "Address":
			status.Address = value
		case "Status":
			status.MembershipStatus = value
		case "Role":
			status.Role = value
		case "Term":
			term, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return Status{}, fmt.Errorf("invalid term %q", value)
			}
			status.Term = term
		case "Leader":
			status.Leader = value
		case "Connections":
			for _, token := range strings.Fields(value) {
				if strings.HasPrefix(token, "(") {
					continue
				}
				nickname := strings.TrimPrefix(strings.TrimPrefix(token, "->"), "<-")
				if nickname != "" {
					connected[nickname] = struct{}{}
				}
			}
		case "Servers":
			inServers = true
		}
	}
	if err := scanner.Err(); err != nil {
		return Status{}, err
	}

	status.MemberCount = len(members)
	if status.MemberCount > 0 {
		status.QuorumSize = status.MemberCount/2 + 1
		status.ConnectedMembers = 1
		for nickname := range connected {
			if _, exists := members[nickname]; exists {
				status.ConnectedMembers++
			}
		}
		if status.ConnectedMembers > status.MemberCount {
			status.ConnectedMembers = status.MemberCount
		}
	}

	status.MembershipChange = membershipChange
	status.Issues = healthIssues(status, selfSeen, members)
	status.Healthy = len(status.Issues) == 0
	return status, nil
}

func parenthesizedUUID(value string) string {
	open := strings.LastIndexByte(value, '(')
	if open < 0 || !strings.HasSuffix(value, ")") {
		return ""
	}
	value = value[open+1 : len(value)-1]
	if !uuidPattern.MatchString(value) {
		return ""
	}
	return strings.ToLower(value)
}

func healthIssues(status Status, selfSeen bool, members map[string]struct{}) []string {
	var issues []string
	if status.Name == "" {
		issues = append(issues, "database name is missing")
	}
	if status.ClusterID == "" {
		issues = append(issues, "cluster ID is missing or invalid")
	}
	if status.ServerID == "" {
		issues = append(issues, "server ID is missing or invalid")
	}
	if status.Address == "" {
		issues = append(issues, "local Raft address is missing")
	}
	if status.MembershipStatus != "cluster member" {
		issues = append(issues, fmt.Sprintf("local status is %q, not a cluster member", status.MembershipStatus))
	}
	if status.Role != "leader" && status.Role != "follower" {
		issues = append(issues, fmt.Sprintf("local role is %q", status.Role))
	}
	if status.Term == 0 {
		issues = append(issues, "Raft term is zero or missing")
	}
	if status.Leader == "" || status.Leader == "unknown" {
		issues = append(issues, "leader is unknown")
	} else if status.Role == "leader" && status.Leader != "self" {
		issues = append(issues, "leader role does not identify itself as leader")
	} else if status.Leader != "self" {
		if _, exists := members[status.Leader]; !exists {
			issues = append(issues, "leader is absent from the member list")
		}
	} else if status.Role != "leader" {
		issues = append(issues, "follower reports itself as leader")
	}
	if status.MemberCount == 0 {
		issues = append(issues, "member list is empty")
	} else {
		if !selfSeen {
			issues = append(issues, "local server is absent from the member list")
		}
		if status.ConnectedMembers < status.QuorumSize {
			issues = append(issues, fmt.Sprintf(
				"observable quorum is unavailable: %d connected of %d members, need %d",
				status.ConnectedMembers, status.MemberCount, status.QuorumSize))
		}
	}
	if status.MembershipChange {
		issues = append(issues, "membership change is in progress")
	}
	return issues
}
