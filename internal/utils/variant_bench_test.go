package utils

import (
	"context"
	"fmt"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// buildHPACluster returns a fake client modelling a shared cluster: trackedNS
// namespaces each holding perNS WVA-managed HPAs, plus noiseCount unmanaged HPAs
// scattered across unrelated namespaces (other teams' workloads). The unmanaged
// HPAs are exactly the objects the cluster-wide discovery path deserializes and
// filters on every optimization tick and the scoped path never touches. The
// returned slice is the tracked-namespace set the scoped path lists.
func buildHPACluster(tb testing.TB, trackedNS, perNS, noiseCount int) (client.Client, []string) {
	tb.Helper()
	s := variantTestScheme(tb)
	objs := make([]client.Object, 0, trackedNS*perNS+noiseCount)
	tracked := make([]string, 0, trackedNS)
	for n := 0; n < trackedNS; n++ {
		ns := fmt.Sprintf("team-%d", n)
		tracked = append(tracked, ns)
		for i := 0; i < perNS; i++ {
			objs = append(objs, managedHPA(ns, fmt.Sprintf("hpa-%d", i), fmt.Sprintf("deploy-%d", i), "model-x"))
		}
	}
	for i := 0; i < noiseCount; i++ {
		// Spread noise across many unrelated namespaces, as in a real multi-team cluster.
		objs = append(objs, &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("noise-%d", i), Namespace: fmt.Sprintf("other-%d", i%256)},
			Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 3},
		})
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return cl, tracked
}

// BenchmarkAnnotationSourcedVariants quantifies the per-tick discovery cost that
// #1134 removes. Both arms hit the same cluster and return the same 10 managed
// variants; only the HPA List scoping differs:
//
//   - cluster-wide(before): annotationSourcedVariants(ctx, cl, nil) — the nil
//     gate state lists every HPA in the cluster and filters in-process.
//   - scoped(after):        annotationSourcedVariants(ctx, cl, tracked) — lists
//     only the namespaces WVA actually tracks.
//
// The ScaledObject list is identical (and empty) in both arms, so the delta
// isolates exactly the HPA-scoping change. ReportAllocs surfaces the B/op and
// allocs/op reduction, which is the cleanest proxy for the saved CPU/GC work:
// the fake client lacks the namespace index a real informer cache has, so it
// over-charges the scoped arm's iteration — i.e. these numbers understate the
// wall-clock saving on a live cluster while reporting allocations faithfully.
func BenchmarkAnnotationSourcedVariants(b *testing.B) {
	ctx := context.Background()
	const trackedNS, perNS = 5, 2 // 10 managed HPAs WVA cares about

	for _, noise := range []int{100, 1000, 5000} {
		cl, tracked := buildHPACluster(b, trackedNS, perNS, noise)

		b.Run(fmt.Sprintf("noise=%d/cluster-wide(before)", noise), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := annotationSourcedVariants(ctx, cl, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("noise=%d/scoped(after)", noise), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := annotationSourcedVariants(ctx, cl, tracked); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
