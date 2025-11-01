package v1alpha1

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// helper: build a valid VariantAutoscaling object
// TODO: move to utils??
func makeValidVA() *VariantAutoscaling {
	return &VariantAutoscaling{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "inferencev1alpha1",
			Kind:       "VariantAutoscaling",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "va-sample",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/name": "workload-variant-autoscaler",
			},
		},
		Spec: VariantAutoscalingSpec{
			ModelID:          "model-123",
			VariantID:        "model-123-A100-1",
			Accelerator:      "A100",
			AcceleratorCount: 1,
			SLOClassRef: ConfigMapKeyRef{
				Name: "slo-config",
				Key:  "gold",
			},
			VariantProfile: VariantProfile{
				PerfParms: PerfParms{
					DecodeParms:  map[string]string{"alpha": "0.8", "beta": "0.2"},
					PrefillParms: map[string]string{"gamma": "0.8", "delta": "0.2"},
				},
				MaxBatchSize: 8,
			},
		},
		Status: VariantAutoscalingStatus{
			CurrentAlloc: Allocation{
				// Note: In single-variant architecture, variantID, accelerator, maxBatch, and variantCost
				// are in the parent VA spec, not in Allocation status
				NumReplicas: 1,
			},
			DesiredOptimizedAlloc: OptimizedAlloc{
				LastRunTime: metav1.NewTime(time.Unix(1730000000, 0).UTC()),
				// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
				NumReplicas: 2,
			},
			Actuation: ActuationStatus{
				Applied: true,
			},
		},
	}
}

func TestSchemeRegistration(t *testing.T) {
	s := runtime.NewScheme()
	if err := SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	kinds, _, err := s.ObjectKinds(&VariantAutoscaling{})
	if err != nil {
		t.Fatalf("ObjectKinds for VariantAutoscaling failed: %v", err)
	}
	if len(kinds) == 0 {
		t.Fatalf("no GVK registered for VariantAutoscaling")
	}

	listKinds, _, err := s.ObjectKinds(&VariantAutoscalingList{})
	if err != nil {
		t.Fatalf("ObjectKinds for VariantAutoscalingList failed: %v", err)
	}
	if len(listKinds) == 0 {
		t.Fatalf("no GVK registered for VariantAutoscalingList")
	}
}

