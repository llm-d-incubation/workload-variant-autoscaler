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

// Package events provides a rate-limited wrapper around the Kubernetes
// EventRecorder for emitting events on VariantAutoscaling resources.
package events

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// RateLimitedRecorder wraps an EventRecorder and ensures at most one event
// per (namespace/name, reason) pair is emitted per cycle.
//
// ResetCycle must be called at the start of each optimization cycle to clear
// the rate limit state.
type RateLimitedRecorder struct {
	recorder record.EventRecorder

	mu    sync.Mutex
	seen  map[string]struct{}
}

// New returns a new RateLimitedRecorder wrapping the given EventRecorder.
// If recorder is nil, the returned RateLimitedRecorder is a no-op and is
// safe to call.
func New(recorder record.EventRecorder) *RateLimitedRecorder {
	return &RateLimitedRecorder{
		recorder: recorder,
		seen:     make(map[string]struct{}),
	}
}

// ResetCycle clears the rate limit state and should be called at the start
// of each optimization cycle.
func (r *RateLimitedRecorder) ResetCycle() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = make(map[string]struct{})
}

// Eventf emits an event on the given object if no prior event with the same
// reason has been emitted for the same (namespace/name) in the current cycle.
// It is a no-op if the recorder is nil or object is nil.
func (r *RateLimitedRecorder) Eventf(
	object runtime.Object,
	namespace, name, eventtype, reason, messageFmt string,
	args ...interface{},
) {
	if r == nil || r.recorder == nil || object == nil {
		return
	}

	key := utils.GetNamespacedKey(namespace, name) + "#" + reason

	r.mu.Lock()
	if _, ok := r.seen[key]; ok {
		r.mu.Unlock()
		return
	}
	r.seen[key] = struct{}{}
	r.mu.Unlock()

	r.recorder.Eventf(object, eventtype, reason, messageFmt, args...)
}
