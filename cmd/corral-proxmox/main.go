// corral-proxmox is the Proxmox-API-compatibility Corral plugin: it serves the
// subset of the Proxmox VE REST API (/api2/json/…) that common ecosystem tools
// touch — nodes, VM list/status/config, start/stop/shutdown/reset, minimal
// create/delete — translated onto KubeVirt via the corral backend packages.
//
// Auth: set a shared secret with --token or CORRAL_PROXMOX_TOKEN to require
// Proxmox API-token headers / ticket cookies. With no secret configured the
// API is open — acceptable only when tailnet membership gates the listener.
// Do not expose this off the tailnet.
//
// Installed via the marketplace (`corral plugin install proxmox`) and run as
// `corral proxmox serve [--addr :8006]`.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/plugin/sdk"
	"github.com/tuna-os/corral/pkg/proxmox"
)

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

func main() {
	if sdk.HandleMetadata(sdk.Metadata{Name: "proxmox", Version: "0.1.0", Description: "Proxmox VE API compatibility server", Capabilities: []string{"cli-command", "http-server"}, Permissions: []string{"listen on network", "mutate KubeVirt VMs"}, SupportedBackends: []string{"kubevirt"}}) {
		return
	}
	var addr, namespace, certFile, keyFile, token string

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the Proxmox-compatible API (/api2/json) backed by KubeVirt",
		RunE: func(cmd *cobra.Command, args []string) error {
			tls := certFile != "" && keyFile != ""
			proto := "http"
			if tls {
				proto = "https"
			}
			fmt.Fprintf(os.Stderr, "corral-proxmox: serving %s://%s/api2/json (namespace %s)\n", proto, displayAddr(addr), namespace)

			srv := proxmox.NewServer(namespace).WithToken(token)
			if token == "" && os.Getenv("CORRAL_PROXMOX_TOKEN") == "" {
				fmt.Fprintln(os.Stderr, "no API token set (--token / CORRAL_PROXMOX_TOKEN) — auth is tailnet-membership only; do not expose this off the tailnet")
			}
			handler := srv.Mux()

			if tls {
				return http.ListenAndServeTLS(addr, certFile, keyFile, handler)
			}
			return http.ListenAndServe(addr, handler)
		},
	}
	serve.Flags().StringVar(&addr, "addr", ":8006", "Listen address")
	serve.Flags().StringVarP(&namespace, "namespace", "n", kubevirt.DefaultNamespace, "KubeVirt namespace to expose")
	serve.Flags().StringVar(&certFile, "cert", "", "TLS certificate file (enables HTTPS)")
	serve.Flags().StringVar(&keyFile, "key", "", "TLS key file (enables HTTPS)")
	serve.Flags().StringVar(&token, "token", "", "Shared API secret for PVEAPIToken/ticket auth (default: CORRAL_PROXMOX_TOKEN)")

	root := &cobra.Command{
		Use:   "corral-proxmox",
		Short: "Corral plugin — Proxmox VE API compatibility layer for KubeVirt",
	}
	root.AddCommand(serve)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