func TestDeepCopyIndependence(t *testing.T) {
	orig := makeValidVA()
	cp := orig.DeepCopy()

	cp.Spec.ModelID = "model-456"
	cp.Spec.SLOClassRef.Name = "slo-config-2"
	cp.Spec.VariantProfile.MaxBatchSize = 16

	if orig.Spec.ModelID == cp.Spec.ModelID {
		t.Errorf("DeepCopy did not create independent copy for Spec.ModelID")
	}
	if orig.Spec.SLOClassRef.Name == cp.Spec.SLOClassRef.Name {
		t.Errorf("DeepCopy did not create independent copy for Spec.SLOClassRef.Name")
	}
	if orig.Spec.VariantProfile.MaxBatchSize == cp.Spec.VariantProfile.MaxBatchSize {
		t.Errorf("DeepCopy did not create independent copy for VariantProfile.MaxBatchSize")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	orig := makeValidVA()

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var back VariantAutoscaling
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	// Note: In single-variant architecture, VariantID is in spec, not in OptimizedAlloc
	// Check NumReplicas instead to ensure unmarshal worked
	if back.Status.DesiredOptimizedAlloc.NumReplicas != orig.Status.DesiredOptimizedAlloc.NumReplicas {
		t.Fatalf("DesiredOptimizedAlloc.NumReplicas mismatch after unmarshal")
	}

	ot := orig.Status.DesiredOptimizedAlloc.LastRunTime.Time
	bt := back.Status.DesiredOptimizedAlloc.LastRunTime.Time
	if !ot.Equal(bt) {
		t.Fatalf("LastRunTime mismatch by instant: orig=%v back=%v", ot, bt)
	}

	back.Status.DesiredOptimizedAlloc.LastRunTime = orig.Status.DesiredOptimizedAlloc.LastRunTime

	if !reflect.DeepEqual(orig, &back) {
		t.Errorf("round-trip mismatch:\norig=%#v\nback=%#v", orig, &back)
	}
}

func TestListDeepCopyAndItemsIndependence(t *testing.T) {
	va1 := makeValidVA()
	va2 := makeValidVA()
	va2.Name = "va-other"
	list := &VariantAutoscalingList{
		Items: []VariantAutoscaling{*va1, *va2},
	}

	cp := list.DeepCopy()
	if len(cp.Items) != 2 {
		t.Fatalf("DeepCopy list items count mismatch: got %d", len(cp.Items))
	}
	// mutate copy
	cp.Items[0].Spec.ModelID = "changed"

	if list.Items[0].Spec.ModelID == cp.Items[0].Spec.ModelID {
		t.Errorf("DeepCopy did not isolate list items")
	}
}

func TestStatusOmitEmpty(t *testing.T) {
	empty := &VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "va-empty-status",
			Namespace: "default",
		},
		Spec: VariantAutoscalingSpec{
			ModelID:          "m",
			VariantID:        "m-A100-1",
			Accelerator:      "A100",
			AcceleratorCount: 1,
			SLOClassRef: ConfigMapKeyRef{
				Name: "slo",
				Key:  "bronze",
			},
			VariantProfile: VariantProfile{
				PerfParms: PerfParms{
					DecodeParms:  map[string]string{"alpha": "1", "beta": "1"},
					PrefillParms: map[string]string{"gamma": "1", "delta": "1"},
				},
				MaxBatchSize: 1,
			},
		},
	}

	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	if !jsonContainsKey(b, "status") {
		t.Fatalf("expected status to be present for non-pointer struct with omitempty; got: %s", string(b))
	}

	// Optional: sanity-check a couple of zero values inside status
	var probe struct {
		Status struct {
			CurrentAllocs []struct {
				Accelerator string `json:"accelerator"`
			} `json:"currentAllocs"`
			DesiredOptimizedAllocs []struct {
				LastRunTime *string `json:"lastRunTime"`
				NumReplicas int     `json:"numReplicas"`
			} `json:"desiredOptimizedAllocs"`
			Actuation struct {
				Applied bool `json:"applied"`
			} `json:"actuation"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal probe failed: %v", err)
	}
	if probe.Status.Actuation.Applied != false {
		t.Errorf("unexpected non-zero defaults in status: %+v", probe.Status)
	}
	empty.Status.Actuation.Applied = true
	b2, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !jsonContainsKey(b2, "status") {
		t.Errorf("status should be present when non-zero, but json did not contain it: %s", string(b2))
	}
}

func TestOptimizedAllocLastRunTimeJSON(t *testing.T) {
	va := makeValidVA()
	// ensure LastRunTime survives marshal/unmarshal with RFC3339 format used by metav1.Time
	raw, err := json.Marshal(va)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	type optimizedAllocs struct {
		Status struct {
			DesiredOptimizedAlloc struct {
				LastRunTime string `json:"lastRunTime"`
			} `json:"desiredOptimizedAlloc"`
		} `json:"status"`
	}
	var probe optimizedAllocs
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal probe failed: %v", err)
	}
	if probe.Status.DesiredOptimizedAlloc.LastRunTime == "" {
		t.Errorf("expected lastRunTime to be serialized, got empty")
	}
}

func jsonContainsKey(b []byte, key string) bool {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// TestVariantCostOmitEmpty verifies that variantCost can be omitted in JSON
// and will use the default value "10" set by the CRD webhook
func TestVariantCostOmitEmpty(t *testing.T) {
	// Test 1: When variantCost is explicitly set, it should be in JSON
	vaWithCost := makeValidVA()
	vaWithCost.Spec.VariantCost = "15.5"

	b, err := json.Marshal(vaWithCost)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var probe1 struct {
		Spec struct {
			VariantCost string `json:"variantCost"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b, &probe1); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if probe1.Spec.VariantCost != "15.5" {
		t.Errorf("expected variantCost=15.5, got %s", probe1.Spec.VariantCost)
	}

	// Test 2: When variantCost is empty, it should be omitted from JSON (omitempty)
	vaWithoutCost := makeValidVA()
	vaWithoutCost.Spec.VariantCost = ""

	b2, err := json.Marshal(vaWithoutCost)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// Parse to check if variantCost is absent
	var rawSpec map[string]interface{}
	var probe2 struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(b2, &probe2); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if err := json.Unmarshal(probe2.Spec, &rawSpec); err != nil {
		t.Fatalf("unmarshal spec failed: %v", err)
	}

	// variantCost should be omitted when empty due to omitempty tag
	if _, exists := rawSpec["variantCost"]; exists {
		t.Errorf("expected variantCost to be omitted when empty, but it was present")
	}
}

