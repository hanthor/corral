// Package ct implements Containers (CT) — Proxmox-style pet pods. See
// docs/adr/0005-containers-as-pet-pods.md and CONTEXT.md's Container (CT)
// entry for the design. Unlike pkg/qemu and pkg/kubevirt, a CT is not a VM
// and doesn't implement types.Backend — it's a plain Kubernetes pod with a
// persistent-volume-backed data directory, not a hypervisor-backed guest.
package ct

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/tuna-os/corral/pkg/shell"
)

// CreateOpts describes a new CT.
type CreateOpts struct {
	Name         string
	Namespace    string
	Image        string
	CPU          int                 // vCPU cores → pod CPU limit/request
	Mem          string              // e.g. "512Mi" → pod memory limit/request
	Disk         string              // PVC size, e.g. "5Gi"
	StorageClass string              // "" = cluster default
	Privileged   bool                // PVE's "Privileged" checkbox
	Init         bool                // run the image's own entrypoint (curated CT images, e.g. ct-debian's sshd init) instead of sleep
	Mounts       []DevContainerMount // additional volume mounts from devcontainer.json
}

// CT describes a running or stopped Container as reported by ListCTs.
type CT struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Node       string `json:"node"`  // K8s node the pod is scheduled on; "" if stopped/unscheduled
	Phase      string `json:"phase"` // pod phase: Running, Pending, Succeeded, Failed, or "Stopped" (no pod)
	Ready      bool   `json:"ready"` // Running and containers ready
	Image      string `json:"image"`
	CPU        int    `json:"cpu"`
	Mem        string `json:"mem"`
	Privileged bool   `json:"privileged"`
}

// ctSpec is the subset of CreateOpts persisted as a PVC annotation. Stop
// deletes the pod (a bare Pod carries no spec once gone, unlike a KubeVirt
// VM object which persists independent of its VMI) — annotating the PVC,
// which does survive Stop, is what lets Start regenerate an identical pod
// without the caller re-specifying everything. Same pattern as bootc's
// corral.bootc/image annotation.
type ctSpec struct {
	Image      string              `json:"image"`
	CPU        int                 `json:"cpu"`
	Mem        string              `json:"mem"`
	Privileged bool                `json:"privileged"`
	Init       bool                `json:"init,omitempty"`
	Mounts     []DevContainerMount `json:"mounts,omitempty"`
}

const (
	specAnnotation  = "corral.ct/spec"
	labelCT         = "corral.dev/ct"
	dataMountPath   = "/data"
	rootfsMountPath = "/corral-rootfs"
	rootfsMarker    = rootfsMountPath + "/.corral-seeded"
)

// bootstrapScript is the entrypoint for a privileged (distrobox-style) CT.
// Unprivileged CTs mount the PVC at /data and run `sleep infinity` straight
// off the image — anything installed there (apt/dnf/apk) is lost the moment
// the pod is deleted, since Kubernetes gives every pod restart a fresh
// filesystem from the image; there's no docker/podman-style "stopped
// container keeps its writable layer" here. A privileged CT instead seeds
// the PVC with a full copy of the image's own root filesystem on first
// boot, then chroots into it — so the PVC *is* the rootfs, and package
// installs, dotfiles, anything under / all survive Stop/Start exactly like
// a real distrobox container survives being stopped and re-entered. Needs
// CAP_SYS_ADMIN (mount) + CAP_SYS_CHROOT (chroot), which is why this mode
// is gated behind Privileged rather than being the unconditional default —
// same tradeoff Proxmox's own unprivileged-vs-privileged LXC split makes.
func bootstrapScript() string {
	return `set -e
ROOTFS="` + rootfsMountPath + `"
MARKER="` + rootfsMarker + `"
if [ ! -e "$MARKER" ]; then
  echo "corral: seeding persistent rootfs from image..."
  cp -a --one-file-system /. "$ROOTFS"
  touch "$MARKER"
  echo "corral: seed complete"
fi
for fs in proc sys dev; do
  mkdir -p "$ROOTFS/$fs"
  mount --rbind "/$fs" "$ROOTFS/$fs"
done
exec chroot "$ROOTFS" /bin/sh -c 'cd /root 2>/dev/null || cd /; exec sleep infinity'`
}

