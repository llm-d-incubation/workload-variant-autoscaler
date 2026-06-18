package gpurebalance

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/go-logr/logr"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AnnotationInferencePool = "llm-d.ai/epp-inference-pool"
	gpuQuotaResource        = "requests.nvidia.com/gpu"
	eppQueueMetric          = `sum(inference_extension_flow_control_queue_size{inference_pool=%q})`
)

// Plugin implements the Coordinator Plugin interface for GPU rebalancing.
type Plugin struct {
	client  client.Client
	promAPI promv1.API
}

// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;patch;update

// New constructs the gpu-rebalance Plugin.
func New(c client.Client, promAPI promv1.API) *Plugin {
	return &Plugin{client: c, promAPI: promAPI}
}

// Name returns the unique plugin identifier.
func (p *Plugin) Name() string { return "gpu-rebalance" }

// hpaEntry pairs an HPA with its inference-pool name.
type hpaEntry struct {
	hpa  *autoscalingv2.HorizontalPodAutoscaler
	pool string
}

// Tick splits the GPU quota proportionally across managed HPAs based on each
// pool's EPP flow-control queue depth. HPAs are grouped by namespace so each
// namespace is rebalanced independently against its own ResourceQuota. When
// all queues in a namespace are zero the quota is divided equally. MaxReplicas
// is patched only when the computed target differs from the current value.
func (p *Plugin) Tick(ctx context.Context, selected []client.Object) error {
	log := ctrl.LoggerFrom(ctx).WithName("gpu-rebalance")

	// Group annotated HPAs by namespace.
	byNamespace := make(map[string][]hpaEntry)
	for _, obj := range selected {
		hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
		if !ok {
			continue
		}
		pool := hpa.Annotations[AnnotationInferencePool]
		if pool == "" {
			continue
		}
		ns := hpa.Namespace
		byNamespace[ns] = append(byNamespace[ns], hpaEntry{hpa: hpa, pool: pool})
	}
	if len(byNamespace) == 0 {
		return nil
	}

	for ns, entries := range byNamespace {
		if err := p.rebalanceNamespace(ctx, log, ns, entries); err != nil {
			return err
		}
	}
	return nil
}

// rebalanceNamespace applies proportional GPU quota allocation to all managed
// HPAs in a single namespace.
func (p *Plugin) rebalanceNamespace(ctx context.Context, log logr.Logger, ns string, entries []hpaEntry) error {
	quota, err := p.namespaceGPUQuota(ctx, ns)
	if err != nil {
		return fmt.Errorf("reading GPU quota in namespace %s: %w", ns, err)
	}
	if quota <= 0 {
		log.V(1).Info("No GPU quota set, skipping", "namespace", ns)
		return nil
	}

	// Query EPP queue depth per pool (best-effort; default to 0 on error).
	queues := make([]float64, len(entries))
	for i, e := range entries {
		q, err := p.queryQueue(ctx, e.pool)
		if err != nil {
			log.V(1).Info("Queue query failed, using 0", "pool", e.pool, "err", err)
		} else {
			queues[i] = q
		}
	}

	// Weights: proportional to queue depth, equal when all queues are zero.
	totalQueue := 0.0
	for _, q := range queues {
		totalQueue += q
	}
	weights := make([]float64, len(entries))
	if totalQueue == 0 {
		for i := range weights {
			weights[i] = 1.0 / float64(len(entries))
		}
	} else {
		for i, q := range queues {
			weights[i] = q / totalQueue
		}
	}

	// TODO: the minimum target is hardcoded to 1 but an HPA may have spec.minReplicas
	// set higher (e.g. 2 or 3). Setting maxReplicas below minReplicas produces an
	// invalid HPA and Kubernetes will reject the patch. The floor should be
	// max(1, hpa.Spec.MinReplicas) so the computed target is always a valid value.

	// TODO: when len(entries) > quota the minimum-1 clamp causes every HPA to
	// receive maxReplicas=1 and allocated exceeds quota (e.g. 100 HPAs, quota=10
	// → allocated=100). The ResourceQuota becomes the only enforcement and the
	// first 10 pods to be scheduled win while the other 90 are stuck pending.
	// Fix: rank HPAs by queue depth, assign maxReplicas=1 only to the top-quota
	// pools, and park the rest at minReplicas so the budget is not over-committed.

	// TODO: scale-to-zero is not supported. When a pool's queue is 0 its weight is
	// 0% and the ideal target is 0 replicas, but the floor clamp below forces it to 1.
	// Supporting scale-to-zero requires: (a) the HPA has spec.minReplicas=0 (opt-in),
	// and (b) the Coordinator avoids setting maxReplicas=0 while in-flight requests are
	// still being drained (check active connection count or use a brief drain window
	// before zeroing). Until those conditions are met the floor is kept at 1 to prevent
	// accidental full scale-down.

	// Targets: floor(quota * weight), minimum 1, remainder to highest-weight pool.
	targets := make([]int32, len(entries))
	allocated := int64(0)
	maxWeightIdx := 0
	for i := range entries {
		t := int32(math.Floor(float64(quota) * weights[i]))
		if t < 1 {
			t = 1
		}
		targets[i] = t
		allocated += int64(t)
		if weights[i] > weights[maxWeightIdx] {
			maxWeightIdx = i
		}
	}
	if rem := quota - allocated; rem > 0 {
		targets[maxWeightIdx] += int32(rem)
	}

	// TODO: the current patch-on-every-tick approach causes wobble. Instantaneous
	// queue depth is noisy, so small fluctuations (e.g. q=270 vs q=286) move the
	// floor/ceil boundary each cycle, producing 1-replica flips every 15s. Each
	// flip triggers pod churn which perturbs the queue and drives the next flip.
	// Fix with two complementary guards:
	//   1. Minimum delta: skip the patch when abs(new - current) < N replicas so
	//      noise-driven micro-adjustments are ignored.
	//   2. Per-HPA cooldown: after patching an HPA, skip it for the next K ticks
	//      so the HPA and its pods have time to converge before the next reading.
	// An alternative to (2) is EWMA smoothing of the queue metric across ticks,
	// which dampens burst noise without adding a hard cooldown delay.

	for i, e := range entries {
		if targets[i] == e.hpa.Spec.MaxReplicas {
			continue
		}
		log.Info("Setting maxReplicas",
			"hpa", e.hpa.Name, "namespace", ns,
			"pool", e.pool,
			"from", e.hpa.Spec.MaxReplicas, "to", targets[i],
			"queue", queues[i], "quota", quota,
		)
		if err := p.setMaxReplicas(ctx, e.hpa, targets[i]); err != nil {
			return fmt.Errorf("patching %s: %w", e.hpa.Name, err)
		}
	}
	return nil
}

