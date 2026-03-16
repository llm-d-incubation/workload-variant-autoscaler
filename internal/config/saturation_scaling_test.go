package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func float64Ptr(v float64) *float64 { return &v }

var _ = Describe("SaturationScalingConfig", func() {

	Context("Validate", func() {

		DescribeTable("validation cases",
			func(config SaturationScalingConfig, expectErr bool) {
				err := config.Validate()
				if expectErr {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("valid default config", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
			}, false),
			Entry("valid custom config", SaturationScalingConfig{
				KvCacheThreshold:     0.75,
				QueueLengthThreshold: 10,
				KvSpareTrigger:       0.15,
				QueueSpareTrigger:    5,
			}, false),
			Entry("invalid KvCacheThreshold too high", SaturationScalingConfig{
				KvCacheThreshold:     1.5,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid KvCacheThreshold negative", SaturationScalingConfig{
				KvCacheThreshold:     -0.1,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid QueueLengthThreshold negative", SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: -1,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid KvSpareTrigger too high", SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       1.5,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid KvSpareTrigger negative", SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       -0.1,
				QueueSpareTrigger:    3,
			}, true),
			Entry("invalid QueueSpareTrigger negative", SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    -1,
			}, true),
			Entry("invalid KvCacheThreshold less than KvSpareTrigger", SaturationScalingConfig{
				KvCacheThreshold:     0.5,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.6,
				QueueSpareTrigger:    3,
			}, true),
			Entry("edge case: zero values are valid", SaturationScalingConfig{
				KvCacheThreshold:     0.0,
				QueueLengthThreshold: 0,
				KvSpareTrigger:       0.0,
				QueueSpareTrigger:    0,
			}, false),
			Entry("edge case: max values are valid", SaturationScalingConfig{
				KvCacheThreshold:     1.0,
				QueueLengthThreshold: 1000,
				KvSpareTrigger:       1.0,
				QueueSpareTrigger:    1000,
			}, false),
			Entry("V2 valid config with explicit thresholds (old-style analyzerName)", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.90,
				ScaleDownBoundary:    0.60,
			}, false),
			Entry("V2 valid config with analyzers list (new-style)", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				ScaleUpThreshold:     0.90,
				ScaleDownBoundary:    0.60,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", Score: 1.0},
				},
			}, false),
			Entry("V2 invalid: scaleUpThreshold > 1", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     1.5,
				ScaleDownBoundary:    0.70,
			}, true),
			Entry("V2 invalid: scaleUpThreshold <= scaleDownBoundary", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.60,
				ScaleDownBoundary:    0.70,
			}, true),
			Entry("V2 thresholds ignored when not V2", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "",
				ScaleUpThreshold:     0,
				ScaleDownBoundary:    0,
			}, false),
			Entry("valid priority", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Priority:             5.0,
			}, false),
			Entry("invalid negative priority", SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Priority:             -1.0,
			}, true),
			Entry("V2 valid per-analyzer threshold override", SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleUpThreshold: float64Ptr(0.90)},
				},
			}, false),
			Entry("V2 invalid per-analyzer scaleUpThreshold > 1", SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleUpThreshold: float64Ptr(1.5)},
				},
			}, true),
			Entry("V2 invalid per-analyzer scaleDownBoundary > 1", SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleDownBoundary: float64Ptr(1.5)},
				},
			}, true),
			Entry("V2 invalid per-analyzer effective up <= down", SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleUpThreshold: float64Ptr(0.60)},
				},
			}, true),
		)
	})

	Context("ApplyDefaults", func() {

		It("should apply defaults for V2 via analyzerName (backward compat)", func() {
			config := SaturationScalingConfig{
				AnalyzerName: "saturation",
			}
			config.ApplyDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(DefaultScaleUpThreshold))
			Expect(config.ScaleDownBoundary).To(Equal(DefaultScaleDownBoundary))
			Expect(config.Analyzers).To(HaveLen(1))
		})

		It("should apply defaults for V2 via analyzers list (new-style)", func() {
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			config.ApplyDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(DefaultScaleUpThreshold))
			Expect(config.ScaleDownBoundary).To(Equal(DefaultScaleDownBoundary))
			Expect(config.Analyzers[0].Score).To(Equal(1.0))
			Expect(config.Analyzers[0].Enabled).NotTo(BeNil())
			Expect(*config.Analyzers[0].Enabled).To(BeTrue())
		})

		It("should not overwrite explicit values", func() {
			config := SaturationScalingConfig{
				AnalyzerName:      "saturation",
				ScaleUpThreshold:  0.90,
				ScaleDownBoundary: 0.60,
			}
			config.ApplyDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(0.90))
			Expect(config.ScaleDownBoundary).To(Equal(0.60))
		})

		It("should be no-op when not V2", func() {
			config := SaturationScalingConfig{
				AnalyzerName: "",
			}
			config.ApplyDefaults()
			Expect(config.ScaleUpThreshold).To(Equal(0.0))
			Expect(config.ScaleDownBoundary).To(Equal(0.0))
			Expect(config.Analyzers).To(BeEmpty())
		})

		It("should apply default priority when zero", func() {
			config := SaturationScalingConfig{}
			config.ApplyDefaults()
			Expect(config.Priority).To(Equal(DefaultPriority))
		})

		It("should not overwrite explicit priority", func() {
			config := SaturationScalingConfig{
				Priority: 5.0,
			}
			config.ApplyDefaults()
			Expect(config.Priority).To(Equal(5.0))
		})

		It("should not overwrite explicit analyzers", func() {
			disabled := false
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", Score: 0.5, Enabled: &disabled},
				},
			}
			config.ApplyDefaults()
			Expect(config.Analyzers[0].Score).To(Equal(0.5))
			Expect(*config.Analyzers[0].Enabled).To(BeFalse())
		})

		It("should apply per-entry defaults for zero score", func() {
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			config.ApplyDefaults()
			Expect(config.Analyzers[0].Score).To(Equal(1.0))
			Expect(config.Analyzers[0].Enabled).NotTo(BeNil())
			Expect(*config.Analyzers[0].Enabled).To(BeTrue())
		})

		It("should pass validation after ApplyDefaults with zero-valued omitempty fields", func() {
			config := SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			config.ApplyDefaults()
			Expect(config.Validate()).To(Succeed())
		})
	})

	Context("IsV2", func() {

		It("should return false when no analyzers and no analyzerName", func() {
			config := SaturationScalingConfig{}
			Expect(config.IsV2()).To(BeFalse())
		})

		It("should return true when analyzerName is saturation (backward compat)", func() {
			config := SaturationScalingConfig{AnalyzerName: "saturation"}
			Expect(config.IsV2()).To(BeTrue())
		})

		It("should return true when analyzers list is populated", func() {
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			Expect(config.IsV2()).To(BeTrue())
		})

		It("should return true when both analyzerName and analyzers set", func() {
			config := SaturationScalingConfig{
				AnalyzerName: "saturation",
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			Expect(config.IsV2()).To(BeTrue())
		})
	})

	Context("GetAnalyzerName", func() {

		It("should return saturation when analyzers list populated", func() {
			config := SaturationScalingConfig{
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation"},
				},
			}
			Expect(config.GetAnalyzerName()).To(Equal("saturation"))
		})

		It("should return raw analyzerName when no analyzers list", func() {
			config := SaturationScalingConfig{AnalyzerName: "saturation"}
			Expect(config.GetAnalyzerName()).To(Equal("saturation"))
		})

		It("should return empty when no analyzers and no analyzerName", func() {
			config := SaturationScalingConfig{}
			Expect(config.GetAnalyzerName()).To(BeEmpty())
		})
	})
})