// rootfsExecCommand is what /api/tty execs into for a privileged CT — a
// fresh `kubectl exec` session joins the container's namespaces but starts
// from its original (un-chrooted) root, since chroot only changes the
// calling process's own apparent root, not a namespace visible to sibling
// exec sessions. Re-chrooting on entry (into the same already-seeded PVC)
// is what makes a console session land inside the persistent rootfs rather
// than the throwaway outer image. Prefers bash (present in the target
// images distrobox mode is meant for) but falls back to sh.
func rootfsExecCommand() []string {
	return []string{"chroot", rootfsMountPath, "/bin/sh", "-c", "exec bash 2>/dev/null || exec sh"}
}

var (
	runnerMu sync.RWMutex
	runner   shell.Runner = shell.DefaultKubectl
)

// SetRunner overrides the command runner (for unit tests). Guarded by a
// mutex: handleCreateCT fires ApplyProxy in a background goroutine that
// outlives the request, so a following test's SetRunner call can otherwise
// race with that goroutine's still-in-flight reads.
func SetRunner(r shell.Runner) {
	runnerMu.Lock()
	defer runnerMu.Unlock()
	runner = r
}

func getRunner() shell.Runner {
	runnerMu.RLock()
	defer runnerMu.RUnlock()
	return runner
}

func run(name string, args ...string) ([]byte, error) { return getRunner().Run(name, args...) }

func runStdin(stdin, name string, args ...string) ([]byte, error) {
	return getRunner().RunStdin(stdin, name, args...)
}

func apply(obj map[string]any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = runStdin(string(data), "kubectl", "apply", "-f", "-")
	return err
}

// pvcName is the stable name of a CT's persistent data volume — stable
// across Stop/Start so a new pod remounts the same volume.
func pvcName(name string) string { return name + "-data" }

// generatePVC builds the CT's persistent data volume, annotated with the
// spec Start needs to recreate the pod later.
func generatePVC(opts CreateOpts) (map[string]any, error) {
	spec := ctSpec{Image: opts.Image, CPU: opts.CPU, Mem: opts.Mem, Privileged: opts.Privileged, Init: opts.Init}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	pvcSpec := map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": opts.Disk}},
	}
	if opts.StorageClass != "" {
		pvcSpec["storageClassName"] = opts.StorageClass
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      pvcName(opts.Name),
			"namespace": opts.Namespace,
			"labels":    map[string]any{labelCT: "true", "corral.dev/ct-name": opts.Name},
			"annotations": map[string]any{
				specAnnotation: string(specJSON),
			},
		},
		"spec": pvcSpec,
	}, nil
}

// generatePod builds the CT's pod from a spec (either a fresh CreateOpts or
// one recovered from the PVC annotation on Start). No init/sshd is baked in
// here — that's the curated ct-* image's job (see the ADR); this defaults
// the command to `sleep infinity` so a generic image (no baked-in
// long-running entrypoint) still stays up for exec/console access instead
// of crash-looping on exit.
func generatePod(name, namespace string, spec ctSpec) map[string]any {
	cpu := spec.CPU
	if cpu == 0 {
		cpu = 1
	}
	mem := spec.Mem
	if mem == "" {
		mem = "512Mi"
	}

	// Privileged CTs get a persistent, mutable rootfs (distrobox-on-k8s —
	// see bootstrapScript) instead of the ephemeral-image + /data-only
	// mount unprivileged CTs use.
	// Unprivileged default: force sleep so generic images (whose CMD would
	// exit immediately) stay up. Init opts out for curated CT images whose
	// entrypoint is a real init (ct-debian: sshd + idle). Privileged always
	// uses the bootstrap chroot script — that IS its init.
	command := []string{"sleep", "infinity"}
	mountPath := dataMountPath
	if spec.Privileged {
		command = []string{"/bin/sh", "-c", bootstrapScript()}
		mountPath = rootfsMountPath
	} else if spec.Init {
		command = nil
	}

	// Add devcontainer mounts (bind = hostPath, volume = PVC).
	volumeMounts := []map[string]any{
		{"name": "data", "mountPath": mountPath},
	}
	volumes := []map[string]any{
		{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": pvcName(name)}},
	}
	for _, m := range spec.Mounts {
		vm := map[string]any{"name": m.Name, "mountPath": m.MountPath}
		volumeMounts = append(volumeMounts, vm)
		vol := map[string]any{"name": m.Name}
		if m.HostPath != "" {
			vol["hostPath"] = map[string]any{"path": m.HostPath}
		} else {
			vol["persistentVolumeClaim"] = map[string]any{"claimName": m.PVCName}
		}
		volumes = append(volumes, vol)
	}

	ctr := map[string]any{
		"name":  "ct",
		"image": spec.Image,
		"stdin": true,
		"tty":   true,
		"resources": map[string]any{
			"limits":   map[string]any{"cpu": strconv.Itoa(cpu), "memory": mem},
			"requests": map[string]any{"cpu": strconv.Itoa(cpu), "memory": mem},
		},
		"securityContext": map[string]any{"privileged": spec.Privileged},
		"volumeMounts":    volumeMounts,
	}
	if command != nil {
		ctr["command"] = command // nil = the image's own entrypoint (Init)
	}

	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    map[string]any{labelCT: "true", "corral.dev/ct-name": name},
		},
		"spec": map[string]any{
			"restartPolicy": "Always",
			"containers":    []map[string]any{ctr},
			"volumes":       volumes,
		},
	}
}

