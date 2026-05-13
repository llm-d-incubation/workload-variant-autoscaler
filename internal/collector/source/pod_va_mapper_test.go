package source

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmdv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

var _ = Describe("PodVAMapper", func() {
	var (
		ctx         context.Context
		deployments map[string]scaletarget.ScaleTargetAccessor
		registry    *prometheus.Registry
	)

	BeforeEach(func() {
		ctx = context.Background()
		deployments = make(map[string]scaletarget.ScaleTargetAccessor)

		// Initialize metrics for error recording
		registry = prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
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
			deployments["default/llama-deploy"] = scaletarget.NewDeploymentAccessor(deployment)

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
			deployments["default/orphan-deploy"] = scaletarget.NewDeploymentAccessor(deployment)

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
			deployments["default/llama-deploy"] = scaletarget.NewDeploymentAccessor(deployment)

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
			objects := make([]client.Object, 0, 5)
			for _, name := range []string{"deploy-a", "deploy-b", "deploy-c"} {
				deployments["default/"+name] = scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
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
				})
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
			deployments["default/cached-deploy"] = scaletarget.NewDeploymentAccessor(deployment)

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
			deployments["default/removable-deploy"] = scaletarget.NewDeploymentAccessor(deployment)

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
			deployments["default/standalone-deploy"] = scaletarget.NewDeploymentAccessor(deployment)

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
			deployments["namespace-a/shared-deploy"] = scaletarget.NewDeploymentAccessor(deploymentA)

			// Deployment in namespace-b (same deployment name, different namespace)
			deploymentB := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "shared-deploy",
					Namespace: "namespace-b",
				},
			}
			deployments["namespace-b/shared-deploy"] = scaletarget.NewDeploymentAccessor(deploymentB)

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

	Describe("Metrics Recording", func() {
		It("should record error when pod is not found", func() {
			By("Initializing metrics with a test registry")
			testRegistry := prometheus.NewRegistry()
			err := metrics.InitMetrics(testRegistry)
			Expect(err).NotTo(HaveOccurred())

			By("Attempting to find VA for a non-existent pod")
			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme)
			mapper := NewPodVAMapper(fakeClient)

			result := mapper.FindVAForPod(ctx, "non-existent-pod", "default", deployments)
			Expect(result).To(BeEmpty())

			By("Verifying error metric was recorded")
			metricFamilies, err := testRegistry.Gather()
			Expect(err).NotTo(HaveOccurred())

			var found bool
			for _, mf := range metricFamilies {
				if mf.GetName() == constants.WVAErrorsTotal {
					for _, metric := range mf.GetMetric() {
						var component, errorType string
						for _, label := range metric.GetLabel() {
							if label.GetName() == constants.LabelComponent {
								component = label.GetValue()
							}
							if label.GetName() == constants.LabelErrorType {
								errorType = label.GetValue()
							}
						}
						if component == constants.ComponentCollector && errorType == "failed to get pod" {
							found = true
							Expect(metric.GetCounter().GetValue()).To(BeNumerically(">=", 1.0))
							break
						}
					}
				}
			}
			Expect(found).To(BeTrue(), "Error metric for 'failed to get pod' should be recorded")
		})

		It("should record error when ReplicaSet is not found", func() {
			By("Initializing metrics with a test registry")
			testRegistry := prometheus.NewRegistry()
			err := metrics.InitMetrics(testRegistry)
			Expect(err).NotTo(HaveOccurred())

			By("Creating a pod with a ReplicaSet owner that doesn't exist")
			pod := createPod("orphan-pod", "default", "non-existent-rs", nil)
			scheme := createScheme()
			fakeClient := createFakeClientWithIndex(scheme, pod)
			mapper := NewPodVAMapper(fakeClient)

			result := mapper.FindVAForPod(ctx, "orphan-pod", "default", deployments)
			Expect(result).To(BeEmpty())

			By("Verifying error metric was recorded")
			metricFamilies, err := testRegistry.Gather()
			Expect(err).NotTo(HaveOccurred())

			var found bool
			for _, mf := range metricFamilies {
				if mf.GetName() == constants.WVAErrorsTotal {
					for _, metric := range mf.GetMetric() {
						var component, errorType string
						for _, label := range metric.GetLabel() {
							if label.GetName() == constants.LabelComponent {
								component = label.GetValue()
							}
							if label.GetName() == constants.LabelErrorType {
								errorType = label.GetValue()
							}
						}
						if component == constants.ComponentCollector && errorType == errorTypeFailedToGetScaleTarget {
							found = true
							Expect(metric.GetCounter().GetValue()).To(BeNumerically(">=", 1.0))
							break
						}
					}
				}
			}
			Expect(found).To(BeTrue(), "Error metric for 'failed to get scale target' should be recorded")
		})

		It("should record different error types separately", func() {
			const errorTypeVANotFound = "failed to find VariantAutoscaling for scale target"
			By("Initializing metrics with a test registry")
			testRegistry := prometheus.NewRegistry()
			err := metrics.InitMetrics(testRegistry)
			Expect(err).NotTo(HaveOccurred())

			By("Recording different error types")
			metrics.RecordError(constants.ComponentCollector, errorTypeVANotFound)
			metrics.RecordError(constants.ComponentCollector, "failed to get pod")
			metrics.RecordError(constants.ComponentCollector, "failed to get ReplicaSet")

			By("Verifying all errors were recorded separately")
			metricFamilies, err := testRegistry.Gather()
			Expect(err).NotTo(HaveOccurred())

			var errorMetricCount int
			for _, mf := range metricFamilies {
				if mf.GetName() == constants.WVAErrorsTotal {
					errorMetricCount = len(mf.GetMetric())
					break
				}
			}
			// Should have 3 separate error counters
			Expect(errorMetricCount).To(Equal(3), "Should have 3 separate error metrics")
		})

		It("should record error when finding VA for deployment fails", func() {
			const errorTypeVANotFound = "failed to find VariantAutoscaling for scale target"
			By("Initializing metrics with a test registry")
			testRegistry := prometheus.NewRegistry()
			err := metrics.InitMetrics(testRegistry)
			Expect(err).NotTo(HaveOccurred())

			By("Setting up a deployment and pod but no VA index")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deploy",
					Namespace: "default",
				},
			}
			deployments["default/test-deploy"] = scaletarget.NewDeploymentAccessor(deployment)

			rs := createReplicaSet("test-deploy-rs", "default", "test-deploy")
			pod := createPod("test-deploy-pod", "default", "test-deploy-rs", nil)

			scheme := createScheme()
			// Create client WITHOUT index to trigger error in FindVAForDeployment
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(pod, rs).
				Build()

			mapper := NewPodVAMapper(fakeClient)
			result := mapper.FindVAForPod(ctx, "test-deploy-pod", "default", deployments)
			Expect(result).To(BeEmpty())

			By("Verifying error metric was recorded")
			metricFamilies, err := testRegistry.Gather()
			Expect(err).NotTo(HaveOccurred())

			var found bool
			for _, mf := range metricFamilies {
				if mf.GetName() == constants.WVAErrorsTotal {
					for _, metric := range mf.GetMetric() {
						var component, errorType string
						for _, label := range metric.GetLabel() {
							if label.GetName() == constants.LabelComponent {
								component = label.GetValue()
							}
							if label.GetName() == constants.LabelErrorType {
								errorType = label.GetValue()
							}
						}
						if component == constants.ComponentCollector && errorType == errorTypeVANotFound {
							found = true
							Expect(metric.GetCounter().GetValue()).To(BeNumerically(">=", 1.0))
							break
						}
					}
				}
			}
			Expect(found).To(BeTrue(), "Error metric for 'failed to find VariantAutoscaling for scale target' should be recorded")
		})
	})
})
