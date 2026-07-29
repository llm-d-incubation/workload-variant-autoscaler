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

package main

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
)

func TestThroughputAnalyzerEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name     string
		analyzer []config.AnalyzerScoreConfig
		want     bool
	}{
		{
			name:     "absent — no saturation config entries",
			analyzer: nil,
			want:     false,
		},
		{
			name: "absent — other analyzers present, throughput missing",
			analyzer: []config.AnalyzerScoreConfig{
				{Name: "saturation"},
			},
			want: false,
		},
		{
			name: "enabled — explicit Enabled:true",
			analyzer: []config.AnalyzerScoreConfig{
				{Name: throughput.AnalyzerName, Enabled: &trueVal},
			},
			want: true,
		},
		{
			name: "enabled — present with Enabled nil (defaults true)",
			analyzer: []config.AnalyzerScoreConfig{
				{Name: throughput.AnalyzerName},
			},
			want: true,
		},
		{
			name: "disabled — explicit Enabled:false",
			analyzer: []config.AnalyzerScoreConfig{
				{Name: throughput.AnalyzerName, Enabled: &falseVal},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
				"default": {Analyzers: tt.analyzer},
			})

			got := throughputAnalyzerEnabled(cfg)
			if got != tt.want {
				t.Errorf("throughputAnalyzerEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
