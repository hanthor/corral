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

	"github.com/tuna-os/corral/pkg/backend"
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
//
// The per-backend switch this package was created to hold now lives in
// pkg/backend's operation contract, which every backend registers with — so
// this dispatches through Power rather than keeping a second copy that could
// drift. A backend gained by that contract (Proxmox) is a backend the scheduler
// can power without a change here, which is the point of the contract.
func Do(ref types.InstanceRef, action Action) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	switch action {
	case Start, Stop:
	default:
		return fmt.Errorf("unknown lifecycle action %q", action)
	}
	if ref.Backend == "qemu" && ref.Context != "" {
		// Worth catching here rather than letting the adapter ignore it: a
		// scheduler pointed at "qemu in context prod" is misconfigured, and
		// silently powering the local VM of that name is the wrong recovery.
		return fmt.Errorf("the local qemu backend has no contexts, got %q", ref.Context)
	}

	adapter, err := backend.For(ref)
	if err != nil {
		return err
	}
	power, ok := adapter.(backend.Power)
	if !ok {
		return fmt.Errorf("the %s backend cannot be powered on and off", ref.Backend)
	}
	if action == Start {
		return power.Start(ref.Name)
	}
	return power.Stop(ref.Name)
}

// Supported reports whether an instance's backend can be powered on and off
// through Do — asked of the contract itself, so it cannot claim more than Do
// can deliver.
func Supported(name string) bool {
	return backend.Provides(name, "start") && backend.Provides(name, "stop")
}
