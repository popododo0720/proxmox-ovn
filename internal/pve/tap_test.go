package pve

import "testing"

func TestParseTapName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		vmid  int
		index int
		valid bool
	}{
		{name: "tap100i0", vmid: 100, index: 0, valid: true},
		{name: "tap900001i31", vmid: 900001, index: 31, valid: true},
		{name: "fwln100i0"},
		{name: "tap0i0"},
		{name: "tap100"},
		{name: "tap100i"},
		{name: "tapx100i0"},
		{name: "tap100i-1"},
		{name: "tap100i0junk"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTapName(test.name)
			if !test.valid {
				if err == nil {
					t.Fatalf("ParseTapName(%q) unexpectedly succeeded: %#v", test.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTapName(%q): %v", test.name, err)
			}
			if got.VMID != test.vmid || got.NICIndex != test.index {
				t.Fatalf("ParseTapName(%q) = %#v, want VMID=%d NICIndex=%d", test.name, got, test.vmid, test.index)
			}
			if got.String() != test.name {
				t.Fatalf("TapName.String() = %q, want %q", got.String(), test.name)
			}
		})
	}
}
