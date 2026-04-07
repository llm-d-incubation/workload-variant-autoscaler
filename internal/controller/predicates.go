package controller

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ConfigMapPredicate returns a predicate that filters ConfigMap events to only the target ConfigMaps.
// It matches the enqueue function logic - allows either configmap name if namespace matches.
// This predicate is used to filter only the target configmaps.
//
// For namespace-local ConfigMap support:
// - Global ConfigMaps: well-known names in controller namespace
// - Namespace-local ConfigMaps: well-known names in watched or tracked namespaces
//
// Filtering behavior:
//   - Single-namespace mode (--watch-namespace set): Always allow ConfigMaps from the watched namespace
//   - Multi-namespace mode: Only allow ConfigMaps from tracked namespaces (namespaces with VAs)
//
// ds is the datastore used to check if a namespace is tracked (fast, in-memory check).
// cfg is the configuration used to check if single-namespace mode is enabled.
// Opt-in labels and exclusion are handled in the handler to avoid expensive API calls in the predicate.
func ConfigMapPredicate(ds datastore.Datastore, cfg *config.Config) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		name := obj.GetName()
		namespace := obj.GetNamespace()
		systemNamespace := config.SystemNamespace()

		// Well-known ConfigMap names
		wellKnownNames := map[string]bool{
			config.ConfigMapName():                 true,
			config.SaturationConfigMapName():       true,
			config.DefaultScaleToZeroConfigMapName: true,
			config.QMAnalyzerConfigMapName():       true,
		}

		// Check if this is a well-known ConfigMap name
		if !wellKnownNames[name] {
			return false
		}

		// Global ConfigMaps: must be in controller namespace
		if namespace == systemNamespace {
			return true
		}

		// Single-namespace mode: watch all ConfigMaps in the watched namespace
		// Explicit CLI flag overrides tracking-based filtering
		if cfg != nil {
			watchNamespace := cfg.WatchNamespace()
			if watchNamespace != "" && namespace == watchNamespace {
				return true
			}
		}

		// Multi-namespace mode: only allow in tracked namespaces (namespaces with VAs)
		// This prevents cluster-wide watching and cache sync timeouts.
		// Opt-in labels and exclusion are still checked in the handler for accuracy.
		if ds != nil {
			return ds.IsNamespaceTracked(namespace)
		}

		// If no datastore provided, fall back to allowing all (backwards compatible)
		// This should not happen in production, but provides safety during setup.
		return true
	})
}

// ServiceMonitorPredicate returns a predicate that filters ServiceMonitor events to only the target ServiceMonitor.
// It checks that the ServiceMonitor name matches serviceMonitorName and namespace matches the configured namespace.
// This predicate is used to filter only the target ServiceMonitor.
// The ServiceMonitor is watched to enable detection when it is deleted, which would prevent
// Prometheus from scraping controller metrics (including optimized replicas).
func ServiceMonitorPredicate() predicate.Predicate {
	const defaultServiceMonitorName = "workload-variant-autoscaler-controller-manager-metrics-monitor"
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetName() == defaultServiceMonitorName && obj.GetNamespace() == config.SystemNamespace()
	})
}

