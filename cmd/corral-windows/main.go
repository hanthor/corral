// corral-windows is the Windows-VM Corral plugin: it creates a VM configured
// the way Windows wants it — q35, UEFI, a TPM 2.0, Hyper-V enlightenments, and
// the virtio-win drivers reachable by Setup. Installed via the marketplace
// (`corral plugin install windows`) and invoked as `corral windows`.
//
// Three backends, three different ways of getting there (#132):
//
//   - **KubeVirt** — the original path, unchanged: the installer ISO imported
//     via CDI, the drivers as a containerdisk, the answer file as a ConfigMap
//     CD-ROM. The cluster supplies UEFI and the TPM.
//   - **libvirt** — a q35 domain with UEFI firmware and an emulated TPM, the
//     drivers as a second CD-ROM, and the answer file on a small ISO Corral
//     builds on the host.
//   - **Incus** — a VM gets one CD-ROM, so the drivers must be slipstreamed
//     into the installer image first. Corral drives distrobuilder's
//     repack-windows for that, and refuses with the exact command when it is
//     absent rather than producing an installer that cannot see its disk.
//
// `corral windows requirements` reports what the selected backend needs before
// anything is created — the alternative is finding out forty minutes into an
// install, at a compatibility check that says nothing about firmware.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/registry"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
	"github.com/tuna-os/corral/pkg/windows"
)

var runner shell.Runner = shell.Real{}

// VirtioWinImage is re-exported for tests; the manifest logic lives in
// pkg/kubevirt so the web GUI shares it.
const VirtioWinImage = kubevirt.VirtioWinImage

// generateWindowsVM delegates to the shared package.
func generateWindowsVM(name, ns, mem string, cpu int, unattended bool) map[string]any {
	return kubevirt.GenerateWindowsVM(name, ns, mem, cpu, unattended)
}

// createWindowsVM imports the installer ISO, provisions the boot disk, and
// applies the Windows-tuned VM (exposing SSH/VNC/RDP via the tailnet proxy),
// then records it in the local registry and prints next steps. With
// unattended (the default), Setup runs with zero interactive prompts via
// an injected autounattend.xml — see pkg/kubevirt/autounattend.go — and
// the generated Administrator password is saved to the registry the same
// way cloud-init VM passwords already are, so `corral ssh`/RDP just work
// once install finishes.
func createWindowsVM(name, ns, iso, disk, mem string, cpu int, unattended bool) error {
	fmt.Fprintf(os.Stderr, "installer ISO importing: %s-iso ← %s\n", name, iso)
	password, err := kubevirt.CreateWindowsVM(name, ns, iso, disk, mem, cpu, unattended)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "SSH/VNC/RDP exposed via proxy service %s-proxy\n", name)
	if store, err := registry.NewStore(); err == nil {
		store.Set(name, types.RegistryEntry{
			Backend:   "kubevirt",
			Namespace: ns,
			Username:  "Administrator",
			Password:  password,
		})
	}
	if unattended {
		fmt.Fprintf(os.Stderr, `VM %q created (stopped). Setup will run unattended — no interactive
clicks needed. Next steps:
  1. wait for the ISO import:   corral images
  2. start it:                  corral start %s
  3. Administrator password:    %s  (saved — corral ssh/RDP will use it automatically)
  4. once Setup finishes rebooting, connect:  corral web → Console, or corral vdi connect %s
`, name, name, password, name)
		return nil
	}
	fmt.Fprintf(os.Stderr, `VM %q created (stopped). Next steps:
  1. wait for the ISO import:   corral images
  2. start it:                  corral start %s
  3. open the console:          corral web → Console (or corral viewer %s)
  4. in Windows Setup, click "Load driver" → browse the virtio CD-ROM
     (E:\amd64\<your windows version>) to see the disk.
`, name, name, name)
	return nil
}

