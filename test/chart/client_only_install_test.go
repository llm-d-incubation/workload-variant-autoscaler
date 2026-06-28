/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package chart_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const chartPath = "../../charts/workload-variant-autoscaler"

// helmTemplate runs "helm template" with the given set values and returns the rendered output.
func helmTemplate(t *testing.T, releaseName string, setValues map[string]string) string {
	t.Helper()

	args := make([]string, 0, 3+2*len(setValues))
	args = append(args, "template", releaseName, chartPath)
	for k, v := range setValues {
		args = append(args, "--set", k+"="+v)
	}

	cmd := exec.Command("helm", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}
	return string(out)
}

// TestClientOnlyInstall verifies that controller.enabled=false produces only
// workload-specific resources (VA, HPA, Service, ServiceMonitor, RBAC ClusterRoles)
// and excludes all controller infrastructure.
func TestClientOnlyInstall(t *testing.T) {
	output := helmTemplate(t, "wva-model-b", map[string]string{
		"controller.enabled":  "false",
		"va.enabled":          "true",
		"hpa.enabled":         "true",
		"llmd.namespace":      "team-b",
		"llmd.modelName":      "my-model",
		"llmd.modelID":        "meta-llama/Llama-3.1-8B",
		"vllmService.enabled": "true",
	})

	// Resources that MUST be present in client-only mode
	mustContain := []string{
		"kind: VariantAutoscaling",
		"kind: HorizontalPodAutoscaler",
		"kind: Service",
		"kind: ServiceMonitor",
	}
	for _, resource := range mustContain {
		if !strings.Contains(output, resource) {
			t.Errorf("client-only install should contain %q", resource)
		}
	}

	// Resources that MUST NOT be present (controller infrastructure).
	// Note: "kind: Deployment" appears inside scaleTargetRef blocks (VA, HPA),
	// so we check for controller-specific markers instead.
	mustNotContain := []struct {
		marker string
		reason string
	}{
		{"kind: ServiceAccount", "controller service account should be excluded"},
		{"leader-election", "leader election RBAC should be excluded"},
		{"controller-manager", "controller manager resources should be excluded"},
		{"prometheus-ca", "prometheus CA configmaps should be excluded"},
	}
	for _, check := range mustNotContain {
		if strings.Contains(output, check.marker) {
			t.Errorf("client-only install should NOT contain %q: %s", check.marker, check.reason)
		}
	}
}

// TestFullInstall verifies that controller.enabled=true (default) produces
// controller infrastructure in addition to workload resources.
func TestFullInstall(t *testing.T) {
	output := helmTemplate(t, "wva-full", map[string]string{
		"controller.enabled": "true",
		"va.enabled":         "true",
		"hpa.enabled":        "true",
	})

	mustContain := []string{
		"kind: Deployment",
		"kind: ServiceAccount",
		"kind: VariantAutoscaling",
		"kind: HorizontalPodAutoscaler",
		"leader-election",
		"controller-manager",
	}
	for _, resource := range mustContain {
		if !strings.Contains(output, resource) {
			t.Errorf("full install should contain %q", resource)
		}
	}
}

// TestClientOnlyNoVA verifies that controller.enabled=false with va.enabled=false
// and hpa.enabled=false produces minimal output (only service/servicemonitor/RBAC).
func TestClientOnlyNoVA(t *testing.T) {
	output := helmTemplate(t, "wva-minimal", map[string]string{
		"controller.enabled":  "false",
		"va.enabled":          "false",
		"hpa.enabled":         "false",
		"vllmService.enabled": "true",
	})

	if strings.Contains(output, "kind: VariantAutoscaling") {
		t.Error("should not contain VariantAutoscaling when va.enabled=false")
	}
	if strings.Contains(output, "kind: HorizontalPodAutoscaler") {
		t.Error("should not contain HPA when hpa.enabled=false")
	}
	if strings.Contains(output, "kind: Deployment") {
		t.Error("should not contain Deployment when controller.enabled=false")
	}
}

// TestClientOnlyControllerInstance verifies that controllerInstance label
// is applied to VA resources in client-only mode.
func TestClientOnlyControllerInstance(t *testing.T) {
	output := helmTemplate(t, "wva-model-c", map[string]string{
		"controller.enabled":     "false",
		"va.enabled":             "true",
		"hpa.enabled":            "true",
		"wva.controllerInstance": "my-team",
		"llmd.namespace":         "team-c",
		"llmd.modelName":         "my-model",
	})

	if !strings.Contains(output, "kind: VariantAutoscaling") {
		t.Fatal("should contain VariantAutoscaling")
	}
	if !strings.Contains(output, "wva.llmd.ai/controller-instance: \"my-team\"") {
		t.Error("VA should have controller-instance label matching controllerInstance value")
	}
	if !strings.Contains(output, `controller_instance: "my-team"`) {
		t.Error("HPA metric selector should filter by controller_instance")
	}
	if strings.Contains(output, "controller-manager") {
		t.Error("should not contain controller Deployment in client-only mode")
	}
}

