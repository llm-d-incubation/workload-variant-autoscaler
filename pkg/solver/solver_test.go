package solver

import (
	"strings"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/core"
)

func TestNewSolver(t *testing.T) {
	tests := []struct {
		name          string
		optimizerSpec *config.OptimizerSpec
		wantErr       bool
	}{
		{
			name: "valid optimizer spec",
			optimizerSpec: &config.OptimizerSpec{
				Unlimited:        false,
				SaturationPolicy: "None",
			},
			wantErr: false,
		},
		{
			name: "unlimited optimizer spec",
			optimizerSpec: &config.OptimizerSpec{
				Unlimited:        true,
				SaturationPolicy: "PriorityExhaustive",
			},
			wantErr: false,
		},
		{
			name:          "nil optimizer spec",
			optimizerSpec: nil,
			wantErr:       false, // Constructor should handle nil gracefully
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			solver := NewSolver(tt.optimizerSpec)
			if solver == nil && !tt.wantErr {
				t.Fatal("NewSolver() returned nil unexpectedly")
			}
			if solver != nil && tt.wantErr {
				t.Fatal("NewSolver() should have failed but didn't")
			}
			if solver != nil {
				// Check that internal maps are initialized
				if solver.currentAllocation == nil {
					t.Error("currentAllocation map not initialized")
				}
				if solver.diffAllocation == nil {
					t.Error("diffAllocation map not initialized")
				}
			}
		})
	}
}

