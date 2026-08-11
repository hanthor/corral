// KubeVirt manifest generation and proxy management.
//
// Extracted from client.go: these are free functions (no *Client receiver)
// that build or apply declarative VM/proxy objects. Keeping them separate
// from transport/lifecycle means the pure generators can be table-tested
// without a client, and client.go stays focused on the Backend/VMAdvanced
// seams it implements (see interfaces.go).
package kubevirt

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/types"
)

func GenerateVM(opts types.CreateOpts) map[string]any {
	name := opts.Name
	ns := opts.Namespace
	if ns == "" {
		ns = "default"
	}
	mem := opts.Mem
	if mem == "" {
		mem = "4G"
	}
	cpu := opts.CPU
	if cpu == 0 {
		cpu = 2
	}
	diskSize := opts.Disk
	if diskSize == "" {
		diskSize = "20G"
	}

	volumes := []map[string]any{}
	disks := []map[string]any{}

	hasISO := opts.ISO != ""
	hasContainer := opts.ContainerDisk != ""

	if hasISO {
		// ISO as CD-ROM (bootOrder 1)
		volumes = append(volumes, map[string]any{
			"name":                  "iso",
			"persistentVolumeClaim": map[string]any{"claimName": name + "-iso"},
		})
		disks = append(disks, map[string]any{
			"name":      "iso",
			"cdrom":     map[string]any{"bus": "sata"},
			"bootOrder": 1,
		})
		volumes = append(volumes, map[string]any{
			"name":                  "rootdisk",
			"persistentVolumeClaim": map[string]any{"claimName": name + "-disk"},
		})
		disks = append(disks, map[string]any{
			"name":      "rootdisk",
			"disk":      map[string]any{"bus": "virtio"},
			"bootOrder": 2,
		})
	} else if hasContainer {
		volumes = append(volumes, map[string]any{
			"name":          "containerdisk",
			"containerDisk": map[string]any{"image": opts.ContainerDisk},
		})
		disks = append(disks, map[string]any{
			"name": "containerdisk",
			"disk": map[string]any{"bus": "virtio"},
		})
		// Extra persistent data disk only when --disk is explicitly requested;
		// CreateVM creates the matching PVC under the same condition.
		if opts.Disk != "" {
			volumes = append(volumes, map[string]any{
				"name":                  "datadisk",
				"persistentVolumeClaim": map[string]any{"claimName": name + "-data"},
			})
			disks = append(disks, map[string]any{
				"name": "datadisk",
				"disk": map[string]any{"bus": "virtio"},
			})
		}
	} else if opts.PVC != "" {
		volumes = append(volumes, map[string]any{
			"name":                  "rootdisk",
			"persistentVolumeClaim": map[string]any{"claimName": opts.PVC},
		})
		disks = append(disks, map[string]any{
			"name": "rootdisk",
			"disk": map[string]any{"bus": "virtio"},
		})
	} else {
		volumes = append(volumes, map[string]any{
			"name":                  "rootdisk",
			"persistentVolumeClaim": map[string]any{"claimName": name + "-disk"},
		})
		disks = append(disks, map[string]any{
			"name": "rootdisk",
			"disk": map[string]any{"bus": "virtio"},
		})
	}

	// Cloud-init
	pwd := opts.CloudInitPassword
	if pwd == "" {
		pwd = randomPassword()
	}
	LastPassword = pwd

	userData := fmt.Sprintf("#cloud-config\npassword: %s\nchpasswd:\n  expire: False\nssh_pwauth: true\n", pwd)
	if opts.SSHPublicKey != "" {
		// May hold several keys (e.g. resolved from a GitHub account),
		// newline-separated — each becomes its own list entry.
		userData += "ssh_authorized_keys:\n"
		for _, k := range strings.Split(opts.SSHPublicKey, "\n") {
			if k = strings.TrimSpace(k); k != "" {
				userData += fmt.Sprintf("  - %s\n", k)
			}
		}
	}
	// Tailnet access is provided by the Tailscale operator proxy (ApplyProxy),
	// not by joining from inside the guest — so no in-guest tailscale here.
	if opts.CloudInitExtra != "" {
		userData = mergeCloudInit(userData, opts.CloudInitExtra)
	}
	volumes = append(volumes, map[string]any{
		"name": "cloudinitdisk",
		"cloudInitNoCloud": map[string]any{
			"userData": userData,
		},
	})
	disks = append(disks, map[string]any{
		"name": "cloudinitdisk",
		"disk": map[string]any{"bus": "virtio"},
	})

	memMib := parseMem(mem)

	domain := map[string]any{
		"devices": map[string]any{
			"disks": disks,
			// masquerade (NAT) binding is required for live migration;
			// the default bridge binding pins the VM to its node.
			"interfaces": []map[string]any{
				{"name": "default", "masquerade": map[string]any{}},
			},
		},
	}
	if opts.UEFI {
		// A guest whose disk was installed under UEFI has its bootloader in an
		// ESP and nothing in the MBR, so it boots to a blank screen on the
		// default BIOS firmware. secureBoot is left off: it needs an EFI vars
		// PVC and a signed bootloader, and turning it on silently would break
		// exactly the imported guests this exists to serve.
		domain["firmware"] = map[string]any{
			"bootloader": map[string]any{
				"efi": map[string]any{"secureBoot": false},
			},
		}
	}
	// An instancetype supplies CPU/memory (and hotplug headroom); only set the
	// domain cpu/memory when not using one.
	if opts.InstanceType == "" {
		domain["cpu"] = cpuSpec(cpu)
		domain["memory"] = memSpec(memMib)
	}

	spec := map[string]any{
		"running": false,
		"template": map[string]any{
			"metadata": map[string]any{
				"labels": map[string]any{"kubevirt.io/vm": name},
			},
			"spec": map[string]any{
				"domain": domain,
				"networks": []map[string]any{
					{"name": "default", "pod": map[string]any{}},
				},
				"volumes": volumes,
			},
		},
	}

	if opts.InstanceType != "" {
		spec["instancetype"] = map[string]any{
			"kind": "VirtualMachineClusterInstancetype",
			"name": opts.InstanceType,
		}
	}
	if opts.Preference != "" {
		spec["preference"] = map[string]any{
			"kind": "VirtualMachineClusterPreference",
			"name": opts.Preference,
		}
	}

	if opts.Node != "" {
		spec["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"] = map[string]any{
			"kubernetes.io/hostname": opts.Node,
		}
	}

	labels := map[string]any{"corral": name}
	metadata := map[string]any{
		"name":      name,
		"namespace": ns,
		"labels":    labels,
	}
	if opts.Ephemeral {
		labels["corral.dev/ephemeral"] = "true"
		metadata["annotations"] = map[string]any{
			"corral.dev/expires-at": time.Now().Add(ephemeralTTL(opts.TTL)).Format(time.RFC3339),
		}
	}

	return map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   metadata,
		"spec":       spec,
	}
}

