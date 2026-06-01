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

var (
	// gpuVendors lists the resource name prefixes for GPU vendors
	GpuVendors = []string{"nvidia.com", "amd.com", "intel.com"}

	// GpuProductKeys are the node selector/affinity keys used to identify GPU products
	GpuProductKeys = []string{
		"nvidia.com/gpu.product",
		"amd.com/gpu.product-name",
		"cloud.google.com/gke-accelerator",
	}

	SpecReplicasFallback int32 = 1 // in case Spec.Replicas is nil
)

// Kubernetes resource kinds and API versions for supported scale targets.
const (
	DeploymentKind            = "Deployment"
	DeploymentAPIVersion      = "apps/v1"
	LeaderWorkerSetKind       = "LeaderWorkerSet"
	LeaderWorkerSetAPIVersion = "leaderworkerset.x-k8s.io/v1"

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
