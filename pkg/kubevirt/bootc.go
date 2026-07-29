//go:build bootc

// Package-local bootc plugin. Compiled only with `-tags bootc`; registers its
// implementations into the always-present seam in bootc_core.go. See that file
// for why bootc is opt-in.
package kubevirt

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/shell"
)

func init() {
	bootcBuildFunc = bootcBuildDisk
	bootcVMFunc = generateBootcVM
	bootcRebuildFunc = bootcRebuild
	bootcResumeFunc = bootcResumeState
	bootcCleanupFunc = cleanupBuilder
}

// The bootc disk is built inside a short-lived **builder VM**, not a pod. The
// builder runs `bootc install to-disk` against a block-mode PVC attached as a
// raw device, so the VM's full kernel does all the filesystem work. This is the
// only way to install images whose root fs or initramfs the *node* kernel can't
// handle — e.g. Universal Blue desktop images (bluefin/dakota) need btrfs +
// composefs, which the Talos node kernel lacks. The finished disk is
// self-bootable (GPT + ESP + bootloader), so the final VM boots it via UEFI
// firmware — no kernelBoot, no kernel/initrd/cmdline capture.

// bootcBuildTimeout bounds how long we wait for the builder VM to finish. The
// in-VM image pull + `bootc install to-disk` (ostree checkout of a multi-GB
// desktop image) can run well over half an hour on slow links. Override with
// CORRAL_BOOTC_BUILD_TIMEOUT (minutes).
func bootcBuildTimeout() time.Duration {
	if v := os.Getenv("CORRAL_BOOTC_BUILD_TIMEOUT"); v != "" {
		if m, err := strconv.Atoi(v); err == nil && m > 0 {
			return time.Duration(m) * time.Minute
		}
	}
	return 45 * time.Minute
}

