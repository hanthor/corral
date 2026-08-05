package libvirt

import (
	"github.com/tuna-os/corral/pkg/shell"
	"strings"
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

// Pause must be virsh suspend, not destroy. Both stop the domain running;
// only one keeps its memory, and confusing them turns a pause into a hard
// power cut.
func TestPauseAndResumeUseSuspendAndResume(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("virsh", "", nil)
	SetRunner(f)
	defer SetRunner(shell.Real{})

	client := NewClient("qemu:///system")
	if err := client.Pause("web"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := client.Resume("web"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	var verbs []string
	for _, call := range f.Calls() {
		// args are -c <uri> <verb> <domain>
		if call.Name == "virsh" && len(call.Args) >= 3 {
			verbs = append(verbs, call.Args[2])
		}
	}
	if strings.Join(verbs, ",") != "suspend,resume" {
		t.Fatalf("verbs = %v, want [suspend resume]", verbs)
	}
	for _, call := range f.Calls() {
		for _, arg := range call.Args {
			if arg == "destroy" {
				t.Fatal("pause used virsh destroy, which is a power cut, not a suspend")
			}
		}
	}
}
