package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/types"
)

var (
	createKubevirt          bool
	createMem               string
	createCPU               int
	createDisk              string
	createISO               string
	createQCOW              string
	createForce             bool
	createContainerDisk     string
	createImage             string
	createImport            string
	createPVC               string
	createNamespace         string
	createNode              string
	createCloudInitPassword string
	createCloudInit         string
	createInstanceType      string
	createPreference        string
	createFile              string
	createStorageClass      string
	createBootc             string
	createProvisionScript   string
	createStart             bool
	createWaitSSH           bool
	createTimeout           int
	createSSHUser           string
	createEphemeral         bool
	createTTL               string
	createLAN               bool
	createNetworkNAD        string
	createBridgeIface       string
	createLANService        bool
)

// limaFile is the Lima YAML format — corral reads Lima files natively.
// Only the subset of fields that map to KubeVirt are parsed; the rest
// (mounts, networks, etc.) are ignored with a warning where applicable.
type limaFile struct {
	Bootc     string          `yaml:"bootc"`
	Images    []limaImage     `yaml:"images"`
	CPUs      int             `yaml:"cpus"`
	Memory    string          `yaml:"memory"`
	Disk      string          `yaml:"disk"`
	Provision []limaProvision `yaml:"provision"`
}

type limaImage struct {
	Location string `yaml:"location"`
	Arch     string `yaml:"arch"`
}

type limaProvision struct {
	Mode   string `yaml:"mode"`
	Script string `yaml:"script"`
}

func loadLimaFile(path string) (*limaFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var spec limaFile
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &spec, nil
}

// parseLimaMemory converts Lima-formatted memory strings ("16GiB", "8G", "4096")
// to the corral format ("16G", "8G", "4096M").
func parseLimaMemory(s string) string {
	s = strings.TrimSuffix(s, "iB")
	s = strings.TrimSuffix(s, "B")
	// If it's just a number, treat as MiB
	if _, err := fmt.Sscanf(s, "%d", new(int)); err == nil && !strings.ContainsAny(s, "GMK") {
		return s + "M"
	}
	return s
}

// parseLimaDisk converts Lima-formatted disk strings ("60GiB", "40G") to
// corral format ("60G", "40G").
func parseLimaDisk(s string) string {
	return parseLimaMemory(s)
}

// limaScriptToCloudInit converts a shell provisioning script to a cloud-init
// runcmd entry. The entire script runs as a single sh invocation so that
// multi-line constructs (if/fi, loops, backslash continuations) work.
func limaScriptToCloudInit(script string) string {
	script = strings.TrimSpace(script)
	// Strip the shebang line — cloud-init runs via sh -c.
	if strings.HasPrefix(script, "#!") {
		idx := strings.Index(script, "\n")
		if idx >= 0 {
			script = strings.TrimSpace(script[idx+1:])
		}
	}
	if script == "" {
		return ""
	}
	// YAML literal block scalar (|) keeps newlines intact inside cloud-init.
	return "runcmd:\n  - |\n" + indentLines(script, "    ")
}

// indentLines prepends prefix to every line of s.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// applyLimaFile applies Lima YAML fields to the corral create flags.  Returns
// the effective VM name (from positional arg, unchanged by Lima — Lima files
// don't have a name field).
func applyLimaFile(spec *limaFile) {
	if spec.Bootc != "" {
		createBootc = spec.Bootc
	}
	var rawScripts []string
	for _, p := range spec.Provision {
		if p.Script != "" {
			rawScripts = append(rawScripts, p.Script)
		}
	}
	if len(rawScripts) > 0 {
		createProvisionScript = strings.Join(rawScripts, "\n")
	}

	if spec.CPUs != 0 && createCPU == 2 {
		createCPU = spec.CPUs
	}
	if spec.Memory != "" && createMem == "4G" {
		createMem = parseLimaMemory(spec.Memory)
	}
	if spec.Disk != "" && createDisk == "" {
		createDisk = parseLimaDisk(spec.Disk)
	}

	// Resolve the first image as the boot source.
	if len(spec.Images) > 0 && createContainerDisk == "" && createImage == "" && createISO == "" && createImport == "" {
		loc := spec.Images[0].Location
		switch {
		case strings.HasSuffix(loc, ".iso") || strings.HasPrefix(loc, "https://") && strings.Contains(loc, ".iso"):
			createISO = loc
		case strings.HasSuffix(loc, ".qcow2") || strings.HasSuffix(loc, ".raw") || strings.HasSuffix(loc, ".img"):
			createImport = loc
		case strings.Contains(loc, "/") && strings.Contains(loc, ":"):
			// OCI image reference (e.g. quay.io/containerdisks/fedora:44)
			createContainerDisk = loc
		default:
			// Bare name — try catalog lookup
			createImage = loc
		}
	}

	// Convert provision scripts to cloud-init.
	if createCloudInit == "" {
		var parts []string
		for _, p := range spec.Provision {
			if p.Script == "" {
				continue
			}
			ci := limaScriptToCloudInit(p.Script)
			if ci != "" {
				parts = append(parts, ci)
			}
		}
		if len(parts) > 0 {
			createCloudInit = "#cloud-config\n" + strings.Join(parts, "\n")
		}
	}
}

var createCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a new VM",
	Long: `Create a new virtual machine.

By default, creates a local QEMU/KVM VM. Use --kubevirt for a
Kubernetes KubeVirt VM. The backend choice is persisted so
subsequent commands (start, stop, viewer...) auto-detect it.

QEMU examples:
  corral create myvm --iso https://example.com/ubuntu.iso
  corral create myvm --qcow ./template.qcow2 --disk 40G

KubeVirt examples:
  corral create myvm --kubevirt --iso https://example.com/bluefin.iso
  corral create myvm --kubevirt --container-disk quay.io/containerdisks/ubuntu:24.04

By default a KubeVirt VM only gets a NATed pod-network interface — it can
reach the internet but not LAN-only devices (a smartwatch, a NAS, a router
admin panel). Two ways to fix that (see docs/kubevirt-proxmox-setup.md
§6 for cluster-side setup):
  --lan bridges a second NIC onto a Multus NetworkAttachmentDefinition —
    a real secondary interface, needs Multus installed:
      corral create myvm --kubevirt --image fedora --lan
      corral create myvm --kubevirt --image fedora --network-nad default/lan-bridge
  --lan-service exposes via a LoadBalancer Service instead — no Multus, no
    new interface; needs a controller that fulfills LoadBalancer Services
    (Cilium's own L2 Announcement/BGP, or MetalLB):
      corral create myvm --kubevirt --image fedora --lan-service

Boot a container image as a VM? Install the bootc extension:
  corral plugin install bootc && corral bootc create myvm --image quay.io/centos-bootc/centos-bootc:stream9`,
	Example: `  corral create myvm --kubevirt --image fedora
  corral create myvm --iso ./install.iso --disk 40G
  corral create scratch --kubevirt --image bluefin --ephemeral --ttl 2h
  corral create gate --bootc ghcr.io/tuna-os/yellowfin:gnome --wait-ssh --timeout 900
  corral create builder --kubevirt --image fedora --lan   # direct LAN access via a Multus bridge NAD
  corral create builder --kubevirt --image fedora --lan-service   # LAN access via LoadBalancer Service (Cilium/MetalLB)`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// --file reads a Lima YAML file — same format limactl uses.
		if createFile != "" {
			spec, err := loadLimaFile(createFile)
			if err != nil {
				return err
			}
			applyLimaFile(spec)
			if spec.Bootc == "" {
				createKubevirt = true
			}
		}

		if existing := resolveBackend(name); existing != "" && !createForce {
			return fmt.Errorf("VM %q already exists (backend: %s). Use --force to overwrite", name, existing)
		}

		if createBootc != "" {
			// Backend auto-selection: honor an explicit --kubevirt/-k, otherwise
			// use KubeVirt when a cluster is reachable and fall back to local
			// QEMU. One command, right backend, no flag juggling.
			useKubevirt := createKubevirt
			if !cmd.Flags().Changed("kubevirt") {
				useKubevirt = kubevirt.Reachable()
			}
			if useKubevirt {
				return runKubevirtBootcCreate(name)
			}
			if err := runLocalBootcCreate(name); err != nil {
				return err
			}
			return maybeStartAndWait(name)
		}

		if createKubevirt || createImage != "" || createImport != "" {
			return runKubevirtCreate(name)
		}
		if err := runQemuCreate(name); err != nil {
			return err
		}
		return maybeStartAndWait(name)
	},
}