func (p *Plugin) namespaceGPUQuota(ctx context.Context, ns string) (int64, error) {
	// TODO: a namespace can have multiple ResourceQuotas; the effective GPU limit
	// is the minimum across all of them, not the first one found. Returning the
	// first match can overestimate the budget and allow over-scheduling.

	// TODO: ResourceQuotas can carry a spec.scopeSelector that restricts which
	// pods they apply to (e.g. only BestEffort or Terminating pods). This code
	// ignores scope selectors and treats any quota with a GPU field as the full
	// namespace budget, which may overstate the applicable limit.

	list := &corev1.ResourceQuotaList{}
	if err := p.client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	for i := range list.Items {
		if q, ok := list.Items[i].Spec.Hard[corev1.ResourceName(gpuQuotaResource)]; ok {
			return q.Value(), nil
		}
	}
	return 0, nil
}

func (p *Plugin) queryQueue(ctx context.Context, inferencePool string) (float64, error) {
	// TODO: the query filters only by inference_pool name with no namespace label.
	// If two namespaces each have a pool with the same name (e.g. "model-a"), the
	// sum includes both, inflating the queue reading for each namespace's allocation
	// decision. Fix: include a namespace label in the query if the EPP emits one,
	// or scope the metric series by passing the namespace as an additional matcher.

	query := fmt.Sprintf(eppQueueMetric, inferencePool)
	result, _, err := p.promAPI.Query(ctx, query, time.Now())
	if err != nil {
		return 0, err
	}
	vec, ok := result.(model.Vector)
	if !ok || len(vec) == 0 {
		return 0, nil
	}
	return float64(vec[0].Value), nil
}

func (p *Plugin) setMaxReplicas(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, target int32) error {
	// TODO: MergeFrom uses the object captured at List time. If another controller
	// or user updated maxReplicas between the List and this Patch, the write will
	// silently overwrite that change. Use MergeFromWithOptimisticLock (which sets
	// resourceVersion on the patch) so the API server rejects a stale write with
	// a 409 Conflict, allowing the Coordinator to re-read and retry cleanly.

	original := hpa.DeepCopy()
	hpa.Spec.MaxReplicas = target
	return p.client.Patch(ctx, hpa, client.MergeFrom(original))
}
