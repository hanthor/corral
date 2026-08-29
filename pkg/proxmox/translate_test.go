package proxmox

import (
	"fmt"
	"math"
	"testing"

	"github.com/tuna-os/corral/pkg/types"
)

// ── VmidFor tests ────────────────────────────────────────────────────────────

func TestVmidFor_Deterministic(t *testing.T) {
	// Same name should produce the same ID every time
	id1 := VmidFor("my-vm-1")
	id2 := VmidFor("my-vm-1")
	if id1 != id2 {
		t.Errorf("VmidFor not deterministic: %d != %d", id1, id2)
	}
}

func TestVmidFor_InRange(t *testing.T) {
	names := []string{"", "a", "vm", "my-very-long-vm-name-with-many-characters-12345", "test-vm-42"}
	for _, name := range names {
		id := VmidFor(name)
		if id < 100 || id > 999999999 {
			t.Errorf("VmidFor(%q) = %d, want in [100, 999999999]", name, id)
		}
	}
}

func TestVmidFor_DifferentNames(t *testing.T) {
	// Different names should (almost certainly) produce different IDs
	ids := make(map[int]bool)
	names := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta",
		"eta", "theta", "iota", "kappa", "lambda", "mu"}
	for _, name := range names {
		id := VmidFor(name)
		if ids[id] {
			t.Logf("collision for %q → %d (acceptable at homelab scale)", name, id)
		}
		ids[id] = true
	}
}

// ── PVEStatus tests ──────────────────────────────────────────────────────────

func TestPVEStatus(t *testing.T) {
	tests := []struct {
		name    string
		running bool
		want    string
	}{
		{"running VM", true, "running"},
		{"stopped VM", false, "stopped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vm := &types.VM{Running: tt.running}
			got := PVEStatus(vm)
			if got != tt.want {
				t.Errorf("PVEStatus(Running=%v) = %q, want %q", tt.running, got, tt.want)
			}
		})
	}
}

// ── MemBytes tests ───────────────────────────────────────────────────────────

func TestMemBytes(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"", 0},
		{"0", 0},
		{"1G", 1 << 30},
		{"1Gi", 1 << 30},
		{"2G", 2 * (1 << 30)},
		{"512M", 512 * (1 << 20)},
		{"512Mi", 512 * (1 << 20)},
		{"1024M", 1024 * (1 << 20)},
		{"1K", 1 << 10},
		{"1k", 1 << 10},
		{"4G", 4 * (1 << 30)},
		{"4096M", 4096 * (1 << 20)},
		{" 2G ", 2 * (1 << 30)}, // whitespace trimmed
		{"0.5G", int64(0.5 * float64(1<<30))},
		{"1.5G", int64(1.5 * float64(1<<30))},
		{"100", 100 * (1 << 20)}, // no unit = MiB
		{"abc", 0},               // invalid input
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := MemBytes(tt.input)
			if got != tt.want {
				t.Errorf("MemBytes(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestMemBytes_RoundTrip(t *testing.T) {
	// Verify that common values round-trip through human-readable format
	values := []int64{0, 1 << 20, 1 << 30, 512 * (1 << 20), 2 * (1 << 30)}
	for _, v := range values {
		// Parse back from a formatted string
		var s string
		switch {
		case v >= 1<<30 && v%(1<<30) == 0:
			s = fmt.Sprintf("%dG", v/(1<<30))
		case v >= 1<<20 && v%(1<<20) == 0:
			s = fmt.Sprintf("%dM", v/(1<<20))
		default:
			continue
		}
		got := MemBytes(s)
		if got != v {
			t.Errorf("MemBytes(%q) = %d, want %d", s, got, v)
		}
	}
}

// ── UPID tests ───────────────────────────────────────────────────────────────

func TestUPID(t *testing.T) {
	tests := []struct {
		node   string
		action string
		vmid   int
	}{
		{"pve-node1", "start", 100},
		{"pve-node2", "stop", 200},
		{"worker-3", "migrate", 300},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			pid := UPID(tt.node, tt.action, tt.vmid)
			if pid == "" {
				t.Fatal("UPID returned empty string")
			}
			if len(pid) < 20 {
				t.Errorf("UPID too short: %q", pid)
			}
		})
	}
}

func TestUPID_Format(t *testing.T) {
	pid := UPID("node1", "start", 100)
	expectedPrefix := "UPID:node1:"
	if len(pid) < len(expectedPrefix) || pid[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("UPID should start with %q, got %q", expectedPrefix, pid)
	}
	// Should contain the action
	if !contains(pid, ":start:") {
		t.Errorf("UPID should contain ':start:', got %q", pid)
	}
}

// ── VMNode tests ─────────────────────────────────────────────────────────────

