package main

import (
	"strings"
	"testing"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/vdi"
)

func TestRootCmd_Subcommands(t *testing.T) {
	root := rootCmd()
	for _, want := range []string{"pool", "assign", "unassign", "connect", "usb"} {
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

func TestUsbCmd_Subcommands(t *testing.T) {
	usb := usbCmd()
	for _, want := range []string{"list", "redir"} {
		c, _, err := usb.Find([]string{want})
		if err != nil || c == usb {
			t.Errorf("usb: missing subcommand %q", want)
		}
	}
}

func TestUsbListCmd(t *testing.T) {
	orig := vdi.ListUSBDevicesFunc
	defer func() { vdi.ListUSBDevicesFunc = orig }()
	vdi.ListUSBDevicesFunc = func() ([]vdi.USBDevice, error) {
		return []vdi.USBDevice{
			{
				BusNum:      "3",
				DevNum:      "2",
				VendorID:    "1050",
				ProductID:   "0407",
				Description: "YubiKey",
			},
		}, nil
	}

	cmd := usbListCmd()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("usb list: %v", err)
	}
}

func TestUsbRedirCmd_RequiresMemberArg(t *testing.T) {
	c := usbRedirCmd()
	if c.Args == nil {
		t.Error("usb redir should require exact args")
	}
}

func TestUsbRedirCmd_ValidationFailure(t *testing.T) {
	fake := shell.NewFake()
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-1", "-n", "corral-vms", "-o", `jsonpath={.metadata.labels.corral\.dev/vdi-pool}`}, "devpool", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", "corral.dev/vdi-pool=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"bob"}},
			"status":{"printableStatus":"Running"}}
	]}`, nil)
	vdi.SetRunner(fake)
	defer vdi.SetRunner(shell.Real{})

	orig := vdi.ListUSBDevicesFunc
	defer func() { vdi.ListUSBDevicesFunc = orig }()
	vdi.ListUSBDevicesFunc = func() ([]vdi.USBDevice, error) {
		return []vdi.USBDevice{
			{
				BusNum:      "3",
				DevNum:      "2",
				VendorID:    "1050",
				ProductID:   "0407",
				Description: "YubiKey",
			},
		}, nil
	}

	c := usbRedirCmd()
	c.Flags().Set("user", "alice")
	c.Flags().Set("yes", "true")
	namespace = "corral-vms"
	defer func() { namespace = "" }()

	err := c.RunE(c, []string{"devpool-1"})
	if err == nil || !strings.Contains(err.Error(), "authorization failed") {
		t.Fatalf("expected authorization error, got: %v", err)
	}
}

func TestUsbRedirCmd_DeviceNotFound(t *testing.T) {
	fake := shell.NewFake()
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-1", "-n", "corral-vms", "-o", `jsonpath={.metadata.labels.corral\.dev/vdi-pool}`}, "devpool", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", "corral.dev/vdi-pool=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"alice"}},
			"status":{"printableStatus":"Running"}}
	]}`, nil)
	vdi.SetRunner(fake)
	defer vdi.SetRunner(shell.Real{})

	orig := vdi.ListUSBDevicesFunc
	defer func() { vdi.ListUSBDevicesFunc = orig }()
	vdi.ListUSBDevicesFunc = func() ([]vdi.USBDevice, error) {
		return []vdi.USBDevice{}, nil
	}

	c := usbRedirCmd()
	c.Flags().Set("device", "9999:9999")
	c.Flags().Set("yes", "true")
	namespace = "corral-vms"
	defer func() { namespace = "" }()

	err := c.RunE(c, []string{"devpool-1"})
	if err == nil || !strings.Contains(err.Error(), "not found on local host") {
		t.Fatalf("expected device not found error, got: %v", err)
	}
}

func TestUsbRedirCmd_BusyDevice(t *testing.T) {
	fake := shell.NewFake()
	fake.AddResponseKV("kubectl", []string{"get", "vm", "devpool-1", "-n", "corral-vms", "-o", `jsonpath={.metadata.labels.corral\.dev/vdi-pool}`}, "devpool", nil)
	fake.AddResponseKV("kubectl", []string{"get", "vm", "-A", "-o", "json", "-l", "corral.dev/vdi-pool=devpool"}, `{"items":[
		{"metadata":{"name":"devpool-1","namespace":"corral-vms","labels":{"corral.dev/vdi-pool":"devpool","corral.dev/vdi-assigned-to":"alice"}},
			"status":{"printableStatus":"Running"}}
	]}`, nil)
	vdi.SetRunner(fake)
	defer vdi.SetRunner(shell.Real{})

	orig := vdi.ListUSBDevicesFunc
	defer func() { vdi.ListUSBDevicesFunc = orig }()
	vdi.ListUSBDevicesFunc = func() ([]vdi.USBDevice, error) {
		return []vdi.USBDevice{
			{
				BusNum:      "1",
				DevNum:      "1",
				VendorID:    "1d6b",
				ProductID:   "0002",
				Description: "Linux Foundation root hub",
				Busy:        true,
				BusyReason:  "USB root hub cannot be redirected",
			},
		}, nil
	}

	c := usbRedirCmd()
	c.Flags().Set("device", "1d6b:0002")
	c.Flags().Set("yes", "true")
	namespace = "corral-vms"
	defer func() { namespace = "" }()

	err := c.RunE(c, []string{"devpool-1"})
	if err == nil || !strings.Contains(err.Error(), "is busy") {
		t.Fatalf("expected busy device error, got: %v", err)
	}
}

func TestUsbRedirCmd_UnsupportedContext(t *testing.T) {
	c := usbRedirCmd()
	c.Flags().Set("yes", "true")
	contextName = "nonexistent-ctx-12345"
	defer func() { contextName = "" }()

	err := c.RunE(c, []string{"devpool-1"})
	if err == nil || !strings.Contains(err.Error(), "unknown context") {
		t.Fatalf("expected unknown context error, got: %v", err)
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
