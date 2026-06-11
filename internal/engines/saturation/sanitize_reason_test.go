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
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

func TestSanitizeReasonForMetrics(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		action   interfaces.SaturationAction
		expected string
	}{
		// Saturation-only mode patterns
		{
			name:     "saturation-only scale-up",
			reason:   "saturation-only mode: ScaleUp",
			action:   interfaces.ActionScaleUp,
			expected: "saturation-only mode: scale-up",
		},
		{
			name:     "saturation-only scale-down",
			reason:   "saturation-only mode: ScaleDown",
			action:   interfaces.ActionScaleDown,
			expected: "saturation-only mode: scale-down",
		},
		{
			name:     "saturation-only no-change",
			reason:   "saturation-only mode: NoChange",
			action:   interfaces.ActionNoChange,
			expected: "saturation-only mode: no-change",
		},
		// Scale-from-zero patterns
		{
			name:     "scalefromzero scale-up",
			reason:   "scalefromzero mode: pending request - scale-up",
			action:   interfaces.ActionScaleUp,
			expected: "scalefromzero: scale-up",
		},
		{
			name:     "scalefromzero scale-down",
			reason:   "scalefromzero mode: some reason",
			action:   interfaces.ActionScaleDown,
			expected: "scalefromzero: scale-down",
		},
		// V2 optimizer patterns with dynamic values (cardinality issue)
		{
			name:     "V2 scale-up with cost-aware optimizer and dynamic required capacity",
			reason:   "V2 scale-up (optimizer: cost-aware, required: 1500)",
			action:   interfaces.ActionScaleUp,
			expected: "V2 scale-up",
		},
		{
			name:     "V2 scale-down with greedy-by-score optimizer and dynamic spare capacity",
			reason:   "V2 scale-down (optimizer: greedy-by-score, spare: 2300)",
			action:   interfaces.ActionScaleDown,
			expected: "V2 scale-down",
		},
		{
			name:     "V2 enforced scale-up",
			reason:   "V2 ScaleUp (optimizer: cost-aware, enforced)",
			action:   interfaces.ActionScaleUp,
			expected: "V2 scale-up",
		},
		{
			name:     "V2 enforced scale-down",
			reason:   "V2 ScaleDown (optimizer: greedy-by-score, enforced)",
			action:   interfaces.ActionScaleDown,
			expected: "V2 scale-down",
		},
		{
			name:     "V2 steady state",
			reason:   "V2 steady state",
			action:   interfaces.ActionNoChange,
			expected: "V2 no-change",
		},
		// Fallback patterns
		{
			name:     "no scaling decision optimization loop scale-up",
			reason:   "No scaling decision (optimization loop)",
			action:   interfaces.ActionScaleUp,
			expected: "scale-up",
		},
		{
			name:     "no scaling decision optimization loop scale-down",
			reason:   "No scaling decision (optimization loop)",
			action:   interfaces.ActionScaleDown,
			expected: "scale-down",
		},
		{
			name:     "unknown pattern scale-up",
			reason:   "some unknown reason",
			action:   interfaces.ActionScaleUp,
			expected: "scale-up",
		},
		{
			name:     "unknown pattern no-change",
			reason:   "some unknown reason",
			action:   interfaces.ActionNoChange,
			expected: "no-change",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeReasonForMetrics(tt.reason, tt.action)
			if result != tt.expected {
				t.Errorf("sanitizeReasonForMetrics(%q, %v) = %q, want %q",
					tt.reason, tt.action, result, tt.expected)
			}
		})
	}
}

// TestSanitizeReasonForMetrics_CardinalityBounded verifies that all possible
// output values are bounded and won't cause Prometheus cardinality explosion.
func TestSanitizeReasonForMetrics_CardinalityBounded(t *testing.T) {
	// All possible reasons from the codebase
	inputReasons := []string{
		// Saturation patterns
		"saturation-only mode: ScaleUp",
		"saturation-only mode: ScaleDown",
		"saturation-only mode: NoChange",
		// Scale-from-zero patterns
		"scalefromzero mode: pending request - scale-up",
		// V2 patterns with unbounded dynamic values
		"V2 scale-up (optimizer: cost-aware, required: 1500)",
		"V2 scale-up (optimizer: cost-aware, required: 2300)",
		"V2 scale-up (optimizer: greedy-by-score, required: 999)",
		"V2 scale-down (optimizer: cost-aware, spare: 800)",
		"V2 scale-down (optimizer: greedy-by-score, spare: 1200)",
		"V2 ScaleUp (optimizer: cost-aware, enforced)",
		"V2 ScaleDown (optimizer: greedy-by-score, enforced)",
		"V2 steady state",
		// Other patterns
		"No scaling decision (optimization loop)",
	}

	actions := []interfaces.SaturationAction{
		interfaces.ActionScaleUp,
		interfaces.ActionScaleDown,
		interfaces.ActionNoChange,
	}

	// Expected bounded set of output values
	expectedOutputs := map[string]bool{
		"saturation-only mode: scale-up":   true,
		"saturation-only mode: scale-down": true,
		"saturation-only mode: no-change":  true,
		"scalefromzero: scale-up":          true,
		"scalefromzero: scale-down":        true,
		"scalefromzero: no-change":         true,
		"V2 scale-up":                      true,
		"V2 scale-down":                    true,
		"V2 no-change":                     true,
		"scale-up":                         true,
		"scale-down":                       true,
		"no-change":                        true,
	}

	seenOutputs := make(map[string]bool)

	// Test all combinations
	for _, reason := range inputReasons {
		for _, action := range actions {
			result := sanitizeReasonForMetrics(reason, action)
			seenOutputs[result] = true

			// Verify the output is in the expected bounded set
			if !expectedOutputs[result] {
				t.Errorf("Unexpected output value: %q (input: %q, action: %v)",
					result, reason, action)
			}
		}
	}

	// Verify cardinality is bounded (should be exactly 12 possible values)
	if len(seenOutputs) > len(expectedOutputs) {
		t.Errorf("Output cardinality exceeds expected bound: got %d unique values, want at most %d",
			len(seenOutputs), len(expectedOutputs))
	}

	t.Logf("Verified bounded cardinality: %d unique output values from %d input combinations",
		len(seenOutputs), len(inputReasons)*len(actions))
}
