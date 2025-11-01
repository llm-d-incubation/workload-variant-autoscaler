package core

import (
	"fmt"

	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
)

// An accelerator used in an inference server
//   - full or multiple GPU units (cards)
type Accelerator struct {
	name string
	spec *config.AcceleratorSpec

	// power profile slope at low utilization
	slopeLow float32
	// power profile slope at high utilization
	slopeHigh float32
}

func NewAcceleratorFromSpec(spec *config.AcceleratorSpec) *Accelerator {
	return &Accelerator{
		name: spec.Name,
		spec: spec,
	}
}

// Calculate basic parameters
func (g *Accelerator) Calculate() {
	g.slopeLow = float32(g.spec.Power.MidPower-g.spec.Power.Idle) / g.spec.Power.MidUtil
	g.slopeHigh = float32(g.spec.Power.Full-g.spec.Power.MidPower) / (1 - g.spec.Power.MidUtil)
}

// Evaluate power consumption at a given utilization
func (g *Accelerator) Power(util float32) float32 {
	if util <= g.spec.Power.MidUtil {
		return float32(g.spec.Power.Idle) + g.slopeLow*util
	} else {
		return float32(g.spec.Power.MidPower) + g.slopeHigh*(util-g.spec.Power.MidUtil)
	}
}

func (g *Accelerator) Name() string {
	return g.name
}

func (g *Accelerator) Spec() *config.AcceleratorSpec {
	return g.spec
}

func (g *Accelerator) Type() string {
	return g.spec.Type
}

func (g *Accelerator) Cost() float32 {
	return g.spec.Cost
}

func (g *Accelerator) Multiplicity() int32 {
	return int32(g.spec.Multiplicity)
}

func (g *Accelerator) MemSize() int {
	return g.spec.MemSize
}

func (g *Accelerator) String() string {
	return fmt.Sprintf("Accelerator: name=%s; type=%s; multiplicity=%d; memSize=%d; memBW=%d; cost=%v; power={ %d, %d, %d @ %v }",
		g.name, g.spec.Type, g.spec.Multiplicity, g.spec.MemSize, g.spec.MemBW, g.spec.Cost,
		g.spec.Power.Idle, g.spec.Power.Full, g.spec.Power.MidPower, g.spec.Power.MidUtil)
}
