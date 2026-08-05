package raftstatus

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const healthyLeader = `aaaa
Name: PVN_Control
Cluster ID: 1111 (11111111-1111-4111-8111-111111111111)
Server ID: aaaa (aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa)
Address: ssl:192.0.2.10:6646
Status: cluster member
Role: leader
Term: 7
Leader: self
Vote: self

Election timer: 1000
Log: [42, 44]
Entries not yet committed: 0
Entries not yet applied: 0
Connections: ->bbbb <-cccc (->bbbb)
Disconnections: 1
Servers:
    aaaa (aaaa at ssl:192.0.2.10:6646) (self) next_index=44 match_index=43
    bbbb (bbbb at ssl:192.0.2.11:6646) next_index=44 match_index=43 last msg 3 ms ago
    cccc (cccc at ssl:192.0.2.12:6646) next_index=44 match_index=43 last msg 4 ms ago
`

func TestParseHealthyLeader(t *testing.T) {
	status, err := Parse([]byte(healthyLeader))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Healthy {
		t.Fatalf("expected healthy status, issues: %v", status.Issues)
	}
	if status.ClusterID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("unexpected cluster ID %q", status.ClusterID)
	}
	if status.ServerID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatalf("unexpected server ID %q", status.ServerID)
	}
	if status.MemberCount != 3 || status.ConnectedMembers != 3 || status.QuorumSize != 2 {
		t.Fatalf("unexpected quorum fields: %+v", status)
	}
}

func TestParseUnhealthyQuorum(t *testing.T) {
	input := strings.Replace(healthyLeader,
		"Connections: ->bbbb <-cccc (->bbbb)",
		"Connections: (->bbbb) (<-cccc)", 1)
	status, err := Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if status.Healthy {
		t.Fatal("disconnected majority must be unhealthy")
	}
	if status.ConnectedMembers != 1 || !containsIssue(status.Issues, "observable quorum is unavailable") {
		t.Fatalf("unexpected issues: %+v", status)
	}
}

func TestParseRejectsJoiningAndUnknownLeader(t *testing.T) {
	input := strings.NewReplacer(
		"Status: cluster member", "Status: joining cluster",
		"Role: leader", "Role: candidate",
		"Leader: self", "Leader: unknown",
	).Replace(healthyLeader)
	status, err := Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if status.Healthy {
		t.Fatal("joining candidate must be unhealthy")
	}
	for _, expected := range []string{"not a cluster member", `local role is "candidate"`, "leader is unknown"} {
		if !containsIssue(status.Issues, expected) {
			t.Fatalf("missing issue %q in %v", expected, status.Issues)
		}
	}
}

func TestParseSingleMemberCluster(t *testing.T) {
	input := strings.NewReplacer(
		"Connections: ->bbbb <-cccc (->bbbb)", "Connections:",
		"    bbbb (bbbb at ssl:192.0.2.11:6646) next_index=44 match_index=43 last msg 3 ms ago\n", "",
		"    cccc (cccc at ssl:192.0.2.12:6646) next_index=44 match_index=43 last msg 4 ms ago\n", "",
	).Replace(healthyLeader)
	status, err := Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Healthy || status.MemberCount != 1 || status.QuorumSize != 1 || status.ConnectedMembers != 1 {
		t.Fatalf("unexpected single-member status: %+v", status)
	}
}

func TestParseMalformedTerm(t *testing.T) {
	input := strings.Replace(healthyLeader, "Term: 7", "Term: seven", 1)
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("malformed term must fail parsing")
	}
}

func TestParseMembershipChangeIsUnhealthy(t *testing.T) {
	input := strings.Replace(healthyLeader,
		"Role: leader", "Adding server dddd (dddd at ssl:192.0.2.13:6646) (catchup)\nRole: leader", 1)
	status, err := Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if status.Healthy || !status.MembershipChange || !containsIssue(status.Issues, "membership change") {
		t.Fatalf("membership transition must be unhealthy: %+v", status)
	}
}

type runnerCall struct {
	binary string
	args   []string
}

type recordingRunner struct {
	calls   []runnerCall
	outputs map[string][]byte
	errors  map[string]error
}

func (r *recordingRunner) Run(_ context.Context, binary string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, runnerCall{binary: binary, args: append([]string(nil), args...)})
	database := args[len(args)-1]
	return r.outputs[database], r.errors[database]
}

func TestInspectQueriesEveryDatabaseAndPreservesFailures(t *testing.T) {
	runner := &recordingRunner{
		outputs: map[string][]byte{
			"PVN_Control":    []byte(healthyLeader),
			"OVN_Northbound": []byte(strings.Replace(healthyLeader, "Name: PVN_Control", "Name: OVN_Northbound", 1)),
			"OVN_Southbound": []byte("socket unavailable"),
		},
		errors: map[string]error{"OVN_Southbound": errors.New("exit status 1")},
	}
	targets := []Target{
		{Component: "control", Database: "PVN_Control", ControlSocket: "/run/a.ctl"},
		{Component: "nb", Database: "OVN_Northbound", ControlSocket: "/run/b.ctl"},
		{Component: "sb", Database: "OVN_Southbound", ControlSocket: "/run/c.ctl"},
	}
	report := Inspect(context.Background(), runner, "/custom/ovs-appctl", 1500*time.Millisecond, targets)
	if report.Healthy {
		t.Fatal("one unavailable database must make the report unhealthy")
	}
	if len(runner.calls) != 3 || len(report.Databases) != 3 {
		t.Fatalf("all databases must be queried: calls=%d results=%d", len(runner.calls), len(report.Databases))
	}
	wantArgs := []string{"--timeout=2", "--target=/run/a.ctl", "cluster/status", "PVN_Control"}
	if runner.calls[0].binary != "/custom/ovs-appctl" || !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("unexpected first call: %+v", runner.calls[0])
	}
	failed := report.Databases[2]
	if failed.Available || failed.Healthy || !strings.Contains(failed.Error, "socket unavailable") {
		t.Fatalf("unexpected failed result: %+v", failed)
	}
}

func TestInspectRejectsRelativeControlSocketWithoutExecution(t *testing.T) {
	runner := &recordingRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	report := Inspect(context.Background(), runner, "", time.Second, []Target{{
		Component: "control", Database: "PVN_Control", ControlSocket: "relative.ctl",
	}})
	if report.Healthy || len(runner.calls) != 0 {
		t.Fatalf("invalid target should fail closed without execution: %+v", report)
	}
	if got := report.Databases[0].Error; got != "control socket must be a clean absolute path" {
		t.Fatalf("unexpected validation error %q", got)
	}
}

func TestInspectDatabaseNameMismatch(t *testing.T) {
	runner := &recordingRunner{outputs: map[string][]byte{
		"OVN_Northbound": []byte(healthyLeader),
	}, errors: map[string]error{}}
	report := Inspect(context.Background(), runner, "", time.Second, []Target{{
		Component: "nb", Database: "OVN_Northbound", ControlSocket: "/run/ovn/nb.ctl",
	}})
	if report.Healthy || !containsIssue(report.Databases[0].Issues, "database name mismatch") {
		t.Fatalf("name mismatch must be unhealthy: %+v", report)
	}
}

func containsIssue(issues []string, substring string) bool {
	for _, issue := range issues {
		if strings.Contains(issue, substring) {
			return true
		}
	}
	return false
}

func ExampleParse() {
	status, _ := Parse([]byte(healthyLeader))
	fmt.Println(status.Healthy, status.MemberCount, status.QuorumSize)
	// Output: true 3 2
}
