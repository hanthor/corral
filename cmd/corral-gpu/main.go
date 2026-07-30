// corral-gpu is the GPU/PCI-passthrough Corral plugin: it discovers
// passthrough-capable devices and attaches them to instances, on KubeVirt,
// libvirt, or Incus (#129). Installed via the marketplace
// (`corral plugin install gpu`) and invoked as `corral gpu`.
//
// Discovery and attachment go through pkg/device. `enable` stays KubeVirt-only
// because it edits the KubeVirt CR's permittedHostDevices allowlist, which has
// no libvirt or Incus counterpart — those backends attach a PCI address
// directly, with no cluster-level allowlist in between.
//
// Attachment is the most destructive thing Corral does, so it prints what will
// happen and asks first. --yes skips the prompt for scripting; nothing skips
// the refusal to pass through the host's boot display.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/device"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

// target resolves the Corral context these commands act on.
func target(contextName string) (config.ContextConfig, error) {
	if contextName == "" {
		return config.DefaultContext(), nil
	}
	found, ok := config.FindContext(contextName)
	if !ok {
		return config.ContextConfig{}, fmt.Errorf("unknown context %q (see corral context ls)", contextName)
	}
	return found, nil
}

// confirm prints what an attachment will do and waits for agreement. The
// acceptance criteria ask for restart/migration/security implications to be
// surfaced before mutation — printing them after would be no use to anyone.
func confirm(dev device.Device, consequences []device.Consequence, assumeYes bool) error {
	fmt.Fprintf(os.Stderr, "\nAbout to pass through %s (%s)\n", dev.ID, dev.Description)
	for _, c := range consequences {
		fmt.Fprintf(os.Stderr, "  • %s\n", c)
	}
	if assumeYes {
		return nil
	}
	fmt.Fprint(os.Stderr, "\nProceed? [y/N] ")
	var answer string
	fmt.Scanln(&answer)
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return fmt.Errorf("cancelled")
	}
	return nil
}

var runner shell.Runner = shell.Real{}

// ── discovery ─────────────────────────────────────────────────────

// nodeDeviceResources returns per-node extended resources that look like
// passthrough devices (vendor-domain resource names), e.g. amd.com/gpu: 1.
func nodeDeviceResources() (map[string]map[string]string, error) {
	out, err := runner.Run("kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %s", strings.TrimSpace(string(out)))
	}
	var res struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	nodes := map[string]map[string]string{}
	for _, n := range res.Items {
		devs := map[string]string{}
		for k, v := range n.Status.Allocatable {
			if isDeviceResource(k) {
				devs[k] = v
			}
		}
		if len(devs) > 0 {
			nodes[n.Metadata.Name] = devs
		}
	}
	return nodes, nil
}

// isDeviceResource reports whether a node resource name is a device-plugin
// resource worth offering for passthrough (vendor-domain qualified, not one of
// the standard kubelet resources or KubeVirt's own virtualization devices).
func isDeviceResource(name string) bool {
	if !strings.Contains(name, "/") {
		return false // cpu, memory, pods, ephemeral-storage, hugepages-*
	}
	if strings.HasPrefix(name, "devices.kubevirt.io/") {
		return false // kvm, tun, vhost-net — plumbing, not passthrough targets
	}
	return true
}

// permittedDevice is one pciHostDevices/mediatedDevices entry in the KubeVirt CR.
type permittedDevice struct {
	PCIVendorSelector        string `json:"pciVendorSelector,omitempty"`
	MdevNameSelector         string `json:"mdevNameSelector,omitempty"`
	ResourceName             string `json:"resourceName"`
	ExternalResourceProvider bool   `json:"externalResourceProvider,omitempty"`
}

type permittedHostDevices struct {
	PCIHostDevices  []permittedDevice `json:"pciHostDevices,omitempty"`
	MediatedDevices []permittedDevice `json:"mediatedDevices,omitempty"`
}

