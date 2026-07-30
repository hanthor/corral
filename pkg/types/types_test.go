package types

import "testing"

func TestPortMap(t *testing.T) {
	if PortMap["ssh"] != 22 {
		t.Errorf("ssh port should be 22, got %d", PortMap["ssh"])
	}
	if PortMap["vnc"] != 5900 {
		t.Errorf("vnc port should be 5900, got %d", PortMap["vnc"])
	}
}

func TestDefaultPorts(t *testing.T) {
	if len(DefaultPorts) < 3 {
		t.Error("should have at least 3 default ports")
	}
	// Should contain 22, 5900
	has22 := false
	has5900 := false
	for _, p := range DefaultPorts {
		if p == 22 {
			has22 = true
		}
		if p == 5900 {
			has5900 = true
		}
	}
	if !has22 {
		t.Error("default ports should include 22 (SSH)")
	}
	if !has5900 {
		t.Error("default ports should include 5900 (VNC)")
	}
}

// Snapshot support is reported per backend so the dashboard can show the tab.
// pkg/snapshot implements an adapter for all four; a backend whose capability
// says otherwise would hide a feature that works (#134).
func TestCapabilitiesForBackend_SnapshotsMatchTheAdapters(t *testing.T) {
	for _, backend := range []string{"kubevirt", "libvirt", "incus", "qemu"} {
		if !CapabilitiesForBackend(backend).Snapshots {
			t.Errorf("%s has a snapshot adapter but reports Snapshots: false", backend)
		}
	}
	if CapabilitiesForBackend("vmware").Snapshots {
		t.Error("an unimplemented backend reports snapshot support")
	}
}
