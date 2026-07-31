package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// A malformed selector must be reported at startup rather than per cycle, and
// NewEngine turns that error into "log and disable" so a bad ConfigMap cannot
// block the controller from starting.
var _ = Describe("buildNamespaceLimiter", func() {
	namespaceInventory := func(selectors map[string]metav1.LabelSelector) config.LimiterConfig {
		return config.LimiterConfig{Limiters: []config.LimiterSpec{{
			Name:      "namespace-inventory",
			Type:      config.LimiterTypeNamespaceInventory,
			Selectors: selectors,
		}}}
	}

	It("returns no limiter when namespace inventory is not configured", func() {
		lim, err := buildNamespaceLimiter(config.LimiterConfig{}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(lim).To(BeNil())
	})

	It("wraps an invalid selector operator in an error naming the namespace", func() {
		lc := namespaceInventory(map[string]metav1.LabelSelector{
			"ns-prod": {MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "team",
				Operator: "NotAnOperator",
				Values:   []string{"prod"},
			}}},
		})

		lim, err := buildNamespaceLimiter(lc, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`invalid node selector for namespace "ns-prod"`))
		Expect(lim).To(BeNil(), "a bad selector yields no limiter, so the engine disables it")
	})

	It("builds a limiter when every selector compiles", func() {
		lc := namespaceInventory(map[string]metav1.LabelSelector{
			"ns-prod": {MatchLabels: map[string]string{"team": "prod"}},
		})

		lim, err := buildNamespaceLimiter(lc, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(lim).NotTo(BeNil())
	})
})
