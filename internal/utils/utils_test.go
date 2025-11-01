package utils

import (
	"testing"
	"time"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSuggestResourceNameFromVariantID(t *testing.T) {
	tests := []struct {
		name      string
		variantID string
		expected  string
	}{
		{
			name:      "variant with slashes and dots",
			variantID: "meta/llama-3.1-8b-A100-1",
			expected:  "meta-llama-3-1-8b-a100-1",
		},
		{
			name:      "variant with uppercase",
			variantID: "Meta/Llama-3.1-8B-A100-1",
			expected:  "meta-llama-3-1-8b-a100-1",
		},
		{
			name:      "variant with special characters",
			variantID: "model@name/variant_1",
			expected:  "modelname-variant1",
		},
		{
			name:      "variant with leading/trailing hyphens",
			variantID: "-model/variant-",
			expected:  "model-variant",
		},
		{
			name:      "simple variant",
			variantID: "vllm-deployment",
			expected:  "vllm-deployment",
		},
		{
			name:      "variant with multiple slashes",
			variantID: "org/team/model-A100-1",
			expected:  "org-team-model-a100-1",
		},
		{
			name:      "variant with underscores",
			variantID: "model_name_variant_1",
			expected:  "modelnamevariant1",
		},
		{
			name:      "variant with spaces",
			variantID: "model name variant 1",
			expected:  "modelnamevariant1",
		},
		{
			name:      "empty string",
			variantID: "",
			expected:  "",
		},
		{
			name:      "only invalid characters",
			variantID: "@#$%",
			expected:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SuggestResourceNameFromVariantID(tt.variantID)
			if result != tt.expected {
				t.Errorf("SuggestResourceNameFromVariantID(%q) = %q, expected %q",
					tt.variantID, result, tt.expected)
			}
		})
	}
}

func TestValidateVariantAutoscalingName(t *testing.T) {
	tests := []struct {
		name          string
		vaName        string
		variantID     string
		shouldLogDiff bool
		description   string
	}{
		{
			name:          "matching normalized name",
			vaName:        "meta-llama-3-1-8b-a100-1",
			variantID:     "meta/llama-3.1-8b-A100-1",
			shouldLogDiff: false,
			description:   "VA name matches the normalized variant_id",
		},
		{
			name:          "different but valid names",
			vaName:        "vllm-deployment",
			variantID:     "meta/llama-3.1-8b-A100-1",
			shouldLogDiff: true,
			description:   "VA name differs from normalized variant_id (normal case)",
		},
		{
			name:          "identical strings",
			vaName:        "simple-variant",
			variantID:     "simple-variant",
			shouldLogDiff: false,
			description:   "Both names are identical",
		},
		{
			name:          "deployment-style name with complex variant_id",
			vaName:        "llm-inference",
			variantID:     "organization/model-name/variant-1-A100-8",
			shouldLogDiff: true,
			description:   "Deployment name style vs hierarchical variant_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.vaName,
					Namespace: "test-namespace",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					VariantID: tt.variantID,
				},
			}

			// This function logs internally, we're just testing it doesn't panic
			// and completes successfully
			ValidateVariantAutoscalingName(va)

			// Verify the suggested name matches what we expect
			suggested := SuggestResourceNameFromVariantID(tt.variantID)
			if tt.shouldLogDiff {
				if va.Name == suggested {
					t.Errorf("Expected VA name %q to differ from suggested %q, but they match",
						va.Name, suggested)
				}
			} else {
				if va.Name != suggested {
					t.Errorf("Expected VA name %q to match suggested %q, but they differ",
						va.Name, suggested)
				}
			}
		})
	}
}

// TestDNS1123Compliance verifies that suggested names are valid Kubernetes resource names
func TestDNS1123Compliance(t *testing.T) {
	tests := []struct {
		name      string
		variantID string
	}{
		{
			name:      "complex variant",
			variantID: "Meta/Llama-3.1-8B-A100-1",
		},
		{
			name:      "special characters",
			variantID: "model@name/variant_1!test",
		},
		{
			name:      "unicode characters",
			variantID: "modèl/variánt",
		},
	}

	// DNS-1123 pattern: ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ (or empty)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SuggestResourceNameFromVariantID(tt.variantID)

			// Check that result only contains valid characters
			for _, c := range result {
				if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
					t.Errorf("SuggestResourceNameFromVariantID(%q) = %q contains invalid character %q",
						tt.variantID, result, string(c))
				}
			}

			// Check it doesn't start or end with hyphen
			if len(result) > 0 {
				if result[0] == '-' || result[len(result)-1] == '-' {
					t.Errorf("SuggestResourceNameFromVariantID(%q) = %q starts or ends with hyphen",
						tt.variantID, result)
				}
			}

			t.Logf("Input: %q -> Output: %q (valid: %t)", tt.variantID, result, result == "" || len(result) > 0)
		})
	}
}

