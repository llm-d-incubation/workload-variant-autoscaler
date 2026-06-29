package gpurebalance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// stubPromAPI implements promv1.API with only Query stubbed.
// Pool→queue values are matched by searching the query string for the pool name.
type stubPromAPI struct {
	promv1.API
	queues map[string]float64
	errs   map[string]error
}

func (s *stubPromAPI) Query(_ context.Context, query string, _ time.Time, _ ...promv1.Option) (model.Value, promv1.Warnings, error) {
	for pool, err := range s.errs {
		if strings.Contains(query, pool) {
			return nil, nil, err
		}
	}
	for pool, q := range s.queues {
		if strings.Contains(query, pool) {
			return model.Vector{{Value: model.SampleValue(q)}}, nil, nil
		}
	}
	return model.Vector{}, nil, nil
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func makeHPA(name, ns, pool string, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	ann := map[string]string{}
	if pool != "" {
		ann[AnnotationInferencePool] = pool
	}
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: ann,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MaxReplicas: maxReplicas,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       name + "-deploy",
			},
		},
	}
}

func makeGPUQuota(name, ns string, gpus int64) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceName(gpuQuotaResource): *resource.NewQuantity(gpus, resource.DecimalSI),
			},
		},
	}
}

func newPlugin(t *testing.T, queues map[string]float64, errs map[string]error, objs ...client.Object) (*Plugin, client.Client) {
	t.Helper()
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return New(c, &stubPromAPI{queues: queues, errs: errs}), c
}

func getMaxReplicas(t *testing.T, c client.Client, name, ns string) int32 {
	t.Helper()
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, hpa); err != nil {
		t.Fatalf("Get HPA %s/%s: %v", ns, name, err)
	}
	return hpa.Spec.MaxReplicas
}

