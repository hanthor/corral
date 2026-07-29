package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tuna-os/corral/pkg/config"
	"github.com/tuna-os/corral/pkg/incus"
	"github.com/tuna-os/corral/pkg/libvirt"
	"github.com/tuna-os/corral/pkg/types"
)

func createBackendVM(w http.ResponseWriter, req createRequest, target config.ContextConfig) {
	if req.Windows || req.Bootc != "" {
		errResp(w, http.StatusBadRequest, fmt.Errorf("%s does not support the selected workflow on backend %s", target.Name, target.Backend))
		return
	}
	done := taskBegin("create", target.Name+"/"+req.Name)
	var err error
	switch target.Backend {
	case "incus":
		image := req.Image
		if image == "" {
			image = req.ContainerDisk
		}
		if image == "" {
			image = req.Import
		}
		if req.ISO != "" || req.PVC != "" {
			err = fmt.Errorf("Incus creation currently accepts an Incus image alias, not ISO or PVC sources")
		} else {
			err = incus.NewClient(target.Context).Create(incus.CreateOpts{Name: req.Name, Image: image, VM: true, CPU: req.CPU, Memory: req.Mem})
		}
	case "libvirt":
		source, qcow := req.ISO, false
		if source == "" {
			source, qcow = req.Import, true
		}
		if source == "" {
			err = fmt.Errorf("libvirt creation needs an installer ISO or qcow2 source")
			break
		}
		if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
			source, err = downloadToCache(source)
		}
		if err == nil {
			opts := types.CreateOpts{Name: req.Name, CPU: req.CPU, Mem: req.Mem, Disk: req.Disk}
			if qcow {
				opts.QCOW = source
			} else {
				opts.ISO = source
			}
			err = libvirt.NewClient(target.Context).Create(opts)
		}
	default:
		err = fmt.Errorf("unsupported create backend %q", target.Backend)
	}
	done(err)
	if err != nil {
		errResp(w, http.StatusBadRequest, err)
		return
	}
	jsonResp(w, http.StatusCreated, map[string]string{"name": req.Name, "backend": target.Backend, "context": target.Name})
}
