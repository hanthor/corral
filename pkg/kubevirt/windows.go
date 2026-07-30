package kubevirt

import (
	"fmt"

	"github.com/tuna-os/corral/pkg/windows"
)

// Windows VM support, shared by the corral-windows plugin (CLI) and the web
// server (GUI create flow). A Windows guest needs q35 + UEFI + TPM + Hyper-V
// enlightenments, the installer ISO as a CD-ROM, and the virtio-win driver
// ISO as a second CD-ROM so Setup can load disk/network drivers.

// VirtioWinImage is KubeVirt's containerdisk build of the virtio-win driver
// ISO — digest-pinned (see #66) against supply-chain tampering on the
// mutable :latest tag.
//
// NOT pinned to the :latest tag's own digest: quay.io/kubevirt's :latest
// tag is stale, still pointing at a legacy Docker Schema v1 manifest
// (content-type application/vnd.docker.distribution.manifest.v1+prettyjws)
// that modern containerd (v2.1+) refuses to pull at all — "media type ...
// is no longer supported". Found live: a real Windows VM create got stuck
// in ImagePullBackOff on this exact image. Pinned instead to the latest
// dated build tag (quay.io publishes these daily, in modern OCI image
// index format) as of 2026-07-02 — re-verify periodically since dated
// tags are, deliberately, not moving targets.
const VirtioWinImage = "quay.io/kubevirt/virtio-container-disk:20260702_54ce361f5b@sha256:3925a851dd92aafdd0999c2133ae4aaaed67c20a7c124fc5d454bdab2fa1f13c"

// GenerateWindowsVM builds the Windows-tuned VirtualMachine manifest. Boot
// order is disk(1) → installer ISO(2): the empty disk falls through to the ISO
// on first boot, and Windows boots from disk directly once installed.
// When unattended is true, a fourth CD-ROM (the ConfigMap-backed
// autounattend.xml ISO — see autounattend.go) is attached so Windows
// Setup runs with zero interactive prompts.
func GenerateWindowsVM(name, ns, mem string, cpu int, unattended bool) map[string]any {
	disks := []map[string]any{
		{"name": "rootdisk", "bootOrder": 1, "disk": map[string]any{"bus": "virtio"}},
		{"name": "windows-iso", "bootOrder": 2, "cdrom": map[string]any{"bus": "sata"}},
		{"name": "virtio-drivers", "cdrom": map[string]any{"bus": "sata"}},
	}
	volumes := []map[string]any{
		{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": name + "-disk"}},
		{"name": "windows-iso", "persistentVolumeClaim": map[string]any{"claimName": name + "-iso"}},
		{"name": "virtio-drivers", "containerDisk": map[string]any{"image": VirtioWinImage}},
	}
	if unattended {
		disks = append(disks, map[string]any{"name": "autounattend", "cdrom": map[string]any{"bus": "sata"}})
		volumes = append(volumes, map[string]any{"name": "autounattend", "configMap": map[string]any{"name": AutounattendConfigMapName(name)}})
	}
	return map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    map[string]any{"corral.dev/os": "windows"},
		},
		"spec": map[string]any{
			"running": false,
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{"kubevirt.io/vm": name},
				},
				"spec": map[string]any{
					"domain": map[string]any{
						"machine": map[string]any{"type": "q35"},
						"firmware": map[string]any{
							"bootloader": map[string]any{
								// secureBoot needs SMM; off keeps it simple and
								// boots every Windows version.
								"efi": map[string]any{"secureBoot": false},
							},
						},
						"features": map[string]any{
							"acpi": map[string]any{},
							"apic": map[string]any{},
							"smm":  map[string]any{"enabled": false},
							// Hyper-V enlightenments: Windows runs poorly without them.
							"hyperv": map[string]any{
								"relaxed": map[string]any{},
								"vapic":   map[string]any{},
								"vpindex": map[string]any{},
								"synic":   map[string]any{},
								"synictimer": map[string]any{
									"direct": map[string]any{},
								},
								"spinlocks":   map[string]any{"spinlocks": 8191},
								"frequencies": map[string]any{},
								"ipi":         map[string]any{},
							},
						},
						"clock": map[string]any{
							"utc": map[string]any{},
							"timer": map[string]any{
								"hpet":   map[string]any{"present": false},
								"pit":    map[string]any{"tickPolicy": "delay"},
								"rtc":    map[string]any{"tickPolicy": "catchup"},
								"hyperv": map[string]any{},
							},
						},
						"cpu": map[string]any{"cores": cpu},
						"devices": map[string]any{
							"tpm":   map[string]any{},
							"disks": disks,
							"interfaces": []map[string]any{
								// e1000e works during Setup before virtio NIC
								// drivers are installed.
								{"name": "default", "masquerade": map[string]any{}, "model": "e1000e"},
							},
							"inputs": []map[string]any{
								{"type": "tablet", "bus": "usb", "name": "tablet"},
							},
						},
						"resources": map[string]any{
							"requests": map[string]any{"memory": mem},
						},
					},
					"networks": []map[string]any{
						{"name": "default", "pod": map[string]any{}},
					},
					"volumes": volumes,
				},
			},
		},
	}
}