// bootcRebuild rebuilds name's bootc disk from imageURI and restarts it. Used by
// `corral bootc rebuild/switch/upgrade` and the web Upgrade button. Steps: stop
// the VM (frees the RWO block disk) → delete the old disk PVC → build a fresh one
// from the image → start. The final VM references the disk PVC by name and boots
// it via UEFI, so no manifest patch is needed — the rebuilt disk just takes the
// same name. Sizing/networks/node on the VM are preserved.
func bootcRebuild(name, namespace, imageURI, sshPublicKey, diskSize string, progress io.Writer) error {
	if progress == nil {
		progress = os.Stderr
	}
	c := NewClient(namespace)
	pvc := name + "-bootc-disk"

	// `--wipe` destroys the disk (incl. /var where the composefs SSH key lives),
	// so rebuild must re-bake the key. Callers without one (the web server pod has
	// no ~/.ssh) fall back to the key recorded on the VM at create time.
	if strings.TrimSpace(sshPublicKey) == "" {
		if out, err := runPkg("kubectl", "get", "vm", name, "-n", namespace,
			"-o", `jsonpath={.metadata.annotations.corral\.bootc/ssh-key}`); err == nil {
			sshPublicKey = strings.TrimSpace(string(out))
		}
	}
	if strings.TrimSpace(sshPublicKey) == "" {
		return fmt.Errorf("no SSH key for rebuild of %q (none provided and none recorded on the VM) — pass --ssh-key, or the rebuilt VM would be unreachable", name)
	}

	// Reuse the existing disk size unless the caller overrides it.
	if diskSize == "" {
		if out, err := runPkg("kubectl", "get", "pvc", pvc, "-n", namespace,
			"-o", "jsonpath={.spec.resources.requests.storage}"); err == nil {
			diskSize = strings.TrimSpace(string(out))
		}
	}

	fmt.Fprintf(progress, "Rebuilding %s from %s\n", name, imageURI)
	fmt.Fprintf(progress, "  Stopping VM to free the disk...\n")
	c.StopVM(name) // ignore: may already be stopped
	c.waitStopped(name)

	fmt.Fprintf(progress, "  Removing the old disk (%s)...\n", pvc)
	runPkg("kubectl", "delete", "pvc", pvc, "-n", namespace, "--ignore-not-found")
	// Wait for the PVC to actually disappear so the rebuild's fresh PVC isn't a
	// no-op against a terminating one.
	for i := 0; i < 60; i++ {
		if _, err := runPkg("kubectl", "get", "pvc", pvc, "-n", namespace); err != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Rebuild keeps whatever storage class the existing PVC used — "" tells
	// bootcBuildDisk to fall back to PreferredStorageClass(), same as before
	// storageClass existed as an explicit override.
	if _, err := bootcBuildDisk(name, namespace, imageURI, sshPublicKey, diskSize, "", "", "", progress); err != nil {
		return fmt.Errorf("rebuild: %w", err)
	}

	// Keep the recorded image current (switch points the VM at a new image).
	c.patchVMMerge(name, fmt.Sprintf(`{"metadata":{"annotations":{"corral.bootc/image":%q}}}`, imageURI))

	fmt.Fprintf(progress, "  Starting %s on the new disk...\n", name)
	return c.StartVM(name)
}

// bootcBuildDisk orchestrates the on-cluster bootc disk build: it creates a
// block-mode target PVC, runs a short-lived builder VM that installs imageURI
// onto it via `bootc install to-disk`, waits for that VM to finish, then deletes
// the builder (keeping the disk). Progress/builder logs go to progress.
// storageClass "" falls back to PreferredStorageClass().
func bootcBuildDisk(name, namespace, imageURI, sshPublicKey, diskSize, storageClass, provisionScript, node string, progress io.Writer) (*BootcBuildResult, error) {
	if progress == nil {
		progress = os.Stderr
	}
	if diskSize == "" {
		diskSize = "80Gi"
	}
	if storageClass == "" {
		storageClass = PreferredStorageClass()
	}

	pvcName := name + "-bootc-disk"
	builderName := name + "-bootc-builder"

	fmt.Fprintf(progress, "Building bootc disk from %s\n", imageURI)
	fmt.Fprintf(progress, "  Disk:   pvc/%s (%s, block mode)\n", pvcName, diskSize)

	// Step 1: block-mode target PVC the builder VM installs onto as a raw device.
	pvc := generateBlockPVC(pvcName, namespace, diskSize, storageClass)
	data, err := json.Marshal(pvc)
	if err != nil {
		return nil, fmt.Errorf("marshaling PVC: %w", err)
	}
	if err := applyManifest(string(data)); err != nil {
		return nil, fmt.Errorf("creating disk PVC: %w", err)
	}

	// Step 2: the builder VM. Clean up any stale one first so cloud-init re-runs.
	cleanupBuilder(builderName, namespace)
	c := NewClient(namespace)
	c.waitStopped(builderName)

	// The cloud-init script is larger than KubeVirt's 2 KB inline userData limit,
	// so deliver it via a Secret the VM references with cloudInitNoCloud.secretRef.
	secret := generateBuilderSecret(builderName+"-cloudinit", namespace, imageURI, sshPublicKey, provisionScript)
	sdata, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("marshaling builder secret: %w", err)
	}
	if err := applyManifest(string(sdata)); err != nil {
		return nil, fmt.Errorf("creating builder secret: %w", err)
	}

	builder := generateBuilderVM(builderName, namespace, pvcName, builderName+"-cloudinit", imageURI, node)
	bdata, err := json.Marshal(builder)
	if err != nil {
		return nil, fmt.Errorf("marshaling builder VM: %w", err)
	}
	if err := applyManifest(string(bdata)); err != nil {
		return nil, fmt.Errorf("creating builder VM: %w", err)
	}
	if err := c.StartVM(builderName); err != nil {
		shell.Command("kubectl", "delete", "vm", builderName, "-n", namespace, "--ignore-not-found").Run()
		return nil, fmt.Errorf("starting builder VM: %w", err)
	}

	// Step 3: wait for the builder to install the disk (streaming its serial log).
	// If this process gets interrupted here (Ctrl+C, SSH drop), the builder VM
	// keeps running/finishes independently — see bootcResumeState, which lets a
	// later `corral bootc create --resume` pick up from here instead of Step 4.
	fmt.Fprintf(progress, "  Builder VM started — installing (this can take 20-40 min)...\n")
	if err := waitForBuilderVM(builderName, namespace, progress); err != nil {
		shell.Command("kubectl", "delete", "vm", builderName, "-n", namespace, "--ignore-not-found").Run()
		return nil, fmt.Errorf("bootc build failed: %w", err)
	}

	// Step 4: delete the builder VM + its cloud-init secret (keeps the disk PVC).
	cleanupBuilder(builderName, namespace)

	fmt.Fprintf(progress, "  Disk ready: pvc/%s\n", pvcName)
	return &BootcBuildResult{PVCName: pvcName}, nil
}

// cleanupBuilder deletes the builder VM and its cloud-init secret, leaving
// the disk PVC (which the final VM boots from) untouched.
func cleanupBuilder(builderName, namespace string) {
	shell.Command("kubectl", "delete", "vm", builderName, "-n", namespace, "--ignore-not-found").Run()
	shell.Command("kubectl", "delete", "secret", builderName+"-cloudinit", "-n", namespace, "--ignore-not-found").Run()
}

