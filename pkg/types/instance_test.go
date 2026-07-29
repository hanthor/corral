package types

import "testing"

func TestInstanceRefRoundTrip(t *testing.T) {
	want := InstanceRef{Peer: "lab", Backend: "libvirt", Context: "qemu+ssh://alice@hypervisor/system", Namespace: "libvirt", Name: "desktop/dev"}
	got, err := ParseInstanceRef(want.String())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip: got %#v want %#v", got, want)
	}
}

func TestCapabilitiesForBackend(t *testing.T) {
	if got := CapabilitiesForBackend("incus"); !got.TTY || got.Snapshots {
		t.Fatalf("unexpected Incus capabilities: %+v", got)
	}
	if got := CapabilitiesForBackend("libvirt"); !got.VNC || got.RDP {
		t.Fatalf("unexpected libvirt capabilities: %+v", got)
	}
}
