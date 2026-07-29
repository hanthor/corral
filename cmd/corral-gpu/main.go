// corral-gpu is the GPU/PCI-passthrough Corral plugin: it discovers
// passthrough-capable devices, registers them in the KubeVirt CR
// (permittedHostDevices), and attaches them to VMs (spec.domain.devices.gpus).
// Installed via the marketplace (`corral plugin install gpu`) and invoked as
// `corral gpu`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/shell"
)

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

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "gpu", Version: "0.1.0", Description: "KubeVirt GPU and PCI passthrough", Capabilities: []string{"cli-command", "gpu"}, Permissions: []string{"mutate KubeVirt configuration and VMs"}, SupportedBackends: []string{"kubevirt"}}) {
		return
	}
	var namespace string

	list := &cobra.Command{
		Use:   "list",
		Short: "Show passthrough-capable device resources and what KubeVirt permits",
		RunE: func(cmd *cobra.Command, args []string) error {
			nodes, err := nodeDeviceResources()
			if err != nil {
				return err
			}
			if len(nodes) == 0 {
				fmt.Println("no device-plugin resources found on any node")
			} else {
				names := make([]string, 0, len(nodes))
				for n := range nodes {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					fmt.Printf("%s:\n", n)
					for res, qty := range nodes[n] {
						fmt.Printf("  %s: %s\n", res, qty)
					}
				}
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
		Short: "Permit a PCI device for passthrough in the KubeVirt CR",
		RunE: func(cmd *cobra.Command, args []string) error {
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
		Use:   "attach <vm> --device <resourceName>",
		Short: "Attach a permitted GPU/device to a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			device, _ := cmd.Flags().GetString("device")
			if device == "" {
				return fmt.Errorf("--device is required (e.g. amd.com/gpu)")
			}
			return attachGPU(args[0], namespace, device, gpuName)
		},
	}
	attach.Flags().String("device", "", "Device resource name (see `corral gpu list`)")
	attach.Flags().StringVar(&gpuName, "name", "", "Device name inside the VM (default gpuN)")

	detach := &cobra.Command{
		Use:   "detach <vm> --name <gpuN>",
		Short: "Detach a GPU/device from a VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if gpuName == "" {
				return fmt.Errorf("--name is required")
			}
			return detachGPU(args[0], namespace, gpuName)
		},
	}
	detach.Flags().StringVar(&gpuName, "name", "", "Device name to remove")

	root := &cobra.Command{
		Use:   "corral-gpu",
		Short: "Corral plugin — discover and attach PCI/vGPU passthrough devices",
	}
	root.PersistentFlags().StringVarP(&namespace, "namespace", "n", kubevirt.DefaultNamespace, "Namespace")
	root.AddCommand(list, enable, attach, detach)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
