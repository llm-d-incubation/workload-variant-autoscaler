package controller

import (
	"context"
	"fmt"
	"testing"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestStatusUpdatePreservesReasonAndLastUpdate tests the complete controller flow
// from allocation to status update, verifying Reason and LastUpdate are preserved.
func TestStatusUpdatePreservesReasonAndLastUpdate(t *testing.T) {
	fmt.Println("\n========== Testing Status Update with Real Controller Flow ==========")

	// Initialize logger
	if _, err := logger.InitLogger(); err != nil {
		t.Fatalf("Failed to initialize logger: %v", err)
	}

	// Setup
	scheme := runtime.NewScheme()
	_ = llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)

	modelID := "model-a"

	// Create initial VA object (simulating what's in the API)
	initialVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-variant",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ModelID:          modelID,
			VariantID:        "variant-1",
			Accelerator:      "nvidia-l4",
			AcceleratorCount: 1,
			MinReplicas:      nil,
			MaxReplicas:      nil,
		},
		Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
			CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
				NumReplicas: 1,
			},
			DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
				NumReplicas: 0,
				Reason:      "",
				LastUpdate:  metav1.Time{},
			},
		},
	}

	// Create fake client with the initial object
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initialVA).
		WithStatusSubresource(initialVA).
		Build()

	ctx := context.Background()
	allVariants := []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
	scaleToZeroConfigData := make(utils.ScaleToZeroConfigData)

	// STEP 1: Fetch fresh VA (simulating line 1188 in controller)
	fmt.Println("\n=== STEP 1: Fetching fresh VA from API ===")
	var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	err := fakeClient.Get(ctx, client.ObjectKey{Name: "test-variant", Namespace: "default"}, &updateVa)
	if err != nil {
		t.Fatalf("Failed to get VA: %v", err)
	}

	fmt.Printf("Initial Reason: %q\n", updateVa.Status.DesiredOptimizedAlloc.Reason)
	fmt.Printf("Initial LastUpdate: %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate)
	fmt.Printf("Initial LastUpdate.IsZero(): %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero())

	// Keep a copy of the original VA for updateConditionsForAllocation
	va := updateVa.DeepCopy()

	// STEP 2: Apply fallback allocation (PATH 3)
	fmt.Println("\n=== STEP 2: Applying fallback allocation (PATH 3) ===")
	hasOptimizedAlloc := false

	applyFallbackAllocation(&updateVa, allVariants, scaleToZeroConfigData, false, "Last resort")

	fmt.Printf("After applyFallbackAllocation:\n")
	fmt.Printf("  NumReplicas: %d\n", updateVa.Status.DesiredOptimizedAlloc.NumReplicas)
	fmt.Printf("  Reason: %q\n", updateVa.Status.DesiredOptimizedAlloc.Reason)
	fmt.Printf("  LastUpdate: %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate)
	fmt.Printf("  LastUpdate.IsZero(): %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero())

	if updateVa.Status.DesiredOptimizedAlloc.Reason == "" {
		t.Errorf("BUG: Reason is empty after applyFallbackAllocation!")
	}
	if updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero() {
		t.Errorf("BUG: LastUpdate is zero after applyFallbackAllocation!")
	}

	reasonAfterAllocation := updateVa.Status.DesiredOptimizedAlloc.Reason
	lastUpdateAfterAllocation := updateVa.Status.DesiredOptimizedAlloc.LastUpdate

	// STEP 3: Set LastRunTime (line 1311 in controller)
	fmt.Println("\n=== STEP 3: Setting LastRunTime ===")
	updateVa.Status.DesiredOptimizedAlloc.LastRunTime = metav1.Now()

	fmt.Printf("After setting LastRunTime:\n")
	fmt.Printf("  Reason: %q\n", updateVa.Status.DesiredOptimizedAlloc.Reason)
	fmt.Printf("  LastUpdate: %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate)

	if updateVa.Status.DesiredOptimizedAlloc.Reason != reasonAfterAllocation {
		t.Errorf("BUG: Reason changed after setting LastRunTime! Was %q, now %q", reasonAfterAllocation, updateVa.Status.DesiredOptimizedAlloc.Reason)
	}

	// STEP 4: Apply safety net (lines 1315-1331 in controller)
	fmt.Println("\n=== STEP 4: Applying safety net ===")
	if updateVa.Status.DesiredOptimizedAlloc.Reason == "" {
		updateVa.Status.DesiredOptimizedAlloc.Reason = "Fallback: allocation set but reason missing (controller bug)"
		updateVa.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Now()
		fmt.Println("  Safety net triggered: Set default Reason")
	}
	if updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero() && updateVa.Status.DesiredOptimizedAlloc.NumReplicas >= 0 {
		updateVa.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Now()
		fmt.Println("  Safety net triggered: Set LastUpdate")
	}

	fmt.Printf("After safety net:\n")
	fmt.Printf("  Reason: %q\n", updateVa.Status.DesiredOptimizedAlloc.Reason)
	fmt.Printf("  LastUpdate: %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate)

	// STEP 5: Update conditions (line 1335 in controller)
	fmt.Println("\n=== STEP 5: Calling updateConditionsForAllocation ===")
	updateConditionsForAllocation(&updateVa, va, hasOptimizedAlloc)

	fmt.Printf("After updateConditionsForAllocation:\n")
	fmt.Printf("  Reason: %q\n", updateVa.Status.DesiredOptimizedAlloc.Reason)
	fmt.Printf("  LastUpdate: %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate)
	fmt.Printf("  LastUpdate.IsZero(): %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero())

	// CRITICAL CHECK: Did updateConditionsForAllocation corrupt the fields?
	if updateVa.Status.DesiredOptimizedAlloc.Reason != reasonAfterAllocation {
		t.Errorf("BUG FOUND: Reason changed after updateConditionsForAllocation!\n  Before: %q\n  After: %q",
			reasonAfterAllocation, updateVa.Status.DesiredOptimizedAlloc.Reason)
	}
	if updateVa.Status.DesiredOptimizedAlloc.LastUpdate != lastUpdateAfterAllocation {
		t.Errorf("BUG FOUND: LastUpdate changed after updateConditionsForAllocation!\n  Before: %v\n  After: %v",
			lastUpdateAfterAllocation, updateVa.Status.DesiredOptimizedAlloc.LastUpdate)
	}

	// Verify condition message contains the Reason
	optCondition := llmdVariantAutoscalingV1alpha1.GetCondition(&updateVa, llmdVariantAutoscalingV1alpha1.TypeOptimizationReady)
	if optCondition == nil {
		t.Fatal("Condition is nil!")
	}
	fmt.Printf("\nCondition Message: %q\n", optCondition.Message)

	if updateVa.Status.DesiredOptimizedAlloc.Reason == "" {
		t.Errorf("BUG: Reason is EMPTY but condition message is: %q", optCondition.Message)
	}

	// STEP 6: Save to API using Status().Update()
	fmt.Println("\n=== STEP 6: Updating status via API ===")
	fmt.Printf("Before Status().Update():\n")
	fmt.Printf("  Reason: %q\n", updateVa.Status.DesiredOptimizedAlloc.Reason)
	fmt.Printf("  LastUpdate: %v\n", updateVa.Status.DesiredOptimizedAlloc.LastUpdate)
	fmt.Printf("  ResourceVersion: %s\n", updateVa.ResourceVersion)

	err = fakeClient.Status().Update(ctx, &updateVa)
	if err != nil {
		t.Fatalf("Status update failed: %v", err)
	}

	fmt.Println("Status().Update() completed successfully")

	// STEP 7: Fetch from API and verify what was saved
	fmt.Println("\n=== STEP 7: Fetching VA from API to verify what was saved ===")
	var savedVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-variant", Namespace: "default"}, &savedVa)
	if err != nil {
		t.Fatalf("Failed to get VA after update: %v", err)
	}

	fmt.Printf("Saved in API:\n")
	fmt.Printf("  NumReplicas: %d\n", savedVa.Status.DesiredOptimizedAlloc.NumReplicas)
	fmt.Printf("  Reason: %q\n", savedVa.Status.DesiredOptimizedAlloc.Reason)
	fmt.Printf("  LastUpdate: %v\n", savedVa.Status.DesiredOptimizedAlloc.LastUpdate)
	fmt.Printf("  LastUpdate.IsZero(): %v\n", savedVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero())

	// FINAL VERIFICATION
	fmt.Println("\n=== FINAL VERIFICATION ===")

	if savedVa.Status.DesiredOptimizedAlloc.Reason == "" {
		t.Errorf("🐛 BUG CONFIRMED: Reason is EMPTY in saved VA!")
		t.Errorf("   Expected: %q", reasonAfterAllocation)
		t.Errorf("   Got: %q", savedVa.Status.DesiredOptimizedAlloc.Reason)
	} else {
		fmt.Printf("✅ Reason preserved: %q\n", savedVa.Status.DesiredOptimizedAlloc.Reason)
	}

	if savedVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero() {
		t.Errorf("🐛 BUG CONFIRMED: LastUpdate is ZERO in saved VA!")
		t.Errorf("   Expected: %v", lastUpdateAfterAllocation)
		t.Errorf("   Got: %v", savedVa.Status.DesiredOptimizedAlloc.LastUpdate)
	} else {
		fmt.Printf("✅ LastUpdate preserved: %v\n", savedVa.Status.DesiredOptimizedAlloc.LastUpdate)
	}

	// Check the condition
	savedCondition := llmdVariantAutoscalingV1alpha1.GetCondition(&savedVa, llmdVariantAutoscalingV1alpha1.TypeOptimizationReady)
	if savedCondition != nil {
		fmt.Printf("✅ Condition Message: %q\n", savedCondition.Message)
	}

	fmt.Println("\n========== Test Complete ==========")
}
