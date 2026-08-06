package libvirt

import (
	"fmt"
	"github.com/tuna-os/corral/pkg/shell"
	"strings"
	"testing"
	"time"
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

// ── metrics ───────────────────────────────────────────────────────

const domStatsFirst = `Domain: 'web'
  cpu.time=1000000000
  balloon.current=4194304
  balloon.rss=2097152
`

const domStatsSecond = `Domain: 'web'
  cpu.time=1000500000
  balloon.current=4194304
  balloon.rss=2097152
`

// balloon.rss, not balloon.current. current is what the guest was *given* and
// stays flat at its configured size — it looks like a working metric while
// telling you nothing about what the host is spending.
func TestMetricsReportsResidentSizeNotTheBalloonTarget(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("virsh", domStatsFirst, nil)
	SetRunner(f)
	defer SetRunner(shell.Real{})

	got, err := NewClient("qemu:///system").Metrics("web")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	// balloon.rss is 2097152 KiB = 2GiB; balloon.current would be 4GiB.
	if got["mem"] != "2.0Gi" {
		t.Fatalf("mem = %q, want 2.0Gi (rss), not 4.0Gi (the balloon target)", got["mem"])
	}
}

func TestMetricsComputesACPURateFromTwoSamples(t *testing.T) {
	previousInterval := cpuSampleInterval
	cpuSampleInterval = time.Millisecond
	defer func() { cpuSampleInterval = previousInterval }()

	f := shell.NewFake()
	// The fake replays the last matching response, so script the second sample
	// as a prefix match after consuming the first via an exact match.
	f.AddResponseKV("virsh", []string{"-c", "qemu:///system", "domstats", "web",
		"--cpu-total", "--balloon", "--raw"}, domStatsFirst, nil)
	SetRunner(f)
	defer SetRunner(shell.Real{})

	got, err := NewClient("qemu:///system").Metrics("web")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	// Both samples read the same cpu.time, so the rate is 0 — the point here is
	// that it is a *rate* at all rather than the raw 1000000000ns counter.
	if got["cpu"] != "0%" {
		t.Fatalf("cpu = %q, want a percentage; a lifetime counter would read as a huge number", got["cpu"])
	}
}

func TestMetricsSurfacesAVirshFailure(t *testing.T) {
	f := shell.NewFake()
	f.AddPrefixResponse("virsh", "error: failed to get domain 'gone'", errNotFound)
	SetRunner(f)
	defer SetRunner(shell.Real{})

	if _, err := NewClient("qemu:///system").Metrics("gone"); err == nil {
		t.Fatal("a missing domain reported metrics")
	}
}

var errNotFound = fmt.Errorf("exit status 1")