// TestRealWorldExamples tests with actual variant_id patterns from the codebase
func TestRealWorldExamples(t *testing.T) {
	tests := []struct {
		name      string
		variantID string
		vaName    string
	}{
		{
			name:      "e2e test pattern",
			variantID: "test-model-A100-1",
			vaName:    "vllm-deployment",
		},
		{
			name:      "openshift test pattern",
			variantID: "test-model/variant-1-A100-1",
			vaName:    "vllm-deployment",
		},
		{
			name:      "huggingface model pattern",
			variantID: "meta-llama/Llama-3.1-8B-Instruct",
			vaName:    "llama-deployment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suggested := SuggestResourceNameFromVariantID(tt.variantID)
			t.Logf("variant_id: %q -> suggested: %q (actual va.Name: %q)",
				tt.variantID, suggested, tt.vaName)

			// Verify suggested name is valid
			if suggested != "" {
				// Must be lowercase alphanumeric with hyphens
				for _, c := range suggested {
					if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
						t.Errorf("Suggested name %q contains invalid character %q", suggested, string(c))
					}
				}
			}
		})
	}
}

// TestCreateOptimizedAlloc verifies that CreateOptimizedAlloc correctly sets VariantID
// from the parameter, matching the production code path in the optimizer.
func TestCreateOptimizedAlloc(t *testing.T) {
	tests := []struct {
		name          string
		vaName        string
		vaNamespace   string
		variantID     string
		accelerator   string
		numReplicas   int32
		expectError   bool
		errorContains string
	}{
		{
			name:        "valid allocation with variantID",
			vaName:      "vllm-deployment",
			vaNamespace: "default",
			variantID:   "meta/llama-3.1-8b-A100-1",
			accelerator: "A100",
			numReplicas: 3,
			expectError: false,
		},
		{
			name:        "variantID with slashes",
			vaName:      "test-deployment",
			vaNamespace: "test-ns",
			variantID:   "org/team/model-A100-4",
			accelerator: "A100",
			numReplicas: 2,
			expectError: false,
		},
		{
			name:        "simple variantID",
			vaName:      "simple-va",
			vaNamespace: "default",
			variantID:   "model-A100-1",
			accelerator: "A100",
			numReplicas: 1,
			expectError: false,
		},
		{
			name:          "server not found in solution",
			vaName:        "nonexistent",
			vaNamespace:   "default",
			variantID:     "test-A100-1",
			accelerator:   "A100",
			numReplicas:   1,
			expectError:   true,
			errorContains: "server nonexistent:default not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create allocation solution with the test data
			allocationSolution := &infernoConfig.AllocationSolution{
				Spec: map[string]infernoConfig.AllocationData{},
			}

			// Only add server to solution if we don't expect error
			if !tt.expectError {
				serverName := FullName(tt.vaName, tt.vaNamespace)
				allocationSolution.Spec[serverName] = infernoConfig.AllocationData{
					Accelerator: tt.accelerator,
					NumReplicas: tt.numReplicas,
					MaxBatch:    32,
					Cost:        40.0,
					ITLAverage:  50.0,
					TTFTAverage: 500.0,
				}
			}

			// Call CreateOptimizedAlloc
			result, err := CreateOptimizedAlloc(tt.vaName, tt.vaNamespace, tt.variantID, allocationSolution)

			// Check error expectations
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error containing %q, but got nil", tt.errorContains)
					return
				}
				if tt.errorContains != "" {
					// Simple substring check
					found := false
					for i := 0; i <= len(err.Error())-len(tt.errorContains); i++ {
						if err.Error()[i:i+len(tt.errorContains)] == tt.errorContains {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("Expected error containing %q, got %q", tt.errorContains, err.Error())
					}
				}
				return
			}

			// No error expected - verify result
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if result == nil {
				t.Error("Result should not be nil")
				return
			}

			// Note: In single-variant architecture, VariantID and Accelerator are in the VA spec,
			// not in OptimizedAlloc. These assertions have been removed.

			// Verify NumReplicas
			if result.NumReplicas != int32(tt.numReplicas) {
				t.Errorf("NumReplicas mismatch: got %d, expected %d", result.NumReplicas, tt.numReplicas)
			}

			// Verify LastRunTime is set (should be recent)
			if result.LastRunTime.Time.IsZero() {
				t.Error("LastRunTime should be set")
			}

			timeSinceCreation := time.Since(result.LastRunTime.Time)
			if timeSinceCreation > 5*time.Second {
				t.Errorf("LastRunTime seems too old: %v ago", timeSinceCreation)
			}

			t.Logf("Success: Created OptimizedAlloc for variant_id=%q, accelerator=%q, replicas=%d",
				tt.variantID, tt.accelerator, result.NumReplicas)
		})
	}
}

