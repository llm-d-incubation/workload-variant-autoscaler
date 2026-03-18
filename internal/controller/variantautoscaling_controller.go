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
	"fmt"

	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// VariantAutoscalingReconciler reconciles a variantAutoscaling object
type VariantAutoscalingReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	Recorder  record.EventRecorder
	Config    *config.Config      // Unified configuration (injected from main.go)
	Datastore datastore.Datastore // Datastore for namespace tracking and InferencePool data
}

// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;list;update;patch;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="apps",resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update;list;watch
// Note: The broad ConfigMap permission above is required for namespace-local ConfigMap overrides.
// The controller filters by well-known names (wva-saturation-scaling-config, wva-model-scale-to-zero-config)
// in its predicate logic, providing effective access control.
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// Note: Namespace watch permission is required for label-based namespace opt-in for namespace-local ConfigMaps.
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

const (
	// ServiceMonitor constants for watching controller's own metrics ServiceMonitor
	defaultServiceMonitorName = "workload-variant-autoscaler-controller-manager-metrics-monitor"
)

var (
	// ServiceMonitor GVK for watching controller's own metrics ServiceMonitor
	serviceMonitorGVK = schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	}
)

func (r *VariantAutoscalingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// NOTE: The reconciliation loop is being incrementally refactored so things may look a bit messy.
	// Changes in progress:
	// - reconcile loop will process one VA at a time. During the refactoring it does both, one and all

	// BEGIN: Per VA logic
	logger := ctrl.LoggerFrom(ctx)

	// Get the specific VA object that triggered this reconciliation
	var va llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	if err := r.Get(ctx, req.NamespacedName, &va); err != nil { // Get returns, by default, a deep copy of the object
		if apierrors.IsNotFound(err) {
			logger.Info("VariantAutoscaling resource not found, may have been deleted",
				"name", req.Name,
				"namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to fetch VariantAutoscaling",
			"name", req.Name,
			"namespace", req.Namespace)
		return ctrl.Result{}, err
	}

	// Keep a copy of the original object for Patch generation
	originalVA := va.DeepCopy()

	// Skip if the VA is being deleted
	if !va.DeletionTimestamp.IsZero() {
		logger.Info("VariantAutoscaling is being deleted, skipping reconciliation",
			"name", va.Name,
			"namespace", va.Namespace)
		// Untrack namespace when VA is deleted
		r.Datastore.NamespaceUntrack("VariantAutoscaling", va.Name, va.Namespace)
		return ctrl.Result{}, nil
	}

	// Track namespace for namespace-local ConfigMap watching
	// Moved after deletion check to avoid tracking deleted VAs
	// Idempotent: tracking the same VA multiple times (e.g., on retry) has no effect
	r.Datastore.NamespaceTrack("VariantAutoscaling", va.Name, va.Namespace)
	logger.Info("Reconciling VariantAutoscaling",
		"name", va.Name,
		"namespace", va.Namespace,
		"modelID", va.Spec.ModelID)

	// Attempts to resolve the target model variant using scaleTargetRef

	// Fetch scale target Deployment
	scaleTargetName := va.GetScaleTargetName()

	var deployment appsv1.Deployment
	if err := utils.GetDeploymentWithBackoff(ctx, r.Client, scaleTargetName, va.Namespace, &deployment); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Scale target Deployment not found, waiting for deployment watch",
				"name", scaleTargetName,
				"namespace", va.Namespace)

			// Update status to reflect target not found
			llmdVariantAutoscalingV1alpha1.SetCondition(&va,
				llmdVariantAutoscalingV1alpha1.TypeTargetResolved,
				metav1.ConditionFalse,
				llmdVariantAutoscalingV1alpha1.ReasonTargetNotFound,
				fmt.Sprintf("Scale target Deployment %s not found", scaleTargetName))

			if err := r.Status().Patch(ctx, &va, client.MergeFrom(fullDesiredAllocPatchBase(originalVA, &va))); err != nil {
				logger.Error(err, "Failed to update VariantAutoscaling status")
				return ctrl.Result{}, err
			}

			// Don't requeue - the deployment watch will trigger reconciliation
			// when the target deployment is created
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get scale target Deployment",
			"name", scaleTargetName,
			"namespace", va.Namespace)
		return ctrl.Result{}, err
	}

	// Target found
	llmdVariantAutoscalingV1alpha1.SetCondition(&va,
		llmdVariantAutoscalingV1alpha1.TypeTargetResolved,
		metav1.ConditionTrue,
		llmdVariantAutoscalingV1alpha1.ReasonTargetFound,
		fmt.Sprintf("Scale target Deployment %s found", scaleTargetName))

	logger.V(logging.DEBUG).Info(
		fmt.Sprintf("Scale target Deployment found: name=%s, namespace=%s", scaleTargetName, va.Namespace),
	)

	// Process Engine Decisions from Shared Cache
	// This mechanism allows the Engine to trigger updates without touching the API server directly.
	if decision, ok := common.DecisionCache.Get(va.Name, va.Namespace); ok {
		// Log scaling outcome and reason for E2E and operator debugging (why did/didn't scaling happen).
		logger.Info("Applying scaling decision from cache",
			"va", va.Name,
			"namespace", va.Namespace,
			"desiredReplicas", decision.TargetReplicas,
			"metricsAvailable", decision.MetricsAvailable,
			"metricsReason", decision.MetricsReason,
			"metricsMessage", decision.MetricsMessage,
			"reason", decision.Reason)
		// Only apply if the decision is fresher than the last one applied or if we haven't applied it
		// Note: We blindly apply for now, assuming the Engine acts as the source of truth for "Desired" state
		numReplicas, accelerator, lastRunTime := common.DecisionToOptimizedAlloc(decision)

		// Only update DesiredOptimizedAlloc if we have a valid accelerator (required by CRD).
		// Note: numReplicas may legitimately be 0 for scale-to-zero scenarios.
		// Replace the entire struct to ensure all required fields are included in the patch.
		if accelerator != "" {
			va.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
				NumReplicas: numReplicas,
				Accelerator: accelerator,
				LastRunTime: lastRunTime,
			}
		} else {
			// When we have a partial decision (no accelerator yet), explicitly preserve
			// the existing DesiredOptimizedAlloc from the fetched object to avoid
			// sending zero-valued struct in the patch which would fail CRD validation.
			va.Status.DesiredOptimizedAlloc = originalVA.Status.DesiredOptimizedAlloc
		}

		// Always apply MetricsAvailable condition from cache
		metricsStatus := metav1.ConditionFalse
		if decision.MetricsAvailable {
			metricsStatus = metav1.ConditionTrue
		}
		llmdVariantAutoscalingV1alpha1.SetCondition(&va,
			llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
			metricsStatus,
			decision.MetricsReason,
			decision.MetricsMessage)

		// Note: CurrentAlloc is removed from Status.
		// Internal allocation state is managed by the Engine and Actuator.
	} else {
		logger.Info("No decision found in cache for VA", "va", va.Name, "namespace", va.Namespace)
	}

	// Patch status — use fullDesiredAllocPatchBase to ensure the complete
	// desiredOptimizedAlloc object is always included in the merge patch.
	// Without this, MergeFrom only includes changed fields within the struct,
	// and the CRD validates the partial patch — rejecting it when required
	// fields (numReplicas, accelerator) are absent. See: #731
	if err := r.Status().Patch(ctx, &va, client.MergeFrom(fullDesiredAllocPatchBase(originalVA, &va))); err != nil {
		logger.Error(err, "Failed to update VariantAutoscaling status",
			"name", va.Name)
		return ctrl.Result{}, err
	}

	// END: Per VA logic

	return ctrl.Result{}, nil
}

