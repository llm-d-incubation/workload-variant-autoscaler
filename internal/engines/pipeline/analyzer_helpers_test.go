package pipeline

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// makeNamed builds a NamedAnalyzerResult with the given RC, SC, and per-variant
// (variantName, perReplicaCapacity) pairs.
func makeNamed(name string, rc, sc float64, vcs ...any) NamedAnalyzerResult {
	var caps []interfaces.VariantCapacity
	for i := 0; i+1 < len(vcs); i += 2 {
		vName := vcs[i].(string)
		prc := vcs[i+1].(float64)
		caps = append(caps, interfaces.VariantCapacity{
			VariantName:        vName,
			PerReplicaCapacity: prc,
		})
	}
	return NamedAnalyzerResult{
		Name: name,
		Result: &interfaces.AnalyzerResult{
			RequiredCapacity:  rc,
			SpareCapacity:     sc,
			VariantCapacities: caps,
		},
		Remaining: rc,
		Spare:     sc,
	}
}

var _ = Describe("analyzer helpers", func() {

	Describe("applyAllocation", func() {
		It("subtracts n×PRC from each analyzer's Remaining counter", func() {
			// PRC=100, n=2 → subtract 200 from each Remaining
			s := []NamedAnalyzerResult{
				makeNamed("sat", 500, 0, "v", 100.0),
				makeNamed("ta", 300, 0, "v", 100.0),
			}
			applyAllocation(s, "v", 2)
			Expect(s[0].Remaining).To(BeNumerically("~", 300.0, 1e-9))
			Expect(s[1].Remaining).To(BeNumerically("~", 100.0, 1e-9))
			// Result.RequiredCapacity is not mutated
			Expect(s[0].Result.RequiredCapacity).To(Equal(500.0))
		})

		It("clamps Remaining to 0", func() {
			s := []NamedAnalyzerResult{makeNamed("sat", 50, 0, "v", 100.0)}
			applyAllocation(s, "v", 2) // would subtract 200 from 50
			Expect(s[0].Remaining).To(Equal(0.0))
		})

		It("is a no-op for variants not in the result", func() {
			s := []NamedAnalyzerResult{makeNamed("sat", 200, 0, "other", 100.0)}
			applyAllocation(s, "v", 3)
			Expect(s[0].Remaining).To(Equal(200.0))
		})
	})

	Describe("saturationEntry", func() {
		It("returns the saturation result from the slice", func() {
			satResult := &interfaces.AnalyzerResult{RequiredCapacity: 42}
			s := []NamedAnalyzerResult{
				{Name: interfaces.SaturationAnalyzerName, Result: satResult},
				makeNamed("ta", 10, 0),
			}
			Expect(saturationEntry(s)).To(BeIdenticalTo(satResult))
		})

		It("returns nil when saturation is absent", func() {
			s := []NamedAnalyzerResult{makeNamed("ta", 10, 0)}
			Expect(saturationEntry(s)).To(BeNil())
		})
	})
})

// makeNamedPD builds a NamedAnalyzerResult with RoleCapacities for P/D tests.
// RoleSpare is initialized from pSC/dSC (as initDisaggregatedRemaining would do).
func makeNamedPD(name string, pRC, dRC, pSC, dSC float64, pDemand, dDemand float64, vPPRC float64, vDPRC float64) NamedAnalyzerResult {
	return NamedAnalyzerResult{
		Name: name,
		Result: &interfaces.AnalyzerResult{
			VariantCapacities: []interfaces.VariantCapacity{
				{VariantName: "pf", Role: "prefill", PerReplicaCapacity: vPPRC},
				{VariantName: "dc", Role: "decode", PerReplicaCapacity: vDPRC},
			},
			RoleCapacities: map[string]interfaces.RoleCapacity{
				"prefill": {Role: "prefill", RequiredCapacity: pRC, SpareCapacity: pSC, TotalDemand: pDemand},
				"decode":  {Role: "decode", RequiredCapacity: dRC, SpareCapacity: dSC, TotalDemand: dDemand},
			},
		},
		Remaining: pRC, // P-scope after initDisaggregatedRemaining
		RoleSpare: map[string]float64{"prefill": pSC, "decode": dSC},
	}
}

