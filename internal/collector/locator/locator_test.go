package locator_test

import (
	"context"
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		kedav1alpha1.AddToScheme,
		lwsv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("scheme add: %v", err)
		}
	}
	return s
}

func newClients(t *testing.T, objs ...runtime.Object) (cached, apiReader client.Client) {
	t.Helper()
	scheme := newScheme(t)
	build := func() client.Client {
		return fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(objs...).
			WithIndex(&autoscalingv2.HorizontalPodAutoscaler{}, indexers.HPAByScaleTargetKey, indexers.HPAByScaleTargetIndexFunc).
			WithIndex(&kedav1alpha1.ScaledObject{}, indexers.ScaledObjectByScaleTargetKey, indexers.ScaledObjectByScaleTargetIndexFunc).
			Build()
	}
	return build(), build()
}

// newClientsNoSOIndex mimics a cluster without the KEDA CRD: the ScaledObject
// field index is not registered, so any MatchingFields List against it would error.
func newClientsNoSOIndex(t *testing.T, objs ...runtime.Object) (cached, apiReader client.Client) {
	t.Helper()
	scheme := newScheme(t)
	build := func() client.Client {
		return fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(objs...).
			WithIndex(&autoscalingv2.HorizontalPodAutoscaler{}, indexers.HPAByScaleTargetKey, indexers.HPAByScaleTargetIndexFunc).
			Build()
	}
	return build(), build()
}

const testNamespace = "default"

func TestLocate_DeploymentChainHitsManagedHPA(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}},
		},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "h", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    5,
		},
	}

	cached, apiReader := newClients(t, deploy, rs, pod, hpa)
	loc, err := locator.New(cached, apiReader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got == nil || got.HPA == nil {
		t.Fatalf("got=%v, want HPA=h", got)
	}
	if got.HPA.Name != "h" {
		t.Errorf("HPA.Name=%q, want h", got.HPA.Name)
	}
}

func TestLocate_UnmanagedReturnsNil(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}},
	}
	cached, apiReader := newClients(t, deploy, rs, pod)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
}

func TestLocate_PodNotFound(t *testing.T) {
	cached, apiReader := newClients(t)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.Locate(context.Background(), testNamespace, "missing")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
}

func TestLocateByVariant_HPA(t *testing.T) {
	ns := testNamespace
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}
	cached, apiReader := newClients(t, hpa)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.LocateByVariant(context.Background(), ns, "v")
	if err != nil {
		t.Fatalf("LocateByVariant: %v", err)
	}
	if got == nil || got.HPA == nil || got.HPA.Name != "v" {
		t.Fatalf("got=%v, want HPA=v", got)
	}
}

func TestLocateByVariant_UnmanagedHPA(t *testing.T) {
	ns := testNamespace
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}
	cached, apiReader := newClients(t, hpa)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.LocateByVariant(context.Background(), ns, "v")
	if err != nil {
		t.Fatalf("LocateByVariant: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil for unmanaged HPA", got)
	}
}

func TestLocateByVariant_AmbiguousHPAndSO(t *testing.T) {
	ns := testNamespace
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
		},
	}
	cached, apiReader := newClients(t, hpa, so)
	loc, _ := locator.New(cached, apiReader)
	if _, err := loc.LocateByVariant(context.Background(), ns, "v"); err == nil {
		t.Errorf("expected ambiguity error, got nil")
	}
}

func TestLocate_CacheHitOnSecondCall(t *testing.T) {
	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}}}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}

	cached, apiReader := newClients(t, deploy, rs, pod, hpa)
	loc, _ := locator.New(cached, apiReader)

	// Warm the cache.
	if _, err := loc.Locate(context.Background(), ns, "p"); err != nil {
		t.Fatalf("first Locate: %v", err)
	}

	// Delete the pod from apiReader; if the cache works, the second Locate
	// must still resolve to the same HPA because the pod → Deployment step
	// is cached.
	if err := apiReader.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("second Locate: %v", err)
	}
	if got == nil || got.HPA == nil || got.HPA.Name != "h" {
		t.Errorf("cache miss on second call: got=%v", got)
	}
}

func TestLocate_LWSChain(t *testing.T) {
	ns := testNamespace
	lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "lws", Namespace: ns, UID: "uid-lws"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "leaderworkerset.x-k8s.io/v1", Kind: "LeaderWorkerSet",
				Name: "lws", UID: "uid-lws", Controller: ptr.To(true),
			}}},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "leaderworkerset.x-k8s.io/v1", Kind: "LeaderWorkerSet", Name: "lws",
			},
			MaxReplicas: 5,
		},
	}
	cached, apiReader := newClients(t, lws, pod, hpa)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil || got == nil || got.HPA == nil || got.HPA.Name != "h" {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

// TestLocate_KEDADisabledSkipsScaledObject verifies that when KEDA is disabled the
// locator does not touch the (unregistered) ScaledObject field index, so Locate
// returns the managed HPA without erroring on the missing index.
func TestLocate_KEDADisabledSkipsScaledObject(t *testing.T) {
	locator.SetKEDAEnabled(false)
	t.Cleanup(func() { locator.SetKEDAEnabled(true) })

	ns := testNamespace
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"}}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d", Controller: ptr.To(true)}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs", Controller: ptr.To(true)}}},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    5,
		},
	}
	cached, apiReader := newClientsNoSOIndex(t, deploy, rs, pod, hpa)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.Locate(context.Background(), ns, "p")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if got == nil || got.HPA == nil || got.HPA.Name != "h" {
		t.Fatalf("got=%v, want HPA=h", got)
	}
}

// TestLocateByVariant_KEDADisabledSkipsScaledObject verifies that LocateByVariant
// skips the cached ScaledObject Get when KEDA is disabled, returning the HPA only.
func TestLocateByVariant_KEDADisabledSkipsScaledObject(t *testing.T) {
	locator.SetKEDAEnabled(false)
	t.Cleanup(func() { locator.SetKEDAEnabled(true) })

	ns := testNamespace
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: ns,
			Annotations: map[string]string{"llm-d.ai/managed": "true"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
			MaxReplicas:    1,
		},
	}
	cached, apiReader := newClientsNoSOIndex(t, hpa)
	loc, _ := locator.New(cached, apiReader)
	got, err := loc.LocateByVariant(context.Background(), ns, "v")
	if err != nil {
		t.Fatalf("LocateByVariant: %v", err)
	}
	if got == nil || got.HPA == nil || got.HPA.Name != "v" {
		t.Fatalf("got=%v, want HPA=v", got)
	}
}