// fullDesiredAllocPatchBase returns a patch base that forces the full
// desiredOptimizedAlloc object into the JSON merge patch. Without this,
// MergeFrom only includes changed fields within nested structs, and the
// CRD validates the partial patch — rejecting it when the required field
// (accelerator) is absent from the partial object.
// When desiredOptimizedAlloc hasn't been set yet (accelerator is empty),
// the base is left unchanged so the zero-valued struct is not included.
func fullDesiredAllocPatchBase(originalVA *llmdVariantAutoscalingV1alpha1.VariantAutoscaling, va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) *llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	base := originalVA.DeepCopy()
	if va.Status.DesiredOptimizedAlloc.Accelerator != "" {
		// Zero out the base so the entire modified desiredOptimizedAlloc
		// appears as a change and is fully included in the merge patch.
		base.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{}
	}
	return base
}

// handleDeploymentEvent maps Deployment events to VA reconcile requests.
// When a Deployment is created, this finds any VAs that reference it and triggers reconciliation.
// This handles the race condition where VA is created before its target deployment.
// Uses custom indexes for efficient VA lookup instead of listing all VAs.
func (r *VariantAutoscalingReconciler) handleDeploymentEvent(ctx context.Context, obj client.Object) []reconcile.Request {
	deploy, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}

	logger := ctrl.LoggerFrom(ctx)

	// Use indexed lookup for VA targeting this Deployment
	va, err := indexers.FindVAForDeployment(ctx, r.Client, deploy.Name, deploy.Namespace)
	if err != nil {
		logger.Error(err, "Failed to find VA for deployment event using index")
		return nil
	}

	if va == nil {
		return nil
	}

	logger.V(logging.DEBUG).Info("Deployment created, triggering VA reconciliation",
		"deployment", deploy.Name,
		"va", va.Name,
		"namespace", deploy.Namespace)

	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{
			Namespace: deploy.Namespace,
			Name:      va.Name,
		},
	}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *VariantAutoscalingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&llmdVariantAutoscalingV1alpha1.VariantAutoscaling{},
			// Filter VAs by controller-instance label and namespace exclusion
			builder.WithPredicates(VariantAutoscalingPredicate(mgr.GetClient(), r.Config)),
		).
		// Note: ConfigMap watching is now handled by ConfigMapReconciler
		// Watch ServiceMonitor for controller's own metrics
		Watches(
			&promoperator.ServiceMonitor{},
			handler.EnqueueRequestsFromMapFunc(r.handleServiceMonitorEvent),
			builder.WithPredicates(ServiceMonitorPredicate()),
		).
		// Watch Deployments to trigger VA reconciliation when target deployment is created
		// This handles the race condition where VA is created before its target deployment
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.handleDeploymentEvent),
			builder.WithPredicates(DeploymentPredicate()),
		).
		// Watch DecisionTrigger channel for Engine decisions
		// This enables the Engine to trigger reconciliation without updating the object in API server
		WatchesRawSource(
			source.Channel(common.DecisionTrigger, &handler.EnqueueRequestForObject{}),
		).
		Named("variantAutoscaling").
		WithEventFilter(EventFilter()).
		Complete(r)
}

