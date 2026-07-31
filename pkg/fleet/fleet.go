// Package fleet aggregates every configured Corral context into one inventory.
package fleet

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/libvirt"
	"github.com/tuna-os/corral/pkg/proxmoxbe"
	"github.com/tuna-os/corral/pkg/qemu"
	"github.com/tuna-os/corral/pkg/types"
)

type Result struct {
	VMs    []types.VM        `json:"vms"`
	Errors map[string]string `json:"errors,omitempty"`
}

// List queries all configured contexts concurrently. A failed remote is a
// visible partial error and never hides healthy local inventory.
func List(ctx context.Context) Result {
	contexts := config.Contexts()
	result := Result{Errors: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, target := range contexts {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				mu.Lock()
				result.Errors[target.Name] = ctx.Err().Error()
				mu.Unlock()
				return
			default:
			}
			var vms []types.VM
			var err error
			switch target.Backend {
			case "qemu":
				vms, err = qemu.List()
			case "kubevirt":
				vms, err = kubevirt.NewClientForContext("", target.Context).ListVMs()
			case "incus":
				vms, err = incus.NewClient(target.Context).List()
			case "libvirt":
				vms, err = libvirt.NewClient(target.Context).List()
			case "proxmox":
				var client *proxmoxbe.Client
				if client, err = proxmoxbe.ClientForContext(target.Context); err == nil {
					vms, err = client.List()
				}
			default:
				err = fmt.Errorf("unsupported backend %q", target.Backend)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Errors[target.Name] = err.Error()
				return
			}
			for i := range vms {
				vms[i].Backend = target.Backend
				vms[i].Context = target.Context
				if target.Backend == "qemu" {
					vms[i].Namespace, vms[i].Node = "local", "local"
				}
				vms[i].SetIdentity()
				if vms[i].IP != "" {
					if vms[i].Endpoints == nil {
						vms[i].Endpoints = map[string]string{}
					}
					if vms[i].Capabilities.SSH {
						vms[i].Endpoints["ssh"] = net.JoinHostPort(vms[i].IP, "22")
					}
					if vms[i].Capabilities.VNC {
						vms[i].Endpoints["vnc"] = net.JoinHostPort(vms[i].IP, "5900")
					}
					if vms[i].Capabilities.RDP {
						vms[i].Endpoints["rdp"] = net.JoinHostPort(vms[i].IP, "3389")
					}
				}
			}
			result.VMs = append(result.VMs, vms...)
		}()
	}
	wg.Wait()
	sort.Slice(result.VMs, func(i, j int) bool { return result.VMs[i].ID < result.VMs[j].ID })
	if len(result.Errors) == 0 {
		result.Errors = nil
	}
	return result
}

// Resolve accepts a canonical ID or an unqualified name. Bare names are only
// valid when unique across the complete inventory.
func Resolve(vms []types.VM, selector string) (types.VM, error) {
	var matches []types.VM
	for _, vm := range vms {
		if vm.ID == selector || vm.Name == selector {
			matches = append(matches, vm)
		}
	}
	if len(matches) == 0 {
		return types.VM{}, fmt.Errorf("instance %q does not exist", selector)
	}
	if len(matches) > 1 {
		return types.VM{}, fmt.Errorf("instance name %q is ambiguous; use one of the fully-scoped IDs shown by corral list", selector)
	}
	return matches[0], nil
}
