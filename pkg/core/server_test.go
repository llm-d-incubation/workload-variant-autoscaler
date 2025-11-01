package core

import (
	"strings"
	"testing"

	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
)

func TestNewServerFromSpec(t *testing.T) {
	tests := []struct {
		name string
		spec *config.ServerSpec
		want *Server
	}{
		{
			name: "valid server spec",
			spec: &config.ServerSpec{
				Name:  "test-server",
				Model: "test-model",
				Class: "default",
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{
						ArrivalRate:  60,
						AvgInTokens:  100,
						AvgOutTokens: 200,
					},
				},
				MinNumReplicas: 2,
				MaxBatchSize:   16,
			},
			want: &Server{
				name:             "test-server",
				serviceClassName: "default",
				modelName:        "test-model",
				minNumReplicas:   2,
				maxBatchSize:     16,
			},
		},
		{
			name: "server with empty class defaults to default",
			spec: &config.ServerSpec{
				Name:  "test-server",
				Model: "test-model",
				Class: "",
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{
						ArrivalRate:  30,
						AvgInTokens:  50,
						AvgOutTokens: 100,
					},
				},
			},
			want: &Server{
				name:             "test-server",
				serviceClassName: config.DefaultServiceClassName,
				modelName:        "test-model",
			},
		},
		{
			name: "server with keep accelerator flag",
			spec: &config.ServerSpec{
				Name:            "test-server",
				Model:           "test-model",
				Class:           "high-priority",
				KeepAccelerator: true,
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{
						ArrivalRate:  120,
						AvgInTokens:  200,
						AvgOutTokens: 400,
					},
				},
			},
			want: &Server{
				name:             "test-server",
				serviceClassName: "high-priority",
				modelName:        "test-model",
				keepAccelerator:  true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServerFromSpec(tt.spec)

			if server.name != tt.want.name {
				t.Errorf("NewServerFromSpec().name = %v, want %v", server.name, tt.want.name)
			}
			if server.serviceClassName != tt.want.serviceClassName {
				t.Errorf("NewServerFromSpec().serviceClassName = %v, want %v", server.serviceClassName, tt.want.serviceClassName)
			}
			if server.modelName != tt.want.modelName {
				t.Errorf("NewServerFromSpec().modelName = %v, want %v", server.modelName, tt.want.modelName)
			}
			if server.keepAccelerator != tt.want.keepAccelerator {
				t.Errorf("NewServerFromSpec().keepAccelerator = %v, want %v", server.keepAccelerator, tt.want.keepAccelerator)
			}
			if server.minNumReplicas != tt.want.minNumReplicas {
				t.Errorf("NewServerFromSpec().minNumReplicas = %v, want %v", server.minNumReplicas, tt.want.minNumReplicas)
			}
			if server.maxBatchSize != tt.want.maxBatchSize {
				t.Errorf("NewServerFromSpec().maxBatchSize = %v, want %v", server.maxBatchSize, tt.want.maxBatchSize)
			}

			// Check that load is properly set
			if server.load == nil {
				t.Error("NewServerFromSpec().load should not be nil")
			} else {
				expectedLoad := &tt.spec.CurrentAlloc.Load
				if server.load.ArrivalRate != expectedLoad.ArrivalRate {
					t.Errorf("NewServerFromSpec().load.ArrivalRate = %v, want %v", server.load.ArrivalRate, expectedLoad.ArrivalRate)
				}
			}

			// Check that spec is stored
			if server.spec != tt.spec {
				t.Error("NewServerFromSpec().spec should reference the input spec")
			}

			// Check that maps are initialized
			if server.allAllocations == nil {
				t.Error("NewServerFromSpec().allAllocations should be initialized")
			}
		})
	}
}