// handleServiceMonitorEvent handles events for the controller's own ServiceMonitor.
// When ServiceMonitor is deleted, it logs an error and emits a Kubernetes event.
// This ensures that administrators are aware when the ServiceMonitor that enables
// Prometheus scraping of controller metrics (including optimized replicas) is missing.
//
// Note: This handler does not enqueue reconcile requests. ServiceMonitor deletion doesn't
// affect the optimization logic (which reads from Prometheus), but it prevents future
// metrics from being scraped. The handler exists solely for observability - logging and
// emitting Kubernetes events to alert operators of the issue.
func (r *VariantAutoscalingReconciler) handleServiceMonitorEvent(ctx context.Context, obj client.Object) []reconcile.Request {
	serviceMonitor, ok := obj.(*promoperator.ServiceMonitor)
	if !ok {
		return nil
	}

	logger := ctrl.LoggerFrom(ctx)
	name := serviceMonitor.Name
	namespace := serviceMonitor.Namespace

	// Check if ServiceMonitor is being deleted
	if !serviceMonitor.GetDeletionTimestamp().IsZero() {
		logger.V(logging.VERBOSE).Info("ServiceMonitor being deleted - Prometheus will not scrape controller metrics",
			"servicemonitor", name,
			"namespace", namespace,
			"impact", "Actuator will not be able to access optimized replicas metrics",
			"action", "ServiceMonitor must be recreated for metrics scraping to resume")

		// Emit Kubernetes event for observability
		if r.Recorder != nil {
			r.Recorder.Eventf(
				serviceMonitor,
				corev1.EventTypeWarning,
				"ServiceMonitorDeleted",
				"ServiceMonitor %s/%s is being deleted. Prometheus will not scrape controller metrics. Actuator will not be able to access optimized replicas metrics. Please recreate the ServiceMonitor.",
				namespace,
				name,
			)
		}

		// Don't trigger reconciliation - ServiceMonitor deletion doesn't affect optimization logic
		return nil
	}

	// For create/update events, no action needed
	// Don't trigger reconciliation - ServiceMonitor changes don't affect optimization logic
	return nil
}
