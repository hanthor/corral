// corral-bootc is the bootc Corral plugin: it builds a bootable container
// image into a VM disk and boots it. Installed via the Corral marketplace
// (`corral plugin install bootc`) and invoked as `corral bootc`.
//
// Two paths, chosen by the selected context (#130):
//
//   - **KubeVirt** — the original: a privileged Job on the cluster runs
//     `bootc install to-disk` into a PVC, and the VM is created around it.
//     The cluster does the work, so the workstation need not be able to.
//   - **Everything else** — pkg/bootc builds the disk here with podman and
//     hands it to the backend's target: local QEMU adopts it as a VM's disk,
//     libvirt defines a UEFI domain around it. Incus is a documented
//     rejection; see bootc.TargetFor.
//
// It's a separate binary built with `-tags bootc`, so the cluster pipeline in
// pkg/kubevirt (//go:build bootc) is registered here but stays out of the lean
// core binary.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/bootc"
	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/registry"
	"github.com/tuna-os/corral/pkg/types"
)

// finishVM applies the final VM from a completed build's disk PVC, exposes
// it on the tailnet, and records it in the registry — the part of `create`
// that's shared between a fresh build and a resumed one (--resume).
func finishVM(name, ns, pvcName, image, key, mem string, cpu int, node string) error {
	vm := kubevirt.GenerateBootcVM(name, ns, pvcName, image, key, mem, cpu, node)
	if vm == nil {
		return fmt.Errorf("bootc VM manifest unavailable")
	}
	if err := kubevirt.Apply(vm); err != nil {
		return fmt.Errorf("creating VM: %w", err)
	}
	// Expose SSH/VNC/RDP on the tailnet via the Tailscale operator proxy.
	if err := kubevirt.ApplyProxy(name, ns, kubevirt.ConsolePorts); err != nil {
		fmt.Fprintf(os.Stderr, "warning: tailnet expose failed: %v\n", err)
	}
	if store, err := registry.NewStore(); err == nil {
		store.Set(name, types.RegistryEntry{
			Backend:   "kubevirt",
			Namespace: ns,
		})
	}
	fmt.Fprintf(os.Stderr, "VM %q created in ns/%s — start: corral start %s\n", name, ns, name)
	return nil
}

// resolveContext picks the Corral context to build for.
func resolveContext(name string) (config.ContextConfig, error) {
	if name == "" {
		return config.DefaultContext(), nil
	}
	found, ok := config.FindContext(name)
	if !ok {
		return config.ContextConfig{}, fmt.Errorf("unknown context %q (see corral context ls)", name)
	}
	return found, nil
}

