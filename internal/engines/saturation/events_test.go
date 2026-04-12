/*
Copyright 2025 The llm-d Authors

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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	eventsHelper "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/events"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

func newTestVA(name, namespace string) *llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	return &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

// newTestEngine returns an Engine with just the eventRecorder initialized
// for testing emitScalingEvents in isolation.
func newTestEngine(recorder record.EventRecorder) *Engine {
	return &Engine{
		eventRecorder: eventsHelper.New(recorder),
	}
}

func drainEvents(ch <-chan string) []string {
	var events []string
	for {
		select {
		case e := <-ch:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestEmitScalingEvents_ScaledUp(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)
	va := newTestVA("va1", "ns1")

	decision := interfaces.VariantDecision{
		CurrentReplicas: 2,
		Reason:          "saturation=0.85",
	}
	e.emitScalingEvents(va, decision, true, true, 4)

	events := drainEvents(fake.Events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %v", len(events), events)
	}
	if !strings.Contains(events[0], constants.EventReasonScaledUp) {
		t.Errorf("expected ScaledUp event, got %q", events[0])
	}
	if !strings.Contains(events[0], "2 to 4") {
		t.Errorf("expected replica transition in message, got %q", events[0])
	}
}

func TestEmitScalingEvents_ScaledDown(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)
	va := newTestVA("va1", "ns1")

	decision := interfaces.VariantDecision{
		CurrentReplicas: 5,
		Reason:          "low load",
	}
	e.emitScalingEvents(va, decision, true, true, 2)

	events := drainEvents(fake.Events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if !strings.Contains(events[0], constants.EventReasonScaledDown) {
		t.Errorf("expected ScaledDown event, got %q", events[0])
	}
	if !strings.Contains(events[0], "5 to 2") {
		t.Errorf("expected replica transition in message, got %q", events[0])
	}
}

func TestEmitScalingEvents_ScaledToZero(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)
	va := newTestVA("va1", "ns1")

	decision := interfaces.VariantDecision{
		CurrentReplicas: 3,
		Reason:          "no active requests",
	}
	e.emitScalingEvents(va, decision, true, true, 0)

	events := drainEvents(fake.Events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if !strings.Contains(events[0], constants.EventReasonScaledToZero) {
		t.Errorf("expected ScaledToZero event, got %q", events[0])
	}
	// Should NOT emit ScaledDown
	if strings.Contains(events[0], constants.EventReasonScaledDown) {
		t.Errorf("expected ScaledToZero (not ScaledDown), got %q", events[0])
	}
}

func TestEmitScalingEvents_NoChange(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)
	va := newTestVA("va1", "ns1")

	decision := interfaces.VariantDecision{
		CurrentReplicas: 3,
	}
	e.emitScalingEvents(va, decision, true, true, 3)

	events := drainEvents(fake.Events)
	if len(events) != 0 {
		t.Errorf("expected 0 events for no-change, got %d: %v", len(events), events)
	}
}

func TestEmitScalingEvents_ResourceConstrained(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)
	va := newTestVA("va1", "ns1")

	decision := interfaces.VariantDecision{
		CurrentReplicas:        2,
		OriginalTargetReplicas: 10,
		WasLimited:             true,
		LimitedBy:              "gpu-limiter",
		Reason:                 "scale up",
	}
	e.emitScalingEvents(va, decision, true, true, 4)

	events := drainEvents(fake.Events)
	// Expect ScaledUp + ResourceConstrained
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(events), events)
	}
	joined := strings.Join(events, "|")
	if !strings.Contains(joined, constants.EventReasonScaledUp) {
		t.Errorf("missing ScaledUp event: %v", events)
	}
	if !strings.Contains(joined, constants.EventReasonResourceConstrained) {
		t.Errorf("missing ResourceConstrained event: %v", events)
	}
	if !strings.Contains(joined, "gpu-limiter") {
		t.Errorf("expected limiter name in message, got %v", events)
	}
}

func TestEmitScalingEvents_MetricsUnavailable(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)
	va := newTestVA("va1", "ns1")

	decision := interfaces.VariantDecision{}
	e.emitScalingEvents(va, decision, false, false, 0)

	events := drainEvents(fake.Events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if !strings.Contains(events[0], constants.EventReasonMetricsUnavailable) {
		t.Errorf("expected MetricsUnavailable event, got %q", events[0])
	}
}

func TestEmitScalingEvents_NoDecisionNoMetricsAvailable(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)
	va := newTestVA("va1", "ns1")

	// No decision, metrics available — should emit nothing (no-op cycle)
	e.emitScalingEvents(va, interfaces.VariantDecision{}, false, true, 3)

	events := drainEvents(fake.Events)
	if len(events) != 0 {
		t.Errorf("expected 0 events for no-decision steady state, got %d: %v", len(events), events)
	}
}

func TestEmitScalingEvents_RateLimitedPerCycle(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)
	va := newTestVA("va1", "ns1")

	decision := interfaces.VariantDecision{
		CurrentReplicas: 2,
		Reason:          "high load",
	}

	// Emit the same scale-up decision 3 times within one cycle
	e.emitScalingEvents(va, decision, true, true, 4)
	e.emitScalingEvents(va, decision, true, true, 4)
	e.emitScalingEvents(va, decision, true, true, 4)

	events := drainEvents(fake.Events)
	if len(events) != 1 {
		t.Errorf("expected 1 event (rate limited), got %d", len(events))
	}

	// New cycle — should allow emission again
	e.eventRecorder.ResetCycle()
	e.emitScalingEvents(va, decision, true, true, 4)

	events2 := drainEvents(fake.Events)
	if len(events2) != 1 {
		t.Errorf("expected 1 event after ResetCycle, got %d", len(events2))
	}
}

func TestEmitScalingEvents_NilVA(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	e := newTestEngine(fake)

	// Should not panic
	e.emitScalingEvents(nil, interfaces.VariantDecision{CurrentReplicas: 2}, true, true, 4)

	events := drainEvents(fake.Events)
	if len(events) != 0 {
		t.Errorf("expected 0 events for nil VA, got %d", len(events))
	}
}

func TestEmitScalingEvents_NilRecorder(t *testing.T) {
	e := &Engine{eventRecorder: nil}
	va := newTestVA("va1", "ns1")

	// Should not panic
	e.emitScalingEvents(va, interfaces.VariantDecision{CurrentReplicas: 2}, true, true, 4)
}
