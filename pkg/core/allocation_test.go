package core

import (
	"strings"
	"testing"

	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
)

// Helper function to setup a complete test system
func setupCompleteTestSystem() {
	system := &System{
		accelerators:     make(map[string]*Accelerator),
		servers:          make(map[string]*Server),
		models:           make(map[string]*Model),
		serviceClasses:   make(map[string]*ServiceClass),
		capacity:         make(map[string]int32),
		allocationByType: make(map[string]*AllocationByType),
	}

	// Add test accelerator
	accSpec := &config.AcceleratorSpec{
		Name: "test-gpu",
		Cost: 100.0,
	}
	acc := NewAcceleratorFromSpec(accSpec)
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

	// Add test server
	serverSpec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  0, // Zero load for testing
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		MinNumReplicas: 1,
	}
	server := NewServerFromSpec(serverSpec)
	system.servers["test-server"] = server

	// Add test service class
	serviceClass := NewServiceClass("default", 10)
	target := &Target{
		TTFT: 100.0,
		ITL:  50.0,
		TPS:  0.0,
	}
	serviceClass.targets["test-model"] = target
	system.serviceClasses["default"] = serviceClass

	// Set global system
	TheSystem = system
}

func TestAllocation_Getters(t *testing.T) {
	// Setup system and create allocation using CreateAllocation
	setupCompleteTestSystem()
	alloc := CreateAllocation("test-server", "test-gpu")
	if alloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}

	tests := []struct {
		name     string
		getter   func() any
		expected any
	}{
		{
			name:     "Accelerator",
			getter:   func() any { return alloc.Accelerator() },
			expected: "test-gpu",
		},
		{
			name:     "NumReplicas",
			getter:   func() any { return alloc.NumReplicas() },
			expected: int32(1),
		},
		{
			name:     "MaxBatchSize",
			getter:   func() any { return alloc.MaxBatchSize() },
			expected: 16,
		},
		{
			name:     "Cost",
			getter:   func() any { return alloc.Cost() },
			expected: float32(100.0),
		},
		{
			name:     "Value",
			getter:   func() any { return alloc.Value() },
			expected: float32(100.0),
		},
		{
			name:     "MaxArrvRatePerReplica",
			getter:   func() any { return alloc.MaxArrvRatePerReplica() },
			expected: float32(0.3298969),
		},
		{
			name:     "MaxRPM",
			getter:   func() any { return alloc.MaxRPM() },
			expected: float32(19793.814),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.getter()
			if got != tt.expected {
				t.Errorf("%s() = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestAllocation_Setters(t *testing.T) {
	// Setup system and create allocation using CreateAllocation
	setupCompleteTestSystem()
	alloc := CreateAllocation("test-server", "test-gpu")
	if alloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}

	tests := []struct {
		name     string
		setter   func()
		getter   func() any
		expected any
	}{
		{
			name:     "SetNumReplicas",
			setter:   func() { alloc.SetNumReplicas(5) },
			getter:   func() any { return alloc.NumReplicas() },
			expected: int32(5),
		},
		{
			name:     "SetMaxBatchSize",
			setter:   func() { alloc.SetMaxBatchSize(16) },
			getter:   func() any { return alloc.MaxBatchSize() },
			expected: 16,
		},
		{
			name:     "SetCost",
			setter:   func() { alloc.SetCost(250.0) },
			getter:   func() any { return alloc.Cost() },
			expected: float32(250.0),
		},
		{
			name:     "SetValue",
			setter:   func() { alloc.SetValue(300.0) },
			getter:   func() any { return alloc.Value() },
			expected: float32(300.0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setter()
			got := tt.getter()
			if got != tt.expected {
				t.Errorf("After %s, got %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestAllocation_Saturated(t *testing.T) {
	// Setup system and create allocation using CreateAllocation
	setupCompleteTestSystem()
	alloc := CreateAllocation("test-server", "test-gpu")
	if alloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}

	tests := []struct {
		name      string
		totalRate float32
		want      bool
	}{
		{
			name:      "below saturation",
			totalRate: 15000.0,
			want:      false,
		},
		{
			name:      "at saturation",
			totalRate: 19794.0, // Clearly above MaxRPM
			want:      true,
		},
		{
			name:      "above saturation",
			totalRate: 25000.0,
			want:      true,
		},
		{
			name:      "zero rate",
			totalRate: 0.0,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := alloc.Saturated(tt.totalRate)
			if got != tt.want {
				t.Errorf("Allocation.Saturated(%v) = %v, want %v", tt.totalRate, got, tt.want)
			}
		})
	}
}

func TestAllocation_TransitionPenalty(t *testing.T) {
	allocA := &Allocation{
		accelerator: "gpu-a",
		numReplicas: 2,
		cost:        100.0,
	}

	tests := []struct {
		name   string
		allocB *Allocation
		want   float32
	}{
		{
			name: "same accelerator same replicas",
			allocB: &Allocation{
				accelerator: "gpu-a",
				numReplicas: 2,
				cost:        100.0,
			},
			want: 0.0,
		},
		{
			name: "same accelerator different replicas",
			allocB: &Allocation{
				accelerator: "gpu-a",
				numReplicas: 3,
				cost:        150.0,
			},
			want: 50.0, // cost difference
		},
		{
			name: "different accelerator",
			allocB: &Allocation{
				accelerator: "gpu-b",
				numReplicas: 2,
				cost:        120.0,
			},
			want: config.AccelPenaltyFactor*(100.0+120.0) + (120.0 - 100.0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allocA.TransitionPenalty(tt.allocB)
			if got != tt.want {
				t.Errorf("TransitionPenalty() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllocation_Clone(t *testing.T) {
	// Setup system and create allocation using CreateAllocation
	setupCompleteTestSystem()
	original := CreateAllocation("test-server", "test-gpu")
	if original == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}
	cloned := original.Clone()

	// Verify all fields are copied
	if cloned.accelerator != original.accelerator {
		t.Errorf("Clone accelerator = %v, want %v", cloned.accelerator, original.accelerator)
	}
	if cloned.numReplicas != original.numReplicas {
		t.Errorf("Clone numReplicas = %v, want %v", cloned.numReplicas, original.numReplicas)
	}
	if cloned.batchSize != original.batchSize {
		t.Errorf("Clone batchSize = %v, want %v", cloned.batchSize, original.batchSize)
	}
	if cloned.cost != original.cost {
		t.Errorf("Clone cost = %v, want %v", cloned.cost, original.cost)
	}
	if cloned.value != original.value {
		t.Errorf("Clone value = %v, want %v", cloned.value, original.value)
	}

	// Verify the cloned copy is a different reference
	if cloned == original {
		t.Error("Clone returned same reference instead of new reference")
	}
	// Verify modifying cloned copy doesn't affect the original copy
	cloned.SetNumReplicas(5)
	if original.NumReplicas() == 5 {
		t.Error("Modifying clone affected original")
	}
}

func TestAllocation_AllocationData(t *testing.T) {
	// Setup system and create allocation using CreateAllocation
	setupCompleteTestSystem()
	alloc := CreateAllocation("test-server", "test-gpu")
	if alloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}
	data := alloc.AllocationData()

	if data.Accelerator != alloc.accelerator {
		t.Errorf("AllocationData.Accelerator = %v, want %v", data.Accelerator, alloc.accelerator)
	}
	if data.NumReplicas != alloc.numReplicas {
		t.Errorf("AllocationData.NumReplicas = %v, want %v", data.NumReplicas, alloc.numReplicas)
	}
	if data.MaxBatch != alloc.batchSize {
		t.Errorf("AllocationData.MaxBatch = %v, want %v", data.MaxBatch, alloc.batchSize)
	}
	if data.Cost != alloc.cost {
		t.Errorf("AllocationData.Cost = %v, want %v", data.Cost, alloc.cost)
	}
	if data.ITLAverage != alloc.itl {
		t.Errorf("AllocationData.ITLAverage = %v, want %v", data.ITLAverage, alloc.itl)
	}
	if data.TTFTAverage != alloc.ttft {
		t.Errorf("AllocationData.TTFTAverage = %v, want %v", data.TTFTAverage, alloc.ttft)
	}
}

func TestAllocationFromData(t *testing.T) {
	data := &config.AllocationData{
		Accelerator: "test-gpu",
		NumReplicas: 3,
		MaxBatch:    16,
		Cost:        200.0,
		ITLAverage:  15.5,
		TTFTAverage: 30.0,
	}

	alloc := AllocationFromData(data)

	if alloc.accelerator != data.Accelerator {
		t.Errorf("AllocationFromData accelerator = %v, want %v", alloc.accelerator, data.Accelerator)
	}
	if alloc.numReplicas != data.NumReplicas {
		t.Errorf("AllocationFromData numReplicas = %v, want %v", alloc.numReplicas, data.NumReplicas)
	}
	if alloc.batchSize != data.MaxBatch {
		t.Errorf("AllocationFromData batchSize = %v, want %v", alloc.batchSize, data.MaxBatch)
	}
	if alloc.cost != data.Cost {
		t.Errorf("AllocationFromData cost = %v, want %v", alloc.cost, data.Cost)
	}
	if alloc.itl != data.ITLAverage {
		t.Errorf("AllocationFromData itl = %v, want %v", alloc.itl, data.ITLAverage)
	}
	if alloc.ttft != data.TTFTAverage {
		t.Errorf("AllocationFromData ttft = %v, want %v", alloc.ttft, data.TTFTAverage)
	}
}

func TestAllocation_String(t *testing.T) {
	// Setup system and create allocation using CreateAllocation
	setupCompleteTestSystem()
	alloc := CreateAllocation("test-server", "test-gpu")
	if alloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}
	str := alloc.String()

	// Verify string contains key information
	expectedSubstrings := []string{
		"test-gpu",    // accelerator name
		"numRep=1",    // num replicas (from actual CreateAllocation)
		"maxBatch=16", // batch size (from actual CreateAllocation)
		"cost=100",    // cost
		"val=100",     // value
	}

	for _, substr := range expectedSubstrings {
		if !strings.Contains(str, substr) {
			t.Errorf("String() = %v, should contain %v", str, substr)
		}
	}
}

func TestCreateAllocationDiff(t *testing.T) {
	// Setup system and create allocations using CreateAllocation
	setupCompleteTestSystem()
	testAlloc := CreateAllocation("test-server", "test-gpu")
	if testAlloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}

	tests := []struct {
		name    string
		a       *Allocation
		b       *Allocation
		wantNil bool
	}{
		{
			name:    "both nil",
			a:       nil,
			b:       nil,
			wantNil: true,
		},
		{
			name:    "a nil, b not nil",
			a:       nil,
			b:       testAlloc,
			wantNil: false,
		},
		{
			name:    "a not nil, b nil",
			a:       testAlloc,
			b:       nil,
			wantNil: false,
		},
		{
			name:    "both not nil",
			a:       testAlloc,
			b:       testAlloc,
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff := CreateAllocationDiff(tt.a, tt.b)
			if (diff == nil) != tt.wantNil {
				t.Errorf("CreateAllocationDiff() = %v, wantNil %v", diff, tt.wantNil)
			}
		})
	}
}

func TestAllocationDiff_Content(t *testing.T) {
	allocA := &Allocation{
		accelerator: "gpu-a",
		numReplicas: 2,
		cost:        100.0,
	}

	allocB := &Allocation{
		accelerator: "gpu-b",
		numReplicas: 3,
		cost:        150.0,
	}

	diff := CreateAllocationDiff(allocA, allocB)

	if diff.oldAccelerator != "gpu-a" {
		t.Errorf("oldAccelerator = %v, want gpu-a", diff.oldAccelerator)
	}
	if diff.newAccelerator != "gpu-b" {
		t.Errorf("newAccelerator = %v, want gpu-b", diff.newAccelerator)
	}
	if diff.oldNumReplicas != 2 {
		t.Errorf("oldNumReplicas = %v, want 2", diff.oldNumReplicas)
	}
	if diff.newNumReplicas != 3 {
		t.Errorf("newNumReplicas = %v, want 3", diff.newNumReplicas)
	}
	if diff.costDiff != 50.0 {
		t.Errorf("costDiff = %v, want 50.0", diff.costDiff)
	}
}

func TestAllocationDiff_String(t *testing.T) {
	allocA := &Allocation{
		accelerator: "gpu-a",
		numReplicas: 2,
		cost:        100.0,
	}

	allocB := &Allocation{
		accelerator: "gpu-b",
		numReplicas: 3,
		cost:        150.0,
	}

	diff := CreateAllocationDiff(allocA, allocB)
	str := diff.String()

	expectedSubstrings := []string{
		"gpu-a -> gpu-b",
		"2 -> 3",
		"50",
	}

	for _, substr := range expectedSubstrings {
		if !strings.Contains(str, substr) {
			t.Errorf("String() = %v, should contain %v", str, substr)
		}
	}
}

func TestAllocationDiff_NilHandling(t *testing.T) {
	// Setup system and create allocation using CreateAllocation
	setupCompleteTestSystem()
	testAlloc := CreateAllocation("test-server", "test-gpu")
	if testAlloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}

	tests := []struct {
		name            string
		a               *Allocation
		b               *Allocation
		wantOldAcc      string
		wantNewAcc      string
		wantOldReplicas int32
		wantNewReplicas int32
	}{
		{
			name:            "nil to allocation",
			a:               nil,
			b:               testAlloc,
			wantOldAcc:      "none",
			wantNewAcc:      "test-gpu",
			wantOldReplicas: 0,
			wantNewReplicas: 1, // Updated from CreateAllocation result
		},
		{
			name:            "allocation to nil",
			a:               testAlloc,
			b:               nil,
			wantOldAcc:      "test-gpu",
			wantNewAcc:      "none",
			wantOldReplicas: 1, // Updated from CreateAllocation result
			wantNewReplicas: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff := CreateAllocationDiff(tt.a, tt.b)
			if diff.oldAccelerator != tt.wantOldAcc {
				t.Errorf("oldAccelerator = %v, want %v", diff.oldAccelerator, tt.wantOldAcc)
			}
			if diff.newAccelerator != tt.wantNewAcc {
				t.Errorf("newAccelerator = %v, want %v", diff.newAccelerator, tt.wantNewAcc)
			}
			if diff.oldNumReplicas != tt.wantOldReplicas {
				t.Errorf("oldNumReplicas = %v, want %v", diff.oldNumReplicas, tt.wantOldReplicas)
			}
			if diff.newNumReplicas != tt.wantNewReplicas {
				t.Errorf("newNumReplicas = %v, want %v", diff.newNumReplicas, tt.wantNewReplicas)
			}
		})
	}
}

func TestCreateAllocation(t *testing.T) {

	tests := []struct {
		name       string
		serverName string
		gName      string
		setupFunc  func() // Custom setup for specific test cases
		wantNil    bool
	}{
		{
			name:       "nonexistent accelerator",
			serverName: "test-server",
			gName:      "nonexistent-gpu",
			wantNil:    true,
		},
		{
			name:       "nonexistent server",
			serverName: "nonexistent-server",
			gName:      "test-gpu",
			wantNil:    true,
		},
		{
			name:       "both nonexistent",
			serverName: "nonexistent-server",
			gName:      "nonexistent-gpu",
			wantNil:    true,
		},
		{
			name:       "zero load case",
			serverName: "test-server",
			gName:      "test-gpu",
			wantNil:    false, // Should succeed with zero load allocation
		},
		{
			name:       "server with no performance data",
			serverName: "test-server",
			gName:      "test-gpu",
			setupFunc: func() {
				setupCompleteTestSystem()
				// Remove performance data from model
				if model, exists := TheSystem.models["test-model"]; exists {
					model.perfData = make(map[string]*config.ModelAcceleratorPerfData)
				}
			},
			wantNil: true,
		},
		{
			name:       "model with no service class target",
			serverName: "test-server",
			gName:      "test-gpu",
			setupFunc: func() {
				setupCompleteTestSystem()
				// Remove target from service class
				if svc, exists := TheSystem.serviceClasses["default"]; exists {
					svc.targets = make(map[string]*Target)
				}
			},
			wantNil: true,
		},
		{
			name:       "server with invalid performance targets",
			serverName: "test-server",
			gName:      "test-gpu",
			setupFunc: func() {
				setupCompleteTestSystem()
				// Set parameters that might cause queue analyzer to fail
				if server, exists := TheSystem.servers["test-server"]; exists {
					server.load = &config.ServerLoadSpec{
						ArrivalRate:  1200, // Very high arrival rate
						AvgInTokens:  100,
						AvgOutTokens: 200,
					}
				}
				// Set very strict performance targets
				if svc, exists := TheSystem.serviceClasses["default"]; exists {
					if target, exists := svc.targets["test-model"]; exists {
						target.TTFT = 1.0 // Very strict TTFT
						target.ITL = 0.1  // Very strict ITL
						target.TPS = 0.0
					}
				}
			},
			wantNil: true,
		},
		{
			name:       "server with non-zero TPS target (covers TPS branch)",
			serverName: "test-server",
			gName:      "test-gpu",
			setupFunc: func() {
				setupCompleteTestSystem()
				// Set reasonable arrival rate for non-zero load
				if server, exists := TheSystem.servers["test-server"]; exists {
					server.load = &config.ServerLoadSpec{
						ArrivalRate:  60, // 1 req/second
						AvgInTokens:  100,
						AvgOutTokens: 200,
					}
				}
				// Set non-zero TPS to test that branch
				if svc, exists := TheSystem.serviceClasses["default"]; exists {
					if target, exists := svc.targets["test-model"]; exists {
						target.TTFT = 2000.0
						target.ITL = 500.0
						target.TPS = 2.0
					}
				}
			},
			wantNil: false, // Should succeed
		},
		{
			name:       "server with arrival rate only (covers arrival rate branch)",
			serverName: "test-server",
			gName:      "test-gpu",
			setupFunc: func() {
				setupCompleteTestSystem()
				// Set non-zero arrival rate
				if server, exists := TheSystem.servers["test-server"]; exists {
					server.load = &config.ServerLoadSpec{
						ArrivalRate:  120, // 2 req/second
						AvgInTokens:  100,
						AvgOutTokens: 200,
					}
				}
				// Keep TPS = 0 to test arrival rate branch
				if svc, exists := TheSystem.serviceClasses["default"]; exists {
					if target, exists := svc.targets["test-model"]; exists {
						target.TTFT = 2000.0
						target.ITL = 500.0
						target.TPS = 0.0 // Zero TPS
					}
				}
			},
			wantNil: false, // Should succeed
		},
		{
			name:       "server with custom max batch size override",
			serverName: "test-server",
			gName:      "test-gpu",
			setupFunc: func() {
				setupCompleteTestSystem()
				// Set non-zero arrival rate
				if server, exists := TheSystem.servers["test-server"]; exists {
					server.load = &config.ServerLoadSpec{
						ArrivalRate:  60,
						AvgInTokens:  100,
						AvgOutTokens: 200,
					}
					server.maxBatchSize = 12 // Override max batch size
				}
				if svc, exists := TheSystem.serviceClasses["default"]; exists {
					if target, exists := svc.targets["test-model"]; exists {
						target.TTFT = 2000.0
						target.ITL = 500.0
						target.TPS = 0.0
					}
				}
			},
			wantNil: false, // Should succeed and use custom batch size
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset system before each test
			TheSystem = nil

			// Setup system with complete test data
			if tt.setupFunc != nil {
				tt.setupFunc()
			} else {
				setupCompleteTestSystem()
			}

			alloc := CreateAllocation(tt.serverName, tt.gName)
			if (alloc == nil) != tt.wantNil {
				t.Errorf("CreateAllocation() = %v, wantNil %v", alloc, tt.wantNil)
			}

			// Verify fields
			if alloc != nil {
				if alloc.accelerator == "" && alloc.numReplicas == 0 {
					// Zero load allocation case
					if alloc.cost != 0 {
						t.Errorf("Zero load allocation cost = %v, want 0", alloc.cost)
					}
				} else {
					// Normal allocation case
					if alloc.accelerator != tt.gName {
						t.Errorf("allocation accelerator = %v, want %v", alloc.accelerator, tt.gName)
					}
					if alloc.numReplicas <= 0 {
						t.Errorf("allocation numReplicas = %v, want > 0", alloc.numReplicas)
					}
				}
			}
		})
	}
}

func TestAllocation_Scale(t *testing.T) {
	// Setup system and create allocation using CreateAllocation
	setupCompleteTestSystem()
	alloc := CreateAllocation("test-server", "test-gpu")
	if alloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}

	tests := []struct {
		name       string
		serverName string
		setupFunc  func() // Custom setup for scaling scenarios
		wantAlloc  bool
		wantInc    int32
	}{
		{
			name:       "nonexistent server",
			serverName: "nonexistent-server",
			wantAlloc:  false,
			wantInc:    0,
		},
		{
			name:       "valid server scale with no change needed",
			serverName: "test-server",
			wantAlloc:  true, // Scale should succeed with test system
			wantInc:    0,    // no scaling needed
		},
		{
			name:       "valid server requiring scale up (inc > 0)",
			serverName: "test-server",
			setupFunc: func() {
				setupCompleteTestSystem()
				// First, set up a low load so the original allocation has minimal replicas
				if server, exists := TheSystem.servers["test-server"]; exists {
					server.load = &config.ServerLoadSpec{
						ArrivalRate:  30, // Low initial load (req/min)
						AvgInTokens:  100,
						AvgOutTokens: 200,
					}
				}
				// Set lenient performance targets
				if svc, exists := TheSystem.serviceClasses["default"]; exists {
					if target, exists := svc.targets["test-model"]; exists {
						target.TTFT = 2000.0
						target.ITL = 500.0
						target.TPS = 0.0
					}
				}
			},
			wantAlloc: true, // Should succeed with scaling up
			wantInc:   1,    // Expecting scale up
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			TheSystem = nil

			// Setup for each test
			if tt.setupFunc != nil {
				tt.setupFunc()
			} else {
				setupCompleteTestSystem()
			}

			// Create initial allocation with the current setup
			origAlloc := CreateAllocation(tt.serverName, "test-gpu")
			if origAlloc == nil && tt.name == "valid server requiring scale up (inc > 0)" {
				t.Fatal("Failed to create initial allocation for scale up test")
			}

			// For scale up test, now increase the load after creating initial allocation
			if tt.name == "valid server requiring scale up (inc > 0)" {
				if server, exists := TheSystem.servers["test-server"]; exists {
					server.load = &config.ServerLoadSpec{
						ArrivalRate:  360, // higher load
						AvgInTokens:  100,
						AvgOutTokens: 200,
					}
				}
				// Use the initial allocation for scaling
				alloc = origAlloc
			}

			newAlloc, inc := alloc.Scale(tt.serverName)

			if (newAlloc != nil) != tt.wantAlloc {
				t.Errorf("Scale() alloc = %v, wantAlloc %v", newAlloc, tt.wantAlloc)
			}

			// Check increment value - for scale up test, we expect positive increment
			if tt.name == "valid server requiring scale up (inc > 0)" {
				if inc <= 0 {
					t.Errorf("Scale() inc = %v, want positive value (> 0)", inc)
				}
			} else {
				if inc != tt.wantInc {
					t.Errorf("Scale() inc = %v, want %v", inc, tt.wantInc)
				}
			}

			if newAlloc != nil {
				expectedInc := newAlloc.numReplicas - alloc.numReplicas
				if inc != expectedInc {
					t.Errorf("Scale() inc = %v, but expected %v based on replica difference", inc, expectedInc)
				}
			}
		})
	}
}