var _ = Describe("paired helpers", func() {

	Describe("initRoleState", func() {
		It("disaggregated: roles from RoleCapacities; picker-state from RC; RoleSpare from SC", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 15000, 5000, 20000, 10000, 15000, 5000, 10000, 10000)}
			roles, ps := initRoleState(s)
			Expect(roles).To(ConsistOf("prefill", "decode"))
			Expect(ps[0]["prefill"]).To(BeNumerically("~", 15000.0, 1e-9))
			Expect(ps[0]["decode"]).To(BeNumerically("~", 5000.0, 1e-9))
			Expect(s[0].RoleSpare["prefill"]).To(BeNumerically("~", 20000.0, 1e-9))
			Expect(s[0].RoleSpare["decode"]).To(BeNumerically("~", 10000.0, 1e-9))
		})

		It("non-disaggregated: synthetic 'both' role using model-level Remaining/Spare", func() {
			s := []NamedAnalyzerResult{makeNamed("sat", 20000, 5000, "v", 10.0)}
			roles, ps := initRoleState(s)
			Expect(roles).To(ConsistOf(interfaces.RoleBoth))
			Expect(ps[0][interfaces.RoleBoth]).To(BeNumerically("~", 20000.0, 1e-9))
			Expect(s[0].RoleSpare[interfaces.RoleBoth]).To(BeNumerically("~", 5000.0, 1e-9))
		})
	})

	Describe("roleBottleneckReplicas", func() {
		It("computes max cross-analyzer ceil(roleRemaining/PRC)", func() {
			// analyzer0: prefill remaining=10000, PRC=5000 → ceil(10000/5000)=2
			// analyzer1: prefill remaining=15000, PRC=5000 → ceil(15000/5000)=3 (max)
			s := []NamedAnalyzerResult{
				makeNamedPD("sat", 10000, 20000, 0, 0, 10000, 20000, 5000, 8000),
				makeNamedPD("ta", 15000, 15000, 0, 0, 15000, 15000, 5000, 8000),
			}
			_, ps := initRoleState(s)
			Expect(roleBottleneckReplicas(s, ps, "prefill", "pf")).To(Equal(3))
			// decode: max(ceil(20000/8000)=3, ceil(15000/8000)=2) = 3
			Expect(roleBottleneckReplicas(s, ps, "decode", "dc")).To(Equal(3))
		})

		It("returns 0 when PRC=0 (cold-start guard)", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 10000, 20000, 0, 0, 10000, 20000, 0, 0)}
			_, ps := initRoleState(s)
			Expect(roleBottleneckReplicas(s, ps, "prefill", "pf")).To(Equal(0))
		})
	})

	Describe("safeRemovalReplicasForRole", func() {
		It("computes removable replicas from RoleSpare for a given role", func() {
			// RoleSpare["prefill"]=20000, PRC_P=10000 → floor(20000/10000)=2
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)}
			Expect(safeRemovalReplicasForRole(s, "pf", "prefill")).To(Equal(2))
			// RoleSpare["decode"]=30000, PRC_D=10000 → floor(30000/10000)=3
			Expect(safeRemovalReplicasForRole(s, "dc", "decode")).To(Equal(3))
		})

		It("returns 0 when RoleSpare for role is 0", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 0, 30000, 10000, 30000, 10000, 10000)}
			Expect(safeRemovalReplicasForRole(s, "pf", "prefill")).To(Equal(0))
		})

		It("returns 0 when RoleSpare is nil", func() {
			e := makeNamed("sat", 0, 100, "v", 10.0)
			e.RoleSpare = nil
			Expect(safeRemovalReplicasForRole([]NamedAnalyzerResult{e}, "v", "prefill")).To(Equal(0))
		})
	})

	Describe("applyDeallocationForRole", func() {
		It("decrements RoleSpare[role] by n×PRC", func() {
			// RoleSpare["prefill"]=20000, PRC=10000, n=2 → 20000-20000=0
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)}
			applyDeallocationForRole(s, "pf", "prefill", 2)
			Expect(s[0].RoleSpare["prefill"]).To(Equal(0.0))
			// decode spare unchanged
			Expect(s[0].RoleSpare["decode"]).To(BeNumerically("~", 30000.0, 1e-9))
		})

		It("clamps RoleSpare to 0", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 5000, 0, 10000, 0, 10000, 10000)}
			applyDeallocationForRole(s, "pf", "prefill", 5) // would subtract 50000
			Expect(s[0].RoleSpare["prefill"]).To(Equal(0.0))
		})
	})

	Describe("needsScaleDownForRole", func() {
		It("returns true when all analyzers have RoleSpare[role] > 0", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 20000, 30000, 10000, 30000, 10000, 10000)}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeTrue())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})

		It("returns false when any analyzer has RoleSpare[role] = 0", func() {
			s := []NamedAnalyzerResult{makeNamedPD("sat", 0, 0, 0, 30000, 10000, 30000, 10000, 10000)}
			Expect(needsScaleDownForRole(s, "prefill")).To(BeFalse())
			Expect(needsScaleDownForRole(s, "decode")).To(BeTrue())
		})

		It("returns false for nil RoleSpare", func() {
			e := makeNamed("sat", 0, 100, "v", 10.0)
			e.RoleSpare = nil
			Expect(needsScaleDownForRole([]NamedAnalyzerResult{e}, "prefill")).To(BeFalse())
		})
	})

	Describe("variantsForRole", func() {
		It("filters variants by exact role match", func() {
			vcs := []interfaces.VariantCapacity{
				{VariantName: "pf", Role: "prefill"},
				{VariantName: "dc", Role: "decode"},
				{VariantName: "both", Role: "both"},
			}
			Expect(variantsForRole(vcs, "prefill")).To(HaveLen(1))
			Expect(variantsForRole(vcs, "prefill")[0].VariantName).To(Equal("pf"))
			Expect(variantsForRole(vcs, "decode")[0].VariantName).To(Equal("dc"))
		})

		It("matches 'both' query against both explicit 'both' and empty-role variants", func() {
			vcs := []interfaces.VariantCapacity{
				{VariantName: "pf", Role: "prefill"},
				{VariantName: "dc", Role: "decode"},
				{VariantName: "all", Role: "both"},
				{VariantName: "also-both"}, // empty Role → canonicalized to "both" by variantsForRole
			}
			result := variantsForRole(vcs, "both")
			Expect(result).To(HaveLen(2))
			names := []string{result[0].VariantName, result[1].VariantName}
			Expect(names).To(ConsistOf("all", "also-both"))
			// querying "" matches nothing (vc empty roles are canonicalized to "both", not "")
			Expect(variantsForRole(vcs, "")).To(BeEmpty())
		})
	})

})
