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
package optimizer

import (
	"context"
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	collector "github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	analyzer "github.com/llm-d-incubation/workload-variant-autoscaler/internal/modelanalyzer"
	utils "github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	inferno "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/core"
	infernoManager "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/manager"
	infernoSolver "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/solver"
	testutils "github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

const (
	configMapName      = "workload-variant-autoscaler-variantautoscaling-config"
	configMapNamespace = "workload-variant-autoscaler-system"
)

var _ = Describe("Optimizer", Ordered, func() {
	var (
		ctx           context.Context
		scheme        *runtime.Scheme
		ns            *corev1.Namespace
		optimizer     *infernoSolver.Optimizer
		manager       *infernoManager.Manager
		systemData    *infernoConfig.SystemData
		system        *inferno.System
		engine        *VariantAutoscalingsEngine
		modelAnalyzer *analyzer.ModelAnalyzer

		acceleratorCm  map[string]map[string]string
		serviceClassCm map[string]string
		minNumReplicas int32 = 1
	)

	Context("Testing optimization", func() {

		readAccFunc := func(c client.Client, ctx context.Context, cmName, cmNamespace string) (map[string]map[string]string, error) {
			cm := corev1.ConfigMap{}
			err := utils.GetConfigMapWithBackoff(ctx, c, cmName, cmNamespace, &cm)
			if err != nil {
				return nil, fmt.Errorf("failed to read ConfigMap %s/%s: %w", cmNamespace, cmName, err)
			}
			out := make(map[string]map[string]string)
			for acc, accInfoStr := range cm.Data {
				accInfoMap := make(map[string]string)
				if err := json.Unmarshal([]byte(accInfoStr), &accInfoMap); err != nil {
					return nil, fmt.Errorf("failed to read entry %s in ConfigMap %s/%s: %w", acc, cmNamespace, cmName, err)
				}
				out[acc] = accInfoMap
			}
			return out, nil
		}

		readCmFunc := func(c client.Client, ctx context.Context, cmName, cmNamespace string) (map[string]string, error) {
			cm := corev1.ConfigMap{}
			err := utils.GetConfigMapWithBackoff(ctx, c, cmName, cmNamespace, &cm)
			if err != nil {
				return nil, err
			}
			return cm.Data, nil
		}

		BeforeAll(func() {
			ctx = context.Background()

			scheme = runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			Expect(llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)).To(Succeed())

			ns = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: configMapNamespace,
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			ns = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: configMapNamespace,
				},
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
		})

		BeforeEach(func() {
			By("creating the required configmap for optimization")
			configMap := testutils.CreateServiceClassConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateAcceleratorUnitCostConfigMap(ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, ns.Name)
			Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

			var err error
			acceleratorCm, err = readAccFunc(k8sClient, ctx, "accelerator-unit-costs", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())
			serviceClassCm, err = readCmFunc(k8sClient, ctx, "service-classes-config", configMapNamespace)
			Expect(err).NotTo(HaveOccurred())
			wvaConfigCm, err := readCmFunc(k8sClient, ctx, configMapName, configMapNamespace)
			Expect(err).NotTo(HaveOccurred())
			if wvaConfigCm["WVA_SCALE_TO_ZERO"] == "true" {
				minNumReplicas = 0
			}

			// WVA operates in unlimited mode - no inventory data needed
			systemData = utils.CreateSystemData(acceleratorCm, serviceClassCm)

			By("Creating test VariantAutoscaling resources")
			for i := 1; i <= 3; i++ {
				// Create Deployment first
				d := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("test-variantautoscaling-%d", i),
						Namespace: "default",
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: utils.Ptr(int32(1)),
						Selector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": fmt.Sprintf("test-variantautoscaling-%d", i)},
						},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": fmt.Sprintf("test-variantautoscaling-%d", i)},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "quay.io/infernoautoscaler/vllme:0.2.1-multi-arch",
										Ports: []corev1.ContainerPort{{ContainerPort: 80}},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, d)).To(Succeed())

				// Create VariantAutoscaling
				variantAutoscaling := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					TypeMeta: metav1.TypeMeta{
						APIVersion: "llm.d.incubation/v1alpha1",
						Kind:       "VariantAutoscaling",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("test-variantautoscaling-%d", i),
						Namespace: "default",
						Labels: map[string]string{
							"inference.optimization/acceleratorName": "A100",
						},
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ModelID:          "meta/llama0-70b",
						VariantID:        "meta/llama0-70b-A100-1",
						Accelerator:      "A100",
						AcceleratorCount: 1,
						VariantCost:      "10.5",
						MinReplicas:      &minNumReplicas,
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
				Expect(k8sClient.Create(ctx, variantAutoscaling)).To(Succeed())
			}

		})

		AfterEach(func() {
			cmAcc := &corev1.ConfigMap{}
			err := utils.GetConfigMapWithBackoff(ctx, k8sClient, "accelerator-unit-costs", configMapNamespace, cmAcc)
			Expect(err).NotTo(HaveOccurred(), "failed to get accelerator-unit-costs configmap")
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cmAcc))).To(Succeed())

			cmServClass := &corev1.ConfigMap{}
			err = utils.GetConfigMapWithBackoff(ctx, k8sClient, "service-classes-config", configMapNamespace, cmServClass)
			Expect(err).NotTo(HaveOccurred(), "failed to get service-class-config configmap")
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cmServClass))).To(Succeed())

			cmWvaClass := &corev1.ConfigMap{}
			err = utils.GetConfigMapWithBackoff(ctx, k8sClient, configMapName, configMapNamespace, cmWvaClass)
			Expect(err).NotTo(HaveOccurred(), "failed to get service-class-config configmap")
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, cmWvaClass))).To(Succeed())

			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			Expect(k8sClient.List(ctx, &variantAutoscalingList)).To(Succeed())
			for _, va := range variantAutoscalingList.Items {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &va))).To(Succeed())
			}

			var deploymentList appsv1.DeploymentList
			Expect(k8sClient.List(ctx, &deploymentList)).To(Succeed())
			for _, deploy := range deploymentList.Items {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &deploy))).To(Succeed())
			}
		})

		It(fmt.Sprintf("should perform optimization for multiple VariantAutoscalings - scaled to %d without load", minNumReplicas), func() {
			allAnalyzerResponses := make(map[string]*interfaces.ModelAnalyzeResponse)
			vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

			By("Populating VariantAutoscalings map from the cluster")
			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			Expect(k8sClient.List(ctx, &variantAutoscalingList)).To(Succeed())
			Expect(len(variantAutoscalingList.Items)).To(BeNumerically(">", 0), "no VariantAutoscalings found in the cluster")

			// Prepare list of VariantAutoscalings to be updated after optimization
			By("Preparing list of VariantAutoscalings to be updated after optimization")
			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList

			// Filter out deleted VariantAutoscalings
			By("Filtering out deleted VariantAutoscalings")
			activeVAs := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, len(variantAutoscalingList.Items))
			for _, va := range variantAutoscalingList.Items {
				if va.DeletionTimestamp.IsZero() {
					activeVAs = append(activeVAs, va)
				}
			}

			// Prepare system data with all VariantAutoscalings info
			By("Preparing system data with all VariantAutoscalings info")
			for _, va := range activeVAs {
				modelName := va.Spec.ModelID
				Expect(modelName).NotTo(BeEmpty(), "variantAutoscaling missing modelName label, skipping optimization - ", "variantAutoscaling-name: ", va.Name)

				_, className, err := utils.FindModelSLO(serviceClassCm, modelName)
				Expect(err).NotTo(HaveOccurred(), "failed to find model SLO for model - ", modelName, ", variantAutoscaling - ", va.Name)

				// Single-variant architecture: add the variant profile to system data
				err = utils.AddVariantProfileToSystemData(systemData, modelName, va.Spec.Accelerator, va.Spec.AcceleratorCount, &va.Spec.VariantProfile)
				Expect(err).NotTo(HaveOccurred(), "failed to add variant profile to system data for model - ", modelName, ", variantAutoscaling - ", va.Name)

				// In single-variant architecture, cost is in spec, not extracted from ConfigMap
				var deploy appsv1.Deployment
				err = utils.GetDeploymentWithBackoff(ctx, k8sClient, va.Name, va.Namespace, &deploy)
				Expect(err).NotTo(HaveOccurred(), "failed to get deployment for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)

				var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
				err = utils.GetVariantAutoscalingWithBackoff(ctx, k8sClient, deploy.Name, deploy.Namespace, &updateVA)
				Expect(err).NotTo(HaveOccurred(), "failed to get variantAutoscaling for deployment - ", "deployment-name: ", deploy.Name)

				// Collect allocation and metrics using new API
				// In single-variant architecture, cost is in spec, not passed to collector
				currentAllocation, err := collector.CollectAllocationForDeployment(updateVA.Spec.VariantID, updateVA.Spec.Accelerator, deploy)
				Expect(err).NotTo(HaveOccurred(), "unable to collect allocation data for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)
				load, ttftAvg, itlAvg, err := collector.CollectAggregateMetrics(ctx, updateVA.Spec.ModelID, deploy.Namespace, &testutils.MockPromAPI{})
				Expect(err).NotTo(HaveOccurred(), "unable to fetch aggregate metrics for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)
				updateVA.Status.CurrentAlloc = currentAllocation

				// Extract metrics to internal structure (all metrics passed separately)
				metrics, err := interfaces.NewVariantMetrics(load, ttftAvg, itlAvg)
				Expect(err).NotTo(HaveOccurred(), "failed to parse variant metrics for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)

				// Pass empty scale-to-zero config for this test
				err = utils.AddServerInfoToSystemData(systemData, &updateVA, className, metrics, utils.ScaleToZeroConfigData{})
				Expect(err).NotTo(HaveOccurred(), "failed to add server info to system data for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)

				By("Updating system data with VariantAutoscaling info")
				vaFullName := utils.FullName(va.Name, va.Namespace)
				updateList.Items = append(updateList.Items, updateVA)
				vaMap[vaFullName] = &va
			}

			system = inferno.NewSystem()
			optimizerSpec := system.SetFromSpec(&systemData.Spec)
			optimizer = infernoSolver.NewOptimizerFromSpec(optimizerSpec)
			manager = infernoManager.NewManager(system, optimizer)

			engine = NewVariantAutoscalingsEngine(manager, system)
			modelAnalyzer = analyzer.NewModelAnalyzer(system)
			Expect(engine).NotTo(BeNil())
			Expect(modelAnalyzer).NotTo(BeNil())

			// Analyze
			By("Analyzing step")
			for _, s := range system.Servers() {
				modelAnalyzeResponse := modelAnalyzer.AnalyzeModel(ctx, *vaMap[s.Name()])
				Expect(len(modelAnalyzeResponse.Allocations)).To(BeNumerically(">", 0), "Expected at least one allocation from model analyzer for server - ", s.Name())
				allAnalyzerResponses[s.Name()] = modelAnalyzeResponse
			}

			By("Performing optimization")
			scaleToZeroConfigData := make(utils.ScaleToZeroConfigData)
			scaleToZeroCache := collector.NewScaleToZeroMetricsCache()
			optimizedAllocs, err := engine.Optimize(ctx, updateList, allAnalyzerResponses, &scaleToZeroConfigData, scaleToZeroCache)
			Expect(err).NotTo(HaveOccurred(), "unable to perform model optimization")
			Expect(len(optimizedAllocs)).To(Equal(len(updateList.Items)), "Expected optimized allocations for all VariantAutoscalings")
			for key, value := range optimizedAllocs {
				logger.Log.Info("Optimized allocation entry - ", "key: ", key, ", value: ", value)
				Expect(value.NumReplicas).To(Equal(minNumReplicas), fmt.Sprintf("Expected optimized number of replicas to be %d under no load for VariantAutoscaling - %s", minNumReplicas, key))
			}
		})

		It("should perform optimization for multiple VariantAutoscalings - scale out under load pressure", func() {
			allAnalyzerResponses := make(map[string]*interfaces.ModelAnalyzeResponse)
			vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

			// Setup MockPromAPI with high load metrics to simulate load pressure
			mockProm := &testutils.MockPromAPI{
				QueryResults: make(map[string]model.Value),
				QueryErrors:  make(map[string]error),
			}

			By("Populating VariantAutoscalings map from the cluster")
			var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
			Expect(k8sClient.List(ctx, &variantAutoscalingList)).To(Succeed())
			Expect(len(variantAutoscalingList.Items)).To(BeNumerically(">", 0), "no VariantAutoscalings found in the cluster")

			// Prepare list of VariantAutoscalings to be updated after optimization
			By("Preparing list of VariantAutoscalings to be updated after optimization")
			var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList

			// Filter out deleted VariantAutoscalings
			By("Filtering out deleted VariantAutoscalings")
			activeVAs := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, len(variantAutoscalingList.Items))
			for _, va := range variantAutoscalingList.Items {
				if va.DeletionTimestamp.IsZero() {
					activeVAs = append(activeVAs, va)
				}
			}

			// Prepare system data with all VariantAutoscalings info
			By("Preparing system data with all VariantAutoscalings info")
			for _, va := range activeVAs {
				modelName := va.Spec.ModelID
				Expect(modelName).NotTo(BeEmpty(), "variantAutoscaling missing modelName label, skipping optimization - ", "variantAutoscaling-name: ", va.Name)

				_, className, err := utils.FindModelSLO(serviceClassCm, modelName)
				Expect(err).NotTo(HaveOccurred(), "failed to find model SLO for model - ", modelName, ", variantAutoscaling - ", va.Name)

				// Single-variant architecture: add the variant profile to system data
				err = utils.AddVariantProfileToSystemData(systemData, modelName, va.Spec.Accelerator, va.Spec.AcceleratorCount, &va.Spec.VariantProfile)
				Expect(err).NotTo(HaveOccurred(), "failed to add variant profile to system data for model - ", modelName, ", variantAutoscaling - ", va.Name)

				// In single-variant architecture, cost is in spec, not extracted from ConfigMap
				var deploy appsv1.Deployment
				err = utils.GetDeploymentWithBackoff(ctx, k8sClient, va.Name, va.Namespace, &deploy)
				Expect(err).NotTo(HaveOccurred(), "failed to get deployment for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)

				var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
				err = utils.GetVariantAutoscalingWithBackoff(ctx, k8sClient, deploy.Name, deploy.Namespace, &updateVA)
				Expect(err).NotTo(HaveOccurred(), "failed to get variantAutoscaling for deployment - ", "deployment-name: ", deploy.Name)

				// Setup high load metrics for simulation
				testNamespace := va.Namespace
				arrivalQuery := testutils.CreateArrivalQuery(modelName, testNamespace)
				avgDecToksQuery := testutils.CreateDecToksQuery(modelName, testNamespace)
				avgPromptToksQuery := testutils.CreatePromptToksQuery(modelName, testNamespace)
				ttftQuery := testutils.CreateTTFTQuery(modelName, testNamespace)
				itlQuery := testutils.CreateITLQuery(modelName, testNamespace)
				// High load metrics that should trigger scaling up
				mockProm.QueryResults[arrivalQuery] = model.Vector{
					&model.Sample{Value: model.SampleValue(20.0)},
				}
				mockProm.QueryResults[avgDecToksQuery] = model.Vector{
					&model.Sample{Value: model.SampleValue(200.0)},
				}
				mockProm.QueryResults[avgPromptToksQuery] = model.Vector{
					&model.Sample{Value: model.SampleValue(20)},
				}
				mockProm.QueryResults[ttftQuery] = model.Vector{
					&model.Sample{Value: model.SampleValue(0.02)},
				}
				mockProm.QueryResults[itlQuery] = model.Vector{
					&model.Sample{Value: model.SampleValue(0.008)},
				}

				// Collect allocation and metrics using new API
				// In single-variant architecture, cost is in spec, not passed to collector
				currentAllocation, err := collector.CollectAllocationForDeployment(updateVA.Spec.VariantID, updateVA.Spec.Accelerator, deploy)
				Expect(err).NotTo(HaveOccurred(), "unable to collect allocation data for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)
				load, ttftAvg, itlAvg, err := collector.CollectAggregateMetrics(ctx, updateVA.Spec.ModelID, deploy.Namespace, mockProm)
				Expect(err).NotTo(HaveOccurred(), "unable to fetch aggregate metrics for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)
				updateVA.Status.CurrentAlloc = currentAllocation

				// Extract metrics to internal structure (all metrics passed separately)
				metrics, err := interfaces.NewVariantMetrics(load, ttftAvg, itlAvg)
				Expect(err).NotTo(HaveOccurred(), "failed to parse variant metrics for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)

				// Pass empty scale-to-zero config for this test
				err = utils.AddServerInfoToSystemData(systemData, &updateVA, className, metrics, utils.ScaleToZeroConfigData{})
				Expect(err).NotTo(HaveOccurred(), "failed to add server info to system data for variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)

				By("Updating system data with VariantAutoscaling info")
				vaFullName := utils.FullName(va.Name, va.Namespace)
				updateList.Items = append(updateList.Items, updateVA)
				vaMap[vaFullName] = &va
			}

			system = inferno.NewSystem()
			optimizerSpec := system.SetFromSpec(&systemData.Spec)
			optimizer = infernoSolver.NewOptimizerFromSpec(optimizerSpec)
			manager = infernoManager.NewManager(system, optimizer)

			engine = NewVariantAutoscalingsEngine(manager, system)
			modelAnalyzer = analyzer.NewModelAnalyzer(system)
			Expect(engine).NotTo(BeNil())
			Expect(modelAnalyzer).NotTo(BeNil())

			// Analyze
			By("Analyzing step")
			for _, s := range system.Servers() {
				modelAnalyzeResponse := modelAnalyzer.AnalyzeModel(ctx, *vaMap[s.Name()])
				Expect(len(modelAnalyzeResponse.Allocations)).To(BeNumerically(">", 0), "Expected at least one allocation from model analyzer for server - ", s.Name())
				allAnalyzerResponses[s.Name()] = modelAnalyzeResponse
			}

			By("Performing optimization")
			scaleToZeroConfigData := make(utils.ScaleToZeroConfigData)
			scaleToZeroCache := collector.NewScaleToZeroMetricsCache()
			optimizedAllocs, err := engine.Optimize(ctx, updateList, allAnalyzerResponses, &scaleToZeroConfigData, scaleToZeroCache)
			Expect(err).NotTo(HaveOccurred(), "unable to perform model optimization")
			Expect(len(optimizedAllocs)).To(Equal(len(updateList.Items)), "Expected optimized allocations for all VariantAutoscalings")
			for key, value := range optimizedAllocs {
				logger.Log.Info("Optimized allocation entry - ", "key: ", key, ", value: ", value)
				Expect(value.NumReplicas).To(BeNumerically(">", 1), "Expected optimized number of replicas to be higher than 1 under high load for VariantAutoscaling - ", key)
			}
		})
	})
})
