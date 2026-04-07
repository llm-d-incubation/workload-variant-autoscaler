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

package controller

import (
	"context"

	"github.com/go-logr/logr"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// parseSaturationConfig parses saturation scaling configuration from ConfigMap data.
// Returns the parsed configs and count of successfully parsed entries.
func parseSaturationConfig(cmData map[string]string, logger logr.Logger) (config.SaturationScalingConfigPerModel, int) {
	configs := make(config.SaturationScalingConfigPerModel)
	count := 0
	for key, yamlStr := range cmData {
		var satConfig config.SaturationScalingConfig
		if err := yaml.Unmarshal([]byte(yamlStr), &satConfig); err != nil {
			logger.Error(err, "Failed to parse saturation scaling config entry", "key", key)
			continue
		}
		// Apply defaults before validation (handles omitempty zero-values like scaleUpThreshold)
		satConfig.ApplyDefaults()
		if err := satConfig.Validate(); err != nil {
			logger.Error(err, "Invalid saturation scaling config entry", "key", key)
			continue
		}
		configs[key] = satConfig
		count++
	}
	return configs, count
}

// parseQMAnalyzerConfig parses queueing model configuration from ConfigMap data.
// Returns the parsed configs and count of successfully parsed entries.
// Invalid or unparseable entries are skipped with an error log.
func parseQMAnalyzerConfig(cmData map[string]string, logger logr.Logger) (config.QMAnalyzerConfigPerModel, int) {
	configs := make(config.QMAnalyzerConfigPerModel)
	count := 0
	for key, yamlStr := range cmData {
		var qmConfig interfaces.QueueingModelScalingConfig
		if err := yaml.Unmarshal([]byte(yamlStr), &qmConfig); err != nil {
			logger.Error(err, "Failed to parse queueing model config entry", "key", key)
			continue
		}
		if err := qmConfig.Validate(); err != nil {
			logger.Error(err, "Invalid queueing model config entry", "key", key)
			continue
		}
		configs[key] = qmConfig
		count++
	}
	return configs, count
}

// isNamespaceConfigEnabled checks if a namespace has the opt-in label for namespace-local ConfigMaps.
// This allows namespaces to opt-in for ConfigMap watching even before VAs are created.
// Package-level function so it can be used by both reconcilers.
func isNamespaceConfigEnabled(ctx context.Context, c client.Reader, namespace string) bool {
	if namespace == "" {
		return false
	}

	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		// If namespace doesn't exist or we can't read it, default to not enabled
		// This is safe - we'll proceed with normal logic
		return false
	}

	labels := ns.GetLabels()
	if labels == nil {
		return false
	}

	value, exists := labels[constants.NamespaceConfigEnabledLabelKey]
	return exists && value == constants.AnnotationValueTrue
}

// isNamespaceExcluded checks if a namespace has the exclude annotation.
// Excluded namespaces are not watched for ConfigMaps or reconciled for VAs.
// Thread-safe (reads namespace object from API server).
// Package-level function so it can be used by both reconcilers.
func isNamespaceExcluded(ctx context.Context, c client.Reader, namespace string) bool {
	if namespace == "" {
		return false
	}

	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		// If namespace doesn't exist or we can't read it, default to not excluded
		// This is safe - we'll proceed with normal logic
		return false
	}

	annotations := ns.GetAnnotations()
	if annotations == nil {
		return false
	}

	value, exists := annotations[constants.NamespaceExcludeAnnotationKey]
	return exists && value == constants.AnnotationValueTrue
}
