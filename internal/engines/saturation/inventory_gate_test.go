package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

var _ = Describe("shouldCollectClusterInventory", func() {

	DescribeTable("limiter type determines whether cluster inventory is collected",
		func(limiterType config.LimiterType, want bool) {
			cfg := config.NewTestConfig()
			config.SetLimiterForTest(cfg, limiterType, "")
			Expect(shouldCollectClusterInventory(cfg)).To(Equal(want))
		},
		Entry("inventory mode allows inventory collection", config.LimiterTypeInventory, true),
		Entry("quota mode blocks inventory collection", config.LimiterTypeQuota, false),
		Entry("empty limiter type is treated as non-inventory", config.LimiterType(""), false),
		Entry("unknown limiter type is treated as non-inventory", config.LimiterType("bogus"), false),
	)
})