func TestServer_Getters(t *testing.T) {
	spec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "high-priority",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  60,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		KeepAccelerator: true,
	}
	server := NewServerFromSpec(spec)

	tests := []struct {
		name     string
		getter   func() any
		expected any
	}{
		{
			name:     "Name",
			getter:   func() any { return server.Name() },
			expected: "test-server",
		},
		{
			name:     "ServiceClassName",
			getter:   func() any { return server.ServiceClassName() },
			expected: "high-priority",
		},
		{
			name:     "ModelName",
			getter:   func() any { return server.ModelName() },
			expected: "test-model",
		},
		{
			name:     "KeepAccelerator",
			getter:   func() any { return server.KeepAccelerator() },
			expected: true,
		},
		{
			name:     "Load",
			getter:   func() any { return server.Load() },
			expected: &config.ServerLoadSpec{ArrivalRate: 60, AvgInTokens: 100, AvgOutTokens: 200},
		},
		{
			name:     "Spec",
			getter:   func() any { return server.Spec() },
			expected: spec,
		},
		{
			name:     "AllAllocations",
			getter:   func() any { return server.AllAllocations() },
			expected: "map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.getter()
			if tt.name == "Load" {
				resultLoad := result.(*config.ServerLoadSpec)
				expectedLoad := tt.expected.(*config.ServerLoadSpec)
				if resultLoad.ArrivalRate != expectedLoad.ArrivalRate ||
					resultLoad.AvgInTokens != expectedLoad.AvgInTokens ||
					resultLoad.AvgOutTokens != expectedLoad.AvgOutTokens {
					t.Errorf("%s() = %v, want %v", tt.name, result, tt.expected)
				}
			} else if tt.name == "AllAllocations" {
				resultMap := result.(map[string]*Allocation)
				if resultMap == nil {
					t.Errorf("%s() should not be nil", tt.name)
				}
			} else if result != tt.expected {
				t.Errorf("%s() = %v, want %v", tt.name, result, tt.expected)
			}
		})
	}
}

func TestServer_Priority(t *testing.T) {
	// Setup a test system with service classes
	setupTestSystemForServerPriority := func() {
		system := &System{
			serviceClasses: make(map[string]*ServiceClass),
		}

		// Add service classes with different priorities
		highPriorityClass := NewServiceClass("high-priority", 1)
		lowPriorityClass := NewServiceClass("low-priority", 8)
		system.serviceClasses["high-priority"] = highPriorityClass
		system.serviceClasses["low-priority"] = lowPriorityClass

		TheSystem = system
	}

	tests := []struct {
		name             string
		serviceClassName string
		setupFunc        func()
		expectedPriority int
	}{
		{
			name:             "server with high priority service class",
			serviceClassName: "high-priority",
			setupFunc:        setupTestSystemForServerPriority,
			expectedPriority: 1,
		},
		{
			name:             "server with low priority service class",
			serviceClassName: "low-priority",
			setupFunc:        setupTestSystemForServerPriority,
			expectedPriority: 8,
		},
		{
			name:             "server with nonexistent service class",
			serviceClassName: "nonexistent",
			setupFunc:        setupTestSystemForServerPriority,
			expectedPriority: config.DefaultServiceClassPriority,
		},
		{
			name:             "server with empty system setup",
			serviceClassName: "any-class",
			setupFunc: func() {
				// Set up empty system instead of nil
				TheSystem = &System{serviceClasses: make(map[string]*ServiceClass)}
			},
			expectedPriority: config.DefaultServiceClassPriority,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupFunc()

			spec := &config.ServerSpec{
				Name:  "test-server",
				Model: "test-model",
				Class: tt.serviceClassName,
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{},
				},
			}
			server := NewServerFromSpec(spec)

			priority := server.Priority()
			if priority != tt.expectedPriority {
				t.Errorf("Priority() = %v, want %v", priority, tt.expectedPriority)
			}
		})
	}
}

func TestServer_SetLoad(t *testing.T) {
	spec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  60,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
	}
	server := NewServerFromSpec(spec)

	newLoad := &config.ServerLoadSpec{
		ArrivalRate:  120,
		AvgInTokens:  150,
		AvgOutTokens: 300,
	}

	server.SetLoad(newLoad)

	if server.Load() != newLoad {
		t.Errorf("SetLoad() did not update load correctly")
	}
	if server.Load().ArrivalRate != 120 {
		t.Errorf("SetLoad() arrival rate = %v, want 120", server.Load().ArrivalRate)
	}
}

