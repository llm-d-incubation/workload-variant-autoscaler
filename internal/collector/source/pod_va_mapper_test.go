package source

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmdv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
)

var _ = Describe("PodVAMapper", func() {
	var (
		ctx         context.Context
		deployments map[string]*appsv1.Deployment
	)

	BeforeEach(func() {
		ctx = context.Background()
		deployments = make(map[string]*appsv1.Deployment)
	})

	// Helper function to create a scheme with all required types
	createScheme := func() *runtime.Scheme {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(llmdv1alpha1.AddToScheme(scheme)).To(Succeed())
		return scheme
	}

	// Helper function to create a fake client with VA scale target index
	createFakeClientWithIndex := func(scheme *runtime.Scheme, objects ...client.Object) client.Client {
		return fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objects...).
			WithIndex(&llmdv1alpha1.VariantAutoscaling{}, indexers.VAScaleTargetKey, indexers.VAScaleTargetIndexFunc).
			Build()
	}

	// Helper function to create a ReplicaSet owned by a Deployment
	createReplicaSet := func(name, namespace, deploymentName string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
						Controller: ptr.To(true),
					},
				},
			},
		}
	}

	// Helper function to create a Pod owned by a ReplicaSet
	createPod := func(name, namespace, rsName string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "ReplicaSet",
						Name:       rsName,
						Controller: ptr.To(true),
					},
				},
			},
		}
	}

	// Helper function to create a VariantAutoscaling targeting a Deployment
	createVA := func(name, namespace, deploymentName string) *llmdv1alpha1.VariantAutoscaling {
		return &llmdv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: llmdv1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind:       "Deployment",
					Name:       deploymentName,
					APIVersion: "apps/v1",
				},
			},
		}
	}

	Describe("FindVAForPod", func() {
		It("should find VA for a pod through its deployment via owner references and indexed lookup", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "llama-deploy",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "llama",
						},
					},
				},
			}
			deployments["default/llama-deploy"] = deployment

			va := createVA("llama-va", "default", "llama-deploy")
			rs := createReplicaSet("llama-deploy-abc123", "default", "llama-deploy")
			pod := createPod("llama-deploy-abc123-xyz", "default", "llama-deploy-abc123", map[string]string{"app": "llama"})

			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme, pod, rs, va)

			mapper := NewPodVAMapper(fakeClient)
			result := mapper.FindVAForPod(ctx, "llama-deploy-abc123-xyz", "default", deployments)
			Expect(result).To(Equal("llama-va"))
		})

		It("should return empty when pod has no matching deployment", func() {
			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme)

			mapper := NewPodVAMapper(fakeClient)
			result := mapper.FindVAForPod(ctx, "unknown-pod", "default", deployments)
			Expect(result).To(BeEmpty())
		})

		It("should return empty when deployment has no matching VA", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "orphan-deploy",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "orphan",
						},
					},
				},
			}
			deployments["default/orphan-deploy"] = deployment

			rs := createReplicaSet("orphan-deploy-abc123", "default", "orphan-deploy")
			pod := createPod("orphan-deploy-abc123-xyz", "default", "orphan-deploy-abc123", map[string]string{"app": "orphan"})

			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme, pod, rs)

			mapper := NewPodVAMapper(fakeClient)
			result := mapper.FindVAForPod(ctx, "orphan-deploy-abc123-xyz", "default", deployments)
			Expect(result).To(BeEmpty())
		})

		It("should not match VA in different namespace", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "llama-deploy",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "llama",
						},
					},
				},
			}
			deployments["default/llama-deploy"] = deployment

			// VA in different namespace should not match
			va := createVA("llama-va", "production", "llama-deploy")
			rs := createReplicaSet("llama-deploy-abc123", "default", "llama-deploy")
			pod := createPod("llama-deploy-abc123-xyz", "default", "llama-deploy-abc123", map[string]string{"app": "llama"})

			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme, pod, rs, va)

			mapper := NewPodVAMapper(fakeClient)
			result := mapper.FindVAForPod(ctx, "llama-deploy-abc123-xyz", "default", deployments)
			Expect(result).To(BeEmpty())
		})

		It("should handle multiple deployments and VAs", func() {
			scheme := createScheme()

			// Setup multiple deployments
			var objects []client.Object
			for _, name := range []string{"deploy-a", "deploy-b", "deploy-c"} {
				deployments["default/"+name] = &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": name,
							},
						},
					},
				}
				rs := createReplicaSet(name+"-rs", "default", name)
				objects = append(objects, rs)
			}

			// Setup corresponding VA for deploy-b only
			va := createVA("va-b", "default", "deploy-b")
			objects = append(objects, va)

			pod := createPod("deploy-b-pod-xyz", "default", "deploy-b-rs", map[string]string{"app": "deploy-b"})
			objects = append(objects, pod)

			fakeClient := createFakeClientWithIndex(scheme, objects...)

			mapper := NewPodVAMapper(fakeClient)
			result := mapper.FindVAForPod(ctx, "deploy-b-pod-xyz", "default", deployments)
			Expect(result).To(Equal("va-b"))
		})

		It("should return consistent results for repeated lookups", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cached-deploy",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "cached",
						},
					},
				},
			}
			deployments["default/cached-deploy"] = deployment

			va := createVA("cached-va", "default", "cached-deploy")
			rs := createReplicaSet("cached-deploy-rs", "default", "cached-deploy")
			pod := createPod("cached-deploy-pod-xyz", "default", "cached-deploy-rs", map[string]string{"app": "cached"})

			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme, pod, rs, va)

			mapper := NewPodVAMapper(fakeClient)

			// First lookup
			result1 := mapper.FindVAForPod(ctx, "cached-deploy-pod-xyz", "default", deployments)
			Expect(result1).To(Equal("cached-va"))

			// Second lookup
			result2 := mapper.FindVAForPod(ctx, "cached-deploy-pod-xyz", "default", deployments)
			Expect(result2).To(Equal("cached-va"))
		})

		It("should return empty when deployment no longer exists in tracked deployments map", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "removable-deploy",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "removable",
						},
					},
				},
			}
			deployments["default/removable-deploy"] = deployment

			va := createVA("removable-va", "default", "removable-deploy")
			rs := createReplicaSet("removable-deploy-rs", "default", "removable-deploy")
			pod := createPod("removable-deploy-pod-xyz", "default", "removable-deploy-rs", map[string]string{"app": "removable"})

			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme, pod, rs, va)

			mapper := NewPodVAMapper(fakeClient)

			// First lookup - should find VA
			result1 := mapper.FindVAForPod(ctx, "removable-deploy-pod-xyz", "default", deployments)
			Expect(result1).To(Equal("removable-va"))

			// Remove deployment from map
			delete(deployments, "default/removable-deploy")

			// Second lookup - should return empty since deployment is gone from tracked map
			result2 := mapper.FindVAForPod(ctx, "removable-deploy-pod-xyz", "default", deployments)
			Expect(result2).To(BeEmpty())
		})

		It("should return empty for pods without ReplicaSet owner", func() {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "standalone-deploy",
					Namespace: "default",
				},
			}
			deployments["default/standalone-deploy"] = deployment

			// Pod without owner references (standalone pod)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "standalone-pod",
					Namespace: "default",
				},
			}

			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme, pod)

			mapper := NewPodVAMapper(fakeClient)
			result := mapper.FindVAForPod(ctx, "standalone-pod", "default", deployments)
			Expect(result).To(BeEmpty())
		})

		It("should find correct VA when same deployment name exists in multiple namespaces", func() {
			// Deployment in namespace-a
			deploymentA := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "shared-deploy",
					Namespace: "namespace-a",
				},
			}
			deployments["namespace-a/shared-deploy"] = deploymentA

			// Deployment in namespace-b (same deployment name, different namespace)
			deploymentB := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "shared-deploy",
					Namespace: "namespace-b",
				},
			}
			deployments["namespace-b/shared-deploy"] = deploymentB

			// VA in namespace-a targeting shared-deploy
			vaA := createVA("va-a", "namespace-a", "shared-deploy")
			rsA := createReplicaSet("shared-deploy-rs-a", "namespace-a", "shared-deploy")
			podA := createPod("shared-deploy-pod-a", "namespace-a", "shared-deploy-rs-a", nil)

			// VA in namespace-b targeting shared-deploy (same name, different namespace)
			vaB := createVA("va-b", "namespace-b", "shared-deploy")
			rsB := createReplicaSet("shared-deploy-rs-b", "namespace-b", "shared-deploy")
			podB := createPod("shared-deploy-pod-b", "namespace-b", "shared-deploy-rs-b", nil)

			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme, podA, rsA, vaA, podB, rsB, vaB)

			mapper := NewPodVAMapper(fakeClient)

			// Pod in namespace-a should find va-a
			resultA := mapper.FindVAForPod(ctx, "shared-deploy-pod-a", "namespace-a", deployments)
			Expect(resultA).To(Equal("va-a"))

			// Pod in namespace-b should find va-b
			resultB := mapper.FindVAForPod(ctx, "shared-deploy-pod-b", "namespace-b", deployments)
			Expect(resultB).To(Equal("va-b"))
		})
	})
})
