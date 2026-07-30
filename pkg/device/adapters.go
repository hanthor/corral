package device

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/corral/pkg/types"
)

// ── libvirt ───────────────────────────────────────────────────────
//
// Discovery is `virsh nodedev-list --cap pci` plus `nodedev-dumpxml` per
// device, which reports vendor, product, driver and address in a stable schema.
//
// Attachment uses a <hostdev managed='yes'> element. managed='yes' is the
// deliberate choice: libvirt unbinds the device from its host driver when the
// domain starts and rebinds it when the domain stops. The alternative — an
// explicit `virsh nodedev-detach` — leaves the host without the device even
// when the guest is off, which is a surprising state to leave a machine in.

type Libvirt struct{}

func (Libvirt) virsh(context string, args ...string) ([]byte, error) {
	uri := context
	if uri == "" {
		uri = "qemu:///system"
	}
	return runner.Run("virsh", append([]string{"-c", uri}, args...)...)
}

// nodedevXML is the subset of libvirt's node-device schema this needs.
type nodedevXML struct {
	Name       string `xml:"name"`
	Driver     string `xml:"driver>name"`
	Capability struct {
		Type     string `xml:"type,attr"`
		Domain   int    `xml:"domain"`
		Bus      int    `xml:"bus"`
		Slot     int    `xml:"slot"`
		Function int    `xml:"function"`
		Product  struct {
			ID   string `xml:"id,attr"`
			Name string `xml:",chardata"`
		} `xml:"product"`
		Vendor struct {
			ID   string `xml:"id,attr"`
			Name string `xml:",chardata"`
		} `xml:"vendor"`
	} `xml:"capability"`
}

func (l Libvirt) List(context string) ([]Device, error) {
	out, err := l.virsh(context, "nodedev-list", "--cap", "pci")
	if err != nil {
		return nil, fmt.Errorf("virsh nodedev-list: %s", commandError(out, err))
	}
	var devices []Device
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		dev, err := l.describe(context, name)
		if err != nil {
			// One unreadable device must not hide the rest — a host has
			// dozens and any of them may be in an odd state.
			continue
		}
		devices = append(devices, dev)
	}
	sortDevices(devices)
	return devices, nil
}

func (l Libvirt) describe(context, name string) (Device, error) {
	out, err := l.virsh(context, "nodedev-dumpxml", name)
	if err != nil {
		return Device{}, err
	}
	var doc nodedevXML
	if err := xml.Unmarshal(out, &doc); err != nil {
		return Device{}, err
	}
	address := fmt.Sprintf("%04x:%02x:%02x.%d",
		doc.Capability.Domain, doc.Capability.Bus, doc.Capability.Slot, doc.Capability.Function)
	description := strings.TrimSpace(doc.Capability.Vendor.Name + " " + doc.Capability.Product.Name)
	return Device{
		ID:          address,
		Address:     address,
		Vendor:      strings.TrimPrefix(doc.Capability.Vendor.ID, "0x"),
		Product:     strings.TrimPrefix(doc.Capability.Product.ID, "0x"),
		Description: description,
		Driver:      doc.Driver,
		Class:       classify(description),
		BootDisplay: isBootDisplay(address),
		Ready:       doc.Driver == "vfio-pci",
	}, nil
}

func (Libvirt) Consequences(dev Device) []Consequence { return baseConsequences(dev) }

// hostdevXML renders the <hostdev> element libvirt attaches. The alias name
// is what Detach later removes, so it has to be deterministic.
func hostdevXML(dev Device, name string) (string, error) {
	var domain, bus, slot, function int
	if _, err := fmt.Sscanf(dev.Address, "%04x:%02x:%02x.%d", &domain, &bus, &slot, &function); err != nil {
		return "", fmt.Errorf("%q is not a PCI address (want 0000:01:00.0): %w", dev.Address, err)
	}
	return fmt.Sprintf(`<hostdev mode='subsystem' type='pci' managed='yes'>
  <source>
    <address domain='0x%04x' bus='0x%02x' slot='0x%02x' function='0x%d'/>
  </source>
  <alias name='ua-%s'/>
</hostdev>`, domain, bus, slot, function, name), nil
}

