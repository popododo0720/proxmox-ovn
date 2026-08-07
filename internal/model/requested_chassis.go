package model

import (
	"fmt"
	"strings"
)

// ParseRequestedChassis accepts the OVN requested-chassis representation used
// by PVN. A single chassis is the stable binding. Exactly two chassis IDs are
// allowed only while an online migration is prepared; the source must be first
// and the target second so retries have one canonical representation.
func ParseRequestedChassis(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	if strings.TrimSpace(value) != value {
		return nil, fmt.Errorf("requested chassis must not contain surrounding whitespace")
	}
	parts := strings.Split(value, ",")
	if len(parts) > 2 {
		return nil, fmt.Errorf("requested chassis may contain at most source and target")
	}
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		if !safeChassisID(part) {
			return nil, fmt.Errorf("requested chassis contains unsafe identifier %q", part)
		}
		if seen[part] {
			return nil, fmt.Errorf("requested chassis contains duplicate identifier %q", part)
		}
		seen[part] = true
	}
	return parts, nil
}

func RequestedChassisContains(value, chassis string) bool {
	parts, err := ParseRequestedChassis(value)
	if err != nil {
		return false
	}
	for _, part := range parts {
		if part == chassis {
			return true
		}
	}
	return false
}

func safeChassisID(value string) bool {
	if value == "" || len(value) > 255 || strings.HasPrefix(value, "-") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}