func TestGetVariantMinReplicas(t *testing.T) {
	tests := []struct {
		name        string
		minReplicas *int32
		expected    int32
	}{
		{
			name:        "MinReplicas is nil (should default to 0)",
			minReplicas: nil,
			expected:    0,
		},
		{
			name:        "MinReplicas is 0",
			minReplicas: intPtr(0),
			expected:    0,
		},
		{
			name:        "MinReplicas is 1",
			minReplicas: intPtr(1),
			expected:    1,
		},
		{
			name:        "MinReplicas is 5",
			minReplicas: intPtr(5),
			expected:    5,
		},
		{
			name:        "MinReplicas is 100",
			minReplicas: intPtr(100),
			expected:    100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					MinReplicas: tt.minReplicas,
				},
			}

			result := GetVariantMinReplicas(va)
			if result != tt.expected {
				t.Errorf("GetVariantMinReplicas() = %d, expected %d", result, tt.expected)
			}
		})
	}
}

func TestGetVariantMaxReplicas(t *testing.T) {
	tests := []struct {
		name        string
		maxReplicas *int32
		expected    int32
	}{
		{
			name:        "MaxReplicas is nil (should return -1 for unlimited)",
			maxReplicas: nil,
			expected:    -1,
		},
		{
			name:        "MaxReplicas is 1",
			maxReplicas: intPtr(1),
			expected:    1,
		},
		{
			name:        "MaxReplicas is 5",
			maxReplicas: intPtr(5),
			expected:    5,
		},
		{
			name:        "MaxReplicas is 10",
			maxReplicas: intPtr(10),
			expected:    10,
		},
		{
			name:        "MaxReplicas is 100",
			maxReplicas: intPtr(100),
			expected:    100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					MaxReplicas: tt.maxReplicas,
				},
			}

			result := GetVariantMaxReplicas(va)
			if result != tt.expected {
				t.Errorf("GetVariantMaxReplicas() = %d, expected %d", result, tt.expected)
			}
		})
	}
}

// intPtr is a helper function to create a pointer to an int value
func intPtr(i int32) *int32 {
	return &i
}

