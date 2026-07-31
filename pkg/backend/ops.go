package backend

// The operation contract.
//
// docs/backend-parity.md names this as the structural fix behind every other
// parity gap: `types.Backend` has nine methods, and everything richer —
// snapshots, migrate, scale, volumes, metrics, clone, template, export, events —
// was reached through `if backend == "kubevirt"` branches. There was no contract
// to fail to satisfy, so nothing failed, and the gaps were invisible.
//
// pkg/snapshot proved the alternative: one small interface per backend, and the
// registry made libvirt's absence obvious enough that somebody filled it. These
// are the same shape, one interface per operation family, deliberately small so
// a backend can implement Power without pretending to do Storage.
//
// Two rules make this more than documentation:
//
//  1. The adapters below are the only place a backend's own signature is
//     translated. A surface asks For(ref) for an adapter and type-asserts the
//     family it needs; it never switches on a backend name again.
//  2. Capabilities are *derived* from those assertions (see Derived), and the
//     conformance tests fail when the matrix, the derivation, and
//     types.CapabilitiesForBackend disagree. Adding a method to an adapter flips
//     the matrix; the flags follow or CI stops.

import (
	"fmt"

	"github.com/tuna-os/corral/pkg/types"
)

// ── the families ──────────────────────────────────────────────────

// Power is the lifecycle every backend has. A backend that cannot do this is
// not a backend.
type Power interface {
	Start(name string) error
	Stop(name string) error
	Delete(name string) error
}

// Restarter reboots a guest through the backend's own mechanism. Separate from
// Power on purpose: two backends only fake it by stopping and starting, which
// loses the guest's shutdown ordering, and a fake is exactly what the contract
// should not let a backend claim.
type Restarter interface {
	Restart(name string) error
}

// Suspender pauses a guest to memory. Distinct from Power because three of the
// five backends can do it and Corral only ever wired one.
type Suspender interface {
	Pause(name string) error
	Resume(name string) error
}

// Sizer changes CPU and memory. HotplugsLive reports whether the change applies
// without a restart, which the hardware form already promises and until now
// guessed at.
type Sizer interface {
	Scale(name string, cores int, mem string) error
	HotplugsLive(name string) bool
}

// Storer manages disks.
type Storer interface {
	AddDisk(name, size string) error
	RemoveDisk(name, disk string) error
	ExpandDisk(name, disk, size string) error
}

// Mover migrates a guest to another node. An empty target means "wherever the
// backend thinks it can go", and CanMigrate is the pre-flight — PVE answers it
// properly, KubeVirt approximates, and a backend with one host says no.
type Mover interface {
	Migrate(name, target string) error
	CanMigrate(name string) (bool, string)
}

// Cloner copies a guest.
type Cloner interface {
	Clone(source, target string) error
}

// Templater marks a guest as a golden template to clone from.
type Templater interface {
	MarkTemplate(name string, on bool) error
}

// Tagger attaches free-form labels.
type Tagger interface {
	SetTag(name, tag string, on bool) error
}

// Observer reports what a guest is doing: live usage and recent activity.
type Observer interface {
	Metrics(name string) (map[string]string, error)
	Events(name string) ([]Event, error)
}

// Event is one entry of a guest's recent activity, flattened from whatever the
// backend calls it — Kubernetes events, PVE tasks, journal lines.
type Event struct {
	Time    string
	Kind    string // "Normal" | "Warning"
	Reason  string
	Object  string
	Message string
}

// Exporter produces a downloadable copy of a guest's disk.
type Exporter interface {
	Export(name, destination string) (string, error)
}

// Addresser reports the guest's own network address, which is what SSH and the
// RDP probe need. It is not a Family: how a backend reaches a guest differs too
// much to gate one matrix row on (KubeVirt tunnels through virtctl and needs no
// address at all), so this is a capability callers ask for, not one the matrix
// derives from.
type Addresser interface {
	Address(name string) (string, error)
}

// ── the registry ──────────────────────────────────────────────────

// Adapter is what For returns: a backend bound to one instance's context, which
// then satisfies whichever families it implements. It is deliberately narrow —
// everything else is discovered by assertion.
type Adapter interface {
	// Backend names the backend, so an error message can say which one refused.
	Backend() string
}

