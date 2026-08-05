package central

import "testing"

func TestCentralPolicy(t *testing.T) {
	tests := []struct {
		count int
		want  int
		mode  Mode
	}{
		{1, 1, ModeStandalone},
		{2, 1, ModeStandalone},
		{3, 3, ModeRaft},
		{4, 3, ModeRaft},
		{5, 5, ModeRaft},
		{9, 5, ModeRaft},
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
		if len(plan.Voters) != tc.want || plan.Mode != tc.mode {
			t.Fatalf("count %d: got %+v", tc.count, plan)
		}
	}
}

func TestCentralPlacementPreservesExistingVoters(t *testing.T) {
	nodes := []Node{
		{Name: "a", Online: true, Eligible: true, Order: 1},
		{Name: "b", Online: true, Eligible: true, Order: 2},
		{Name: "c", Online: true, Eligible: true, Order: 3},
		{Name: "d", Online: true, Eligible: true, Order: 4},
	}
	plan, err := Select(nodes, []string{"d", "b", "a"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"d", "b", "a"}
	for i := range want {
		if plan.Voters[i] != want[i] {
			t.Fatalf("existing voter order changed: got %v want %v", plan.Voters, want)
		}
	}
	if plan.RequiresPromotion {
		t.Fatal("unchanged membership should not require promotion")
	}
}