// buildAndImport is the off-cluster path: build the disk here with podman,
// then hand it to the backend's target. The split is the point of #130 — the
// same built disk works for local QEMU and for libvirt, and neither needs a
// cluster to produce it.
func buildAndImport(name string, target config.ContextConfig, image, sshKey, disk, mem string, cpu int, filesystem string, useSudo bool) error {
	if image == "" {
		return fmt.Errorf("--image is required — a catalog name (see `corral bootc images`) or an OCI ref like quay.io/fedora/fedora-bootc:41")
	}
	image = catalog.ResolveBootc(image)

	importer, err := bootc.TargetFor(target.Backend)
	if err != nil {
		return err
	}
	builder := bootc.LocalBuilder{Sudo: useSudo}
	if err := builder.Available(); err != nil {
		// Report what the target needs too, so one run tells the operator
		// everything to install rather than one thing per attempt.
		fmt.Fprintf(os.Stderr, "the %s target also needs:\n", target.Backend)
		for _, req := range importer.Requires() {
			fmt.Fprintf(os.Stderr, "  - %s\n", req)
		}
		return err
	}

	key := sshKey
	if key == "" {
		key = kubevirt.LoadSSHPublicKey()
	}
	size := disk
	if size == "" {
		size = "20G"
	}

	// The disk is built into the image cache and consumed by Import, which
	// converts it into the backend's own storage.
	workDir := filepath.Join(qemu.VMHome(), "cache", "bootc")
	built, err := builder.Build(bootc.BuildRequest{
		Image: image, Dest: filepath.Join(workDir, name+".raw"),
		Size: size, SSHKey: key, Filesystem: filesystem,
	}, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	if err != nil {
		return err
	}
	defer os.Remove(built.Path)

	ref := types.InstanceRef{Backend: target.Backend, Context: target.Context, Name: name}
	if err := importer.Import(ref, built.Path, bootc.CreateOpts{
		CPU: cpu, Memory: mem, Disk: size, SSHKey: key,
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created %s from %s\n", ref.String(), image)
	return nil
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "bootc", Version: "0.3.0", Description: "Build bootable container images into VMs", Capabilities: []string{"cli-command", "backend-workflow"}, Permissions: []string{"mutate Kubernetes resources and instance definitions", "pull container images", "run privileged containers to partition a disk image"}, SupportedBackends: bootc.SupportedBackends}) {
		return
	}
	var (
		namespace, disk, mem, sshKey, node, storageClass string
		contextName, filesystem                          string
		cpu                                              int
		resume, useSudo                                  bool
	)

	create := &cobra.Command{
		Use:   "create <name> --image <bootc-image>",
		Short: "Build a bootc container image into a VM and create it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ns := namespace
			if ns == "" {
				ns = kubevirt.DefaultNamespace
			}
			// Off-cluster backends build locally and import, rather than
			// running a privileged Job on a cluster they do not have (#130).
			target, err := resolveContext(contextName)
			if err != nil {
				return err
			}
			if target.Backend != "kubevirt" {
				image, _ := cmd.Flags().GetString("image")
				return buildAndImport(name, target, image, sshKey, disk, mem, cpu, filesystem, useSudo)
			}
			if !kubevirt.BootcAvailable() {
				return fmt.Errorf("this corral-bootc build lacks the bootc pipeline (build with -tags bootc)")
			}
			kubevirt.EnsureNamespace(ns)

			key := sshKey
			if key == "" {
				key = kubevirt.LoadSSHPublicKey()
			}

			// --resume picks up an interrupted build: a builder VM that
			// finished installing but never got its final VM created because
			// the CLI died mid-wait (Ctrl+C, SSH drop) — see
			// kubevirt.BootcResumeState. Skips the build step entirely.
			if resume {
				image, pvcName, ready, failed := kubevirt.BootcResumeState(name, ns)
				if failed {
					return fmt.Errorf("the previous build for %q failed — rerun without --resume to start a fresh build", name)
				}
				if !ready {
					return fmt.Errorf("no resumable build found for %q (no completed builder + disk PVC)", name)
				}
				if err := finishVM(name, ns, pvcName, image, key, mem, cpu, node); err != nil {
					return err
				}
				kubevirt.BootcCleanupBuilder(name+"-bootc-builder", ns)
				return nil
			}

			image, _ := cmd.Flags().GetString("image")
			if image == "" {
				return fmt.Errorf("--image is required — a catalog name (see `corral bootc images`) or an OCI ref like quay.io/centos-bootc/centos-bootc:stream9")
			}
			image = catalog.ResolveBootc(image)
			if key == "" {
				return fmt.Errorf("no SSH public key (--ssh-key or ~/.ssh/*.pub) — needed for bootc VM access")
			}
			size := disk
			if size == "" {
				size = "80Gi"
			}

			build, err := kubevirt.BootcBuildDisk(name, ns, image, key, size, storageClass, "", node, os.Stderr)
			if err != nil {
				return fmt.Errorf("bootc build: %w — if the builder actually finished after this failed, retry with --resume", err)
			}
			return finishVM(name, ns, build.PVCName, image, key, mem, cpu, node)
		},
	}
	create.Flags().String("image", "", "Bootc container image — catalog name or OCI ref")
	create.Flags().StringVar(&disk, "disk", "80Gi", "Disk size")
	create.Flags().StringVar(&mem, "mem", "4G", "Memory")
	create.Flags().IntVar(&cpu, "cpu", 2, "vCPUs")
	create.Flags().StringVar(&node, "node", "", "Schedule on a specific node")
	create.Flags().StringVar(&sshKey, "ssh-key", "", "SSH public key (default: ~/.ssh/*.pub)")
	create.Flags().StringVarP(&storageClass, "storage-class", "s", "", "StorageClass for the disk PVC (default: cluster preference)")
	create.Flags().BoolVar(&resume, "resume", false, "Finish a build that completed after a previous `create` was interrupted")
	create.Flags().StringVar(&filesystem, "filesystem", "", "Override the root filesystem a local build would pick from the image (xfs for ostree, btrfs for composefs)")
	create.Flags().BoolVar(&useSudo, "sudo", false, "Run the local build's podman under sudo — bootc install needs root (off-cluster backends only)")

	images := &cobra.Command{
		Use:   "images",
		Short: "List the curated bootc image catalog",
		Long: `Well-maintained bootable container bases from Fedora, CentOS, and
Universal Blue. Use a catalog name directly:

  corral bootc create myvm --image fedora-bootc

This is the bootc-only view. "corral images" (core CLI, no plugin needed)
lists this same catalog alongside the OS image catalog in one place.`,
		Example: `  corral bootc images
  corral bootc create myvm --image bluefin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("%-22s %-46s %s\n", "NAME", "IMAGE", "DESCRIPTION")
			for _, b := range catalog.BootcImages {
				fmt.Printf("%-22s %-46s %s\n", b.Name, b.Image, b.Description)
			}
			return nil
		},
	}

	// rebuild reruns the on-cluster build and re-points the VM. newImage "" =
	// keep the recorded image (an upgrade — pull the latest of the same tag).
	rebuild := func(name, newImage string) error {
		ns := namespace
		if ns == "" {
			ns = kubevirt.DefaultNamespace
		}
		if !kubevirt.BootcAvailable() {
			return fmt.Errorf("this corral-bootc build lacks the bootc pipeline (build with -tags bootc)")
		}
		image := catalog.ResolveBootc(newImage)
		if image == "" {
			image = kubevirt.BootcImageOf(name, ns)
		}
		if image == "" {
			return fmt.Errorf("no image recorded for %q — pass --image <ref>", name)
		}
		key := sshKey
		if key == "" {
			key = kubevirt.LoadSSHPublicKey()
		}
		if err := kubevirt.BootcRebuild(name, ns, image, key, disk, os.Stderr); err != nil {
			return err
		}
		if store, err := registry.NewStore(); err == nil {
			store.Set(name, types.RegistryEntry{Backend: "kubevirt", Namespace: ns})
		}
		fmt.Fprintf(os.Stderr, "VM %q rebuilt from %s and restarted\n", name, image)
		return nil
	}

	rebuildCmd := &cobra.Command{
		Use:   "rebuild <name>",
		Short: "Rebuild a bootc VM's disk from its image and restart it",
		Long: `Rebuilds the on-cluster disk from the VM's recorded bootc image (or
--image to override), swaps it in, and restarts the VM. Sizing/networking are
preserved. Use this to apply image updates.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			img, _ := cmd.Flags().GetString("image")
			return rebuild(args[0], img)
		},
	}
	rebuildCmd.Flags().String("image", "", "Override the image (default: the recorded one)")
	rebuildCmd.Flags().StringVar(&disk, "disk", "", "Override disk size (default: keep existing)")
	rebuildCmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH public key (default: ~/.ssh/*.pub)")

	upgradeCmd := &cobra.Command{
		Use:   "upgrade <name>",
		Short: "Rebuild a bootc VM from the latest of its current image (alias for rebuild)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return rebuild(args[0], "") },
	}
	upgradeCmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH public key (default: ~/.ssh/*.pub)")

	switchCmd := &cobra.Command{
		Use:   "switch <name> --image <ref>",
		Short: "Rebuild a bootc VM onto a different image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			img, _ := cmd.Flags().GetString("image")
			if img == "" {
				return fmt.Errorf("--image is required (catalog name or OCI ref)")
			}
			return rebuild(args[0], img)
		},
	}
	switchCmd.Flags().String("image", "", "New bootc image — catalog name or OCI ref")
	switchCmd.Flags().StringVar(&disk, "disk", "", "Override disk size (default: keep existing)")
	switchCmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH public key (default: ~/.ssh/*.pub)")

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "List bootc VMs and the image each was built from",
		RunE: func(cmd *cobra.Command, args []string) error {
			vms, err := kubevirt.NewClient(namespace).ListVMs()
			if err != nil {
				return err
			}
			fmt.Printf("%-24s %-12s %s\n", "VM", "NAMESPACE", "BOOTC IMAGE")
			for _, vm := range vms {
				if !vm.Bootc {
					continue
				}
				ns := vm.Namespace
				if ns == "" {
					ns = kubevirt.DefaultNamespace
				}
				fmt.Printf("%-24s %-12s %s\n", vm.Name, ns, kubevirt.BootcImageOf(vm.Name, ns))
			}
			return nil
		},
	}

	root := &cobra.Command{
		Use:   "corral-bootc",
		Short: "Corral bootc plugin — boot a container image as a VM",
	}
	root.PersistentFlags().StringVarP(&namespace, "namespace", "n", kubevirt.DefaultNamespace, "Namespace (KubeVirt only)")
	root.PersistentFlags().StringVar(&contextName, "context", "", "Corral context to build for (default: the selected context)")
	root.AddCommand(create, images, rebuildCmd, upgradeCmd, switchCmd, statusCmd)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
