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

// Package epp_saturation implements an analyzer that consumes the pool-level
// saturation score emitted by the EPP's latency detector plugin. Unlike the
// V1/V2 saturation analyzers that scrape per-pod vLLM metrics and compute
// capacity internally, this analyzer relies on a pre-computed signal:
//
//	saturation = predictedLatency / SLO  (averaged across endpoints)
//
// The EPP exposes this as inference_extension_latency_detector_pool_saturation.
// A value < 1.0 means the pool has headroom; >= 1.0 means it is at or over SLO.
package epp_saturation

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturation"
	ctrl "sigs.k8s.io/controller-runtime"
)

// AnalyzerName is the canonical name used in config and result metadata.
const AnalyzerName = "epp-saturation"

// EPPSaturationAnalyzer implements interfaces.Analyzer using the EPP's
// pool-level saturation signal. It assumes a single model per pool.
type EPPSaturationAnalyzer struct {
	metricsRegistry *source.SourceRegistry
}

// NewEPPSaturationAnalyzer creates a new EPP saturation analyzer.
func NewEPPSaturationAnalyzer(registry *source.SourceRegistry) *EPPSaturationAnalyzer {
	return &EPPSaturationAnalyzer{
		metricsRegistry: registry,
	}
}

// Name implements interfaces.Analyzer.
func (a *EPPSaturationAnalyzer) Name() string {
	return AnalyzerName
}

// Analyze implements interfaces.Analyzer.
// It queries the EPP pool saturation metric from Prometheus and translates
// it into capacity signals for the optimizer pipeline.
func (a *EPPSaturationAnalyzer) Analyze(ctx context.Context, input interfaces.AnalyzerInput) (*interfaces.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	cfg, ok := input.Config.(*EPPSaturationConfig)
	if !ok {
		return nil, fmt.Errorf("expected *EPPSaturationConfig, got %T", input.Config)
	}

	// Query the EPP pool saturation metric
	saturationScore, err := a.queryPoolSaturation(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query EPP pool saturation: %w", err)
	}

	logger.Info("EPP pool saturation score",
		"modelID", input.ModelID,
		"saturation", saturationScore,
		"scaleUpThreshold", cfg.ScaleUpThreshold,
		"scaleDownBoundary", cfg.ScaleDownBoundary)

	// Compute current replica count from variant states
	var totalReplicas int
	for _, vs := range input.VariantStates {
		totalReplicas += vs.CurrentReplicas
	}
	if totalReplicas == 0 {
		totalReplicas = 1
	}

	// Normalized capacity model for proportional scaling.
	//
	// We normalize the pool's capacity to 1.0 (its full SLO budget) so that
	// each replica contributes perReplicaCapacity = 1/N of the total. The
	// saturation score IS the demand in normalized units (demand/capacity ratio).
	//
	// This makes the optimizer compute proportional replica deltas:
	//   replicasNeeded = ceil(requiredCapacity / perReplicaCapacity)
	//                  = ceil(requiredCapacity * N)
	//
	// Examples (scaleUpThreshold=0.85, scaleDownBoundary=0.50):
	//
	//   saturation=0.95, N=4:
	//     required = 0.95/0.85 - 1.0 = 0.118
	//     replicas = ceil(0.118 * 4) = ceil(0.47) = 1  (add 1 of 4)
	//
	//   saturation=0.95, N=40:
	//     required = 0.118
	//     replicas = ceil(0.118 * 40) = ceil(4.7) = 5  (add 5 of 40)
	//
	//   saturation=0.30, N=40:
	//     spare = 1.0 - 0.30/0.50 = 0.40
	//     replicas = floor(0.40 * 40) = 16  (remove 16 of 40 → 24 left, sat→0.50)
	//
	perReplicaCapacity := 1.0 / float64(totalReplicas)
	totalSupply := 1.0                // normalized pool capacity
	totalDemand := saturationScore    // saturation = demand/capacity

	utilization := saturationScore
	if utilization > 1.0 {
		utilization = 1.0 // cap for the 0-1 field; raw score preserved in demand
	}

	// Scaling signals using the same threshold logic as V2:
	// requiredCapacity > 0 → scale-up needed
	// spareCapacity > 0 → scale-down possible
	requiredCapacity := math.Max(0, totalDemand/cfg.ScaleUpThreshold-totalSupply)
	spareCapacity := math.Max(0, totalSupply-totalDemand/cfg.ScaleDownBoundary)

	// Build per-variant capacity breakdown.
	// Single model per pool: distribute proportionally across variants.
	variantCapacities := make([]interfaces.VariantCapacity, 0, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}

		variantSupply := float64(readyCount) * perReplicaCapacity
		variantDemand := saturationScore * variantSupply
		vc := interfaces.VariantCapacity{
			VariantName:        vs.VariantName,
			AcceleratorName:    "", // not needed for EPP-based signal
			Cost:               saturation.DefaultVariantCost,
			ReplicaCount:       readyCount,
			PendingReplicas:    vs.PendingReplicas,
			PerReplicaCapacity: perReplicaCapacity,
			TotalCapacity:      variantSupply,
			TotalDemand:        variantDemand,
			Utilization:        saturationScore,
		}
		variantCapacities = append(variantCapacities, vc)
	}

	return &interfaces.AnalyzerResult{
		AnalyzerName:      a.Name(),
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        time.Now(),
		VariantCapacities: variantCapacities,
		TotalSupply:       totalSupply,
		TotalDemand:       totalDemand,
		Utilization:       utilization,
		RequiredCapacity:  requiredCapacity,
		SpareCapacity:     spareCapacity,
	}, nil
}

// queryPoolSaturation fetches the EPP pool saturation score from Prometheus.
func (a *EPPSaturationAnalyzer) queryPoolSaturation(ctx context.Context) (float64, error) {
	promSource := a.metricsRegistry.Get("prometheus")
	if promSource == nil {
		return 0, fmt.Errorf("prometheus source not registered")
	}

	results, err := promSource.Refresh(ctx, source.RefreshSpec{
		Queries: []string{registration.QueryEPPPoolSaturation},
		Params:  map[string]string{},
	})
	if err != nil {
		return 0, fmt.Errorf("prometheus refresh failed: %w", err)
	}

	result, ok := results[registration.QueryEPPPoolSaturation]
	if !ok || result == nil {
		return 0, fmt.Errorf("no result for query %s", registration.QueryEPPPoolSaturation)
	}
	if result.Error != nil {
		return 0, fmt.Errorf("query %s failed: %w", registration.QueryEPPPoolSaturation, result.Error)
	}
	if len(result.Values) == 0 {
		return 0, fmt.Errorf("query %s returned no values (EPP latency detector may not be running)", registration.QueryEPPPoolSaturation)
	}

	return result.Values[0].Value, nil
}