func TestSolver_Solve(t *testing.T) {
	tests := []struct {
		name          string
		optimizerSpec *config.OptimizerSpec
		setup         func(optimizerSpec *config.OptimizerSpec)
		wantErr       bool
	}{
		{
			name: "solve with limited resources - basic test",
			optimizerSpec: &config.OptimizerSpec{
				Unlimited:        false,
				SaturationPolicy: "None",
			},
			setup: func(optimizerSpec *config.OptimizerSpec) {
				system := core.NewSystem()
				system.SetFromSpec(&config.SystemSpec{
					Accelerators: config.AcceleratorData{
						Spec: []config.AcceleratorSpec{
							{
								Name: "A100",
								Power: config.PowerSpec{
									Idle:     50,
									MidPower: 150,
									Full:     350,
									MidUtil:  0.4,
								},
							},
						},
					},
					Models: config.ModelData{
						PerfData: []config.ModelAcceleratorPerfData{
							{
								Name:     "llama-7b",
								Acc:      "A100",
								AccCount: 1,
							},
						},
					},
					Capacity: config.CapacityData{
						Count: []config.AcceleratorCount{
							{
								Type:  "A100",
								Count: 2,
							},
						},
					},
					Servers: config.ServerData{
						Spec: []config.ServerSpec{
							{
								Name:            "server1",
								Class:           "default",
								Model:           "llama-7b",
								KeepAccelerator: true,
								MinNumReplicas:  1,
								MaxBatchSize:    512,
								CurrentAlloc: config.AllocationData{
									Accelerator: "A100",
									NumReplicas: 1,
								},
							},
						},
					},
					ServiceClasses: config.ServiceClassData{
						Spec: []config.ServiceClassSpec{
							{
								Name:     "default",
								Priority: 1,
								ModelTargets: []config.ModelTarget{
									{
										Model:    "llama-7b",
										SLO_ITL:  9,
										SLO_TTFT: 1000,
									},
								},
							},
						},
					},
					Optimizer: config.OptimizerData{
						Spec: *optimizerSpec,
					},
				})
				core.TheSystem = system
			},
			wantErr: false,
		},
		{
			name: "solve with unlimited resources - basic test",
			optimizerSpec: &config.OptimizerSpec{
				Unlimited:        true,
				SaturationPolicy: "None",
			},
			setup: func(optimizerSpec *config.OptimizerSpec) {
				system := core.NewSystem()
				system.SetFromSpec(&config.SystemSpec{
					Accelerators: config.AcceleratorData{
						Spec: []config.AcceleratorSpec{
							{
								Name: "A100",
								Power: config.PowerSpec{
									Idle:     50,
									MidPower: 150,
									Full:     350,
									MidUtil:  0.4,
								},
							},
						},
					},
					Models: config.ModelData{
						PerfData: []config.ModelAcceleratorPerfData{
							{
								Name:     "llama-7b",
								Acc:      "A100",
								AccCount: 1,
							},
						},
					},
					Capacity: config.CapacityData{
						Count: []config.AcceleratorCount{
							{
								Type:  "A100",
								Count: 2,
							},
						},
					},
					Servers: config.ServerData{
						Spec: []config.ServerSpec{
							{
								Name:            "server1",
								Class:           "default",
								Model:           "llama-7b",
								KeepAccelerator: true,
								MinNumReplicas:  1,
								MaxBatchSize:    512,
								CurrentAlloc: config.AllocationData{
									Accelerator: "A100",
									NumReplicas: 1,
								},
							},
						},
					},
					ServiceClasses: config.ServiceClassData{
						Spec: []config.ServiceClassSpec{
							{
								Name:     "default",
								Priority: 1,
								ModelTargets: []config.ModelTarget{
									{
										Model:    "llama-7b",
										SLO_ITL:  9,
										SLO_TTFT: 1000,
									},
								},
							},
						},
					},
					Optimizer: config.OptimizerData{
						Spec: *optimizerSpec,
					},
				})
				core.TheSystem = system
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(tt.optimizerSpec)
			}

			solver := NewSolver(tt.optimizerSpec)
			err := solver.Solve()
			if (err != nil) != tt.wantErr {
				t.Errorf("Solver.Solve() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSolver_String(t *testing.T) {
	optimizerSpec := &config.OptimizerSpec{
		Unlimited:        false,
		SaturationPolicy: "None",
	}

	solver := NewSolver(optimizerSpec)

	str := solver.String()
	if str == "" {
		t.Error("Solver.String() returned empty string")
	}
}

func TestSolver_AllocationDiff(t *testing.T) {
	optimizerSpec := &config.OptimizerSpec{
		Unlimited:        false,
		SaturationPolicy: "None",
	}

	solver := NewSolver(optimizerSpec)

	// Initially, AllocationDiff should return empty map
	diffMap := solver.AllocationDiff()
	if diffMap == nil {
		t.Error("AllocationDiff() returned nil")
	}
	if len(diffMap) != 0 {
		t.Errorf("AllocationDiff() should return empty map initially, got %d entries", len(diffMap))
	}

	// Test that subsequent calls return consistent results
	diffMap2 := solver.AllocationDiff()
	if len(diffMap) != len(diffMap2) {
		t.Error("AllocationDiff() should return consistent results")
	}
}

func TestSolver_SolveUnlimited(t *testing.T) {
	// Setup a more comprehensive system for testing
	system := core.NewSystem()
	system.SetFromSpec(&config.SystemSpec{
		Accelerators: config.AcceleratorData{
			Spec: []config.AcceleratorSpec{
				{
					Name: "A100",
					Power: config.PowerSpec{
						Idle:     50,
						MidPower: 150,
						Full:     350,
						MidUtil:  0.4,
					},
				},
				{
					Name: "H100",
					Power: config.PowerSpec{
						Idle:     60,
						MidPower: 200,
						Full:     450,
						MidUtil:  0.5,
					},
				},
			},
		},
		Models: config.ModelData{
			PerfData: []config.ModelAcceleratorPerfData{
				{
					Name:     "llama-7b",
					Acc:      "A100",
					AccCount: 1,
				},
				{
					Name:     "llama-7b",
					Acc:      "H100",
					AccCount: 1,
				},
			},
		},
		Capacity: config.CapacityData{
			Count: []config.AcceleratorCount{
				{
					Type:  "A100",
					Count: 4,
				},
				{
					Type:  "H100",
					Count: 2,
				},
			},
		},
		Servers: config.ServerData{
			Spec: []config.ServerSpec{
				{
					Name:            "server1",
					Class:           "default",
					Model:           "llama-7b",
					KeepAccelerator: true,
					MinNumReplicas:  1,
					MaxBatchSize:    512,
					CurrentAlloc: config.AllocationData{
						Accelerator: "A100",
						NumReplicas: 2,
					},
				},
				{
					Name:            "server2",
					Class:           "default",
					Model:           "llama-7b",
					KeepAccelerator: true,
					MinNumReplicas:  1,
					MaxBatchSize:    512,
					CurrentAlloc: config.AllocationData{
						Accelerator: "H100",
						NumReplicas: 1,
					},
				},
			},
		},
		ServiceClasses: config.ServiceClassData{
			Spec: []config.ServiceClassSpec{
				{
					Name:     "default",
					Priority: 1,
					ModelTargets: []config.ModelTarget{
						{
							Model:    "llama-7b",
							SLO_ITL:  9,
							SLO_TTFT: 1000,
						},
					},
				},
			},
		},
	})
	core.TheSystem = system

	// Calculate server allocations to populate candidate allocations
	for _, server := range core.GetServers() {
		server.Calculate(core.GetAccelerators())
	}

	optimizerSpec := &config.OptimizerSpec{
		Unlimited:        true,
		SaturationPolicy: "None",
	}

	solver := NewSolver(optimizerSpec)

	// Test SolveUnlimited directly
	solver.SolveUnlimited()

	// Verify that servers received allocations (should select minimum value allocations)
	servers := core.GetServers()
	if len(servers) == 0 {
		t.Fatal("Expected servers to exist in the system")
	}

	allocatedCount := 0
	for _, server := range servers {
		if server.Allocation() != nil {
			allocatedCount++
			t.Logf("Server %s allocated: %d replicas of %s (value: %f)",
				server.Name(),
				server.Allocation().NumReplicas(),
				server.Allocation().Accelerator(),
				server.Allocation().Value())
		}
	}

	// With unlimited resources and valid server setup, all servers should get allocations
	if allocatedCount == 0 {
		t.Error("Expected at least some servers to receive allocations in unlimited mode")
	}

	// Verify allocated servers have positive replica counts
	for _, server := range servers {
		if alloc := server.Allocation(); alloc != nil {
			if alloc.NumReplicas() <= 0 {
				t.Errorf("Server %s allocation should have positive replicas, got %d",
					server.Name(), alloc.NumReplicas())
			}
		}
	}
}

func TestSolver_SolveUnlimited_EdgeCases(t *testing.T) {
	// Test SolveUnlimited with no servers
	t.Run("NoServers", func(t *testing.T) {
		system := core.NewSystem()
		core.TheSystem = system

		optimizerSpec := &config.OptimizerSpec{
			Unlimited:        true,
			SaturationPolicy: "None",
		}
		solver := NewSolver(optimizerSpec)

		solver.SolveUnlimited()
	})

	// Test SolveUnlimited with servers that have no allocations
	t.Run("ServersWithNoAllocations", func(t *testing.T) {
		system := core.NewSystem()
		system.SetFromSpec(&config.SystemSpec{
			Accelerators: config.AcceleratorData{
				Spec: []config.AcceleratorSpec{
					{
						Name: "A100",
						Power: config.PowerSpec{
							Idle:     50,
							MidPower: 150,
							Full:     350,
							MidUtil:  0.4,
						},
					},
				},
			},
			Models: config.ModelData{
				PerfData: []config.ModelAcceleratorPerfData{
					{
						Name:     "llama-7b",
						Acc:      "A100",
						AccCount: 1,
					},
				},
			},
			Servers: config.ServerData{
				Spec: []config.ServerSpec{
					{
						Name:            "test-server",
						Class:           "default",
						Model:           "llama-7b",
						KeepAccelerator: true,
						MinNumReplicas:  1,
						MaxBatchSize:    512,
					},
				},
			},
			ServiceClasses: config.ServiceClassData{
				Spec: []config.ServiceClassSpec{
					{
						Name:     "default",
						Priority: 1,
						ModelTargets: []config.ModelTarget{
							{
								Model:    "llama-7b",
								SLO_ITL:  9,
								SLO_TTFT: 1000,
							},
						},
					},
				},
			},
		})
		core.TheSystem = system

		// Clear all allocations from servers to test empty allocation case
		for _, server := range core.GetServers() {
			server.RemoveAllocation()
		}

		optimizerSpec := &config.OptimizerSpec{
			Unlimited:        true,
			SaturationPolicy: "None",
		}
		solver := NewSolver(optimizerSpec)
		solver.SolveUnlimited()

		// Verify servers still have no allocations
		for _, server := range core.GetServers() {
			if server.Allocation() != nil {
				t.Errorf("Expected server %s to have no allocation", server.Name())
			}
		}
	})
}

func TestSolver_SolveUnlimited_MinValueSelection(t *testing.T) {
	// Create a simple system for testing minimum value selection
	system := core.NewSystem()
	system.SetFromSpec(&config.SystemSpec{
		Accelerators: config.AcceleratorData{
			Spec: []config.AcceleratorSpec{
				{
					Name: "A100",
					Power: config.PowerSpec{
						Idle:     50,
						MidPower: 150,
						Full:     350,
						MidUtil:  0.4,
					},
				},
			},
		},
		Models: config.ModelData{
			PerfData: []config.ModelAcceleratorPerfData{
				{
					Name:     "llama-7b",
					Acc:      "A100",
					AccCount: 1,
				},
			},
		},
		Capacity: config.CapacityData{
			Count: []config.AcceleratorCount{
				{
					Type:  "A100",
					Count: 2,
				},
			},
		},
		Servers: config.ServerData{
			Spec: []config.ServerSpec{
				{
					Name:            "server1",
					Class:           "default",
					Model:           "llama-7b",
					KeepAccelerator: true,
					MinNumReplicas:  1,
					MaxBatchSize:    512,
				},
			},
		},
		ServiceClasses: config.ServiceClassData{
			Spec: []config.ServiceClassSpec{
				{
					Name:     "default",
					Priority: 1,
					ModelTargets: []config.ModelTarget{
						{
							Model:    "llama-7b",
							SLO_ITL:  9,
							SLO_TTFT: 1000,
						},
					},
				},
			},
		},
	})
	core.TheSystem = system

	optimizerSpec := &config.OptimizerSpec{
		Unlimited:        true,
		SaturationPolicy: "None",
	}

	solver := NewSolver(optimizerSpec)

	// Get server and its allocations to manipulate values
	server := core.GetServer("server1")
	if server == nil {
		t.Fatal("Could not find server1")
	}

	allocations := server.AllAllocations()
	if len(allocations) < 2 {
		t.Skip("Need at least 2 allocations to test minimum value selection")
	}

	// Set different values and verify minimum is selected
	allocs := make([]*core.Allocation, 0, len(allocations))
	for _, alloc := range allocations {
		allocs = append(allocs, alloc)
	}

	if len(allocs) >= 2 {
		allocs[0].SetValue(100.0) // Higher value
		allocs[1].SetValue(50.0)  // Lower value - should be selected
	}

	solver.SolveUnlimited()

	selectedAlloc := server.Allocation()
	if selectedAlloc == nil {
		t.Error("Expected server to have an allocation after SolveUnlimited")
	} else if selectedAlloc.Value() > 50.0 {
		// The allocation with minimum value should be selected
		t.Errorf("Expected allocation with minimum value to be selected, got value %f", selectedAlloc.Value())
	}
}

func TestSolver_String_WithDiffs(t *testing.T) {
	// Setup system and run solve to generate allocation diffs
	system := core.NewSystem()
	system.SetFromSpec(&config.SystemSpec{
		Accelerators: config.AcceleratorData{
			Spec: []config.AcceleratorSpec{
				{
					Name: "A100",
					Power: config.PowerSpec{
						Idle:     50,
						MidPower: 150,
						Full:     350,
						MidUtil:  0.4,
					},
				},
			},
		},
		Models: config.ModelData{
			PerfData: []config.ModelAcceleratorPerfData{
				{
					Name:     "llama-7b",
					Acc:      "A100",
					AccCount: 1,
				},
			},
		},
		Capacity: config.CapacityData{
			Count: []config.AcceleratorCount{
				{
					Type:  "A100",
					Count: 2,
				},
			},
		},
		Servers: config.ServerData{
			Spec: []config.ServerSpec{
				{
					Name:            "test-server",
					Class:           "default",
					Model:           "llama-7b",
					KeepAccelerator: true,
					MinNumReplicas:  1,
					MaxBatchSize:    512,
					CurrentAlloc: config.AllocationData{
						Accelerator: "A100",
						NumReplicas: 1,
					},
				},
			},
		},
		ServiceClasses: config.ServiceClassData{
			Spec: []config.ServiceClassSpec{
				{
					Name:     "default",
					Priority: 1,
					ModelTargets: []config.ModelTarget{
						{
							Model:    "llama-7b",
							SLO_ITL:  9,
							SLO_TTFT: 1000,
						},
					},
				},
			},
		},
	})
	core.TheSystem = system

	optimizerSpec := &config.OptimizerSpec{
		Unlimited:        false,
		SaturationPolicy: "None",
	}

	solver := NewSolver(optimizerSpec)

	// Run solve to potentially generate allocation diffs
	err := solver.Solve()
	if err != nil {
		t.Fatalf("Solve() failed: %v", err)
	}

	str := solver.String()

	// If there are allocation diffs, should contain server names
	if len(solver.AllocationDiff()) > 0 {
		foundServerInfo := false
		for serverName := range solver.AllocationDiff() {
			if strings.Contains(str, serverName) {
				foundServerInfo = true
				break
			}
		}
		if !foundServerInfo {
			t.Error("String() should contain server information when allocation diffs exist")
		}
	}
}

// Additional tests to improve coverage of SolveUnlimited and other functions
func TestSolver_SolveUnlimited_ValueComparison(t *testing.T) {
	// Test the value comparison logic in SolveUnlimited
	system := core.NewSystem()
	system.SetFromSpec(&config.SystemSpec{
		Accelerators: config.AcceleratorData{
			Spec: []config.AcceleratorSpec{
				{
					Name: "A100",
					Power: config.PowerSpec{
						Idle:     50,
						MidPower: 150,
						Full:     350,
						MidUtil:  0.4,
					},
				},
			},
		},
		Models: config.ModelData{
			PerfData: []config.ModelAcceleratorPerfData{
				{
					Name:     "llama-7b",
					Acc:      "A100",
					AccCount: 1,
				},
			},
		},
		Capacity: config.CapacityData{
			Count: []config.AcceleratorCount{
				{
					Type:  "A100",
					Count: 4,
				},
			},
		},
		Servers: config.ServerData{
			Spec: []config.ServerSpec{
				{
					Name:            "test-server",
					Class:           "default",
					Model:           "llama-7b",
					KeepAccelerator: true,
					MinNumReplicas:  1,
					MaxBatchSize:    512,
				},
			},
		},
		ServiceClasses: config.ServiceClassData{
			Spec: []config.ServiceClassSpec{
				{
					Name:     "default",
					Priority: 1,
					ModelTargets: []config.ModelTarget{
						{
							Model:    "llama-7b",
							SLO_ITL:  9,
							SLO_TTFT: 1000,
						},
					},
				},
			},
		},
	})
	core.TheSystem = system

	// Ensure server has multiple allocations with different values
	server := core.GetServer("test-server")
	if server != nil {
		server.Calculate(core.GetAccelerators())
		allocations := server.AllAllocations()

		if len(allocations) >= 2 {
			// Set different values to test minimum selection
			i := 0
			for _, alloc := range allocations {
				if i == 0 {
					alloc.SetValue(100.0) // Higher value
				} else {
					alloc.SetValue(50.0) // Lower value - should be selected
				}
				i++
			}
		}
	}

	optimizerSpec := &config.OptimizerSpec{
		Unlimited:        true,
		SaturationPolicy: "None",
	}

	solver := NewSolver(optimizerSpec)
	solver.SolveUnlimited()

	// Verify minimum value logic was exercised correctly
	server = core.GetServer("test-server")
	if server == nil {
		t.Fatal("Server should exist after solve")
	}

	allocation := server.Allocation()
	if allocation == nil {
		t.Error("Server should have an allocation after SolveUnlimited")
	} else {
		// Verify that the allocation with minimum value was selected
		// We set values to 100.0 and 50.0, so selected value should be around 50.0
		if allocation.Value() > 60.0 {
			t.Errorf("Expected minimum value allocation to be selected (~50.0), got %f", allocation.Value())
		}
		t.Logf("SolveUnlimited selected allocation with value: %f", allocation.Value())
	}
}