// ephemeralTTL parses opts.TTL (e.g. "4h", "30m"); an empty or invalid value
// falls back to a conservative default rather than erroring at create time —
// getting *some* GC is better than silently getting none over a typo.
func ephemeralTTL(ttl string) time.Duration {
	const defaultTTL = 4 * time.Hour
	if ttl == "" {
		return defaultTTL
	}
	d, err := time.ParseDuration(ttl)
	if err != nil || d <= 0 {
		return defaultTTL
	}
	return d
}

// GeneratePVC creates a PersistentVolumeClaim manifest.
func GeneratePVC(name, namespace, size string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			// ManagedLabel marks this PVC as corral-created so `corral gc` can
			// safely reclaim it when its VM is gone, without ever matching an
			// unrelated application PVC by name.
			"labels": map[string]any{ManagedLabel: "true"},
		},
		"spec": map[string]any{
			"accessModes": []string{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{"storage": size},
			},
		},
	}
}

// isoSizeFallbackGi is used when DetectISOSize can't determine the real
// size (HEAD blocked, no Content-Length). Deliberately generous — CDI
// crash-loops forever on an undersized PVC rather than failing fast (see
// DetectISOSize's doc comment for the incident that found this), so a
// too-big guess is far cheaper than a too-small one.
const isoSizeFallbackGi = 12

