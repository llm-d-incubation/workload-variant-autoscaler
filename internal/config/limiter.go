package config

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Limiter type identifiers for LimiterSpec.Type.
const (
	// LimiterTypeNamespaceInventory scopes GPU inventory to per-namespace node
	// pools selected by node label selectors.
	LimiterTypeNamespaceInventory = "namespace-inventory"
)

// LimiterConfig is the parsed wva-limiter-config ConfigMap. It is read once at
// startup; changing it requires a controller restart.
type LimiterConfig struct {
	// Limiters is the ordered list of configured limiters.
	Limiters []LimiterSpec `json:"limiters,omitempty"`
}

// LimiterSpec configures a single limiter.
type LimiterSpec struct {
	// Name is an operator-chosen identifier for logging.
	Name string `json:"name,omitempty"`
	// Type selects the limiter implementation (e.g. namespace-inventory).
	Type string `json:"type"`
	// Exclude lists namespaces that bypass this limiter entirely.
	Exclude []string `json:"exclude,omitempty"`
	// Selectors maps a namespace (or the reserved "default" key) to the node
	// label selector whose matching nodes form that namespace's GPU pool.
	Selectors map[string]metav1.LabelSelector `json:"selectors,omitempty"`
}

// NamespaceInventorySpec returns the first namespace-inventory limiter spec and
// whether one is configured.
func (c LimiterConfig) NamespaceInventorySpec() (LimiterSpec, bool) {
	for _, l := range c.Limiters {
		if l.Type == LimiterTypeNamespaceInventory {
			return l, true
		}
	}
	return LimiterSpec{}, false
}

// ParseLimiterConfig parses the limiter ConfigMap data. The configuration is
// read from the GlobalDefaultsKey ("default") entry, consistent with the other
// global ConfigMaps. An absent entry yields an empty (no-op) config.
func ParseLimiterConfig(data map[string]string) (LimiterConfig, error) {
	raw, ok := data[GlobalDefaultsKey]
	if !ok || raw == "" {
		return LimiterConfig{}, nil
	}
	var cfg LimiterConfig
	// sigs.k8s.io/yaml honors the json tags on metav1.LabelSelector
	// (matchLabels / matchExpressions), so selectors parse correctly.
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return LimiterConfig{}, fmt.Errorf("parsing limiter config: %w", err)
	}
	return cfg, nil
}