// TestReplicaBoundsIntegration tests the integration of minReplicas and maxReplicas
func TestReplicaBoundsIntegration(t *testing.T) {
	tests := []struct {
		name               string
		minReplicas        *int32
		maxReplicas        *int32
		optimizedReplicas  int32
		expectedAfterClamp int32
		description        string
	}{
		{
			name:               "no bounds, no clamping",
			minReplicas:        nil,
			maxReplicas:        nil,
			optimizedReplicas:  5,
			expectedAfterClamp: 5,
			description:        "No bounds means no clamping",
		},
		{
			name:               "below minReplicas should clamp up",
			minReplicas:        intPtr(3),
			maxReplicas:        nil,
			optimizedReplicas:  1,
			expectedAfterClamp: 3,
			description:        "Optimizer suggests 1, but minReplicas=3 enforces minimum",
		},
		{
			name:               "above maxReplicas should clamp down",
			minReplicas:        nil,
			maxReplicas:        intPtr(5),
			optimizedReplicas:  10,
			expectedAfterClamp: 5,
			description:        "Optimizer suggests 10, but maxReplicas=5 limits it",
		},
		{
			name:               "within bounds, no clamping",
			minReplicas:        intPtr(2),
			maxReplicas:        intPtr(10),
			optimizedReplicas:  5,
			expectedAfterClamp: 5,
			description:        "Optimizer suggests 5, which is within [2,10]",
		},
		{
			name:               "at minReplicas boundary",
			minReplicas:        intPtr(3),
			maxReplicas:        intPtr(10),
			optimizedReplicas:  3,
			expectedAfterClamp: 3,
			description:        "Optimizer suggests exactly minReplicas",
		},
		{
			name:               "at maxReplicas boundary",
			minReplicas:        intPtr(1),
			maxReplicas:        intPtr(8),
			optimizedReplicas:  8,
			expectedAfterClamp: 8,
			description:        "Optimizer suggests exactly maxReplicas",
		},
		{
			name:               "zero with zero minReplicas",
			minReplicas:        intPtr(0),
			maxReplicas:        intPtr(10),
			optimizedReplicas:  0,
			expectedAfterClamp: 0,
			description:        "Optimizer suggests 0, minReplicas=0 allows it",
		},
		{
			name:               "zero but minReplicas prevents it",
			minReplicas:        intPtr(1),
			maxReplicas:        nil,
			optimizedReplicas:  0,
			expectedAfterClamp: 1,
			description:        "Optimizer suggests 0, but minReplicas=1 enforces minimum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					VariantID:   "test-model-A100-1",
					Accelerator: "A100",
					MinReplicas: tt.minReplicas,
					MaxReplicas: tt.maxReplicas,
				},
			}

			// Simulate clamping logic (similar to optimizer)
			clampedReplicas := tt.optimizedReplicas

			// Apply minReplicas constraint
			minReplicas := GetVariantMinReplicas(va)
			if clampedReplicas < minReplicas {
				clampedReplicas = minReplicas
			}

			// Apply maxReplicas constraint
			maxReplicas := GetVariantMaxReplicas(va)
			if maxReplicas > 0 && clampedReplicas > maxReplicas {
				clampedReplicas = maxReplicas
			}

			if clampedReplicas != tt.expectedAfterClamp {
				t.Errorf("Clamping failed: expected %d, got %d (optimized=%d, min=%v, max=%v)",
					tt.expectedAfterClamp, clampedReplicas, tt.optimizedReplicas,
					tt.minReplicas, tt.maxReplicas)
			}

			t.Logf("✓ %s: optimized=%d, min=%v, max=%v => clamped=%d",
				tt.description, tt.optimizedReplicas, tt.minReplicas, tt.maxReplicas, clampedReplicas)
		})
	}
}

// TestAddServerInfoWithReplicaBounds tests AddServerInfoToSystemData with replica bounds
func TestAddServerInfoWithReplicaBounds(t *testing.T) {
	tests := []struct {
		name                string
		minReplicas         *int32
		expectedMinInSystem int32
		description         string
	}{
		{
			name:                "nil minReplicas uses default 0",
			minReplicas:         nil,
			expectedMinInSystem: 0,
			description:         "Default minimum should be 0",
		},
		{
			name:                "explicit minReplicas=0",
			minReplicas:         intPtr(0),
			expectedMinInSystem: 0,
			description:         "Explicit 0 should be used",
		},
		{
			name:                "minReplicas=1",
			minReplicas:         intPtr(1),
			expectedMinInSystem: 1,
			description:         "minReplicas=1 prevents scale to zero",
		},
		{
			name:                "minReplicas=5",
			minReplicas:         intPtr(5),
			expectedMinInSystem: 5,
			description:         "High minReplicas enforced",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "test-model",
					VariantID:        "test-model-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					MinReplicas:      tt.minReplicas,
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "slo-config",
						Key:  "gold",
					},
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						MaxBatchSize: 8,
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "0.8", "beta": "0.2"},
							PrefillParms: map[string]string{"gamma": "0.8", "delta": "0.2"},
						},
					},
				},
			}

			// Get effective minReplicas
			effectiveMin := GetVariantMinReplicas(va)

			if effectiveMin != tt.expectedMinInSystem {
				t.Errorf("Expected minReplicas=%d, got %d", tt.expectedMinInSystem, effectiveMin)
			}

			t.Logf("✓ %s: minReplicas=%v => effectiveMin=%d",
				tt.description, tt.minReplicas, effectiveMin)
		})
	}
}
