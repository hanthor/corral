package web

import (
	"fmt"

	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/types"
)

// resolveGenericImage maps req.Image (a catalog name) onto the request
// fields the matching entry boots from — a containerdisk, a CDI import URL,
// or an installer ISO. No-op if req.Image is unset.
func resolveGenericImage(req *createRequest) error {
	if req.Image == "" {
		return nil
	}
	img := catalog.Find(req.Image)
	if img == nil {
		return badRequest(fmt.Errorf("unknown image %q", req.Image))
	}
	// Catalog entries boot three ways: containerdisks directly, official
	// cloud images via CDI import, installer ISOs via the ISO path.
	switch img.Kind() {
	case "containerDisk":
		req.ContainerDisk = img.ContainerDisk
	case "import":
		req.Import = img.URL
	case "iso":
		req.ISO = img.ISO
	}
	return nil
}

// createGeneric handles everything that isn't bootc or the Windows guided
// flow: catalog images, container disks, import URLs, ISO installs, and
// PVC-backed creation.
func createGeneric(req createRequest, ns string) error {
	return createGenericInContext(req, ns, "")
}
func createGenericInContext(req createRequest, ns, context string) error {
	if err := resolveGenericImage(&req); err != nil {
		return err
	}

	// The wizard's SSH key box takes a literal key or a GitHub username; it
	// overrides the server's own key when set.
	sshKey, err := resolveSSHKey(req.SSHKey)
	if err != nil {
		return badRequest(err)
	}
	if sshKey == "" {
		sshKey = kubevirt.LoadSSHPublicKey()
	}

	opts := types.CreateOpts{
		Name:           req.Name,
		Namespace:      ns,
		CPU:            req.CPU,
		Mem:            req.Mem,
		Disk:           req.Disk,
		ContainerDisk:  req.ContainerDisk,
		ImportURL:      req.Import,
		ISO:            req.ISO,
		PVC:            req.PVC,
		Node:           req.Node,
		CloudInitExtra: req.CloudInit,
		InstanceType:   req.InstanceType,
		Preference:     req.Preference,
		SSHPublicKey:   sshKey,
		StorageClass:   req.StorageClass,
	}
	done := taskBegin("create", ns+"/"+req.Name)
	password, err := kubevirt.CreateVMInContext(opts, context)
	if err != nil {
		done(err)
		return err
	}
	done(nil)
	if store != nil {
		ref := types.InstanceRef{Backend: "kubevirt", Context: context, Namespace: ns, Name: req.Name}
		store.SetRef(ref, types.RegistryEntry{Backend: "kubevirt", Context: context, Namespace: ns, Password: password})
	}
	// Tailnet-by-default: expose SSH/VNC/RDP on the tailnet via the Tailscale
	// operator proxy. Best-effort and async — ApplyProxy retries for up to a
	// minute on transient RBAC flakes, which must not block the create response.
	go func() {
		dp := taskBegin("tailnet expose", ns+"/"+req.Name)
		dp(kubevirt.WithContext(context, func() error { return kubevirt.ApplyProxy(req.Name, ns, kubevirt.ConsolePorts) }))
	}()
	return nil
}