func (l Libvirt) Attach(ref types.InstanceRef, dev Device, name string) error {
	if err := checkSafe(dev); err != nil {
		return err
	}
	doc, err := hostdevXML(dev, name)
	if err != nil {
		return err
	}
	file, cleanup, err := tempXML(doc)
	if err != nil {
		return err
	}
	defer cleanup()
	// --config, not --live: a hostdev cannot be hot-plugged reliably, and
	// pretending otherwise produces a guest that half-sees the device.
	// Consequences() has already told the caller a restart is needed.
	if out, err := l.virsh(ref.Context, "attach-device", ref.Name, file, "--config"); err != nil {
		return fmt.Errorf("virsh attach-device: %s", commandError(out, err))
	}
	return nil
}

func (l Libvirt) Detach(ref types.InstanceRef, name string) error {
	attached, err := l.Attached(ref)
	if err != nil {
		return err
	}
	for _, a := range attached {
		if a.Name != name {
			continue
		}
		doc, err := hostdevXML(Device{Address: a.Device}, name)
		if err != nil {
			return err
		}
		file, cleanup, err := tempXML(doc)
		if err != nil {
			return err
		}
		defer cleanup()
		if out, err := l.virsh(ref.Context, "detach-device", ref.Name, file, "--config"); err != nil {
			return fmt.Errorf("virsh detach-device: %s", commandError(out, err))
		}
		return nil
	}
	return nil // already gone
}

// domainXML is the subset of a domain definition needed to read back which
// host devices are attached.
type domainXML struct {
	Devices struct {
		Hostdev []struct {
			Type   string `xml:"type,attr"`
			Source struct {
				Address struct {
					Domain   string `xml:"domain,attr"`
					Bus      string `xml:"bus,attr"`
					Slot     string `xml:"slot,attr"`
					Function string `xml:"function,attr"`
				} `xml:"address"`
			} `xml:"source"`
			Alias struct {
				Name string `xml:"name,attr"`
			} `xml:"alias"`
		} `xml:"hostdev"`
	} `xml:"devices"`
}

func (l Libvirt) Attached(ref types.InstanceRef) ([]Attachment, error) {
	out, err := l.virsh(ref.Context, "dumpxml", ref.Name)
	if err != nil {
		return nil, fmt.Errorf("virsh dumpxml: %s", commandError(out, err))
	}
	var doc domainXML
	if err := xml.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var attachments []Attachment
	for _, h := range doc.Devices.Hostdev {
		if h.Type != "pci" {
			continue
		}
		a := h.Source.Address
		address := fmt.Sprintf("%s:%s:%s.%s",
			trimHex(a.Domain), trimHex(a.Bus), trimHex(a.Slot), trimHex(a.Function))
		attachments = append(attachments, Attachment{
			Name:   strings.TrimPrefix(h.Alias.Name, "ua-"),
			Device: address,
		})
	}
	return attachments, nil
}

func trimHex(s string) string { return strings.TrimPrefix(s, "0x") }

func tempXML(doc string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "corral-hostdev-*.xml")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(doc); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// ── Incus ─────────────────────────────────────────────────────────
//
// Discovery reads /1.0/resources, which reports GPUs and PCI devices with
// their addresses and IDs. Attachment is `incus config device add … gpu
// pci=<address>` for a GPU and `… pci address=<address>` for anything else —
// Incus models those as separate device types.

type Incus struct{}

// incusResources is the subset of Incus's /1.0/resources this needs.
type incusResources struct {
	GPU struct {
		Cards []struct {
			Driver     string `json:"driver"`
			PCIAddress string `json:"pci_address"`
			Vendor     string `json:"vendor"`
			VendorID   string `json:"vendor_id"`
			Product    string `json:"product"`
			ProductID  string `json:"product_id"`
		} `json:"cards"`
	} `json:"gpu"`
	PCI struct {
		Devices []struct {
			Driver     string `json:"driver"`
			PCIAddress string `json:"pci_address"`
			Vendor     string `json:"vendor"`
			VendorID   string `json:"vendor_id"`
			Product    string `json:"product"`
			ProductID  string `json:"product_id"`
		} `json:"devices"`
	} `json:"pci"`
}

func remoteOr(context string) string {
	if context == "" {
		return "local"
	}
	return context
}

