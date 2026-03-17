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

package saturation

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	interfaces "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	utils "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

var _ = Describe("Saturation Engine", func() {

	// Use config.SystemNamespace() instead of local function

	// CreateServiceClassConfigMap creates a service class ConfigMap for testing
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

	Context("When handling multiple VariantAutoscalings", func() {
		const totalVAs = 3
		const configMapName = "wva-variantautoscaling-config"
		var configMapNamespace = config.SystemNamespace()

		BeforeEach(func() {
			logging.NewTestLogger()

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: configMapNamespace,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmaps")
			// Use custom configmap creation function
			var modelNames []string
			for i := range totalVAs {
				modelNames = append(modelNames, fmt.Sprintf("model-%d-model-%d", i, i))
			}
			configMap := CreateServiceClassConfigMap(ns.Name, modelNames...)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			By("Creating VariantAutoscaling resources and Deployments")
			for i := range totalVAs {
				modelID := fmt.Sprintf("model-%d-model-%d", i, i)
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
										Image: "registry.k8s.io/pause:3.9",
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
							utils.AcceleratorNameLabel: "A100",
						},
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							Kind: "Deployment",
							Name: name,
						},
						ModelID:     modelID,
						MaxReplicas: 2,
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
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
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

		It("should set OptimizationReady condition when optimization succeeds", func() {
			By("Using a working mock Prometheus API with sample data")
			mockPromAPI := &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					// Add default responses for common queries
				},
				QueryErrors: map[string]error{},
			}

			// Initialize MetricsCollector with mock Prometheus API
			sourceRegistry := source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck
			// Create minimal test config with saturation config
			testConfig := config.NewTestConfig()
			testConfig.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {},
			})
			engine := NewEngine(k8sClient, k8sClient.Scheme(), nil, sourceRegistry, testConfig)

			By("Performing optimization loop")
			err := engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("Checking that conditions are set correctly")
			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			err = k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred())

			for _, va := range variantAutoscalingList.Items {
				if va.DeletionTimestamp.IsZero() {
					metricsCondition := llmdVariantAutoscalingV1alpha1.GetCondition(&va, llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable)
					// Note: optimization runs even if metrics are not available (skips parts), but condition checks depend on flow
					// In mock, activeVAs will be processed.
					if metricsCondition != nil && metricsCondition.Status == metav1.ConditionTrue {
						optimizationCondition := llmdVariantAutoscalingV1alpha1.GetCondition(&va, llmdVariantAutoscalingV1alpha1.TypeOptimizationReady)
						Expect(optimizationCondition).NotTo(BeNil(),
							fmt.Sprintf("OptimizationReady condition should be set for %s", va.Name))
					}
				}
			}
		})
	})

	Context("convertSaturationTargetsToDecisions", func() {
		BeforeEach(func() {
			logging.NewTestLogger()
		})

		It("should include ActionNoChange decisions in the result", func() {
			By("Creating test data where target equals current replicas")
			saturationTargets := map[string]int{
				"variant-a": 3,
				"variant-b": 5,
				"variant-c": 2,
			}

			saturationAnalysis := &interfaces.ModelSaturationAnalysis{
				ModelID:   "test-model",
				Namespace: "test-ns",
				VariantAnalyses: []interfaces.VariantSaturationAnalysis{
					{VariantName: "variant-a", AcceleratorName: "A100", Cost: 10.0},
					{VariantName: "variant-b", AcceleratorName: "A100", Cost: 10.0},
					{VariantName: "variant-c", AcceleratorName: "A100", Cost: 10.0},
				},
			}

			variantStates := []interfaces.VariantReplicaState{
				{VariantName: "variant-a", CurrentReplicas: 3, DesiredReplicas: 3},
				{VariantName: "variant-b", CurrentReplicas: 3, DesiredReplicas: 3},
				{VariantName: "variant-c", CurrentReplicas: 2, DesiredReplicas: 2},
			}

			By("Converting saturation targets to decisions")
			sourceRegistry := source.NewSourceRegistry()
			sourceRegistry.Register("prometheus", source.NewNoOpSource()) // nolint:errcheck
			// Create minimal test config
			testConfig := config.NewTestConfig()
			engine := NewEngine(k8sClient, k8sClient.Scheme(), nil, sourceRegistry, testConfig)
			decisions := engine.convertSaturationTargetsToDecisions(context.Background(), saturationTargets, saturationAnalysis, variantStates)

			By("Verifying all variants are included in decisions")
			Expect(len(decisions)).To(Equal(3), "All 3 variants should have decisions including ActionNoChange")

			By("Verifying ActionNoChange decisions are present")
			decisionMap := make(map[string]interfaces.VariantDecision)
			for _, d := range decisions {
				decisionMap[d.VariantName] = d
			}

			Expect(decisionMap).To(HaveKey("variant-a"))
			Expect(decisionMap["variant-a"].Action).To(Equal(interfaces.ActionNoChange))
			Expect(decisionMap["variant-b"].Action).To(Equal(interfaces.ActionScaleUp))
			Expect(decisionMap["variant-c"].Action).To(Equal(interfaces.ActionNoChange))
		})
	})

	Context("Source Infrastructure Optimization Tests", func() {
		const totalVAs = 3
		const configMapName = "wva-variantautoscaling-config"
		var configMapNamespace = config.SystemNamespace()
		var sourceRegistry *source.SourceRegistry
		var mockPromAPI *testutils.MockPromAPI

		BeforeEach(func() {
			logging.NewTestLogger()

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: configMapNamespace,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("Using a working mock Prometheus API with no data")
			mockPromAPI = &testutils.MockPromAPI{
				QueryResults: map[string]model.Value{
					// Add default responses for common queries
				},
				QueryErrors: map[string]error{},
			}

			By("creating the source registry with mock Prometheus API")
			sourceRegistry = source.NewSourceRegistry()
			promSource := prometheus.NewPrometheusSource(ctx, mockPromAPI, prometheus.DefaultPrometheusSourceConfig())
			sourceRegistry.Register("prometheus", promSource) // nolint:errcheck

			By("creating the required configmaps")
			// Use custom configmap creation function
			var modelNames []string
			for i := range totalVAs {
				modelNames = append(modelNames, fmt.Sprintf("v2-model-%d-model-%d", i, i))
			}
			configMap := CreateServiceClassConfigMap(ns.Name, modelNames...)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			By("Creating VariantAutoscaling resources and Deployments for source infrastructure tests")
			for i := range totalVAs {
				modelID := fmt.Sprintf("v2-model-%d-model-%d", i, i)
				name := fmt.Sprintf("v2-test-resource-%d", i)

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
										Image: "registry.k8s.io/pause:3.9",
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
							utils.AcceleratorNameLabel: "A100",
						},
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							Kind: "Deployment",
							Name: name,
						},
						ModelID:     modelID,
						MaxReplicas: 2,
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
					Namespace: configMapNamespace,
				},
			}
			err := k8sClient.Delete(ctx, configMap)
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

			// Clean up all deployments created by v2 tests
			for i := range deploymentList.Items {
				deployment := &deploymentList.Items[i]
				if strings.HasPrefix(deployment.Spec.Template.Labels["app"], "v2-test-resource") {
					err = k8sClient.Delete(ctx, deployment)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete deployment")
				}
			}

			// Clean up all VariantAutoscaling resources created by v2 tests
			for i := range variantAutoscalingList.Items {
				va := &variantAutoscalingList.Items[i]
				if strings.HasPrefix(va.Name, "v2-test-resource") {
					err = k8sClient.Delete(ctx, va)
					Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred(), "Failed to delete VariantAutoscaling resource")
				}
			}

		})

		It("should successfully run optimization with source infrastructure", func() {

			// Initialize legacy MetricsCollector for non-saturation metrics
			// Create minimal test config with saturation config
			testConfig := config.NewTestConfig()
			testConfig.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {},
			})
			engine := NewEngine(k8sClient, k8sClient.Scheme(), nil, sourceRegistry, testConfig)

			By("Performing optimization loop with source infrastructure")
			err := engine.optimize(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying optimization completed successfully")
			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			err = k8sClient.List(ctx, &variantAutoscalingList)
			Expect(err).NotTo(HaveOccurred())

			// Verify VAs were processed
			testVAs := 0
			for _, va := range variantAutoscalingList.Items {
				if strings.HasPrefix(va.Name, "v2-test-resource") && va.DeletionTimestamp.IsZero() {
					testVAs++
				}
			}
			Expect(testVAs).To(Equal(totalVAs), "Expected all test VAs to be present")
		})

	})

})
