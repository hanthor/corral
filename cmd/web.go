package cmd

import (
	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/web"
)

var (
	webAddr          string
	webAccent        string
	webBrandTitle    string
	webBrandEmoji    string
	webBrandSubtitle string
	webCustomCSS     string
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Serve the Corral web UI (Proxmox-style dashboard)",
	Long: `Serve the Corral web UI: a Proxmox-style dashboard with in-browser
VNC and serial TTY consoles. Works on mobile.

Shows both backends: KubeVirt VMs and CTs from the cluster, plus this
host's local QEMU VMs under a "local" node (lifecycle, VNC console, info).

The web UI shares the registry and cluster state with the CLI and TUI,
so all three can be used in tandem.

By default it binds to 127.0.0.1:8006 (Proxmox's port). To reach it from
other tailnet devices, bind your Tailscale IP.

There is no authentication — never bind a public interface.

--demo serves a built-in fake cluster (VMs, CTs, nodes, live metrics) so
you can explore the dashboard — or develop on it — with no cluster at all.`,
	Example: `  corral web
  corral web --addr "$(tailscale ip -4):8006"
  corral web --demo   # explore the UI without a cluster
  corral web --accent "#3b82f6" --brand-emoji "⚡"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if rootDemo {
			web.EnableDemo()
		}
		// CLI flags override config file; the theme is assembled inside web.Serve.
		web.SetCLITheme(webAccent, webBrandTitle, webBrandEmoji, webBrandSubtitle, webCustomCSS)
		return web.Serve(webAddr)
	},
}

func init() {
	rootCmd.AddCommand(webCmd)
	webCmd.Flags().StringVar(&webAddr, "addr", "127.0.0.1:8006", "Listen address")
	webCmd.Flags().StringVar(&webAccent, "accent", "", "Accent colour (CSS hex, e.g. #3b82f6)")
	webCmd.Flags().StringVar(&webBrandTitle, "brand-title", "", "Brand title shown in the header")
	webCmd.Flags().StringVar(&webBrandEmoji, "brand-emoji", "", "Emoji shown left of the brand title")
	webCmd.Flags().StringVar(&webBrandSubtitle, "brand-subtitle", "", "Subtitle text next to the brand title")
	webCmd.Flags().StringVar(&webCustomCSS, "custom-css", "", "Path to a CSS file whose contents are injected into the page")
}
