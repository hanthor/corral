//go:build bootc

package main

// Tests for resolveContext, the pure context-selection logic of the bootc
// plugin (was 0%): default context when none given, named-context lookup,
// and unknown-context error. Config is driven through env vars so no real
// config file is needed.

import (
	"os"
	"testing"

	"github.com/tuna-os/corral/pkg/config"
)

func TestResolveContext_Default(t *testing.T) {
	// No name → DefaultContext(). With an empty config the default falls back
	// to the "local"/qemu context.
	ctx, err := resolveContext("")
	if err != nil {
		t.Fatalf("resolveContext(\"\"): %v", err)
	}
	if ctx.Name == "" {
		t.Error("resolveContext(\"\") returned an empty context")
	}
}

func TestResolveContext_NamedContext(t *testing.T) {
	// A named context must round-trip through FindContext.
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Write a config with one explicit context at the default path.
	cfgDir := home + "/.config/corral"
	os.MkdirAll(cfgDir, 0o755)
	cfgPath := cfgDir + "/config.yaml"
	os.WriteFile(cfgPath, []byte(`default:
  context: prod
contexts:
  - name: prod
    backend: incus
`), 0o644)

	ctx, err := resolveContext("prod")
	if err != nil {
		t.Fatalf("resolveContext(\"prod\"): %v", err)
	}
	if ctx.Name != "prod" {
		t.Errorf("resolveContext(\"prod\") = %q, want prod", ctx.Name)
	}
	if ctx.Backend != "incus" {
		t.Errorf("resolveContext(\"prod\") backend = %q, want incus", ctx.Backend)
	}
}

func TestResolveContext_UnknownContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := resolveContext("no-such-context")
	if err == nil {
		t.Fatal("resolveContext(\"no-such-context\"): expected error")
	}
	if !contains(err.Error(), "unknown context") {
		t.Errorf("error = %v, want 'unknown context'", err)
	}
}

func TestResolveContext_DefaultEqualsDefaultContext(t *testing.T) {
	// resolveContext("") must agree with config.DefaultContext().
	got, err := resolveContext("")
	if err != nil {
		t.Fatalf("resolveContext(\"\"): %v", err)
	}
	want := config.DefaultContext()
	if got.Name != want.Name || got.Backend != want.Backend {
		t.Errorf("resolveContext(\"\") = %+v, DefaultContext() = %+v", got, want)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(s != "" && (len(s) >= len(sub)) && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