// bootcResumeState checks whether name has a completed-but-unfinished bootc
// build: a builder VM that reports "Succeeded" (the guest shut itself down
// after `bootc install to-disk`, the only path that reaches CORRAL_BUILD_OK)
// and a disk PVC that exists — the state left behind when the CLI that
// started the build is interrupted (Ctrl+C, SSH drop) before it can run
// Step 4/create the final VM. imageURI is read back from the annotation
// generateBuilderVM records. ready=false means there's nothing to resume;
// failed=true means a builder exists but didn't succeed (don't resume, the
// caller should report the failure and let the user retry a fresh build).
func bootcResumeState(name, namespace string) (imageURI, pvcName string, ready, failed bool) {
	builderName := name + "-bootc-builder"
	pvcName = name + "-bootc-disk"

	out, err := shell.Command("kubectl", "get", "vm", builderName, "-n", namespace,
		"-o", `jsonpath={.metadata.annotations.corral\.bootc/image}`).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", pvcName, false, false // no builder (or no annotation) — nothing to resume
	}
	imageURI = strings.TrimSpace(string(out))

	phase := builderVMIPhase(builderName, namespace)
	if phase != "Succeeded" {
		return imageURI, pvcName, false, phase == "Failed"
	}

	if err := shell.Command("kubectl", "get", "pvc", pvcName, "-n", namespace).Run(); err != nil {
		return imageURI, pvcName, false, false // builder succeeded but the disk is gone — can't resume
	}
	return imageURI, pvcName, true, false
}

// generateBlockPVC returns a block-mode PVC manifest. The builder VM attaches it
// as a raw device so `bootc install to-disk` can partition and format it.
func generateBlockPVC(name, namespace, size, storageClass string) map[string]any {
	pvc := GeneratePVC(name, namespace, size)
	spec := pvc["spec"].(map[string]any)
	// Filesystem, not Block: KubeVirt file-backed disks work on every
	// provisioner (local-path can't do Block volumes at all), and the guest
	// still sees a plain virtio block device either way.
	spec["volumeMode"] = "Filesystem"
	if storageClass != "" {
		spec["storageClassName"] = storageClass
	}
	return pvc
}

// builderImage is the containerdisk the builder VM boots. It needs a full kernel
// (btrfs/loop), podman, and cloud-init — a stock Fedora cloud image fits.
func builderImage() string {
	if v := strings.TrimSpace(os.Getenv("CORRAL_BOOTC_BUILDER_IMAGE")); v != "" {
		return v
	}
	return "quay.io/containerdisks/fedora:42"
}

// generateBuilderVM builds the short-lived VM that installs imageURI onto the
// block PVC. cloud-init runs `bootc install to-disk` and the post-install fixups,
// then powers off; bootcBuildDisk watches the serial console for the result.
//
// The cloud-init script auto-detects the image's backend:
//   - composefs (Universal Blue desktop: no bootupd) → `--composefs-backend
//     --filesystem btrfs`, then re-extract the real kernel/initrd over bootc's
//     EROFS-zero-filled copies on the ESP, and inject the root SSH key manually
//     (--root-ssh-authorized-keys is a no-op on composefs).
//   - ostree (server bootc: ships bootupd) → plain `bootc install to-disk` (xfs).
//
// Both paths add `console=ttyS0 systemd.wants=sshd.service` to the BLS entries —
// desktop images ship sshd disabled, and corral reaches VMs over SSH.
// builderScript returns the cloud-init the builder VM runs, with the image and
// SSH key substituted in.
func builderScript(imageURI, sshPublicKey, provisionScript string) string {
	return strings.NewReplacer(
		"__IMAGE__", imageURI,
		"__SSHKEY__", strings.TrimSpace(sshPublicKey),
		"__MIRRORCONF__", builderMirrorConf(),
		"__PROVISIONB64__", base64.StdEncoding.EncodeToString([]byte(provisionScript)),
	).Replace(builderCloudInit)
}

// registryCache identifies the in-cluster pull-through cache Service.
const (
	registryCacheService   = "registry-cache"
	registryCacheNamespace = "corral"
	registryCachePort      = "5000"
)

// bootcRegistryMirror is the pull-through cache host:port for the builder's
// podman. It is DEFAULT-ON when the cache is deployed: if the registry-cache
// Service exists, builds route ghcr.io through it automatically (deploy
// registry-cache.yaml to turn it on). When the cache is absent, this returns ""
// and builds pull directly exactly as before — so there's no regression for
// clusters without it. CORRAL_REGISTRY_MIRROR overrides the host; set it to
// "off"/"none" to force-disable even when the cache is present.
func bootcRegistryMirror() string {
	switch v := strings.TrimSpace(os.Getenv("CORRAL_REGISTRY_MIRROR")); strings.ToLower(v) {
	case "":
		return detectRegistryCache() // default: use the cache when it's deployed
	case "off", "none", "false", "0":
		return ""
	default:
		return v
	}
}