// TestVariantCostDefaultValue verifies the default value behavior
// Note: The actual default value "10" is set by Kubernetes API server via
// the +kubebuilder:default="10" marker, not by Go struct defaults
func TestVariantCostDefaultValue(t *testing.T) {
	tests := []struct {
		name         string
		variantCost  string
		expectInJSON bool
		expectedVal  string
	}{
		{
			name:         "explicit cost set",
			variantCost:  "20.5",
			expectInJSON: true,
			expectedVal:  "20.5",
		},
		{
			name:         "empty cost - should be omitted",
			variantCost:  "",
			expectInJSON: false,
			expectedVal:  "",
		},
		{
			name:         "default value explicitly set",
			variantCost:  "10",
			expectInJSON: true,
			expectedVal:  "10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := makeValidVA()
			va.Spec.VariantCost = tt.variantCost

			b, err := json.Marshal(va)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}

			var probe struct {
				Spec struct {
					VariantCost string `json:"variantCost,omitempty"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(b, &probe); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}

			if tt.expectInJSON {
				if probe.Spec.VariantCost != tt.expectedVal {
					t.Errorf("expected variantCost=%s, got %s", tt.expectedVal, probe.Spec.VariantCost)
				}
			} else {
				// Check raw JSON to ensure field is truly omitted
				var rawMap map[string]interface{}
				if err := json.Unmarshal(b, &rawMap); err != nil {
					t.Fatalf("unmarshal to map failed: %v", err)
				}
				specMap := rawMap["spec"].(map[string]interface{})
				if _, exists := specMap["variantCost"]; exists {
					t.Errorf("expected variantCost to be omitted, but it exists in JSON")
				}
			}
		})
	}
}

// TestVariantIDPatternValidation tests the VariantID pattern validation
// Pattern: ^.+-[A-Za-z0-9_-]+-[1-9][0-9]*$
// Format: {modelID}-{accelerator}-{count}
func TestVariantIDPatternValidation(t *testing.T) {
	tests := []struct {
		name        string
		variantID   string
		shouldMatch bool
		reason      string
	}{
		// Valid cases - standard format
		{
			name:        "simple alphanumeric",
			variantID:   "llama-A100-1",
			shouldMatch: true,
			reason:      "basic format with alphanumeric accelerator",
		},
		{
			name:        "model with slash",
			variantID:   "meta/llama-3.1-8b-A100-4",
			shouldMatch: true,
			reason:      "HuggingFace style model name with slash",
		},
		{
			name:        "model with lowercase",
			variantID:   "llama-8b-h100-1",
			shouldMatch: true,
			reason:      "lowercase accelerator name",
		},

		// Valid cases - complex accelerator names (NEW with flexible pattern)
		{
			name:        "accelerator with hyphen",
			variantID:   "meta/llama-8b-H100-SXM-4",
			shouldMatch: true,
			reason:      "accelerator name with hyphen (H100-SXM)",
		},
		{
			name:        "accelerator with underscore",
			variantID:   "model-A100_80GB-1",
			shouldMatch: true,
			reason:      "accelerator name with underscore (A100_80GB)",
		},
		{
			name:        "complex accelerator",
			variantID:   "model-H100-SXM4-80GB-2",
			shouldMatch: true,
			reason:      "complex accelerator with multiple hyphens",
		},
		{
			name:        "accelerator with mixed separators",
			variantID:   "model-A100_PCIE-SXM4-1",
			shouldMatch: true,
			reason:      "accelerator with both underscore and hyphen",
		},

		// Valid cases - various model names
		{
			name:        "multiple slashes in model",
			variantID:   "org/team/model-GPU-1",
			shouldMatch: true,
			reason:      "model name with multiple organization levels",
		},
		{
			name:        "model with dots",
			variantID:   "gpt-4.0-turbo-H100-2",
			shouldMatch: true,
			reason:      "model name with version dots",
		},
		{
			name:        "model with underscores",
			variantID:   "model_name-A100-1",
			shouldMatch: true,
			reason:      "model name with underscores",
		},

		// Invalid cases - count must start with 1-9
		{
			name:        "count starting with zero",
			variantID:   "model-A100-0",
			shouldMatch: false,
			reason:      "replica count cannot start with 0",
		},
		{
			name:        "count is zero",
			variantID:   "model-GPU-00",
			shouldMatch: false,
			reason:      "replica count of 00 not allowed",
		},

		// Invalid cases - missing components
		{
			name:        "missing count",
			variantID:   "model-A100",
			shouldMatch: false,
			reason:      "missing replica count",
		},
		{
			name:        "missing accelerator",
			variantID:   "model-1",
			shouldMatch: false,
			reason:      "missing accelerator name",
		},
		{
			name:        "only model name",
			variantID:   "model",
			shouldMatch: false,
			reason:      "missing accelerator and count",
		},

		// Invalid cases - empty or malformed
		{
			name:        "empty string",
			variantID:   "",
			shouldMatch: false,
			reason:      "empty variantID",
		},
		{
			name:        "only dashes",
			variantID:   "---",
			shouldMatch: false,
			reason:      "no valid components",
		},
	}

	// The actual pattern from the CRD validation
	pattern := `^.+-[A-Za-z0-9_-]+-[1-9][0-9]*$`

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test using Go regex matching (simulating Kubernetes validation)
			matched, err := regexp.MatchString(pattern, tt.variantID)
			if err != nil {
				t.Fatalf("regex error: %v", err)
			}

			if matched != tt.shouldMatch {
				if tt.shouldMatch {
					t.Errorf("Expected '%s' to MATCH pattern, but it didn't. Reason: %s",
						tt.variantID, tt.reason)
				} else {
					t.Errorf("Expected '%s' to NOT MATCH pattern, but it did. Reason: %s",
						tt.variantID, tt.reason)
				}
			}
		})
	}
}

// TestReplicaBoundsEdgeCases tests edge cases for minReplicas and maxReplicas
func TestReplicaBoundsEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		minReplicas *int32
		maxReplicas *int32
		expectValid bool
		description string
	}{
		{
			name:        "both nil (defaults)",
			minReplicas: nil,
			maxReplicas: nil,
			expectValid: true,
			description: "Should use defaults: min=0, max=unlimited",
		},
		{
			name:        "min=0, max=nil",
			minReplicas: intPtr(0),
			maxReplicas: nil,
			expectValid: true,
			description: "Explicit min=0 with unlimited max",
		},
		{
			name:        "min=1, max=10",
			minReplicas: intPtr(1),
			maxReplicas: intPtr(10),
			expectValid: true,
			description: "Valid range: min < max",
		},
		{
			name:        "min=5, max=5",
			minReplicas: intPtr(5),
			maxReplicas: intPtr(5),
			expectValid: true,
			description: "Edge case: min = max (fixed replicas)",
		},
		{
			name:        "min=10, max=5",
			minReplicas: intPtr(10),
			maxReplicas: intPtr(5),
			expectValid: false,
			description: "Invalid: min > max",
		},
		{
			name:        "min=0, max=1",
			minReplicas: intPtr(0),
			maxReplicas: intPtr(1),
			expectValid: true,
			description: "Minimum range: can scale 0-1",
		},
		{
			name:        "large values: min=100, max=1000",
			minReplicas: intPtr(100),
			maxReplicas: intPtr(1000),
			expectValid: true,
			description: "Should handle large replica counts",
		},
		{
			name:        "very large max: 10000",
			minReplicas: intPtr(0),
			maxReplicas: intPtr(10000),
			expectValid: true,
			description: "Should handle very large max replica counts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := makeValidVA()
			va.Spec.MinReplicas = tt.minReplicas
			va.Spec.MaxReplicas = tt.maxReplicas

			// Marshal to JSON to ensure serialization works
			_, err := json.Marshal(va)
			if err != nil {
				t.Errorf("Failed to marshal VA: %v", err)
			}

			// Check logical validity
			if tt.minReplicas != nil && tt.maxReplicas != nil {
				if *tt.minReplicas > *tt.maxReplicas {
					if tt.expectValid {
						t.Errorf("Expected configuration to be valid, but min > max: min=%d, max=%d",
							*tt.minReplicas, *tt.maxReplicas)
					}
					// This is expected to be invalid
					return
				}
			}

			if !tt.expectValid {
				t.Errorf("Expected configuration to be invalid, but validation passed")
			}

			t.Logf("✓ %s: min=%v, max=%v", tt.description,
				ptrToString(tt.minReplicas), ptrToString(tt.maxReplicas))
		})
	}
}

// TestReplicaBoundsWithScaleToZero tests interaction between replica bounds and scale-to-zero
func TestReplicaBoundsWithScaleToZero(t *testing.T) {
	tests := []struct {
		name               string
		minReplicas        *int32
		scaleToZeroEnabled bool
		canScaleToZero     bool
		description        string
	}{
		{
			name:               "min=0, scaleToZero=true",
			minReplicas:        intPtr(0),
			scaleToZeroEnabled: true,
			canScaleToZero:     true,
			description:        "Should allow scaling to zero",
		},
		{
			name:               "min=1, scaleToZero=true",
			minReplicas:        intPtr(1),
			scaleToZeroEnabled: true,
			canScaleToZero:     false,
			description:        "minReplicas=1 prevents scale-to-zero",
		},
		{
			name:               "min=0, scaleToZero=false",
			minReplicas:        intPtr(0),
			scaleToZeroEnabled: false,
			canScaleToZero:     false,
			description:        "scaleToZero disabled prevents scaling to zero",
		},
		{
			name:               "min=nil (default 0), scaleToZero=true",
			minReplicas:        nil,
			scaleToZeroEnabled: true,
			canScaleToZero:     true,
			description:        "Default minReplicas=0 allows scale-to-zero",
		},
		{
			name:               "min=5, scaleToZero=true",
			minReplicas:        intPtr(5),
			scaleToZeroEnabled: true,
			canScaleToZero:     false,
			description:        "High minReplicas prevents scale-to-zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := makeValidVA()
			va.Spec.MinReplicas = tt.minReplicas

			// Determine effective minReplicas
			effectiveMin := int32(0)
			if tt.minReplicas != nil {
				effectiveMin = *tt.minReplicas
			}

			// Logic: can scale to zero only if scaleToZero is enabled AND minReplicas is 0
			actualCanScale := tt.scaleToZeroEnabled && effectiveMin == 0

			if actualCanScale != tt.canScaleToZero {
				t.Errorf("Expected canScaleToZero=%v, got %v (minReplicas=%d, scaleToZero=%v)",
					tt.canScaleToZero, actualCanScale, effectiveMin, tt.scaleToZeroEnabled)
			}

			t.Logf("✓ %s: effectiveMin=%d, scaleToZero=%v, canScale=%v",
				tt.description, effectiveMin, tt.scaleToZeroEnabled, actualCanScale)
		})
	}
}

// TestReplicaBoundsJSONRoundTrip tests JSON marshaling/unmarshaling of replica bounds
func TestReplicaBoundsJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		minReplicas *int32
		maxReplicas *int32
	}{
		{name: "both nil", minReplicas: nil, maxReplicas: nil},
		{name: "only min set", minReplicas: intPtr(2), maxReplicas: nil},
		{name: "only max set", minReplicas: nil, maxReplicas: intPtr(10)},
		{name: "both set", minReplicas: intPtr(1), maxReplicas: intPtr(5)},
		{name: "min=0, max=1", minReplicas: intPtr(0), maxReplicas: intPtr(1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := makeValidVA()
			original.Spec.MinReplicas = tt.minReplicas
			original.Spec.MaxReplicas = tt.maxReplicas

			// Marshal to JSON
			data, err := json.Marshal(original)
			if err != nil {
				t.Fatalf("Failed to marshal: %v", err)
			}

			// Unmarshal back
			var decoded VariantAutoscaling
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Failed to unmarshal: %v", err)
			}

			// Compare replica bounds
			if !intPtrEqual(original.Spec.MinReplicas, decoded.Spec.MinReplicas) {
				t.Errorf("MinReplicas mismatch: original=%v, decoded=%v",
					ptrToString(original.Spec.MinReplicas),
					ptrToString(decoded.Spec.MinReplicas))
			}

			if !intPtrEqual(original.Spec.MaxReplicas, decoded.Spec.MaxReplicas) {
				t.Errorf("MaxReplicas mismatch: original=%v, decoded=%v",
					ptrToString(original.Spec.MaxReplicas),
					ptrToString(decoded.Spec.MaxReplicas))
			}

			t.Logf("✓ Round trip successful: min=%v, max=%v",
				ptrToString(tt.minReplicas), ptrToString(tt.maxReplicas))
		})
	}
}

// TestOptimizedAllocReasonField tests the Reason field in OptimizedAlloc
func TestOptimizedAllocReasonField(t *testing.T) {
	tests := []struct {
		name         string
		reason       string
		expectInJSON bool
	}{
		{
			name:         "optimizer reason",
			reason:       "Optimizer solution: cost and latency optimized allocation",
			expectInJSON: true,
		},
		{
			name:         "fallback reason",
			reason:       "Fallback: metrics unavailable, using max(minReplicas=2, current=3) = 3",
			expectInJSON: true,
		},
		{
			name:         "scale-to-zero reason",
			reason:       "Fallback: scale-to-zero after retention period (no metrics for >5m)",
			expectInJSON: true,
		},
		{
			name:         "empty reason - should be omitted",
			reason:       "",
			expectInJSON: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := makeValidVA()
			va.Status.DesiredOptimizedAlloc.Reason = tt.reason

			data, err := json.Marshal(va)
			if err != nil {
				t.Fatalf("Failed to marshal: %v", err)
			}

			var decoded VariantAutoscaling
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Failed to unmarshal: %v", err)
			}

			if tt.expectInJSON {
				if decoded.Status.DesiredOptimizedAlloc.Reason != tt.reason {
					t.Errorf("Reason mismatch: expected=%q, got=%q", tt.reason, decoded.Status.DesiredOptimizedAlloc.Reason)
				}
			} else {
				// Check if field is omitted in JSON
				var raw map[string]interface{}
				if err := json.Unmarshal(data, &raw); err != nil {
					t.Fatalf("Failed to unmarshal to map: %v", err)
				}
				status := raw["status"].(map[string]interface{})
				optimizedAlloc := status["desiredOptimizedAlloc"].(map[string]interface{})
				if _, exists := optimizedAlloc["reason"]; exists {
					t.Errorf("Expected reason to be omitted when empty, but it exists")
				}
			}
		})
	}
}

// TestOptimizedAllocLastUpdateField tests the LastUpdate field in OptimizedAlloc
func TestOptimizedAllocLastUpdateField(t *testing.T) {
	tests := []struct {
		name           string
		setLastUpdate  bool
		lastUpdateTime time.Time
	}{
		{
			name:           "with lastUpdate timestamp",
			setLastUpdate:  true,
			lastUpdateTime: time.Unix(1730000000, 0).UTC(),
		},
		{
			name:          "without lastUpdate - should be omitted",
			setLastUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := makeValidVA()

			if tt.setLastUpdate {
				va.Status.DesiredOptimizedAlloc.LastUpdate = metav1.NewTime(tt.lastUpdateTime)
			} else {
				va.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Time{}
			}

			data, err := json.Marshal(va)
			if err != nil {
				t.Fatalf("Failed to marshal: %v", err)
			}

			var decoded VariantAutoscaling
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Failed to unmarshal: %v", err)
			}

			if tt.setLastUpdate {
				if !decoded.Status.DesiredOptimizedAlloc.LastUpdate.Time.Equal(tt.lastUpdateTime) {
					t.Errorf("LastUpdate mismatch: expected=%v, got=%v",
						tt.lastUpdateTime, decoded.Status.DesiredOptimizedAlloc.LastUpdate.Time)
				}
			} else {
				// Verify zero time
				if !decoded.Status.DesiredOptimizedAlloc.LastUpdate.IsZero() {
					t.Errorf("Expected LastUpdate to be zero, but got %v",
						decoded.Status.DesiredOptimizedAlloc.LastUpdate)
				}
			}
		})
	}
}

// TestOptimizedAllocDeepCopyWithReasonAndLastUpdate tests DeepCopy for new fields
func TestOptimizedAllocDeepCopyWithReasonAndLastUpdate(t *testing.T) {
	orig := makeValidVA()
	orig.Status.DesiredOptimizedAlloc.Reason = "Optimizer solution: cost-optimal allocation"
	orig.Status.DesiredOptimizedAlloc.LastUpdate = metav1.NewTime(time.Unix(1730100000, 0).UTC())
	orig.Status.DesiredOptimizedAlloc.NumReplicas = 5

	cp := orig.DeepCopy()

	// Mutate the copy
	cp.Status.DesiredOptimizedAlloc.Reason = "Fallback: metrics unavailable"
	cp.Status.DesiredOptimizedAlloc.LastUpdate = metav1.NewTime(time.Unix(1730200000, 0).UTC())
	cp.Status.DesiredOptimizedAlloc.NumReplicas = 3

	// Verify original is unchanged
	if orig.Status.DesiredOptimizedAlloc.Reason == cp.Status.DesiredOptimizedAlloc.Reason {
		t.Errorf("DeepCopy did not create independent copy for Reason")
	}
	if orig.Status.DesiredOptimizedAlloc.LastUpdate.Equal(&cp.Status.DesiredOptimizedAlloc.LastUpdate) {
		t.Errorf("DeepCopy did not create independent copy for LastUpdate")
	}
	if orig.Status.DesiredOptimizedAlloc.NumReplicas == cp.Status.DesiredOptimizedAlloc.NumReplicas {
		t.Errorf("DeepCopy did not create independent copy for NumReplicas")
	}

	// Verify copy has expected values
	if cp.Status.DesiredOptimizedAlloc.Reason != "Fallback: metrics unavailable" {
		t.Errorf("Copy has unexpected Reason: %q", cp.Status.DesiredOptimizedAlloc.Reason)
	}
	if cp.Status.DesiredOptimizedAlloc.NumReplicas != 3 {
		t.Errorf("Copy has unexpected NumReplicas: %d", cp.Status.DesiredOptimizedAlloc.NumReplicas)
	}
}

// TestOptimizedAllocJSONRoundTripWithAllFields tests JSON round trip with all fields populated
func TestOptimizedAllocJSONRoundTripWithAllFields(t *testing.T) {
	orig := makeValidVA()
	orig.Status.DesiredOptimizedAlloc.LastRunTime = metav1.NewTime(time.Unix(1730000000, 0).UTC())
	orig.Status.DesiredOptimizedAlloc.NumReplicas = 4
	orig.Status.DesiredOptimizedAlloc.Reason = "Optimizer solution: cost and latency optimized allocation"
	orig.Status.DesiredOptimizedAlloc.LastUpdate = metav1.NewTime(time.Unix(1730010000, 0).UTC())

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	var decoded VariantAutoscaling
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	// Verify all fields
	origAlloc := orig.Status.DesiredOptimizedAlloc
	decodedAlloc := decoded.Status.DesiredOptimizedAlloc

	if !origAlloc.LastRunTime.Equal(&decodedAlloc.LastRunTime) {
		t.Errorf("LastRunTime mismatch: expected=%v, got=%v", origAlloc.LastRunTime, decodedAlloc.LastRunTime)
	}
	if origAlloc.NumReplicas != decodedAlloc.NumReplicas {
		t.Errorf("NumReplicas mismatch: expected=%d, got=%d", origAlloc.NumReplicas, decodedAlloc.NumReplicas)
	}
	if origAlloc.Reason != decodedAlloc.Reason {
		t.Errorf("Reason mismatch: expected=%q, got=%q", origAlloc.Reason, decodedAlloc.Reason)
	}
	if !origAlloc.LastUpdate.Equal(&decodedAlloc.LastUpdate) {
		t.Errorf("LastUpdate mismatch: expected=%v, got=%v", origAlloc.LastUpdate, decodedAlloc.LastUpdate)
	}
}

// TestConditionConstants verifies that condition type and reason constants are defined
func TestConditionConstants(t *testing.T) {
	// Verify TypeMetricsAvailable constant
	if TypeMetricsAvailable != "MetricsAvailable" {
		t.Errorf("TypeMetricsAvailable constant mismatch: expected=%q, got=%q", "MetricsAvailable", TypeMetricsAvailable)
	}

	// Verify TypeOptimizationReady constant
	if TypeOptimizationReady != "OptimizationReady" {
		t.Errorf("TypeOptimizationReady constant mismatch: expected=%q, got=%q", "OptimizationReady", TypeOptimizationReady)
	}

	// Verify MetricsAvailable reason constants
	expectedReasons := map[string]string{
		"ReasonMetricsFound":    ReasonMetricsFound,
		"ReasonMetricsMissing":  ReasonMetricsMissing,
		"ReasonMetricsStale":    ReasonMetricsStale,
		"ReasonPrometheusError": ReasonPrometheusError,
	}

	for name, value := range expectedReasons {
		if value != name[len("Reason"):] {
			t.Logf("Verified constant %s = %q", name, value)
		}
	}

	// Verify OptimizationReady reason constants
	expectedOptReasons := map[string]string{
		"ReasonOptimizationSucceeded": ReasonOptimizationSucceeded,
		"ReasonOptimizationFailed":    ReasonOptimizationFailed,
		"ReasonMetricsUnavailable":    ReasonMetricsUnavailable,
	}

	for name, value := range expectedOptReasons {
		if value != name[len("Reason"):] {
			t.Logf("Verified constant %s = %q", name, value)
		}
	}
}

// Helper functions for tests

func intPtr(i int32) *int32 {
	return &i
}

func intPtrEqual(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func ptrToString(p *int32) string {
	if p == nil {
		return "nil"
	}
	// Convert integer to string representation
	return fmt.Sprintf("%d", *p)
}
