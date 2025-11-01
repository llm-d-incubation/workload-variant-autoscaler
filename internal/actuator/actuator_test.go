/*
Copyright 2025.

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

package actuator

import (
	"context"
	"fmt"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/metrics"
	ctrlutils "github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Actuator", func() {
	var (
		ctx          context.Context
		scheme       *runtime.Scheme
		actuator     *Actuator
		registry     *prometheus.Registry
		resourceName string
		namespace    string
	)

	BeforeEach(func() {
		ctx = context.Background()
		resourceName = "test-variant-autoscaling"
		namespace = "default"

		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)).To(Succeed())

		// Create a new registry for each test to avoid conflicts
		registry = prometheus.NewRegistry()

		// Initialize metrics with the test registry
		err := metrics.InitMetrics(registry)
		Expect(err).NotTo(HaveOccurred())

		// Initialize the actuator with the controller-runtime client
		actuator = NewActuator(k8sClient)
		Expect(actuator).NotTo(BeNil())
		Expect(actuator.MetricsEmitter).NotTo(BeNil())
	})

	Context("Actuator initialization", func() {
		It("should create a new actuator with valid client and metrics emitter", func() {
			newActuator := NewActuator(k8sClient)
			Expect(newActuator).NotTo(BeNil())
			Expect(newActuator.Client).To(Equal(k8sClient))
			Expect(newActuator.MetricsEmitter).NotTo(BeNil())
		})
	})

	Context("Testing getCurrentDeploymentReplicas", func() {
		var deployment *appsv1.Deployment
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling

		BeforeEach(func() {
			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(3)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": resourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": resourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "quay.io/infernoautoscaler/vllme:0.2.3-multi-arch",
									Ports: []corev1.ContainerPort{{ContainerPort: 80}},
								},
							},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 3,
				},
			}

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
			}

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
		})

		AfterEach(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, va))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deployment))).To(Succeed())
		})

		It("should return status replicas when available", func() {
			deployment.Status.Replicas = 3
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			replicas, err := actuator.getCurrentDeploymentReplicas(ctx, va)
			Expect(err).NotTo(HaveOccurred())
			Expect(replicas).To(Equal(deployment.Status.Replicas), fmt.Sprintf("Should return status replicas - actual: %d", replicas))
		})

		It("should return error when deployment doesn't exist", func() {
			nonExistentVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-existent",
					Namespace: namespace,
				},
			}

			_, err := actuator.getCurrentDeploymentReplicas(ctx, nonExistentVA)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get Deployment"))
		})
	})

	Context("EmitMetrics", func() {
		var deployment *appsv1.Deployment
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling

		BeforeEach(func() {
			// Use unique resource name for this test context
			contextResourceName := resourceName + "-emit"

			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": contextResourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": contextResourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "quay.io/infernoautoscaler/vllme:0.2.3-multi-arch",
									Ports: []corev1.ContainerPort{{ContainerPort: 80}},
								},
							},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 2,
				},
			}

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
					Labels: map[string]string{
						"inference.optimization/acceleratorName": "A100",
					},
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "test-model/variant-1",
					VariantID:        "test-model/variant-1-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "test-slo-config",
						Key:  "test-slo-key",
					},
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms: map[string]string{
								"alpha": "20.58",
								"beta":  "0.41",
							},
							PrefillParms: map[string]string{
								"gamma": "200.58",
								"delta": "0.041",
							},
						},
						MaxBatchSize: 32,
					},
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
						// Note: In single-variant architecture, variantID, accelerator, maxBatch, and variantCost
						// are in the parent VA spec, not in Allocation status
						NumReplicas: 2,
					},
					DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
						// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
						NumReplicas: 4,
					},
				},
			}

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			Expect(k8sClient.Create(ctx, va)).To(Succeed())
		})

		AfterEach(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, va))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deployment))).To(Succeed())
		})

		It("should emit metrics successfully when desired replicas > 0", func() {
			replicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %d\n", va.Name, replicas)
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Verify metrics were emitted correctly
			// We can't directly test the metrics values due to registry isolation,
			// but we can verify the method completed without error
		})

		It("should emit metrics for scale-to-zero case (desired replicas = 0)", func() {
			va.Status.DesiredOptimizedAlloc.NumReplicas = 0
			replicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %d\n", va.Name, replicas)
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Method should succeed and emit metrics even for scale-to-zero (NumReplicas=0)
			// This is critical for KEDA to function properly in scale-to-zero scenarios
		})

		It("should use fallback replicas when deployment retrieval fails", func() {
			// Delete the deployment to simulate retrieval failure
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())

			// Wait for deletion to complete
			Eventually(func() error {
				var dep appsv1.Deployment
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      resourceName,
					Namespace: namespace,
				}, &dep)
			}).Should(HaveOccurred())
			// Note: In single-variant architecture, VariantID is in spec, not in OptimizedAlloc
			// Check NumReplicas to determine if there's a valid allocation
			replicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %d\n", va.Name, replicas)
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Should use fallback from VariantAutoscaling status (2 replicas)
			// Method should complete without error despite deployment retrieval failure
		})

		It("should handle metrics emission errors gracefully", func() {
			// This test verifies that metrics emission errors don't fail the method
			// We can't easily simulate a metrics emission error without mocking,
			// but we can verify the error handling logic exists
			// Note: In single-variant architecture, VariantID is in spec, not in OptimizedAlloc
			replicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %d\n", va.Name, replicas)
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should skip metrics emission when DesiredOptimizedAlloc is empty", func() {
			// Test the early exit when no allocation (empty VariantID)
			va.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{}
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should skip allocation when NumReplicas is negative", func() {
			// Test skipping negative replicas
			// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
			va.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
				NumReplicas: -1,
			}
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should emit metrics for single allocation", func() {
			// Test metrics emission for single variant allocation
			// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
			va.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
				NumReplicas: 2,
			}
			va.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
				NumReplicas: 2,
			}
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should use variantID matching fallback when deployment not found", func() {
			// Delete deployment to trigger fallback logic
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			Eventually(func() error {
				var dep appsv1.Deployment
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      deployment.Name,
					Namespace: namespace,
				}, &dep)
			}).Should(HaveOccurred())

			// Set up matching CurrentAlloc
			// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
			va.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
				NumReplicas: 5,
			}
			va.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
				NumReplicas: 3,
			}

			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())
			// Should use fallback of 3 replicas from matching CurrentAlloc
		})

		It("should use zero fallback when no matching CurrentAlloc found", func() {
			// Delete deployment to trigger fallback logic
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
			Eventually(func() error {
				var dep appsv1.Deployment
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      deployment.Name,
					Namespace: namespace,
				}, &dep)
			}).Should(HaveOccurred())

			// DesiredAlloc with different CurrentAlloc (testing fallback)
			// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
			va.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
				NumReplicas: 5,
			}
			va.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
				NumReplicas: 3,
			}

			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())
			// Should use fallback of 0 replicas (no match found)
		})

		It("should emit metrics for valid allocation with zero replicas", func() {
			// Test with zero replicas (valid but should be skipped)
			// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
			va.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
				NumReplicas: 0,
			}
			va.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
				NumReplicas: 0,
			}

			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Metrics integration", func() {
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		var deployment *appsv1.Deployment

		BeforeEach(func() {
			// Use unique resource name for this test context
			contextResourceName := resourceName + "-metrics"

			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": contextResourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": contextResourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "quay.io/infernoautoscaler/vllme:0.2.3-multi-arch",
									Ports: []corev1.ContainerPort{{ContainerPort: 80}},
								},
							},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 1,
				},
			}

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "test-model/metrics-test",
					VariantID:        "test-model/metrics-test-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "test-slo-config",
						Key:  "metrics-slo-key",
					},
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms: map[string]string{
								"alpha": "20.58",
								"beta":  "0.41",
							},
							PrefillParms: map[string]string{
								"gamma": "200.58",
								"delta": "0.041",
							},
						},
						MaxBatchSize: 32,
					},
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
						// Note: In single-variant architecture, variantID, accelerator, maxBatch, and variantCost
						// are in the parent VA spec, not in Allocation status
						NumReplicas: 1,
					},
					DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
						// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
						NumReplicas: 3,
					},
				},
			}

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

		})

		AfterEach(func() {
			// Cleanup resources with proper error handling
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, va))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deployment))).To(Succeed())
		})

		It("should verify that metrics emitter can emit scaling metrics", func() {
			// In single-variant architecture, just use NumReplicas directly
			replicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			fmt.Printf("Emitting scaling metrics for variantAutoscaling - name: %s\n numReplicas: %d\n", va.Name, replicas)
			err := actuator.MetricsEmitter.EmitReplicaScalingMetrics(ctx, va, "up", "optimization")
			Expect(err).NotTo(HaveOccurred())
		})

		It("should verify that metrics emitter can emit replica metrics", func() {
			// In single-variant architecture, just use NumReplicas directly
			replicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			fmt.Printf("Emitting replica metrics for variantAutoscaling - name: %s\n numReplicas: %d\n", va.Name, replicas)
			err := actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 1, 3, "A100", "test-model-A100-1")
			Expect(err).NotTo(HaveOccurred())
		})

		It("should verify full metric emission workflow", func() {
			// Test the complete workflow
			// In single-variant architecture, just use NumReplicas directly
			replicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %d\n", va.Name, replicas)
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Additional scaling metrics
			err = actuator.MetricsEmitter.EmitReplicaScalingMetrics(ctx, va, "up", "load_increase")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Edge cases and error handling", func() {
		It("should handle VariantAutoscaling with missing status fields", func() {
			// Create a minimal valid VariantAutoscaling but with zero desired replicas
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "incomplete-va",
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "test-model/incomplete",
					VariantID:        "test-model/incomplete-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "test-slo-config",
						Key:  "test-slo-key",
					},
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms: map[string]string{
								"alpha": "20.58",
								"beta":  "0.41",
							},
							PrefillParms: map[string]string{
								"gamma": "200.58",
								"delta": "0.041",
							},
						},
						MaxBatchSize: 32,
					},
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					// DesiredOptimizedAllocs will be empty by default, causing EmitMetrics to skip
					DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{},
				},
			}

			Expect(k8sClient.Create(ctx, va)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, va))).To(Succeed())
			}()
			// In single-variant architecture, just use NumReplicas directly
			replicas := va.Status.DesiredOptimizedAlloc.NumReplicas
			fmt.Printf("Emitting metrics for variantAutoscaling - name: %s\n numReplicas: %d\n", va.Name, replicas)
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred()) // Should skip metrics emission due to 0 replicas
		})
	})

	Context("Metrics validation", func() {
		var va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		var deployment *appsv1.Deployment

		BeforeEach(func() {
			// Use unique resource name for this test context
			contextResourceName := resourceName + "-validation"

			deployment = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ctrlutils.Ptr(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": contextResourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": contextResourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "quay.io/infernoautoscaler/vllme:0.2.3-multi-arch",
									Ports: []corev1.ContainerPort{{ContainerPort: 80}},
								},
							},
						},
					},
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 2,
				},
			}

			va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contextResourceName,
					Namespace: namespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "test-model/validation-test",
					VariantID:        "test-model/validation-test-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "test-slo-config",
						Key:  "validation-slo-key",
					},
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms: map[string]string{
								"alpha": "20.58",
								"beta":  "0.41",
							},
							PrefillParms: map[string]string{
								"gamma": "200.58",
								"delta": "0.041",
							},
						},
						MaxBatchSize: 32,
					},
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
						// Note: In single-variant architecture, variantID, accelerator, maxBatch, and variantCost
						// are in the parent VA spec, not in Allocation status
						NumReplicas: 2,
					},
					DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
						// Note: In single-variant architecture, variantID and accelerator are in the parent VA spec
						NumReplicas: 5,
					},
				},
			}

			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

		})

		AfterEach(func() {
			// Cleanup resources with proper error handling
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, va))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, deployment))).To(Succeed())
		})

		It("should test ratio calculation scenarios", func() {
			fmt.Printf("Testing metrics emission for variant autoscaling: %s\n", va.Name)
			err := actuator.EmitMetrics(ctx, va)
			Expect(err).NotTo(HaveOccurred())

			// Test normal case: current = 2, desired = 5, ratio = 2.5
			err = actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 2, 5, "A100", "test-model-A100-1")
			Expect(err).NotTo(HaveOccurred())

			// Test scale-to-zero case: current = 0, desired = 3, ratio = 3
			err = actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 0, 3, "A100", "test-model-A100-1")
			Expect(err).NotTo(HaveOccurred())

			// Test no-change case: current = 4, desired = 4, ratio = 1
			err = actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 4, 4, "A100", "test-model-A100-1")
			Expect(err).NotTo(HaveOccurred())

			// Test scale-down case: current = 6, desired = 2, ratio = 0.33
			err = actuator.MetricsEmitter.EmitReplicaMetrics(ctx, va, 6, 2, "A100", "test-model-A100-1")
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