func TestAllocation_ReAllocate(t *testing.T) {
	// Setup system with multiple accelerators for reallocation
	setupReAllocateTestSystem := func() {
		setupCompleteTestSystem()

		// Add additional accelerators for reallocation testing
		gpuSpecs := []*config.AcceleratorSpec{
			{Name: "gpu-a", Cost: 100.0},
			{Name: "gpu-b", Cost: 150.0},
			{Name: "gpu-c", Cost: 80.0},
		}

		for _, spec := range gpuSpecs {
			acc := NewAcceleratorFromSpec(spec)
			TheSystem.accelerators[spec.Name] = acc
		}

		// Update test model to work with all accelerators
		if model, exists := TheSystem.models["test-model"]; exists {
			model.numInstances["gpu-a"] = 1
			model.numInstances["gpu-b"] = 1
			model.numInstances["gpu-c"] = 2
		}
	}

	// Create allocation using CreateAllocation
	setupReAllocateTestSystem()
	alloc := CreateAllocation("test-server", "test-gpu")
	if alloc == nil {
		t.Fatal("CreateAllocation returned nil, setup may be incorrect")
	}

	tests := []struct {
		name       string
		serverName string
		wantAlloc  bool
		wantGName  string
	}{
		{
			name:       "nonexistent server",
			serverName: "nonexistent-server",
			wantAlloc:  false,
			wantGName:  "",
		},
		{
			name:       "valid server with multiple accelerators",
			serverName: "test-server",
			wantAlloc:  true,       // Should find an allocation
			wantGName:  "test-gpu", // ReAllocate returns the original accelerator from the system
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset system before each test
			TheSystem = nil

			// Setup system with multiple accelerators for reallocation
			setupReAllocateTestSystem()

			newAlloc, gName := alloc.ReAllocate(tt.serverName)

			if (newAlloc != nil) != tt.wantAlloc {
				t.Errorf("ReAllocate() alloc = %v, wantAlloc %v", newAlloc, tt.wantAlloc)
			}
			if gName != tt.wantGName {
				t.Errorf("ReAllocate() gName = %v, want %v", gName, tt.wantGName)
			}

			// If reallocation succeeded, verify the allocation properties
			if newAlloc != nil {
				if newAlloc.accelerator != gName {
					t.Errorf("ReAllocate() allocation accelerator = %v, want %v", newAlloc.accelerator, gName)
				}
				if newAlloc.value <= 0 {
					t.Errorf("ReAllocate() allocation value = %v, want > 0", newAlloc.value)
				}
			}
		})
	}
}

