package central

import "testing"

func TestCentralPolicy(t *testing.T) {
	tests := []struct {
		count int
		mode  Mode
	}{
		{1, ModeStandalone},
		{3, ModeRaft},
		{5, ModeRaft},
		{7, ModeRaft},
	}
	for _, tc := range tests {
		nodes := make([]Node, tc.count)
		for index := range nodes {
			nodes[index] = Node{Name: string(rune('a' + index)), Online: true, Eligible: true, Order: index}
		}
		plan, err := Select(nodes, nil)
		if err != nil {
			t.Fatalf("count %d: %v", tc.count, err)
		}
		if len(plan.Voters) != tc.count || plan.TargetVoterCount != tc.count || plan.Mode != tc.mode {
			t.Fatalf("count %d: got %+v", tc.count, plan)
		}
	}
}

func TestCentralPolicyRejectsInvalidVoterCounts(t *testing.T) {
	for _, count := range []int{-1, 0, 2, 4, 6} {
		if err := validateVoterCount(count); err == nil {
			t.Fatalf("count %d: expected validation failure", count)
		}
	}

	for _, count := range []int{0, 2, 4, 6} {
		nodes := make([]Node, count)
		for index := range nodes {
			nodes[index] = Node{Name: string(rune('a' + index)), Online: true, Eligible: true, Order: index}
		}
		if plan, err := Select(nodes, nil); err == nil {
			t.Fatalf("count %d: invalid placement succeeded: %+v", count, plan)
		}
	}
}

func TestCentralPlacementPreservesExistingVoters(t *testing.T) {
	nodes := []Node{
		{Name: "a", Online: true, Eligible: true, Order: 1},
		{Name: "b", Online: true, Eligible: true, Order: 2},
		{Name: "c", Online: true, Eligible: true, Order: 3},
		{Name: "d", Online: true, Eligible: true, Order: 4},
		{Name: "e", Online: true, Eligible: true, Order: 5},
	}
	plan, err := Select(nodes, []string{"d", "b", "a", "e", "c"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"d", "b", "a", "e", "c"}
	for i := range want {
		if plan.Voters[i] != want[i] {
			t.Fatalf("existing voter order changed: got %v want %v", plan.Voters, want)
		}
	}
	if plan.RequiresPromotion {
		t.Fatal("unchanged membership should not require promotion")
	}
}
