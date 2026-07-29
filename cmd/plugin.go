package cmd

import (
	"fmt"
	"os"

	semver "github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/plugin"
)

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Manage Corral extensions (plugins)",
	Long: `Browse, install, and remove Corral extensions.

Plugins are standalone executables named "corral-<name>" in
~/.local/share/corral/plugins. Once installed, run them as "corral <name> …".
The marketplace is a curated index of installable plugins.`,
	Example: `  corral plugin search
  corral plugin install bootc
  corral bootc create myvm --image bluefin
  corral plugin list`,
}

var pluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed plugins",
	RunE: func(cmd *cobra.Command, args []string) error {
		ps := plugin.Installed()
		if len(ps) == 0 {
			fmt.Println("No plugins installed. Browse: corral plugin search")
			return nil
		}
		for _, p := range ps {
			fmt.Printf("%-16s %s\n", p.Name, p.Path)
		}
		return nil
	},
}

var pluginSearchCmd = &cobra.Command{
	Use:   "search [term]",
	Short: "Search the marketplace",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		idx, errs := plugin.FetchAll()
		for _, err := range errs {
			fmt.Fprintln(os.Stderr, "warning:", err)
		}
		if len(idx.Plugins) == 0 && len(errs) > 0 {
			return fmt.Errorf("all marketplaces failed")
		}
		term := ""
		if len(args) == 1 {
			term = args[0]
		}
		fmt.Printf("%-14s %-12s %-9s %-12s %s\n", "NAME", "VERSION", "STATUS", "SOURCE", "DESCRIPTION")
		for _, e := range idx.Plugins {
			if term != "" && !contains(e.Name+" "+e.Description, term) {
				continue
			}
			status := "available"
			if plugin.IsInstalled(e.Name) {
				status = "installed"
			}
			fmt.Printf("%-14s %-12s %-9s %-12s %s\n", e.Name, e.Version, status, e.Source, e.Description)
		}
		return nil
	},
}

var pluginInstallCmd = &cobra.Command{
	Use:   "install <name>",
	Short: "Install a plugin from the marketplace",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		idx, errs := plugin.FetchAll()
		if len(idx.Plugins) == 0 && len(errs) > 0 {
			return errs[0]
		}
		e := idx.Find(args[0])
		if e == nil {
			return fmt.Errorf("no plugin %q in the marketplace (corral plugin search)", args[0])
		}
		pin, _ := cmd.Flags().GetBool("pin")
		accept, _ := cmd.Flags().GetBool("accept-permissions")
		if len(e.Permissions) > 0 && !accept {
			return fmt.Errorf("%s requests permissions %v; inspect with `corral plugin info %s`, then repeat with --accept-permissions", e.Name, e.Permissions, args[0])
		}
		if e.SchemaVersion != plugin.MarketplaceAPIV2 {
			fmt.Fprintln(os.Stderr, "warning: installing a legacy v1 entry; migrate this publisher to marketplace v2")
		}
		if err := e.InstallPinned(pin); err != nil {
			return err
		}
		fmt.Printf("Installed %s %s → run `corral %s`\n", e.Name, e.Version, e.Name)
		return nil
	},
}

var pluginInfoCmd = &cobra.Command{Use: "info <name>", Short: "Show marketplace and installed metadata", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
	idx, _ := plugin.FetchAll()
	e := idx.Find(args[0])
	state, _ := plugin.ReadState(args[0])
	if e == nil && state.Name == "" {
		return fmt.Errorf("plugin %q not found", args[0])
	}
	if e != nil {
		fmt.Printf("Name: %s\nVersion: %s\nSource: %s\nPublisher: %s\nLicense: %s\nRequires Corral: %s\nCapabilities: %v\nPermissions: %v\n", e.Name, e.Version, e.Source, e.Publisher.Name, e.License, e.Corral, e.Capabilities, e.Permissions)
	}
	if state.Name != "" {
		fmt.Printf("Installed: %s (pinned: %v, sha256: %s)\n", state.Version, state.Pinned, state.SHA256)
		if meta, err := plugin.Inspect(args[0]); err == nil {
			fmt.Printf("Runtime API: %s\nRuntime capabilities: %v\n", meta.API, meta.Capabilities)
		}
	}
	return nil
}}

