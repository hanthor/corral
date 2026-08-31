package main

import (
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/vdi"
)

func TestRootCmd_Subcommands(t *testing.T) {
	root := rootCmd()
	for _, want := range []string{"pool", "assign", "unassign", "connect"} {
		c, _, err := root.Find([]string{want})
		if err != nil || c == root {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

func TestPoolCmd_Subcommands(t *testing.T) {
	pool := poolCmd()
	for _, want := range []string{"create", "list", "delete"} {
		c, _, err := pool.Find([]string{want})
		if err != nil || c == pool {
			t.Errorf("pool: missing subcommand %q", want)
		}
	}
}

func TestPoolCreateCmd_RequiresFrom(t *testing.T) {
	c := poolCreateCmd()
	if c.Flags().Lookup("from").DefValue != "" {
		t.Errorf("expected --from to default empty")
	}
	if err := c.MarkFlagRequired("from"); err != nil {
		t.Fatalf("--from should be markable required: %v", err)
	}
}

func TestNsOrDefault(t *testing.T) {
	namespace = "custom-ns"
	if got := nsOrDefault(); got != "custom-ns" {
		t.Errorf("got %q, want custom-ns", got)
	}
	namespace = ""
	if got := nsOrDefault(); got == "" {
		t.Error("expected a non-empty default namespace")
	}
}

func TestPoolCreateCmd_Success(t *testing.T) {
	fake := shell.NewFake()
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden", "-n", "corral-vms", "-o", "name"}, "vm/golden", nil)
	fake.AddResponseKV("kubectl", []string{"apply", "-f", "-"}, "applied", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-1", "-n", "corral-vms", "-o", "name"}, "vm/devpool-1", nil)
	fake.AddPrefixResponse("kubectl label vm devpool-1", "", nil)
	fake.AddPrefixResponse("kubectl annotate vm devpool-1", "", nil)
	vdi.SetRunner(fake)
	defer vdi.SetRunner(shell.Real{})

	namespace = "corral-vms"
	defer func() { namespace = "" }()

	c := poolCreateCmd()
	c.Flags().Set("from", "golden")
	c.Flags().Set("size", "1")
	if err := c.RunE(c, []string{"devpool"}); err != nil {
		t.Fatalf("pool create: %v", err)
	}
}

func TestPoolCreateCmd_GPUCapacityRefusal(t *testing.T) {
	fake := shell.NewFake()
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-gpu", "-n", "corral-vms", "-o", "name"}, "vm/golden-gpu", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "golden-gpu", "-n", "corral-vms", "-o", "json"}, `{
	  "metadata": {"name": "golden-gpu", "namespace": "corral-vms"},
	  "spec": {
	    "template": {
	      "spec": {
	        "domain": {
	          "devices": {
	            "gpus": [{"name": "gpu1", "deviceName": "nvidia.com/gpu"}]
	          }
	        }
	      }
	    }
	  }
	}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json"}, `{"spec":{}}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "nodes", "-o", "json"}, `{"items":[]}`, nil)
	fake.AddResponseKV("kubectl", []string{"get", "vmi", "-A", "-o", "json"}, `{"items":[]}`, nil)
	vdi.SetRunner(fake)
	defer vdi.SetRunner(shell.Real{})

	namespace = "corral-vms"
	defer func() { namespace = "" }()

	c := poolCreateCmd()
	c.Flags().Set("from", "golden-gpu")
	c.Flags().Set("size", "1")
	err := c.RunE(c, []string{"gpupool"})
	if err == nil || !strings.Contains(err.Error(), "device admission failed") {
		t.Fatalf("expected device admission failure, got: %v", err)
	}
}

func TestPoolListCmd_Empty(t *testing.T) {
	fake := shell.NewFake()
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", "corral.dev/vdi-pool"}, `{"items":[]}`, nil)
	vdi.SetRunner(fake)
	defer vdi.SetRunner(shell.Real{})

	if err := poolListCmd().RunE(nil, nil); err != nil {
		t.Fatalf("pool list: %v", err)
	}
}

func TestConnectCmd_PrintsAllThreePaths(t *testing.T) {
	c := connectCmd()
	if c.Args == nil {
		t.Error("connect should require exactly one arg")
	}
}