func (i Incus) List(context string) ([]Device, error) {
	remote := remoteOr(context)
	out, err := runner.Run("incus", "query", remote+":/1.0/resources")
	if err != nil {
		return nil, fmt.Errorf("incus query resources: %s", commandError(out, err))
	}
	var res incusResources
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("unreadable Incus resources: %w", err)
	}

	seen := map[string]bool{}
	var devices []Device
	for _, c := range res.GPU.Cards {
		description := strings.TrimSpace(c.Vendor + " " + c.Product)
		devices = append(devices, Device{
			ID: c.PCIAddress, Address: c.PCIAddress,
			Vendor: c.VendorID, Product: c.ProductID,
			Description: description, Driver: c.Driver, Class: GPU,
			BootDisplay: isBootDisplay(c.PCIAddress),
			Ready:       c.Driver == "vfio-pci",
		})
		seen[c.PCIAddress] = true
	}
	for _, d := range res.PCI.Devices {
		if seen[d.PCIAddress] {
			continue // already reported as a GPU, with better metadata
		}
		description := strings.TrimSpace(d.Vendor + " " + d.Product)
		devices = append(devices, Device{
			ID: d.PCIAddress, Address: d.PCIAddress,
			Vendor: d.VendorID, Product: d.ProductID,
			Description: description, Driver: d.Driver,
			Class:       classify(description),
			BootDisplay: isBootDisplay(d.PCIAddress),
			Ready:       d.Driver == "vfio-pci",
		})
	}
	sortDevices(devices)
	return devices, nil
}

func (Incus) Consequences(dev Device) []Consequence { return baseConsequences(dev) }

func (i Incus) Attach(ref types.InstanceRef, dev Device, name string) error {
	if err := checkSafe(dev); err != nil {
		return err
	}
	target := remoteOr(ref.Context) + ":" + ref.Name
	// Incus models a GPU and a bare PCI function as different device types,
	// with different key names for the same address.
	args := []string{"config", "device", "add", target, name, "pci", "address=" + dev.Address}
	if dev.Class == GPU {
		args = []string{"config", "device", "add", target, name, "gpu", "gputype=physical", "pci=" + dev.Address}
	}
	if out, err := runner.Run("incus", args...); err != nil {
		return fmt.Errorf("incus config device add: %s", commandError(out, err))
	}
	return nil
}

func (i Incus) Detach(ref types.InstanceRef, name string) error {
	target := remoteOr(ref.Context) + ":" + ref.Name
	out, err := runner.Run("incus", "config", "device", "remove", target, name)
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "doesn't exist") ||
			strings.Contains(strings.ToLower(string(out)), "not found") {
			return nil
		}
		return fmt.Errorf("incus config device remove: %s", commandError(out, err))
	}
	return nil
}

func (i Incus) Attached(ref types.InstanceRef) ([]Attachment, error) {
	target := remoteOr(ref.Context)
	out, err := runner.Run("incus", "query",
		fmt.Sprintf("%s:/1.0/instances/%s", target, ref.Name))
	if err != nil {
		return nil, fmt.Errorf("incus query instance: %s", commandError(out, err))
	}
	var inst struct {
		Devices map[string]map[string]string `json:"devices"`
	}
	if err := json.Unmarshal(out, &inst); err != nil {
		return nil, fmt.Errorf("unreadable instance config: %w", err)
	}
	var attachments []Attachment
	for name, cfg := range inst.Devices {
		switch cfg["type"] {
		case "gpu":
			attachments = append(attachments, Attachment{Name: name, Device: cfg["pci"]})
		case "pci":
			attachments = append(attachments, Attachment{Name: name, Device: cfg["address"]})
		}
	}
	return attachments, nil
}

// ── KubeVirt ──────────────────────────────────────────────────────
//
// The pre-existing model, behind the contract. KubeVirt is the odd one out:
// devices are allowlisted cluster-wide in the KubeVirt CR and then advertised
// by the device plugin as extended resources, so what a VM attaches is a
// resource name, not a PCI address. The adapter reports that honestly rather
// than inventing addresses it cannot know.

type KubeVirt struct{}