// detectRegistryCache returns the cache Service's cluster DNS host:port when the
// Service exists, else "". Probed per build (builds are infrequent).
func detectRegistryCache() string {
	out, err := runPkg("kubectl", "get", "svc", registryCacheService,
		"-n", registryCacheNamespace, "-o", "jsonpath={.metadata.name}")
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return ""
	}
	return registryCacheService + "." + registryCacheNamespace + ".svc.cluster.local:" + registryCachePort
}

// builderMirrorConf renders the registries.conf.d body that points the builder's
// podman at the cache for ghcr.io (where the heavy desktop bootc images live),
// indented to sit under `content: |`. Empty (a harmless empty drop-in) when no
// mirror is configured. See deploy/registry-cache.yaml.
func builderMirrorConf() string {
	host := bootcRegistryMirror()
	if host == "" {
		return ""
	}
	lines := []string{
		`[[registry]]`,
		`prefix = "ghcr.io"`,
		`location = "ghcr.io"`,
		`[[registry.mirror]]`,
		fmt.Sprintf(`location = %q`, host),
		`insecure = true`,
	}
	return strings.Join(lines, "\n      ") // 6-space indent under content: |
}

// generateBuilderSecret holds the cloud-init userData (too large for KubeVirt's
// 2 KB inline limit) for the builder VM to reference via cloudInitNoCloud.secretRef.
func generateBuilderSecret(name, namespace, imageURI, sshPublicKey, provisionScript string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    map[string]any{"corral-bootc-builder": name},
		},
		"stringData": map[string]any{"userdata": builderScript(imageURI, sshPublicKey, provisionScript)},
	}
}

// generateBuilderVM builds the short-lived builder VM manifest. imageURI is
// recorded as an annotation — not used by the builder itself (it's baked
// into the cloud-init secret instead) but read back by bootcResumeState if
// the CLI that started the build gets interrupted before it can create the
// final VM: the annotation is what lets a later `--resume` recover it.
func generateBuilderVM(name, namespace, pvcName, secretName, imageURI, node string) map[string]any {
	disk := func(n string, extra map[string]any) map[string]any {
		d := map[string]any{"name": n, "disk": map[string]any{"bus": "virtio"}}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}

	vmSpec := map[string]any{
		"runStrategy": "Manual",
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"kubevirt.io/vm": name}},
			"spec": map[string]any{
				"domain": map[string]any{
					"cpu":    cpuSpec(2),
					"memory": memSpec(4096),
					"devices": map[string]any{
						"logSerialConsole": true,
						"disks": []map[string]any{
							disk("rootdisk", nil),
							disk("scratch", map[string]any{"serial": "scratch"}),
							disk("target", map[string]any{"serial": "target"}),
							disk("cloudinit", nil),
						},
						"interfaces": []map[string]any{
							{"name": "default", "masquerade": map[string]any{}},
						},
					},
				},
				"networks": []map[string]any{{"name": "default", "pod": map[string]any{}}},
				"volumes": []map[string]any{
					{"name": "rootdisk", "containerDisk": map[string]any{"image": builderImage()}},
					{"name": "scratch", "emptyDisk": map[string]any{"capacity": "40Gi"}},
					{"name": "target", "persistentVolumeClaim": map[string]any{"claimName": pvcName}},
					{"name": "cloudinit", "cloudInitNoCloud": map[string]any{
						"secretRef": map[string]any{"name": secretName},
					}},
				},
			},
		},
	}

	// Pin the builder to a node when asked. local-path disks are node-bound
	// (WaitForFirstConsumer), so pinning the builder guarantees the final VM —
	// which mounts that same PVC — can schedule on the same node.
	if node != "" {
		vmSpec["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"] =
			map[string]any{"kubernetes.io/hostname": node}
	}

	return map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   namespace,
			"labels":      map[string]any{"corral-bootc-builder": name},
			"annotations": map[string]any{"corral.bootc/image": imageURI},
		},
		"spec": vmSpec,
	}
}

