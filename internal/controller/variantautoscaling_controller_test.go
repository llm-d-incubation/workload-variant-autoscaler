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

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils/resources"
)

var _ = Describe("VariantAutoscalings Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		VariantAutoscalings := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}

		BeforeEach(func() {
			logging.NewTestLogger()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required scale target ref deployment")
			deployment := resources.CreateLlmdSimDeployment("default", resourceName, "default-default", "default", "8000", 0, 0, 1)
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("creating the required configmap for optimization")
			configMap := testutils.CreateServiceClassConfigMap(ns.Name)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(config.DefaultConfigMapName, ns.Name)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).To(Succeed())

			By("creating the custom resource for the Kind VariantAutoscalings")
			err := k8sClient.Get(ctx, typeNamespacedName, VariantAutoscalings)
			if err != nil && errors.IsNotFound(err) {
				resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// TODO(user): Specify other spec details if needed.
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							Kind: "Deployment",
							Name: resourceName,
						},
						ModelID:     "default-default",
						MaxReplicas: 2,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance VariantAutoscalings")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DefaultConfigMapName,
					Namespace: config.SystemNamespace(),
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")

			// Initialize MetricsCollector with mock Prometheus API
			controllerReconciler := &VariantAutoscalingReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Datastore: datastore.NewDatastore(config.NewTestConfig()),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

		})
	})

	Context("When validating configurations", func() {

		BeforeEach(func() {
			logging.NewTestLogger()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmaps")
			configMap := testutils.CreateServiceClassConfigMap(ns.Name)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).NotTo(HaveOccurred())

			configMap = testutils.CreateVariantAutoscalingConfigMap(config.DefaultConfigMapName, ns.Name)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DefaultConfigMapName,
					Namespace: config.SystemNamespace(),
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should handle empty ModelID value", func() {
			By("Creating VariantAutoscaling with empty ModelID")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-model-id",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "invalid-model-id",
					},
					ModelID:     "", // Empty ModelID
					MaxReplicas: 2,
				},
			}
			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred()) // Expect validation error at API level
			Expect(err.Error()).To(ContainSubstring("spec.modelID"))
		})

	})

	Context("ServiceMonitor Watch", func() {
		var (
			controllerReconciler *VariantAutoscalingReconciler
			fakeRecorder         *record.FakeRecorder
		)

		BeforeEach(func() {
			logging.NewTestLogger()
			fakeRecorder = record.NewFakeRecorder(10)
			controllerReconciler = &VariantAutoscalingReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Recorder:  fakeRecorder,
				Datastore: datastore.NewDatastore(config.NewTestConfig()),
			}
		})

		Context("handleServiceMonitorEvent", func() {
			It("should log and emit event when ServiceMonitor is being deleted", func() {
				By("Creating a ServiceMonitor with deletion timestamp")
				now := metav1.Now()
				serviceMonitor := &promoperator.ServiceMonitor{
					ObjectMeta: metav1.ObjectMeta{
						Name:              defaultServiceMonitorName,
						Namespace:         config.SystemNamespace(),
						DeletionTimestamp: &now,
					},
				}

				By("Calling handleServiceMonitorEvent")
				result := controllerReconciler.handleServiceMonitorEvent(ctx, serviceMonitor)

				By("Verifying no reconciliation is triggered")
				Expect(result).To(BeEmpty())

				By("Verifying event was emitted")
				select {
				case event := <-fakeRecorder.Events:
					Expect(event).To(ContainSubstring("ServiceMonitorDeleted"))
					Expect(event).To(ContainSubstring(defaultServiceMonitorName))
				case <-time.After(2 * time.Second):
					Fail("Expected event to be emitted but none was received")
				}
			})

			It("should not emit event when ServiceMonitor is created", func() {
				By("Creating a ServiceMonitor without deletion timestamp")
				serviceMonitor := &promoperator.ServiceMonitor{
					ObjectMeta: metav1.ObjectMeta{
						Name:      defaultServiceMonitorName,
						Namespace: config.SystemNamespace(),
					},
				}

				By("Calling handleServiceMonitorEvent")
				result := controllerReconciler.handleServiceMonitorEvent(ctx, serviceMonitor)

				By("Verifying no reconciliation is triggered")
				Expect(result).To(BeEmpty())

				By("Verifying no error event was emitted")
				Consistently(fakeRecorder.Events).ShouldNot(Receive(ContainSubstring("ServiceMonitorDeleted")))
			})

			It("should handle non-ServiceMonitor objects gracefully", func() {
				By("Creating a non-ServiceMonitor object")
				configMap := &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-configmap",
						Namespace: config.SystemNamespace(),
					},
				}

				By("Calling handleServiceMonitorEvent with non-ServiceMonitor object")
				result := controllerReconciler.handleServiceMonitorEvent(ctx, configMap)

				By("Verifying no reconciliation is triggered")
				Expect(result).To(BeEmpty())
			})
		})
	})

	Context("Target Condition", func() {
		const resourceName = "target-condition-test"

		BeforeEach(func() {
			logging.NewTestLogger()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())
		})

		It("should set TargetResolved condition based on deployment existence", func() {
			By("Creating VariantAutoscaling without target deployment")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: resourceName,
					},
					ModelID:     "default-default",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			// Mock controller components
			controllerReconciler := &VariantAutoscalingReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Datastore: datastore.NewDatastore(config.NewTestConfig()),
			}

			By("Reconciling - expect TargetNotFound")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify condition
			fetchedResource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "default"}, fetchedResource)).To(Succeed())

			condition := llmdVariantAutoscalingV1alpha1.GetCondition(fetchedResource, llmdVariantAutoscalingV1alpha1.TypeTargetResolved)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(llmdVariantAutoscalingV1alpha1.ReasonTargetNotFound))

			By("Creating target deployment")
			deployment := resources.CreateLlmdSimDeployment("default", resourceName, "default-default", "default", "8000", 0, 0, 1)
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("Reconciling - expect TargetFound")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify condition
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: "default"}, fetchedResource)).To(Succeed())

			condition = llmdVariantAutoscalingV1alpha1.GetCondition(fetchedResource, llmdVariantAutoscalingV1alpha1.TypeTargetResolved)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(llmdVariantAutoscalingV1alpha1.ReasonTargetFound))

			// Cleanup
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})

	Context("When handling partial decisions from cache", func() {
		const resourceName = "test-partial-decision"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			logging.NewTestLogger()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required scale target ref deployment")
			deployment := resources.CreateLlmdSimDeployment("default", resourceName, "test-model", "default", "8000", 0, 0, 1)
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("creating the required configmap for optimization")
			configMap := testutils.CreateServiceClassConfigMap(ns.Name)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(config.DefaultConfigMapName, ns.Name)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).To(Succeed())

			By("creating the custom resource for the Kind VariantAutoscaling")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: resourceName,
					},
					ModelID:     "test-model",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance VariantAutoscaling")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Deleting the configmap resources")
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.DefaultConfigMapName,
					Namespace: config.SystemNamespace(),
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should handle partial decision without validation error", func() {
			By("Storing a partial decision in cache (no accelerator)")
			// Create a partial decision with only metrics status, no accelerator info
			partialDecision := interfaces.VariantDecision{
				VariantName:      resourceName,
				Namespace:        "default",
				MetricsAvailable: false,
				MetricsReason:    "MetricsUnavailable",
				MetricsMessage:   "Metrics are not yet available for this variant",
				// AcceleratorName and TargetReplicas are left at zero values (empty string and 0)
			}
			common.DecisionCache.Set(resourceName, "default", partialDecision)

			By("Reconciling the resource")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Recorder:  record.NewFakeRecorder(100),
				Config:    config.NewTestConfig(),
				Datastore: datastore.NewDatastore(config.NewTestConfig()),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred(), "Reconciliation should succeed without validation error")

			By("Verifying MetricsAvailable condition is updated")
			Eventually(func(g Gomega) {
				resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
				err := k8sClient.Get(ctx, typeNamespacedName, resource)
				g.Expect(err).NotTo(HaveOccurred())

				condition := llmdVariantAutoscalingV1alpha1.GetCondition(resource, llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable)
				g.Expect(condition).NotTo(BeNil(), "MetricsAvailable condition should be set")
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse), "MetricsAvailable should be False")
				g.Expect(condition.Reason).To(Equal("MetricsUnavailable"), "Reason should match partial decision")
			}, 5*time.Second, 500*time.Millisecond).Should(Succeed())

			By("Verifying DesiredOptimizedAlloc is not modified (remains empty)")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			err = k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			// DesiredOptimizedAlloc should remain at zero values (not set)
			Expect(resource.Status.DesiredOptimizedAlloc.Accelerator).To(BeEmpty(), "Accelerator should remain empty")
			Expect(resource.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(0), "NumReplicas should remain 0")
		})
	})

	// ConfigMap-related tests have been moved to configmap_handler_test.go

})
