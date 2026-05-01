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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
)

// newVAFixture builds a minimal VariantAutoscaling for event-emission tests.
func newVAFixture(name, namespace string) *llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	return &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

// TestEmitAcceleratorNotResolvedEvent_RecordsWarning confirms the helper
// emits a Warning event with the AcceleratorNotResolved reason and a
// remediation message that points operators at nodeSelector / VA label.
func TestEmitAcceleratorNotResolvedEvent_RecordsWarning(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	e := &Engine{Recorder: recorder}
	va := newVAFixture("variant-a", "team-x")

	e.emitAcceleratorNotResolvedEvent(va)

	select {
	case got := <-recorder.Events:
		if !strings.HasPrefix(got, "Warning AcceleratorNotResolved ") {
			t.Fatalf("unexpected event prefix: %q", got)
		}
		if !strings.Contains(got, "nodeSelector") {
			t.Errorf("event message should mention nodeSelector remediation, got: %q", got)
		}
		if !strings.Contains(got, "HPA/KEDA metrics will not be emitted") {
			t.Errorf("event message should warn that metrics are gated, got: %q", got)
		}
	default:
		t.Fatal("expected an event to be recorded, none was")
	}
}

// TestEmitAcceleratorNotResolvedEvent_NilRecorderIsNoOp confirms the helper
// is safe to call when the engine has no recorder configured (e.g. unit
// tests that exercise other code paths).
func TestEmitAcceleratorNotResolvedEvent_NilRecorderIsNoOp(t *testing.T) {
	e := &Engine{Recorder: nil}
	va := newVAFixture("variant-a", "team-x")

	// Should not panic.
	e.emitAcceleratorNotResolvedEvent(va)
}

// TestEmitAcceleratorNotResolvedEvent_ConstantMessageEnablesDedup confirms
// that repeated emissions for the same VA produce identical (type, reason,
// message) tuples — which is what allows the K8s API server's event
// aggregator to collapse them into a single Event with an updated count
// rather than creating a new entry per optimization cycle.
func TestEmitAcceleratorNotResolvedEvent_ConstantMessageEnablesDedup(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	e := &Engine{Recorder: recorder}
	va := newVAFixture("variant-a", "team-x")

	e.emitAcceleratorNotResolvedEvent(va)
	e.emitAcceleratorNotResolvedEvent(va)

	first := <-recorder.Events
	second := <-recorder.Events
	if first != second {
		t.Fatalf("expected identical event strings to enable API-server-side dedup, got\nfirst:  %q\nsecond: %q", first, second)
	}
	if !strings.Contains(first, corev1.EventTypeWarning) {
		t.Errorf("event should be Warning-typed, got: %q", first)
	}
}
