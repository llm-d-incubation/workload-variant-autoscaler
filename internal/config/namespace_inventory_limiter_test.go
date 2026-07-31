package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("inline namespace-inventory limiter", func() {
	nsEntry := func(mutate func(*QuotaLimiterConfig)) SaturationScalingConfig {
		e := QuotaLimiterConfig{
			Type: string(LimiterTypeNamespaceInventory),
			Name: "namespace-inventory",
			Selectors: map[string]metav1.LabelSelector{
				"ns-prod": {MatchLabels: map[string]string{"team": "prod"}},
			},
		}
		if mutate != nil {
			mutate(&e)
		}
		return SaturationScalingConfig{Limiters: []QuotaLimiterConfig{e}}
	}

	Describe("validation", func() {
		It("accepts an entry carrying selectors and exclude", func() {
			c := nsEntry(func(e *QuotaLimiterConfig) { e.Exclude = []string{"kube-system"} })
			Expect(c.validateLimiters()).To(Succeed())
		})

		It("rejects an entry with no selectors", func() {
			c := nsEntry(func(e *QuotaLimiterConfig) { e.Selectors = nil })
			Expect(c.validateLimiters()).To(MatchError(ContainSubstring("requires at least one entry in selectors")))
		})

		It("rejects an invalid selector operator, naming the namespace", func() {
			c := nsEntry(func(e *QuotaLimiterConfig) {
				e.Selectors = map[string]metav1.LabelSelector{
					"ns-prod": {MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key: "team", Operator: "NotAnOperator", Values: []string{"prod"},
					}}},
				}
			})
			Expect(c.validateLimiters()).To(MatchError(ContainSubstring(`invalid node selector for namespace "ns-prod"`)))
		})

		It("rejects quota fields on a namespace-inventory entry", func() {
			c := nsEntry(func(e *QuotaLimiterConfig) { e.ClusterQuotas = map[string]int{"H100": 8} })
			Expect(c.validateLimiters()).To(MatchError(ContainSubstring("must not set quota fields")))
		})
	})

	Describe("mode selection", func() {
		It("selects namespace-inventory when declared", func() {
			cfg := NewTestConfig()
			cfg.UpdateSaturationConfig(map[string]SaturationScalingConfig{"default": nsEntry(nil)})

			Expect(cfg.EffectiveLimiterMode()).To(Equal(LimiterTypeNamespaceInventory))
			entry, ok := cfg.EffectiveNamespaceInventoryEntry()
			Expect(ok).To(BeTrue())
			Expect(entry.Selectors).To(HaveKey("ns-prod"))
		})

		It("lets a quota entry win, leaving composition to the limiter chain", func() {
			c := nsEntry(nil)
			c.Limiters = append(c.Limiters, QuotaLimiterConfig{
				Type: string(LimiterTypeQuota), Name: "cluster",
				Scope: QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8},
			})
			cfg := NewTestConfig()
			cfg.UpdateSaturationConfig(map[string]SaturationScalingConfig{"default": c})

			Expect(cfg.EffectiveLimiterMode()).To(Equal(LimiterTypeQuota))
		})

		It("defaults to cluster-wide inventory when nothing is declared", func() {
			cfg := NewTestConfig()
			Expect(cfg.EffectiveLimiterMode()).To(Equal(LimiterTypeInventory))
			_, ok := cfg.EffectiveNamespaceInventoryEntry()
			Expect(ok).To(BeFalse())
		})

		It("deep-copies selectors so callers cannot mutate stored config", func() {
			cfg := NewTestConfig()
			cfg.UpdateSaturationConfig(map[string]SaturationScalingConfig{"default": nsEntry(nil)})

			entry, _ := cfg.EffectiveNamespaceInventoryEntry()
			delete(entry.Selectors, "ns-prod")

			again, _ := cfg.EffectiveNamespaceInventoryEntry()
			Expect(again.Selectors).To(HaveKey("ns-prod"))
		})
	})
})