// TestScaleTargetKindDefault verifies that when scaleTargetKind is not specified,
// the VA defaults to Deployment as the scale target.
func TestScaleTargetKindDefault(t *testing.T) {
	output := helmTemplate(t, "wva-default-kind", map[string]string{
		"controller.enabled": "false",
		"va.enabled":         "true",
		"llmd.namespace":     "test-ns",
		"llmd.modelName":     "test-model",
	})

	if !strings.Contains(output, "kind: VariantAutoscaling") {
		t.Fatal("should contain VariantAutoscaling")
	}

	// Should default to Deployment
	if !strings.Contains(output, "apiVersion: apps/v1") {
		t.Error("should contain apiVersion: apps/v1 for Deployment")
	}
	if !strings.Contains(output, "kind: Deployment") {
		t.Error("should contain kind: Deployment as default scale target")
	}

	// Should NOT contain LeaderWorkerSet
	if strings.Contains(output, "kind: LeaderWorkerSet") {
		t.Error("should not contain LeaderWorkerSet when scaleTargetKind is not set")
	}
	if strings.Contains(output, "apiVersion: leaderworkerset.x-k8s.io/v1") {
		t.Error("should not contain leaderworkerset apiVersion when scaleTargetKind is not set")
	}
}

// TestScaleTargetKindDeployment verifies that scaleTargetKind="Deployment"
// produces a VA with Deployment as scale target.
func TestScaleTargetKindDeployment(t *testing.T) {
	output := helmTemplate(t, "wva-deployment-kind", map[string]string{
		"controller.enabled":   "false",
		"va.enabled":           "true",
		"llmd.namespace":       "test-ns",
		"llmd.modelName":       "test-model",
		"llmd.scaleTargetKind": "Deployment",
	})

	if !strings.Contains(output, "kind: VariantAutoscaling") {
		t.Fatal("should contain VariantAutoscaling")
	}

	// Should use Deployment
	if !strings.Contains(output, "apiVersion: apps/v1") {
		t.Error("should contain apiVersion: apps/v1 for Deployment")
	}
	if !strings.Contains(output, "kind: Deployment") {
		t.Error("should contain kind: Deployment as scale target")
	}

	// Should NOT contain LeaderWorkerSet
	if strings.Contains(output, "kind: LeaderWorkerSet") {
		t.Error("should not contain LeaderWorkerSet when scaleTargetKind is Deployment")
	}
}

// TestScaleTargetKindLeaderWorkerSet verifies that scaleTargetKind="LeaderWorkerSet"
// produces a VA with LeaderWorkerSet as scale target.
func TestScaleTargetKindLeaderWorkerSet(t *testing.T) {
	output := helmTemplate(t, "wva-lws-kind", map[string]string{
		"controller.enabled":   "false",
		"va.enabled":           "true",
		"llmd.namespace":       "test-ns",
		"llmd.modelName":       "test-model",
		"llmd.scaleTargetKind": "LeaderWorkerSet",
	})

	if !strings.Contains(output, "kind: VariantAutoscaling") {
		t.Fatal("should contain VariantAutoscaling")
	}

	// Should use LeaderWorkerSet
	if !strings.Contains(output, "apiVersion: leaderworkerset.x-k8s.io/v1") {
		t.Error("should contain apiVersion: leaderworkerset.x-k8s.io/v1 for LeaderWorkerSet")
	}
	if !strings.Contains(output, "kind: LeaderWorkerSet") {
		t.Error("should contain kind: LeaderWorkerSet as scale target")
	}

	// Note: "kind: Deployment" might still appear in HPA scaleTargetRef,
	// but the LeaderWorkerSet check above ensures the VA uses the correct scale target.
}

// TestScaleTargetNameOverride verifies that scaleTargetName can override
// the default scale target name in the VA scaleTargetRef.
func TestScaleTargetNameOverride(t *testing.T) {
	output := helmTemplate(t, "wva-custom-target", map[string]string{
		"controller.enabled":   "false",
		"va.enabled":           "true",
		"llmd.namespace":       "test-ns",
		"llmd.modelName":       "test-model",
		"llmd.scaleTargetName": "custom-statefulset",
		"llmd.scaleTargetKind": "LeaderWorkerSet",
	})

	if !strings.Contains(output, "kind: VariantAutoscaling") {
		t.Fatal("should contain VariantAutoscaling")
	}

	// Should use custom name in VA
	if !strings.Contains(output, "name: custom-statefulset") {
		t.Error("should contain custom scaleTargetName in VA scaleTargetRef")
	}

	// Verify that both LeaderWorkerSet and custom name appear together
	// (indicates they're in the same scaleTargetRef block)
	if !strings.Contains(output, "kind: LeaderWorkerSet") {
		t.Error("should contain LeaderWorkerSet when scaleTargetKind is LeaderWorkerSet")
	}

	// Note: "name: test-model-decode" will still appear in the HPA scaleTargetRef
	// because HPA always targets the Deployment, not the LeaderWorkerSet.
	// This is expected behavior, so we don't check for its absence.
}

// TestScaleTargetKindWithDefaultName verifies that when scaleTargetKind is set
// but scaleTargetName is not, it uses the default naming pattern.
func TestScaleTargetKindWithDefaultName(t *testing.T) {
	output := helmTemplate(t, "wva-lws-default-name", map[string]string{
		"controller.enabled":   "false",
		"va.enabled":           "true",
		"llmd.namespace":       "test-ns",
		"llmd.modelName":       "my-model",
		"llmd.scaleTargetKind": "LeaderWorkerSet",
	})

	if !strings.Contains(output, "kind: VariantAutoscaling") {
		t.Fatal("should contain VariantAutoscaling")
	}

	// Should use LeaderWorkerSet
	if !strings.Contains(output, "kind: LeaderWorkerSet") {
		t.Error("should contain kind: LeaderWorkerSet")
	}

	// Should use default name pattern (modelName-decode)
	if !strings.Contains(output, "name: my-model-decode") {
		t.Error("should use default name pattern {modelName}-decode when scaleTargetName is not specified")
	}
}
