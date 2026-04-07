package solver

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/config"
)

type Optimizer struct {
	spec             *config.OptimizerSpec
	solver           *Solver
	solutionTimeMsec int64
}

// Create optimizer from spec
func NewOptimizerFromSpec(spec *config.OptimizerSpec) *Optimizer {
	return &Optimizer{
		spec: spec,
	}
}

func (o *Optimizer) Optimize() error {
	if o.spec == nil {
		return errors.New("missing optimizer spec")
	}
	o.solver = NewSolver(o.spec)

	startTime := time.Now()
	err := o.solver.Solve()
	endTime := time.Now()
	o.solutionTimeMsec = endTime.Sub(startTime).Milliseconds()
	return err
}

func (o *Optimizer) SolutionTimeMsec() int64 {
	return o.solutionTimeMsec
}

func (o *Optimizer) String() string {
	var b bytes.Buffer
	if o.solver != nil {
		b.WriteString(o.solver.String())
	}
	fmt.Fprintf(&b, "Solution time: %d msec\n", o.solutionTimeMsec)
	return b.String()
}