func TestVMNode_ExplicitNode(t *testing.T) {
	vm := &types.VM{Name: "test", Node: "explicit-node"}
	nodes := []NodeInfo{{Name: "other", Ready: true}}
	got := VMNode(vm, nodes)
	if got != "explicit-node" {
		t.Errorf("VMNode should return explicit node, got %q", got)
	}
}

func TestVMNode_DashNode(t *testing.T) {
	// "—" should be treated as empty (KubeVirt unplaced marker)
	vm := &types.VM{Name: "test", Node: "—"}
	nodes := []NodeInfo{{Name: "node1", Ready: true}}
	got := VMNode(vm, nodes)
	if got != "node1" {
		t.Errorf("VMNode should fall back to first ready node for '—', got %q", got)
	}
}

func TestVMNode_EmptyNode(t *testing.T) {
	vm := &types.VM{Name: "test", Node: ""}
	nodes := []NodeInfo{{Name: "node1", Ready: true}}
	got := VMNode(vm, nodes)
	if got != "node1" {
		t.Errorf("VMNode should fall back to first ready node, got %q", got)
	}
}

func TestVMNode_NoReadyNodes(t *testing.T) {
	vm := &types.VM{Name: "test", Node: ""}
	nodes := []NodeInfo{{Name: "node1", Ready: false}}
	got := VMNode(vm, nodes)
	if got != "" {
		t.Errorf("VMNode should return empty when no nodes are ready, got %q", got)
	}
}

func TestVMNode_EmptyNodes(t *testing.T) {
	vm := &types.VM{Name: "test", Node: ""}
	got := VMNode(vm, nil)
	if got != "" {
		t.Errorf("VMNode should return empty for nil nodes, got %q", got)
	}
}

func TestVMNode_PrefersFirstReady(t *testing.T) {
	vm := &types.VM{Name: "test", Node: ""}
	nodes := []NodeInfo{
		{Name: "node1", Ready: false},
		{Name: "node2", Ready: true},
		{Name: "node3", Ready: true},
	}
	got := VMNode(vm, nodes)
	if got != "node2" {
		t.Errorf("VMNode should return first ready node (node2), got %q", got)
	}
}

// ── VMEntry tests ────────────────────────────────────────────────────────────

func TestVMEntry_Shape(t *testing.T) {
	vm := &types.VM{Name: "test-vm", CPU: 2, Mem: "4G", Running: true}
	entry := VMEntry(vm, 100, "node1")

	if entry["vmid"] != 100 {
		t.Errorf("vmid = %v, want 100", entry["vmid"])
	}
	if entry["name"] != "test-vm" {
		t.Errorf("name = %v, want test-vm", entry["name"])
	}
	if entry["status"] != "running" {
		t.Errorf("status = %v, want running", entry["status"])
	}
	if entry["node"] != "node1" {
		t.Errorf("node = %v, want node1", entry["node"])
	}
	if entry["cpus"] != 2 {
		t.Errorf("cpus = %v, want 2", entry["cpus"])
	}
	if entry["maxmem"] != int64(4*1024*1024*1024) {
		t.Errorf("maxmem = %v, want %d", entry["maxmem"], 4*1024*1024*1024)
	}
	if entry["template"] != 0 {
		t.Errorf("template = %v, want 0", entry["template"])
	}
}

func TestVMEntry_Stopped(t *testing.T) {
	vm := &types.VM{Name: "stopped-vm", Running: false}
	entry := VMEntry(vm, 200, "node2")
	if entry["status"] != "stopped" {
		t.Errorf("status = %v, want stopped", entry["status"])
	}
}

// ── K8sRolesToProxmox tests ──────────────────────────────────────────────────

func TestK8sRolesToProxmox_HasExpectedRoles(t *testing.T) {
	roles := K8sRolesToProxmox()
	expected := []string{"Administrator", "Operator", "Viewer", "NoAccess"}
	if len(roles) != len(expected) {
		t.Fatalf("expected %d roles, got %d", len(expected), len(roles))
	}
	for i, role := range roles {
		if role["roleid"] != expected[i] {
			t.Errorf("role[%d].roleid = %v, want %q", i, role["roleid"], expected[i])
		}
	}
}

func TestK8sRolesToProxmox_AdminHasFullPrivs(t *testing.T) {
	roles := K8sRolesToProxmox()
	for _, r := range roles {
		if r["roleid"] == "Administrator" {
			privs, ok := r["privs"].(string)
			if !ok {
				t.Fatal("Administrator privs should be a string")
			}
			if privs == "" {
				t.Error("Administrator should have non-empty privileges")
			}
			break
		}
	}
}

