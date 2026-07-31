package proxmoxbe

// runInteractive hands the terminal to a command — ssh, in practice. It is the
// one place this package leaves the HTTP world, and it is deliberately tiny:
// there is no runner seam because there is nothing to fake, the process either
// replaces the terminal or it does not.

import (
	"os"
	"os/exec"
)

var runInteractive = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