func TestServer_AllocationManagement(t *testing.T) {
	spec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  60,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
	}
	server := NewServerFromSpec(spec)

	// Test initial state
	if server.Allocation() != nil {
		t.Error("Initial allocation should be nil")
	}

	// Create a mock allocation
	mockAlloc := &Allocation{
		accelerator: "test-gpu",
		numReplicas: 2,
		batchSize:   16,
		cost:        100.0,
	}

	// Test SetAllocation
	server.SetAllocation(mockAlloc)
	if server.Allocation() != mockAlloc {
		t.Error("SetAllocation() did not set allocation correctly")
	}

	// Test RemoveAllocation
	server.RemoveAllocation()
	if server.Allocation() != nil {
		t.Error("RemoveAllocation() did not remove allocation")
	}
}

func TestServer_CurAllocationManagement(t *testing.T) {
	spec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Accelerator: "test-gpu",
			NumReplicas: 1,
			MaxBatch:    8,
			Cost:        50.0,
			Load: config.ServerLoadSpec{
				ArrivalRate:  60,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
	}
	server := NewServerFromSpec(spec)

	// Test that current allocation is set from spec
	if server.CurAllocation() == nil {
		t.Error("CurAllocation should be initialized from spec")
	}

	// Create a new current allocation
	newCurAlloc := &Allocation{
		accelerator: "new-gpu",
		numReplicas: 3,
		batchSize:   32,
		cost:        200.0,
	}

	// Test SetCurAllocation
	server.SetCurAllocation(newCurAlloc)
	if server.CurAllocation() != newCurAlloc {
		t.Error("SetCurAllocation() did not set current allocation correctly")
	}
}

func TestServer_GetCandidateAccelerators(t *testing.T) {
	accelerators := map[string]*Accelerator{
		"gpu-a": NewAcceleratorFromSpec(&config.AcceleratorSpec{Name: "gpu-a", Cost: 100.0}),
		"gpu-b": NewAcceleratorFromSpec(&config.AcceleratorSpec{Name: "gpu-b", Cost: 150.0}),
		"gpu-c": NewAcceleratorFromSpec(&config.AcceleratorSpec{Name: "gpu-c", Cost: 80.0}),
	}

	tests := []struct {
		name            string
		keepAccelerator bool
		curAllocation   *Allocation
		expectedCount   int
		expectedNames   []string
	}{
		{
			name:            "no keep accelerator constraint",
			keepAccelerator: false,
			curAllocation:   nil,
			expectedCount:   3,
			expectedNames:   []string{"gpu-a", "gpu-b", "gpu-c"},
		},
		{
			name:            "keep accelerator but no current allocation",
			keepAccelerator: true,
			curAllocation:   nil,
			expectedCount:   3,
			expectedNames:   []string{"gpu-a", "gpu-b", "gpu-c"},
		},
		{
			name:            "keep accelerator with current allocation",
			keepAccelerator: true,
			curAllocation:   &Allocation{accelerator: "gpu-b"},
			expectedCount:   1,
			expectedNames:   []string{"gpu-b"},
		},
		{
			name:            "keep accelerator with nonexistent current accelerator",
			keepAccelerator: true,
			curAllocation:   &Allocation{accelerator: "nonexistent-gpu"},
			expectedCount:   0,
			expectedNames:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &config.ServerSpec{
				Name:            "test-server",
				Model:           "test-model",
				Class:           "default",
				KeepAccelerator: tt.keepAccelerator,
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{},
				},
			}
			server := NewServerFromSpec(spec)
			server.SetCurAllocation(tt.curAllocation)

			candidates := server.GetCandidateAccelerators(accelerators)

			if len(candidates) != tt.expectedCount {
				t.Errorf("GetCandidateAccelerators() returned %d accelerators, want %d", len(candidates), tt.expectedCount)
			}

			for _, name := range tt.expectedNames {
				if _, exists := candidates[name]; !exists {
					t.Errorf("GetCandidateAccelerators() missing expected accelerator %s", name)
				}
			}
		})
	}
}