var pluginUpdateCmd = &cobra.Command{Use: "update [name]", Short: "Update one plugin, or every unpinned plugin", Args: cobra.MaximumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
	idx, errs := plugin.FetchAll()
	if len(idx.Plugins) == 0 && len(errs) > 0 {
		return errs[0]
	}
	updated := 0
	for _, p := range plugin.Installed() {
		if len(args) == 1 && p.Name != args[0] {
			continue
		}
		state, err := plugin.ReadState(p.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s has no state manifest; reinstall it\n", p.Name)
			continue
		}
		if state.Pinned {
			fmt.Printf("%s pinned at %s\n", p.Name, state.Version)
			continue
		}
		key := p.Name
		if state.Source != "" {
			key = state.Source + "/" + p.Name
		}
		e := idx.Find(key)
		if e == nil {
			fmt.Fprintf(os.Stderr, "warning: %s not found in source %s\n", p.Name, state.Source)
			continue
		}
		old, _ := semver.NewVersion(state.Version)
		next, _ := semver.NewVersion(e.Version)
		if old != nil && next != nil && !next.GreaterThan(old) {
			continue
		}
		if err := e.InstallPinned(false); err != nil {
			return err
		}
		fmt.Printf("Updated %s %s → %s\n", p.Name, state.Version, e.Version)
		updated++
	}
	if updated == 0 {
		fmt.Println("All selected plugins are up to date.")
	}
	return nil
}}

func pinCmd(pinned bool) *cobra.Command {
	verb := "pin"
	if !pinned {
		verb = "unpin"
	}
	return &cobra.Command{Use: verb + " <name>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error { return plugin.SetPinned(args[0], pinned) }}
}

var marketplaceCmd = &cobra.Command{Use: "marketplace", Aliases: []string{"source", "sources"}, Short: "Manage plugin marketplace sources"}
var marketplaceListCmd = &cobra.Command{Use: "list", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {
	for _, s := range plugin.Sources() {
		fmt.Printf("%-16s %-7v %s\n", s.Name, s.Enabled, s.URL)
	}
}}
var marketplaceAddCmd = &cobra.Command{Use: "add <name> <https-url>", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
	return plugin.AddSource(plugin.Source{Name: args[0], URL: args[1], Enabled: true})
}}
var marketplaceRemoveCmd = &cobra.Command{Use: "remove <name>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error { return plugin.RemoveSource(args[0]) }}
var marketplaceValidateCmd = &cobra.Command{Use: "validate <index.json>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
	if err := plugin.ValidateIndexFile(args[0]); err != nil {
		return err
	}
	fmt.Println("marketplace index is valid")
	return nil
}}

func marketplaceToggle(enabled bool) *cobra.Command {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	return &cobra.Command{Use: verb + " <name>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error { return plugin.SetSourceEnabled(args[0], enabled) }}
}

var pluginRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"uninstall", "rm"},
	Short:   "Remove an installed plugin",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := plugin.Remove(args[0]); err != nil {
			return err
		}
		fmt.Printf("Removed %s\n", args[0])
		return nil
	},
}

func init() {
	rootCmd.AddCommand(pluginCmd)
	pluginInstallCmd.Flags().Bool("pin", false, "Pin this version and exclude it from updates")
	pluginInstallCmd.Flags().Bool("accept-permissions", false, "Accept the permissions declared by this plugin")
	marketplaceCmd.AddCommand(marketplaceListCmd, marketplaceAddCmd, marketplaceRemoveCmd, marketplaceValidateCmd, marketplaceToggle(true), marketplaceToggle(false))
	pluginCmd.AddCommand(pluginListCmd, pluginSearchCmd, pluginInstallCmd, pluginRemoveCmd, pluginInfoCmd, pluginUpdateCmd, pinCmd(true), pinCmd(false))
	rootCmd.AddCommand(marketplaceCmd)
}

func contains(haystack, needle string) bool {
	h, n := []rune(haystack), []rune(needle)
	if len(n) == 0 {
		return true
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		ok := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