func init() {
	rootCmd.AddCommand(createCmd)
	createCmd.Flags().BoolVarP(&createKubevirt, "kubevirt", "k", false, "Use KubeVirt backend")
	createCmd.Flags().StringVar(&createMem, "mem", "4G", "Memory allocation")
	createCmd.Flags().IntVar(&createCPU, "cpu", 2, "CPU cores")
	createCmd.Flags().StringVar(&createDisk, "disk", "", "Disk size (default: 20G)")
	createCmd.Flags().StringVar(&createISO, "iso", "", "ISO path/URL (QEMU: local file, KubeVirt: CDI imports from URL)")
	createCmd.Flags().StringVar(&createQCOW, "qcow", "", "[qemu] QCOW2 template")
	createCmd.Flags().BoolVar(&createForce, "force", false, "Overwrite existing VM")
	createCmd.Flags().StringVar(&createContainerDisk, "container-disk", "", "[kubevirt] Container disk image")
	createCmd.Flags().StringVar(&createImage, "image", "", "[kubevirt] OS image from the catalog (see `corral images`)")
	createCmd.Flags().StringVar(&createImport, "import", "", "[kubevirt] Import a qcow2/raw disk image URL via CDI")
	createCmd.Flags().StringVar(&createPVC, "pvc", "", "[kubevirt] Existing PVC to use")
	createCmd.Flags().StringVarP(&createNamespace, "namespace", "n", kubevirt.DefaultNamespace, "[kubevirt] Namespace")
	createCmd.Flags().StringVar(&createNode, "node", "", "[kubevirt] Schedule on specific node")
	createCmd.Flags().StringVar(&createCloudInitPassword, "cloud-init-password", "", "[kubevirt] Cloud-init password")
	createCmd.Flags().StringVar(&createCloudInit, "cloud-init", "", "[kubevirt] Extra cloud-init user-data YAML")
	createCmd.Flags().StringVar(&createInstanceType, "instancetype", "", "[kubevirt] Cluster instancetype for sizing (overrides --cpu/--mem)")
	createCmd.Flags().StringVar(&createPreference, "preference", "", "[kubevirt] Cluster preference (guest device/firmware defaults)")
	createCmd.Flags().StringVarP(&createFile, "file", "f", "", "Lima YAML file (corral reads Lima format natively)")
	createCmd.Flags().StringVarP(&createStorageClass, "storage-class", "s", "", "[kubevirt] StorageClass for new disks (default: cluster preference)")
	createCmd.Flags().StringVar(&createBootc, "bootc", "", "Bootc container image to run")
	createCmd.Flags().BoolVar(&createStart, "start", false, "Start the VM after creating it")
	createCmd.Flags().BoolVar(&createWaitSSH, "wait-ssh", false, "Start the VM and block until SSH answers; nonzero exit on timeout (CI boot gate; works on QEMU and KubeVirt bootc VMs)")
	createCmd.Flags().IntVar(&createTimeout, "timeout", 600, "Seconds to wait for SSH with --wait-ssh")
	createCmd.Flags().StringVar(&createSSHUser, "ssh-user", "root", "User for the --wait-ssh probe (bootc injects the key for root)")
	createCmd.Flags().BoolVar(&createEphemeral, "ephemeral", false, "[kubevirt] Mark for `corral gc`: stopped (PVCs kept) once --ttl expires, deleted after a grace period")
	createCmd.Flags().StringVar(&createTTL, "ttl", "", "[kubevirt] Lifetime before `corral gc` stops this VM, e.g. \"4h\" (default 4h; requires --ephemeral)")
	createCmd.Flags().BoolVar(&createLAN, "lan", false, "[kubevirt] Bridge a secondary NIC onto the LAN (needs a Multus NetworkAttachmentDefinition)")
	createCmd.Flags().StringVar(&createNetworkNAD, "network-nad", "", "[kubevirt] NetworkAttachmentDefinition to bridge onto (\"ns/name\"); implies --lan")
	createCmd.Flags().StringVar(&createBridgeIface, "bridge-iface", "", "[kubevirt] Guest interface name for the LAN bridge (default: net1)")
	createCmd.Flags().BoolVar(&createLANService, "lan-service", false, "[kubevirt] Expose via a LoadBalancer Service instead of a secondary NIC (no Multus needed — works with Cilium L2/BGP or MetalLB)")
}