// builderCloudInit is the #cloud-config the builder VM runs. __IMAGE__ and
// __SSHKEY__ are substituted by generateBuilderVM. Markers CORRAL_BUILD_OK /
// CORRAL_BUILD_FAIL on the serial console signal the result to waitForBuilderVM.
const builderCloudInit = `#cloud-config
write_files:
  - path: /etc/containers/registries.conf.d/corral-mirror.conf
    content: |
      __MIRRORCONF__
  - path: /root/buildkey.pub
    content: "__SSHKEY__"
  - path: /root/provision.b64
    content: "__PROVISIONB64__"
  - path: /root/build.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      exec > >(tee /dev/ttyS0) 2>&1
      set -x
      echo CORRAL_BUILD_START
      mkfs.xfs -f /dev/disk/by-id/virtio-scratch
      mount /dev/disk/by-id/virtio-scratch /var/lib/containers
      # Blob downloads stage in $TMPDIR (default /var/tmp) before landing in
      # storage — on the ~4G containerdisk rootfs that ENOSPCs on desktop
      # images. Stage on the scratch disk instead.
      mkdir -p /var/lib/containers/tmp
      export TMPDIR=/var/lib/containers/tmp
      # Disable zstd:chunked partial pulls: they need multi-range HTTP GETs,
      # GHCR's blob CDN answers those with 501 Unsupported client range, and
      # this podman errors out instead of falling back to a full pull.
      printf '[storage]\ndriver = "overlay"\nrunroot = "/run/containers/storage"\ngraphroot = "/var/lib/containers/storage"\n\n[storage.options.pull_options]\nenable_partial_images = "false"\n' > /etc/containers/storage.conf
      IMG=__IMAGE__
      podman pull "$IMG" || { echo CORRAL_BUILD_FAIL pull; echo CORRAL_BUILD_FAIL pull > /dev/ttyS0 2>/dev/null; sync; sleep 2; poweroff; exit 1; }
      # Read the image to pick the bootc storage backend (per the bootc docs the
      # composefs-rs backend requires systemd-boot and NO bootupd; traditional
      # ostree images ship bootupd):
      #   - bootupd present                       -> ostree backend  (xfs)
      #   - else systemd-boot present, no bootupd -> composefs backend (btrfs)
      #   - neither                               -> ostree backend  (xfs)
      # Probe by inspecting the image FILESYSTEM with podman cp (no execution):
      # UB desktop images ship Rust uutils, where /usr/bin/test is a symlink to a
      # multicall binary that podman --entrypoint can't dispatch (argv[0] breaks),
      # so any "run a binary in the image" probe misdetects them. The same CTR is
      # reused for the composefs kernel/initrd extraction below. The backend also
      # drives the root fs: composefs images ship a btrfs-only initramfs, ostree xfs.
      CTR=$(podman create "$IMG")
      imghas() { podman cp "$CTR:$1" - >/dev/null 2>&1; }
      if imghas /usr/sbin/bootupctl || imghas /usr/bin/bootupctl; then
        BACKEND_KIND=ostree
      elif imghas /usr/lib/systemd/boot/efi/systemd-bootx64.efi \
        || imghas /usr/lib/systemd/boot/efi/systemd-bootaa64.efi; then
        BACKEND_KIND=composefs
      else
        BACKEND_KIND=ostree
      fi
      echo "CORRAL_BACKEND=$BACKEND_KIND"
      if [ "$BACKEND_KIND" = composefs ]; then
        # composefs backend installs systemd-boot to the ESP removable path itself.
        COMPOSEFS=1; FS=btrfs; BACKEND=--composefs-backend
      else
        # ostree backend installs the bootloader via bootupd, which by default
        # only writes EFI/<vendor>/ + an efibootmgr NVRAM entry. A fresh VM has
        # empty NVRAM, so it needs the removable fallback path EFI/BOOT/BOOTX64.EFI
        # — that's exactly what --generic-image adds (and it skips firmware changes).
        COMPOSEFS=0; FS=xfs; BACKEND=--generic-image
      fi
      echo "CORRAL_COMPOSEFS=$COMPOSEFS FS=$FS"
      podman run --rm --privileged --pid=host --security-opt label=type:unconfined_t \
        -v /var/lib/containers:/var/lib/containers -v /dev:/dev -v /root/buildkey.pub:/buildkey.pub:ro \
        "$IMG" bootc install to-disk $BACKEND --filesystem "$FS" --wipe \
        --root-ssh-authorized-keys /buildkey.pub /dev/disk/by-id/virtio-target \
        || { echo CORRAL_BUILD_FAIL install; echo CORRAL_BUILD_FAIL install > /dev/ttyS0 2>/dev/null; sync; sleep 2; poweroff; exit 1; }
      udevadm settle
      # ESP partition index varies by layout (--generic-image, backend);
      # probe for the FAT partition that actually contains /EFI.
      mkdir -p /mnt/esp
      ESP_OK=0
      for P in 1 2 3; do
        if mount /dev/disk/by-id/virtio-target-part$P /mnt/esp 2>/dev/null; then
          if [ -d /mnt/esp/EFI ]; then ESP_OK=1; break; fi
          umount /mnt/esp
        fi
      done
      [ "$ESP_OK" = 1 ] || { echo CORRAL_BUILD_FAIL espmount; echo CORRAL_BUILD_FAIL espmount > /dev/ttyS0 2>/dev/null; sync; sleep 2; poweroff; exit 1; }
      if [ "$COMPOSEFS" = 1 ]; then
        # bootc's composefs backend writes the ESP kernel/initrd from the EROFS
        # store, which zero-fills large files past the inline threshold (corrupt
        # 200MB initrd -> "Volume corrupt" at boot). Re-extract the real bytes
        # from $CTR (created for detection above).
        podman cp "$CTR":/usr/lib/modules /scratch_mods
        KVER=$(ls /scratch_mods | head -1)
        D=$(ls -d /mnt/esp/EFI/Linux/bootc_composefs-* | head -1)
        cp -f /scratch_mods/$KVER/vmlinuz "$D/vmlinuz"
        cp -f /scratch_mods/$KVER/initramfs.img "$D/initrd"
        # --root-ssh-authorized-keys is a no-op on composefs; /root -> /var/roothome
        # and /var comes from state/os/default/var. Write the key where it lands.
        mkdir -p /mnt/root; mount /dev/disk/by-id/virtio-target-part3 /mnt/root
        SSHDIR=/mnt/root/state/os/default/var/roothome/.ssh
        mkdir -p "$SSHDIR"; chmod 700 "$SSHDIR"
        cp /root/buildkey.pub "$SSHDIR/authorized_keys"; chmod 600 "$SSHDIR/authorized_keys"; chown -R 0:0 "$SSHDIR"
        sync; umount /mnt/root
      fi
      # console + sshd on the BLS entries (composefs: on ESP).
      for E in /mnt/esp/loader/entries/*.conf; do
        [ -f "$E" ] && (grep -q 'systemd.wants=sshd.service' "$E" \
          || sed -i 's#^options #options console=ttyS0 systemd.wants=sshd.service #' "$E")
      done
      sync; umount /mnt/esp
      if [ "$COMPOSEFS" != 1 ]; then
        # ostree keeps BLS under the root /boot.
        mkdir -p /mnt/root; mount /dev/disk/by-id/virtio-target-part3 /mnt/root 2>/dev/null
        for E in /mnt/root/boot/loader/entries/*.conf; do
          [ -f "$E" ] && (grep -q 'systemd.wants=sshd.service' "$E" \
            || sed -i 's#^options #options console=ttyS0 systemd.wants=sshd.service #' "$E")
        done
        sync; umount /mnt/root 2>/dev/null
      fi
      # Run custom provisioning script chrooted into the root filesystem.
      # The script travels base64-encoded in its own write_files entry:
      # embedding it verbatim here put its lines at column 0, which
      # terminated this YAML block literal and broke the whole cloud-config.
      mkdir -p /mnt/root; mount /dev/disk/by-id/virtio-target-part3 /mnt/root 2>/dev/null
      base64 -d /root/provision.b64 > /mnt/root/tmp/provision.sh 2>/dev/null || true
      if [ -s /mnt/root/tmp/provision.sh ]; then
        chmod +x /mnt/root/tmp/provision.sh
        chroot /mnt/root /bin/bash /tmp/provision.sh
      fi
      rm -f /mnt/root/tmp/provision.sh
      sync; umount /mnt/root 2>/dev/null

      echo CORRAL_BUILD_OK
      # Ensure the marker reaches the serial console even if the early
      # tee-to-ttyS0 redirect hit an I/O error (KubeVirt serial can be
      # transiently unavailable at boot). Write directly + flush.
      echo CORRAL_BUILD_OK > /dev/ttyS0 2>/dev/null || true
      sync; sleep 2; poweroff
runcmd:
  - [ bash, -c, "nohup /root/build.sh &" ]
`