// DetectISOSize does a best-effort HTTP HEAD on isoURL to size its PVC
// correctly. GenerateDataVolume used to hardcode "6Gi" for every ISO
// regardless of actual size — harmless for small Linux install ISOs, but a
// real Windows 11 ISO (~7.2GB) blew straight through it: CDI's importer
// doesn't fail fast on an undersized target, it crash-loops (exit 1,
// restart, retry, repeat) forever, silently burning hours before anyone
// notices the DataVolume is stuck. Found and fixed after a live Windows
// VM creation sat crash-looping for 5+ hours undetected.
//
// Rounds the detected size up to the next GiB plus a 1GiB safety margin.
// Falls back to isoSizeFallbackGi on any failure (network error, HEAD not
// supported, no Content-Length) — best-effort, not a hard requirement.
func DetectISOSize(isoURL string) string {
	resp, err := http.Head(isoURL)
	if err != nil {
		return fmt.Sprintf("%dGi", isoSizeFallbackGi)
	}
	defer resp.Body.Close()
	if resp.ContentLength <= 0 {
		return fmt.Sprintf("%dGi", isoSizeFallbackGi)
	}
	const gib = 1 << 30
	const minSizeGi = 2                               // a real ISO smaller than this is unusual; floor rather than provision a suspiciously tiny PVC
	sizeGi := int((resp.ContentLength+gib-1)/gib) + 1 // round up + 1GiB margin
	if sizeGi < minSizeGi {
		sizeGi = minSizeGi
	}
	return fmt.Sprintf("%dGi", sizeGi)
}

// GenerateDataVolume creates a CDI DataVolume to import an ISO from URL.
// size should come from DetectISOSize (or an explicit override) — an
// undersized PVC doesn't fail fast, it crash-loops forever (see
// DetectISOSize's doc comment).
func GenerateDataVolume(name, namespace, isoURL, size string) map[string]any {
	if size == "" {
		size = fmt.Sprintf("%dGi", isoSizeFallbackGi)
	}
	return map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1",
		"kind":       "DataVolume",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			// Provision + import immediately instead of waiting for a consumer.
			// On WaitForFirstConsumer StorageClasses (kind's local-path, many
			// cloud defaults) a standalone library import has no consuming pod,
			// so without this the PVC never binds and the import hangs forever.
			"annotations": map[string]string{
				"cdi.kubevirt.io/storage.bind.immediate.requested": "true",
			},
		},
		"spec": map[string]any{
			"source": map[string]any{
				"http": map[string]any{"url": isoURL},
			},
			"pvc": map[string]any{
				"accessModes": []string{"ReadWriteOnce"},
				"resources": map[string]any{
					"requests": map[string]any{"storage": size},
				},
			},
		},
	}
}

// GenerateBootDataVolume creates a CDI DataVolume that imports a qcow2/raw disk
// image from a URL into a bootable PVC (sized, optional StorageClass).
func GenerateBootDataVolume(name, namespace, url, size, storageClass string) map[string]any {
	if size == "" {
		size = "20G"
	}
	dv := GenerateDataVolume(name, namespace, url, size)
	if storageClass != "" {
		dv["spec"].(map[string]any)["pvc"].(map[string]any)["storageClassName"] = storageClass
	}
	return dv
}

// ProxyTags, when set, tags exposed VM devices on the tailnet
// (tailscale.com/tags annotation), e.g. "tag:corral-vm".
var ProxyTags string