func TestZeroLoadAllocation(t *testing.T) {
	tests := []struct {
		name          string
		server        *Server
		model         *Model
		acc           *Accelerator
		perf          *config.ModelAcceleratorPerfData
		wantAccel     string
		wantReplicas  int32
		wantBatchSize int
		wantCost      float32
	}{
		{
			name: "zero replicas",
			server: &Server{
				minNumReplicas: 0,
				maxBatchSize:   0,
			},
			model: &Model{
				name:         "test-model",
				numInstances: map[string]int32{},
			},
			acc: &Accelerator{
				name: "test-gpu",
				spec: &config.AcceleratorSpec{
					Cost: 100.0,
				},
			},
			perf: &config.ModelAcceleratorPerfData{
				MaxBatchSize: 16,
				DecodeParms: config.DecodeParms{
					Alpha: 5.0,
					Beta:  2.0,
				},
				PrefillParms: config.PrefillParms{
					Gamma: 10.0,
					Delta: 1.5,
				},
			},
			wantAccel:     "",
			wantReplicas:  0,
			wantBatchSize: 0,
			wantCost:      0.0,
		},
		{
			name: "normal case with min replicas",
			server: &Server{
				minNumReplicas: 2,
				maxBatchSize:   0,
			},
			model: &Model{
				name: "test-model",
				numInstances: map[string]int32{
					"test-gpu": 1,
				},
			},
			acc: &Accelerator{
				name: "test-gpu",
				spec: &config.AcceleratorSpec{
					Cost: 100.0,
				},
			},
			perf: &config.ModelAcceleratorPerfData{
				MaxBatchSize: 16,
				DecodeParms: config.DecodeParms{
					Alpha: 5.0,
					Beta:  2.0,
				},
				PrefillParms: config.PrefillParms{
					Gamma: 10.0,
					Delta: 1.5,
				},
			},
			wantAccel:     "test-gpu",
			wantReplicas:  2,
			wantBatchSize: 16,
			wantCost:      200.0, // 100 * 1 instance * 2 replicas
		},
		{
			name: "with server max batch size override",
			server: &Server{
				minNumReplicas: 1,
				maxBatchSize:   8,
			},
			model: &Model{
				name: "test-model",
				numInstances: map[string]int32{
					"test-gpu": 2,
				},
			},
			acc: &Accelerator{
				name: "test-gpu",
				spec: &config.AcceleratorSpec{
					Cost: 50.0,
				},
			},
			perf: &config.ModelAcceleratorPerfData{
				MaxBatchSize: 16, // Should be overridden by server.maxBatchSize
				DecodeParms: config.DecodeParms{
					Alpha: 3.0,
					Beta:  1.0,
				},
				PrefillParms: config.PrefillParms{
					Gamma: 8.0,
					Delta: 2.0,
				},
			},
			wantAccel:     "test-gpu",
			wantReplicas:  1,
			wantBatchSize: 8,     // Override from server
			wantCost:      100.0, // 50 * 2 instances * 1 replica
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alloc := zeroLoadAllocation(tt.server, tt.model, tt.acc, tt.perf)

			if alloc == nil {
				t.Fatal("zeroLoadAllocation() returned nil")
			}

			if alloc.accelerator != tt.wantAccel {
				t.Errorf("accelerator = %v, want %v", alloc.accelerator, tt.wantAccel)
			}
			if alloc.numReplicas != tt.wantReplicas {
				t.Errorf("numReplicas = %v, want %v", alloc.numReplicas, tt.wantReplicas)
			}
			if alloc.batchSize != tt.wantBatchSize {
				t.Errorf("batchSize = %v, want %v", alloc.batchSize, tt.wantBatchSize)
			}
			if alloc.cost != tt.wantCost {
				t.Errorf("cost = %v, want %v", alloc.cost, tt.wantCost)
			}

			// Verify that value is set to cost
			if alloc.value != alloc.cost {
				t.Errorf("value = %v, want %v (should equal cost)", alloc.value, alloc.cost)
			}

			// For zero load allocation, rho should be 0
			if alloc.rho != 0 {
				t.Errorf("rho = %v, want 0", alloc.rho)
			}

			// Verify performance metrics are calculated correctly for non-zero replicas
			if tt.wantReplicas > 0 {
				expectedDecodeTime := tt.perf.DecodeParms.Alpha + tt.perf.DecodeParms.Beta
				if alloc.itl != expectedDecodeTime {
					t.Errorf("itl = %v, want %v", alloc.itl, expectedDecodeTime)
				}

				expectedPrefillTime := tt.perf.PrefillParms.Gamma + tt.perf.PrefillParms.Delta
				if alloc.ttft != expectedPrefillTime {
					t.Errorf("ttft = %v, want %v", alloc.ttft, expectedPrefillTime)
				}

				// Verify maxArrvRatePerReplica calculation
				maxDecodeTime := tt.perf.DecodeParms.Alpha + tt.perf.DecodeParms.Beta*float32(alloc.batchSize)
				maxServTime := expectedPrefillTime + maxDecodeTime
				expectedMaxRate := float32(alloc.batchSize) / maxServTime
				if alloc.maxArrvRatePerReplica != expectedMaxRate {
					t.Errorf("maxArrvRatePerReplica = %v, want %v", alloc.maxArrvRatePerReplica, expectedMaxRate)
				}
			}
		})
	}
}

func TestZeroLoadAllocation_EdgeCases(t *testing.T) {
	tests := []struct {
		name   string
		server *Server
		model  *Model
		acc    *Accelerator
		perf   *config.ModelAcceleratorPerfData
	}{
		{
			name: "minimal valid inputs",
			server: &Server{
				minNumReplicas: 1,
			},
			model: &Model{
				numInstances: make(map[string]int32),
			},
			acc: &Accelerator{
				name: "test-gpu",
				spec: &config.AcceleratorSpec{
					Cost: 0.0,
				},
			},
			perf: &config.ModelAcceleratorPerfData{
				MaxBatchSize: 1,
				DecodeParms: config.DecodeParms{
					Alpha: 0.1,
					Beta:  0.1,
				},
				PrefillParms: config.PrefillParms{
					Gamma: 0.1,
					Delta: 0.1,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alloc := zeroLoadAllocation(tt.server, tt.model, tt.acc, tt.perf)
			if alloc == nil {
				t.Error("zeroLoadAllocation() returned nil unexpectedly")
			}
		})
	}
}
