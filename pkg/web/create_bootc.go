package web

import (
	"fmt"
	"time"

	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/types"
)

// createBootc starts an on-cluster bootc disk build as a background task —
// the build takes minutes, so it doesn't block the HTTP response. Validation
// failures (bootc unavailable, no SSH key) return immediately as a
// *httpError; once the build is actually running, createBootc returns the
// task id and handle without waiting for it. Callers that need to wait for
// completion (tests, in particular — see buildTask.wait) can do so on the
// returned task instead of racing the goroutine through HTTP.
func createBootc(req createRequest, ns string) (id string, task *buildTask, err error) {
	return createBootcInContext(req, ns, "")
}

func createBootcInContext(req createRequest, ns, context string) (id string, task *buildTask, err error) {
	if !kubevirt.BootcAvailable() {
		return "", nil, badRequest(fmt.Errorf(
			"bootc support is not enabled on this server (optional plugin — run the corral:bootc image)"))
	}
	image := catalog.ResolveBootc(req.Bootc)

	sshKey, err := resolveSSHKey(req.SSHKey)
	if err != nil {
		return "", nil, badRequest(err)
	}
	if sshKey == "" {
		sshKey = kubevirt.LoadSSHPublicKey()
	}
	if sshKey == "" {
		return "", nil, badRequest(fmt.Errorf("sshKey is required for bootc VMs (no key on the server)"))
	}

	id = fmt.Sprintf("bootc-%s-%d", req.Name, time.Now().UnixNano())
	task = newBuildTask()
	tasks.Store(id, task)
	done := taskBegin("bootc build", ns+"/"+req.Name)

	go func() {
		err := kubevirt.WithContext(context, func() error {
			build, buildErr := kubevirt.BootcBuildDisk(req.Name, ns, image, sshKey, req.Disk, req.StorageClass, "", req.Node, task)
			if buildErr != nil {
				return buildErr
			}
			vm := kubevirt.GenerateBootcVM(req.Name, ns, build.PVCName, image, sshKey, req.Mem, req.CPU, req.Node)
			return kubevirt.Apply(vm)
		})
		if err == nil && store != nil {
			ref := types.InstanceRef{Backend: "kubevirt", Context: context, Namespace: ns, Name: req.Name}
			store.SetRef(ref, types.RegistryEntry{
				Backend:   "kubevirt",
				Context:   context,
				Namespace: ns,
			})
		}
		// The build is done once the VM exists — finish the task on that.
		task.finish(err)
		done(err)
		// Expose SSH/VNC/RDP on the tailnet via the Tailscale operator proxy
		// (bootc guests have no in-guest tailscale, only baked sshd on :22).
		// Best-effort and separate: a flaky proxy apply must not mark a
		// successful build as failed — the VM is already up and reachable
		// via virtctl port-forward.
		if err == nil {
			dp := taskBegin("tailnet expose", ns+"/"+req.Name)
			dp(kubevirt.WithContext(context, func() error { return kubevirt.ApplyProxy(req.Name, ns, kubevirt.ConsolePorts) }))
		}
	}()
	return id, task, nil
}
