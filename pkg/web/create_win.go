package web

import (
	"fmt"

	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/types"
)

// createWindows runs the Windows guided-install flow synchronously:
// UEFI+TPM+Hyper-V tuned VM with the installer ISO + virtio-win drivers.
// Setup runs unattended (injected autounattend.xml, see
// pkg/kubevirt/autounattend.go) — no interactive install clicks needed;
// the generated Administrator password is saved to the registry the same
// way cloud-init VM passwords already are.
func createWindows(req createRequest, ns string) error {
	return createWindowsInContext(req, ns, "")
}
func createWindowsInContext(req createRequest, ns, context string) error {
	if req.ISO == "" {
		return badRequest(fmt.Errorf("a Windows installer ISO URL is required"))
	}
	done := taskBegin("create windows", ns+"/"+req.Name)
	password, err := kubevirt.CreateWindowsVMInContext(req.Name, ns, req.ISO, req.Disk, req.Mem, req.CPU, true, context)
	if err != nil {
		done(err)
		return err
	}
	done(nil)
	if store != nil {
		ref := types.InstanceRef{Backend: "kubevirt", Context: context, Namespace: ns, Name: req.Name}
		store.SetRef(ref, types.RegistryEntry{Backend: "kubevirt", Context: context, Namespace: ns, Username: "Administrator", Password: password})
	}
	return nil
}