// ConsolePorts are the ports exposed on the CT's tailnet Service by
// default — just SSH, matching a plain container's usual single entry
// point. Curated ct-* images (with sshd baked in, per the ADR) would
// listen here; the generic image this mechanism is tested against won't
// answer, same as how a VM's exposed console ports don't imply the guest
// is listening (see kubevirt.ConsolePorts).
var ConsolePorts = []int{22}

// generateService exposes the CT's ports via a plain Service selecting the
// CT pod directly — simpler than the VM port-proxy (which exists to work
// around KubeVirt VMs not having a stable pod selector; a CT's own pod is
// the pod). tailscaleAnnotations, if non-nil, exposes it on the tailnet the
// same way VM proxies do.
func generateService(name, namespace string, ports []int, tailscaleAnnotations map[string]string) map[string]any {
	svcPorts := make([]map[string]any, 0, len(ports))
	for _, p := range ports {
		svcPorts = append(svcPorts, map[string]any{
			"port": p, "targetPort": p, "name": fmt.Sprintf("port-%d", p), "protocol": "TCP",
		})
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":        name + "-svc",
			"namespace":   namespace,
			"annotations": tailscaleAnnotations,
		},
		"spec": map[string]any{
			"type":     "ClusterIP",
			"selector": map[string]string{"corral.dev/ct-name": name},
			"ports":    svcPorts,
		},
	}
}

func tailscaleOperatorPresent() bool {
	_, err := run("kubectl", "get", "ingressclass", "tailscale")
	return err == nil
}

// ApplyProxy exposes a CT's ports via a tailnet Service. No-op if the
// Tailscale K8s operator isn't installed — same skip-when-absent behavior
// as kubevirt.ApplyProxy, for the same reason (the Service annotation is
// useless without the operator to act on it).
func ApplyProxy(name, namespace string, ports []int) error {
	if !tailscaleOperatorPresent() {
		return nil
	}
	annotations := map[string]string{
		"tailscale.com/expose":   "true",
		"tailscale.com/hostname": name + "-ct",
	}
	return apply(generateService(name, namespace, ports, annotations))
}

// Create provisions a CT: the PVC (annotated with its spec) and the pod.
func Create(opts CreateOpts) error {
	if opts.Disk == "" {
		opts.Disk = "5Gi"
	}
	pvc, err := generatePVC(opts)
	if err != nil {
		return err
	}
	if err := apply(pvc); err != nil {
		return fmt.Errorf("creating data PVC: %w", err)
	}
	// Create PVCs for devcontainer volume mounts.
	for _, m := range opts.Mounts {
		if m.PVCName == "" {
			continue // bind mount, no PVC needed
		}
		mountPVC := map[string]any{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata":   map[string]any{"name": m.PVCName, "namespace": opts.Namespace},
			"spec": map[string]any{
				"accessModes": []string{"ReadWriteOnce"},
				"resources":   map[string]any{"requests": map[string]any{"storage": "1Gi"}},
			},
		}
		if opts.StorageClass != "" {
			mountPVC["spec"].(map[string]any)["storageClassName"] = opts.StorageClass
		}
		if err := apply(mountPVC); err != nil {
			return fmt.Errorf("creating mount PVC %s: %w", m.PVCName, err)
		}
	}
	spec := ctSpec{Image: opts.Image, CPU: opts.CPU, Mem: opts.Mem, Privileged: opts.Privileged, Init: opts.Init, Mounts: opts.Mounts}
	if err := apply(generatePod(opts.Name, opts.Namespace, spec)); err != nil {
		return fmt.Errorf("creating pod: %w", err)
	}
	return nil
}

