package saturation

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// shouldCollectClusterInventory reports whether the per-cycle
// collector.CollectInventoryK8S call is appropriate for the active
// configuration. The intent is to keep quota-mode deployments free of any
// Node API traffic: when the operator selected --limiter-type=quota, the
// physical-capacity path is deliberately disabled and listing Nodes here
// (even just for logging) would defeat that contract.
func shouldCollectClusterInventory(cfg *config.Config) bool {
	return cfg.LimiterMode() == config.LimiterTypeInventory
}
