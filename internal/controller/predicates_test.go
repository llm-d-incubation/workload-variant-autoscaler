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
	"fmt"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

var _ = Describe("ConfigMapPredicate - Custom Saturation ConfigMaps", func() {
	var (
		cfg *config.Config
		ds  datastore.Datastore
	)

	BeforeEach(func() {
		logging.NewTestLogger()
		cfg = config.NewTestConfig()
		ds = datastore.NewDatastore(cfg)
	})

	It("should allow a ConfigMap with the WVA ownership label in system namespace", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "custom-saturation-config",
				Namespace: config.SystemNamespace(),
				Labels: map[string]string{
					"app.kubernetes.io/name": "workload-variant-autoscaler",
				},
			},
		}
		predicateFn := ConfigMapPredicate(ds, cfg)
		result := predicateFn.Generic(event.GenericEvent{Object: cm})
		Expect(result).To(BeTrue(), "Predicate should allow WVA-labelled ConfigMap in system namespace")
	})

	It("should reject a ConfigMap without label and non-well-known name", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "some-random-configmap",
				Namespace: config.SystemNamespace(),
			},
		}
		predicateFn := ConfigMapPredicate(ds, cfg)
		result := predicateFn.Generic(event.GenericEvent{Object: cm})
		Expect(result).To(BeFalse(), "Predicate should reject ConfigMap without label and non-well-known name")
	})

	It("should reject a ConfigMap with wrong label value", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-config",
				Namespace: config.SystemNamespace(),
				Labels: map[string]string{
					"app.kubernetes.io/name": "some-other-app",
				},
			},
		}
		predicateFn := ConfigMapPredicate(ds, cfg)
		result := predicateFn.Generic(event.GenericEvent{Object: cm})
		Expect(result).To(BeFalse(), "Predicate should reject ConfigMap with wrong label value")
	})

	It("should allow a WVA-labelled ConfigMap in a tracked namespace", func() {
		ds.NamespaceTrack("VariantAutoscaling", "test-va", "my-namespace")
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "stack-a-saturation",
				Namespace: "my-namespace",
				Labels: map[string]string{
					"app.kubernetes.io/name": "workload-variant-autoscaler",
				},
			},
		}
		predicateFn := ConfigMapPredicate(ds, cfg)
		result := predicateFn.Generic(event.GenericEvent{Object: cm})
		Expect(result).To(BeTrue(), "Predicate should allow WVA-labelled ConfigMap in tracked namespace")
	})

	It("should reject a WVA-labelled ConfigMap in an untracked namespace", func() {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "stack-a-saturation",
				Namespace: "untracked-namespace",
				Labels: map[string]string{
					"app.kubernetes.io/name": "workload-variant-autoscaler",
				},
			},
		}
		predicateFn := ConfigMapPredicate(ds, cfg)
		result := predicateFn.Generic(event.GenericEvent{Object: cm})
		Expect(result).To(BeFalse(), "Predicate should reject WVA-labelled ConfigMap in untracked namespace")
	})
})