// EventFilter returns a predicate.Funcs that filters events for the VariantAutoscaling controller.
// It allows:
//   - All Create events
//   - Update events for ConfigMap (needed to trigger reconcile on config changes)
//   - Update events for ServiceMonitor when deletionTimestamp is set (finalizers cause deletion to emit Update events)
//   - Delete events for ServiceMonitor (for immediate deletion detection)
//
// It blocks:
//   - Update events for VariantAutoscaling resource (controller reconciles periodically, so individual updates are unnecessary)
//   - Delete events for VariantAutoscaling resource (controller reconciles periodically and filters out deleted resources)
//   - Generic events
func EventFilter() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			gvk := e.ObjectNew.GetObjectKind().GroupVersionKind()
			// Allow Update events for ConfigMap (needed to trigger reconcile on config changes)
			if gvk.Kind == "ConfigMap" && gvk.Group == "" {
				return true
			}
			// Allow Update events for ServiceMonitor when deletionTimestamp is set
			// (finalizers cause deletion to emit Update events with deletionTimestamp)
			if gvk.Group == serviceMonitorGVK.Group && gvk.Kind == serviceMonitorGVK.Kind {
				// Check if deletionTimestamp was just set (deletion started)
				if deletionTimestamp := e.ObjectNew.GetDeletionTimestamp(); deletionTimestamp != nil && !deletionTimestamp.IsZero() {
					// Check if this is a newly set deletion timestamp
					oldDeletionTimestamp := e.ObjectOld.GetDeletionTimestamp()
					if oldDeletionTimestamp == nil || oldDeletionTimestamp.IsZero() {
						return true // Deletion just started
					}
				}
			}
			// Block Update events for VariantAutoscaling resource.
			// The controller reconciles all VariantAutoscaling resources periodically (every 60s by default),
			// so individual resource update events would only cause unnecessary reconciles without benefit.
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			gvk := e.Object.GetObjectKind().GroupVersionKind()
			// Allow Delete events for ServiceMonitor (for immediate deletion detection)
			if gvk.Group == serviceMonitorGVK.Group && gvk.Kind == serviceMonitorGVK.Kind {
				return true
			}
			// Block Delete events for VariantAutoscaling resource.
			// The controller reconciles all VariantAutoscaling resources periodically and filters out
			// deleted resources in filterActiveVariantAutoscalings, so individual delete events
			// would only cause unnecessary reconciles without benefit.
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// ScaleTargetPredicate returns a predicate that filters Deployment/LeaderWorkerSet events.
// It allows Create and Delete events for all Deployments/LeaderWorkerSet to trigger VA reconciliation:
// - Create: handles the race condition where VA is created before its target deployment/leaderWorkerSet
// - Delete: allows VA to update status and clear metrics when target deployment/leaderWorkerSet is removed
func ScaleTargetPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			// Allow all Deployment/LeaderWorkerSet create events to trigger reconciliation
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Allow all Deployment/LeaderWorkerSet delete events to trigger reconciliation
			// so VAs can update their status when target deployment/leaderWorkerSet is removed
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// VariantAutoscalingPredicate returns a predicate that filters VariantAutoscaling events
// based on the controller instance label and namespace exclusion annotation.
// This enables multi-controller isolation and namespace exclusion.
//
// Filtering behavior:
//   - Single-namespace mode (--watch-namespace set): Exclusion annotation is ignored for the watched namespace
//   - Multi-namespace mode: VAs in namespaces with wva.llmd.ai/exclude: "true" annotation are filtered out
//   - Controller instance: If CONTROLLER_INSTANCE env var is set, only allow VAs with matching wva.llmd.ai/controller-instance label
//   - If CONTROLLER_INSTANCE env var is not set: allow all VAs (backwards compatible)
//
// This predicate should be used with the VA watch to ensure controllers only reconcile
// their assigned VAs, preventing conflicts when multiple controllers run simultaneously.
//
// The client parameter is used to fetch namespace objects to check for exclusion annotations.
// The cfg parameter is used to check if the controller is in single-namespace mode.
func VariantAutoscalingPredicate(k8sClient client.Client, cfg *config.Config) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		namespace := obj.GetNamespace()

		// In single-namespace mode, skip exclusion check for the watched namespace
		// Explicit CLI flag overrides annotation-based filtering
		if cfg != nil {
			watchNamespace := cfg.WatchNamespace()
			if watchNamespace != "" && namespace == watchNamespace {
				// Still apply controller instance filtering, but skip exclusion check
				// This allows multiple controllers to share a namespace via controller-instance labels
				controllerInstance := metrics.GetControllerInstance()
				if controllerInstance == "" {
					return true
				}

				labels := obj.GetLabels()
				if labels == nil {
					return false
				}

				vaInstance, hasLabel := labels[constants.ControllerInstanceLabelKey]
				return hasLabel && vaInstance == controllerInstance
			}
		}

		// Multi-namespace mode: Check namespace exclusion first (highest priority)
		if namespace != "" {
			var ns corev1.Namespace
			// Use background context for predicate (no cancellation needed)
			if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: namespace}, &ns); err == nil {
				annotations := ns.GetAnnotations()
				if annotations != nil {
					if value, exists := annotations[constants.NamespaceExcludeAnnotationKey]; exists && value == constants.AnnotationValueTrue {
						// Namespace is excluded - filter out this VA
						return false
					}
				}
			}
			// If namespace fetch fails, proceed with other checks (fail open)
		}

		// Check controller instance label
		controllerInstance := metrics.GetControllerInstance()

		// If no controller instance configured, allow all VAs (backwards compatible)
		if controllerInstance == "" {
			return true
		}

		// Only allow VAs with matching controller-instance label
		labels := obj.GetLabels()
		if labels == nil {
			return false
		}

		vaInstance, hasLabel := labels[constants.ControllerInstanceLabelKey]
		return hasLabel && vaInstance == controllerInstance
	})
}
