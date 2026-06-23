package constants

import (
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// Global backoff configurations
var (
	// Standard backoff for most operations
	StandardBackoff = wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    5,
	}

	// Slow backoff for operations that need more time
	ReconcileBackoff = wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
	}

	// Lightweight backoff for individual Prometheus queries (collector, etc.)
	PrometheusQueryBackoff = wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    5, // 500ms, 1s, 2s, 4s = ~7.5s total
	}

	// Prometheus validation backoff with longer intervals
	// TODO: investigate why Prometheus needs longer backoff durations
	PrometheusValidationBackoff = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    6, // 5s, 10s, 20s, 40s, 80s, 160s = ~5 minutes total
	}
)

type GpuInfo struct {
	Vendor              string
	ResourceName        string
	ProductLabel        string
	ProductLabelAliases []string
	MemoryLabel         string
}

var (
	// vendorResources lists each supported GPU resource and its discovery labels.
	VendorResources = []GpuInfo{
		{
			Vendor:              "NVIDIA",
			ResourceName:        "nvidia.com/gpu",
			ProductLabel:        "nvidia.com/gpu.product",
			ProductLabelAliases: []string{"cloud.google.com/gke-accelerator"},
			MemoryLabel:         "nvidia.com/gpu.memory",
		},
		{
			Vendor:       "AMD",
			ResourceName: "amd.com/gpu",
			ProductLabel: "amd.com/gpu.product-name",
			MemoryLabel:  "amd.com/gpu.memory",
		},
		// NOTE: Node labeling rules installed for Node Feature Discovery (NFD) by Intel GPU operator,
		// provide product labels only for Data Center products. Current Intel Gaudi / GPU operators
		// do not label nodes with device memory information, that info needs to be labeled separately.
		{
			Vendor:       "Intel",
			ResourceName: "habana.ai/gaudi",
			ProductLabel: "habana.ai/product.name",
			MemoryLabel:  "habana.ai/device.memory",
		},
		{
			Vendor:       "Intel",
			ResourceName: "gpu.intel.com/i915",
			ProductLabel: "gpu.intel.com/product",
			MemoryLabel:  "gpu.intel.com/memory",
		},
		{
			Vendor:       "Intel",
			ResourceName: "gpu.intel.com/xe",
			ProductLabel: "gpu.intel.com/product",
			MemoryLabel:  "gpu.intel.com/memory",
		},
	}

	SpecReplicasFallback int32 = 1 // in case Spec.Replicas is nil
)

// Kubernetes resource kinds and API versions for supported scale targets.
const (
	DeploymentKind            = "Deployment"
	DeploymentAPIVersion      = "apps/v1"
	LeaderWorkerSetKind       = "LeaderWorkerSet"
	LeaderWorkerSetAPIVersion = "leaderworkerset.x-k8s.io/v1"

	// K8s Events
	K8SEventScaledUp                  = "ScaledUp"
	K8SEventScaledDown                = "ScaledDown"
	K8SEventResourceConstrained       = "ResourceConstrained"
	K8SEventMetricsUnavailable        = "MetricsUnavailable"
	K8SEventScaledToZero              = "ScaledToZero"
	K8SEventOptimizationFailed        = "OptimizationFailed"
	EnforcerPolicyTypeScaleToZero     = "scale_to_zero"
	EnforcerPolicyTypeMinimumReplicas = "minimum_replicas"

	// DefaultAcceleratorName is used internally by the GPU limiter when the
	// accelerator type cannot be resolved from the scale target or VA label.
	// In homogeneous clusters (single GPU type), the limiter resolves this to
	// the real type before it reaches status or metrics. This value must never
	// be persisted to VA status or used as a Prometheus label.
	DefaultAcceleratorName = "unknown"
)

// Component names identify WVA components for observability (metrics, logging, tracing).
const (
	ComponentCollector  = "collector"
	ComponentAnalyzer   = "analyzer"
	ComponentOptimizer  = "optimizer"
	ComponentLimiter    = "limiter"
	ComponentEnforcer   = "enforcer"
	ComponentController = "controller"
)

// IsAcceleratorResolved returns true if the accelerator name is a real GPU type
// (not empty and not the "unknown" sentinel).
func IsAcceleratorResolved(name string) bool {
	return name != "" && name != DefaultAcceleratorName
}