// TestTick_EmptySelectionIsNoop verifies that an empty selected slice returns
// nil without touching the cluster.
func TestTick_EmptySelectionIsNoop(t *testing.T) {
	p, _ := newPlugin(t, nil, nil)
	if err := p.Tick(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestTick_SkipsNonHPAObjects verifies that objects that are not HPAs are
// silently skipped.
func TestTick_SkipsNonHPAObjects(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}}
	p, _ := newPlugin(t, nil, nil)
	if err := p.Tick(context.Background(), []client.Object{pod}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestTick_SkipsUnannotatedHPA verifies that an HPA without the
// llm-d.ai/epp-inference-pool annotation is skipped and never patched.
func TestTick_SkipsUnannotatedHPA(t *testing.T) {
	hpa := makeHPA("model-a", "ns", "" /* no pool */, 10)
	p, c := newPlugin(t, map[string]float64{"model-a": 100}, nil, hpa, makeGPUQuota("q", "ns", 10))

	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 10 {
		t.Errorf("maxReplicas changed to %d, want 10 (unannotated HPA should be skipped)", got)
	}
}

// TestTick_NoQuota_Skips verifies that when no ResourceQuota exists in a
// namespace, the plugin skips rebalancing and leaves HPAs unchanged.
func TestTick_NoQuota_Skips(t *testing.T) {
	hpa := makeHPA("model-a", "ns", "model-a", 10)
	// No ResourceQuota added to the cluster.
	p, c := newPlugin(t, map[string]float64{"model-a": 100}, nil, hpa)

	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 10 {
		t.Errorf("maxReplicas changed to %d, want 10 (no quota → skip)", got)
	}
}

// TestRebalance_SinglePool verifies that a single annotated HPA receives the
// entire GPU quota.
func TestRebalance_SinglePool(t *testing.T) {
	hpa := makeHPA("model-a", "ns", "model-a", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 100},
		nil,
		hpa, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpa}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 10 {
		t.Errorf("maxReplicas = %d, want 10", got)
	}
}

// TestRebalance_EqualQueues verifies that two pools with the same queue depth
// each receive half the quota.
func TestRebalance_EqualQueues(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	hpaB := makeHPA("model-b", "ns", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 100, "model-b": 100},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 5 {
		t.Errorf("model-a maxReplicas = %d, want 5", got)
	}
	if got := getMaxReplicas(t, c, "model-b", "ns"); got != 5 {
		t.Errorf("model-b maxReplicas = %d, want 5", got)
	}
}

// TestRebalance_ProportionalQueues verifies that when queue depths differ, the
// allocation is proportional: 70% queue → 70% replicas (7/10), 30% → 3/10.
func TestRebalance_ProportionalQueues(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	hpaB := makeHPA("model-b", "ns", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 70, "model-b": 30},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 7 {
		t.Errorf("model-a maxReplicas = %d, want 7", got)
	}
	if got := getMaxReplicas(t, c, "model-b", "ns"); got != 3 {
		t.Errorf("model-b maxReplicas = %d, want 3", got)
	}
}

// TestRebalance_AllQueuesZero verifies that when all queues are zero the
// quota is split equally.
func TestRebalance_AllQueuesZero(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	hpaB := makeHPA("model-b", "ns", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 0, "model-b": 0},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 5 {
		t.Errorf("model-a maxReplicas = %d, want 5 (equal split when queues are zero)", got)
	}
	if got := getMaxReplicas(t, c, "model-b", "ns"); got != 5 {
		t.Errorf("model-b maxReplicas = %d, want 5 (equal split when queues are zero)", got)
	}
}

// TestRebalance_QueryError_TreatedAsZero verifies that a Prometheus query
// failure for a pool is treated as queue depth 0 — the pool still receives
// the minimum 1 replica and the other pool gets the remaining quota.
func TestRebalance_QueryError_TreatedAsZero(t *testing.T) {
	hpaA := makeHPA("model-a", "ns", "model-a", 10)
	hpaB := makeHPA("model-b", "ns", "model-b", 10)
	p, c := newPlugin(t,
		map[string]float64{"model-b": 100},
		map[string]error{"model-a": errors.New("prometheus unreachable")},
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// model-a failed query → weight=0 → clamped to min 1.
	// model-b has all queue → gets 10; total=11 but quota enforcement is at admission.
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 1 {
		t.Errorf("model-a maxReplicas = %d, want 1 (query error → treated as 0, clamped to min)", got)
	}
	if got := getMaxReplicas(t, c, "model-b", "ns"); got != 10 {
		t.Errorf("model-b maxReplicas = %d, want 10 (all queue depth)", got)
	}
}

// TestRebalance_RemainderToHighestWeight verifies that fractional replicas
// (remainder after floor) are assigned to the pool with the highest weight.
func TestRebalance_RemainderToHighestWeight(t *testing.T) {
	// q_a=7, q_b=4, total=11, quota=10
	// weight_a=7/11≈0.636 → floor(6.36)=6
	// weight_b=4/11≈0.364 → floor(3.63)=3
	// allocated=9, remainder=1 → goes to model-a (higher weight)
	// final: model-a=7, model-b=3
	hpaA := makeHPA("model-a", "ns", "model-a", 1)
	hpaB := makeHPA("model-b", "ns", "model-b", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 7, "model-b": 4},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 7 {
		t.Errorf("model-a maxReplicas = %d, want 7 (gets remainder)", got)
	}
	if got := getMaxReplicas(t, c, "model-b", "ns"); got != 3 {
		t.Errorf("model-b maxReplicas = %d, want 3", got)
	}
}

// TestRebalance_NoChangeWhenAlreadyCorrect verifies that when the current
// maxReplicas already matches the computed target, the patch is skipped (no
// error, value unchanged).
func TestRebalance_NoChangeWhenAlreadyCorrect(t *testing.T) {
	// Equal queues, quota=10 → each should get 5; set maxReplicas=5 already.
	hpaA := makeHPA("model-a", "ns", "model-a", 5)
	hpaB := makeHPA("model-b", "ns", "model-b", 5)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 50, "model-b": 50},
		nil,
		hpaA, hpaB, makeGPUQuota("q", "ns", 10),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := getMaxReplicas(t, c, "model-a", "ns"); got != 5 {
		t.Errorf("model-a maxReplicas = %d, want 5", got)
	}
	if got := getMaxReplicas(t, c, "model-b", "ns"); got != 5 {
		t.Errorf("model-b maxReplicas = %d, want 5", got)
	}
}

// TestRebalance_MultiNamespace verifies that HPAs in different namespaces are
// rebalanced independently against their own ResourceQuota and queue metrics.
func TestRebalance_MultiNamespace(t *testing.T) {
	// ns-x: quota=10, q_a=30, q_b=70 → a=3, b=7
	// ns-y: quota=6,  q_c=50, q_d=50 → c=3, d=3
	hpaA := makeHPA("model-a", "ns-x", "model-a", 1)
	hpaB := makeHPA("model-b", "ns-x", "model-b", 1)
	hpaC := makeHPA("model-c", "ns-y", "model-c", 1)
	hpaD := makeHPA("model-d", "ns-y", "model-d", 1)
	p, c := newPlugin(t,
		map[string]float64{"model-a": 30, "model-b": 70, "model-c": 50, "model-d": 50},
		nil,
		hpaA, hpaB, hpaC, hpaD,
		makeGPUQuota("q-x", "ns-x", 10),
		makeGPUQuota("q-y", "ns-y", 6),
	)

	if err := p.Tick(context.Background(), []client.Object{hpaA, hpaB, hpaC, hpaD}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name, ns string
		want     int32
	}{
		{"model-a", "ns-x", 3},
		{"model-b", "ns-x", 7},
		{"model-c", "ns-y", 3},
		{"model-d", "ns-y", 3},
	}
	for _, tc := range tests {
		if got := getMaxReplicas(t, c, tc.name, tc.ns); got != tc.want {
			t.Errorf("%s/%s maxReplicas = %d, want %d", tc.ns, tc.name, got, tc.want)
		}
	}
}