var _ = Describe("VariantAutoscalingPredicate", func() {
	var (
		ctx                    context.Context
		cfg                    *config.Config
		namespace1             string
		namespace2             string
		controllerInstanceOrig string
		testCounter            int
	)

	// Helper function to test predicate against an object
	testPredicate := func(predicateFn predicate.Predicate, obj client.Object) bool {
		genericEvent := event.GenericEvent{
			Object: obj,
		}
		return predicateFn.Generic(genericEvent)
	}

	BeforeEach(func() {
		ctx = context.Background()
		logging.NewTestLogger()

		// Use unique namespace names for each test to avoid conflicts
		testCounter++
		namespace1 = fmt.Sprintf("pred-test-ns-1-%d", testCounter)
		namespace2 = fmt.Sprintf("pred-test-ns-2-%d", testCounter)

		// Save original CONTROLLER_INSTANCE env var
		controllerInstanceOrig = os.Getenv("CONTROLLER_INSTANCE")

		// Ensure CONTROLLER_INSTANCE is unset for tests that don't need it
		_ = os.Unsetenv("CONTROLLER_INSTANCE")
		// Reinitialize metrics to ensure clean state
		_ = metrics.InitMetrics(prometheus.NewRegistry())

		// Create test config
		cfg = config.NewTestConfig()
	})

	AfterEach(func() {
		// Restore original CONTROLLER_INSTANCE env var
		if controllerInstanceOrig != "" {
			_ = os.Setenv("CONTROLLER_INSTANCE", controllerInstanceOrig)
		} else {
			_ = os.Unsetenv("CONTROLLER_INSTANCE")
		}

		// Clean up test namespaces (best effort, ignore errors)
		ns1 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace1}}
		_ = k8sClient.Delete(ctx, ns1)
		ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace2}}
		_ = k8sClient.Delete(ctx, ns2)
	})

	Context("Multi-namespace mode (no --watch-namespace)", func() {
		BeforeEach(func() {
			// Ensure watch namespace is not set
			cfg = config.NewTestConfig()
		})

		It("should allow VA in namespace without exclusion annotation", func() {
			By("Creating namespace without exclusion annotation")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA in namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeTrue(), "Predicate should allow VA in non-excluded namespace")
		})

		It("should filter out VA in namespace with exclusion annotation", func() {
			By("Creating namespace with exclusion annotation")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA in excluded namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeFalse(), "Predicate should filter out VA in excluded namespace")
		})

		It("should allow VA when exclusion annotation is not 'true'", func() {
			By("Creating namespace with exclusion annotation set to 'false'")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: "false",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA in namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeTrue(), "Predicate should allow VA when exclusion is not 'true'")
		})
	})

	Context("Single-namespace mode (--watch-namespace set)", func() {
		BeforeEach(func() {
			// Set watch namespace via environment variable
			Expect(os.Setenv("WATCH_NAMESPACE", namespace1)).To(Succeed())
			// Set dummy Prometheus URL for config load
			Expect(os.Setenv("PROMETHEUS_BASE_URL", "http://prometheus:9090")).To(Succeed())
			// Use Load() instead of NewTestConfig() to read from environment
			var err error
			cfg, err = config.Load(nil, "")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			_ = os.Unsetenv("WATCH_NAMESPACE")
			_ = os.Unsetenv("PROMETHEUS_BASE_URL")
		})

		It("should allow VA in watched namespace even with exclusion annotation", func() {
			By("Creating watched namespace with exclusion annotation")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA in watched namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeTrue(), "Predicate should allow VA in watched namespace even with exclusion")
		})

		It("should allow VA in watched namespace without exclusion annotation", func() {
			By("Creating watched namespace without exclusion annotation")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA in watched namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeTrue(), "Predicate should allow VA in watched namespace")
		})

		It("should filter out VA in non-watched namespace with exclusion annotation", func() {
			By("Creating non-watched namespace with exclusion annotation")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace2,
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA in non-watched namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace2,
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeFalse(), "Predicate should filter out VA in non-watched excluded namespace")
		})
	})

	Context("Controller instance filtering", func() {
		var controllerInstance string

		BeforeEach(func() {
			controllerInstance = "instance-1"
			Expect(os.Setenv("CONTROLLER_INSTANCE", controllerInstance)).To(Succeed())
			// Reinitialize metrics to pick up the new CONTROLLER_INSTANCE value
			_ = metrics.InitMetrics(prometheus.NewRegistry())
		})

		AfterEach(func() {
			_ = os.Unsetenv("CONTROLLER_INSTANCE")
			// Reinitialize metrics to clear the CONTROLLER_INSTANCE
			_ = metrics.InitMetrics(prometheus.NewRegistry())
		})

		It("should allow VA with matching controller-instance label in multi-namespace mode", func() {
			By("Creating namespace without exclusion")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA with matching controller-instance label")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
					Labels: map[string]string{
						constants.ControllerInstanceLabelKey: controllerInstance,
					},
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeTrue(), "Predicate should allow VA with matching controller-instance label")
		})

		It("should filter out VA with non-matching controller-instance label", func() {
			By("Creating namespace without exclusion")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA with non-matching controller-instance label")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
					Labels: map[string]string{
						constants.ControllerInstanceLabelKey: "instance-2",
					},
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeFalse(), "Predicate should filter out VA with non-matching controller-instance label")
		})

		It("should filter out VA without controller-instance label when CONTROLLER_INSTANCE is set", func() {
			By("Creating namespace without exclusion")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA without controller-instance label")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeFalse(), "Predicate should filter out VA without controller-instance label")
		})

		It("should apply controller-instance filtering in single-namespace mode", func() {
			By("Setting watch namespace")
			Expect(os.Setenv("WATCH_NAMESPACE", namespace1)).To(Succeed())
			Expect(os.Setenv("PROMETHEUS_BASE_URL", "http://prometheus:9090")).To(Succeed())
			// Use Load() to read from environment
			var err error
			cfg, err = config.Load(nil, "")
			Expect(err).NotTo(HaveOccurred())

			By("Creating watched namespace with exclusion annotation")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA with matching controller-instance label")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
					Labels: map[string]string{
						constants.ControllerInstanceLabelKey: controllerInstance,
					},
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeTrue(), "Predicate should allow VA with matching controller-instance in watched namespace")

			_ = os.Unsetenv("WATCH_NAMESPACE")
			_ = os.Unsetenv("PROMETHEUS_BASE_URL")
		})
	})

	Context("Edge cases", func() {
		It("should handle nil config gracefully", func() {
			By("Creating namespace without exclusion")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespace1,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Creating VA")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: namespace1,
				},
			}

			By("Applying predicate with nil config")
			predicateFn := VariantAutoscalingPredicate(k8sClient, nil)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeTrue(), "Predicate should allow VA when config is nil")
		})

		It("should fail open when namespace fetch fails", func() {
			By("Creating VA in non-existent namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "non-existent-namespace",
				},
			}

			By("Applying predicate")
			predicateFn := VariantAutoscalingPredicate(k8sClient, cfg)
			result := testPredicate(predicateFn, va)
			Expect(result).To(BeTrue(), "Predicate should fail open when namespace doesn't exist")
		})
	})
})
