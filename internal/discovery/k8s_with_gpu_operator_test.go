package discovery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDiscover_NvidiaOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia-1",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-A100-PCIE-80GB",
					"nvidia.com/gpu.memory":  "81920",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia-2",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
					"nvidia.com/gpu.memory":  "81920",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("8"),
				},
			},
		},
		// CPU-only node (should be excluded)
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-cpu-only",
				Labels: map[string]string{},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Should find 2 NVIDIA nodes
	assert.Len(t, result, 2)
	assert.Contains(t, result, "node-nvidia-1")
	assert.Contains(t, result, "node-nvidia-2")

	// Verify node-nvidia-1
	assert.Equal(t, 4, result["node-nvidia-1"]["NVIDIA-A100-PCIE-80GB"].Count)
	assert.Equal(t, "81920", result["node-nvidia-1"]["NVIDIA-A100-PCIE-80GB"].Memory)

	// Verify node-nvidia-2
	assert.Equal(t, 8, result["node-nvidia-2"]["NVIDIA-H100-SXM5-80GB"].Count)
}

func TestDiscover_AMDOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd-1",
				Labels: map[string]string{
					"amd.com/gpu.product": "AMD-MI300X-192G",
					"amd.com/gpu.memory":  "196608",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"amd.com/gpu": resource.MustParse("8"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd-2",
				Labels: map[string]string{
					"amd.com/gpu.product": "AMD-MI250-128G",
					"amd.com/gpu.memory":  "131072",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"amd.com/gpu": resource.MustParse("4"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Should find 2 AMD nodes
	assert.Len(t, result, 2)
	assert.Contains(t, result, "node-amd-1")
	assert.Contains(t, result, "node-amd-2")

	// Verify AMD node details
	assert.Equal(t, 8, result["node-amd-1"]["AMD-MI300X-192G"].Count)
	assert.Equal(t, "196608", result["node-amd-1"]["AMD-MI300X-192G"].Memory)
	assert.Equal(t, 4, result["node-amd-2"]["AMD-MI250-128G"].Count)
}

func TestDiscover_MixedVendors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
					"nvidia.com/gpu.memory":  "81920",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd",
				Labels: map[string]string{
					"amd.com/gpu.product": "AMD-MI300X-192G",
					"amd.com/gpu.memory":  "196608",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"amd.com/gpu": resource.MustParse("8"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-intel",
				Labels: map[string]string{
					"intel.com/gpu.product": "Intel-Gaudi-2-96GB",
					"intel.com/gpu.memory":  "98304",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"intel.com/gpu": resource.MustParse("8"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Should find all 3 nodes from different vendors
	assert.Len(t, result, 3)
	assert.Contains(t, result, "node-nvidia")
	assert.Contains(t, result, "node-amd")
	assert.Contains(t, result, "node-intel")

	// Verify each vendor's GPU details
	assert.Equal(t, 4, result["node-nvidia"]["NVIDIA-H100-SXM5-80GB"].Count)
	assert.Equal(t, 8, result["node-amd"]["AMD-MI300X-192G"].Count)
	assert.Equal(t, 8, result["node-intel"]["Intel-Gaudi-2-96GB"].Count)
}

func TestDiscover_WithNodeSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-shard-a",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-A100-PCIE-80GB",
					"wva.llmd.ai/shard":      "a",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-shard-b",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-A100-PCIE-80GB",
					"wva.llmd.ai/shard":      "b",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	// Set node selector to only match shard-a
	t.Setenv("WVA_NODE_SELECTOR", "wva.llmd.ai/shard=a")

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)

	// Should only find shard-a node
	assert.Len(t, result, 1)
	assert.Contains(t, result, "node-shard-a")
}

func TestDiscoverUsage_MixedVendors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("4"),
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd",
				Labels: map[string]string{
					"amd.com/gpu.product": "AMD-MI300X-192G",
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"amd.com/gpu": resource.MustParse("8"),
				},
			},
		},
	}

	pods := []runtime.Object{
		// Pod on NVIDIA node using 2 GPUs
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-nvidia-1",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				NodeName: "node-nvidia",
				Containers: []corev1.Container{
					{
						Name: "gpu-container",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								"nvidia.com/gpu": resource.MustParse("2"),
							},
						},
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		// Pod on AMD node using 4 GPUs
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-amd-1",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{
				NodeName: "node-amd",
				Containers: []corev1.Container{
					{
						Name: "gpu-container",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								"amd.com/gpu": resource.MustParse("4"),
							},
						},
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
	}

	allObjects := make([]runtime.Object, 0, len(nodes)+len(pods))
	allObjects = append(allObjects, nodes...)
	allObjects = append(allObjects, pods...)
	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(allObjects...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.DiscoverUsage(context.Background())
	require.NoError(t, err)

	// Should track usage per GPU type
	assert.Equal(t, 2, result["NVIDIA-H100-SXM5-80GB"])
	assert.Equal(t, 4, result["AMD-MI300X-192G"])
}

func TestDiscoverNodeGPUTypes_MixedVendors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-nvidia",
				Labels: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-SXM5-80GB",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-amd",
				Labels: map[string]string{
					"amd.com/gpu.product": "AMD-MI300X-192G",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-intel",
				Labels: map[string]string{
					"intel.com/gpu.product": "Intel-Gaudi-2-96GB",
				},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.discoverNodeGPUTypes(context.Background())
	require.NoError(t, err)

	assert.Len(t, result, 3)
	assert.Equal(t, "NVIDIA-H100-SXM5-80GB", result["node-nvidia"])
	assert.Equal(t, "AMD-MI300X-192G", result["node-amd"])
	assert.Equal(t, "Intel-Gaudi-2-96GB", result["node-intel"])
}

func TestGetPodGPURequests_MixedVendors(t *testing.T) {
	// Pod with both NVIDIA and AMD GPU requests (unusual but should be handled)
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "nvidia-container",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("2"),
						},
					},
				},
				{
					Name: "amd-container",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							"amd.com/gpu": resource.MustParse("4"),
						},
					},
				},
			},
		},
	}

	result := getPodGPURequests(pod)
	// Should sum all GPU requests across vendors
	assert.Equal(t, 6, result)
}

func TestDiscover_EmptyCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestDiscover_NoGPUNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	nodes := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-cpu-1",
				Labels: map[string]string{},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-cpu-2",
				Labels: map[string]string{},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(nodes...).Build()
	discoverer := NewK8sWithGpuOperator(client)

	result, err := discoverer.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, result)
}