func TestServer_Calculate(t *testing.T) {
	// Setup a complete test system with performance data
	setupCompleteTestSystemForCalculate := func() {
		system := &System{
			accelerators:     make(map[string]*Accelerator),
			servers:          make(map[string]*Server),
			models:           make(map[string]*Model),
			serviceClasses:   make(map[string]*ServiceClass),
			capacity:         make(map[string]int32),
			allocationByType: make(map[string]*AllocationByType),
		}

		// Add test accelerator
		acc := NewAcceleratorFromSpec(&config.AcceleratorSpec{Name: "test-gpu", Cost: 100.0})
		system.accelerators["test-gpu"] = acc

		// Add test model with performance data
		model := NewModel("test-model")
		model.numInstances["test-gpu"] = 1
		// Add performance data for the model
		perfData := &config.ModelAcceleratorPerfData{
			Name:         "test-model",
			Acc:          "test-gpu",
			AccCount:     1,
			MaxBatchSize: 16,
			AtTokens:     200,
			DecodeParms: config.DecodeParms{
				Alpha: 5.0,
				Beta:  2.0,
			},
			PrefillParms: config.PrefillParms{
				Gamma: 10.0,
				Delta: 1.5,
			},
		}
		model.AddPerfDataFromSpec(perfData)
		system.models["test-model"] = model

		// Add test service class
		serviceClass := NewServiceClass("default", 5)
		target := &Target{TTFT: 2000.0, ITL: 500.0, TPS: 0.0}
		serviceClass.targets["test-model"] = target
		system.serviceClasses["default"] = serviceClass

		TheSystem = system
	}

	tests := []struct {
		name             string
		setupFunc        func()
		expectAllocs     bool
		withCurrentAlloc bool
	}{
		{
			name:         "calculate with complete system",
			setupFunc:    setupCompleteTestSystemForCalculate,
			expectAllocs: true,
		},
		{
			name:             "calculate with current allocation (covers transition penalty)",
			setupFunc:        setupCompleteTestSystemForCalculate,
			expectAllocs:     true,
			withCurrentAlloc: true,
		},
		{
			name: "calculate with empty system",
			setupFunc: func() {
				// Set up minimal empty system
				TheSystem = &System{
					accelerators:   make(map[string]*Accelerator),
					servers:        make(map[string]*Server),
					models:         make(map[string]*Model),
					serviceClasses: make(map[string]*ServiceClass),
				}
			},
			expectAllocs: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupFunc()

			spec := &config.ServerSpec{
				Name:  "test-server",
				Model: "test-model",
				Class: "default",
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{
						ArrivalRate:  60,
						AvgInTokens:  100,
						AvgOutTokens: 200,
					},
				},
			}

			// Setting up a current allocation
			if tt.withCurrentAlloc {
				spec.CurrentAlloc.Accelerator = "test-gpu"
				spec.CurrentAlloc.NumReplicas = 2
				spec.CurrentAlloc.MaxBatch = 8
				spec.CurrentAlloc.Cost = 150.0
			}

			server := NewServerFromSpec(spec)

			accelerators := map[string]*Accelerator{
				"test-gpu": NewAcceleratorFromSpec(&config.AcceleratorSpec{Name: "test-gpu", Cost: 100.0}),
			}

			// Add the server to the system if it exists
			if TheSystem != nil {
				TheSystem.servers["test-server"] = server
			}

			server.Calculate(accelerators)

			// Check that allAllocations map is initialized
			if server.AllAllocations() == nil {
				t.Error("Calculate() should initialize allAllocations map")
			}

			// Check if allocations were created as expected
			if tt.expectAllocs {
				if len(server.AllAllocations()) == 0 {
					t.Error("Calculate() should create allocations with complete system setup")
				}
				// If we have a current allocation, verify transition penalty was calculated
				if tt.withCurrentAlloc && len(server.AllAllocations()) > 0 {
					foundPenaltyApplied := false
					for _, alloc := range server.AllAllocations() {
						if alloc != nil {
							expectedPenalty := server.CurAllocation().TransitionPenalty(alloc)
							if alloc.Value() == expectedPenalty {
								foundPenaltyApplied = true
								break
							}
						}
					}
					if !foundPenaltyApplied {
						t.Error("Transition penalty was not applied to allocation values")
					}
				}
			}
		})
	}
}

