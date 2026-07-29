package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/plugin"
)

// version is the release version, stamped at build time via
//
//	go build -ldflags "-X github.com/tuna-os/corral/cmd.version=v1.2.3"
//
// When unset (a plain `go build`), it falls back to the module version or the
// VCS revision recorded by the Go toolchain, so a locally built binary still
// reports something you can compare against the source you built it from.
var version = ""

// buildInfo returns (version, commit, buildTime), filling gaps from the build
// info the Go toolchain embeds. commit/buildTime come from the VCS stamps Go
// adds automatically for `go build`/`go install` in a git checkout.
func buildInfo() (ver, commit, buildTime string) {
	ver = version
	info, ok := debug.ReadBuildInfo()
	if !ok {
		if ver == "" {
			ver = "unknown"
		}
		return ver, "unknown", "unknown"
	}
	// Module version: "(devel)" for a local build, a real tag for `go install
	// module@version`. Prefer an explicit ldflags version over it.
	if ver == "" && info.Main.Version != "" {
		ver = info.Main.Version
	}
	commit, buildTime = "unknown", "unknown"
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.time":
			buildTime = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				commit += "-dirty"
			}
		}
	}
	if ver == "" {
		if commit != "unknown" {
			ver = "git-" + shortSHA(commit)
		} else {
			ver = "unknown"
		}
	}
	return ver, commit, buildTime
}

func shortSHA(sha string) string {
	sha = strings.TrimSuffix(sha, "-dirty")
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// versionString is the one-line form used by both `corral version` and the
// root command's --version flag.
func versionString() string {
	ver, commit, buildTime := buildInfo()
	return fmt.Sprintf("corral %s (commit %s, built %s, %s)",
		ver, shortSHA(commit), buildTime, runtime.Version())
}

var versionCmd = &cobra.Command{
	Use:     "version",
	Short:   "Print the corral version, git commit, and build info",
	Example: "  corral version",
	Args:    cobra.NoArgs,
	// Skip the persistent registry init — version must work even when the
	// registry/config is unavailable.
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), versionString())
	},
}

func init() {
	plugin.CurrentVersion, _, _ = buildInfo()
	rootCmd.AddCommand(versionCmd)
	// `corral --version` / `corral -v` as well, so the common reflexes work.
	rootCmd.Version = versionString()
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.Flags().BoolP("version", "v", false, "Print version information")
}
