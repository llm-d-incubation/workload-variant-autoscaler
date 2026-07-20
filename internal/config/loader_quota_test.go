package config

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("loadQuotaLimiterEntries (via ReloadQuotaForTest)", func() {
	var cfg *Config

	BeforeEach(func() {
		cfg = NewTestConfig()
	})

	// writeQuotaFile writes content to a temp file and points the Config's
	// quota limiter at it (limiter-type=quota), so ReloadQuotaForTest loads it.
	writeQuotaFile := func(content string) string {
		path := filepath.Join(GinkgoT().TempDir(), "quota.yaml")
		Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
		return path
	}

	It("errors when the quota config file does not exist", func() {
		SetLimiterForTest(cfg, LimiterTypeQuota, filepath.Join(GinkgoT().TempDir(), "missing.yaml"))
		Expect(ReloadQuotaForTest(cfg)).To(MatchError(ContainSubstring("read")))
	})

	It("errors when the quota config file is not valid YAML", func() {
		SetLimiterForTest(cfg, LimiterTypeQuota, writeQuotaFile("limiters: [unterminated"))
		Expect(ReloadQuotaForTest(cfg)).To(MatchError(ContainSubstring("parse")))
	})

	It("errors when the parsed entries fail validation", func() {
		SetLimiterForTest(cfg, LimiterTypeQuota, writeQuotaFile(
			"limiters:\n  - name: q\n    type: quota\n    scope: bogus\n"))
		Expect(ReloadQuotaForTest(cfg)).To(MatchError(ContainSubstring("validate")))
	})

	It("loads a valid quota config and populates QuotaEntries", func() {
		SetLimiterForTest(cfg, LimiterTypeQuota, writeQuotaFile(
			"limiters:\n  - name: q\n    type: quota\n    scope: cluster\n    quotas:\n      A100: 4\n"))
		Expect(ReloadQuotaForTest(cfg)).To(Succeed())
		entries := cfg.QuotaEntries()
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].Name).To(Equal("q"))
		Expect(entries[0].Scope).To(Equal(QuotaScopeCluster))
		Expect(entries[0].ClusterQuotas).To(HaveKeyWithValue("A100", 4))
	})

	It("is a no-op when limiter-type is not quota", func() {
		SetLimiterForTest(cfg, LimiterTypeInventory, writeQuotaFile("limiters: [unterminated"))
		Expect(ReloadQuotaForTest(cfg)).To(Succeed(), "non-quota mode must not read/parse the file")
	})
})
