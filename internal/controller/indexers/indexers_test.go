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

package indexers

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	llmdv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var _ = Describe("Indexers", Ordered, func() {
	var (
		testCtx   context.Context
		cancel    context.CancelFunc
		namespace string
		mgr       manager.Manager
		mgrClient client.Client
	)

	BeforeAll(func() {
		testCtx, cancel = context.WithCancel(context.Background()) //nolint:fatcontext // shared across BeforeAll/AfterAll
		namespace = fmt.Sprintf("test-indexers-%d", GinkgoRandomSeed())

		// Create the test namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())

		// Create a manager with indexes for testing
		var err error
		mgr, err = manager.New(cfg, manager.Options{
			Metrics: metricsserver.Options{BindAddress: "0"}, // disable metrics server in tests
		})
		Expect(err).NotTo(HaveOccurred())

		// Setup indexes
		err = SetupIndexes(testCtx, mgr)
		Expect(err).NotTo(HaveOccurred())

		// Start the manager's cache
		go func() {
			defer GinkgoRecover()
			_ = mgr.Start(testCtx)
		}()

		// Wait for cache to sync
		Expect(mgr.GetCache().WaitForCacheSync(testCtx)).To(BeTrue())
		mgrClient = mgr.GetClient()
	})

	AfterAll(func() {
		// Cancel the context to stop the manager goroutine
		cancel()

		// Clean up the namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), ns))).To(Succeed())
	})

	Describe("SetupIndexes", func() {
		It("should register VA scale target index successfully", func() {
			// The indexes are set up in the BeforeAll
			// If we got here without error, the indexes were registered successfully
			Expect(mgr).NotTo(BeNil())
		})
	})

	Describe("FindVAForDeployment", func() {
		var (
			deploymentName string
			va1            *llmdv1alpha1.VariantAutoscaling
			vaOther        *llmdv1alpha1.VariantAutoscaling
		)

		BeforeEach(func() {
			deploymentName = "test-deployment"

			// Create a VA targeting the Deployment
			va1 = &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-1",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
					ModelID:     "model-1",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, va1)).To(Succeed())

			// Create a VA targeting a different Deployment
			vaOther = &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-other",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       "other-deployment",
					},
					ModelID:     "model-other",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, vaOther)).To(Succeed())
		})

		AfterEach(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, va1))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, vaOther))).To(Succeed())
		})

		It("should return VA targeting a specific deployment", func() {
			Eventually(func() string {
				va, err := FindVAForDeployment(testCtx, mgrClient, deploymentName, namespace)
				if err != nil || va == nil {
					return ""
				}
				return va.Name
			}).Should(Equal("va-1"))

			va, err := FindVAForDeployment(testCtx, mgrClient, deploymentName, namespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(va).NotTo(BeNil())
			Expect(va.Name).To(Equal("va-1"))
		})

		It("should return nil for non-existent deployment", func() {
			va, err := FindVAForDeployment(testCtx, mgrClient, "non-existent", namespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(va).To(BeNil())
		})

		It("should not return VAs from other namespaces", func() {
			// Create a VA in a different Namespace
			otherNs := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace + "-other",
				},
			}
			Expect(k8sClient.Create(testCtx, otherNs)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, otherNs))).To(Succeed())
			}()

			vaOtherNs := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-other-ns",
					Namespace: otherNs.Name,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName, // Same deployment name but different namespace
					},
					ModelID:     "model-other-ns",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, vaOtherNs)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, vaOtherNs))).To(Succeed())
			}()

			// Should only return VA from the specified namespace
			Eventually(func() string {
				va, err := FindVAForDeployment(testCtx, mgrClient, deploymentName, namespace)
				if err != nil || va == nil {
					return ""
				}
				return va.Name
			}).Should(Equal("va-1"))
		})
	})

	Describe("FindVAForLeaderWorkerSet", func() {
		var (
			lwsName string
			va1     *llmdv1alpha1.VariantAutoscaling
			vaOther *llmdv1alpha1.VariantAutoscaling
		)

		BeforeEach(func() {
			lwsName = "test-lws"

			// Create a VA targeting the LeaderWorkerSet
			va1 = &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-lws-1",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: constants.LeaderWorkerSetAPIVersion,
						Kind:       constants.LeaderWorkerSetKind,
						Name:       lwsName,
					},
					ModelID:     "model-lws-1",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, va1)).To(Succeed())

			// Create a VA targeting a different LeaderWorkerSet
			vaOther = &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-lws-other",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: constants.LeaderWorkerSetAPIVersion,
						Kind:       constants.LeaderWorkerSetKind,
						Name:       "other-lws",
					},
					ModelID:     "model-lws-other",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, vaOther)).To(Succeed())
		})

		AfterEach(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, va1))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, vaOther))).To(Succeed())
		})

		It("should return VA targeting a specific LeaderWorkerSet", func() {
			Eventually(func() string {
				va, err := FindVAForLeaderWorkerSet(testCtx, mgrClient, lwsName, namespace)
				if err != nil || va == nil {
					return ""
				}
				return va.Name
			}).Should(Equal("va-lws-1"))

			va, err := FindVAForLeaderWorkerSet(testCtx, mgrClient, lwsName, namespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(va).NotTo(BeNil())
			Expect(va.Name).To(Equal("va-lws-1"))
		})

		It("should return nil for non-existent LeaderWorkerSet", func() {
			va, err := FindVAForLeaderWorkerSet(testCtx, mgrClient, "non-existent-lws", namespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(va).To(BeNil())
		})

		It("should not return VAs from other namespaces", func() {
			// Create a VA in a different Namespace
			otherNs := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace + "-lws-other",
				},
			}
			Expect(k8sClient.Create(testCtx, otherNs)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, otherNs))).To(Succeed())
			}()

			vaOtherNs := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-lws-other-ns",
					Namespace: otherNs.Name,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: constants.LeaderWorkerSetAPIVersion,
						Kind:       constants.LeaderWorkerSetKind,
						Name:       lwsName, // Same LWS name but different namespace
					},
					ModelID:     "model-lws-other-ns",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, vaOtherNs)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, vaOtherNs))).To(Succeed())
			}()

			// Should only return VA from the specified namespace
			Eventually(func() string {
				va, err := FindVAForLeaderWorkerSet(testCtx, mgrClient, lwsName, namespace)
				if err != nil || va == nil {
					return ""
				}
				return va.Name
			}).Should(Equal("va-lws-1"))
		})

		It("should distinguish LeaderWorkerSet from Deployment with the same name", func() {
			sharedName := "my-workload-lws"

			// VA targeting a LeaderWorkerSet named "my-workload-lws"
			vaLWS := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-targets-lws",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: constants.LeaderWorkerSetAPIVersion,
						Kind:       constants.LeaderWorkerSetKind,
						Name:       sharedName,
					},
					ModelID:     "model-lws",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, vaLWS)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, vaLWS))).To(Succeed())
			}()

			// VA targeting a Deployment named "my-workload-lws" - same name, different kind
			vaDeployment := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-targets-deployment-lws",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       sharedName,
					},
					ModelID:     "model-deploy",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, vaDeployment)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, vaDeployment))).To(Succeed())
			}()

			// FindVAForLeaderWorkerSet should return the LeaderWorkerSet-targeting VA
			Eventually(func() string {
				va, err := FindVAForLeaderWorkerSet(testCtx, mgrClient, sharedName, namespace)
				if err != nil || va == nil {
					return ""
				}
				return va.Name
			}).Should(Equal("va-targets-lws"))

			// FindVAForDeployment should return the Deployment-targeting VA
			Eventually(func() string {
				va, err := FindVAForDeployment(testCtx, mgrClient, sharedName, namespace)
				if err != nil || va == nil {
					return ""
				}
				return va.Name
			}).Should(Equal("va-targets-deployment-lws"))
		})

		It("should match VAs with explicit LeaderWorkerSet APIVersion", func() {
			lwsNameAPI := "test-lws-apiversion"

			va := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-lws-with-apiversion",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: constants.LeaderWorkerSetAPIVersion,
						Kind:       constants.LeaderWorkerSetKind,
						Name:       lwsNameAPI,
					},
					ModelID:     "model-lws-apiversion",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, va)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, va))).To(Succeed())
			}()

			// FindVAForLeaderWorkerSet uses LeaderWorkerSetAPIVersion by default
			Eventually(func() string {
				found, err := FindVAForLeaderWorkerSet(testCtx, mgrClient, lwsNameAPI, namespace)
				if err != nil || found == nil {
					return ""
				}
				return found.Name
			}).Should(Equal("va-lws-with-apiversion"))
		})

		It("should match VAs without APIVersion (defaults to leaderworkerset.x-k8s.io/v1 for LeaderWorkerSet)", func() {
			lwsNameNoAPI := "test-lws-no-apiversion"

			va := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-lws-without-apiversion",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						// APIVersion is not set - should default to leaderworkerset.x-k8s.io/v1
						Kind: constants.LeaderWorkerSetKind,
						Name: lwsNameNoAPI,
					},
					ModelID:     "model-lws-no-apiversion",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, va)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, va))).To(Succeed())
			}()

			// FindVAForLeaderWorkerSet should still find it since it defaults to LeaderWorkerSetAPIVersion
			Eventually(func() string {
				found, err := FindVAForLeaderWorkerSet(testCtx, mgrClient, lwsNameNoAPI, namespace)
				if err != nil || found == nil {
					return ""
				}
				return found.Name
			}).Should(Equal("va-lws-without-apiversion"))
		})
	})

	Describe("FindVAForScaleTarget", func() {
		It("should distinguish between different resource kinds with the same name", func() {
			sharedName := "my-workload"

			// VA targeting a Deployment named "my-workload"
			vaDeployment := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-targets-deployment",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       sharedName,
					},
					ModelID:     "model-deploy",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, vaDeployment)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, vaDeployment))).To(Succeed())
			}()

			// VA targeting a StatefulSet named "my-workload" - same name, different kind
			vaStatefulSet := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-targets-statefulset",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "StatefulSet",
						Name:       sharedName,
					},
					ModelID:     "model-sts",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, vaStatefulSet)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, vaStatefulSet))).To(Succeed())
			}()

			// FindVAForDeployment should return the Deployment-targeting VA
			Eventually(func() string {
				va, err := FindVAForDeployment(testCtx, mgrClient, sharedName, namespace)
				if err != nil || va == nil {
					return ""
				}
				return va.Name
			}).Should(Equal("va-targets-deployment"))

			// FindVAForScaleTarget with StatefulSet should return the StatefulSet-targeting VA
			Eventually(func() string {
				va, err := FindVAForScaleTarget(testCtx, mgrClient, autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       sharedName,
				}, namespace)
				if err != nil || va == nil {
					return ""
				}
				return va.Name
			}).Should(Equal("va-targets-statefulset"))
		})

		It("should return an error when multiple VAs target the same scale target", func() {
			sharedName := "dup-target"

			va1 := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-dup-1",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       sharedName,
					},
					ModelID:     "model-dup-1",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, va1)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, va1))).To(Succeed())
			}()

			va2 := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-dup-2",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       sharedName,
					},
					ModelID:     "model-dup-2",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, va2)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, va2))).To(Succeed())
			}()

			Eventually(func() error {
				_, err := FindVAForScaleTarget(testCtx, mgrClient, autoscalingv2.CrossVersionObjectReference{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       sharedName,
				}, namespace)
				return err
			}).Should(MatchError(ContainSubstring("multiple VariantAutoscalings found")))
		})
	})

	Describe("APIVersion handling", func() {
		It("should match VAs with explicit APIVersion", func() {
			deploymentName := "test-deploy-apiversion"

			va := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-with-apiversion",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						APIVersion: "apps/v1",
						Kind:       "Deployment",
						Name:       deploymentName,
					},
					ModelID:     "model-apiversion",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, va)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, va))).To(Succeed())
			}()

			// FindVAForDeployment uses apps/v1 by default
			Eventually(func() string {
				found, err := FindVAForDeployment(testCtx, mgrClient, deploymentName, namespace)
				if err != nil || found == nil {
					return ""
				}
				return found.Name
			}).Should(Equal("va-with-apiversion"))
		})

		It("should match VAs without APIVersion (defaults to apps/v1 for Deployment)", func() {
			deploymentName := "test-deploy-no-apiversion"

			va := &llmdv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "va-without-apiversion",
					Namespace: namespace,
				},
				Spec: llmdv1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
						// APIVersion is not set - should default to apps/v1
						Kind: "Deployment",
						Name: deploymentName,
					},
					ModelID:     "model-no-apiversion",
					MaxReplicas: 2,
				},
			}
			Expect(k8sClient.Create(testCtx, va)).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, va))).To(Succeed())
			}()

			// FindVAForDeployment should still find it since it defaults to apps/v1
			Eventually(func() string {
				found, err := FindVAForDeployment(testCtx, mgrClient, deploymentName, namespace)
				if err != nil || found == nil {
					return ""
				}
				return found.Name
			}).Should(Equal("va-without-apiversion"))
		})
	})
})
