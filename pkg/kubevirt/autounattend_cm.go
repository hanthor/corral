package kubevirt

// KubeVirt's packaging of the Windows answer file. The answer file itself is
// backend-neutral and lives in pkg/windows (#132); what is KubeVirt-specific
// is how it reaches the guest — a ConfigMap presented as a `cdrom` volume,
// which KubeVirt packages into a real ISO9660 image. Other backends attach an
// ISO built on the host instead.

// AutounattendConfigMapName is the ConfigMap (and CD-ROM volume) name for a
// given Windows VM's answer file.
func AutounattendConfigMapName(vmName string) string { return vmName + "-autounattend" }

// GenerateAutounattendConfigMap wraps xml as the ConfigMap KubeVirt will
// package into an ISO9660 CD-ROM. The key name matters: Windows Setup looks
// for a file literally named "autounattend.xml" at the media root.
func GenerateAutounattendConfigMap(vmName, ns, xml string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      AutounattendConfigMapName(vmName),
			"namespace": ns,
		},
		"data": map[string]any{
			"autounattend.xml": xml,
		},
	}
}
