package pve

import "testing"

func TestNetPropertyPreservesUnknownFields(t *testing.T) {
	t.Parallel()

	raw := "virtio=02:00:00:00:00:01,bridge=br-int,mystery=a=b,firewall=0,custom-flag"
	property, err := ParseNetProperty(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := property.String(); got != raw {
		t.Fatalf("round trip = %q, want %q", got, raw)
	}
	if err := property.SetLinkDown(true); err != nil {
		t.Fatal(err)
	}
	want := raw + ",link_down=1"
	if got := property.String(); got != want {
		t.Fatalf("staged property = %q, want %q", got, want)
	}
	if err := property.SetLinkDown(false); err != nil {
		t.Fatal(err)
	}
	want = raw + ",link_down=0"
	if got := property.String(); got != want {
		t.Fatalf("unstaged property = %q, want %q", got, want)
	}
}

func TestNetPropertySetRetainsOrderAndDeduplicates(t *testing.T) {
	t.Parallel()

	property, err := ParseNetProperty("virtio=old,tag=2,virtio=duplicate,unknown=x")
	if err != nil {
		t.Fatal(err)
	}
	if err := property.Set("virtio", "new"); err != nil {
		t.Fatal(err)
	}
	if got, want := property.String(), "virtio=new,tag=2,unknown=x"; got != want {
		t.Fatalf("Set result = %q, want %q", got, want)
	}
}
