package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
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
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "va-sample-deployment",
			},
			ModelID:     "model-123",
			MaxReplicas: 2,
		},
		Status: VariantAutoscalingStatus{
			// CurrentAlloc: Allocation{...} -- Removed
			DesiredOptimizedAlloc: OptimizedAlloc{
				LastRunTime: metav1.NewTime(time.Unix(1730000000, 0).UTC()),
				Accelerator: "nvidia.com/mig-1g.5gb",
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
	cp.Spec.ModelID = "model-456"
	// cp.Status.CurrentAlloc.Load.ArrivalRate = "20 rps" -- Removed

	if orig.Spec.ModelID == cp.Spec.ModelID {
		t.Errorf("DeepCopy did not create independent copy for Spec.ModelID")
	}

	// if orig.Status.CurrentAlloc.Load.ArrivalRate == cp.Status.CurrentAlloc.Load.ArrivalRate {
	// 	t.Errorf("DeepCopy did not create independent copy for nested Status.Load")
	// }
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
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "va-empty-status-deployment",
			},
			ModelID: "m",
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
			DesiredOptimizedAlloc struct {
				LastRunTime *string `json:"lastRunTime"`
				NumReplicas int     `json:"numReplicas"`
			} `json:"desiredOptimizedAlloc"`
			Actuation struct {
				Applied bool `json:"applied"`
			} `json:"actuation"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal probe failed: %v", err)
	}
	if probe.Status.DesiredOptimizedAlloc.NumReplicas != 0 ||
		probe.Status.Actuation.Applied != false {
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

	type optimizedAlloc struct {
		Status struct {
			DesiredOptimizedAlloc struct {
				LastRunTime string `json:"lastRunTime"`
			} `json:"desiredOptimizedAlloc"`
		} `json:"status"`
	}
	var probe optimizedAlloc
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

func TestMinMaxReplicasJSON(t *testing.T) {
	minVal := int32(2)
	va := &VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "va-replicas", Namespace: "default"},
		Spec: VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-deploy",
			},
			ModelID:     "model-x",
			MinReplicas: &minVal,
			MaxReplicas: 5,
		},
	}

	b, err := json.Marshal(va)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var probe struct {
		Spec struct {
			MinReplicas *int32 `json:"minReplicas"`
			MaxReplicas int32  `json:"maxReplicas"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if probe.Spec.MinReplicas == nil || *probe.Spec.MinReplicas != 2 {
		t.Errorf("expected minReplicas=2, got %v", probe.Spec.MinReplicas)
	}
	if probe.Spec.MaxReplicas != 5 {
		t.Errorf("expected maxReplicas=5, got %d", probe.Spec.MaxReplicas)
	}

	// minReplicas must be absent from JSON when nil (omitempty)
	vaNoMin := &VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "va-no-min", Namespace: "default"},
		Spec: VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "my-deploy",
			},
			ModelID:     "model-x",
			MaxReplicas: 5,
		},
	}
	b2, err := json.Marshal(vaNoMin)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var probeSpec struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(b2, &probeSpec); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := probeSpec.Spec["minReplicas"]; ok {
		t.Errorf("expected minReplicas to be absent when nil, but it was present")
	}
}
