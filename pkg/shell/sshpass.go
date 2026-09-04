package shell

import (
	"fmt"
	"os"
	"os/exec"
)

// RunWithSSHPass runs bin (typically ssh or virtctl ssh) with args, piping
// password via sshpass for password-based auth. Streams are wired directly
// to the process's own stdio, unlike Run/RunStdin — this is for interactive
// commands, not ones whose output gets captured.
func RunWithSSHPass(password, bin string, args ...string) error {
	sshpass, err := exec.LookPath("sshpass")
	if err != nil {
		return fmt.Errorf("sshpass not found (needed for password auth) — install: brew install sshpass")
	}
	allArgs := append([]string{"-e", bin}, args...)
	cmd := exec.Command(sshpass, allArgs...)
	cmd.Env = append(os.Environ(), "SSHPASS="+password)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
