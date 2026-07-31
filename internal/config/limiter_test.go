package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseLimiterConfig_NamespaceInventory(t *testing.T) {
	const cm = `
limiters:
  - name: ns-inv
    type: namespace-inventory
    exclude:
      - kube-system
      - llm-d-system
    selectors:
      team-a:
        matchLabels:
          team: a
      team-b:
        matchExpressions:
          - key: team
            operator: In
            values: [b, b-canary]
      default:
        matchLabels:
          pool: shared
`
	cfg, err := ParseLimiterConfig(map[string]string{GlobalDefaultsKey: cm})
	require.NoError(t, err)

	spec, ok := cfg.NamespaceInventorySpec()
	require.True(t, ok, "namespace-inventory limiter should be found")
	assert.Equal(t, LimiterTypeNamespaceInventory, spec.Type)
	assert.ElementsMatch(t, []string{"kube-system", "llm-d-system"}, spec.Exclude)

	require.Contains(t, spec.Selectors, "team-a")
	assert.Equal(t, map[string]string{"team": "a"}, spec.Selectors["team-a"].MatchLabels)

	require.Contains(t, spec.Selectors, "team-b")
	require.Len(t, spec.Selectors["team-b"].MatchExpressions, 1)
	expr := spec.Selectors["team-b"].MatchExpressions[0]
	assert.Equal(t, "team", expr.Key)
	assert.Equal(t, metav1.LabelSelectorOpIn, expr.Operator)
	assert.Equal(t, []string{"b", "b-canary"}, expr.Values)

	require.Contains(t, spec.Selectors, "default")
	assert.Equal(t, map[string]string{"pool": "shared"}, spec.Selectors["default"].MatchLabels)
}

func TestParseLimiterConfig_EmptyAndMissing(t *testing.T) {
	// No data at all.
	cfg, err := ParseLimiterConfig(nil)
	require.NoError(t, err)
	_, ok := cfg.NamespaceInventorySpec()
	assert.False(t, ok)

	// Data present but no "default" entry.
	cfg, err = ParseLimiterConfig(map[string]string{"other": "limiters: []"})
	require.NoError(t, err)
	assert.Empty(t, cfg.Limiters)
}

func TestParseLimiterConfig_InvalidYAML(t *testing.T) {
	_, err := ParseLimiterConfig(map[string]string{GlobalDefaultsKey: "limiters: [::::"})
	assert.Error(t, err)
}
