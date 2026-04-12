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

package events

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

func newTestObject() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "test-ns",
		},
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

func TestRateLimitedRecorder_EmitsEvent(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	rec := New(fake)
	obj := newTestObject()

	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "replicas 2->4")

	events := drainEvents(fake.Events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if !strings.Contains(events[0], "ScaledUp") {
		t.Errorf("expected event to contain ScaledUp, got %q", events[0])
	}
	if !strings.Contains(events[0], "replicas 2->4") {
		t.Errorf("expected event to contain message, got %q", events[0])
	}
}

func TestRateLimitedRecorder_RateLimitsSameReason(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	rec := New(fake)
	obj := newTestObject()

	// Same (ns/name, reason) emitted 3 times — should only produce 1 event
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "first")
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "second")
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "third")

	events := drainEvents(fake.Events)
	if len(events) != 1 {
		t.Errorf("expected 1 event (rate limited), got %d", len(events))
	}
}

func TestRateLimitedRecorder_DifferentReasonsNotLimited(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	rec := New(fake)
	obj := newTestObject()

	// Same VA, different reasons — both should be emitted
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "first")
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeWarning, "ResourceConstrained", "second")

	events := drainEvents(fake.Events)
	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
}

func TestRateLimitedRecorder_DifferentVAsNotLimited(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	rec := New(fake)
	obj := newTestObject()

	// Different VAs, same reason — both should be emitted
	rec.Eventf(obj, "ns1", "va1", corev1.EventTypeNormal, "ScaledUp", "first")
	rec.Eventf(obj, "ns2", "va2", corev1.EventTypeNormal, "ScaledUp", "second")

	events := drainEvents(fake.Events)
	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
}

func TestRateLimitedRecorder_ResetCycle(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	rec := New(fake)
	obj := newTestObject()

	// Cycle 1
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "cycle1")
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "cycle1-dup")

	// Reset before cycle 2
	rec.ResetCycle()

	// Cycle 2 — same (VA, reason) should be emitted again
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "cycle2")

	events := drainEvents(fake.Events)
	if len(events) != 2 {
		t.Errorf("expected 2 events (one per cycle), got %d", len(events))
	}
}

func TestRateLimitedRecorder_NilRecorderNoPanic(t *testing.T) {
	rec := New(nil)
	obj := newTestObject()

	// Should not panic
	rec.Eventf(obj, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "msg")
	rec.ResetCycle()
}

func TestRateLimitedRecorder_NilReceiverNoPanic(t *testing.T) {
	var rec *RateLimitedRecorder

	// Should not panic
	rec.ResetCycle()
}

func TestRateLimitedRecorder_NilObjectNoEvent(t *testing.T) {
	fake := record.NewFakeRecorder(10)
	rec := New(fake)

	rec.Eventf(nil, "test-ns", "test-va", corev1.EventTypeNormal, "ScaledUp", "msg")

	events := drainEvents(fake.Events)
	if len(events) != 0 {
		t.Errorf("expected 0 events for nil object, got %d", len(events))
	}
}
