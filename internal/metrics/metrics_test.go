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

package metrics

import (
	"context"
	"strings"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordError(t *testing.T) {
	tests := []struct {
		name          string
		component     string
		errorType     string
		callCount     int
		expectedValue float64
	}{
		{
			name:          "single error increment",
			component:     constants.ComponentController,
			errorType:     "TestError",
			callCount:     1,
			expectedValue: 1.0,
		},
		{
			name:          "multiple error increments",
			component:     constants.ComponentController,
			errorType:     "AnotherError",
			callCount:     3,
			expectedValue: 3.0,
		},
		{
			name:          "analyzer error",
			component:     constants.ComponentAnalyzer,
			errorType:     "AnalyzerError",
			callCount:     2,
			expectedValue: 2.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a new registry for each test to ensure isolation
			registry := prometheus.NewRegistry()

			// Initialize metrics with the test registry
			err := InitMetrics(registry)
			require.NoError(t, err)

			ctx := context.Background()

			// Call RecordError the specified number of times
			for i := 0; i < tt.callCount; i++ {
				RecordError(ctx, tt.component, tt.errorType)
			}

			// Method 1: Using testutil.ToFloat64 - simplest for single metric
			metricName := constants.WVAErrorsTotal
			labels := prometheus.Labels{
				constants.LabelComponent: tt.component,
				constants.LabelErrorType: tt.errorType,
			}
			actualValue := testutil.ToFloat64(errorsTotal.With(labels))
			assert.Equal(t, tt.expectedValue, actualValue,
				"Counter should be incremented to %v", tt.expectedValue)

			// Method 2: Using Gather and manual inspection
			metricFamilies, err := registry.Gather()
			require.NoError(t, err)

			found := false
			for _, mf := range metricFamilies {
				if mf.GetName() == metricName {
					for _, metric := range mf.GetMetric() {
						// Check if labels match
						labelMatch := true
						for _, label := range metric.GetLabel() {
							expectedVal, exists := labels[label.GetName()]
							if exists && label.GetValue() != expectedVal {
								labelMatch = false
								break
							}
						}

						if labelMatch {
							found = true
							assert.Equal(t, tt.expectedValue, metric.GetCounter().GetValue(),
								"Counter value from Gather should match expected")
						}
					}
				}
			}
			assert.True(t, found, "Metric with matching labels should be found")
		})
	}
}

// Note: Testing with controller_instance label requires a separate test run
// because Prometheus metrics cannot change their label cardinality after creation.
// To test controller_instance behavior, set CONTROLLER_INSTANCE env var before
// running the test suite, or test it in integration/e2e tests.

func TestRecordErrorNotInitialized(t *testing.T) {
	// Save original errorsTotal
	originalErrorsTotal := errorsTotal
	defer func() {
		errorsTotal = originalErrorsTotal
	}()

	// Set errorsTotal to nil to simulate uninitialized state
	errorsTotal = nil

	ctx := context.Background()

	// RecordError will panic when errorsTotal is nil because metrics must be initialized
	// before calling RecordError. This is by design - callers must use InitMetrics first.
	require.Panics(t, func() {
		RecordError(ctx, constants.ComponentController, "TestError")
	}, "RecordError should panic when errorsTotal is nil (metrics not initialized)")
}

func TestRecordErrorMetricFormat(t *testing.T) {
	// Test that the metric can be scraped in Prometheus exposition format
	registry := prometheus.NewRegistry()
	err := InitMetrics(registry)
	require.NoError(t, err)

	ctx := context.Background()
	RecordError(ctx, constants.ComponentController, "ConfigMapError")

	// Use testutil.CollectAndCompare to verify metric format
	expected := `
		# HELP wva_errors_total Total number of errors by component
		# TYPE wva_errors_total counter
		wva_errors_total{component="controller",error_type="ConfigMapError"} 1
	`

	err = testutil.CollectAndCompare(errorsTotal, strings.NewReader(expected))
	assert.NoError(t, err, "Metric should match expected Prometheus format")
}
