package web

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives the package a private HOME and a stub kubeconfig.
//
// HOME, because config.ConfigDir resolves under it: without this the suite
// reads — and, through handlers that persist settings, writes — the developer's
// real ~/.config/corral. That made results order-dependent, since a context or
// backend default written by one test changed what config.Contexts returned for
// every later one. It also isolates qemu.VMHome, so local-backend listings see
// an empty machine instead of the developer's actual VMs.
//
// KUBECONFIG, because config.Contexts only offers the legacy kubevirt context
// when the host has a kubeconfig. Without one the cluster paths would be green
// on a dev box and red on a bare CI runner. Individual tests reassign HOME to
// their own t.TempDir for SSH-key fixtures, and KUBECONFIG survives that.
//
// Nothing here talks to a real cluster: every kubectl call goes through the
// fake runner wired up by NewTestFixture.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "corral-web-home-*")
	if err != nil {
		panic(err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	const stub = "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"
	if err := os.WriteFile(kubeconfig, []byte(stub), 0o600); err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("KUBECONFIG", kubeconfig)

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