// CreateWindowsVM imports the installer ISO, provisions the boot disk, and
// applies the Windows-tuned VM, then exposes ConsolePorts (SSH/VNC/RDP)
// through the corral tailnet proxy — same as every other creation path.
// disk/mem default to 64Gi/8Gi, cpu to 4.
//
// When unattended is true (the recommended default), also generates a
// random Administrator password meeting Windows' complexity policy,
// applies an autounattend.xml ConfigMap/CD-ROM built from it (see
// autounattend.go), and returns the password — the caller (CLI/web) is
// responsible for surfacing/saving it, the same way cloud-init VM
// passwords are handled. Returns "" when unattended is false, since
// there's then no way for corral to know what account/password ends up
// on the guest — the operator drives Setup manually.
func CreateWindowsVM(name, ns, iso, disk, mem string, cpu int, unattended bool) (password string, err error) {
	if ns == "" {
		ns = DefaultNamespace
	}
	if disk == "" {
		disk = "64Gi"
	}
	if mem == "" {
		mem = "8Gi"
	}
	if cpu == 0 {
		cpu = 4
	}
	if iso == "" {
		return "", fmt.Errorf("a Windows installer ISO URL is required")
	}
	EnsureNamespace(ns)
	sc := PreferredStorageClass()

	if err := Apply(GenerateDataVolume(name+"-iso", ns, iso, DetectISOSize(iso))); err != nil {
		return "", fmt.Errorf("creating ISO DataVolume: %w", err)
	}
	if err := Apply(GeneratePVCWithClass(name+"-disk", ns, disk, sc)); err != nil {
		return "", fmt.Errorf("creating boot disk PVC: %w", err)
	}
	if unattended {
		password = windows.RandomPassword()
		xml := windows.AutounattendXML(name, password)
		if err := Apply(GenerateAutounattendConfigMap(name, ns, xml)); err != nil {
			return "", fmt.Errorf("creating autounattend ConfigMap: %w", err)
		}
	}
	if err := Apply(GenerateWindowsVM(name, ns, mem, cpu, unattended)); err != nil {
		return "", fmt.Errorf("creating VM: %w", err)
	}
	if err := ApplyProxy(name, ns, ConsolePorts); err != nil {
		return "", fmt.Errorf("exposing console: %w", err)
	}
	return password, nil
}

func CreateWindowsVMInContext(name, ns, iso, disk, mem string, cpu int, unattended bool, context string) (password string, err error) {
	err = WithContext(context, func() error {
		var inner error
		password, inner = CreateWindowsVM(name, ns, iso, disk, mem, cpu, unattended)
		return inner
	})
	return password, err
}