// builderProgressRe selects the meaningful build lines worth streaming to the
// caller from the (very verbose) serial console.
var builderProgressRe = regexp.MustCompile(`CORRAL_|bootc install|Installing|Initializing|Creating root|Bootloader|systemd-boot|Finalizing|podman pull|KVER=|error|[Ff]ail`)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;:]*[a-zA-Z]`)

// waitForBuilderVM polls the builder VM's serial console (via the virt-launcher
// pod's guest-console-log) until it prints CORRAL_BUILD_OK or CORRAL_BUILD_FAIL,
// or the VMI ends, streaming relevant lines to progress. Times out per
// bootcBuildTimeout.
func waitForBuilderVM(name, namespace string, progress io.Writer) error {
	deadline := time.Now().Add(bootcBuildTimeout())
	seen := map[string]bool{}
	var lastFail string

	emit := func(log string) (ok, fail bool) {
		for _, line := range strings.Split(log, "\n") {
			clean := strings.TrimRight(ansiRe.ReplaceAllString(line, ""), "\r")
			if strings.Contains(clean, "CORRAL_BUILD_OK") {
				ok = true
			}
			if i := strings.Index(clean, "CORRAL_BUILD_FAIL"); i >= 0 {
				lastFail = strings.TrimSpace(clean[i:])
				fail = true
			}
			if builderProgressRe.MatchString(clean) && !seen[clean] {
				seen[clean] = true
				fmt.Fprintf(progress, "  %s\n", strings.TrimSpace(clean))
			}
		}
		return
	}

	for time.Now().Before(deadline) {
		log := builderSerialLog(name, namespace)
		if log != "" {
			ok, fail := emit(log)
			if ok {
				return nil
			}
			if fail {
				return fmt.Errorf("builder reported failure (%s)", lastFail)
			}
		}
		// If the VMI has ended (guest powered off), read the final log and
		// decide. Retry the read for a while: right after poweroff the
		// virt-launcher pod is tearing down and `kubectl logs` can come back
		// empty for several seconds — a single read here used to declare
		// completed builds failed ("ended without success" with a finished
		// disk PVC sitting right there).
		if phase := builderVMIPhase(name, namespace); phase == "Succeeded" || phase == "Failed" {
			// Try reading the serial log for up to 60s — the virt-launcher pod
			// may linger briefly after the guest powers off.
			for attempt := 0; attempt < 12; attempt++ {
				time.Sleep(5 * time.Second)
				log := builderSerialLog(name, namespace)
				if log == "" {
					continue
				}
				ok, fail := emit(log)
				if ok {
					return nil
				}
				if fail {
					return fmt.Errorf("builder reported failure (%s)", lastFail)
				}
				break
			}
			// Serial log lost (common: virt-launcher pod terminates before we
			// read). Fall back to the durable ground truth: did the build
			// actually write data to the disk? A successful bootc install
			// produces a 4-8 GB disk image; a failed/empty one is <1 MB.
			if phase == "Succeeded" {
				size := bootcDiskSizeBytes(name, namespace)
				if size > 500*1024*1024 { // >500MB = real OS installed
					fmt.Fprintf(progress, "  Builder powered off cleanly; disk has %d MB of data — success.\n", size/1024/1024)
					return nil
				}
				if size >= 0 {
					return fmt.Errorf("builder ended (Succeeded) but disk is only %d MB — install likely failed; retry with --resume or check the image", size/1024/1024)
				}
			}
			return fmt.Errorf("builder VM ended (%s) without a build marker — if the build actually finished (disk PVC exists), retry with --resume; console: kubectl logs <virt-launcher-%s-*> -c guest-console-log -n %s", phase, name, namespace)
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("timeout waiting for bootc build (%d minutes)", int(bootcBuildTimeout()/time.Minute))
}

// bootcDiskExists reports whether the built disk PVC for a builder is present —
// the source of truth for "did the build produce a disk". builderName is the
// builder VM's name (<vm>-bootc-builder); the disk PVC is <vm>-bootc-disk, so
// we strip the builder suffix to recover the base name. Used to disambiguate a
// clean builder poweroff whose success marker was lost to log truncation.
func bootcDiskExists(builderName, namespace string) bool {
	base := strings.TrimSuffix(builderName, "-bootc-builder")
	err := shell.Command("kubectl", "get", "pvc", base+"-bootc-disk",
		"-n", namespace).Run()
	return err == nil
}

// bootcDiskSizeBytes checks the real data size of the built disk by launching a
// short-lived pod that mounts the PVC and stats disk.img. Returns -1 if the
// check fails (PVC doesn't exist, pod can't schedule, etc). This is the durable
// ground-truth for build success: a real bootc install writes 4-8 GB, a
// failed/empty one stays at the sparse file header size (<1 MB).
func bootcDiskSizeBytes(builderName, namespace string) int64 {
	base := strings.TrimSuffix(builderName, "-bootc-builder")
	pvcName := base + "-bootc-disk"
	podName := base + "-sizecheck"

	// Clean up any leftover from a prior attempt.
	shell.Command("kubectl", "delete", "pod", podName, "-n", namespace, "--ignore-not-found", "--wait=false").Run()
	time.Sleep(2 * time.Second)

	manifest := fmt.Sprintf(`{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": {"name": %q, "namespace": %q},
		"spec": {
			"restartPolicy": "Never",
			"containers": [{"name": "c", "image": "busybox",
				"command": ["sh", "-c", "du -sb /disk/ 2>/dev/null | cut -f1"],
				"volumeMounts": [{"name": "d", "mountPath": "/disk"}]}],
			"volumes": [{"name": "d", "persistentVolumeClaim": {"claimName": %q}}]
		}
	}`, podName, namespace, pvcName)

	cmd := shell.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		shell.Command("kubectl", "delete", "pod", podName, "-n", namespace, "--ignore-not-found").Run()
		_ = out
		return -1
	}

	// Wait for the pod to complete (up to 60s).
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := shell.Command("kubectl", "get", "pod", podName, "-n", namespace,
			"-o", "jsonpath={.status.phase}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "Succeeded" {
			break
		}
		if err == nil && strings.TrimSpace(string(out)) == "Failed" {
			shell.Command("kubectl", "delete", "pod", podName, "-n", namespace, "--ignore-not-found").Run()
			return -1
		}
		time.Sleep(3 * time.Second)
	}

	out, err := shell.Command("kubectl", "logs", podName, "-n", namespace).Output()
	shell.Command("kubectl", "delete", "pod", podName, "-n", namespace, "--ignore-not-found").Run()
	if err != nil {
		return -1
	}
	sizeStr := strings.TrimSpace(string(out))
	var size int64
	if _, err := fmt.Sscanf(sizeStr, "%d", &size); err != nil {
		return -1
	}
	return size
}

// builderSerialLog returns the captured serial console of the builder VM, read
// from its virt-launcher pod's guest-console-log container. Empty if not ready.
func builderSerialLog(name, namespace string) string {
	pod := builderLauncherPod(name, namespace)
	if pod == "" {
		return ""
	}
	out, err := shell.Command("kubectl", "logs", pod, "-c", "guest-console-log", "-n", namespace).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func builderLauncherPod(name, namespace string) string {
	out, err := shell.Command("kubectl", "get", "pod", "-n", namespace,
		"-l", "kubevirt.io/created-by",
		"--field-selector=status.phase!=Failed",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}").Output()
	if err != nil {
		return ""
	}
	want := "virt-launcher-" + name + "-"
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(p, want) {
			return p
		}
	}
	return ""
}

func builderVMIPhase(name, namespace string) string {
	out, err := shell.Command("kubectl", "get", "vmi", name, "-n", namespace,
		"-o", "jsonpath={.status.phase}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// generateBootcVM builds the final VM manifest: it boots the installed block-mode
// disk PVC via UEFI firmware + the bootloader bootc installed (systemd-boot or
// GRUB). No kernelBoot — the disk is fully self-bootable.
func generateBootcVM(name, namespace, pvcName, imageURI, sshKey, mem string, cpu int, node string) map[string]any {
	if mem == "" {
		mem = "4G"
	}
	if cpu == 0 {
		cpu = 2
	}

	spec := map[string]any{
		"running": false,
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"kubevirt.io/vm": name}},
			"spec": map[string]any{
				"domain": map[string]any{
					"cpu":     cpuSpec(cpu),
					"memory":  memSpec(parseMem(mem)),
					"machine": map[string]any{"type": "q35"},
					"firmware": map[string]any{
						"bootloader": map[string]any{
							"efi": map[string]any{"secureBoot": false},
						},
					},
					"devices": map[string]any{
						"disks": []map[string]any{
							{
								"name":      "rootdisk",
								"disk":      map[string]any{"bus": "virtio"},
								"bootOrder": 1,
							},
						},
						"interfaces": []map[string]any{
							{"name": "default", "masquerade": map[string]any{}},
						},
					},
				},
				"networks": []map[string]any{{"name": "default", "pod": map[string]any{}}},
				"volumes": []map[string]any{
					{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": pvcName}},
				},
			},
		},
	}

	if node != "" {
		spec["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"] = map[string]any{
			"kubernetes.io/hostname": node,
		}
	}

	return map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    map[string]any{"corral": name, "corral-bootc-image": ""},
			"annotations": map[string]any{
				"corral.bootc/image": imageURI,
				// Record the SSH key so `bootc rebuild/upgrade/switch` can re-bake
				// it: --wipe destroys /var (where the composefs key lives), and the
				// web server pod has no ~/.ssh to fall back on.
				"corral.bootc/ssh-key": strings.TrimSpace(sshKey),
			},
		},
		"spec": spec,
	}
}
