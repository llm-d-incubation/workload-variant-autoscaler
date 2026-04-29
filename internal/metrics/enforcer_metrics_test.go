/*
Copyright 2026 The llm-d Authors

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

package metrics

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/prometheus/client_golang/prometheus"
)

func TestEmitEnforcerMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit enforcer metric for scale-to-zero policy
	if err := emitter.EmitEnforcerMetric("scale-to-zero"); err != nil {
		t.Fatalf("EmitEnforcerMetric failed: %v", err)
	}

	// Emit enforcer metric for minimum-replica policy
	if err := emitter.EmitEnforcerMetric("minimum-replica"); err != nil {
		t.Fatalf("EmitEnforcerMetric failed: %v", err)
	}

	// Emit multiple times for the same policy (counter should increment)
	if err := emitter.EmitEnforcerMetric("scale-to-zero"); err != nil {
		t.Fatalf("EmitEnforcerMetric failed: %v", err)
	}

	// Verify the counter was recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAEnforcerModificationsTotal {
			found = true
			// Should have 2 metric series (one per policy_type)
			if len(mf.GetMetric()) != 2 {
				t.Errorf("Expected 2 metric series, got %d", len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				c := m.GetCounter()
				if c == nil {
					t.Error("Expected counter metric")
					continue
				}
				// Check policy_type label
				policyType := getLabelValue(m, constants.LabelPolicyType)
				switch policyType {
				case "scale-to-zero":
					if c.GetValue() != 2 {
						t.Errorf("Expected scale-to-zero counter to be 2, got %f", c.GetValue())
					}
				case "minimum-replica":
					if c.GetValue() != 1 {
						t.Errorf("Expected minimum-replica counter to be 1, got %f", c.GetValue())
					}
				default:
					t.Errorf("Unexpected policy_type label: %s", policyType)
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAEnforcerModificationsTotal)
	}
}

func TestEmitEnforcerMetric_NilSafety(t *testing.T) {
	// Reset the package-level var to nil to simulate uninitialized state
	savedEnforcerModificationsTotal := enforcerModificationsTotal
	enforcerModificationsTotal = nil
	defer func() {
		enforcerModificationsTotal = savedEnforcerModificationsTotal
	}()

	emitter := NewMetricsEmitter()

	// Should return error when metrics are not initialized
	err := emitter.EmitEnforcerMetric("scale-to-zero")
	if err == nil {
		t.Error("Expected error when enforcerModificationsTotal is nil, got nil")
	}
	expectedErr := "enforcerModificationsTotal metric not initialized"
	if err.Error() != expectedErr {
		t.Errorf("Expected error message '%s', got '%s'", expectedErr, err.Error())
	}
}

func TestEmitEnforcerMetric_WithControllerInstance(t *testing.T) {
	// Save and restore original controller instance and metrics
	savedInstance := controllerInstance
	savedEnforcerModificationsTotal := enforcerModificationsTotal
	defer func() {
		controllerInstance = savedInstance
		enforcerModificationsTotal = savedEnforcerModificationsTotal
	}()

	// Set environment variable BEFORE InitMetrics so labels are created correctly
	t.Setenv(ControllerInstanceEnvVar, "controller-1")

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Emit enforcer metric
	if err := emitter.EmitEnforcerMetric("scale-to-zero"); err != nil {
		t.Fatalf("EmitEnforcerMetric failed: %v", err)
	}

	// Verify the metric includes controller_instance label
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAEnforcerModificationsTotal {
			found = true
			if len(mf.GetMetric()) != 1 {
				t.Errorf("Expected 1 metric series, got %d", len(mf.GetMetric()))
			}
			m := mf.GetMetric()[0]
			instance := getLabelValue(m, constants.LabelControllerInstance)
			if instance != "controller-1" {
				t.Errorf("Expected controller_instance=controller-1, got %s", instance)
			}
			policyType := getLabelValue(m, constants.LabelPolicyType)
			if policyType != "scale-to-zero" {
				t.Errorf("Expected policy_type=scale-to-zero, got %s", policyType)
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAEnforcerModificationsTotal)
	}
}

func TestEmitEnforcerMetric_MultiplePolicyTypes(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}

	emitter := NewMetricsEmitter()

	// Test various policy types
	policyTypes := []string{
		"scale-to-zero",
		"minimum-replica",
		"custom-policy",
	}

	for _, policyType := range policyTypes {
		if err := emitter.EmitEnforcerMetric(policyType); err != nil {
			t.Fatalf("EmitEnforcerMetric failed for policy %s: %v", policyType, err)
		}
	}

	// Verify all policy types were recorded
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	var found bool
	foundPolicyTypes := make(map[string]bool)
	for _, mf := range metrics {
		if mf.GetName() == constants.WVAEnforcerModificationsTotal {
			found = true
			if len(mf.GetMetric()) != len(policyTypes) {
				t.Errorf("Expected %d metric series, got %d", len(policyTypes), len(mf.GetMetric()))
			}
			for _, m := range mf.GetMetric() {
				policyType := getLabelValue(m, constants.LabelPolicyType)
				foundPolicyTypes[policyType] = true
				c := m.GetCounter()
				if c == nil {
					t.Error("Expected counter metric")
					continue
				}
				if c.GetValue() != 1 {
					t.Errorf("Expected counter value 1 for policy %s, got %f", policyType, c.GetValue())
				}
			}
		}
	}
	if !found {
		t.Errorf("Metric %s not found in gathered metrics", constants.WVAEnforcerModificationsTotal)
	}

	// Verify all expected policy types were found
	for _, expected := range policyTypes {
		if !foundPolicyTypes[expected] {
			t.Errorf("Expected policy type %s not found in metrics", expected)
		}
	}
}