func currentPermitted() (permittedHostDevices, error) {
	out, err := runner.Run("kubectl", "get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "json")
	if err != nil {
		return permittedHostDevices{}, fmt.Errorf("reading KubeVirt CR: %s", strings.TrimSpace(string(out)))
	}
	var kv struct {
		Spec struct {
			Configuration struct {
				PermittedHostDevices permittedHostDevices `json:"permittedHostDevices"`
			} `json:"configuration"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &kv); err != nil {
		return permittedHostDevices{}, err
	}
	return kv.Spec.Configuration.PermittedHostDevices, nil
}

// enableDevice adds a PCI vendor:device selector to permittedHostDevices
// (idempotent — no duplicate selectors or resource names).
func enableDevice(vendorSelector, resourceName string) error {
	cur, err := currentPermitted()
	if err != nil {
		return err
	}
	for _, d := range cur.PCIHostDevices {
		if d.PCIVendorSelector == vendorSelector || d.ResourceName == resourceName {
			fmt.Fprintf(os.Stderr, "already permitted: %s → %s\n", d.PCIVendorSelector, d.ResourceName)
			return nil
		}
	}
	cur.PCIHostDevices = append(cur.PCIHostDevices, permittedDevice{
		PCIVendorSelector: vendorSelector,
		ResourceName:      resourceName,
	})
	patch := map[string]any{
		"spec": map[string]any{
			"configuration": map[string]any{
				"permittedHostDevices": cur,
			},
		},
	}
	body, _ := json.Marshal(patch)
	out, err := runner.Run("kubectl", "patch", "kubevirt", "kubevirt", "-n", "kubevirt",
		"--type", "merge", "-p", string(body))
	if err != nil {
		return fmt.Errorf("patching KubeVirt CR: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ── attach / detach ───────────────────────────────────────────────

type vmGPU struct {
	Name       string `json:"name"`
	DeviceName string `json:"deviceName"`
}

func vmGPUs(vm, ns string) ([]vmGPU, error) {
	out, err := runner.Run("kubectl", "get", "vm", vm, "-n", ns, "-o",
		"jsonpath={.spec.template.spec.domain.devices.gpus}")
	if err != nil {
		return nil, fmt.Errorf("reading VM %s: %s", vm, strings.TrimSpace(string(out)))
	}
	var gpus []vmGPU
	if s := strings.TrimSpace(string(out)); s != "" {
		if err := json.Unmarshal([]byte(s), &gpus); err != nil {
			return nil, err
		}
	}
	return gpus, nil
}

func patchVMGPUs(vm, ns string, gpus []vmGPU) error {
	patch := map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"domain": map[string]any{"devices": map[string]any{"gpus": gpus}},
		}}},
	}
	body, _ := json.Marshal(patch)
	out, err := runner.Run("kubectl", "patch", "vm", vm, "-n", ns,
		"--type", "merge", "-p", string(body))
	if err != nil {
		return fmt.Errorf("patching VM: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func attachGPU(vm, ns, deviceName, name string) error {
	gpus, err := vmGPUs(vm, ns)
	if err != nil {
		return err
	}
	if name == "" {
		name = fmt.Sprintf("gpu%d", len(gpus)+1)
	}
	for _, g := range gpus {
		if g.Name == name {
			return fmt.Errorf("VM %s already has a GPU named %q", vm, name)
		}
	}
	gpus = append(gpus, vmGPU{Name: name, DeviceName: deviceName})
	if err := patchVMGPUs(vm, ns, gpus); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "attached %s as %q to %s (takes effect on next boot)\n", deviceName, name, vm)
	return nil
}

func detachGPU(vm, ns, name string) error {
	gpus, err := vmGPUs(vm, ns)
	if err != nil {
		return err
	}
	kept := []vmGPU{}
	for _, g := range gpus {
		if g.Name != name {
			kept = append(kept, g)
		}
	}
	if len(kept) == len(gpus) {
		return fmt.Errorf("VM %s has no GPU named %q", vm, name)
	}
	return patchVMGPUs(vm, ns, kept)
}

// ── CLI ───────────────────────────────────────────────────────────

// instanceRef builds the canonical reference for the instance a command names.
func instanceRef(ctx config.ContextConfig, name, namespace string) types.InstanceRef {
	ref := types.InstanceRef{Backend: ctx.Backend, Context: ctx.Context, Name: name}
	if ctx.Backend == "kubevirt" {
		ref.Namespace = namespace
	}
	return ref
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "gpu", Version: "0.2.0", Description: "GPU and PCI passthrough", Capabilities: []string{"cli-command", "gpu"}, Permissions: []string{"enumerate host PCI devices", "bind host devices to guests (removes them from the host)", "mutate KubeVirt configuration and instance definitions"}, SupportedBackends: []string{"kubevirt", "libvirt", "incus"}}) {
		return
	}
	var namespace string

	var contextName string
	var assumeYes bool

	list := &cobra.Command{
		Use:   "list",
		Short: "Show passthrough-capable devices in the selected context",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := target(contextName)
			if err != nil {
				return err
			}
			adapter, err := device.For(ctx.Backend)
			if err != nil {
				return err
			}
			devices, err := adapter.List(ctx.Context)
			if err != nil {
				return err
			}
			if len(devices) == 0 {
				fmt.Printf("no passthrough-capable devices found in context %q (%s)\n", ctx.Name, ctx.Backend)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "DEVICE\tCLASS\tIDS\tDRIVER\tNODE\tDESCRIPTION")
			for _, d := range devices {
				note := d.Description
				if d.BootDisplay {
					note += "  [host boot display — cannot be passed through]"
				}
				ids := d.Vendor
				if d.Product != "" {
					ids += ":" + d.Product
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					d.ID, d.Class, orDash(ids), orDash(d.Driver), orDash(d.Node), note)
			}
			w.Flush()

			// The KubeVirt allowlist is a cluster concept with no counterpart
			// elsewhere, so it is only shown where it exists.
			if ctx.Backend != "kubevirt" {
				return nil
			}
			cur, err := currentPermitted()
			if err != nil {
				return err
			}
			fmt.Println("\npermittedHostDevices (KubeVirt CR):")
			if len(cur.PCIHostDevices) == 0 && len(cur.MediatedDevices) == 0 {
				fmt.Println("  none — add with: corral gpu enable --vendor <vid:did> --resource <name>")
			}
			for _, d := range cur.PCIHostDevices {
				fmt.Printf("  pci %s → %s\n", d.PCIVendorSelector, d.ResourceName)
			}
			for _, d := range cur.MediatedDevices {
				fmt.Printf("  mdev %s → %s\n", d.MdevNameSelector, d.ResourceName)
			}
			return nil
		},
	}

	var vendor, resource string
	enable := &cobra.Command{
		Use:   "enable --vendor <vid:did> --resource <vendor.com/name>",
		Short: "Permit a PCI device for passthrough in the KubeVirt CR (KubeVirt only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := target(contextName)
			if err != nil {
				return err
			}
			if ctx.Backend != "kubevirt" {
				return fmt.Errorf("`enable` edits the KubeVirt allowlist, which the %s backend does not have — "+
					"attach a device directly with `corral gpu attach`", ctx.Backend)
			}
			if vendor == "" || resource == "" {
				return fmt.Errorf("--vendor (e.g. 1002:744c) and --resource (e.g. amd.com/gpu) are required")
			}
			return enableDevice(vendor, resource)
		},
	}
	enable.Flags().StringVar(&vendor, "vendor", "", "PCI vendor:device selector (lspci -nn format, e.g. 1002:744c)")
	enable.Flags().StringVar(&resource, "resource", "", "Resource name VMs will request (e.g. amd.com/gpu)")

	var gpuName string
	attach := &cobra.Command{
		Use:   "attach <instance> --device <id>",
		Short: "Attach a passthrough device to an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deviceID, _ := cmd.Flags().GetString("device")
			if deviceID == "" {
				return fmt.Errorf("--device is required (see `corral gpu list`)")
			}
			ctx, err := target(contextName)
			if err != nil {
				return err
			}
			adapter, err := device.For(ctx.Backend)
			if err != nil {
				return err
			}
			// Resolve against discovery rather than trusting the flag: that is
			// what supplies the boot-display flag and the current host driver,
			// and both change what happens next.
			devices, err := adapter.List(ctx.Context)
			if err != nil {
				return err
			}
			var dev device.Device
			for _, d := range devices {
				if d.ID == deviceID || d.Address == deviceID {
					dev = d
					break
				}
			}
			if dev.ID == "" {
				return fmt.Errorf("no device %q in context %q — see `corral gpu list`", deviceID, ctx.Name)
			}
			name := gpuName
			if name == "" {
				name = "gpu0"
			}
			if err := confirm(dev, adapter.Consequences(dev), assumeYes); err != nil {
				return err
			}
			ref := instanceRef(ctx, args[0], namespace)
			if err := adapter.Attach(ref, dev, name); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "attached %s to %s as %q — restart it to take effect\n", dev.ID, ref.String(), name)
			return nil
		},
	}
	attach.Flags().String("device", "", "Device ID or PCI address (see `corral gpu list`)")
	attach.Flags().StringVar(&gpuName, "name", "", "Device name inside the instance (default gpu0)")
	attach.Flags().BoolVar(&assumeYes, "yes", false, "Skip the confirmation prompt")

	detach := &cobra.Command{
		Use:   "detach <instance> --name <name>",
		Short: "Detach a passthrough device from an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if gpuName == "" {
				return fmt.Errorf("--name is required (see `corral gpu attached`)")
			}
			ctx, err := target(contextName)
			if err != nil {
				return err
			}
			adapter, err := device.For(ctx.Backend)
			if err != nil {
				return err
			}
			return adapter.Detach(instanceRef(ctx, args[0], namespace), gpuName)
		},
	}
	detach.Flags().StringVar(&gpuName, "name", "", "Device name to remove")

	attached := &cobra.Command{
		Use:   "attached <instance>",
		Short: "Show the devices currently attached to an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, err := target(contextName)
			if err != nil {
				return err
			}
			adapter, err := device.For(ctx.Backend)
			if err != nil {
				return err
			}
			list, err := adapter.Attached(instanceRef(ctx, args[0], namespace))
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("no devices attached")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDEVICE")
			for _, a := range list {
				fmt.Fprintf(w, "%s\t%s\n", a.Name, a.Device)
			}
			return w.Flush()
		},
	}

	root := &cobra.Command{
		Use:   "corral-gpu",
		Short: "Corral plugin — discover and attach PCI/vGPU passthrough devices",
	}
	root.PersistentFlags().StringVarP(&namespace, "namespace", "n", kubevirt.DefaultNamespace, "Namespace (KubeVirt only)")
	root.PersistentFlags().StringVar(&contextName, "context", "", "Corral context to act on (default: the selected context)")
	root.AddCommand(list, enable, attach, detach, attached)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
