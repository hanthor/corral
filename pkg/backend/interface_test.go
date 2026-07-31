package backend

// The backends reachable through types.Backend, the interface the CLI
// dispatches through. Keeping this list in the test package means adding a
// backend that satisfies the interface is a one-line change here, and a backend
// that stops satisfying it fails to compile with a parity-shaped message.

import (
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/types"
)

func interfaceImplementations() map[string]types.Backend {
	return map[string]types.Backend{
		"qemu":  qemu.Backend{},
		"incus": incus.Backend{},
	}
}
