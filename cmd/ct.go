package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/kubevirt"
)

var (
	ctNamespace    string
	ctImage        string
	ctCPU          int
	ctPorts        []int
	ctMem          string
	ctDisk         string
	ctStorageClass string
	ctPrivileged   bool
	ctInit         bool
	ctDevcontainer string
	ctReadyTimeout time.Duration
)

var ctCmd = &cobra.Command{
	Use:   "ct",
	Short: "Manage Containers (CT) — Proxmox-style pet pods, not VMs",
	Long: `Containers (CT) are plain Kubernetes pods presented in the Proxmox CT shape —
console-able and long-lived like a VM, but without a hypervisor underneath.

Unprivileged (default): a PVC mounts at /data only; the rest of the
filesystem resets to the image's baked-in state on every Stop/Start.

Privileged: distrobox-on-Kubernetes — the CT seeds its volume with a full
copy of the image's own root filesystem and chroots into it on boot, so
package installs and dotfiles survive Stop/Start. Needs a real OS image
(debian/ubuntu/fedora), not alpine/busybox.`,
	Example: `  corral ct create devbox --image debian:12 --privileged
  corral ct console devbox
  corral ct stop devbox   # data volume survives
  corral ct list`,
}

var ctCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a Container (CT) — optionally from a project's devcontainer.json",
	Long: `Create a Container (CT).

--devcontainer <path> reads image, postCreateCommand, remoteUser, and
forwardPorts from a project's devcontainer.json (the path itself, or a
directory containing .devcontainer/devcontainer.json or .devcontainer.json).
This is a scoped MVP, not full devcontainer-spec/VS Code support: Features,
build.dockerfile, and postCreateCommand's parallel-named-commands object
form aren't implemented (see the tracking issue). --image/--privileged etc.
still override anything --devcontainer would otherwise set.`,
	Example: `  corral ct create tools --image fedora:40
  corral ct create devbox --image debian:12 --privileged --cpu 2 --mem 2Gi --disk 10Gi
  corral ct create web --image nginx:alpine --ports 80,8080   # published on the tailnet Service
  corral ct create myproj --devcontainer ./myproj
  corral ct create shell --image ghcr.io/tuna-os/ct-debian:latest --init   # curated image: sshd via its own init`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ns := ctNamespace
		if ns == "" {
			ns = kubevirt.DefaultNamespace
		}

		image := ctImage
		privileged := ctPrivileged
		var devcfg *ct.DevContainerConfig
		var err error
		if ctDevcontainer != "" {
			jsonPath, e := ct.FindDevContainerJSON(ctDevcontainer)
			if e != nil {
				return e
			}
			devcfg, err = ct.LoadDevContainerConfig(jsonPath)
			if err != nil {
				return err
			}
			if devcfg.Build != nil && image == "" {
				df := devcfg.Build.Dockerfile
				if df == "" {
					df = "Dockerfile"
				}
				ctx := ctDevcontainer
				if st, err := os.Stat(ctx); err == nil && st.IsDir() {
					// ctx is already the project dir
				} else {
					ctx = filepath.Dir(ctx)
				}
				return fmt.Errorf(
					"%s uses build.dockerfile — build and push the image first, then pass --image:\n"+
						"  cd %s && docker build -t ghcr.io/YOU/corral-ct:%s -f %s . && docker push ghcr.io/YOU/corral-ct:%s\n"+
						"  corral ct create %s --image ghcr.io/YOU/corral-ct:%s --devcontainer %s",
					jsonPath, ctx, name, df, name, name, name, ctDevcontainer)
			}
			if len(devcfg.Features) > 0 && string(devcfg.Features) != "null" && string(devcfg.Features) != "{}" {
				return fmt.Errorf(
					"%s has features — build with the devcontainer CLI first, then pass --image:\n"+
						"  devcontainer build --workspace-folder %s --image-name ghcr.io/YOU/corral-ct:%s\n"+
						"  docker push ghcr.io/YOU/corral-ct:%s\n"+
						"  corral ct create %s --image ghcr.io/YOU/corral-ct:%s --devcontainer %s",
					jsonPath, ctDevcontainer, name, name, name, name, ctDevcontainer)
			}
			if image == "" {
				image = devcfg.Image
			}
			if image == "" {
				return fmt.Errorf("%s has no usable image (image or build.dockerfile)", jsonPath)
			}
			// devcontainer.json's whole point is a persistent, customized
			// environment — --privileged unless the user explicitly said
			// otherwise.
			if !cmd.Flags().Changed("privileged") {
				privileged = true
			}
		}
		if image == "" {
			return fmt.Errorf("--image is required (or --devcontainer pointing at a devcontainer.json with one)")
		}

		var mounts []ct.DevContainerMount
		if devcfg != nil {
			mounts, err = devcfg.ResolveMounts()
			if err != nil {
				return err
			}
		}

		if err := ct.Create(ct.CreateOpts{
			Name: name, Namespace: ns, Image: image,
			CPU: ctCPU, Mem: ctMem, Disk: ctDisk,
			StorageClass: ctStorageClass, Privileged: privileged,
			Init: ctInit, Mounts: mounts,
		}); err != nil {
			return err
		}
		fmt.Printf("CT %q created in ns/%s\n", name, ns)

		if devcfg == nil {
			exposeCTPorts(name, ns, ctPorts)
			return nil
		}
		return applyDevContainerPostCreate(name, ns, devcfg, ctPorts)
	},
}

