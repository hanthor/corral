package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderWebUnit_User(t *testing.T) {
	unit := renderWebUnit(`/usr/bin/corral web --addr 127.0.0.1:8006`, false)
	for _, want := range []string{
		"Description=Corral web UI",
		"ExecStart=/usr/bin/corral web --addr 127.0.0.1:8006",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("user unit missing %q in:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Error("user unit should target default.target, not multi-user.target")
	}
}

func TestRenderWebUnit_System(t *testing.T) {
	t.Setenv("SUDO_USER", "alice")
	unit := renderWebUnit(`/usr/bin/corral web --addr 0.0.0.0:8006`, true)
	if !strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Errorf("system unit should target multi-user.target:\n%s", unit)
	}
	if !strings.Contains(unit, "User=alice") {
		t.Errorf("system unit should pin User to the installing (SUDO_USER) account:\n%s", unit)
	}
}

func TestSystemdQuote(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8006":        "127.0.0.1:8006",
		"/usr/bin/corral":       "/usr/bin/corral",
		"--accent=#3b82f6":      "--accent=#3b82f6",
		"/opt/my corral/corral": `"/opt/my corral/corral"`,
		`say "hi"`:              `"say \"hi\""`,
	}
	for in, want := range cases {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebUnitPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/home/bob/.config")
	got, err := webUnitPath(false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/home/bob/.config", "systemd", "user", webServiceUnit); got != want {
		t.Errorf("user unit path = %q, want %q", got, want)
	}

	sys, err := webUnitPath(true)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/etc/systemd/system", webServiceUnit); sys != want {
		t.Errorf("system unit path = %q, want %q", sys, want)
	}
}
