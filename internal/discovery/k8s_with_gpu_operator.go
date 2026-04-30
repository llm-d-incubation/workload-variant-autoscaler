package discovery

import (
	"context"
	"fmt"
	"os"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// vendors list for GPU vendors
var vendors = []string{
	"nvidia.com",
	"amd.com",
	"intel.com",
}

// K8sWithGpuOperator implements CapacityDiscovery for Kubernetes clusters with GPU Operator
type K8sWithGpuOperator struct {
	Client         client.Client
	metricsEmitter *metrics.MetricsEmitter
}

// NewK8sWithGpuOperator creates a new K8sWithGpuOperator instance.
func NewK8sWithGpuOperator(client client.Client) *K8sWithGpuOperator {
	return &K8sWithGpuOperator{
		Client:         client,
		metricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// Discover discovers GPU capacity by iterating over nodes and checking GFD labels.
// It queries nodes for each GPU vendor (NVIDIA, AMD, Intel) separately since
// Kubernetes LabelSelectors don't support OR logic across different label keys.
func (d *K8sWithGpuOperator) Discover(ctx context.Context) (map[string]map[string]AcceleratorModelInfo, error) {
	inv := make(map[string]map[string]AcceleratorModelInfo)

	// Parse WVA_NODE_SELECTOR once for reuse across vendor queries
	var userRequirements []labels.Requirement
	if selectorStr := os.Getenv("WVA_NODE_SELECTOR"); selectorStr != "" {
		userSelector, err := labels.Parse(selectorStr)
		if err != nil {
			return nil, fmt.Errorf("invalid WVA_NODE_SELECTOR: %w", err)
		}
		userRequirements, _ = userSelector.Requirements()
	}

	// Query nodes for each GPU vendor separately
	// K8s LabelSelectors don't support OR logic across different keys (e.g. nvidia OR amd)
	for _, vendor := range vendors {
		prodKey := vendor + "/gpu.product"

		// Create vendor-specific selector
		req, err := labels.NewRequirement(prodKey, selection.Exists, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create label requirement for %s: %w", vendor, err)
		}
		selector := labels.NewSelector().Add(*req)

		// Add user requirements for sharding
		for _, userReq := range userRequirements {
			selector = selector.Add(userReq)
		}

		var nodeList corev1.NodeList
		if err := d.Client.List(ctx, &nodeList, &client.ListOptions{LabelSelector: selector}); err != nil {
			return nil, fmt.Errorf("failed to list nodes for vendor %s: %w", vendor, err)
		}

		// Process nodes for this vendor
		total := 0
		for _, node := range nodeList.Items {
			nodeName := node.Name
			memKey := vendor + "/gpu.memory"

			model, ok := node.Labels[prodKey]
			if !ok {
				continue
			}

			mem := node.Labels[memKey]
			count := 0
			if cap, ok := node.Status.Allocatable[corev1.ResourceName(vendor+"/gpu")]; ok {
				count = int(cap.Value())
				total += count
			}

			if inv[nodeName] == nil {
				inv[nodeName] = make(map[string]AcceleratorModelInfo)
			}

			inv[nodeName][model] = AcceleratorModelInfo{
				Count:  count,
				Memory: mem,
			}
		}

		d.metricsEmitter.EmitAvailableGPUsMetric(vendor, int32(total)) // emit as soon as discovered
	}

	return inv, nil
}

// DiscoverUsage calculates current GPU usage by summing GPU requests from running pods.
// Returns a map of accelerator type to used GPU count.
func (d *K8sWithGpuOperator) DiscoverUsage(ctx context.Context) (map[string]int, error) {
	// First, build a map of node name -> GPU type
	nodeGPUType, err := d.discoverNodeGPUTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover node GPU types: %w", err)
	}

	// List all pods (running or pending on a node)
	var podList corev1.PodList
	if err := d.Client.List(ctx, &podList); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Aggregate GPU requests by accelerator type
	usageByType := make(map[string]int)

	for _, pod := range podList.Items {
		// Skip pods that aren't scheduled or are completed/failed
		if pod.Spec.NodeName == "" {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		// Get the GPU type for this node
		gpuType, ok := nodeGPUType[pod.Spec.NodeName]
		if !ok {
			// Node doesn't have GPUs, skip
			continue
		}

		// Sum GPU requests from all containers
		gpuCount := getPodGPURequests(&pod)
		if gpuCount > 0 {
			usageByType[gpuType] += gpuCount
		}
	}

	return usageByType, nil
}

// discoverNodeGPUTypes returns a map of node name to GPU type (model name).
// It queries nodes for each GPU vendor separately to support multi-vendor clusters.
func (d *K8sWithGpuOperator) discoverNodeGPUTypes(ctx context.Context) (map[string]string, error) {
	nodeGPUType := make(map[string]string)

	// Parse WVA_NODE_SELECTOR once for reuse across vendor queries
	var userRequirements []labels.Requirement
	if selectorStr := os.Getenv("WVA_NODE_SELECTOR"); selectorStr != "" {
		userSelector, err := labels.Parse(selectorStr)
		if err != nil {
			return nil, fmt.Errorf("invalid WVA_NODE_SELECTOR: %w", err)
		}
		userRequirements, _ = userSelector.Requirements()
	}

	// Query nodes for each GPU vendor separately
	for _, vendor := range vendors {
		prodKey := vendor + "/gpu.product"

		req, err := labels.NewRequirement(prodKey, selection.Exists, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create label requirement for %s: %w", vendor, err)
		}
		selector := labels.NewSelector().Add(*req)

		// Add user requirements for sharding
		for _, userReq := range userRequirements {
			selector = selector.Add(userReq)
		}

		var nodeList corev1.NodeList
		if err := d.Client.List(ctx, &nodeList, &client.ListOptions{LabelSelector: selector}); err != nil {
			return nil, fmt.Errorf("failed to list nodes for vendor %s: %w", vendor, err)
		}

		for _, node := range nodeList.Items {
			if model, ok := node.Labels[prodKey]; ok {
				nodeGPUType[node.Name] = model
			}
		}
	}

	return nodeGPUType, nil
}

// getPodGPURequests returns the total GPU requests for a pod across all containers.
// For regular containers, GPUs are summed (they run concurrently).
// For init containers, we take the max (they run sequentially).
// The final result is max(initContainerMax, regularContainerSum) since init containers
// complete before regular containers start.
func getPodGPURequests(pod *corev1.Pod) int {
	// Sum GPU requests from regular containers (run concurrently)
	regularTotal := 0
	for _, container := range pod.Spec.Containers {
		for _, vendor := range vendors {
			resName := corev1.ResourceName(vendor + "/gpu")
			if qty, ok := container.Resources.Requests[resName]; ok {
				regularTotal += int(qty.Value())
			}
		}
	}

	// Find max GPU request from init containers (run sequentially)
	initMax := 0
	for _, container := range pod.Spec.InitContainers {
		containerGPUs := 0
		for _, vendor := range vendors {
			resName := corev1.ResourceName(vendor + "/gpu")
			if qty, ok := container.Resources.Requests[resName]; ok {
				containerGPUs += int(qty.Value())
			}
		}
		if containerGPUs > initMax {
			initMax = containerGPUs
		}
	}

	// Return max of init containers and regular containers
	// (init containers finish before regular containers start)
	if initMax > regularTotal {
		return initMax
	}
	return regularTotal
}

// Ensure K8sWithGpuOperator implements FullDiscovery
var _ FullDiscovery = (*K8sWithGpuOperator)(nil)
