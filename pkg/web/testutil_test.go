package web

import (
	"net/http/httptest"
	"os"

	"github.com/tuna-os/corral/pkg/ct"
	"github.com/tuna-os/corral/pkg/doctor"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/registry"
	"github.com/tuna-os/corral/pkg/shell"
	"github.com/tuna-os/corral/pkg/sources"
)

// TestFixture holds a test server and its fake runner for handler tests.
type TestFixture struct {
	Server *httptest.Server
	Runner *shell.Fake
	tmpDir string
}

// NewTestFixture creates a test server with a FakeRunner wired into the
// kubevirt client and the web package runner. Caller should defer
// fixture.Close().
func NewTestFixture() *TestFixture {
	runner := shell.NewFake()

	// Always succeed at looking up virtctl
	runner.AddResponseKV("virtctl", nil, "", nil)

	// Wire into the kubevirt package's apply runner (for CreateVM)
	kubevirt.SetApplyRunner(runner)

	// Wire into the kubevirt package's default client runner (for all NewClient calls)
	kubevirt.SetDefaultRunner(runner)

	// Wire into the kubevirt package-level runner (for EnsureNamespace, ListDataVolumes, etc.)
	kubevirt.SetPackageRunner(runner)

	// Wire into the web package's default runner (for vmiIndex, handleNodes, handleExport)
	defaultRunner = runner

	// Wire into the doctor package (for /api/doctor and /api/doctor/fix) —
	// without this, doctor handlers would shell out to the real kubectl.
	doctor.SetRunner(runner)
	sources.SetRunner(runner)
	ct.SetRunner(runner)
	incus.SetRunner(runner)

	// Create a temp registry store so create/delete handlers don't panic
	tmpDir, _ := os.MkdirTemp("", "corral-test-*")
	store = registry.NewStoreAt(tmpDir + "/registry.json")

	mux, err := newMux()
	if err != nil {
		panic(err)
	}

	return &TestFixture{
		Server: httptest.NewServer(mux),
		Runner: runner,
		tmpDir: tmpDir,
	}
}

// Close shuts down the test server and cleans up the temp registry.
func (f *TestFixture) Close() {
	f.Server.Close()
	f.Runner.Reset()
	if f.tmpDir != "" {
		os.RemoveAll(f.tmpDir)
	}
}

// Reset clears recorded calls and responses (between test cases).
// Re-wires the runner into the package-level vars that NewTestFixture set.
func (f *TestFixture) Reset() {
	f.Runner.Reset()
	f.Runner.AddResponseKV("virtctl", nil, "", nil)
	kubevirt.SetApplyRunner(f.Runner)
	kubevirt.SetDefaultRunner(f.Runner)
	kubevirt.SetPackageRunner(f.Runner)
	defaultRunner = f.Runner
	doctor.SetRunner(f.Runner)
	sources.SetRunner(f.Runner)
	ct.SetRunner(f.Runner)
	incus.SetRunner(f.Runner)
}
