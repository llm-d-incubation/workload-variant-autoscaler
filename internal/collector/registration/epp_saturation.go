/*
Copyright 2025 The llm-d Authors

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

package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
)

// Query name constants for EPP saturation metrics.
const (
	// QueryEPPPoolSaturation is the pool-level saturation score from the EPP's
	// latency detector probe. The value is predictedLatency / SLO averaged
	// across all endpoints: < 1.0 means headroom, >= 1.0 means at/over SLO.
	// Source: inference_extension_latency_detector_pool_saturation (EPP)
	QueryEPPPoolSaturation = "epp_pool_saturation"
)

// RegisterEPPSaturationQueries registers queries used by the EPP saturation analyzer.
func RegisterEPPSaturationQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// Pool-level saturation from the EPP latency detector.
	// This is a scalar gauge with no labels — it represents the aggregate
	// predicted saturation across all endpoints in the pool.
	// The namespace param is used only for cache key differentiation.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryEPPPoolSaturation,
		Type:        source.QueryTypePromQL,
		Template:    `inference_extension_latency_detector_pool_saturation`,
		Params:      []string{},
		Description: "Pool-level saturation score from EPP latency detector (predictedLatency / SLO, averaged across endpoints)",
	})
}
