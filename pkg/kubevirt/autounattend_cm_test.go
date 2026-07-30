package kubevirt

import "testing"

func TestGenerateAutounattendConfigMap_Shape(t *testing.T) {
	cm := GenerateAutounattendConfigMap("win11", "corral-vms", "<xml/>")
	if cm["kind"] != "ConfigMap" {
		t.Errorf("kind = %v, want ConfigMap", cm["kind"])
	}
	meta := cm["metadata"].(map[string]any)
	if meta["name"] != "win11-autounattend" || meta["namespace"] != "corral-vms" {
		t.Errorf("metadata = %v", meta)
	}
	data := cm["data"].(map[string]any)
	if data["autounattend.xml"] != "<xml/>" {
		t.Errorf("data[autounattend.xml] = %v", data["autounattend.xml"])
	}
}

func TestAutounattendConfigMapName(t *testing.T) {
	if got := AutounattendConfigMapName("win11"); got != "win11-autounattend" {
		t.Errorf("got %q, want win11-autounattend", got)
	}
}