func proxyAnnotations(name string) map[string]string {
	a := map[string]string{
		"tailscale.com/expose":   "true",
		"tailscale.com/hostname": name + "-vm",
	}
	if ProxyTags != "" {
		a["tailscale.com/tags"] = ProxyTags
	}
	return a
}

// GenerateProxyService creates the unified proxy Service with Tailscale annotation.
func GenerateProxyService(name, namespace string, ports []int) map[string]any {
	svcPorts := []map[string]any{}
	for _, p := range ports {
		svcPorts = append(svcPorts, map[string]any{
			"port":       p,
			"targetPort": p,
			"name":       fmt.Sprintf("port-%d", p),
			"protocol":   "TCP",
		})
	}

	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":        name + "-proxy",
			"namespace":   namespace,
			"annotations": proxyAnnotations(name),
		},
		"spec": map[string]any{
			"type": "ClusterIP",
			"selector": map[string]string{
				"app": "corral-proxy",
				"vm":  name,
			},
			"ports": svcPorts,
		},
	}
}

// GenerateProxyDeployment creates the proxy Deployment that forwards all ports.
func GenerateProxyDeployment(name, namespace string, ports []int) map[string]any {
	containerPorts := []map[string]any{}
	for _, p := range ports {
		containerPorts = append(containerPorts, map[string]any{
			"containerPort": p,
			"name":          fmt.Sprintf("port-%d", p),
			"protocol":      "TCP",
		})
	}

	hasVNC := false
	for _, p := range ports {
		if p == 5900 {
			hasVNC = true
			break
		}
	}

	// Build shell script
	var script strings.Builder
	script.WriteString("apk add --no-cache curl socat jq >/dev/null 2>&1\n")
	if hasVNC {
		script.WriteString(fmt.Sprintf("/tmp/virtctl vnc %s -n %s --proxy-only --address=0.0.0.0 --port=5900 &\n", name, namespace))
	}

	script.WriteString(fmt.Sprintf(`while true; do
  IP=$(curl -sS --cacert /var/run/secrets/kubernetes.io/serviceaccount/ca.crt \
       -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
       "https://kubernetes.default.svc/apis/kubevirt.io/v1/namespaces/%s/virtualmachineinstances/%s" | \
       jq -r '.status.interfaces[0].ipAddress // empty' 2>/dev/null)
  if [ -n "$IP" ]; then
`, namespace, name))

	for _, p := range ports {
		if p == 5900 {
			continue // VNC handled above
		}
		script.WriteString(fmt.Sprintf("    socat TCP-LISTEN:%d,fork,reuseaddr TCP:$IP:%d &\n", p, p))
	}
	script.WriteString("    wait\n  fi\n  sleep 5\ndone\n")

	// virtctl is only needed for the VNC proxy
	var initContainers []map[string]any
	if hasVNC {
		initContainers = append(initContainers, map[string]any{
			"name":  "install-tools",
			"image": AlpineImage,
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"capabilities":             map[string]any{"drop": []string{"ALL"}},
				"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
			},
			"command": []string{"sh", "-c", "apk add --no-cache curl >/dev/null 2>&1\ncurl -sSL \"https://github.com/kubevirt/kubevirt/releases/download/v1.8.2/virtctl-v1.8.2-linux-amd64\" -o /tmp/virtctl\nchmod +x /tmp/virtctl"},
			"volumeMounts": []map[string]any{
				{"name": "tools", "mountPath": "/tmp"},
			},
		})
	}

	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name + "-proxy",
			"namespace": namespace,
			"labels": map[string]string{
				"app": "corral-proxy",
				"vm":  name,
			},
		},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{
				"matchLabels": map[string]string{
					"app": "corral-proxy",
					"vm":  name,
				},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]string{
						"app": "corral-proxy",
						"vm":  name,
					},
				},
				"spec": map[string]any{
					"serviceAccountName": "corral-" + name + "-proxy",
					"securityContext": map[string]any{
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"initContainers": initContainers,
					"containers": []map[string]any{
						{
							"name":  "proxy",
							"image": AlpineImage,
							"securityContext": map[string]any{
								"allowPrivilegeEscalation": false,
								"capabilities":             map[string]any{"drop": []string{"ALL"}},
								"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
							},
							"command": []string{"sh", "-c", script.String()},
							"ports":   containerPorts,
							"volumeMounts": []map[string]any{
								{"name": "tools", "mountPath": "/tmp", "readOnly": true},
							},
						},
					},
					"volumes": []map[string]any{
						{"name": "tools", "emptyDir": map[string]any{}},
					},
				},
			},
		},
	}
}