// attachDrivers adds the virtio-win driver CD-ROM to an existing VM via a
// JSON patch appending a disk + matching containerDisk volume.
func attachDrivers(vm, ns string) error {
	patch := fmt.Sprintf(`[
      {"op":"add","path":"/spec/template/spec/domain/devices/disks/-","value":{"name":"virtio-drivers","cdrom":{"bus":"sata"}}},
      {"op":"add","path":"/spec/template/spec/volumes/-","value":{"name":"virtio-drivers","containerDisk":{"image":%q}}}
    ]`, VirtioWinImage)
	out, err := runner.Run("kubectl", "patch", "vm", vm, "-n", ns, "--type", "json", "-p", patch)
	if err != nil {
		return fmt.Errorf("attaching drivers: %s", strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(os.Stderr, "virtio-win drivers attached to %s (takes effect on next boot)\n", vm)
	return nil
}

// target resolves the Corral context to create in.
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

// resolveVirtioISO returns a local virtio-win.iso, fetching it from the
// official Fedora virt group location if it is not already cached. Windows
// Setup cannot see a virtio disk without it, so this is not optional — it is
// just tedious, which is why Corral does it.
func resolveVirtioISO(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	cache := filepath.Join(qemu.VMHome(), "cache")
	return windows.EnsureVirtioISO(cache, downloadTo)
}

func downloadTo(url, dest string) error {
	fmt.Fprintf(os.Stderr, "downloading %s …\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	// Write to a temp file first so an interrupted download never leaves a
	// truncated ISO in the cache to be reused forever.
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// requirementsCmd answers "what does this backend need from me" before an
// install is attempted, rather than after Windows Setup refuses at its
// compatibility check.
func requirementsCmd(contextName *string) *cobra.Command {
	return &cobra.Command{
		Use:   "requirements",
		Short: "Show what this context needs to install Windows",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			ctx, err := target(*contextName)
			if err != nil {
				return err
			}
			if ctx.Backend == "kubevirt" {
				fmt.Println("kubevirt: UEFI, TPM, and the virtio-win containerdisk are all provided by the cluster.")
				return nil
			}
			adapter, err := windows.For(ctx.Backend)
			if err != nil {
				return err
			}
			req := adapter.Requires()
			fmt.Printf("%s (context %q)\n", ctx.Backend, ctx.Name)
			fmt.Printf("  firmware:     UEFI=%v SecureBoot=%v TPM2.0=%v\n",
				req.Firmware.UEFI, req.Firmware.SecureBoot, req.Firmware.TPM)
			fmt.Printf("  drivers:      %s\n", req.Drivers)
			fmt.Printf("  answer file:  %s\n", req.AnswerFileMedium)
			fmt.Println("  needs:")
			for _, p := range req.Prerequisites {
				fmt.Printf("    - %s\n", p)
			}
			return nil
		},
	}
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "windows", Version: "0.2.0", Description: "Windows VM creation workflow", Capabilities: []string{"cli-command", "backend-workflow"}, Permissions: []string{"mutate Kubernetes resources and instance definitions", "download installer and driver images", "write disk images to the host"}, SupportedBackends: []string{"kubevirt", "libvirt", "incus"}}) {
		return
	}
	var (
		namespace, iso, disk, mem string
		contextName, virtioISO    string
		cpu                       int
		unattended                bool
	)

	create := &cobra.Command{
		Use:   "create <name> --iso <windows-iso-url>",
		Short: "Create a Windows-ready VM (UEFI, TPM, virtio drivers, Hyper-V enlightenments)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if iso == "" {
				return fmt.Errorf("--iso is required (a Windows installer ISO: a URL for KubeVirt, a path for libvirt/Incus)")
			}
			ctx, err := target(contextName)
			if err != nil {
				return err
			}
			if ctx.Backend == "kubevirt" {
				return createWindowsVM(args[0], namespace, iso, disk, mem, cpu, unattended)
			}
			adapter, err := windows.For(ctx.Backend)
			if err != nil {
				return err
			}
			virtio, err := resolveVirtioISO(virtioISO)
			if err != nil {
				return err
			}
			spec := windows.Spec{
				Name: args[0], CPU: cpu, Memory: mem, DiskSize: disk,
				InstallerISO: iso, VirtioISO: virtio, Unattended: unattended,
			}
			ref := types.InstanceRef{Backend: ctx.Backend, Context: ctx.Context, Name: args[0]}
			result, err := adapter.Define(ref, spec)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "defined %s — start it to begin Windows Setup\n", ref.String())
			if result.AdminPassword != "" {
				fmt.Fprintf(os.Stderr, "Administrator password: %s\n", result.AdminPassword)
			}
			return nil
		},
	}
	create.Flags().StringVar(&iso, "iso", "", "Windows installer ISO URL (imported via CDI)")
	create.Flags().StringVar(&disk, "disk", "64Gi", "Boot disk size")
	create.Flags().StringVar(&mem, "mem", "8Gi", "Memory")
	create.Flags().IntVar(&cpu, "cpu", 4, "vCPU cores")
	create.Flags().StringVar(&virtioISO, "virtio-iso", "", "Path to virtio-win.iso (downloaded to the cache if omitted; libvirt/Incus only)")
	create.Flags().BoolVar(&unattended, "unattended", true, "Run Windows Setup unattended via an injected autounattend.xml (no interactive prompts)")

	drivers := &cobra.Command{
		Use:   "drivers <vm>",
		Short: "Attach the virtio-win driver CD-ROM to an existing VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return attachDrivers(args[0], namespace)
		},
	}

	root := &cobra.Command{
		Use:   "corral-windows",
		Short: "Corral plugin — first-class Windows VMs on KubeVirt",
	}
	root.PersistentFlags().StringVarP(&namespace, "namespace", "n", kubevirt.DefaultNamespace, "Namespace (KubeVirt only)")
	root.PersistentFlags().StringVar(&contextName, "context", "", "Corral context to create in (default: the selected context)")
	root.AddCommand(create, drivers, requirementsCmd(&contextName))
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