// specFromPVC reads a CT's spec back from its data PVC's annotation — used
// by Start to recreate the pod after Stop deleted it.
func specFromPVC(name, namespace string) (ctSpec, error) {
	out, err := run("kubectl", "get", "pvc", pvcName(name), "-n", namespace,
		"-o", "jsonpath={.metadata.annotations.corral\\.ct/spec}")
	if err != nil {
		return ctSpec{}, fmt.Errorf("reading CT spec (has it been created?): %w", err)
	}
	var spec ctSpec
	if err := json.Unmarshal(out, &spec); err != nil {
		return ctSpec{}, fmt.Errorf("CT spec annotation is corrupt: %w", err)
	}
	return spec, nil
}

// ExecCommand returns the command /api/tty should exec into a CT's
// container: a plain shell for the default (ephemeral-image) mode, or a
// re-chroot into the persistent rootfs for a privileged (distrobox-style)
// CT — see bootstrapScript/rootfsExecCommand for why a fresh exec session
// needs to re-chroot rather than landing inside it automatically.
func ExecCommand(name, namespace string) ([]string, error) {
	spec, err := specFromPVC(name, namespace)
	if err != nil {
		return nil, err
	}
	if spec.Privileged {
		return rootfsExecCommand(), nil
	}
	return []string{"sh"}, nil
}

// Console opens an interactive exec session on the CT's own terminal
// (inherited stdio) — the CLI/TUI equivalent of /api/tty's websocket
// bridge, for callers that already own a real terminal instead of needing
// one relayed over a websocket. Mirrors kubevirt.Client.SSH's pattern.
func Console(name, namespace string) error {
	shellCmd, err := ExecCommand(name, namespace)
	if err != nil {
		return err
	}
	args := append([]string{"exec", "-it", name, "-n", namespace, "--"}, shellCmd...)
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execArgv builds the argv a one-shot Exec should run: chrooted into the
// persistent rootfs for a privileged CT (matching how a console session
// re-enters it — see rootfsExecCommand), plain `sh -c` otherwise. argv, when
// set, runs directly with no shell (devcontainer.json's postCreateCommand as
// a JSON array); script runs under `sh -c` (postCreateCommand as a string).
// Exactly one of argv/script is set.
func execArgv(privileged bool, argv []string, script string) []string {
	inner := argv
	if len(inner) == 0 {
		inner = []string{"sh", "-c", script}
	}
	if privileged {
		return append([]string{"chroot", rootfsMountPath}, inner...)
	}
	return inner
}

// Exec runs a one-shot, non-interactive command inside a running CT,
// streaming its output to stdout/stderr. Used for devcontainer.json's
// postCreateCommand (see cmd/ct.go's --devcontainer flag and
// devcontainer.go). Exactly one of argv/script should be set — argv runs
// directly (devcontainer's array form), script runs under `sh -c` (string
// form).
func Exec(name, namespace string, argv []string, script string) error {
	spec, err := specFromPVC(name, namespace)
	if err != nil {
		return err
	}
	args := append([]string{"exec", name, "-n", namespace, "--"}, execArgv(spec.Privileged, argv, script)...)
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ExecParallel runs multiple one-shot commands inside a running CT
// concurrently, streaming each to stdout/stderr with a header prefix.
// Returns the first error encountered, but all commands are started
// and run to completion before returning.
func ExecParallel(name, namespace string, cmds []NamedCommand) error {
	if len(cmds) == 0 {
		return nil
	}
	spec, err := specFromPVC(name, namespace)
	if err != nil {
		return err
	}

	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(cmds))

	for _, c := range cmds {
		go func(cmd NamedCommand) {
			args := append([]string{"exec", name, "-n", namespace, "--"},
				execArgv(spec.Privileged, cmd.Argv, cmd.Script)...)
			c := exec.Command("kubectl", args...)
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if cmd.Name != "" {
				fmt.Printf("\n── %s ──\n", cmd.Name)
			}
			results <- result{name: cmd.Name, err: c.Run()}
		}(c)
	}

	var firstErr error
	for range len(cmds) {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("command %q failed: %w", r.name, r.err)
		}
	}
	return firstErr
}

