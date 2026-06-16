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
		decision interfaces.VariantDecision
		expected string
	}{
		// Saturation-only mode: classification driven by the SaturationOnly flag,
		// not by the reason string content.
		{
			name:     "saturation-only scale-up",
			decision: interfaces.VariantDecision{SaturationOnly: true, Action: interfaces.ActionScaleUp},
			expected: "saturation-only mode: scale-up",
		},
		{
			name:     "saturation-only scale-down",
			decision: interfaces.VariantDecision{SaturationOnly: true, Action: interfaces.ActionScaleDown},
			expected: "saturation-only mode: scale-down",
		},
		{
			name:     "saturation-only no-change",
			decision: interfaces.VariantDecision{SaturationOnly: true, Action: interfaces.ActionNoChange},
			expected: "saturation-only mode: no-change",
		},
		// Scale-from-zero: identified by the scalefromzero reason prefix.
		{
			name: "scalefromzero scale-up",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleUp,
				Reason: "scalefromzero mode: pending request - scale-up",
			},
			expected: "scalefromzero: scale-up",
		},
		{
			name: "scalefromzero scale-down",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleDown,
				Reason: "scalefromzero mode: some reason",
			},
			expected: "scalefromzero: scale-down",
		},
		// V2 optimizer: identified by the "V2" reason prefix.
		// Dynamic values embedded in the reason string (required capacity, spare
		// capacity, optimizer name) are stripped to prevent cardinality explosion.
		{
			name: "V2 scale-up with cost-aware optimizer and dynamic required capacity",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleUp,
				Reason: "V2 scale-up (optimizer: cost-aware, required: 1500)",
			},
			expected: "V2 scale-up",
		},
		{
			name: "V2 scale-down with greedy-by-score optimizer and dynamic spare capacity",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleDown,
				Reason: "V2 scale-down (optimizer: greedy-by-score, spare: 2300)",
			},
			expected: "V2 scale-down",
		},
		{
			name: "V2 enforced scale-up",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleUp,
				Reason: "V2 ScaleUp (optimizer: cost-aware, enforced)",
			},
			expected: "V2 scale-up",
		},
		{
			name: "V2 enforced scale-down",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleDown,
				Reason: "V2 ScaleDown (optimizer: greedy-by-score, enforced)",
			},
			expected: "V2 scale-down",
		},
		{
			name: "V2 steady state",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionNoChange,
				Reason: "V2 steady state",
			},
			expected: "V2 no-change",
		},
		// Default fallback: any other reason maps to action only.
		{
			name: "no scaling decision optimization loop scale-up",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleUp,
				Reason: "No scaling decision (optimization loop)",
			},
			expected: "scale-up",
		},
		{
			name: "no scaling decision optimization loop scale-down",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleDown,
				Reason: "No scaling decision (optimization loop)",
			},
			expected: "scale-down",
		},
		{
			name: "unknown pattern scale-up",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionScaleUp,
				Reason: "some unknown reason",
			},
			expected: "scale-up",
		},
		{
			name: "unknown pattern no-change",
			decision: interfaces.VariantDecision{
				Action: interfaces.ActionNoChange,
				Reason: "some unknown reason",
			},
			expected: "no-change",
		},
		// SaturationOnly flag takes precedence over any reason prefix.
		{
			name: "saturation-only flag beats scalefromzero prefix",
			decision: interfaces.VariantDecision{
				SaturationOnly: true,
				Action:         interfaces.ActionScaleUp,
				Reason:         "scalefromzero mode: pending request - scale-up",
			},
			expected: "saturation-only mode: scale-up",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeReasonForMetrics(&tt.decision)
			if result != tt.expected {
				t.Errorf("sanitizeReasonForMetrics() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestSanitizeReasonForMetrics_CardinalityBounded verifies that all possible
// output values are bounded and won't cause Prometheus cardinality explosion.
func TestSanitizeReasonForMetrics_CardinalityBounded(t *testing.T) {
	actions := []interfaces.SaturationAction{
		interfaces.ActionScaleUp,
		interfaces.ActionScaleDown,
		interfaces.ActionNoChange,
	}

	// Representative decisions covering every classification branch.
	// Reason strings include dynamic numeric values to confirm they are stripped.
	decisionTemplates := []interfaces.VariantDecision{
		// Saturation-only (flag-driven, reason irrelevant)
		{SaturationOnly: true},
		// Scale-from-zero prefix
		{Reason: "scalefromzero mode: pending request - scale-up"},
		// V2 prefix with dynamic values
		{Reason: "V2 scale-up (optimizer: cost-aware, required: 1500)"},
		{Reason: "V2 scale-up (optimizer: cost-aware, required: 2300)"},
		{Reason: "V2 scale-up (optimizer: greedy-by-score, required: 999)"},
		{Reason: "V2 scale-down (optimizer: cost-aware, spare: 800)"},
		{Reason: "V2 scale-down (optimizer: greedy-by-score, spare: 1200)"},
		{Reason: "V2 ScaleUp (optimizer: cost-aware, enforced)"},
		{Reason: "V2 ScaleDown (optimizer: greedy-by-score, enforced)"},
		{Reason: "V2 steady state"},
		// Default fallback
		{Reason: "No scaling decision (optimization loop)"},
	}

	// Expected bounded set of output values — 12 in total.
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

	for _, tmpl := range decisionTemplates {
		for _, action := range actions {
			d := tmpl
			d.Action = action
			result := sanitizeReasonForMetrics(&d)
			seenOutputs[result] = true

			if !expectedOutputs[result] {
				t.Errorf("unexpected output value %q (reason: %q, saturationOnly: %v, action: %v)",
					result, d.Reason, d.SaturationOnly, action)
			}
		}
	}

	t.Logf("verified bounded cardinality: %d unique output values from %d input combinations",
		len(seenOutputs), len(decisionTemplates)*len(actions))
}