// applyDevContainerPostCreate runs devcontainer.json's postCreateCommand
// (if any) and exposes forwardPorts (if any) once the CT is actually ready
// — best-effort, matching how kubevirt's ApplyProxy failures are handled
// elsewhere: the CT itself is already up, so these are reported as warnings
// rather than rolling back a successful Create.
// exposeCTPorts publishes extra ports (plus the console ports) on the CT's
// tailnet Service — best-effort with a warning, same policy as forwardPorts:
// the CT itself is already up, so a Service failure shouldn't roll it back.
func exposeCTPorts(name, ns string, ports []int) {
	if len(ports) == 0 {
		return
	}
	all := append(append([]int{}, ct.ConsolePorts...), ports...)
	if err := ct.ApplyProxy(name, ns, all); err != nil {
		fmt.Fprintf(os.Stderr, "warning: exposing ports failed: %v\n", err)
	} else {
		fmt.Printf("Exposed ports: %v\n", ports)
	}
}

func applyDevContainerPostCreate(name, ns string, cfg *ct.DevContainerConfig, extraPorts []int) error {
	// Run all postCreate commands (single string/array or multiple named).
	postCmds, err := cfg.ResolvePostCreateCommands()
	if err != nil {
		return err
	}

	// Run all postStart commands (single string/array or multiple named).
	postStartCmds, err := cfg.ResolvePostStartCommands()
	if err != nil {
		return err
	}

	allCmds := append(append([]ct.NamedCommand{}, postCmds...), postStartCmds...)
	for i := range allCmds {
		allCmds[i] = *allCmds[i].WithUser(cfg.RemoteUser)
	}

	if len(allCmds) > 0 {
		fmt.Println("Waiting for the CT to be ready before running devcontainer commands…")
		if err := ct.WaitReady(name, ns, ctReadyTimeout); err != nil {
			return fmt.Errorf("devcontainer lifecycle: %w", err)
		}

		if len(allCmds) == 1 {
			fmt.Println("Running lifecycle command…")
			if err := ct.Exec(name, ns, allCmds[0].Argv, allCmds[0].Script); err != nil {
				return fmt.Errorf("lifecycle command failed: %w", err)
			}
		} else {
			fmt.Printf("Running %d lifecycle commands in parallel…\n", len(allCmds))
			if err := ct.ExecParallel(name, ns, allCmds); err != nil {
				return fmt.Errorf("lifecycle commands: %w", err)
			}
		}
	}

	exposeCTPorts(name, ns, append(cfg.Ports(), extraPorts...))
	return nil
}

var ctListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Containers",
	RunE: func(_ *cobra.Command, _ []string) error {
		cts, err := ct.ListCTs()
		if err != nil {
			return err
		}
		if len(cts) == 0 {
			fmt.Println("No Containers found.")
			return nil
		}
		fmt.Printf("%-20s %-16s %-10s %-6s %-6s %s\n", "NAME", "NAMESPACE", "PHASE", "CPU", "MEM", "PRIVILEGED")
		for _, c := range cts {
			fmt.Printf("%-20s %-16s %-10s %-6d %-6s %v\n", c.Name, c.Namespace, c.Phase, c.CPU, c.Mem, c.Privileged)
		}
		return nil
	},
}

var ctStartCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a stopped Container",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return ct.Start(args[0], ctNamespaceOrDefault())
	},
}

var ctStopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a Container (its data volume is kept)",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return ct.Stop(args[0], ctNamespaceOrDefault())
	},
}

var ctDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a Container and its data volume",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return ct.Delete(args[0], ctNamespaceOrDefault())
	},
}

var ctConsoleCmd = &cobra.Command{
	Use:   "console <name>",
	Short: "Open an interactive console on a Container",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return ct.Console(args[0], ctNamespaceOrDefault())
	},
}

func ctNamespaceOrDefault() string {
	if ctNamespace != "" {
		return ctNamespace
	}
	return kubevirt.DefaultNamespace
}

func init() {
	rootCmd.AddCommand(ctCmd)
	ctCmd.PersistentFlags().StringVarP(&ctNamespace, "namespace", "n", "", "Namespace (default: corral's default)")
	ctCmd.AddCommand(ctCreateCmd, ctListCmd, ctStartCmd, ctStopCmd, ctDeleteCmd, ctConsoleCmd)

	ctCreateCmd.Flags().StringVar(&ctImage, "image", "", "OCI image (required, unless --devcontainer's json has one)")
	ctCreateCmd.Flags().IntVar(&ctCPU, "cpu", 1, "vCPU cores")
	ctCreateCmd.Flags().StringVar(&ctMem, "mem", "512Mi", "Memory")
	ctCreateCmd.Flags().StringVar(&ctDisk, "disk", "5Gi", "Data volume size")
	ctCreateCmd.Flags().StringVarP(&ctStorageClass, "storage-class", "s", "", "StorageClass (default: cluster preference)")
	ctCreateCmd.Flags().BoolVar(&ctPrivileged, "privileged", false, "Persistent full rootfs (distrobox-style) — needs a real OS image")
	ctCreateCmd.Flags().StringVar(&ctDevcontainer, "devcontainer", "", "Path to a devcontainer.json, or a directory containing one")
	ctCreateCmd.Flags().IntSliceVar(&ctPorts, "ports", nil, "Extra ports to publish on the CT's tailnet Service, e.g. --ports 8080,3000")
	ctCreateCmd.Flags().BoolVar(&ctInit, "init", false, "Run the image's own entrypoint instead of corral's sleep — for curated CT images (ghcr.io/tuna-os/ct-debian) whose init starts sshd")
	ctCreateCmd.Flags().DurationVar(&ctReadyTimeout, "devcontainer-ready-timeout", 2*time.Minute, "How long to wait for the CT before running postCreateCommand")
}
