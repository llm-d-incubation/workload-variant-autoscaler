package pipeline

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// writeQuotaConfigFile writes a YAML quota config to a temp file and returns
// the path. The file is cleaned up automatically when the test completes.
func writeQuotaConfigFile(content string) string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	path := filepath.Join(dir, "quota.yaml")
	Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
	return path
}

// loadTestConfig builds a Config populated with the minimum prometheus
// settings (so Validate succeeds) plus the supplied limiter selection.
func loadTestConfig(limiterType config.LimiterType, quotaConfigFile string) *config.Config {
	GinkgoHelper()
	// Use NewTestConfig as the base, then mutate the limiter selection
	// through the same loader path that production uses.
	cfg := config.NewTestConfig()
	// Mutate via a focused helper to keep the field private to the package.
	config.SetLimiterForTest(cfg, limiterType, quotaConfigFile)
	return cfg
}

var _ = Describe("NewLimiterFromConfig", func() {

	It("returns an inventory limiter by default", func() {
		cfg := loadTestConfig(config.LimiterTypeInventory, "")
		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).NotTo(BeNil())
		Expect(l.Name()).To(Equal("gpu-limiter"))
		// The inventory limiter is a DefaultLimiter wrapping TypeInventory.
		_, ok := l.(*DefaultLimiter)
		Expect(ok).To(BeTrue(), "inventory mode should produce a DefaultLimiter")
	})

	It("rejects unknown limiter types", func() {
		cfg := loadTestConfig(config.LimiterType("bogus"), "")
		_, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`unknown limiter type "bogus"`))
	})

	It("returns a single DefaultLimiter when quota config has one entry", func() {
		path := writeQuotaConfigFile(`limiters:
  - name: cluster-quota
    type: quota
    scope: cluster
    quotas:
      H100: 16
`)
		cfg := loadTestConfig(config.LimiterTypeQuota, path)
		// Re-run the loader against the file so Config carries the entries.
		Expect(config.ReloadQuotaForTest(cfg)).To(Succeed())

		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(l).NotTo(BeNil())
		Expect(l.Name()).To(Equal("cluster-quota"))
		_, ok := l.(*DefaultLimiter)
		Expect(ok).To(BeTrue(), "single-entry quota should produce a DefaultLimiter")
	})

	It("wraps multiple quota entries in a CompositeLimiter", func() {
		path := writeQuotaConfigFile(`limiters:
  - name: cluster-quota
    type: quota
    scope: cluster
    quotas:
      H100: 16
  - name: namespace-quota
    type: quota
    scope: namespace
    namespaceQuotas:
      team-a:
        H100: 8
`)
		cfg := loadTestConfig(config.LimiterTypeQuota, path)
		Expect(config.ReloadQuotaForTest(cfg)).To(Succeed())

		l, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).NotTo(HaveOccurred())

		comp, ok := l.(*CompositeLimiter)
		Expect(ok).To(BeTrue(), "multi-entry quota should produce a CompositeLimiter")
		Expect(comp.Name()).To(Equal("quota-limiter"))
		Expect(comp.Constituents()).To(HaveLen(2))
		Expect(comp.Constituents()[0].Name()).To(Equal("cluster-quota"))
		Expect(comp.Constituents()[1].Name()).To(Equal("namespace-quota"))
	})

	It("errors when limiter-type=quota but no entries are loaded", func() {
		cfg := loadTestConfig(config.LimiterTypeQuota, "")
		// No reload — quotaEntries is empty.
		_, err := NewLimiterFromConfig(cfg, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("at least one entry"))
	})
})
