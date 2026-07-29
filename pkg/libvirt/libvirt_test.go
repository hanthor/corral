package libvirt

import (
	"github.com/tuna-os/corral/pkg/shell"
	"testing"
)

func TestListRemoteURI(t *testing.T) {
	f := shell.NewFake()
	uri := "qemu+ssh://lab/system"
	f.AddResponseKV("virsh", []string{"-c", uri, "list", "--all", "--name"}, "desk\n", nil)
	f.AddResponseKV("virsh", []string{"-c", uri, "domstate", "desk"}, "running\n", nil)
	f.AddResponseKV("virsh", []string{"-c", uri, "dominfo", "desk"}, "CPU(s): 4\nMax memory: 8388608 KiB\n", nil)
	SetRunner(f)
	defer SetRunner(shell.Real{})
	vms, err := NewClient(uri).List()
	if err != nil || len(vms) != 1 {
		t.Fatalf("List: %v %+v", err, vms)
	}
	if vms[0].Backend != "libvirt" || vms[0].Context != uri || !vms[0].Running {
		t.Fatalf("bad domain: %+v", vms[0])
	}
}