// WaitReady polls ListCTs until name is Ready or timeout elapses. A
// privileged CT's rootfs-seeding bootstrap (see bootstrapScript) runs
// asynchronously after the pod starts, so callers that need to Exec into it
// right after Create (e.g. devcontainer.json's postCreateCommand) must wait
// for real readiness first — the container reporting Ready only once its
// entrypoint's `exec chroot ... sleep infinity` has actually started.
func WaitReady(name, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		cts, err := ListCTs()
		if err == nil {
			for _, c := range cts {
				if c.Name == name && c.Namespace == namespace && c.Ready {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("CT %q not ready within %s", name, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// Start creates the pod from the spec recorded on the data PVC — the PVC
// (and its annotation) survives Stop, so this recreates an identical pod
// without the caller re-specifying image/cpu/mem/privileged.
func Start(name, namespace string) error {
	spec, err := specFromPVC(name, namespace)
	if err != nil {
		return err
	}
	return apply(generatePod(name, namespace, spec))
}

// Stop deletes the pod, keeping the data PVC (and its spec annotation) so
// Start can bring it back.
func Stop(name, namespace string) error {
	_, err := run("kubectl", "delete", "pod", name, "-n", namespace, "--ignore-not-found")
	return err
}

// Delete removes the CT entirely: pod, data PVC, and (if present) its
// Service.
func Delete(name, namespace string) error {
	run("kubectl", "delete", "pod", name, "-n", namespace, "--ignore-not-found")
	run("kubectl", "delete", "svc", name+"-svc", "-n", namespace, "--ignore-not-found")
	_, err := run("kubectl", "delete", "pvc", pvcName(name), "-n", namespace, "--ignore-not-found")
	return err
}

// Exists reports whether a CT (its data PVC — the durable half of a CT's
// identity) exists, regardless of whether it's currently started.
func Exists(name, namespace string) bool {
	_, err := run("kubectl", "get", "pvc", pvcName(name), "-n", namespace, "-o", "name")
	return err == nil
}

// ListCTs returns every CT across all namespaces. A CT's durable identity
// is its data PVC (labeled corral.dev/ct=true) — it exists whether or not
// the pod is currently running, same as a stopped VM still being a VM.
// Live phase/readiness come from the pod when one exists; "Stopped"
// otherwise.
func ListCTs() ([]CT, error) {
	pvcOut, err := run("kubectl", "get", "pvc", "-A", "-l", labelCT+"=true", "-o", "json")
	if err != nil {
		return nil, err
	}
	var pvcRes struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Annotations map[string]string `json:"annotations"`
				Labels      map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(pvcOut, &pvcRes); err != nil {
		return nil, err
	}

	type podInfo struct {
		Phase string
		Ready bool
		Node  string
	}
	pods := map[string]podInfo{} // "ns/name" -> info
	if podOut, err := run("kubectl", "get", "pods", "-A", "-l", labelCT+"=true", "-o", "json"); err == nil {
		var podRes struct {
			Items []struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Spec struct {
					NodeName string `json:"nodeName"`
				} `json:"spec"`
				Status struct {
					Phase             string `json:"phase"`
					ContainerStatuses []struct {
						Ready bool `json:"ready"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(podOut, &podRes) == nil {
			for _, p := range podRes.Items {
				ready := len(p.Status.ContainerStatuses) > 0
				for _, cs := range p.Status.ContainerStatuses {
					ready = ready && cs.Ready
				}
				pods[p.Metadata.Namespace+"/"+p.Metadata.Name] = podInfo{
					Phase: p.Status.Phase, Ready: ready, Node: p.Spec.NodeName,
				}
			}
		}
	}

	var out []CT
	for _, item := range pvcRes.Items {
		name := item.Metadata.Labels["corral.dev/ct-name"]
		if name == "" {
			continue // not one of ours (shouldn't happen given the label selector, but be defensive)
		}
		ns := item.Metadata.Namespace
		var spec ctSpec
		json.Unmarshal([]byte(item.Metadata.Annotations[specAnnotation]), &spec)

		phase, ready, node := "Stopped", false, ""
		if pi, ok := pods[ns+"/"+name]; ok {
			phase, ready, node = pi.Phase, pi.Ready, pi.Node
		}
		out = append(out, CT{
			Name: name, Namespace: ns, Node: node, Phase: phase, Ready: ready,
			Image: spec.Image, CPU: spec.CPU, Mem: spec.Mem, Privileged: spec.Privileged,
		})
	}
	return out, nil
}
