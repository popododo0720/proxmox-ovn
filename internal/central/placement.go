package central

import (
	"fmt"
	"sort"
)

type Mode string

const (
	ModeStandalone Mode = "standalone"
	ModeRaft       Mode = "raft"
)

type Node struct {
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	Eligible bool   `json:"eligible"`
	Order    int    `json:"order"`
}

type Plan struct {
	Mode              Mode     `json:"mode"`
	Voters            []string `json:"voters"`
	TargetVoterCount  int      `json:"target_voter_count"`
	RequiresPromotion bool     `json:"requires_promotion"`
	Warning           string   `json:"warning,omitempty"`
}

// Select preserves healthy existing voters, then fills available slots in
// deterministic enrollment order. It never performs membership changes; the
// returned plan must be applied by the guarded central promotion workflow.
func Select(nodes []Node, existing []string) (Plan, error) {
	eligible := make(map[string]Node)
	ordered := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Name == "" {
			return Plan{}, fmt.Errorf("node name is required")
		}
		if !node.Online || !node.Eligible {
			continue
		}
		if _, duplicate := eligible[node.Name]; duplicate {
			return Plan{}, fmt.Errorf("duplicate node %q", node.Name)
		}
		eligible[node.Name] = node
		ordered = append(ordered, node)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Order == ordered[j].Order {
			return ordered[i].Name < ordered[j].Name
		}
		return ordered[i].Order < ordered[j].Order
	})

	target := len(ordered)
	if err := validateVoterCount(target); err != nil {
		return Plan{}, err
	}
	mode := ModeRaft
	if target == 1 {
		mode = ModeStandalone
	}
	voters := make([]string, 0, target)
	seen := make(map[string]struct{})
	for _, name := range existing {
		if _, ok := eligible[name]; !ok || len(voters) == target {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		voters = append(voters, name)
	}
	for _, node := range ordered {
		if len(voters) == target {
			break
		}
		if _, ok := seen[node.Name]; ok {
			continue
		}
		seen[node.Name] = struct{}{}
		voters = append(voters, node.Name)
	}

	plan := Plan{
		Mode:              mode,
		Voters:            voters,
		TargetVoterCount:  target,
		RequiresPromotion: !sameSet(voters, existing),
	}
	return plan, nil
}

func validateVoterCount(count int) error {
	if count < 1 || count%2 == 0 {
		return fmt.Errorf(
			"central voter count must be one standalone voter or an odd clustered count of at least three, got %d",
			count,
		)
	}
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, value := range a {
		set[value]++
	}
	for _, value := range b {
		set[value]--
		if set[value] < 0 {
			return false
		}
	}
	return true
}