func (KubeVirt) List(context string) ([]Device, error) {
	out, err := runner.Run("kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %s", commandError(out, err))
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
	var devices []Device
	for _, node := range res.Items {
		for name, count := range node.Status.Allocatable {
			if !isDeviceResource(name) {
				continue
			}
			devices = append(devices, Device{
				ID:          name,
				Description: fmt.Sprintf("%s (%s available)", name, count),
				Class:       classify(name),
				Node:        node.Metadata.Name,
				// Allocatable means the device plugin has it bound and ready;
				// that is exactly what readiness means on this backend.
				Ready: count != "0",
			})
		}
	}
	sortDevices(devices)
	return devices, nil
}

// isDeviceResource reports whether a node resource is a passthrough device
// rather than CPU/memory or KubeVirt's own plumbing.
func isDeviceResource(name string) bool {
	if !strings.Contains(name, "/") {
		return false // cpu, memory, pods, ephemeral-storage, hugepages-*
	}
	if strings.HasPrefix(name, "devices.kubevirt.io/") {
		return false // kvm, tun, vhost-net
	}
	return true
}

// Consequences drops HostLosesDevice: on a cluster the device plugin already
// owns the device, so attaching consumes an allocatable unit rather than
// taking something away from a host that was using it.
func (KubeVirt) Consequences(dev Device) []Consequence {
	return []Consequence{RequiresRestart, BlocksMigration, GuestGetsDMA}
}

func (KubeVirt) Attach(ref types.InstanceRef, dev Device, name string) error {
	if err := checkSafe(dev); err != nil {
		return err
	}
	patch := fmt.Sprintf(
		`[{"op":"add","path":"/spec/template/spec/domain/devices/gpus/-","value":{"name":%q,"deviceName":%q}}]`,
		name, dev.ID)
	out, err := runner.Run("kubectl", "patch", "vm", ref.Name, "-n", ref.Namespace,
		"--type=json", "-p", patch)
	if err != nil {
		return fmt.Errorf("kubectl patch: %s", commandError(out, err))
	}
	return nil
}

func (k KubeVirt) Detach(ref types.InstanceRef, name string) error {
	attached, err := k.Attached(ref)
	if err != nil {
		return err
	}
	for i, a := range attached {
		if a.Name != name {
			continue
		}
		patch := fmt.Sprintf(`[{"op":"remove","path":"/spec/template/spec/domain/devices/gpus/%d"}]`, i)
		out, err := runner.Run("kubectl", "patch", "vm", ref.Name, "-n", ref.Namespace,
			"--type=json", "-p", patch)
		if err != nil {
			return fmt.Errorf("kubectl patch: %s", commandError(out, err))
		}
		return nil
	}
	return nil
}

func (KubeVirt) Attached(ref types.InstanceRef) ([]Attachment, error) {
	out, err := runner.Run("kubectl", "get", "vm", ref.Name, "-n", ref.Namespace,
		"-o", "jsonpath={.spec.template.spec.domain.devices.gpus}")
	if err != nil {
		return nil, fmt.Errorf("reading VM devices: %s", commandError(out, err))
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		return nil, nil
	}
	var gpus []struct {
		Name       string `json:"name"`
		DeviceName string `json:"deviceName"`
	}
	if err := json.Unmarshal([]byte(body), &gpus); err != nil {
		return nil, fmt.Errorf("unreadable device list: %w", err)
	}
	var attachments []Attachment
	for _, g := range gpus {
		attachments = append(attachments, Attachment{Name: g.Name, Device: g.DeviceName})
	}
	return attachments, nil
}

// ── host safety ───────────────────────────────────────────────────

// bootVGAPath is where Linux marks the device the firmware chose as the
// console. Overridable so the check is testable without a real sysfs.
var bootVGAPath = func(address string) string {
	return filepath.Join("/sys/bus/pci/devices", address, "boot_vga")
}

// isBootDisplay reports whether a PCI device is the host's boot display.
// Passing that through leaves the machine without a console — recoverable only
// by physical access — so it is the one thing this package refuses outright.
//
// A false negative is possible (a host with no sysfs, a non-Linux host), which
// is why it is a refusal on a positive rather than a permission on a negative:
// everything else still surfaces its consequences and asks the caller.
func isBootDisplay(address string) bool {
	if address == "" {
		return false
	}
	data, err := os.ReadFile(bootVGAPath(address))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
}
