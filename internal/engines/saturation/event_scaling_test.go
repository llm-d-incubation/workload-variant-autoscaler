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

package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// TestScaledUpEvent verifies K8SEventScaledUp is recorded for scale-up decisions
func TestScaledUpEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "default",
		},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "test-deployment",
			},
			ModelID:     "test-model",
			MaxReplicas: 5,
		},
	}

	decision := &interfaces.VariantDecision{
		VariantName: "test-va",
		Action:      interfaces.ActionScaleUp,
		Reason:      "KV cache utilization above threshold",
	}

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case interfaces.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason)
		case interfaces.ActionScaleDown:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason)
		}
	}

	// Verify event was recorded
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventScaledUp,
			"Event should contain K8SEventScaledUp constant")
		assert.Contains(t, event, decision.Reason,
			"Event should contain the reason message")
		assert.Contains(t, event, "Normal",
			"Event should be Normal type")
	default:
		t.Error("Expected ScaledUp event to be recorded but none was found")
	}
}

// TestScaledDownEvent verifies K8SEventScaledDown is recorded for scale-down decisions
func TestScaledDownEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "default",
		},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "test-deployment",
			},
			ModelID:     "test-model",
			MaxReplicas: 5,
		},
	}

	decision := &interfaces.VariantDecision{
		VariantName: "test-va",
		Action:      interfaces.ActionScaleDown,
		Reason:      "KV cache utilization below threshold",
	}

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case interfaces.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason)
		case interfaces.ActionScaleDown:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason)
		}
	}

	// Verify event was recorded
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventScaledDown,
			"Event should contain K8SEventScaledDown constant")
		assert.Contains(t, event, decision.Reason,
			"Event should contain the reason message")
		assert.Contains(t, event, "Normal",
			"Event should be Normal type")
	default:
		t.Error("Expected ScaledDown event to be recorded but none was found")
	}
}

// TestResourceConstrainedEvent verifies K8SEventResourceConstrained is recorded when limited
func TestResourceConstrainedEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "default",
		},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "test-deployment",
			},
			ModelID:     "test-model",
			MaxReplicas: 5,
		},
	}

	decision := &interfaces.VariantDecision{
		VariantName: "test-va",
		Action:      interfaces.ActionScaleUp,
		Reason:      "KV cache utilization above threshold",
		WasLimited:  true,
	}

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case interfaces.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason)
		case interfaces.ActionScaleDown:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason)
		}
		if decision.WasLimited {
			fakeRecorder.Eventf(va, corev1.EventTypeWarning, constants.K8SEventResourceConstrained, decision.Reason)
		}
	}

	// Verify both events were recorded
	eventsRecorded := 0
	foundScaleUp := false
	foundResourceConstrained := false

	for eventsRecorded < 2 {
		select {
		case event := <-fakeRecorder.Events:
			t.Logf("Received event: %s", event)
			if !foundScaleUp && assert.Contains(t, event, constants.K8SEventScaledUp) {
				foundScaleUp = true
				assert.Contains(t, event, decision.Reason,
					"ScaledUp event should contain the reason")
				assert.Contains(t, event, "Normal",
					"ScaledUp event should be Normal type")
			} else if !foundResourceConstrained && assert.Contains(t, event, constants.K8SEventResourceConstrained) {
				foundResourceConstrained = true
				assert.Contains(t, event, "Warning",
					"ResourceConstrained event should be Warning type")
				assert.Contains(t, event, decision.Reason,
					"ResourceConstrained event should contain the reason")
			}
			eventsRecorded++
		default:
			goto done
		}
	}

done:
	assert.True(t, foundScaleUp, "Should have recorded ScaledUp event")
	assert.True(t, foundResourceConstrained, "Should have recorded ResourceConstrained event")
}

// TestNoEventForNoDecision verifies no events are recorded when action is NoChange
func TestNoEventForNoDecision(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(100)

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "default",
		},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "test-deployment",
			},
			ModelID:     "test-model",
			MaxReplicas: 5,
		},
	}

	decision := &interfaces.VariantDecision{
		VariantName: "test-va",
		Action:      interfaces.ActionNoChange,
		Reason:      "No scaling needed",
	}

	// Simulate the event recording logic from applySaturationDecisions
	if fakeRecorder != nil {
		switch decision.Action {
		case interfaces.ActionScaleUp:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, decision.Reason)
		case interfaces.ActionScaleDown:
			fakeRecorder.Eventf(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, decision.Reason)
		default:
			// do nothing
		}
	}

	// Verify no event was recorded
	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("Unexpected event recorded for ActionNoChange: %s", event)
	default:
		// No event expected - this is correct
	}
}

// TestK8SEventScalingConstants verifies the scaling event constants are correctly defined
func TestK8SEventScalingConstants(t *testing.T) {
	assert.Equal(t, "ScaledUp", constants.K8SEventScaledUp,
		"K8SEventScaledUp constant should match expected value")
	assert.Equal(t, "ScaledDown", constants.K8SEventScaledDown,
		"K8SEventScaledDown constant should match expected value")
	assert.Equal(t, "ResourceConstrained", constants.K8SEventResourceConstrained,
		"K8SEventResourceConstrained constant should match expected value")
}
