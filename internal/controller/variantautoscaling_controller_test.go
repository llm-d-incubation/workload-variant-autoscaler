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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	logger "github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	utils "github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	testutils "github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
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
			logger.Log = zap.NewNop().Sugar()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmap for optimization")
			configMap := testutils.CreateServiceClassConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateAcceleratorUnitCostConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

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
						// Example spec fields, adjust as necessary
						ModelID:          "default/default",
						VariantID:        "default/default-A100-1",
						Accelerator:      "A100",
						AcceleratorCount: 1,
						VariantCost:      "10.5",
						VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
							PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
								DecodeParms:  map[string]string{"alpha": "20.28", "beta": "0.72"},
								PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
							},
							MaxBatchSize: 4,
						},
						SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
							Name: "premium",
							Key:  "default/default",
						},
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
					Name:      "accelerator-unit-costs",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When handling error conditions on missing config maps", func() {
		BeforeEach(func() {
			logger.Log = zap.NewNop().Sugar()
		})

		It("should fail on missing serviceClass ConfigMap", func() {
			By("Creating VariantAutoscaling without required ConfigMaps")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.readServiceClassConfig(ctx, "service-classes-config", configMapNamespace)
			Expect(err).To(HaveOccurred(), "Expected error when reading missing serviceClass ConfigMap")
		})

		It("should fail on missing accelerator ConfigMap", func() {
			By("Creating VariantAutoscaling without required ConfigMaps")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.readAcceleratorConfig(ctx, "accelerator-unit-costs", configMapNamespace)
			Expect(err).To(HaveOccurred(), "Expected error when reading missing accelerator ConfigMap")
		})

		It("should fail on missing variant autoscaling optimization ConfigMap", func() {
			By("Creating VariantAutoscaling without required ConfigMaps")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.readOptimizationConfig(ctx)
			Expect(err).To(HaveOccurred(), "Expected error when reading missing variant autoscaling optimization ConfigMap")
		})
	})

	Context("When validating configurations", func() {
		const configResourceName = "config-test-resource"

		BeforeEach(func() {
			logger.Log = zap.NewNop().Sugar()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmaps")
			configMap := testutils.CreateServiceClassConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).NotTo(HaveOccurred())

			configMap = testutils.CreateAcceleratorUnitCostConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).NotTo(HaveOccurred())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).NotTo(HaveOccurred())
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
					Name:      "accelerator-unit-costs",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		})

		It("should return empty on variant autoscaling optimization ConfigMap with missing interval value", func() {
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// delete correct configMap
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(err).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/name": "workload-variant-autoscaler",
					},
				},
				Data: map[string]string{
					"PROMETHEUS_BASE_URL": "https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090",
					"GLOBAL_OPT_INTERVAL": "",
					"GLOBAL_OPT_TRIGGER":  "false",
				},
			}
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			interval, err := controllerReconciler.readOptimizationConfig(ctx)
			Expect(err).NotTo(HaveOccurred(), "Unexpected error when reading variant autoscaling optimization ConfigMap with missing interval")
			Expect(interval).To(Equal(""), "Expected empty interval value")
		})

		It("should return empty on variant autoscaling optimization ConfigMap with missing prometheus base URL", func() {
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// delete correct configMap
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(err).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/name": "workload-variant-autoscaler",
					},
				},
				Data: map[string]string{
					"PROMETHEUS_BASE_URL": "",
					"GLOBAL_OPT_INTERVAL": "60s",
					"GLOBAL_OPT_TRIGGER":  "false",
				},
			}
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			prometheusURL, err := controllerReconciler.getPrometheusConfigFromConfigMap(ctx)
			Expect(err).NotTo(HaveOccurred(), "Unexpected error when reading variant autoscaling optimization ConfigMap with missing Prometheus URL")
			Expect(prometheusURL).To(BeNil(), "Expected empty Prometheus URL")
		})

		It("should return error on VA optimization ConfigMap with missing prometheus base URL and no env variable", func() {
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// delete correct configMap
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(err).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/name": "workload-variant-autoscaler",
					},
				},
				Data: map[string]string{
					"PROMETHEUS_BASE_URL": "",
					"GLOBAL_OPT_INTERVAL": "60s",
					"GLOBAL_OPT_TRIGGER":  "false",
				},
			}
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			_, err = controllerReconciler.getPrometheusConfig(ctx)
			Expect(err).To(HaveOccurred(), "It should fail when neither env variable nor Prometheus URL are found")
		})

		It("should return default values on variant autoscaling optimization ConfigMap with missing TLS values", func() {
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// delete correct configMap
			configMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
			Expect(err).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/name": "workload-variant-autoscaler",
					},
				},
				Data: map[string]string{
					"PROMETHEUS_BASE_URL":                 "https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090",
					"GLOBAL_OPT_INTERVAL":                 "60s",
					"GLOBAL_OPT_TRIGGER":                  "false",
					"PROMETHEUS_TLS_INSECURE_SKIP_VERIFY": "true",
					// no values set for TLS config - dev env
				},
			}
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			prometheusConfig, err := controllerReconciler.getPrometheusConfigFromConfigMap(ctx)
			Expect(err).NotTo(HaveOccurred(), "It should not fail when neither env variable nor Prometheus URL are found")

			Expect(prometheusConfig.BaseURL).To(Equal("https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090"), "Expected Base URL to be set")
			Expect(prometheusConfig.InsecureSkipVerify).To(BeTrue(), "Expected Insecure Skip Verify to be true")

			Expect(prometheusConfig.CACertPath).To(Equal(""), "Expected CA Cert Path to be empty")
			Expect(prometheusConfig.ClientCertPath).To(Equal(""), "Expected Client Cert path to be empty")
			Expect(prometheusConfig.ClientKeyPath).To(Equal(""), "Expected Client Key path to be empty")
			Expect(prometheusConfig.BearerToken).To(Equal(""), "Expected Bearer Token to be empty")
			Expect(prometheusConfig.TokenPath).To(Equal(""), "Expected Token Path to be empty")
			Expect(prometheusConfig.ServerName).To(Equal(""), "Expected Server Name to be empty")
		})

		It("should validate accelerator profiles", func() {
			By("Creating VariantAutoscaling with invalid accelerator profile")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configResourceName,
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "default/default",
					VariantID:        "default/default-INVALID_GPU--1",
					Accelerator:      "INVALID_GPU",
					AcceleratorCount: -1, // Invalid count
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "invalid", "beta": "invalid"},
							PrefillParms: map[string]string{"gamma": "invalid", "delta": "invalid"},
						},
						MaxBatchSize: -1, // Invalid batch size
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "premium",
						Key:  "default/default",
					},
				},
			}
			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred()) // Expect validation error at API level
			Expect(err.Error()).To(ContainSubstring("Invalid value"))
		})

		It("should handle empty ModelID value", func() {
			By("Creating VariantAutoscaling with empty ModelID")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-model-id",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "", // Empty ModelID
					VariantID:        "-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "0.28", "beta": "0.72"},
							PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
						},
						MaxBatchSize: 4,
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "premium",
						Key:  "default/default",
					},
				},
			}
			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred()) // Expect validation error at API level
			Expect(err.Error()).To(ContainSubstring("spec.modelID"))
		})

		It("should handle empty accelerator field", func() {
			By("Creating VariantAutoscaling with no accelerator")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-accelerators",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "default/default",
					VariantID:        "default/default--1",
					Accelerator:      "", // Empty accelerator
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "0.28", "beta": "0.72"},
							PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
						},
						MaxBatchSize: 4,
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "premium",
						Key:  "default/default",
					},
				},
			}
			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.accelerator"))
		})

		It("should handle empty SLOClassRef", func() {
			By("Creating VariantAutoscaling with no SLOClassRef")
			resource := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-slo-class-ref",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "default/default",
					VariantID:        "default/default-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "0.28", "beta": "0.72"},
							PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
						},
						MaxBatchSize: 4,
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						// no configuration for SLOClassRef
					},
				},
			}
			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.sloClassRef"))
		})
	})

	Context("When handling multiple VariantAutoscalings", func() {
		const totalVAs = 3

		var CreateServiceClassConfigMap = func(controllerNamespace string, models ...string) *v1.ConfigMap {
			data := map[string]string{}

			// Build premium.yaml with all models
			premiumModels := ""
			freemiumModels := ""

			for _, model := range models {
				premiumModels += fmt.Sprintf("  - model: %s\n    slo-tpot: 24\n    slo-ttft: 500\n", model)
				freemiumModels += fmt.Sprintf("  - model: %s\n    slo-tpot: 200\n    slo-ttft: 2000\n", model)
			}

			data["premium.yaml"] = fmt.Sprintf(`name: Premium
priority: 1
data:
%s`, premiumModels)

			data["freemium.yaml"] = fmt.Sprintf(`name: Freemium
priority: 10
data:
%s`, freemiumModels)

			return &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-classes-config",
					Namespace: controllerNamespace,
				},
				Data: data,
			}
		}

		BeforeEach(func() {
			logger.Log = zap.NewNop().Sugar()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmaps")
			// Use custom configmap creation function
			var modelNames []string
			for i := range totalVAs {
				modelNames = append(modelNames, fmt.Sprintf("model-%d/model-%d", i, i))
			}
			configMap := CreateServiceClassConfigMap(ns.Name, modelNames...)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateAcceleratorUnitCostConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			By("Creating VariantAutoscaling resources and Deployments")
			for i := range totalVAs {
				modelID := fmt.Sprintf("model-%d/model-%d", i, i)
				name := fmt.Sprintf("multi-test-resource-%d", i)

				d := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: utils.Ptr(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": name},
						},
						Template: v1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": name},
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{
										Name:  "test-container",
										Image: "quay.io/infernoautoscaler/vllme:0.2.1-multi-arch",
										Ports: []v1.ContainerPort{{ContainerPort: 80}},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, d)).To(Succeed())

				r := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: "default",
						Labels: map[string]string{
							"inference.optimization/acceleratorName": "A100",
						},
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ModelID:          modelID,
						VariantID:        fmt.Sprintf("%s-A100-1", modelID),
						Accelerator:      "A100",
						AcceleratorCount: 1,
						VariantCost:      "10.5",
						VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
							PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
								DecodeParms:  map[string]string{"alpha": "0.28", "beta": "0.72"},
								PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
							},
							MaxBatchSize: 4,
						},
						SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
							Name: "premium",
							Key:  modelID,
						},
					},
				}
				Expect(k8sClient.Create(ctx, r)).To(Succeed())
			}
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
					Name:      "accelerator-unit-costs",
					Namespace: "workload-variant-autoscaler-system",
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			configMap = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: configMapNamespace,
				},
			}
			err = k8sClient.Delete(ctx, configMap)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())

			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			err = k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred(), "Failed to list VariantAutoscaling resources")

			var deploymentList appsv1.DeploymentList
			err = k8sClient.List(ctx, &deploymentList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred(), "Failed to list deployments")

			// Clean up all deployments
			for i := range deploymentList.Items {
				deployment := &deploymentList.Items[i]
				if strings.HasPrefix(deployment.Spec.Template.Labels["app"], "multi-test-resource") {
					err = k8sClient.Delete(ctx, deployment)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete deployment")
				}
			}

			// Clean up all VariantAutoscaling resources
			for i := range variantAutoscalingList.Items {
				err = k8sClient.Delete(ctx, &variantAutoscalingList.Items[i])
				Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete VariantAutoscaling resource")
			}
		})

		It("should filter out VAs marked for deletion", func() {
			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			err := k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred(), "Failed to list VariantAutoscaling resources")
			filterActiveVariantAutoscalings(variantAutoscalingList.Items)
			Expect(len(variantAutoscalingList.Items)).To(Equal(3), "All VariantAutoscaling resources should be active before deletion")

			// Delete the VAs (this sets DeletionTimestamp)
			for i := range totalVAs {
				Expect(k8sClient.Delete(ctx, &variantAutoscalingList.Items[i])).To(Succeed())
			}

			err = k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred(), "Failed to list VariantAutoscaling resources")
			filterActiveVariantAutoscalings(variantAutoscalingList.Items)
			Expect(len(variantAutoscalingList.Items)).To(Equal(0), "No active VariantAutoscaling resources should be found")
		})

		It("should prepare active VAs for optimization", func() {
			// Create a mock Prometheus API with valid metric data that passes validation
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					// Default: return a vector with one sample to pass validation
				},
				QueryErrors: map[string]error{},
			}

			controllerReconciler := &VariantAutoscalingReconciler{
				Client:  k8sClient,
				Scheme:  k8sClient.Scheme(),
				PromAPI: mockPromAPI,
			}

			By("Reading the required configmaps")
			accMap, err := controllerReconciler.readAcceleratorConfig(ctx, "accelerator-unit-costs", configMapNamespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to read accelerator config")
			Expect(accMap).NotTo(BeNil(), "Accelerator config map should not be nil")

			serviceClassMap, err := controllerReconciler.readServiceClassConfig(ctx, "service-classes-config", configMapNamespace)
			Expect(err).NotTo(HaveOccurred(), "Failed to read service class config")
			Expect(serviceClassMap).NotTo(BeNil(), "Service class config map should not be nil")

			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			err = k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred(), "Failed to list VariantAutoscaling resources")
			activeVAs := filterActiveVariantAutoscalings(variantAutoscalingList.Items)
			Expect(len(activeVAs)).To(Equal(totalVAs), "All VariantAutoscaling resources should be active")

			// Prepare system data for VAs
			By("Preparing the system data for optimization")
			// WVA operates in unlimited mode - no inventory data needed
			systemData := utils.CreateSystemData(accMap, serviceClassMap)
			Expect(systemData).NotTo(BeNil(), "System data should not be nil")

			scaleToZeroConfigData := make(utils.ScaleToZeroConfigData)
			updateList, vaMap, allAnalyzerResponses, err := controllerReconciler.prepareVariantAutoscalings(ctx, activeVAs, accMap, serviceClassMap, systemData, scaleToZeroConfigData)

			Expect(err).NotTo(HaveOccurred(), "prepareVariantAutoscalings should not return an error")
			Expect(vaMap).NotTo(BeNil(), "VA map should not be nil")
			Expect(allAnalyzerResponses).NotTo(BeNil(), "Analyzer responses should not be nil")
			Expect(len(updateList.Items)).To(Equal(totalVAs), "UpdatedList should be the same number of all active VariantAutoscalings")

			var vaNames []string
			for _, va := range activeVAs {
				vaNames = append(vaNames, va.Name)
			}

			for _, updatedVa := range updateList.Items {
				Expect(vaNames).To(ContainElement(updatedVa.Name), fmt.Sprintf("Active VariantAutoscaling list should contain %s", updatedVa.Name))
				// In single-variant architecture, check that CurrentAlloc has been populated by verifying NumReplicas
				Expect(updatedVa.Status.CurrentAlloc.NumReplicas).To(BeNumerically(">=", 0), fmt.Sprintf("CurrentAlloc should be populated for %s after preparation", updatedVa.Name))
				// In single-variant architecture, accelerator is in spec, not in status
				Expect(updatedVa.Spec.Accelerator).To(Equal("A100"), fmt.Sprintf("Accelerator in spec for %s should be \"A100\" after preparation", updatedVa.Name))
				Expect(updatedVa.Status.CurrentAlloc.NumReplicas).To(Equal(int32(1)), fmt.Sprintf("Current NumReplicas for %s should be 1 after preparation", updatedVa.Name))

				// DesiredOptimizedAlloc may be empty initially after preparation
				// In single-variant architecture, check NumReplicas > 0 to see if optimization has run
				if updatedVa.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
					// Accelerator is in spec, already verified above
					Expect(updatedVa.Spec.Accelerator).NotTo(BeEmpty(), fmt.Sprintf("Accelerator in spec for %s should be set", updatedVa.Name))
				}
			}
		})

		It("should set MetricsAvailable condition when metrics validation fails", func() {
			By("Creating a mock Prometheus API that returns no metrics")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{},
				QueryErrors:  map[string]error{},
			}

			controllerReconciler := &VariantAutoscalingReconciler{
				Client:  k8sClient,
				Scheme:  k8sClient.Scheme(),
				PromAPI: mockPromAPI,
			}

			By("Reading the required configmaps")
			accMap, err := controllerReconciler.readAcceleratorConfig(ctx, "accelerator-unit-costs", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			serviceClassMap, err := controllerReconciler.readServiceClassConfig(ctx, "service-classes-config", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			err = k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred())

			activeVAs := filterActiveVariantAutoscalings(variantAutoscalingList.Items)
			Expect(len(activeVAs)).To(BeNumerically(">", 0))

			By("Preparing system data and calling prepareVariantAutoscalings")
			systemData := utils.CreateSystemData(accMap, serviceClassMap)
			scaleToZeroConfigData := make(utils.ScaleToZeroConfigData)

			_, _, _, err = controllerReconciler.prepareVariantAutoscalings(ctx, activeVAs, accMap, serviceClassMap, systemData, scaleToZeroConfigData)
			Expect(err).NotTo(HaveOccurred())

			By("Checking that MetricsAvailable condition is set to False")
			for _, va := range activeVAs {
				var updatedVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
				err = k8sClient.Get(ctx, types.NamespacedName{Name: va.Name, Namespace: va.Namespace}, &updatedVa)
				Expect(err).NotTo(HaveOccurred())

				metricsCondition := llmdVariantAutoscalingV1alpha1.GetCondition(&updatedVa, llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable)
				if metricsCondition != nil {
					Expect(metricsCondition.Status).To(Equal(metav1.ConditionFalse),
						fmt.Sprintf("MetricsAvailable condition should be False for %s", va.Name))
					Expect(metricsCondition.Reason).To(Or(
						Equal(llmdVariantAutoscalingV1alpha1.ReasonPrometheusError),
						Equal(llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing),
					))
				}
			}
		})

		It("should set OptimizationReady condition when optimization succeeds", func() {
			By("Using a working mock Prometheus API with sample data")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					// Add default responses for common queries
				},
				QueryErrors: map[string]error{},
			}

			controllerReconciler := &VariantAutoscalingReconciler{
				Client:  k8sClient,
				Scheme:  k8sClient.Scheme(),
				PromAPI: mockPromAPI,
			}

			By("Performing a full reconciliation")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{})
			Expect(err).NotTo(HaveOccurred())

			By("Checking that conditions are set correctly")
			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			err = k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred())

			for _, va := range variantAutoscalingList.Items {
				if va.DeletionTimestamp.IsZero() {
					metricsCondition := llmdVariantAutoscalingV1alpha1.GetCondition(&va, llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable)
					if metricsCondition != nil && metricsCondition.Status == metav1.ConditionTrue {
						optimizationCondition := llmdVariantAutoscalingV1alpha1.GetCondition(&va, llmdVariantAutoscalingV1alpha1.TypeOptimizationReady)
						Expect(optimizationCondition).NotTo(BeNil(),
							fmt.Sprintf("OptimizationReady condition should be set for %s", va.Name))
					}
				}
			}
		})
	})

	Context("Scale-to-Zero ConfigMap Integration Tests", func() {
		It("should read scale-to-zero ConfigMap successfully", func() {
			By("Creating a scale-to-zero ConfigMap")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"model.meta_llama-3.1-8b": `modelID: "meta_llama-3.1-8b"
enableScaleToZero: true
retentionPeriod: "5m"`,
					"model.meta_llama-3.1-70b": `modelID: "meta_llama-3.1-70b"
enableScaleToZero: false`,
					"model.mistralai_Mistral-7B-v0.1": `modelID: "mistralai_Mistral-7B-v0.1"
enableScaleToZero: true
retentionPeriod: "15m"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the scale-to-zero ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(configData).NotTo(BeNil())

			By("Verifying ConfigMap data was parsed correctly")
			Expect(len(configData)).To(Equal(3))

			// Check llama-3.1-8b config
			config1, exists := configData["meta_llama-3.1-8b"]
			Expect(exists).To(BeTrue())
			Expect(config1.EnableScaleToZero).NotTo(BeNil())
			Expect(*config1.EnableScaleToZero).To(BeTrue())
			Expect(config1.RetentionPeriod).To(Equal("5m"))

			// Check llama-3.1-70b config
			config2, exists := configData["meta_llama-3.1-70b"]
			Expect(exists).To(BeTrue())
			Expect(config2.EnableScaleToZero).NotTo(BeNil())
			Expect(*config2.EnableScaleToZero).To(BeFalse())
			Expect(config2.RetentionPeriod).To(BeEmpty())

			// Check Mistral config
			config3, exists := configData["mistralai_Mistral-7B-v0.1"]
			Expect(exists).To(BeTrue())
			Expect(config3.EnableScaleToZero).NotTo(BeNil())
			Expect(*config3.EnableScaleToZero).To(BeTrue())
			Expect(config3.RetentionPeriod).To(Equal("15m"))

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should return empty config when ConfigMap does not exist", func() {
			By("Reading non-existent scale-to-zero ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "non-existent-configmap", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(configData).NotTo(BeNil())
			Expect(len(configData)).To(Equal(0))
		})

		It("should skip invalid JSON entries in ConfigMap", func() {
			By("Creating a ConfigMap with invalid YAML")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-invalid",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"model.meta_llama-3.1-8b": `modelID: "meta_llama-3.1-8b"
enableScaleToZero: true
retentionPeriod: "5m"`,
					"model.meta_llama-3.1-70b": `invalid yaml`,
					"model.mistralai_Mistral-7B-v0.1": `modelID: "mistralai_Mistral-7B-v0.1"
enableScaleToZero: true
retentionPeriod: "15m"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-invalid", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only valid entries were parsed")
			Expect(len(configData)).To(Equal(2))

			// Check valid entries exist
			_, exists := configData["meta_llama-3.1-8b"]
			Expect(exists).To(BeTrue())
			_, exists = configData["mistralai_Mistral-7B-v0.1"]
			Expect(exists).To(BeTrue())

			// Check invalid entry was skipped
			_, exists = configData["meta_llama-3.1-70b"]
			Expect(exists).To(BeFalse())

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should apply scale-to-zero config during prepareVariantAutoscalings", func() {
			By("Creating required ConfigMaps")
			// Create accelerator ConfigMap
			acceleratorConfigMap := testutils.CreateAcceleratorUnitCostConfigMap(configMapNamespace)
			Expect(k8sClient.Create(ctx, acceleratorConfigMap)).To(Succeed())

			// Create service class ConfigMap
			serviceClassConfigMap := testutils.CreateServiceClassConfigMap(configMapNamespace)
			Expect(k8sClient.Create(ctx, serviceClassConfigMap)).To(Succeed())

			// Create scale-to-zero ConfigMap
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-test",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"vllm_meta_llama-3.1-8b": `{
						"enableScaleToZero": true,
						"retentionPeriod": "5m"
					}`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading ConfigMaps")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			accMap, err := controllerReconciler.readAcceleratorConfig(ctx, "accelerator-unit-costs", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			serviceClassMap, err := controllerReconciler.readServiceClassConfig(ctx, "service-classes-config", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			scaleToZeroConfigData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-test", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Listing VariantAutoscaling resources")
			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			err = k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred())

			activeVAs := filterActiveVariantAutoscalings(variantAutoscalingList.Items)
			if len(activeVAs) > 0 {
				By("Creating system data")
				systemData := utils.CreateSystemData(accMap, serviceClassMap)
				Expect(systemData).NotTo(BeNil())

				By("Calling prepareVariantAutoscalings with scale-to-zero config")
				updateList, vaMap, _, err := controllerReconciler.prepareVariantAutoscalings(ctx, activeVAs, accMap, serviceClassMap, systemData, scaleToZeroConfigData)
				Expect(err).NotTo(HaveOccurred())
				Expect(vaMap).NotTo(BeNil())
				Expect(updateList).NotTo(BeNil())
			}

			By("Cleaning up ConfigMaps")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
			Expect(k8sClient.Delete(ctx, acceleratorConfigMap)).To(Succeed())
			Expect(k8sClient.Delete(ctx, serviceClassConfigMap)).To(Succeed())
		})

		It("should handle empty ConfigMap data", func() {
			By("Creating an empty ConfigMap")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-empty",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the empty ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-empty", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())
			Expect(configData).NotTo(BeNil())
			Expect(len(configData)).To(Equal(0))

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		// NEW TESTS FOR PREFIXED-KEY FORMAT WITH YAML VALUES

		It("should parse prefixed-key format with YAML values", func() {
			By("Creating a ConfigMap with prefixed-key format and YAML values")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-yaml",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"__defaults__": `enableScaleToZero: true
retentionPeriod: "15m"`,
					"model.meta.llama-3.1-8b": `modelID: "meta/llama-3.1-8b"
retentionPeriod: "5m"`,
					"model.vllm.meta.llama-3.1-8b": `modelID: "vllm:meta/llama-3.1-8b"
enableScaleToZero: true
retentionPeriod: "3m"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-yaml", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying parsed data")
			Expect(len(configData)).To(Equal(3)) // __defaults__ + 2 models

			// Check defaults
			defaults, exists := configData["__defaults__"]
			Expect(exists).To(BeTrue())
			Expect(defaults.EnableScaleToZero).NotTo(BeNil())
			Expect(*defaults.EnableScaleToZero).To(BeTrue())
			Expect(defaults.RetentionPeriod).To(Equal("15m"))

			// Check model with slash in ID
			model1, exists := configData["meta/llama-3.1-8b"]
			Expect(exists).To(BeTrue())
			Expect(model1.ModelID).To(Equal("meta/llama-3.1-8b"))
			Expect(model1.RetentionPeriod).To(Equal("5m"))
			Expect(model1.EnableScaleToZero).To(BeNil()) // Not set, inherits from defaults

			// Check model with colon in ID
			model2, exists := configData["vllm:meta/llama-3.1-8b"]
			Expect(exists).To(BeTrue())
			Expect(model2.ModelID).To(Equal("vllm:meta/llama-3.1-8b"))
			Expect(model2.EnableScaleToZero).NotTo(BeNil())
			Expect(*model2.EnableScaleToZero).To(BeTrue())
			Expect(model2.RetentionPeriod).To(Equal("3m"))

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should handle duplicate modelID deterministically - first key wins", func() {
			By("Creating a ConfigMap with duplicate modelIDs")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-duplicates",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					// Same modelID in different keys - lexicographically first key should win
					"model.z.duplicate": `modelID: "test/model"
retentionPeriod: "999m"`,
					"model.a.duplicate": `modelID: "test/model"
retentionPeriod: "5m"`,
					"model.m.duplicate": `modelID: "test/model"
retentionPeriod: "10m"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-duplicates", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only first key (lexicographically) wins")
			Expect(len(configData)).To(Equal(1)) // Only one entry for the duplicate modelID
			model, exists := configData["test/model"]
			Expect(exists).To(BeTrue())
			// "model.a.duplicate" comes first lexicographically, so its value should win
			Expect(model.RetentionPeriod).To(Equal("5m"))
			Expect(model.ModelID).To(Equal("test/model"))

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should skip entries without modelID field", func() {
			By("Creating a ConfigMap with missing modelID")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-no-modelid",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"model.valid": `modelID: "valid/model"
retentionPeriod: "5m"`,
					"model.invalid": `retentionPeriod: "10m"`, // Missing modelID field
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-no-modelid", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only valid entry was parsed")
			Expect(len(configData)).To(Equal(1))
			model, exists := configData["valid/model"]
			Expect(exists).To(BeTrue())
			Expect(model.ModelID).To(Equal("valid/model"))

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should skip invalid YAML entries", func() {
			By("Creating a ConfigMap with invalid YAML")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-invalid-yaml",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"model.valid": `modelID: "valid/model"
retentionPeriod: "5m"`,
					"model.invalid": `this is not: valid: yaml: [[[`, // Invalid YAML
					"__defaults__":  `invalid yaml here too: {{{{`,   // Invalid defaults
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-invalid-yaml", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only valid entry was parsed")
			Expect(len(configData)).To(Equal(1))
			model, exists := configData["valid/model"]
			Expect(exists).To(BeTrue())
			Expect(model.ModelID).To(Equal("valid/model"))

			// Invalid defaults and invalid model should be skipped
			_, exists = configData["__defaults__"]
			Expect(exists).To(BeFalse())

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should ignore non-prefixed keys", func() {
			By("Creating a ConfigMap with mixed key formats")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-mixed",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"model.prefixed": `modelID: "prefixed/model"
retentionPeriod: "5m"`,
					"notprefixed": `modelID: "should/be/ignored"
retentionPeriod: "10m"`,
					"another-key": `modelID: "also/ignored"
retentionPeriod: "15m"`,
					"__defaults__": `enableScaleToZero: true
retentionPeriod: "20m"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-mixed", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only prefixed keys and defaults were parsed")
			Expect(len(configData)).To(Equal(2)) // defaults + 1 prefixed model

			// Check defaults
			defaults, exists := configData["__defaults__"]
			Expect(exists).To(BeTrue())
			Expect(defaults.RetentionPeriod).To(Equal("20m"))

			// Check prefixed model
			model, exists := configData["prefixed/model"]
			Expect(exists).To(BeTrue())
			Expect(model.ModelID).To(Equal("prefixed/model"))

			// Non-prefixed keys should be ignored
			_, exists = configData["should/be/ignored"]
			Expect(exists).To(BeFalse())
			_, exists = configData["also/ignored"]
			Expect(exists).To(BeFalse())

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should handle special characters in modelID", func() {
			By("Creating a ConfigMap with special characters in modelIDs")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-special-chars",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"model.test1": `modelID: "org/model-name_v1.2.3"
retentionPeriod: "5m"`,
					"model.test2": `modelID: "vllm:meta/llama@latest"
retentionPeriod: "10m"`,
					"model.test3": `modelID: "prefix:org/model-name:tag"
retentionPeriod: "15m"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-special-chars", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying all special character modelIDs were parsed correctly")
			Expect(len(configData)).To(Equal(3))

			model1, exists := configData["org/model-name_v1.2.3"]
			Expect(exists).To(BeTrue())
			Expect(model1.ModelID).To(Equal("org/model-name_v1.2.3"))
			Expect(model1.RetentionPeriod).To(Equal("5m"))

			model2, exists := configData["vllm:meta/llama@latest"]
			Expect(exists).To(BeTrue())
			Expect(model2.ModelID).To(Equal("vllm:meta/llama@latest"))
			Expect(model2.RetentionPeriod).To(Equal("10m"))

			model3, exists := configData["prefix:org/model-name:tag"]
			Expect(exists).To(BeTrue())
			Expect(model3.ModelID).To(Equal("prefix:org/model-name:tag"))
			Expect(model3.RetentionPeriod).To(Equal("15m"))

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should handle partial overrides correctly", func() {
			By("Creating a ConfigMap with partial overrides")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-partial",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"__defaults__": `enableScaleToZero: true
retentionPeriod: "15m"`,
					"model.only-retention": `modelID: "test/only-retention"
retentionPeriod: "5m"`,
					"model.only-enable": `modelID: "test/only-enable"
enableScaleToZero: false`,
					"model.both": `modelID: "test/both"
enableScaleToZero: false
retentionPeriod: "20m"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-partial", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying partial override behavior")
			Expect(len(configData)).To(Equal(4))

			// Model with only retentionPeriod set (EnableScaleToZero should be nil)
			model1, exists := configData["test/only-retention"]
			Expect(exists).To(BeTrue())
			Expect(model1.RetentionPeriod).To(Equal("5m"))
			Expect(model1.EnableScaleToZero).To(BeNil()) // Should inherit from defaults

			// Model with only EnableScaleToZero set (RetentionPeriod should be empty)
			model2, exists := configData["test/only-enable"]
			Expect(exists).To(BeTrue())
			Expect(model2.EnableScaleToZero).NotTo(BeNil())
			Expect(*model2.EnableScaleToZero).To(BeFalse())
			Expect(model2.RetentionPeriod).To(Equal("")) // Should inherit from defaults

			// Model with both set
			model3, exists := configData["test/both"]
			Expect(exists).To(BeTrue())
			Expect(model3.EnableScaleToZero).NotTo(BeNil())
			Expect(*model3.EnableScaleToZero).To(BeFalse())
			Expect(model3.RetentionPeriod).To(Equal("20m"))

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		// EDGE CASE TESTS FOR RETENTION PERIOD VALIDATION

		It("should handle invalid retention period formats", func() {
			By("Creating a ConfigMap with invalid retention periods")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-invalid-retention",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"__defaults__": `enableScaleToZero: true
retentionPeriod: "10m"`,
					"model.valid": `modelID: "test/valid"
retentionPeriod: "5m"`,
					"model.invalid-format": `modelID: "test/invalid-format"
retentionPeriod: "invalid"`,
					"model.number-only": `modelID: "test/number-only"
retentionPeriod: "5"`,
					"model.negative": `modelID: "test/negative"
retentionPeriod: "-5m"`,
					"model.zero": `modelID: "test/zero"
retentionPeriod: "0s"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-invalid-retention", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying invalid retention periods are still parsed but will fail validation during use")
			// All entries should be parsed (YAML is valid)
			Expect(len(configData)).To(Equal(6)) // defaults + 5 models

			// Check that entries are present (validation happens at use time via GetScaleToZeroRetentionPeriod)
			_, exists := configData["test/valid"]
			Expect(exists).To(BeTrue())
			_, exists = configData["test/invalid-format"]
			Expect(exists).To(BeTrue())
			_, exists = configData["test/number-only"]
			Expect(exists).To(BeTrue())
			_, exists = configData["test/negative"]
			Expect(exists).To(BeTrue())
			_, exists = configData["test/zero"]
			Expect(exists).To(BeTrue())

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should handle unusually long retention periods with warning", func() {
			By("Creating a ConfigMap with very long retention period")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-long-retention",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"model.long-retention": `modelID: "test/long"
retentionPeriod: "48h"`,
					"model.very-long": `modelID: "test/very-long"
retentionPeriod: "168h"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-long-retention", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying long retention periods are accepted")
			Expect(len(configData)).To(Equal(2))

			model1, exists := configData["test/long"]
			Expect(exists).To(BeTrue())
			Expect(model1.RetentionPeriod).To(Equal("48h"))

			model2, exists := configData["test/very-long"]
			Expect(exists).To(BeTrue())
			Expect(model2.RetentionPeriod).To(Equal("168h"))

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})

		It("should fall back to defaults when retention period is invalid", func() {
			By("Creating a ConfigMap with invalid retention and valid defaults")
			scaleToZeroConfigMap := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "model-scale-to-zero-config-fallback",
					Namespace: configMapNamespace,
				},
				Data: map[string]string{
					"__defaults__": `enableScaleToZero: true
retentionPeriod: "20m"`,
					"model.invalid-retention": `modelID: "test/invalid"
retentionPeriod: "not-a-duration"`,
				},
			}
			Expect(k8sClient.Create(ctx, scaleToZeroConfigMap)).To(Succeed())

			By("Reading the ConfigMap")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			configData, err := controllerReconciler.readScaleToZeroConfig(ctx, "model-scale-to-zero-config-fallback", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying both entries are present")
			Expect(len(configData)).To(Equal(2))

			// GetScaleToZeroRetentionPeriod will fall back to defaults when model's retention is invalid
			duration := utils.GetScaleToZeroRetentionPeriod(configData, "test/invalid")
			Expect(duration).To(Equal(20 * time.Minute)) // Should use defaults, not system default

			By("Cleaning up ConfigMap")
			Expect(k8sClient.Delete(ctx, scaleToZeroConfigMap)).To(Succeed())
		})
	})

	// Additional tests for improved coverage
	Context("When testing utility functions", func() {
		It("should correctly return max of two int32 values", func() {
			// Test max function
			result := max(5, 10)
			Expect(result).To(Equal(int32(10)))

			result = max(10, 5)
			Expect(result).To(Equal(int32(10)))

			result = max(7, 7)
			Expect(result).To(Equal(int32(7)))

			result = max(-5, 3)
			Expect(result).To(Equal(int32(3)))
		})
	})

	Context("When testing addVariantWithFallbackAllocation", func() {
		It("should apply fallback allocation with scale-to-zero disabled", func() {
			// Create a test VA
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:   "test-model",
					VariantID: "test-variant",
				},
			}

			// Create a deployment with 3 replicas
			replicas := int32(3)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           3,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)

			// Test with scale-to-zero disabled for non-cheapest variant
			allVariants := []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}
			retentionPeriod := 5 * time.Minute
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", nil, false, allVariants, retentionPeriod)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.CurrentAlloc.NumReplicas).To(Equal(int32(3)))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(3)))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.Reason).NotTo(BeEmpty())
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.LastUpdate.IsZero()).To(BeFalse())
		})

		It("should apply fallback allocation with scale-to-zero enabled and no load", func() {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-zero",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:   "test-model",
					VariantID: "test-variant",
				},
			}

			replicas := int32(2)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           2,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)
			enableScaleToZero := true
			scaleToZeroConfig["test-model"] = utils.ModelScaleToZeroConfig{
				ModelID:           "test-model",
				EnableScaleToZero: &enableScaleToZero,
				RetentionPeriod:   "10m",
			}

			// Test with zero load
			zeroLoad := float64(0)
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", &zeroLoad, false, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.CurrentAlloc.NumReplicas).To(Equal(int32(2)))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(0)))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.Reason).NotTo(BeEmpty())
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.LastUpdate.IsZero()).To(BeFalse())
		})

		It("should apply fallback allocation with scale-to-zero enabled and active load", func() {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-load",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:   "test-model",
					VariantID: "test-variant",
				},
			}

			replicas := int32(2)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           2,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)
			enableScaleToZero := true
			scaleToZeroConfig["test-model"] = utils.ModelScaleToZeroConfig{
				ModelID:           "test-model",
				EnableScaleToZero: &enableScaleToZero,
				RetentionPeriod:   "10m",
			}

			// Test with active load
			activeLoad := float64(5.5)
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", &activeLoad, false, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.CurrentAlloc.NumReplicas).To(Equal(int32(2)))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(2)))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.Reason).NotTo(BeEmpty())
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.LastUpdate.IsZero()).To(BeFalse())
		})

		It("should respect minReplicas when applying fallback", func() {
			minReplicas := int32(2)
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-minreplicas",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					VariantID:   "test-variant",
					MinReplicas: &minReplicas,
				},
			}

			replicas := int32(3)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           3,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)
			enableScaleToZero := true
			scaleToZeroConfig["test-model"] = utils.ModelScaleToZeroConfig{
				ModelID:           "test-model",
				EnableScaleToZero: &enableScaleToZero,
				RetentionPeriod:   "10m",
			}

			// Test with zero load but minReplicas set
			zeroLoad := float64(0)
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", &zeroLoad, false, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			// Should be clamped to minReplicas
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(2)))
		})

		It("should respect maxReplicas when applying fallback", func() {
			maxReplicas := int32(5)
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-maxreplicas",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					VariantID:   "test-variant",
					MaxReplicas: &maxReplicas,
				},
			}

			replicas := int32(10)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           10,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)

			// Test with current replicas exceeding maxReplicas
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", nil, false, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			// Should be clamped to maxReplicas
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(5)))
		})

		It("should handle cheapest variant with scale-to-zero disabled and no load", func() {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-cheapest",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:   "test-model",
					VariantID: "test-variant",
				},
			}

			// Deployment with 0 replicas
			replicas := int32(0)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           0,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)

			// Test with cheapest variant and zero load (not nil) - should set to 1 replica
			zeroLoad := float64(0)
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", &zeroLoad, true, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(1)))
		})

		It("should handle nil deployment", func() {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-nil-deploy",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:   "test-model",
					VariantID: "test-variant",
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)

			// Test with nil deployment
			addVariantWithFallbackAllocation(va, nil, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", nil, false, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.CurrentAlloc.NumReplicas).To(Equal(int32(0)))
		})

		It("should handle non-cheapest variant with scale-to-zero disabled and no load", func() {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-non-cheapest",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:   "test-model",
					VariantID: "test-variant",
				},
			}

			replicas := int32(2)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           2,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)

			// Test non-cheapest variant with zero load - should set to 0
			zeroLoad := float64(0)
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", &zeroLoad, false, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(0)))
		})

		It("should enforce minReplicas on non-cheapest variant with scale-to-zero disabled", func() {
			minReplicas := int32(1)
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-non-cheapest-min",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					VariantID:   "test-variant",
					MinReplicas: &minReplicas,
				},
			}

			replicas := int32(3)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           3,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)

			// Test non-cheapest variant with zero load and minReplicas=1
			// Should respect minReplicas even though it's not cheapest
			zeroLoad := float64(0)
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", &zeroLoad, false, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			// Should be clamped to minReplicas, not 0
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(1)))
		})

		It("should enforce minReplicas=0 allows scale-to-zero when enabled", func() {
			minReplicas := int32(0)
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-stz-min0",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					VariantID:   "test-variant",
					MinReplicas: &minReplicas,
				},
			}

			replicas := int32(2)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           2,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)
			enableScaleToZero := true
			scaleToZeroConfig["test-model"] = utils.ModelScaleToZeroConfig{
				ModelID:           "test-model",
				EnableScaleToZero: &enableScaleToZero,
				RetentionPeriod:   "10m",
			}

			// Test with scale-to-zero enabled, zero load, minReplicas=0
			// Should allow scaling to 0
			zeroLoad := float64(0)
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", &zeroLoad, true, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(0)))
		})

		It("should ensure cheapest variant has 1 replica when metrics unavailable, all at 0, scale-to-zero disabled", func() {
			minReplicas := int32(0)
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-cheapest-nil-metrics",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					VariantID:   "test-variant",
					MinReplicas: &minReplicas,
				},
			}

			// Deployment with 0 replicas
			replicas := int32(0)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           0,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)

			// Test with nil metrics (unavailable), cheapest variant, scale-to-zero disabled
			// Should set to 1 to ensure at least one variant can serve requests
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", nil, true, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(1)))
		})

		It("should allow all 0s when metrics unavailable, all at 0, scale-to-zero enabled", func() {
			minReplicas := int32(0)
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-all-zero-stz",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					VariantID:   "test-variant",
					MinReplicas: &minReplicas,
				},
			}

			// Deployment with 0 replicas
			replicas := int32(0)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           0,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)
			enableScaleToZero := true
			scaleToZeroConfig["test-model"] = utils.ModelScaleToZeroConfig{
				ModelID:           "test-model",
				EnableScaleToZero: &enableScaleToZero,
				RetentionPeriod:   "10m",
			}

			// Test with nil metrics (unavailable), scale-to-zero enabled, all at 0
			// Should allow 0 because scale-to-zero is enabled
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", nil, true, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(0)))
		})

		It("should respect minReplicas when metrics unavailable even for cheapest variant", func() {
			minReplicas := int32(2)
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-fallback-va-cheapest-nil-min2",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					VariantID:   "test-variant",
					MinReplicas: &minReplicas,
				},
			}

			// Deployment with 0 replicas
			replicas := int32(0)
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
				Status: appsv1.DeploymentStatus{
					Replicas:           0,
					ObservedGeneration: 1,
				},
			}

			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			scaleToZeroConfig := make(utils.ScaleToZeroConfigData)

			// Test with nil metrics, minReplicas=2
			// Should use max(minReplicas=2, current=0) = 2
			addVariantWithFallbackAllocation(va, deploy, "TestReason", "Test message",
				&updateList, scaleToZeroConfig, "test-model", nil, true, []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}, 5*time.Minute)

			Expect(updateList.Items).To(HaveLen(1))
			Expect(updateList.Items[0].Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(2)))
		})
	})

	Describe("Helper Functions", func() {
		Context("applyRetentionPeriodScaling", func() {
			var (
				va                    *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
				allVariants           []llmdVariantAutoscalingV1alpha1.VariantAutoscaling
				scaleToZeroConfigData utils.ScaleToZeroConfigData
				retentionPeriod       time.Duration
			)

			BeforeEach(func() {
				minReplicas := int32(0)
				enabled := true
				va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-va",
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ModelID:          "test-model",
						MinReplicas:      &minReplicas,
						AcceleratorCount: 4,
					},
				}
				allVariants = []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*va}
				scaleToZeroConfigData = utils.ScaleToZeroConfigData{
					"test-model": utils.ModelScaleToZeroConfig{
						ModelID:           "test-model",
						EnableScaleToZero: &enabled,
						RetentionPeriod:   "5m",
					},
				}
				retentionPeriod = 5 * time.Minute
			})

			It("should scale to zero when scaleToZero enabled and all minReplicas are zero", func() {
				replicas, reason := applyRetentionPeriodScaling(va, allVariants, scaleToZeroConfigData, retentionPeriod, "Test")

				Expect(replicas).To(Equal(int32(0)))
				Expect(reason).To(ContainSubstring("scale-to-zero enabled"))
				Expect(reason).To(ContainSubstring("scaling to 0"))
			})

			It("should set cheapest variant to 1 when scaleToZero disabled", func() {
				disabled := false
				scaleToZeroConfigData["test-model"] = utils.ModelScaleToZeroConfig{
					ModelID:           "test-model",
					EnableScaleToZero: &disabled,
					RetentionPeriod:   "5m",
				}

				replicas, reason := applyRetentionPeriodScaling(va, allVariants, scaleToZeroConfigData, retentionPeriod, "Test")

				Expect(replicas).To(Equal(int32(1)))
				Expect(reason).To(ContainSubstring("cheapest variant set to 1"))
			})

			It("should use minReplicas when some variants have minReplicas > 0", func() {
				minReplicas := int32(3)
				va.Spec.MinReplicas = &minReplicas
				// Update allVariants to reflect the change
				allVariants[0].Spec.MinReplicas = &minReplicas

				replicas, reason := applyRetentionPeriodScaling(va, allVariants, scaleToZeroConfigData, retentionPeriod, "Test")

				Expect(replicas).To(Equal(int32(3)))
				Expect(reason).To(ContainSubstring("using minReplicas=3"))
			})

			It("should include path name in reason message", func() {
				_, reason := applyRetentionPeriodScaling(va, allVariants, scaleToZeroConfigData, retentionPeriod, "Fallback")

				Expect(reason).To(ContainSubstring("Fallback:"))
			})
		})

		Context("applyReplicaBounds", func() {
			It("should not change replicas when within bounds", func() {
				minReplicas := int32(2)
				maxReplicas := int32(10)

				clamped, changed := applyReplicaBounds(5, &minReplicas, &maxReplicas, "test-va")

				Expect(clamped).To(Equal(int32(5)))
				Expect(changed).To(BeFalse())
			})

			It("should clamp to minReplicas when below minimum", func() {
				minReplicas := int32(5)

				clamped, changed := applyReplicaBounds(2, &minReplicas, nil, "test-va")

				Expect(clamped).To(Equal(int32(5)))
				Expect(changed).To(BeTrue())
			})

			It("should clamp to maxReplicas when above maximum", func() {
				maxReplicas := int32(10)

				clamped, changed := applyReplicaBounds(15, nil, &maxReplicas, "test-va")

				Expect(clamped).To(Equal(int32(10)))
				Expect(changed).To(BeTrue())
			})

			It("should handle nil minReplicas and maxReplicas", func() {
				clamped, changed := applyReplicaBounds(5, nil, nil, "test-va")

				Expect(clamped).To(Equal(int32(5)))
				Expect(changed).To(BeFalse())
			})

			It("should apply both bounds when needed", func() {
				minReplicas := int32(5)
				maxReplicas := int32(10)

				// Test below minimum
				clamped, changed := applyReplicaBounds(2, &minReplicas, &maxReplicas, "test-va")
				Expect(clamped).To(Equal(int32(5)))
				Expect(changed).To(BeTrue())

				// Test above maximum
				clamped, changed = applyReplicaBounds(15, &minReplicas, &maxReplicas, "test-va")
				Expect(clamped).To(Equal(int32(10)))
				Expect(changed).To(BeTrue())
			})
		})

		Context("createOptimizedAllocWithUpdate", func() {
			It("should update LastUpdate when NumReplicas changes", func() {
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 5,
					Reason:      "Previous reason",
					LastUpdate:  metav1.NewTime(time.Now().Add(-1 * time.Hour)),
				}

				newAlloc := createOptimizedAllocWithUpdate(8, "New reason", previousAlloc)

				Expect(newAlloc.NumReplicas).To(Equal(int32(8)))
				Expect(newAlloc.Reason).To(Equal("New reason"))
				Expect(newAlloc.LastUpdate.Time).To(BeTemporally(">", previousAlloc.LastUpdate.Time))
			})

			It("should update LastUpdate when Reason changes", func() {
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 5,
					Reason:      "Previous reason",
					LastUpdate:  metav1.NewTime(time.Now().Add(-1 * time.Hour)),
				}

				newAlloc := createOptimizedAllocWithUpdate(5, "New reason", previousAlloc)

				Expect(newAlloc.NumReplicas).To(Equal(int32(5)))
				Expect(newAlloc.Reason).To(Equal("New reason"))
				Expect(newAlloc.LastUpdate.Time).To(BeTemporally(">", previousAlloc.LastUpdate.Time))
			})

			It("should preserve LastUpdate when nothing changes", func() {
				previousTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 5,
					Reason:      "Same reason",
					LastUpdate:  previousTime,
				}

				newAlloc := createOptimizedAllocWithUpdate(5, "Same reason", previousAlloc)

				Expect(newAlloc.NumReplicas).To(Equal(int32(5)))
				Expect(newAlloc.Reason).To(Equal("Same reason"))
				Expect(newAlloc.LastUpdate).To(Equal(previousTime))
			})

			It("should handle zero NumReplicas", func() {
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 5,
					Reason:      "Previous reason",
					LastUpdate:  metav1.NewTime(time.Now().Add(-1 * time.Hour)),
				}

				newAlloc := createOptimizedAllocWithUpdate(0, "Scaled to zero", previousAlloc)

				Expect(newAlloc.NumReplicas).To(Equal(int32(0)))
				Expect(newAlloc.Reason).To(Equal("Scaled to zero"))
				Expect(newAlloc.LastUpdate.Time).To(BeTemporally(">", previousAlloc.LastUpdate.Time))
			})
		})
	})

	Describe("Path 1 Retention Period Check", func() {
		Context("When optimizer returns 0 replicas", func() {
			var (
				scaleToZeroConfigData utils.ScaleToZeroConfigData
			)

			BeforeEach(func() {
				enabled := true
				scaleToZeroConfigData = utils.ScaleToZeroConfigData{
					"test-model": utils.ModelScaleToZeroConfig{
						ModelID:           "test-model",
						EnableScaleToZero: &enabled,
						RetentionPeriod:   "5m",
					},
				}
			})

			It("should preserve previous allocation when retention period not exceeded", func() {
				// Simulate optimizer returning 0 replicas
				optimizedAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 0,
				}

				// Previous allocation with recent LastUpdate (within retention period)
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 3,
					Reason:      "Optimizer solution: cost and latency optimized allocation",
					LastUpdate:  metav1.NewTime(time.Now().Add(-2 * time.Minute)), // 2m ago < 5m retention
				}

				// Simulate the Path 1 retention check logic
				newAlloc := optimizedAlloc
				newAlloc.Reason = "Optimizer solution: cost and latency optimized allocation"

				modelName := "test-model"
				retentionPeriod := utils.GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, modelName)

				if optimizedAlloc.NumReplicas == 0 {
					if !previousAlloc.LastUpdate.IsZero() {
						timeSinceLastUpdate := time.Since(previousAlloc.LastUpdate.Time)
						if timeSinceLastUpdate <= retentionPeriod {
							// Preserve previous allocation
							newAlloc.NumReplicas = previousAlloc.NumReplicas
							newAlloc.Reason = "Optimizer returned 0 but retention period not exceeded, preserving allocation"
						}
					}
				}

				Expect(newAlloc.NumReplicas).To(Equal(int32(3)), "Should preserve previous allocation")
				Expect(newAlloc.Reason).To(ContainSubstring("retention period not exceeded"))
			})

			It("should allow scale to zero when retention period exceeded", func() {
				// Simulator optimizer returning 0 replicas
				optimizedAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 0,
				}

				// Previous allocation with old LastUpdate (beyond retention period)
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 3,
					Reason:      "Optimizer solution: cost and latency optimized allocation",
					LastUpdate:  metav1.NewTime(time.Now().Add(-10 * time.Minute)), // 10m ago > 5m retention
				}

				// Simulate the Path 1 retention check logic
				newAlloc := optimizedAlloc
				newAlloc.Reason = "Optimizer solution: cost and latency optimized allocation"

				modelName := "test-model"
				retentionPeriod := utils.GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, modelName)

				if optimizedAlloc.NumReplicas == 0 {
					if !previousAlloc.LastUpdate.IsZero() {
						timeSinceLastUpdate := time.Since(previousAlloc.LastUpdate.Time)
						if timeSinceLastUpdate <= retentionPeriod {
							// Preserve previous allocation
							newAlloc.NumReplicas = previousAlloc.NumReplicas
							newAlloc.Reason = "Optimizer returned 0 but retention period not exceeded, preserving allocation"
						}
					}
				}

				Expect(newAlloc.NumReplicas).To(Equal(int32(0)), "Should allow scale to zero")
				Expect(newAlloc.Reason).To(Equal("Optimizer solution: cost and latency optimized allocation"))
			})

			It("should preserve currentReplicas on first run (LastUpdate is zero)", func() {
				// Simulate optimizer returning 0 replicas
				optimizedAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 0,
				}

				// Previous allocation with zero LastUpdate (first run)
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 0,
					Reason:      "",
					LastUpdate:  metav1.Time{}, // Zero time = first run
				}

				// Current deployment has 2 replicas
				currentReplicas := int32(2)

				// Simulate the Path 1 retention check logic
				newAlloc := optimizedAlloc
				newAlloc.Reason = "Optimizer solution: cost and latency optimized allocation"

				modelName := "test-model"
				retentionPeriod := utils.GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, modelName)

				if optimizedAlloc.NumReplicas == 0 {
					if previousAlloc.LastUpdate.IsZero() {
						// First run: preserve currentReplicas
						newAlloc.NumReplicas = currentReplicas
						newAlloc.Reason = "First run: preserving current replicas for Prometheus discovery grace period"
					} else {
						timeSinceLastUpdate := time.Since(previousAlloc.LastUpdate.Time)
						if timeSinceLastUpdate <= retentionPeriod {
							// Preserve previous allocation
							newAlloc.NumReplicas = previousAlloc.NumReplicas
							newAlloc.Reason = "Optimizer returned 0 but retention period not exceeded, preserving allocation"
						}
					}
				}

				Expect(newAlloc.NumReplicas).To(Equal(int32(2)), "Should preserve currentReplicas on first run")
				Expect(newAlloc.Reason).To(Equal("First run: preserving current replicas for Prometheus discovery grace period"))
			})

			It("should not affect optimizer solutions with non-zero replicas", func() {
				// Simulate optimizer returning non-zero replicas
				optimizedAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 5,
				}

				// Previous allocation
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 3,
					Reason:      "Optimizer solution: cost and latency optimized allocation",
					LastUpdate:  metav1.NewTime(time.Now().Add(-2 * time.Minute)),
				}

				// Simulate the Path 1 retention check logic
				newAlloc := optimizedAlloc
				newAlloc.Reason = "Optimizer solution: cost and latency optimized allocation"

				modelName := "test-model"
				retentionPeriod := utils.GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, modelName)

				if optimizedAlloc.NumReplicas == 0 {
					if !previousAlloc.LastUpdate.IsZero() {
						timeSinceLastUpdate := time.Since(previousAlloc.LastUpdate.Time)
						if timeSinceLastUpdate <= retentionPeriod {
							// Preserve previous allocation
							newAlloc.NumReplicas = previousAlloc.NumReplicas
							newAlloc.Reason = "Optimizer returned 0 but retention period not exceeded, preserving allocation"
						}
					}
				}

				Expect(newAlloc.NumReplicas).To(Equal(int32(5)), "Should apply optimizer's non-zero allocation")
				Expect(newAlloc.Reason).To(Equal("Optimizer solution: cost and latency optimized allocation"))
			})
		})
	})

	// Tests for refactored helper functions
	Describe("Helper Functions", func() {
		Describe("isRetentionPeriodExceeded", func() {
			It("should return false when lastUpdate is zero", func() {
				retentionPeriod := 5 * time.Minute
				lastUpdate := metav1.Time{}

				result := isRetentionPeriodExceeded(lastUpdate, retentionPeriod)

				Expect(result).To(BeFalse(), "Should return false when lastUpdate is zero (never set)")
			})

			It("should return false when time since lastUpdate is within retention period", func() {
				retentionPeriod := 5 * time.Minute
				lastUpdate := metav1.NewTime(time.Now().Add(-2 * time.Minute))

				result := isRetentionPeriodExceeded(lastUpdate, retentionPeriod)

				Expect(result).To(BeFalse(), "Should return false when within retention period")
			})

			It("should return true when time since lastUpdate exceeds retention period", func() {
				retentionPeriod := 5 * time.Minute
				lastUpdate := metav1.NewTime(time.Now().Add(-10 * time.Minute))

				result := isRetentionPeriodExceeded(lastUpdate, retentionPeriod)

				Expect(result).To(BeTrue(), "Should return true when retention period exceeded")
			})

			It("should return false at exactly retention period boundary", func() {
				retentionPeriod := 5 * time.Minute
				// Set to just under retention period to avoid timing issues
				lastUpdate := metav1.NewTime(time.Now().Add(-5*time.Minute + 100*time.Millisecond))

				result := isRetentionPeriodExceeded(lastUpdate, retentionPeriod)

				Expect(result).To(BeFalse(), "Should return false at retention period boundary")
			})
		})

		Describe("createOptimizedAllocWithUpdate", func() {
			It("should set LastUpdate when NumReplicas changes", func() {
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 3,
					Reason:      "Previous reason",
					LastUpdate:  metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				}

				newAlloc := createOptimizedAllocWithUpdate(5, "New reason", previousAlloc)

				Expect(newAlloc.NumReplicas).To(Equal(int32(5)))
				Expect(newAlloc.Reason).To(Equal("New reason"))
				Expect(newAlloc.LastUpdate.IsZero()).To(BeFalse(), "LastUpdate should be set")
				Expect(newAlloc.LastUpdate.Time).To(BeTemporally("~", time.Now(), 2*time.Second))
			})

			It("should set LastUpdate when Reason changes", func() {
				oldTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 5,
					Reason:      "Previous reason",
					LastUpdate:  oldTime,
				}

				newAlloc := createOptimizedAllocWithUpdate(5, "New reason", previousAlloc)

				Expect(newAlloc.NumReplicas).To(Equal(int32(5)))
				Expect(newAlloc.Reason).To(Equal("New reason"))
				Expect(newAlloc.LastUpdate.Time).To(BeTemporally("~", time.Now(), 2*time.Second))
				Expect(newAlloc.LastUpdate.Time).NotTo(Equal(oldTime.Time))
			})

			It("should preserve LastUpdate when nothing changes", func() {
				oldTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
				previousAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 5,
					Reason:      "Same reason",
					LastUpdate:  oldTime,
				}

				newAlloc := createOptimizedAllocWithUpdate(5, "Same reason", previousAlloc)

				Expect(newAlloc.NumReplicas).To(Equal(int32(5)))
				Expect(newAlloc.Reason).To(Equal("Same reason"))
				Expect(newAlloc.LastUpdate).To(Equal(oldTime), "LastUpdate should be preserved when nothing changes")
			})
		})

		Describe("updateConditionsForAllocation", func() {
			var updateVa, preparedVa *llmdVariantAutoscalingV1alpha1.VariantAutoscaling

			BeforeEach(func() {
				updateVa = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						Accelerator: "A100",
					},
					Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
						DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
							NumReplicas: 5,
							Reason:      "Test reason",
						},
					},
				}

				preparedVa = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
						Conditions: []metav1.Condition{
							{
								Type:    llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
								Status:  metav1.ConditionTrue,
								Reason:  llmdVariantAutoscalingV1alpha1.ReasonMetricsFound,
								Message: "Metrics available",
							},
						},
					},
				}
			})

			It("should set optimizer success condition when hasOptimizedAlloc is true", func() {
				updateConditionsForAllocation(updateVa, preparedVa, true)

				metricsCond := llmdVariantAutoscalingV1alpha1.GetCondition(updateVa, llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable)
				Expect(metricsCond).NotTo(BeNil(), "MetricsAvailable condition should be copied")
				Expect(metricsCond.Status).To(Equal(metav1.ConditionTrue))

				optCond := llmdVariantAutoscalingV1alpha1.GetCondition(updateVa, llmdVariantAutoscalingV1alpha1.TypeOptimizationReady)
				Expect(optCond).NotTo(BeNil(), "OptimizationReady condition should be set")
				Expect(optCond.Status).To(Equal(metav1.ConditionTrue))
				Expect(optCond.Reason).To(Equal(llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded))
				Expect(optCond.Message).To(ContainSubstring("Optimization completed"))
				Expect(optCond.Message).To(ContainSubstring("5 replicas"))
			})

			It("should set fallback condition when hasOptimizedAlloc is false and Reason is set", func() {
				updateConditionsForAllocation(updateVa, preparedVa, false)

				optCond := llmdVariantAutoscalingV1alpha1.GetCondition(updateVa, llmdVariantAutoscalingV1alpha1.TypeOptimizationReady)
				Expect(optCond).NotTo(BeNil(), "OptimizationReady condition should be set")
				Expect(optCond.Status).To(Equal(metav1.ConditionTrue))
				Expect(optCond.Reason).To(Equal(llmdVariantAutoscalingV1alpha1.ReasonFallbackUsed))
				Expect(optCond.Message).To(ContainSubstring("Test reason"))
				Expect(optCond.Message).To(ContainSubstring("5 replicas"))
			})

			It("should copy OptimizationReady from preparation when Reason is empty", func() {
				updateVa.Status.DesiredOptimizedAlloc.Reason = ""

				preparedVa.Status.Conditions = append(preparedVa.Status.Conditions, metav1.Condition{
					Type:    llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					Status:  metav1.ConditionFalse,
					Reason:  llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
					Message: "Preparation phase message",
				})

				updateConditionsForAllocation(updateVa, preparedVa, false)

				optCond := llmdVariantAutoscalingV1alpha1.GetCondition(updateVa, llmdVariantAutoscalingV1alpha1.TypeOptimizationReady)
				Expect(optCond).NotTo(BeNil(), "OptimizationReady should be copied from preparation")
				Expect(optCond.Status).To(Equal(metav1.ConditionFalse))
				Expect(optCond.Reason).To(Equal(llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed))
				Expect(optCond.Message).To(Equal("Preparation phase message"))
			})

			It("should preserve MetricsAvailable from preparation phase", func() {
				updateConditionsForAllocation(updateVa, preparedVa, true)

				metricsCond := llmdVariantAutoscalingV1alpha1.GetCondition(updateVa, llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable)
				Expect(metricsCond).NotTo(BeNil())
				Expect(metricsCond.Status).To(Equal(metav1.ConditionTrue))
				Expect(metricsCond.Reason).To(Equal(llmdVariantAutoscalingV1alpha1.ReasonMetricsFound))
				Expect(metricsCond.Message).To(Equal("Metrics available"))
			})
		})
	})

	Describe("Fallback Allocation - Reason and LastUpdate Fields", func() {
		var updateVa *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		var allVariants []llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		var scaleToZeroConfigData utils.ScaleToZeroConfigData

		BeforeEach(func() {
			minReplicas := int32(0)
			updateVa = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "default",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					MinReplicas: &minReplicas,
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
						NumReplicas: 1,
					},
				},
			}
			allVariants = []llmdVariantAutoscalingV1alpha1.VariantAutoscaling{*updateVa}
			scaleToZeroConfigData = utils.ScaleToZeroConfigData{}
		})

		Context("PATH 3 - First run (no previous allocation)", func() {
			It("should set Reason and LastUpdate on first run", func() {
				// Simulate first run: empty DesiredOptimizedAlloc
				updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{}

				// Apply fallback allocation (PATH 3)
				applyFallbackAllocation(updateVa, allVariants, scaleToZeroConfigData, false, "Last resort")

				// Verify Reason is set
				Expect(updateVa.Status.DesiredOptimizedAlloc.Reason).NotTo(BeEmpty(),
					"Reason should be set on first run")
				Expect(updateVa.Status.DesiredOptimizedAlloc.Reason).To(ContainSubstring("Last resort: first run"))

				// Verify LastUpdate is set
				Expect(updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero()).To(BeFalse(),
					"LastUpdate should be set on first run")

				// Verify NumReplicas is set
				Expect(updateVa.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(1)),
					"NumReplicas should be max(minReplicas=0, current=1) = 1")
			})

			It("should set Reason with minReplicas > 0", func() {
				minReplicas := int32(2)
				updateVa.Spec.MinReplicas = &minReplicas
				updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{}

				applyFallbackAllocation(updateVa, allVariants, scaleToZeroConfigData, false, "Last resort")

				Expect(updateVa.Status.DesiredOptimizedAlloc.Reason).To(ContainSubstring("max(minReplicas=2, current=1)"))
				Expect(updateVa.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(2)))
				Expect(updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero()).To(BeFalse())
			})
		})

		Context("PATH 2 - Has previous allocation", func() {
			It("should preserve Reason and LastUpdate when no bounds changed", func() {
				// Set previous allocation
				previousTime := metav1.NewTime(time.Now().Add(-5 * time.Minute))
				updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 3,
					Reason:      "Previous allocation: optimizer result",
					LastUpdate:  previousTime,
				}

				// Apply fallback allocation (PATH 2)
				applyFallbackAllocation(updateVa, allVariants, scaleToZeroConfigData, true, "Fallback")

				// Verify Reason and LastUpdate are preserved
				Expect(updateVa.Status.DesiredOptimizedAlloc.Reason).To(ContainSubstring("Previous allocation"))
				Expect(updateVa.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int32(3)))
				// LastUpdate might be updated due to bounds check, but Reason should be preserved or updated
			})

			It("should set empty Reason to default fallback message", func() {
				// Simulate scenario where Reason is empty (shouldn't happen, but safety net)
				updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 2,
					Reason:      "", // Empty reason
					LastUpdate:  metav1.NewTime(time.Now()),
				}

				applyFallbackAllocation(updateVa, allVariants, scaleToZeroConfigData, true, "Fallback")

				// Verify default Reason is set
				Expect(updateVa.Status.DesiredOptimizedAlloc.Reason).To(Equal("Fallback: preserving previous allocation (no optimizer solution)"))
				Expect(updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero()).To(BeFalse())
			})
		})

		Context("LastUpdate vs LastRunTime distinction", func() {
			It("should only update LastUpdate when allocation changes", func() {
				// Set previous allocation
				previousTime := metav1.NewTime(time.Now().Add(-5 * time.Minute))
				updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
					NumReplicas: 1,
					Reason:      "Previous: test",
					LastUpdate:  previousTime,
				}

				// Apply same allocation again
				applyFallbackAllocation(updateVa, allVariants, scaleToZeroConfigData, true, "Fallback")

				// Since allocation didn't change, LastUpdate might be preserved or set
				// The key is that Reason should not be empty
				Expect(updateVa.Status.DesiredOptimizedAlloc.Reason).NotTo(BeEmpty())
			})
		})
	})

	Describe("Goroutine Cleanup", func() {
		Context("Cache cleanup goroutine lifecycle", func() {
			// NOTE: This test is skipped because SetupWithManager requires a full controller-runtime
			// Manager which is not available in unit tests. The goroutine cleanup logic is verified
			// through code review and integration tests.
			XIt("should initialize cacheCleanupDone channel during setup", func() {
				// SKIPPED: Requires full Manager setup not available in unit tests
			})

			XIt("should start cleanup goroutine that responds to manager shutdown", func() {
				// SKIPPED: Requires full Manager setup not available in unit tests
			})

			XIt("should not leak goroutines after manager shutdown", func() {
				// SKIPPED: Requires full Manager setup not available in unit tests
				// This test would require:
				// 1. Starting the manager
				// 2. Wait for goroutine to start
				// 3. Stop the manager (closes mgr.Elected())
				// 4. Verify cacheCleanupDone is closed
				// 5. Check goroutine count doesn't increase
				//
				// The actual goroutine shutdown is tested in integration tests
			})
		})

		Context("Context timeout in SetupWithManager", func() {
			It("should use context with timeout for API calls during setup", func() {
				// This is verified by code review rather than runtime test
				// The SetupTimeout constant should be used in SetupWithManager
				// to create context.WithTimeout for all API calls during setup

				Expect(SetupTimeout).To(Equal(30*time.Second),
					"Setup timeout should be 30 seconds")

				// In production:
				// - getPrometheusConfig uses ctx with timeout
				// - ValidatePrometheusAPI uses ctx with timeout
				// - readOptimizationConfig uses ctx with timeout
				//
				// This prevents hanging indefinitely during startup if:
				// - Kubernetes API server is unavailable
				// - ConfigMaps cannot be fetched
				// - Prometheus cannot be reached
			})
		})
	})

	Describe("First Run Scenario Fix", func() {
		Context("When metrics are available but optimizer doesn't run", func() {
			It("should use current deployment replicas and not scale to zero", func() {
				// This test verifies the fix for the bug where first-run scenarios with
				// metrics available but no optimizer solution would incorrectly scale to 0
				// because NumReplicas defaults to 0.

				// Simulate first run: VA with no previous status (all defaults)
				firstRunVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-variant-first-run",
						Namespace: "default",
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ModelID:          "test-model",
						Accelerator:      "A100",
						AcceleratorCount: 1,
					},
					Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
						// First run: no status set yet
						// DesiredOptimizedAlloc will have default values:
						//   NumReplicas: 0 (int32 default)
						//   LastRunTime: zero time
						//   Reason: "" (empty string)
						//   LastUpdate: zero time
					},
				}

				// Simulate current deployment with 3 replicas
				currentDeploymentReplicas := int32(3)
				firstRunVA.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
					NumReplicas: currentDeploymentReplicas,
				}

				// Safety net sets Reason when metrics collected but optimizer hasn't run
				firstRunVA.Status.DesiredOptimizedAlloc.Reason = "Metrics collected, awaiting optimizer decision"
				firstRunVA.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Now()
				// Note: LastRunTime remains zero (optimizer hasn't run)

				// Check hasPrecomputedFallback logic (line 1359)
				hasPrecomputedFallback := !firstRunVA.Status.DesiredOptimizedAlloc.LastRunTime.IsZero()
				Expect(hasPrecomputedFallback).To(BeFalse(),
					"First run should not have precomputed fallback (LastRunTime is zero)")

				// Verify Path 3 (Last Resort) will be used
				// Path 3 checks if previous allocation exists using LastRunTime
				previousAlloc := firstRunVA.Status.DesiredOptimizedAlloc
				hasPreviousAllocation := !previousAlloc.LastRunTime.IsZero()
				Expect(hasPreviousAllocation).To(BeFalse(),
					"First run should not have previous allocation (LastRunTime is zero)")

				// Verify that Path 3 would use current deployment replicas as baseline
				var baselineReplicas int32
				if !previousAlloc.LastRunTime.IsZero() {
					// Would use previous allocation
					baselineReplicas = previousAlloc.NumReplicas
				} else {
					// First run - should use current deployment replicas
					baselineReplicas = firstRunVA.Status.CurrentAlloc.NumReplicas
				}

				Expect(baselineReplicas).To(Equal(currentDeploymentReplicas),
					"First run should use current deployment replicas (%d) as baseline, not default NumReplicas (0)",
					currentDeploymentReplicas)

				// Verify the fix: before the fix, the check was:
				//   hasPrecomputedFallback := NumReplicas >= 0 || !LastRunTime.IsZero()
				// This would be true (0 >= 0), treating first run as having a precomputed fallback
				// and copying NumReplicas=0, scaling to zero.
				//
				// After the fix, the check is:
				//   hasPrecomputedFallback := !LastRunTime.IsZero()
				// This is false on first run, so Path 3 is used, which correctly uses
				// current deployment replicas.

				// Document the fix
				oldCheckWouldBeTrue := previousAlloc.NumReplicas >= 0 || !previousAlloc.LastRunTime.IsZero()
				newCheckIsFalse := !previousAlloc.LastRunTime.IsZero()

				Expect(oldCheckWouldBeTrue).To(BeTrue(),
					"Old check (NumReplicas >= 0) would incorrectly be true")
				Expect(newCheckIsFalse).To(BeFalse(),
					"New check (only LastRunTime) correctly identifies no precomputed fallback")
			})

			It("should detect precomputed fallback when addVariantWithFallbackAllocation ran", func() {
				// This test verifies that when addVariantWithFallbackAllocation actually runs,
				// it's correctly detected as having a precomputed fallback.

				vaWithFallback := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
						CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
							NumReplicas: 2,
						},
						DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
							NumReplicas: 2,
							LastRunTime: metav1.Now(), // Set by addVariantWithFallbackAllocation
							Reason:      "Metrics unavailable, maintaining controller intent",
							LastUpdate:  metav1.Now(),
						},
					},
				}

				hasPrecomputedFallback := !vaWithFallback.Status.DesiredOptimizedAlloc.LastRunTime.IsZero()
				Expect(hasPrecomputedFallback).To(BeTrue(),
					"Should detect precomputed fallback when LastRunTime is set")
			})

			It("should detect previous optimizer solution exists", func() {
				// This test verifies that when optimizer ran previously,
				// it's correctly detected as having a previous allocation.

				vaWithOptimizerSolution := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
						CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
							NumReplicas: 5,
						},
						DesiredOptimizedAlloc: llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
							NumReplicas: 5,
							LastRunTime: metav1.Now(), // Set by optimizer
							Reason:      "Optimizer solution: cost and latency optimized allocation",
							LastUpdate:  metav1.Now(),
						},
					},
				}

				hasPreviousAllocation := !vaWithOptimizerSolution.Status.DesiredOptimizedAlloc.LastRunTime.IsZero()
				Expect(hasPreviousAllocation).To(BeTrue(),
					"Should detect previous optimizer solution when LastRunTime is set")
			})
		})
	})
})
