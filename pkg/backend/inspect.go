package backend

// Inspect: the two facts about a guest that no listing carries and a move
// cannot be safe without (ADR-0010).
//
// A guest installed under UEFI has its bootloader in an ESP and nothing in the
// MBR. Move it to a BIOS-default target and it boots to a blank screen — no
// error, no log line, just a VM that is running and does nothing. The preflight
// is supposed to refuse that, and it can only refuse what it knows, so
// somebody has to ask the source.
//
// Guest OS type is the same shape of problem one step milder: a Windows guest
// imported onto virtio-scsi without virtio drivers already installed bluescreens
// on boot. That is a warning rather than a refusal — the drivers may well be
// there — but it has to be said, and saying it needs the fact.
//
// Neither belongs in types.VM. A listing runs against every context on every
// poll, and these need a per-instance config read; paying for that on the
// dashboard's 5-second refresh to serve an operation nobody has started yet
// would be a poor trade. So it is its own family, asked once, at preflight.

import (
	"encoding/json"
	"strings"

	"github.com/tuna-os/corral/pkg/types"
)

// GuestInfo is what an inspection found. A backend that cannot determine a
// field leaves it zero, and the caller treats that as unknown rather than as
// "no" — an unknown OS produces a warning, not an assertion.
type GuestInfo struct {
	// UEFI reports that the guest boots via EFI firmware.
	UEFI bool
	// OSType is the guest family the backend recorded: "linux", "windows", or
	// "" when it records nothing.
	OSType string
}

// Inspector reports a guest's firmware and OS family.
type Inspector interface {
	GuestInfo(name string) (GuestInfo, error)
}

func init() {
	Families = append(Families, Family{
		Name:       "Inspector",
		Operations: nil, // not a per-instance operation; no matrix row
		Implements: func(a Adapter) bool { _, ok := a.(Inspector); return ok },
	})
}

// Inspect asks a reference's backend about the guest, and returns a zero
// GuestInfo when the backend cannot say.
//
// It never returns an error for "this backend does not implement inspection":
// the caller's next move is the same either way — treat both fields as unknown
// — and making that an error would push every call site into handling a case
// that is not a failure.
func Inspect(ref types.InstanceRef) GuestInfo {
	adapter, err := For(ref)
	if err != nil {
		return GuestInfo{}
	}
	inspector, ok := adapter.(Inspector)
	if !ok {
		return GuestInfo{}
	}
	info, err := inspector.GuestInfo(ref.Name)
	if err != nil {
		// A config read that failed is not a claim that the guest is BIOS. The
		// zero value means unknown, and the preflight warns rather than asserts.
		return GuestInfo{}
	}
	return info
}

// ── kubevirt ──────────────────────────────────────────────────────

func (a kubevirtAdapter) GuestInfo(name string) (GuestInfo, error) {
	out, err := a.client.VMInfo(name)
	if err != nil {
		return GuestInfo{}, err
	}
	var manifest struct {
		Spec struct {
			Template struct {
				Spec struct {
					Domain struct {
						Firmware struct {
							Bootloader struct {
								EFI *json.RawMessage `json:"efi"`
							} `json:"bootloader"`
						} `json:"firmware"`
					} `json:"domain"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out, &manifest); err != nil {
		return GuestInfo{}, err
	}
	info := GuestInfo{
		UEFI: manifest.Spec.Template.Spec.Domain.Firmware.Bootloader.EFI != nil,
	}
	// KubeVirt has no OS-type field; the preference name is the closest thing a
	// cluster records, and Corral's Windows flow sets a windows preference.
	for key, value := range manifest.Metadata.Labels {
		if strings.Contains(strings.ToLower(key+value), "windows") {
			info.OSType = "windows"
		}
	}
	return info, nil
}

// ── libvirt ───────────────────────────────────────────────────────

func (a libvirtAdapter) GuestInfo(name string) (GuestInfo, error) {
	out, err := a.client.DumpXML(name)
	if err != nil {
		return GuestInfo{}, err
	}
	xml := string(out)
	// Both spellings mean EFI: the modern `<os firmware='efi'>` attribute and
	// the older explicit `<loader …>OVMF_CODE.fd</loader>`.
	info := GuestInfo{
		UEFI: strings.Contains(xml, `firmware='efi'`) ||
			strings.Contains(xml, `firmware="efi"`) ||
			strings.Contains(strings.ToUpper(xml), "OVMF"),
	}
	if strings.Contains(strings.ToLower(xml), "<os") && strings.Contains(strings.ToLower(xml), "windows") {
		info.OSType = "windows"
	}
	return info, nil
}

// ── proxmox ───────────────────────────────────────────────────────

func (a proxmoxAdapter) GuestInfo(name string) (GuestInfo, error) {
	client, err := a.client()
	if err != nil {
		return GuestInfo{}, err
	}
	cfg, err := client.GuestConfig(name)
	if err != nil {
		return GuestInfo{}, err
	}
	bios, _ := cfg.Raw["bios"].(string)
	info := GuestInfo{UEFI: bios == "ovmf"}
	// PVE's ostype is granular ("win11", "l26", "other"); the move only needs
	// the family, and reporting "win11" would make a caller string-match on
	// versions.
	switch {
	case strings.HasPrefix(cfg.OSType, "w"):
		info.OSType = "windows"
	case strings.HasPrefix(cfg.OSType, "l"), cfg.OSType == "solaris":
		info.OSType = "linux"
	}
	return info, nil
}

// ── incus ─────────────────────────────────────────────────────────

// GuestInfo: an Incus VM is always UEFI. Incus boots its VMs under OVMF with
// no BIOS option at all, which makes this the one backend where the answer is
// a property of the backend rather than of the instance — and it is a
// load-bearing answer, because it means an Incus VM cannot move to a
// BIOS-only destination and the preflight now says so before the disk is
// copied rather than after.
func (a incusAdapter) GuestInfo(name string) (GuestInfo, error) {
	instances, err := a.client.ListInstances()
	if err != nil {
		return GuestInfo{}, err
	}
	for _, inst := range instances {
		if inst.Name != name {
			continue
		}
		if inst.IsContainer() {
			// A container has no firmware. Saying UEFI here would be nonsense,
			// and move refuses containers by name well before this anyway.
			return GuestInfo{}, nil
		}
		return GuestInfo{UEFI: true}, nil
	}
	return GuestInfo{}, nil
}