func runKubevirtCreate(name string) error {
	ns := createNamespace
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}

	containerDisk := createContainerDisk
	importURL := createImport
	iso := createISO
	if createImage != "" {
		img := catalog.Find(createImage)
		if img == nil {
			return fmt.Errorf("unknown image %q — see `corral images`", createImage)
		}
		// Catalog entries boot three ways: containerdisks directly, official
		// cloud images via CDI import, installer ISOs via the ISO path.
		switch img.Kind() {
		case "containerDisk":
			containerDisk = img.ContainerDisk
		case "import":
			importURL = img.URL
		case "iso":
			iso = img.ISO
		}
	}

	opts := types.CreateOpts{
		Name:              name,
		Namespace:         ns,
		Mem:               createMem,
		CPU:               createCPU,
		Disk:              createDisk,
		ISO:               iso,
		ContainerDisk:     containerDisk,
		ImportURL:         importURL,
		PVC:               createPVC,
		Node:              createNode,
		CloudInitPassword: createCloudInitPassword,
		CloudInitExtra:    createCloudInit,
		InstanceType:      createInstanceType,
		Preference:        createPreference,
		SSHPublicKey:      kubevirt.LoadSSHPublicKey(),
		StorageClass:      createStorageClass,
		Ephemeral:         createEphemeral,
		TTL:               createTTL,
	}
	if err := kubevirt.CreateVM(opts); err != nil {
		return err
	}
	// Expose SSH/VNC on the tailnet via the Tailscale operator proxy (no
	// in-guest tailscale needed). Best-effort — the VM is already created.
	if err := kubevirt.ApplyProxy(name, ns, []int{22, 5900}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: tailnet expose failed: %v\n", err)
	}

	if createLAN || createNetworkNAD != "" {
		if err := attachLANBridge(ns, name); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
	}
	if createLANService {
		if err := kubevirt.ApplyLANService(name, ns, []int{22}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: LAN service not created: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "  LAN:    corral lanservice %s   # check the assigned external IP\n", name)
		}
	}

	if registryStore != nil {
		if err := registryStore.Set(name, types.RegistryEntry{
			Backend:   "kubevirt",
			Namespace: ns,
			Password:  kubevirt.LastPassword,
		}); err != nil {
			return fmt.Errorf("saving registry: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "VM %q created in ns/%s\n", name, ns)
	fmt.Fprintf(os.Stderr, "  Start:  corral start %s\n", name)
	fmt.Fprintf(os.Stderr, "  SSH:    corral ssh %s\n", name)
	return nil
}

// attachLANBridge resolves which Multus NAD to use (--network-nad, or the
// cluster's only one) and hotplugs a bridge-bound secondary NIC onto the
// just-created VM — see kubevirt.ResolveNAD/AddNIC and issue #82.
func attachLANBridge(ns, name string) error {
	nad, err := kubevirt.ResolveNAD(createNetworkNAD, kubevirt.ListNADs())
	if err != nil {
		return fmt.Errorf("LAN bridge not attached: %w", err)
	}
	if err := kubevirt.NewClient(ns).AddNIC(name, nad, createBridgeIface); err != nil {
		return fmt.Errorf("LAN bridge not attached: %w", err)
	}
	iface := createBridgeIface
	if iface == "" {
		iface = "net1"
	}
	fmt.Fprintf(os.Stderr, "  LAN:    bridged via %s (guest iface %s)\n", nad, iface)
	return nil
}

func runQemuCreate(name string) error {
	if err := qemu.Create(types.CreateOpts{
		Name:  name,
		Mem:   createMem,
		CPU:   createCPU,
		Disk:  createDisk,
		ISO:   createISO,
		QCOW:  createQCOW,
		Force: createForce,
	}); err != nil {
		return err
	}
	if registryStore != nil {
		registryStore.Set(name, types.RegistryEntry{Backend: "qemu"})
	}
	return nil
}

func runKubevirtBootcCreate(name string) error {
	ns := createNamespace
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}
	sshKey := kubevirt.LoadSSHPublicKey()
	if sshKey == "" {
		return fmt.Errorf("no SSH public key found (needed for bootc VM)")
	}
	size := createDisk
	if size == "" {
		size = "80Gi"
	}

	// The bootc build pipeline is an optional plugin (kept out of the lean core
	// binary — see pkg/kubevirt/bootc_core.go). When it isn't compiled in,
	// delegate the build+VM creation to the `corral-bootc` plugin so
	// `corral create --bootc ... --wait-ssh` still works as one command. The
	// plugin creates the VM stopped; we then handle --start/--wait-ssh below.
	if !kubevirt.BootcAvailable() {
		if !plugin.IsInstalled("bootc") {
			return fmt.Errorf("bootc build needs the bootc plugin: corral plugin install bootc")
		}
		args := []string{"create", name, "--image", createBootc, "-n", ns}
		if size != "" {
			args = append(args, "--disk", size)
		}
		if createStorageClass != "" {
			args = append(args, "--storage-class", createStorageClass)
		}
		if createNode != "" {
			args = append(args, "--node", createNode)
		}
		if err := plugin.Dispatch("bootc", args); err != nil {
			return err
		}
		if registryStore != nil {
			registryStore.Set(name, types.RegistryEntry{Backend: "kubevirt", Namespace: ns})
		}
		if createWaitSSH {
			fmt.Fprintf(os.Stderr, "  Waiting for SSH (timeout %ds)...\n", createTimeout)
			return kubevirt.NewClient(ns).WaitSSH(name, createSSHUser, "", time.Duration(createTimeout)*time.Second)
		}
		if createStart {
			return kubevirt.NewClient(ns).StartVM(name)
		}
		return nil
	}

	build, err := kubevirt.BootcBuildDisk(name, ns, createBootc, sshKey, size, createStorageClass, createProvisionScript, createNode, os.Stderr)
	if err != nil {
		return err
	}
	vm := kubevirt.GenerateBootcVM(name, ns, build.PVCName, createBootc, sshKey, createMem, createCPU, createNode)
	if err := kubevirt.Apply(vm); err != nil {
		return err
	}
	if err := kubevirt.ApplyProxy(name, ns, []int{22, 5900}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: tailnet expose failed: %v\n", err)
	}
	if registryStore != nil {
		registryStore.Set(name, types.RegistryEntry{Backend: "kubevirt", Namespace: ns})
	}
	fmt.Fprintf(os.Stderr, "VM %q created in ns/%s\n", name, ns)
	fmt.Fprintf(os.Stderr, "  Start:  corral start %s\n", name)
	fmt.Fprintf(os.Stderr, "  SSH:    corral ssh %s\n", name)

	// --wait-ssh turns create into a boot gate on KubeVirt too: start the VM
	// and block until root SSH answers, so the same command works on both
	// backends. --start (without --wait-ssh) just brings it up.
	if createWaitSSH {
		fmt.Fprintf(os.Stderr, "  Waiting for SSH (timeout %ds)...\n", createTimeout)
		return kubevirt.NewClient(ns).WaitSSH(name, createSSHUser, "", time.Duration(createTimeout)*time.Second)
	}
	if createStart {
		return kubevirt.NewClient(ns).StartVM(name)
	}
	return nil
}

func runLocalBootcCreate(name string) error {
	vmDir := filepath.Join(qemu.VMHome(), name)
	if qemu.Exists(name) && !createForce {
		return fmt.Errorf("VM %q already exists. Use --force to overwrite", name)
	}
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return fmt.Errorf("creating VM directory: %w", err)
	}

	diskSize := createDisk
	if diskSize == "" {
		diskSize = "20G"
	}

	diskPath := filepath.Join(vmDir, "disk.raw")
	out, err := exec.Command("truncate", "-s", diskSize, diskPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating raw disk: %s: %w", string(out), err)
	}

	out, err = exec.Command("sudo", "losetup", "-fP", diskPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("losetup failed: %s: %w", string(out), err)
	}

	out, err = exec.Command("sudo", "losetup", "-a").CombinedOutput()
	if err != nil {
		return fmt.Errorf("listing loop devices: %s: %w", string(out), err)
	}
	var loopDev string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, filepath.Base(diskPath)) {
			loopDev = strings.Split(line, ":")[0]
			break
		}
	}
	if loopDev == "" {
		return fmt.Errorf("could not find loop device for %s", diskPath)
	}
	defer exec.Command("sudo", "losetup", "-d", loopDev).Run()

	var provisionArg string
	if createProvisionScript != "" {
		provFile := filepath.Join(vmDir, "provision.sh")
		if err := os.WriteFile(provFile, []byte(createProvisionScript), 0755); err != nil {
			return err
		}
		defer os.Remove(provFile)
		provisionArg = "&& cat /output/provision.sh | chroot /mnt /bin/bash"
	}

	sshKey := kubevirt.LoadSSHPublicKey()
	if sshKey == "" {
		return fmt.Errorf("no SSH public key found")
	}
	keyFile := filepath.Join(vmDir, "id_rsa.pub")
	if err := os.WriteFile(keyFile, []byte(sshKey), 0644); err != nil {
		return err
	}
	defer os.Remove(keyFile)

	fmt.Printf("Building bootc image locally onto %s...\n", loopDev)
	// Backend-aware install (mirrors pkg/kubevirt/bootc.go's builder). ostree/
	// server images ship bootupctl and install to xfs with --generic-image (so
	// the disk boots the removable EFI path under plain QEMU). composefs images
	// (Universal Blue / TunaOS desktops: no bootupctl, systemd-boot on the ESP)
	// REQUIRE btrfs — their initramfs is btrfs-only, so installing them the
	// ostree/xfs way drops to dracut emergency mode at initrd-switch-root
	// (tuna-os sailfin desktop Gate). They also need the real kernel/initrd
	// re-extracted over bootc's EROFS-zeroed ESP copies, and the SSH key written
	// into the deployment (--root-ssh-authorized-keys is a no-op on composefs).
	cmd := exec.Command("sudo", "podman", "run", "--privileged", "--pid=host", "--security-opt", "label=disable",
		"-v", "/dev:/dev", "-v", vmDir+":/output:Z",
		createBootc, "sh", "-c",
		fmt.Sprintf(`set -e
DISK=%s
if command -v bootupctl >/dev/null 2>&1; then
  COMPOSEFS=0; FS=xfs; BACKEND=--generic-image
elif [ -f /usr/lib/systemd/boot/efi/systemd-bootx64.efi ] || [ -f /usr/lib/systemd/boot/efi/systemd-bootaa64.efi ]; then
  COMPOSEFS=1; FS=btrfs; BACKEND=--composefs-backend
else
  COMPOSEFS=0; FS=xfs; BACKEND=--generic-image
fi
echo "corral: install backend composefs=$COMPOSEFS fs=$FS"
bootc install to-disk $BACKEND --filesystem "$FS" --wipe --root-ssh-authorized-keys /output/id_rsa.pub "$DISK"
udevadm settle
if [ "$COMPOSEFS" = 1 ]; then
  mkdir -p /mnt/esp
  for P in 1 2 3; do
    if mount "${DISK}p${P}" /mnt/esp 2>/dev/null; then
      if [ -d /mnt/esp/EFI ]; then break; fi
      umount /mnt/esp
    fi
  done
  KVER=$(ls /usr/lib/modules | head -1)
  D=$(ls -d /mnt/esp/EFI/Linux/bootc_composefs-* 2>/dev/null | head -1)
  if [ -n "$D" ]; then
    cp -f "/usr/lib/modules/$KVER/vmlinuz" "$D/vmlinuz"
    cp -f "/usr/lib/modules/$KVER/initramfs.img" "$D/initrd"
  fi
  sync; umount /mnt/esp 2>/dev/null || true
  mkdir -p /mnt/root; mount "${DISK}p3" /mnt/root
  SSHDIR=/mnt/root/state/os/default/var/roothome/.ssh
  mkdir -p "$SSHDIR"; chmod 700 "$SSHDIR"
  cp /output/id_rsa.pub "$SSHDIR/authorized_keys"; chmod 600 "$SSHDIR/authorized_keys"; chown -R 0:0 "$SSHDIR"
  sync; umount /mnt/root
fi
udevadm settle
mkdir -p /mnt && mount "${DISK}p3" /mnt %s && umount /mnt`, loopDev, provisionArg))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("local bootc build failed: %w", err)
	}

	qcowPath := filepath.Join(vmDir, "disk.qcow2")
	out, err = exec.Command("qemu-img", "convert", "-O", "qcow2", diskPath, qcowPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("converting to qcow2: %s: %w", string(out), err)
	}
	os.Remove(diskPath)

	// ExistingDisk: the qcow2 we just built IS the boot disk — without it
	// qemu.Create would recreate disk.qcow2 empty and the VM would boot
	// into nothing.
	if err := qemu.Create(types.CreateOpts{
		Name:         name,
		Mem:          createMem,
		CPU:          createCPU,
		Disk:         createDisk,
		Force:        true,
		ExistingDisk: true,
	}); err != nil {
		return err
	}
	if registryStore != nil {
		registryStore.Set(name, types.RegistryEntry{Backend: "qemu"})
	}
	return nil
}

// maybeStartAndWait handles --start / --wait-ssh for QEMU-backed VMs.
// --wait-ssh implies --start and turns creation into a CI gate: the process
// exits nonzero unless the guest answers SSH within --timeout seconds.
func maybeStartAndWait(name string) error {
	if !createStart && !createWaitSSH {
		return nil
	}
	if err := qemu.Start(name); err != nil {
		return err
	}
	if !createWaitSSH {
		return nil
	}
	return qemu.WaitSSH(name, createSSHUser, time.Duration(createTimeout)*time.Second)
}
