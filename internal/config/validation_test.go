package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Validate limiter selection", func() {
	// validBaseConfig returns a Config that passes Validate's non-limiter
	// checks, so each spec exercises only the limiter-mode branch.
	validBaseConfig := func() *Config {
		cfg := NewTestConfig()
		cfg.setPrometheusBaseURLForTesting("http://prometheus:9090")
		return cfg
	}

	It("accepts limiter-type=inventory", func() {
		cfg := validBaseConfig()
		SetLimiterForTest(cfg, LimiterTypeInventory, "")
		Expect(Validate(cfg)).To(Succeed())
	})

	It("rejects limiter-type=quota without a quota config file", func() {
		cfg := validBaseConfig()
		SetLimiterForTest(cfg, LimiterTypeQuota, "")
		err := Validate(cfg)
		Expect(err).To(MatchError(ContainSubstring("requires --quota-config-file")))
	})

	It("rejects limiter-type=quota whose config parsed to an empty entries list", func() {
		cfg := validBaseConfig()
		// Non-empty path passes the first check; quotaEntries is left empty by
		// SetLimiterForTest, so the empty-entries branch fires.
		SetLimiterForTest(cfg, LimiterTypeQuota, "/some/quota.yaml")
		err := Validate(cfg)
		Expect(err).To(MatchError(ContainSubstring("empty entries list")))
	})

	It("rejects an unknown limiter type", func() {
		cfg := validBaseConfig()
		SetLimiterForTest(cfg, LimiterType("bogus"), "")
		err := Validate(cfg)
		Expect(err).To(MatchError(ContainSubstring("limiter-type must be")))
	})
})
