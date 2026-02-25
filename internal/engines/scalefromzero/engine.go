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

package scalefromzero

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
)

// Constants for condition
const (
	MetricsReasonAvailable  = "ScaleFromZero"
	MetricsMessageAvailable = "Scaled from zero due to pending requests"
	reason                  = "scalefromzero mode: pending request - scale-up"
	targetEPPMetricName     = "inference_extension_flow_control_queue_size"
	targetEPPMetricLabel    = "target_model_name"
)

type Engine struct {
	client         client.Client
	executor       executor.Executor
	Datastore      datastore.Datastore
	DynamicClient  dynamic.Interface
	Actuator       *actuator.DirectActuator
	Mapper         meta.RESTMapper
	maxConcurrency int
	config         *config.Config // Unified configuration (injected from main.go)
}

// NewEngine creates a new instance of the scale-from-zero engine.
// cfg must be non-nil (validated in main.go before engine creation).
func NewEngine(client client.Client, mapper meta.RESTMapper, restConfig *rest.Config, ds datastore.Datastore, cfg *config.Config) (*Engine, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil in NewEngine - this should not happen")
	}

	maxConcurrency := cfg.ScaleFromZeroMaxConcurrency()
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("invalid scale-from-zero max concurrency: must be positive, got %d", maxConcurrency)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	actuator, err := actuator.NewDirectActuator(restConfig)
	if err != nil {
		return nil, err
	}

	engine := Engine{
		client:         client,
		Datastore:      ds,
		DynamicClient:  dynamicClient,
		Actuator:       actuator,
		Mapper:         mapper,
		maxConcurrency: maxConcurrency,
		config:         cfg,
	}

	// TODO: replace by an hybrid, polling and reactive executor when available
	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     100 * time.Millisecond, // frequent polling to quickly detect scale-from-zero opportunities
		RetryBackoff: 100 * time.Millisecond,
	})

	return &engine, nil
}

// StartOptimizeLoop starts the optimization loop for the scale-from-zero engine.
// It runs until the context is cancelled.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.executor.Start(ctx)
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) error {
	logger := log.FromContext(ctx)

	// Get all inactive (replicas == 0) VAs
	inactiveVAs, err := utils.InactiveVariantAutoscaling(ctx, e.client)
	if err != nil {
		return err
	}

	logger.V(logging.DEBUG).Info("Found inactive VariantAutoscaling resources", "count", len(inactiveVAs))

	var wg sync.WaitGroup
	sem := make(chan struct{}, e.maxConcurrency)
	errorCh := make(chan error, e.maxConcurrency)

	// Start error aggregation in a separate goroutine to prevent deadlock
	var aggregatedErrors []error
	var errorWg sync.WaitGroup
	errorWg.Add(1)
	go func() {
		defer errorWg.Done()
		for err := range errorCh {
			if err != nil {
				aggregatedErrors = append(aggregatedErrors, err)
			}
		}
	}()

variantLoop:
	for _, va := range inactiveVAs {
		// Check if context is cancelled, but don't return immediately
		select {
		case <-ctx.Done():
			logger.V(logging.DEBUG).Info("Context cancelled, stopping new work")
			break variantLoop
		default:
		}

		logger.V(logging.DEBUG).Info("Processing variant", "name", va.Name)
		wg.Add(1)

		// This call blocks if the channel is full (concurrency limit reached)
		sem <- struct{}{}
		go func(variant wvav1alpha1.VariantAutoscaling) {
			defer wg.Done()
			defer func() { <-sem }()

			err := e.processInactiveVariant(ctx, variant, 1)
			if err != nil {
				logger.V(logging.DEBUG).Error(err, "Error Processing variant", "name", variant.Name)
				errorCh <- err
			} else {
				errorCh <- nil
			}
		}(va)
	}

	// Wait for all goroutines to complete, then close error channel
	wg.Wait()
	close(errorCh)

	// Wait for error aggregation to complete
	errorWg.Wait()

	// After all work is done, if the context was cancelled, return that error
	if err := ctx.Err(); err != nil {
		return err
	}

	if len(aggregatedErrors) > 0 {
		return errors.Join(aggregatedErrors...)
	}
	return nil
}

