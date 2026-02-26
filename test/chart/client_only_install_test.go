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

	args := []string{"template", releaseName, chartPath}
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