// factories build an adapter per backend. Registering here is what makes a
// backend reachable through the contract; a backend absent from this map is
// reachable only through types.Backend's nine methods, which the matrix records.
var factories = map[string]func(types.InstanceRef) (Adapter, error){}

// Register adds a backend's adapter factory. Called from adapters.go rather than
// from the backend packages themselves, so pkg/backend keeps the only import
// edge and no backend has to know this package exists.
func Register(backend string, factory func(types.InstanceRef) (Adapter, error)) {
	factories[backend] = factory
}

// For returns the adapter for an instance. The error names what is missing
// rather than returning nil, because a nil adapter turns into a panic three
// frames later in whichever surface asked.
func For(ref types.InstanceRef) (Adapter, error) {
	if ref.Backend == "" {
		return nil, fmt.Errorf("instance reference has no backend")
	}
	factory, ok := factories[ref.Backend]
	if !ok {
		return nil, fmt.Errorf("backend %q has no adapter (have: %s); see docs/backend-parity.md",
			ref.Backend, backendNames())
	}
	return factory(ref)
}

// Registered reports whether a backend has an adapter at all.
func Registered(backend string) bool {
	_, ok := factories[backend]
	return ok
}

// ── derivation ────────────────────────────────────────────────────

// Family is one operation family, paired with the assertion that detects it.
// The matrix operation IDs it covers are listed so a conformance test can tie
// "implements Sizer" to "the matrix says scale is shipped".
type Family struct {
	Name string
	// Operations are the matrix operation IDs this family provides.
	Operations []string
	// Implements reports whether an adapter satisfies the family.
	Implements func(Adapter) bool
}

// Families is every family, in the order docs/backend-parity.md lists them.
var Families = []Family{
	{"Power", []string{"start", "stop", "delete"}, func(a Adapter) bool { _, ok := a.(Power); return ok }},
	{"Restarter", []string{"restart"}, func(a Adapter) bool { _, ok := a.(Restarter); return ok }},
	{"Suspender", []string{"pause"}, func(a Adapter) bool { _, ok := a.(Suspender); return ok }},
	{"Sizer", []string{"scale"}, func(a Adapter) bool { _, ok := a.(Sizer); return ok }},
	{"Storer", []string{"volumes", "expand"}, func(a Adapter) bool { _, ok := a.(Storer); return ok }},
	{"Mover", []string{"migrate"}, func(a Adapter) bool { _, ok := a.(Mover); return ok }},
	{"Cloner", []string{"clone"}, func(a Adapter) bool { _, ok := a.(Cloner); return ok }},
	{"Templater", []string{"template"}, func(a Adapter) bool { _, ok := a.(Templater); return ok }},
	{"Tagger", []string{"tags"}, func(a Adapter) bool { _, ok := a.(Tagger); return ok }},
	{"Observer", []string{"metrics", "events"}, func(a Adapter) bool { _, ok := a.(Observer); return ok }},
	{"Exporter", []string{"export"}, func(a Adapter) bool { _, ok := a.(Exporter); return ok }},
}

// Implemented returns the families a backend's adapter satisfies. It builds a
// probe adapter with an empty reference: the families are a property of the
// type, not of a live connection, so nothing here talks to a cluster.
func Implemented(backend string) []string {
	adapter, err := probe(backend)
	if err != nil {
		return nil
	}
	var out []string
	for _, family := range Families {
		if family.Implements(adapter) {
			out = append(out, family.Name)
		}
	}
	return out
}

// Provides reports whether a backend's adapter implements the family covering
// an operation.
func Provides(backend, operation string) bool {
	adapter, err := probe(backend)
	if err != nil {
		return false
	}
	for _, family := range Families {
		if !contains(family.Operations, operation) {
			continue
		}
		if family.Implements(adapter) {
			return true
		}
	}
	return false
}

// probe builds an adapter for capability derivation only. Adapters must be
// constructible from a bare reference for this to work, which is a constraint
// worth keeping: an adapter that needs a live connection to exist cannot be
// asked what it supports.
func probe(backend string) (Adapter, error) {
	return For(types.InstanceRef{Backend: backend, Name: "probe"})
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