var _ = Describe("AnalyzerScoreConfig", func() {

	Context("EffectiveScaleUpThreshold", func() {

		It("should return global when per-analyzer not set", func() {
			a := AnalyzerScoreConfig{Name: "saturation"}
			Expect(a.EffectiveScaleUpThreshold(0.85)).To(Equal(0.85))
		})

		It("should return per-analyzer when set", func() {
			a := AnalyzerScoreConfig{
				Name:             "saturation",
				ScaleUpThreshold: float64Ptr(0.90),
			}
			Expect(a.EffectiveScaleUpThreshold(0.85)).To(Equal(0.90))
		})
	})

	Context("EffectiveScaleDownBoundary", func() {

		It("should return global when per-analyzer not set", func() {
			a := AnalyzerScoreConfig{Name: "saturation"}
			Expect(a.EffectiveScaleDownBoundary(0.70)).To(Equal(0.70))
		})

		It("should return per-analyzer when set", func() {
			a := AnalyzerScoreConfig{
				Name:              "saturation",
				ScaleDownBoundary: float64Ptr(0.60),
			}
			Expect(a.EffectiveScaleDownBoundary(0.70)).To(Equal(0.60))
		})
	})

	It("should support partial override (only scaleUpThreshold)", func() {
		a := AnalyzerScoreConfig{
			Name:             "saturation",
			ScaleUpThreshold: float64Ptr(0.95),
		}
		Expect(a.EffectiveScaleUpThreshold(0.85)).To(Equal(0.95))
		Expect(a.EffectiveScaleDownBoundary(0.70)).To(Equal(0.70))
	})
})
