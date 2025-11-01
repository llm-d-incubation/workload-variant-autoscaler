package core

import (
	"testing"

	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
)

func TestNewAcceleratorFromSpec(t *testing.T) {
	tests := []struct {
		name string
		spec *config.AcceleratorSpec
		want string
	}{
		{
			name: "valid accelerator spec",
			spec: &config.AcceleratorSpec{
				Name: "H100",
				Power: config.PowerSpec{
					Idle:     100,
					MidPower: 300,
					Full:     700,
					MidUtil:  0.5,
				},
			},
			want: "H100",
		},
		{
			name: "empty name",
			spec: &config.AcceleratorSpec{
				Name: "",
				Power: config.PowerSpec{
					Idle:     50,
					MidPower: 150,
					Full:     350,
					MidUtil:  0.4,
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := NewAcceleratorFromSpec(tt.spec)
			if acc == nil {
				t.Fatal("NewAcceleratorFromSpec() returned nil")
			}
			if got := acc.Name(); got != tt.want {
				t.Errorf("Accelerator.Name() = %v, want %v", got, tt.want)
			}
			if got := acc.Spec(); got != tt.spec {
				t.Errorf("Accelerator.Spec() = %v, want %v", got, tt.spec)
			}
		})
	}
}

func TestAccelerator_Calculate(t *testing.T) {
	spec := &config.AcceleratorSpec{
		Name: "TestAcc",
		Power: config.PowerSpec{
			Idle:     100,
			MidPower: 300,
			Full:     700,
			MidUtil:  0.5,
		},
	}

	acc := NewAcceleratorFromSpec(spec)
	acc.Calculate()

	// Test that calculation doesn't crash and accelerator is still functional
	if acc.Name() != "TestAcc" {
		t.Errorf("Name after Calculate() = %v, want TestAcc", acc.Name())
	}
}

func TestAccelerator_Fields(t *testing.T) {
	spec := &config.AcceleratorSpec{
		Name: "TestAcc",
		Power: config.PowerSpec{
			Idle:     100,
			MidPower: 300,
			Full:     700,
			MidUtil:  0.5,
		},
	}

	acc := NewAcceleratorFromSpec(spec)
	acc.Calculate()

	if acc.Type() != spec.Type {
		t.Errorf("Accelerator.Type() = %v, want %v", acc.Type(), spec.Type)
	}
	if acc.Cost() != spec.Cost {
		t.Errorf("Accelerator.Cost() = %v, want %v", acc.Cost(), spec.Cost)
	}
	if acc.Multiplicity() != int32(spec.Multiplicity) {
		t.Errorf("Accelerator.Multiplicity() = %v, want %v", acc.Multiplicity(), spec.Multiplicity)
	}
	if acc.MemSize() != spec.MemSize {
		t.Errorf("Accelerator.MemSize() = %v, want %v", acc.MemSize(), spec.MemSize)
	}
	if acc.Name() != "TestAcc" {
		t.Errorf("Name after Calculate() = %v, want TestAcc", acc.Name())
	}
}

func TestAccelerator_Power(t *testing.T) {
	spec := &config.AcceleratorSpec{
		Name: "TestAcc",
		Power: config.PowerSpec{
			Idle:     100,
			MidPower: 300,
			Full:     700,
			MidUtil:  0.5,
		},
	}

	acc := NewAcceleratorFromSpec(spec)
	acc.Calculate()

	tests := []struct {
		name string
		util float32
		want float32
	}{
		{
			name: "zero utilization",
			util: 0.0,
			want: 100.0, // idle power
		},
		{
			name: "mid utilization",
			util: 0.5,
			want: 300.0, // mid power
		},
		{
			name: "full utilization",
			util: 1.0,
			want: 700.0, // full power
		},
		{
			name: "low utilization",
			util: 0.25,
			want: 200.0, // interpolated between idle and mid
		},
		{
			name: "high utilization",
			util: 0.75,
			want: 500.0, // interpolated between mid and full
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := acc.Power(tt.util)
			if got != tt.want {
				t.Errorf("Accelerator.Power(%v) = %v, want %v", tt.util, got, tt.want)
			}
		})
	}
}

func TestAccelerator_Power_EdgeCases(t *testing.T) {
	spec := &config.AcceleratorSpec{
		Name: "TestAcc",
		Power: config.PowerSpec{
			Idle:     100,
			MidPower: 300,
			Full:     700,
			MidUtil:  0.5,
		},
	}

	acc := NewAcceleratorFromSpec(spec)
	acc.Calculate()

	tests := []struct {
		name string
		util float32
	}{
		{
			name: "negative utilization",
			util: -0.1,
		},
		{
			name: "over 100% utilization",
			util: 1.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			power := acc.Power(tt.util)
			if power < 0 {
				t.Errorf("Accelerator.Power(%v) = %v, expected non-negative", tt.util, power)
			}
		})
	}
}
