// Package lifecycle dispatches start/stop on a canonical instance reference.
//
// Every surface that needs to power an instance on or off — the scheduler, the
// web handlers, the TUI — otherwise has to switch on the backend itself and
// construct the right client with the right context. That switch was copied
// into each of them, and a scheduler that guesses wrong doesn't fail loudly:
// it stops nothing, or stops something else with the same name in another
// context.
package lifecycle

import (
	"fmt"

	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/libvirt"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/types"
)

// Action is a power operation. Start and Stop are the two every backend
// implements; anything narrower belongs on the concrete client.
type Action string

const (
	Start Action = "start"
	Stop  Action = "stop"
)

// Do performs the action on the referenced instance. The reference carries the
// context, so an instance name that exists in several contexts is never
// ambiguous.
func Do(ref types.InstanceRef, action Action) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	switch action {
	case Start, Stop:
	default:
		return fmt.Errorf("unknown lifecycle action %q", action)
	}

	switch ref.Backend {
	case "kubevirt":
		c := kubevirt.NewClientForContext(ref.Namespace, ref.Context)
		if action == Start {
			return c.StartVM(ref.Name)
		}
		return c.StopVM(ref.Name)

	case "qemu":
		if ref.Context != "" {
			return fmt.Errorf("the local qemu backend has no contexts, got %q", ref.Context)
		}
		if action == Start {
			return qemu.Start(ref.Name)
		}
		return qemu.Stop(ref.Name)

	case "incus":
		remote := ref.Context
		if remote == "" {
			remote = "local"
		}
		c := incus.NewClient(remote)
		if action == Start {
			return c.Start(ref.Name)
		}
		return c.Stop(ref.Name)

	case "libvirt":
		c := libvirt.NewClient(ref.Context)
		if action == Start {
			return c.Start(ref.Name)
		}
		return c.Stop(ref.Name)

	default:
		return fmt.Errorf("no lifecycle adapter for backend %q", ref.Backend)
	}
}

// Supported reports whether an instance's backend can be powered on and off
// through Do. It reads the capability table rather than duplicating the switch
// above, so the two cannot drift.
func Supported(backend string) bool {
	caps := types.CapabilitiesForBackend(backend)
	return caps.Start && caps.Stop
}