// GenerateLANService creates a LoadBalancer-type Service fronting the same
// proxy Deployment ApplyProxy's tailnet Service targets (same selector —
// see GenerateProxyDeployment), so a VM can be reached on the LAN without
// Multus/a secondary NIC. Unlike ApplyProxy this carries no vendor-specific
// annotations: any controller that fulfills LoadBalancer Services — Cilium's
// own L2 Announcement or BGP Control Plane, or MetalLB — assigns the
// external IP the same way, so this works regardless of which one a given
// cluster runs. If none is installed, the Service just sits <pending>,
// same as it would with a plain `kubectl apply` of a LoadBalancer Service.
func GenerateLANService(name, namespace string, ports []int) map[string]any {
	svcPorts := []map[string]any{}
	for _, p := range ports {
		svcPorts = append(svcPorts, map[string]any{
			"port":       p,
			"targetPort": p,
			"name":       fmt.Sprintf("port-%d", p),
			"protocol":   "TCP",
		})
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      name + "-lan",
			"namespace": namespace,
			"labels": map[string]string{
				"app": "corral-proxy",
				"vm":  name,
			},
		},
		"spec": map[string]any{
			"type": "LoadBalancer",
			"selector": map[string]string{
				"app": "corral-proxy",
				"vm":  name,
			},
			"ports": svcPorts,
		},
	}
}

// ApplyLANService ensures the shared proxy Deployment/RBAC exist (same ones
// ApplyProxy's tailnet Service targets — applying them twice is a no-op,
// kubectl apply is idempotent) and applies the LoadBalancer Service. Unlike
// ApplyProxy, this doesn't gate on any specific operator being present —
// there's no reliable single "is a LoadBalancer-Service controller
// installed" check the way tailscaleOperatorPresent checks for one
// IngressClass, so it always applies and simply sits <pending> if nothing
// fulfills it.
func ApplyLANService(name, ns string, ports []int) error {
	if err := applyProxyManifest("RBAC", GenerateProxyRBAC(name, ns)); err != nil {
		return err
	}
	deploy, _ := json.Marshal(GenerateProxyDeployment(name, ns, ports))
	if err := applyProxyManifest("deployment", string(deploy)); err != nil {
		return err
	}
	svc, _ := json.Marshal(GenerateLANService(name, ns, ports))
	return applyProxyManifest("LAN service", string(svc))
}

// LANServiceIP returns the external IP a LoadBalancer controller has
// assigned to name-lan, or "" if it's still <pending> (or doesn't exist).
func LANServiceIP(name, ns string) string {
	out, err := runPkg("kubectl", "get", "svc", name+"-lan", "-n", ns, "-o", "json")
	if err != nil {
		return ""
	}
	var svc struct {
		Status struct {
			LoadBalancer struct {
				Ingress []struct {
					IP string `json:"ip"`
				} `json:"ingress"`
			} `json:"loadBalancer"`
		} `json:"status"`
	}
	if json.Unmarshal(out, &svc) != nil || len(svc.Status.LoadBalancer.Ingress) == 0 {
		return ""
	}
	return svc.Status.LoadBalancer.Ingress[0].IP
}