func TestServer_Saturated(t *testing.T) {
	tests := []struct {
		name       string
		allocation *Allocation
		load       *config.ServerLoadSpec
		expected   bool
	}{
		{
			name:       "no allocation",
			allocation: nil,
			load:       &config.ServerLoadSpec{ArrivalRate: 60},
			expected:   false,
		},
		{
			name: "no load",
			allocation: &Allocation{
				accelerator:           "test-gpu",
				numReplicas:           1,
				maxArrvRatePerReplica: 100.0,
			},
			load:     nil,
			expected: false,
		},
		{
			name: "with allocation and load",
			allocation: &Allocation{
				accelerator:           "test-gpu",
				numReplicas:           1,
				maxArrvRatePerReplica: 0.001, // Very small rate to ensure saturation
			},
			load:     &config.ServerLoadSpec{ArrivalRate: 120}, // Higher than capacity
			expected: true,                                     // Should be saturated
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &config.ServerSpec{
				Name:  "test-server",
				Model: "test-model",
				Class: "default",
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{},
				},
			}
			server := NewServerFromSpec(spec)
			server.SetAllocation(tt.allocation)
			server.SetLoad(tt.load)

			result := server.Saturated()
			if result != tt.expected {
				t.Errorf("Saturated() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestServer_UpdateDesiredAlloc(t *testing.T) {
	spec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  60,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
	}
	server := NewServerFromSpec(spec)

	tests := []struct {
		name       string
		allocation *Allocation
		expectData bool
	}{
		{
			name:       "no allocation",
			allocation: nil,
			expectData: false,
		},
		{
			name: "with allocation",
			allocation: &Allocation{
				accelerator: "test-gpu",
				numReplicas: 2,
				batchSize:   16,
				cost:        100.0,
			},
			expectData: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server.SetAllocation(tt.allocation)
			server.UpdateDesiredAlloc()

			if tt.expectData {
				if server.Spec().DesiredAlloc.Accelerator == "" {
					t.Error("UpdateDesiredAlloc() should set DesiredAlloc data when allocation exists")
				}
				if server.Spec().DesiredAlloc.Load.ArrivalRate != server.Load().ArrivalRate {
					t.Error("UpdateDesiredAlloc() should copy current load to DesiredAlloc")
				}
			} else {
				if server.Spec().DesiredAlloc.Accelerator != "" {
					t.Error("UpdateDesiredAlloc() should clear DesiredAlloc when no allocation")
				}
			}
		})
	}
}

func TestServer_ApplyDesiredAlloc(t *testing.T) {
	spec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  60,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		DesiredAlloc: config.AllocationData{
			Accelerator: "new-gpu",
			NumReplicas: 3,
			MaxBatch:    32,
			Cost:        150.0,
			Load: config.ServerLoadSpec{
				ArrivalRate:  120,
				AvgInTokens:  150,
				AvgOutTokens: 300,
			},
		},
	}
	server := NewServerFromSpec(spec)

	server.ApplyDesiredAlloc()

	// Check that CurrentAlloc was updated from DesiredAlloc
	if server.Spec().CurrentAlloc.Accelerator != "new-gpu" {
		t.Error("ApplyDesiredAlloc() should copy DesiredAlloc to CurrentAlloc")
	}
	if server.Spec().CurrentAlloc.NumReplicas != 3 {
		t.Error("ApplyDesiredAlloc() should copy replica count")
	}

	// Check that current allocation object was updated
	if server.CurAllocation() == nil {
		t.Error("ApplyDesiredAlloc() should update current allocation object")
	}

	// Check that load was updated
	if server.Load().ArrivalRate != 120 {
		t.Error("ApplyDesiredAlloc() should update server load")
	}
}

func TestServer_String(t *testing.T) {
	spec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  60,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
	}
	server := NewServerFromSpec(spec)

	result := server.String()

	// Check that string contains key information
	if !strings.Contains(result, "test-server") {
		t.Error("String() should contain server name")
	}
	if !strings.Contains(result, "test-model") {
		t.Error("String() should contain model name")
	}
	if !strings.Contains(result, "default") {
		t.Error("String() should contain service class name")
	}
}
