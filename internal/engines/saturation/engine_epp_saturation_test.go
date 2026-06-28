package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
)

// drainDecisionTrigger empties the global DecisionTrigger channel so assertions
// about how many events were fired are not affected by prior tests.
func drainDecisionTrigger() {
	for {
		select {
		case <-common.DecisionTrigger:
		default:
			return
		}
	}
}

// TestMarkEPPSignalUnavailable verifies that the EPP signal-unavailable fallback
// pushes a MetricsAvailable=False decision (with an EPP-specific reason) into the
// shared cache for real VAs, skips synthetic VAs, and triggers a reconcile.
func TestMarkEPPSignalUnavailable(t *testing.T) {
	drainDecisionTrigger()

	realVA := llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "epp-real", Namespace: "ns1"},
	}
	syntheticVA := llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "epp-synthetic",
			Namespace:   "ns1",
			Annotations: map[string]string{annotations.Synthetic: "true"},
		},
	}

	e := &Engine{}
	e.markEPPSignalUnavailable([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{realVA, syntheticVA})

	// Real VA gets a MetricsAvailable=False cache entry with the EPP-specific reason.
	d, ok := common.DecisionCache.Get("epp-real", "ns1")
	require.True(t, ok, "expected a cache entry for the real VA")
	assert.False(t, d.MetricsAvailable)
	assert.Equal(t, llmdVariantAutoscalingV1alpha1.ReasonPrometheusError, d.MetricsReason)
	assert.Equal(t, eppSignalUnavailableMessage, d.MetricsMessage)

	// Synthetic VA is skipped (never written to the cache).
	_, ok = common.DecisionCache.Get("epp-synthetic", "ns1")
	assert.False(t, ok, "synthetic VA should not be cached")

	// Exactly one reconcile event is fired (for the real VA only).
	assert.Len(t, common.DecisionTrigger, 1)
}