// ExposedPorts returns the currently exposed proxy ports for a VM.
func ExposedPorts(name, ns string) []int {
	out, err := runPkg("kubectl", "get", "svc", name+"-proxy", "-n", ns, "-o", "json")
	if err != nil {
		return nil
	}
	var svc struct {
		Spec struct {
			Ports []struct {
				Port int `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if json.Unmarshal(out, &svc) != nil {
		return nil
	}
	var ports []int
	for _, p := range svc.Spec.Ports {
		ports = append(ports, p.Port)
	}
	return ports
}

// applyProxyManifest applies one proxy manifest with retries. The proxy Role
// grants only vmi get + vnc get (both held by corral-web, so the RBAC
// privilege-escalation check passes); retries cover transient apiserver/webhook
// blips during the post-build churn.
func applyProxyManifest(label, manifest string) error {
	var err error
	for attempt := 0; attempt < 12; attempt++ {
		if err = applyManifest(manifest); err == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("proxy %s: %w", label, err)
}

// ConsolePorts are the ports exposed on the tailnet proxy for any kubevirt
// VM: SSH, VNC, RDP. Opening a port nobody's listening on is harmless (the
// tailnet is already the auth boundary — see ADR-0003); this is the uniform
// list every VM-creation path should pass to ApplyProxy, regardless of guest
// OS, so console access doesn't silently vary by which flow created the VM.
var ConsolePorts = []int{22, 5900, 3389}

// ApplyProxy creates/updates the proxy resources for a VM.
func ApplyProxy(name, ns string, ports []int) error {
	// The proxy Service is only useful if the Tailscale K8s operator is there to
	// turn its `tailscale.com/expose` annotation into a tailnet device. Without
	// the operator (e.g. a plain/kind cluster) the proxy Deployment is just a
	// useless socat pod, so skip it — keeps non-operator clusters clean.
	if !tailscaleOperatorPresent() {
		return nil
	}
	if err := applyProxyManifest("RBAC", GenerateProxyRBAC(name, ns)); err != nil {
		return err
	}
	svc, _ := json.Marshal(GenerateProxyService(name, ns, ports))
	if err := applyProxyManifest("service", string(svc)); err != nil {
		return err
	}
	deploy, _ := json.Marshal(GenerateProxyDeployment(name, ns, ports))
	if err := applyProxyManifest("deployment", string(deploy)); err != nil {
		return err
	}
	return nil
}

// tailscaleOperatorPresent reports whether the Tailscale K8s operator is
// installed, by checking for the `tailscale` IngressClass it registers. Used to
// skip exposing VMs on clusters without the operator.
func tailscaleOperatorPresent() bool {
	_, err := runPkg("kubectl", "get", "ingressclass", "tailscale")
	return err == nil
}

// DeleteProxy removes all proxy resources for a VM, including the LAN
// Service ApplyLANService may have created (it shares the same proxy
// Deployment/RBAC, but is a separate Service that isn't otherwise cleaned
// up by deleting those).
func DeleteProxy(name, ns string) error {
	for _, kind := range []string{"deploy", "svc", "sa", "role", "rolebinding"} {
		rname := name + "-proxy"
		if kind != "deploy" && kind != "svc" {
			rname = "corral-" + name + "-proxy"
		}
		shell.Command("kubectl", "delete", kind, rname, "-n", ns, "--ignore-not-found").Run()
	}
	shell.Command("kubectl", "delete", "svc", name+"-lan", "-n", ns, "--ignore-not-found").Run()
	return nil
}

// GenerateProxyRBAC creates RBAC resources for the proxy.
func GenerateProxyRBAC(name, ns string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: corral-%s-proxy
  namespace: %s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: corral-%s-proxy
  namespace: %s
rules:
  - apiGroups: ["subresources.kubevirt.io"]
    resources: ["virtualmachineinstances/vnc"]
    verbs: ["get"]
  - apiGroups: ["kubevirt.io"]
    resources: ["virtualmachineinstances"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: corral-%s-proxy
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: corral-%s-proxy
subjects:
  - kind: ServiceAccount
    name: corral-%s-proxy
`, name, ns, name, ns, name, ns, name, name)
}