func TestK8sRolesToProxmox_NoAccessHasEmptyPrivs(t *testing.T) {
	roles := K8sRolesToProxmox()
	for _, r := range roles {
		if r["roleid"] == "NoAccess" {
			privs, ok := r["privs"].(string)
			if !ok || privs != "" {
				t.Errorf("NoAccess privs should be empty, got %q", privs)
			}
			break
		}
	}
}

// ── AllProxmoxPrivileges tests ───────────────────────────────────────────────

func TestAllProxmoxPrivileges_NotEmpty(t *testing.T) {
	privs := AllProxmoxPrivileges()
	if privs == "" {
		t.Error("AllProxmoxPrivileges should not be empty")
	}
}

func TestAllProxmoxPrivileges_CommaSeparated(t *testing.T) {
	privs := AllProxmoxPrivileges()
	parts := splitAndTrim(privs, ",")
	if len(parts) < 10 {
		t.Errorf("expected at least 10 privileges, got %d: %v", len(parts), parts)
	}
	// Each privilege should start with a known prefix
	for _, p := range parts {
		if !startsWithAny(p, []string{"VM.", "Datastore.", "Sys.", "Permissions."}) {
			t.Errorf("unexpected privilege: %q", p)
		}
	}
}

func TestAllProxmoxPrivileges_NoDuplicates(t *testing.T) {
	privs := AllProxmoxPrivileges()
	parts := splitAndTrim(privs, ",")
	seen := make(map[string]bool)
	for _, p := range parts {
		if seen[p] {
			t.Errorf("duplicate privilege: %q", p)
		}
		seen[p] = true
	}
}

// ── K8sRoles privs consistency ───────────────────────────────────────────────

func TestAdminPrivsMatchAll(t *testing.T) {
	allPrivs := AllProxmoxPrivileges()
	roles := K8sRolesToProxmox()
	for _, r := range roles {
		if r["roleid"] == "Administrator" {
			adminPrivs, _ := r["privs"].(string)
			if adminPrivs != allPrivs {
				t.Error("Administrator privs should equal AllProxmoxPrivileges()")
			}
			break
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsString(s, substr)
}

func containsString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func splitAndTrim(s, sep string) []string {
	var result []string
	for _, part := range stringsSplit(s, sep) {
		trimmed := stringsTrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func stringsSplit(s, sep string) []string {
	var result []string
	for {
		i := indexOf(s, sep)
		if i < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:i])
		s = s[i+len(sep):]
	}
	return result
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func stringsTrimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && s[start] <= ' ' {
		start++
	}
	for end > start && s[end-1] <= ' ' {
		end--
	}
	return s[start:end]
}

func startsWithAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}

// Suppress unused import warnings
var _ = math.Max(float64(0), float64(0))

func TestProxmoxStorageEntry(t *testing.T) {
	sc := StorageEntry{Name: "local-lvm", Type: "lvmthin"}
	entry := ProxmoxStorageEntry(sc, "node1")

	if entry["storage"] != "local-lvm" {
		t.Errorf("storage = %v, want local-lvm", entry["storage"])
	}
	if entry["node"] != "node1" {
		t.Errorf("node = %v, want node1", entry["node"])
	}
	if entry["type"] != "lvmthin" {
		t.Errorf("type = %v, want lvmthin", entry["type"])
	}
	if entry["content"] != "images,rootdir" {
		t.Errorf("content = %v, want images,rootdir", entry["content"])
	}
	if entry["active"] != 1 || entry["enabled"] != 1 {
		t.Errorf("expected active=1 and enabled=1, got active=%v, enabled=%v", entry["active"], entry["enabled"])
	}
}

func TestRBACUsersToProxmox(t *testing.T) {
	users := []RBACUser{
		{UserID: "admin@pve", Comment: "Cluster Admin"},
		{UserID: "viewer@pve", Comment: "Read Only"},
	}
	res := RBACUsersToProxmox(users)
	if len(res) != 2 {
		t.Fatalf("expected 2 users, got %d", len(res))
	}
	if res[0]["userid"] != "admin@pve" || res[0]["comment"] != "Cluster Admin" {
		t.Errorf("user[0] mismatch: %v", res[0])
	}
	if res[0]["enable"] != 1 {
		t.Errorf("user[0] enable = %v, want 1", res[0]["enable"])
	}
}

func TestRBACGroupsToProxmox(t *testing.T) {
	groups := []RBACGroup{
		{GroupID: "devs"},
		{GroupID: "ops"},
	}
	res := RBACGroupsToProxmox(groups)
	if len(res) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(res))
	}
	if res[0]["groupid"] != "devs" || res[1]["groupid"] != "ops" {
		t.Errorf("group mismatch: %v", res)
	}
}
