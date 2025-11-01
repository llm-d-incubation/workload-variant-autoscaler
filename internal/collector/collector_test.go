package collector

import (
	"context"
	"fmt"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

var _ = Describe("Collector", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		logger.Log = zap.NewNop().Sugar()

		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		Expect(llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)).To(Succeed())
	})

	// TODO: Re-enable these tests when implementing limited mode support
	// These tests are for CollectInventoryK8S which is not used in unlimited mode
	PContext("When collecting inventory from K8s", func() {
		It("should collect GPU inventory from multiple nodes", func() {
			// Create nodes with fake GPU labels
			nodes := []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-1",
						Labels: map[string]string{
							"nvidia.com/gpu.product": "A100",
							"nvidia.com/gpu.memory":  "40Gi",
						},
					},
					Status: corev1.NodeStatus{
						Allocatable: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("4"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-2",
						Labels: map[string]string{
							"amd.com/gpu.product": "MI300X",
							"amd.com/gpu.memory":  "192Gi",
						},
					},
					Status: corev1.NodeStatus{
						Allocatable: corev1.ResourceList{
							"amd.com/gpu": resource.MustParse("2"),
						},
					},
				},
			}

			// Create fake client with nodes
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(&nodes[0], &nodes[1]).
				Build()

				// Validate results
			inventory, err := CollectInventoryK8S(ctx, fakeClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(inventory).To(HaveLen(2))

			Expect(inventory["gpu-node-1"]).To(HaveKey("A100"))
			Expect(inventory["gpu-node-1"]["A100"].Count).To(Equal(4))
			Expect(inventory["gpu-node-1"]["A100"].Memory).To(Equal("40Gi"))

			// Check gpu-node-2
			Expect(inventory["gpu-node-2"]).To(HaveKey("MI300X"))
			Expect(inventory["gpu-node-2"]["MI300X"].Count).To(Equal(2))
			Expect(inventory["gpu-node-2"]["MI300X"].Memory).To(Equal("192Gi"))
		})

		It("should handle nodes without GPU labels", func() {
			// Create node without GPU labels
			nodes := []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-1",
						// no GPU labels
					},
					Status: corev1.NodeStatus{
						// no allocatable resources
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "gpu-node-2",
						Labels: map[string]string{
							// no GPU labels
						},
					},
					Status: corev1.NodeStatus{
						// no allocatable resources
					},
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(&nodes[0], &nodes[1]).
				Build()

			inventory, err := CollectInventoryK8S(ctx, fakeClient)

			Expect(err).NotTo(HaveOccurred())
			Expect(inventory).To(BeEmpty())
		})

		It("should handle missing GPU capacity", func() {
			// Create node with GPU labels but no allocatable capacity
			nodes := []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-1",
						Labels: map[string]string{
							"nvidia.com/gpu.product": "A100",
							"nvidia.com/gpu.memory":  "40Gi",
						},
					},
					Status: corev1.NodeStatus{
						Allocatable: corev1.ResourceList{
							// no allocatable capacity
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-2",
						Labels: map[string]string{
							"amd.com/gpu.product": "MI300X",
							"amd.com/gpu.memory":  "192Gi",
						},
					},
					Status: corev1.NodeStatus{
						Allocatable: corev1.ResourceList{
							// no allocatable capacity
						},
					},
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(&nodes[0], &nodes[1]).
				Build()

			inventory, err := CollectInventoryK8S(ctx, fakeClient)

			Expect(err).NotTo(HaveOccurred())
			Expect(inventory).To(HaveLen(2))
			Expect(inventory["gpu-node-1"]["A100"].Count).To(Equal(0))
			Expect(inventory["gpu-node-1"]["A100"].Memory).To(Equal("40Gi"))
			Expect(inventory["gpu-node-2"]["MI300X"].Count).To(Equal(0))
			Expect(inventory["gpu-node-2"]["MI300X"].Memory).To(Equal("192Gi"))
		})

		It("should handle multiple GPU types on same node", func() {
			// Create node with multiple vendor GPUs
			nodes := []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-1",
						Labels: map[string]string{
							"nvidia.com/gpu.product": "A100",
							"nvidia.com/gpu.memory":  "40Gi",
							"intel.com/gpu.product":  "G2",
							"intel.com/gpu.memory":   "96Gi",
						},
					},
					Status: corev1.NodeStatus{
						Allocatable: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("2"),
							"intel.com/gpu":  resource.MustParse("1")},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-2",
						Labels: map[string]string{
							"amd.com/gpu.product": "MI300X",
							"amd.com/gpu.memory":  "192Gi",
						},
					},
					Status: corev1.NodeStatus{
						Allocatable: corev1.ResourceList{
							"amd.com/gpu": resource.MustParse("2"),
						},
					},
				},
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(&nodes[0], &nodes[1]).
				Build()

			inventory, err := CollectInventoryK8S(ctx, fakeClient)

			Expect(err).NotTo(HaveOccurred())
			Expect(inventory).To(HaveLen(2))
			Expect(inventory["gpu-node-1"]).To(HaveLen(2))
			Expect(inventory["gpu-node-1"]["A100"].Count).To(Equal(2))
			Expect(inventory["gpu-node-1"]["G2"].Count).To(Equal(1))
			Expect(inventory["gpu-node-2"]).To(HaveLen(1))
			Expect(inventory["gpu-node-2"]["MI300X"].Count).To(Equal(2))
			Expect(inventory["gpu-node-2"]["MI300X"].Memory).To(Equal("192Gi"))
		})
	})

	Context("When collecting allocation and metrics", func() {
		var (
			deployment    appsv1.Deployment
			modelID       string
			testNamespace string
			variantID     string
			accelerator   string
			mockProm      *utils.MockPromAPI
		)

		BeforeEach(func() {
			modelID = "test-model"
			testNamespace = "test-namespace"
			variantID = "test-model-A100-1"
			accelerator = "A100"

			replicas := int32(2)
			deployment = appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
				},
			}

			mockProm = &utils.MockPromAPI{
				QueryResults: make(map[string]model.Value),
				QueryErrors:  make(map[string]error),
			}
		})

		It("should collect allocation and metrics successfully", func() {
			// Setup mock responses for aggregate metrics
			arrivalQuery := utils.CreateArrivalQuery(modelID, testNamespace)
			promptTokensQuery := utils.CreatePromptToksQuery(modelID, testNamespace)
			tokenQuery := utils.CreateDecToksQuery(modelID, testNamespace)
			ttftQuery := utils.CreateTTFTQuery(modelID, testNamespace)
			itlQuery := utils.CreateITLQuery(modelID, testNamespace)

			mockProm.QueryResults[arrivalQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(0.175)}, // 0.175 requests/sec (will be * 60 = 10.5 req/min)
			}
			mockProm.QueryResults[promptTokensQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(100.0)}, // 100 input tokens per request
			}
			mockProm.QueryResults[tokenQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(150.0)}, // 150 output tokens per request
			}
			mockProm.QueryResults[ttftQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(0.5)}, // 0.5 seconds
			}
			mockProm.QueryResults[itlQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(0.05)}, // 0.05 seconds
			}

			// Test new API - CollectAllocationForDeployment
			allocation, err := CollectAllocationForDeployment(variantID, accelerator, deployment)
			Expect(err).NotTo(HaveOccurred())
			// Note: In single-variant architecture, Accelerator, VariantID, MaxBatch, and VariantCost
			// are in the VA spec, not in the Allocation status
			Expect(allocation.NumReplicas).To(Equal(int32(2)))

			// Test new API - CollectAggregateMetrics
			load, ttftAvg, itlAvg, err := CollectAggregateMetrics(ctx, modelID, testNamespace, mockProm)
			Expect(err).NotTo(HaveOccurred())
			Expect(ttftAvg).To(Equal("500.00"))              // 0.5 * 1000 ms
			Expect(itlAvg).To(Equal("50.00"))                // 0.05 * 1000 ms
			Expect(load.ArrivalRate).To(Equal("10.50"))      // req per min
			Expect(load.AvgOutputTokens).To(Equal("150.00")) // tokens per req
		})

		It("should handle empty accelerator gracefully", func() {
			// Test with empty accelerator string
			allocation, err := CollectAllocationForDeployment(variantID, "", deployment)
			Expect(err).NotTo(HaveOccurred())
			// Note: In single-variant architecture, Accelerator is in the VA spec, not Allocation status
			Expect(allocation.NumReplicas).To(Equal(int32(2)))
		})

		It("should handle Prometheus query errors", func() {
			// Setup error for arrival query
			arrivalQuery := utils.CreateArrivalQuery(modelID, testNamespace)
			mockProm.QueryErrors[arrivalQuery] = fmt.Errorf("prometheus connection failed")

			// CollectAggregateMetrics should return error
			_, _, _, err := CollectAggregateMetrics(ctx, modelID, testNamespace, mockProm)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("prometheus connection failed"))
		})

		It("should handle empty metric results gracefully", func() {
			// Setup empty responses (no data points)
			arrivalQuery := utils.CreateArrivalQuery(modelID, testNamespace)
			promptTokensQuery := utils.CreatePromptToksQuery(modelID, testNamespace)
			tokenQuery := utils.CreateDecToksQuery(modelID, testNamespace)
			ttftQuery := utils.CreateTTFTQuery(modelID, testNamespace)
			itlQuery := utils.CreateITLQuery(modelID, testNamespace)

			// Empty vectors (no data)
			mockProm.QueryResults[arrivalQuery] = model.Vector{}
			mockProm.QueryResults[promptTokensQuery] = model.Vector{}
			mockProm.QueryResults[tokenQuery] = model.Vector{}
			mockProm.QueryResults[ttftQuery] = model.Vector{}
			mockProm.QueryResults[itlQuery] = model.Vector{}

			// CollectAggregateMetrics should handle empty data gracefully
			load, ttftAvg, itlAvg, err := CollectAggregateMetrics(ctx, modelID, testNamespace, mockProm)
			Expect(err).NotTo(HaveOccurred())
			Expect(ttftAvg).To(Equal("0.00"))
			Expect(itlAvg).To(Equal("0.00"))
			Expect(load.ArrivalRate).To(Equal("0.00"))
			Expect(load.AvgInputTokens).To(Equal("0.00"))
			Expect(load.AvgOutputTokens).To(Equal("0.00"))
		})
	})

	Context("When testing FixValue func", func() {
		It("should fix NaN values", func() {
			val := math.NaN()
			FixValue(&val)
			Expect(val).To(Equal(0.0))
		})

		It("should fix positive infinity", func() {
			val := math.Inf(1)
			FixValue(&val)
			Expect(val).To(Equal(0.0))
		})

		It("should fix negative infinity", func() {
			val := math.Inf(-1)
			FixValue(&val)
			Expect(val).To(Equal(0.0))
		})

		It("should not modify normal values", func() {
			val := 42.5
			FixValue(&val)
			Expect(val).To(Equal(42.5))
		})

		It("should not modify zero", func() {
			val := 0.0
			FixValue(&val)
			Expect(val).To(Equal(0.0))
		})

		It("should not modify negative values", func() {
			val := -15.3
			FixValue(&val)
			Expect(val).To(Equal(-15.3))
		})
	})

	Context("When testing AcceleratorModelInfo struct", func() {
		It("should create AcceleratorModelInfo correctly", func() {
			info := AcceleratorModelInfo{
				Count:  8,
				Memory: "80Gi",
			}

			Expect(info.Count).To(Equal(8))
			Expect(info.Memory).To(Equal("80Gi"))
		})
	})

	Context("When testing MetricKV struct", func() {
		It("should create MetricKV correctly", func() {
			metric := MetricKV{
				Name: "test_metric",
				Labels: map[string]string{
					"model": "test-model",
					"gpu":   "A100",
				},
				Value: 123.45,
			}

			Expect(metric.Name).To(Equal("test_metric"))
			Expect(metric.Labels).To(HaveKeyWithValue("model", "test-model"))
			Expect(metric.Labels).To(HaveKeyWithValue("gpu", "A100"))
			Expect(metric.Value).To(Equal(123.45))
		})
	})

	// TODO: Re-enable when implementing limited mode support
	PContext("When testing vendor list", func() {
		It("should have expected GPU vendors", func() {
			expectedVendors := []string{
				"nvidia.com",
				"amd.com",
				"intel.com",
			}

			Expect(vendors).To(ConsistOf(expectedVendors))
		})
	})
})