// ProcessInactiveVariant processes a single inactive VariantAutoscaling resource.
func (e *Engine) processInactiveVariant(ctx context.Context, va wvav1alpha1.VariantAutoscaling, targetWorkloadReplicas int) error {
	logger := log.FromContext(ctx)
	objAPI := va.GetScaleTargetAPI()
	objKind := va.GetScaleTargetKind()
	objName := va.GetScaleTargetName()

	// Parse Group, Version, Kind, Resource
	gvr, err := poolutil.GetResourceForKind(e.Mapper, objAPI, objKind)
	if err != nil {
		return err
	}

	unstructuredObj, err := e.DynamicClient.Resource(gvr).Namespace(va.Namespace).Get(ctx, objName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Extract Labels for the pods created by the ScaleTarget object
	labels, found, err := unstructured.NestedStringMap(unstructuredObj.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return err
	}

	if !found {
		return errors.New("labels are missing for target workload object")
	}

	// Check if inferencepool datastore is empty: this can happen during bootstrapping
	dsPoolList := e.Datastore.PoolList()
	if len(dsPoolList) == 0 {
		logger.Info("Inferencepool datastore is empty - skipping processing inactive variant", "value", va.Name)
		return nil
	}

	// Find target EPP for metrics collection
	pool, err := e.Datastore.PoolGetFromLabels(labels)
	if err != nil {
		logger.Error(err, "Error finding target EPP", "variant", va.Name, "target VA model", va.Spec.ModelID)
		return err
	}

	// Use EPP source from registry
	eppSource := e.Datastore.PoolGetMetricsSource(pool.Name)
	if eppSource == nil {
		return errors.New("endpointpicker metrics source not found in datastore")
	}

	results, err := eppSource.Refresh(ctx, source.RefreshSpec{})
	if err != nil {
		return err
	}

	// Check for pending requests using EPP flowcontrol queue size metrics
	result := results["all_metrics"]
	pendingRequestExist := false
	for _, value := range result.Values {
		metricName := value.Labels["__name__"]
		if metricName == targetEPPMetricName && value.Value > 0 {
			if value.Labels[targetEPPMetricLabel] == va.Spec.ModelID {
				logger.Info(
					"Target workload has pending requests, scaling up from zero", "metricName", metricName,
					"metric", value.Labels, "value", value.Value)
				pendingRequestExist = true
				break
			}
		}
	}

	if !pendingRequestExist {
		logger.V(logging.DEBUG).Info("No pending requests found in the flowcontrol queue - skipping scaling up from zero")
		return nil
	}

	// 1.  Scale up from zero to one
	// TODO: Right now we are scaling all the VA for the same target model. We need to scale only the VA that has the lowest cost.
	err = e.Actuator.ScaleTargetObject(ctx, unstructuredObj, int32(targetWorkloadReplicas))
	if err != nil {
		logger.Error(err, "Error scaling up Target Workload", "variant", va.Name, "target VA model", va.Spec.ModelID)
		return err
	}
	logger.Info("Successfully scaled up Target Workload", "variant", va.Name, "target VA model", va.Spec.ModelID, "inferencepool", pool.EndpointPicker.ServiceName)

	// 2. Create or update VariantDecision
	va.Status.Actuation.Applied = false
	// Determine accelerator - try status first, then labels
	var accelerator string
	accelerator = va.Status.DesiredOptimizedAlloc.Accelerator
	if accelerator == "" {
		// Try to get from VA labels as last resort
		if val, ok := va.Labels["inference.optimization/acceleratorName"]; ok && val != "" {
			accelerator = val
		}
	}

	decision, hasDecision := common.DecisionCache.Get(va.Name, va.Namespace)
	if !hasDecision {
		cost, err := strconv.ParseFloat(va.Spec.VariantCost, 64)
		if err != nil {
			return err
		}
		common.DecisionCache.Set(va.Name, va.Namespace, interfaces.VariantDecision{
			VariantName:        va.Name,
			Namespace:          va.Namespace,
			ModelID:            va.Spec.ModelID,
			Cost:               cost,
			TargetReplicas:     targetWorkloadReplicas, // Scale up to 1 replica
			CurrentReplicas:    targetWorkloadReplicas,
			DesiredReplicas:    targetWorkloadReplicas,
			LastRunTime:        metav1.Now(),
			SaturationBased:    false,
			SafetyOverride:     false,
			ModelBasedDecision: false,
			AcceleratorName:    accelerator,
			Reason:             reason, // Reason for scaling up
			MetricsAvailable:   true,
			MetricsReason:      MetricsReasonAvailable,
			MetricsMessage:     MetricsMessageAvailable,
		})
	} else {
		if decision.CurrentReplicas == 0 {
			decision.TargetReplicas = targetWorkloadReplicas
			decision.CurrentReplicas = targetWorkloadReplicas
			decision.DesiredReplicas = targetWorkloadReplicas
			decision.LastRunTime = metav1.Now()
			decision.SaturationBased = false
			decision.SafetyOverride = false
			decision.ModelBasedDecision = false
			decision.Reason = reason
			decision.AcceleratorName = accelerator
			decision.MetricsAvailable = true
			decision.MetricsReason = MetricsReasonAvailable
			decision.MetricsMessage = MetricsMessageAvailable
			common.DecisionCache.Set(va.Name, va.Namespace, decision)
		} else {
			logger.Info("Target variant decision.CurrentReplicas is not zero", "value", decision.CurrentReplicas)
		}
	}

	// 3. Updates VA status.
	va.Status.DesiredOptimizedAlloc = wvav1alpha1.OptimizedAlloc{
		NumReplicas: targetWorkloadReplicas,
		LastRunTime: metav1.Now(),
		Accelerator: accelerator,
	}

	// Set condition based on decision characteristics
	wvav1alpha1.SetCondition(&va,
		wvav1alpha1.TypeOptimizationReady,
		metav1.ConditionTrue,
		"ScaleFromZeroMode",
		fmt.Sprintf("scalefromzero decision: %s", reason))

	va.Status.Actuation.Applied = true

	// 4. Trigger Reconciler
	common.DecisionTrigger <- event.GenericEvent{
		Object: &va,
	}

	return nil
}
