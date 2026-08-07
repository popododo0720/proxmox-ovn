package model

import "testing"

func TestParseRequestedChassisCanonicalSingleAndMigrationPair(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{
		{"", 0},
		{"chassis-a", 1},
		{"chassis-a,chassis-b", 2},
	} {
		got, err := ParseRequestedChassis(test.value)
		if err != nil || len(got) != test.want {
			t.Fatalf("ParseRequestedChassis(%q)=%v err=%v", test.value, got, err)
		}
	}
	if !RequestedChassisContains("chassis-a,chassis-b", "chassis-b") || RequestedChassisContains("chassis-a,chassis-b", "chassis-c") {
		t.Fatal("requested chassis membership is incorrect")
	}
}

func TestParseRequestedChassisRejectsAmbiguousOrUnsafeValues(t *testing.T) {
	for _, value := range []string{
		" chassis-a", "chassis-a ", "chassis-a, chassis-b", "chassis-a,chassis-a",
		"chassis-a,chassis-b,chassis-c", "chassis-a,$bad", "-option",
	} {
		if _, err := ParseRequestedChassis(value); err == nil {
			t.Errorf("ParseRequestedChassis(%q) accepted an unsafe value", value)
		}
	}
}
