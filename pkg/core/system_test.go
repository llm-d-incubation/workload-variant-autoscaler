package core

import (
	"strings"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/config"
)

const (
	testAccTypeGPUA100   = "GPU_A100"
	testModelNameLlama7b = "llama-7b"
	testSvcClassDefault  = "default"
)

func TestNewSystem(t *testing.T) {
	system := NewSystem()

	if system == nil {
		t.Fatal("NewSystem() returned nil")
	}

	if system.accelerators == nil {
		t.Error("accelerators map should be initialized")
	}

	if system.models == nil {
		t.Error("models map should be initialized")
	}

	if system.serviceClasses == nil {
		t.Error("serviceClasses map should be initialized")
	}

	if system.servers == nil {
		t.Error("servers map should be initialized")
	}

	if system.capacity == nil {
		t.Error("capacity map should be initialized")
	}

	if system.allocationByType == nil {
		t.Error("allocationByType map should be initialized")
	}
}

func TestSystem_SetFromSpec(t *testing.T) {
	system := NewSystem()

	spec := &config.SystemSpec{
		Accelerators: config.AcceleratorData{
			Spec: []config.AcceleratorSpec{
				{
					Name: "A100",
					Type: "GPU_A100",
					Power: config.PowerSpec{
						Idle:     50,
						MidPower: 150,
						Full:     350,
						MidUtil:  0.4,
					},
					Cost:         1.0,
					Multiplicity: 1,
					MemSize:      40,
				},
			},
		},
		Models: config.ModelData{
			PerfData: []config.ModelAcceleratorPerfData{
				{
					Name:         testModelNameLlama7b,
					Acc:          "A100",
					AccCount:     1,
					MaxBatchSize: 16,
					AtTokens:     100,
					ServiceParms: config.ServiceParms{
						Alpha: 10.0,
						Beta:  2.0,
						Gamma: 0.1,
					},
				},
			},
		},
		Capacity: config.CapacityData{
			Count: []config.AcceleratorCount{
				{
					Type:  "GPU_A100",
					Count: 4,
				},
			},
		},
		Servers: config.ServerData{
			Spec: []config.ServerSpec{
				{
					Name:  "server1",
					Model: testModelNameLlama7b,
					Class: "default",
					CurrentAlloc: config.AllocationData{
						Load: config.ServerLoadSpec{
							ArrivalRate:  30,
							AvgInTokens:  100,
							AvgOutTokens: 200,
						},
					},
					MinNumReplicas: 1,
					MaxBatchSize:   16,
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
							Model:    testModelNameLlama7b,
							SLO_ITL:  100,
							SLO_TTFT: 1000,
							SLO_TPS:  50,
						},
					},
				},
			},
		},
		Optimizer: config.OptimizerData{
			Spec: config.OptimizerSpec{
				Unlimited:         false,
				SaturationPolicy:  "None",
				DelayedBestEffort: false,
			},
		},
	}

	optimizerSpec := system.SetFromSpec(spec)

	// Check that components were created with correct counts
	if len(system.accelerators) != 1 {
		t.Errorf("Expected 1 accelerator, got %d", len(system.accelerators))
	}

	if len(system.models) != 1 {
		t.Errorf("Expected 1 model, got %d", len(system.models))
	}

	if len(system.servers) != 1 {
		t.Errorf("Expected 1 server, got %d", len(system.servers))
	}

	if len(system.serviceClasses) != 1 {
		t.Errorf("Expected 1 service class, got %d", len(system.serviceClasses))
	}

	if len(system.capacity) != 1 {
		t.Errorf("Expected 1 capacity entry, got %d", len(system.capacity))
	}

	if optimizerSpec == nil {
		t.Fatal("Optimizer spec should be returned")
	}

	// Validate accelerator properties
	acc := system.accelerators["A100"]
	if acc == nil {
		t.Fatal("A100 accelerator should exist")
	}
	if acc.Type() != testAccTypeGPUA100 {
		t.Errorf("Expected accelerator type GPU_A100, got %s", acc.Type())
	}

	// Validate model properties
	model := system.models[testModelNameLlama7b]
	if model == nil {
		t.Fatal("llama-7b model should exist")
	}
	if model.Name() != testModelNameLlama7b {
		t.Errorf("Expected model name llama-7b, got %s", model.Name())
	}

	// Validate server properties
	server := system.servers["server1"]
	if server == nil {
		t.Fatal("server1 should exist")
	}
	if server.Name() != "server1" {
		t.Errorf("Expected server name server1, got %s", server.Name())
	}
	if server.ModelName() != testModelNameLlama7b {
		t.Errorf("Expected server model llama-7b, got %s", server.ModelName())
	}

	// Validate service class properties
	svcClass := system.serviceClasses["default"]
	if svcClass == nil {
		t.Fatal("default service class should exist")
	}
	if svcClass.Name() != testSvcClassDefault {
		t.Errorf("Expected service class name default, got %s", svcClass.Name())
	}
	if svcClass.Priority() != 1 {
		t.Errorf("Expected service class priority 1, got %d", svcClass.Priority())
	}

	// Validate capacity
	if system.capacity["GPU_A100"] != 4 {
		t.Errorf("Expected GPU_A100 capacity 4, got %d", system.capacity["GPU_A100"])
	}

	// Validate optimizer spec
	if optimizerSpec.Unlimited {
		t.Error("Expected Unlimited to be false")
	}
	if optimizerSpec.SaturationPolicy != "None" {
		t.Errorf("Expected SaturationPolicy None, got %s", optimizerSpec.SaturationPolicy)
	}
	if optimizerSpec.DelayedBestEffort {
		t.Error("Expected DelayedBestEffort to be false")
	}
}

func TestSystem_SetAcceleratorsFromSpec(t *testing.T) {
	system := NewSystem()

	acceleratorData := &config.AcceleratorData{
		Spec: []config.AcceleratorSpec{
			{
				Name: "A100",
				Type: "GPU_A100",
				Power: config.PowerSpec{
					Idle:     50,
					MidPower: 150,
					Full:     350,
					MidUtil:  0.4,
				},
				Cost:         1.0,
				Multiplicity: 1,
				MemSize:      40,
			},
			{
				Name: "H100",
				Type: "GPU_H100",
				Power: config.PowerSpec{
					Idle:     60,
					MidPower: 200,
					Full:     450,
					MidUtil:  0.5,
				},
				Cost:         2.0,
				Multiplicity: 1,
				MemSize:      80,
			},
		},
	}

	system.SetAcceleratorsFromSpec(acceleratorData)

	if len(system.accelerators) != 2 {
		t.Errorf("Expected 2 accelerators, got %d", len(system.accelerators))
	}

	// Validate A100 accelerator
	a100 := system.accelerators["A100"]
	if a100 == nil {
		t.Fatal("A100 accelerator should exist")
	}
	if a100.Type() != "GPU_A100" {
		t.Errorf("Expected A100 type GPU_A100, got %s", a100.Type())
	}
	if a100.Spec().Cost != 1.0 {
		t.Errorf("Expected A100 cost 1.0, got %f", a100.Spec().Cost)
	}
	if a100.Spec().MemSize != 40 {
		t.Errorf("Expected A100 memsize 40, got %d", a100.Spec().MemSize)
	}

	// Validate H100 accelerator
	h100 := system.accelerators["H100"]
	if h100 == nil {
		t.Fatal("H100 accelerator should exist")
	}
	if h100.Type() != "GPU_H100" {
		t.Errorf("Expected H100 type GPU_H100, got %s", h100.Type())
	}
	if h100.Spec().Cost != 2.0 {
		t.Errorf("Expected H100 cost 2.0, got %f", h100.Spec().Cost)
	}
	if h100.Spec().MemSize != 80 {
		t.Errorf("Expected H100 memsize 80, got %d", h100.Spec().MemSize)
	}
}

func TestSystem_AddAcceleratorFromSpec(t *testing.T) {
	system := NewSystem()

	spec := config.AcceleratorSpec{
		Name: "A100",
		Type: "GPU_A100",
		Power: config.PowerSpec{
			Idle:     50,
			MidPower: 150,
			Full:     350,
			MidUtil:  0.4,
		},
		Cost:         1.0,
		Multiplicity: 1,
		MemSize:      40,
	}

	system.AddAcceleratorFromSpec(spec)

	if len(system.accelerators) != 1 {
		t.Errorf("Expected 1 accelerator, got %d", len(system.accelerators))
	}

	acc := system.accelerators["A100"]
	if acc == nil {
		t.Fatal("A100 accelerator should exist")
	}

	// Validate accelerator properties were correctly set
	if acc.Name() != "A100" {
		t.Errorf("Expected accelerator name A100, got %s", acc.Name())
	}

	if acc.Type() != testAccTypeGPUA100 {
		t.Errorf("Expected accelerator type GPU_A100, got %s", acc.Type())
	}

	// Validate power spec
	powerSpec := acc.Spec().Power
	if powerSpec.Idle != 50 {
		t.Errorf("Expected Idle power 50, got %d", powerSpec.Idle)
	}
	if powerSpec.MidPower != 150 {
		t.Errorf("Expected MidPower 150, got %d", powerSpec.MidPower)
	}
	if powerSpec.Full != 350 {
		t.Errorf("Expected Full power 350, got %d", powerSpec.Full)
	}
	if powerSpec.MidUtil != 0.4 {
		t.Errorf("Expected MidUtil 0.4, got %f", powerSpec.MidUtil)
	}

	// Validate cost, multiplicity, memsize
	if acc.Spec().Cost != 1.0 {
		t.Errorf("Expected Cost 1.0, got %f", acc.Spec().Cost)
	}
	if acc.Spec().Multiplicity != 1 {
		t.Errorf("Expected Multiplicity 1, got %d", acc.Spec().Multiplicity)
	}
	if acc.Spec().MemSize != 40 {
		t.Errorf("Expected MemSize 40, got %d", acc.Spec().MemSize)
	}
}

func TestSystem_RemoveAccelerator(t *testing.T) {
	system := NewSystem()

	spec := config.AcceleratorSpec{
		Name: "A100",
		Type: "GPU_A100",
		Power: config.PowerSpec{
			Idle:     50,
			MidPower: 150,
			Full:     350,
			MidUtil:  0.4,
		},
		Cost:         1.0,
		Multiplicity: 1,
		MemSize:      40,
	}

	system.AddAcceleratorFromSpec(spec)

	if len(system.accelerators) == 0 {
		t.Errorf("Expected 1 accelerators after add, got %d", len(system.accelerators))
	}

	// Test successful removal
	err := system.RemoveAccelerator("A100")
	if err != nil {
		t.Errorf("RemoveAccelerator should succeed, got error: %v", err)
	}

	if len(system.accelerators) != 0 {
		t.Errorf("Expected 0 accelerators after removal, got %d", len(system.accelerators))
	}

	// Test removal of non-existent accelerator
	err = system.RemoveAccelerator("NonExistent")
	if err == nil {
		t.Error("RemoveAccelerator should fail for non-existent accelerator")
	}
}

func TestSystem_SetCapacityFromSpec(t *testing.T) {
	system := NewSystem()

	capacityData := &config.CapacityData{
		Count: []config.AcceleratorCount{
			{Type: "GPU_A100", Count: 4},
			{Type: "GPU_H100", Count: 2},
		},
	}

	system.SetCapacityFromSpec(capacityData)

	if len(system.capacity) != 2 {
		t.Errorf("Expected 2 capacity entries, got %d", len(system.capacity))
	}

	if system.capacity["GPU_A100"] != 4 {
		t.Errorf("Expected GPU_A100 capacity 4, got %d", system.capacity["GPU_A100"])
	}

	if system.capacity["GPU_H100"] != 2 {
		t.Errorf("Expected GPU_H100 capacity 2, got %d", system.capacity["GPU_H100"])
	}
}

func TestSystem_SetCountFromSpec(t *testing.T) {
	system := NewSystem()

	spec := config.AcceleratorCount{Type: "GPU_A100", Count: 4}

	system.SetCountFromSpec(spec)

	if len(system.capacity) != 1 {
		t.Errorf("Expected 1 capacity entry, got %d", len(system.capacity))
	}

	if system.capacity["GPU_A100"] != 4 {
		t.Errorf("Expected GPU_A100 capacity 4, got %d", system.capacity["GPU_A100"])
	}
}

func TestSystem_SetModelsFromSpec(t *testing.T) {
	system := NewSystem()

	modelData := &config.ModelData{
		PerfData: []config.ModelAcceleratorPerfData{
			{
				Name:         testModelNameLlama7b,
				Acc:          "A100",
				AccCount:     1,
				MaxBatchSize: 16,
				AtTokens:     100,
				ServiceParms: config.ServiceParms{
					Alpha: 10.0,
					Beta:  2.0,
					Gamma: 0.1,
				},
			},
			{
				Name:         "llama-13b",
				Acc:          "A100",
				AccCount:     2,
				MaxBatchSize: 8,
				AtTokens:     150,
				ServiceParms: config.ServiceParms{
					Alpha: 15.0,
					Beta:  3.0,
					Gamma: 0.15,
				},
			},
		},
	}

	system.SetModelsFromSpec(modelData)

	if len(system.models) != 2 {
		t.Errorf("Expected 2 models, got %d", len(system.models))
	}

	// Validate llama-7b model
	model7b := system.models[testModelNameLlama7b]
	if model7b == nil {
		t.Fatal("llama-7b model should exist")
	}
	if model7b.Name() != testModelNameLlama7b {
		t.Errorf("Expected model name llama-7b, got %s", model7b.Name())
	}
	perfData7b := model7b.PerfData("A100")
	if perfData7b == nil {
		t.Fatal("llama-7b should have A100 perf data")
	}
	if perfData7b.AccCount != 1 {
		t.Errorf("Expected AccCount 1 for llama-7b, got %d", perfData7b.AccCount)
	}
	if perfData7b.MaxBatchSize != 16 {
		t.Errorf("Expected MaxBatchSize 16 for llama-7b, got %d", perfData7b.MaxBatchSize)
	}
	if perfData7b.AtTokens != 100 {
		t.Errorf("Expected AtTokens 100 for llama-7b, got %d", perfData7b.AtTokens)
	}
	if perfData7b.ServiceParms.Alpha != 10.0 {
		t.Errorf("Expected ServiceParms.Alpha 10.0 for llama-7b, got %f", perfData7b.ServiceParms.Alpha)
	}
	if perfData7b.ServiceParms.Beta != 2.0 {
		t.Errorf("Expected ServiceParms.Beta 2.0 for llama-7b, got %f", perfData7b.ServiceParms.Beta)
	}
	if perfData7b.ServiceParms.Gamma != 0.1 {
		t.Errorf("Expected ServiceParms.Gamma 0.1 for llama-7b, got %f", perfData7b.ServiceParms.Gamma)
	}

	// Validate llama-13b model
	model13b := system.models["llama-13b"]
	if model13b == nil {
		t.Fatal("llama-13b model should exist")
	}
	if model13b.Name() != "llama-13b" {
		t.Errorf("Expected model name llama-13b, got %s", model13b.Name())
	}
	perfData13b := model13b.PerfData("A100")
	if perfData13b == nil {
		t.Fatal("llama-13b should have A100 perf data")
	}
	if perfData13b.AccCount != 2 {
		t.Errorf("Expected AccCount 2 for llama-13b, got %d", perfData13b.AccCount)
	}
	if perfData13b.MaxBatchSize != 8 {
		t.Errorf("Expected MaxBatchSize 8 for llama-13b, got %d", perfData13b.MaxBatchSize)
	}
	if perfData13b.AtTokens != 150 {
		t.Errorf("Expected AtTokens 150 for llama-13b, got %d", perfData13b.AtTokens)
	}
	if perfData13b.ServiceParms.Alpha != 15.0 {
		t.Errorf("Expected ServiceParms.Alpha 15.0 for llama-13b, got %f", perfData13b.ServiceParms.Alpha)
	}
	if perfData13b.ServiceParms.Beta != 3.0 {
		t.Errorf("Expected ServiceParms.Beta 3.0 for llama-13b, got %f", perfData13b.ServiceParms.Beta)
	}
	if perfData13b.ServiceParms.Gamma != 0.15 {
		t.Errorf("Expected ServiceParms.Gamma 0.15 for llama-13b, got %f", perfData13b.ServiceParms.Gamma)
	}
}

func TestSystem_AddModel(t *testing.T) {
	system := NewSystem()

	model := system.AddModel("test-model")

	if model == nil {
		t.Fatal("AddModel should return a model")
	}

	if len(system.models) != 1 {
		t.Errorf("Expected 1 model, got %d", len(system.models))
	}

	if system.models["test-model"] != model {
		t.Error("Model should be stored in system")
	}

	if model.Name() != "test-model" {
		t.Errorf("Expected model name test-model, got %s", model.Name())
	}
}

func TestSystem_RemoveModel(t *testing.T) {
	system := NewSystem()

	system.AddModel("test-model")

	// Test successful removal
	err := system.RemoveModel("test-model")
	if err != nil {
		t.Errorf("RemoveModel should succeed, got error: %v", err)
	}

	if len(system.models) != 0 {
		t.Errorf("Expected 0 models after removal, got %d", len(system.models))
	}

	// Test removal of non-existent model
	err = system.RemoveModel("NonExistent")
	if err == nil {
		t.Error("RemoveModel should fail for non-existent model")
	}
}

func TestSystem_SetServersFromSpec(t *testing.T) {
	system := NewSystem()

	serverData := &config.ServerData{
		Spec: []config.ServerSpec{
			{
				Name:  "server1",
				Model: testModelNameLlama7b,
				Class: "default",
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{
						ArrivalRate:  30,
						AvgInTokens:  100,
						AvgOutTokens: 200,
					},
				},
				MinNumReplicas: 1,
				MaxBatchSize:   16,
			},
			{
				Name:  "server2",
				Model: "llama-13b",
				Class: "high-priority",
				CurrentAlloc: config.AllocationData{
					Load: config.ServerLoadSpec{
						ArrivalRate:  20,
						AvgInTokens:  150,
						AvgOutTokens: 300,
					},
				},
				MinNumReplicas: 2,
				MaxBatchSize:   8,
			},
		},
	}

	system.SetServersFromSpec(serverData)

	if len(system.servers) != 2 {
		t.Errorf("Expected 2 servers, got %d", len(system.servers))
	}

	// Validate server1
	server1 := system.servers["server1"]
	if server1 == nil {
		t.Fatal("server1 should exist")
	}
	if server1.Name() != "server1" {
		t.Errorf("Expected server name server1, got %s", server1.Name())
	}
	if server1.ModelName() != testModelNameLlama7b {
		t.Errorf("Expected model name llama-7b for server1, got %s", server1.ModelName())
	}
	if server1.ServiceClassName() != "default" {
		t.Errorf("Expected service class default for server1, got %s", server1.ServiceClassName())
	}
	load1 := server1.Load()
	if load1 == nil {
		t.Fatal("server1 should have load")
	}
	if load1.ArrivalRate != 30 {
		t.Errorf("Expected ArrivalRate 30 for server1, got %f", load1.ArrivalRate)
	}
	if load1.AvgInTokens != 100 {
		t.Errorf("Expected AvgInTokens 100 for server1, got %d", load1.AvgInTokens)
	}
	if load1.AvgOutTokens != 200 {
		t.Errorf("Expected AvgOutTokens 200 for server1, got %d", load1.AvgOutTokens)
	}
	spec1 := server1.Spec()
	if spec1 == nil {
		t.Fatal("server1 should have spec")
	}
	if spec1.MinNumReplicas != 1 {
		t.Errorf("Expected MinNumReplicas 1 for server1, got %d", spec1.MinNumReplicas)
	}
	if spec1.MaxBatchSize != 16 {
		t.Errorf("Expected MaxBatchSize 16 for server1, got %d", spec1.MaxBatchSize)
	}

	// Validate server2
	server2 := system.servers["server2"]
	if server2 == nil {
		t.Fatal("server2 should exist")
	}
	if server2.Name() != "server2" {
		t.Errorf("Expected server name server2, got %s", server2.Name())
	}
	if server2.ModelName() != "llama-13b" {
		t.Errorf("Expected model name llama-13b for server2, got %s", server2.ModelName())
	}
	if server2.ServiceClassName() != "high-priority" {
		t.Errorf("Expected service class high-priority for server2, got %s", server2.ServiceClassName())
	}
	load2 := server2.Load()
	if load2 == nil {
		t.Fatal("server2 should have load")
	}
	if load2.ArrivalRate != 20 {
		t.Errorf("Expected ArrivalRate 20 for server2, got %f", load2.ArrivalRate)
	}
	if load2.AvgInTokens != 150 {
		t.Errorf("Expected AvgInTokens 150 for server2, got %d", load2.AvgInTokens)
	}
	if load2.AvgOutTokens != 300 {
		t.Errorf("Expected AvgOutTokens 300 for server2, got %d", load2.AvgOutTokens)
	}
	spec2 := server2.Spec()
	if spec2 == nil {
		t.Fatal("server2 should have spec")
	}
	if spec2.MinNumReplicas != 2 {
		t.Errorf("Expected MinNumReplicas 2 for server2, got %d", spec2.MinNumReplicas)
	}
	if spec2.MaxBatchSize != 8 {
		t.Errorf("Expected MaxBatchSize 8 for server2, got %d", spec2.MaxBatchSize)
	}
}

func TestSystem_AddServerFromSpec(t *testing.T) {
	system := NewSystem()

	spec := config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	}

	system.AddServerFromSpec(spec)

	if len(system.servers) != 1 {
		t.Errorf("Expected 1 server, got %d", len(system.servers))
	}

	server := system.servers["test-server"]
	if server == nil {
		t.Fatal("test-server should exist")
	}

	if server.Name() != "test-server" {
		t.Errorf("Expected server name test-server, got %s", server.Name())
	}
	if server.ModelName() != "test-model" {
		t.Errorf("Expected model name test-model, got %s", server.ModelName())
	}
	if server.ServiceClassName() != "default" {
		t.Errorf("Expected service class default, got %s", server.ServiceClassName())
	}
	load := server.Load()
	if load == nil {
		t.Fatal("server should have load")
	}
	if load.ArrivalRate != 30 {
		t.Errorf("Expected ArrivalRate 30, got %f", load.ArrivalRate)
	}
	if load.AvgInTokens != 100 {
		t.Errorf("Expected AvgInTokens 100, got %d", load.AvgInTokens)
	}
	if load.AvgOutTokens != 200 {
		t.Errorf("Expected AvgOutTokens 200, got %d", load.AvgOutTokens)
	}
	serverSpec := server.Spec()
	if serverSpec == nil {
		t.Fatal("server should have spec")
	}
	if serverSpec.MinNumReplicas != 1 {
		t.Errorf("Expected MinNumReplicas 1, got %d", serverSpec.MinNumReplicas)
	}
	if serverSpec.MaxBatchSize != 16 {
		t.Errorf("Expected MaxBatchSize 16, got %d", serverSpec.MaxBatchSize)
	}
}

func TestSystem_RemoveServer(t *testing.T) {
	system := NewSystem()

	spec := config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	}

	system.AddServerFromSpec(spec)

	// Test successful removal
	err := system.RemoveServer("test-server")
	if err != nil {
		t.Errorf("RemoveServer should succeed, got error: %v", err)
	}

	if len(system.servers) != 0 {
		t.Errorf("Expected 0 servers after removal, got %d", len(system.servers))
	}

	// Test removal of non-existent server
	err = system.RemoveServer("NonExistent")
	if err == nil {
		t.Error("RemoveServer should fail for non-existent server")
	}
}

func TestSystem_SetServiceClassesFromSpec(t *testing.T) {
	system := NewSystem()

	serviceClassData := &config.ServiceClassData{
		Spec: []config.ServiceClassSpec{
			{
				Name:     "high-priority",
				Priority: 1,
				ModelTargets: []config.ModelTarget{
					{
						Model:    testModelNameLlama7b,
						SLO_ITL:  100,
						SLO_TTFT: 1000,
						SLO_TPS:  50,
					},
				},
			},
			{
				Name:     "low-priority",
				Priority: 3,
				ModelTargets: []config.ModelTarget{
					{
						Model:    "llama-13b",
						SLO_ITL:  200,
						SLO_TTFT: 2000,
						SLO_TPS:  25,
					},
				},
			},
		},
	}

	system.SetServiceClassesFromSpec(serviceClassData)

	if len(system.serviceClasses) != 2 {
		t.Errorf("Expected 2 service classes, got %d", len(system.serviceClasses))
	}

	// Validate high-priority service class
	highPriority := system.serviceClasses["high-priority"]
	if highPriority == nil {
		t.Fatal("high-priority service class should exist")
	}
	if highPriority.Name() != "high-priority" {
		t.Errorf("Expected service class name high-priority, got %s", highPriority.Name())
	}
	if highPriority.Priority() != 1 {
		t.Errorf("Expected priority 1 for high-priority, got %d", highPriority.Priority())
	}
	target1 := highPriority.ModelTarget(testModelNameLlama7b)
	if target1 == nil {
		t.Fatal("high-priority should have llama-7b target")
	}
	if target1.ITL != 100 {
		t.Errorf("Expected ITL 100 for high-priority llama-7b, got %f", target1.ITL)
	}
	if target1.TTFT != 1000 {
		t.Errorf("Expected TTFT 1000 for high-priority llama-7b, got %f", target1.TTFT)
	}
	if target1.TPS != 50 {
		t.Errorf("Expected TPS 50 for high-priority llama-7b, got %f", target1.TPS)
	}

	// Validate low-priority service class
	lowPriority := system.serviceClasses["low-priority"]
	if lowPriority == nil {
		t.Fatal("low-priority service class should exist")
	}
	if lowPriority.Name() != "low-priority" {
		t.Errorf("Expected service class name low-priority, got %s", lowPriority.Name())
	}
	if lowPriority.Priority() != 3 {
		t.Errorf("Expected priority 3 for low-priority, got %d", lowPriority.Priority())
	}
	target2 := lowPriority.ModelTarget("llama-13b")
	if target2 == nil {
		t.Fatal("low-priority should have llama-13b target")
	}
	if target2.ITL != 200 {
		t.Errorf("Expected ITL 200 for low-priority llama-13b, got %f", target2.ITL)
	}
	if target2.TTFT != 2000 {
		t.Errorf("Expected TTFT 2000 for low-priority llama-13b, got %f", target2.TTFT)
	}
	if target2.TPS != 25 {
		t.Errorf("Expected TPS 25 for low-priority llama-13b, got %f", target2.TPS)
	}
}

func TestSystem_AddServiceClass(t *testing.T) {
	system := NewSystem()

	system.AddServiceClass("test-class", 2)

	if len(system.serviceClasses) != 1 {
		t.Errorf("Expected 1 service class, got %d", len(system.serviceClasses))
	}

	serviceClass := system.serviceClasses["test-class"]
	if serviceClass == nil {
		t.Fatal("test-class should exist")
	}

	if serviceClass.Name() != "test-class" {
		t.Errorf("Expected service class name test-class, got %s", serviceClass.Name())
	}

	if serviceClass.Priority() != 2 {
		t.Errorf("Expected service class priority 2, got %d", serviceClass.Priority())
	}
}

func TestSystem_RemoveServiceClass(t *testing.T) {
	system := NewSystem()

	system.AddServiceClass("test-class", 2)

	// Test successful removal
	err := system.RemoveServiceClass("test-class")
	if err != nil {
		t.Errorf("RemoveServiceClass should succeed, got error: %v", err)
	}

	if len(system.serviceClasses) != 0 {
		t.Errorf("Expected 0 service classes after removal, got %d", len(system.serviceClasses))
	}

	// Test removal of non-existent service class
	err = system.RemoveServiceClass("NonExistent")
	if err == nil {
		t.Error("RemoveServiceClass should fail for non-existent service class")
	}
}

func TestSystem_Accelerators(t *testing.T) {
	system := NewSystem()

	spec := config.AcceleratorSpec{
		Name: "A100",
		Type: "GPU_A100",
		Power: config.PowerSpec{
			Idle:     50,
			MidPower: 150,
			Full:     350,
			MidUtil:  0.4,
		},
		Cost:         1.0,
		Multiplicity: 1,
		MemSize:      40,
	}

	system.AddAcceleratorFromSpec(spec)

	accelerators := system.Accelerators()

	if len(accelerators) != 1 {
		t.Errorf("Expected 1 accelerator, got %d", len(accelerators))
	}

	if accelerators["A100"] == nil {
		t.Error("A100 accelerator should exist")
	}
}

func TestSystem_Models(t *testing.T) {
	system := NewSystem()

	system.AddModel("test-model")

	models := system.Models()

	if len(models) != 1 {
		t.Errorf("Expected 1 model, got %d", len(models))
	}

	if models["test-model"] == nil {
		t.Error("test-model should exist")
	}
}

func TestSystem_ServiceClasses(t *testing.T) {
	system := NewSystem()

	system.AddServiceClass("test-class", 2)

	serviceClasses := system.ServiceClasses()

	if len(serviceClasses) != 1 {
		t.Errorf("Expected 1 service class, got %d", len(serviceClasses))
	}

	if serviceClasses["test-class"] == nil {
		t.Error("test-class should exist")
	}
}

func TestSystem_Servers(t *testing.T) {
	system := NewSystem()

	spec := config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	}

	system.AddServerFromSpec(spec)

	servers := system.Servers()

	if len(servers) != 1 {
		t.Errorf("Expected 1 server, got %d", len(servers))
	}

	if servers["test-server"] == nil {
		t.Error("test-server should exist")
	}
}

func TestSystem_Accelerator(t *testing.T) {
	system := NewSystem()

	spec := config.AcceleratorSpec{
		Name: "A100",
		Type: "GPU_A100",
		Power: config.PowerSpec{
			Idle:     50,
			MidPower: 150,
			Full:     350,
			MidUtil:  0.4,
		},
		Cost:         1.0,
		Multiplicity: 1,
		MemSize:      40,
	}

	system.AddAcceleratorFromSpec(spec)

	// Test existing accelerator
	acc := system.Accelerator("A100")
	if acc == nil {
		t.Error("Accelerator A100 should exist")
	}

	// Test non-existent accelerator
	acc = system.Accelerator("NonExistent")
	if acc != nil {
		t.Error("NonExistent accelerator should return nil")
	}
}

func TestSystem_Model(t *testing.T) {
	system := NewSystem()

	system.AddModel("test-model")

	// Test existing model
	model := system.Model("test-model")
	if model == nil {
		t.Error("Model test-model should exist")
	}

	// Test non-existent model
	model = system.Model("NonExistent")
	if model != nil {
		t.Error("NonExistent model should return nil")
	}
}

func TestSystem_ServiceClass(t *testing.T) {
	system := NewSystem()

	system.AddServiceClass("test-class", 2)

	// Test existing service class
	serviceClass := system.ServiceClass("test-class")
	if serviceClass == nil {
		t.Error("ServiceClass test-class should exist")
	}

	// Test non-existent service class
	serviceClass = system.ServiceClass("NonExistent")
	if serviceClass != nil {
		t.Error("NonExistent service class should return nil")
	}
}

func TestSystem_Server(t *testing.T) {
	system := NewSystem()

	spec := config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	}

	system.AddServerFromSpec(spec)

	// Test existing server
	server := system.Server("test-server")
	if server == nil {
		t.Error("Server test-server should exist")
	}

	// Test non-existent server
	server = system.Server("NonExistent")
	if server != nil {
		t.Error("NonExistent server should return nil")
	}
}

func TestSystem_Capacities(t *testing.T) {
	system := NewSystem()

	system.SetCountFromSpec(config.AcceleratorCount{Type: "GPU_A100", Count: 4})

	capacities := system.Capacities()

	if len(capacities) != 1 {
		t.Errorf("Expected 1 capacity entry, got %d", len(capacities))
	}

	if capacities["GPU_A100"] != 4 {
		t.Errorf("Expected GPU_A100 capacity 4, got %d", capacities["GPU_A100"])
	}
}

func TestSystem_Capacity(t *testing.T) {
	system := NewSystem()

	system.SetCountFromSpec(config.AcceleratorCount{Type: "GPU_A100", Count: 4})

	// Test existing capacity
	capacity, exists := system.Capacity("GPU_A100")
	if !exists {
		t.Error("GPU_A100 capacity should exist")
	}
	if capacity != 4 {
		t.Errorf("Expected GPU_A100 capacity 4, got %d", capacity)
	}

	// Test non-existent capacity
	capacity, exists = system.Capacity("NonExistent")
	if exists {
		t.Error("NonExistent capacity should not exist")
	}
	if capacity != 0 {
		t.Errorf("Expected capacity 0 for non-existent type, got %d", capacity)
	}
}

func TestSystem_RemoveCapacity(t *testing.T) {
	system := NewSystem()

	system.SetCountFromSpec(config.AcceleratorCount{Type: "GPU_A100", Count: 4})

	// Test successful removal
	removed := system.RemoveCapacity("GPU_A100")
	if !removed {
		t.Error("RemoveCapacity should return true for existing capacity")
	}

	if len(system.capacity) != 0 {
		t.Errorf("Expected 0 capacity entries after removal, got %d", len(system.capacity))
	}

	// Test removal of non-existent capacity
	removed = system.RemoveCapacity("NonExistent")
	if removed {
		t.Error("RemoveCapacity should return false for non-existent capacity")
	}
}

func TestSystem_Calculate(t *testing.T) {
	system := NewSystem()
	TheSystem = system

	// Add accelerator
	system.AddAcceleratorFromSpec(config.AcceleratorSpec{
		Name: "A100",
		Type: "GPU_A100",
		Power: config.PowerSpec{
			Idle:     50,
			MidPower: 150,
			Full:     350,
			MidUtil:  0.4,
		},
		Cost:         1.0,
		Multiplicity: 1,
		MemSize:      40,
	})

	// Add model
	model := system.AddModel("test-model")
	model.AddPerfDataFromSpec(&config.ModelAcceleratorPerfData{
		Name:         "test-model",
		Acc:          "A100",
		AccCount:     1,
		MaxBatchSize: 16,
		AtTokens:     100,
		ServiceParms: config.ServiceParms{
			Alpha: 10.0,
			Beta:  2.0,
			Gamma: 0.1,
		},
	})

	// Add service class with target
	system.AddServiceClass("default", 1)
	serviceClass := system.ServiceClass("default")
	if serviceClass != nil {
		serviceClass.AddModelTarget(&config.ModelTarget{
			Model:    "test-model",
			SLO_ITL:  100,
			SLO_TTFT: 1000,
			SLO_TPS:  50,
		})
	}

	// Add server
	system.AddServerFromSpec(config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	})

	// Before Calculate, server should have no allocations
	server := system.Server("test-server")
	if server == nil {
		t.Fatal("test-server should exist")
	}
	allAllocationsBefore := server.AllAllocations()
	if len(allAllocationsBefore) != 0 {
		t.Errorf("Expected 0 allocations before Calculate, got %d", len(allAllocationsBefore))
	}

	system.Calculate()

	// After Calculate, server should have calculated allocations for compatible accelerators
	allAllocationsAfterCalculate := server.AllAllocations()
	if len(allAllocationsAfterCalculate) == 0 {
		t.Error("Expected allocations after Calculate, got 0")
	}

	// Verify that the A100 allocation was created
	a100Alloc := allAllocationsAfterCalculate["A100"]
	if a100Alloc == nil {
		t.Error("Expected A100 allocation after Calculate")
	} else {
		// Verify allocation properties were calculated
		if a100Alloc.NumReplicas() <= 0 {
			t.Errorf("Expected positive NumReplicas for A100 allocation, got %d", a100Alloc.NumReplicas())
		}
		if a100Alloc.MaxBatchSize() <= 0 {
			t.Errorf("Expected positive MaxBatchSize for A100 allocation, got %d", a100Alloc.MaxBatchSize())
		}
		if a100Alloc.Cost() < 0 {
			t.Errorf("Expected non-negative Cost for A100 allocation, got %f", a100Alloc.Cost())
		}
	}
}

func TestSystem_AllocateByType(t *testing.T) {
	system := NewSystem()
	TheSystem = system // Set global reference

	// Add accelerator
	system.AddAcceleratorFromSpec(config.AcceleratorSpec{
		Name: "A100",
		Type: "GPU_A100",
		Power: config.PowerSpec{
			Idle:     50,
			MidPower: 150,
			Full:     350,
			MidUtil:  0.4,
		},
		Cost:         1.0,
		Multiplicity: 1,
		MemSize:      40,
	})

	// Add model
	model := system.AddModel("test-model")
	model.AddPerfDataFromSpec(&config.ModelAcceleratorPerfData{
		Name:         "test-model",
		Acc:          "A100",
		AccCount:     1,
		MaxBatchSize: 16,
		AtTokens:     100,
		ServiceParms: config.ServiceParms{
			Alpha: 10.0,
			Beta:  2.0,
			Gamma: 0.1,
		},
	})

	// Add service class with target
	system.AddServiceClass("default", 1)
	serviceClass := system.ServiceClass("default")
	if serviceClass != nil {
		serviceClass.AddModelTarget(&config.ModelTarget{
			Model:    "test-model",
			SLO_ITL:  100,
			SLO_TTFT: 1000,
			SLO_TPS:  50,
		})
	}

	// Set capacity
	system.SetCountFromSpec(config.AcceleratorCount{Type: "GPU_A100", Count: 4})

	// Add server
	system.AddServerFromSpec(config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
			Accelerator: "A100",
			NumReplicas: 2,
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	})

	// Calculate to prepare allocations
	system.Calculate()

	// Get the server and create an allocation for it
	server := system.Server("test-server")
	if server != nil {
		alloc := CreateAllocation("test-server", "A100")
		if alloc != nil {
			server.SetAllocation(alloc)
		}
	}

	// Before AllocateByType, allocationByType should be empty
	if len(system.allocationByType) != 0 {
		t.Errorf("Expected 0 allocationByType entries before AllocateByType, got %d", len(system.allocationByType))
	}

	system.AllocateByType()

	// After AllocateByType, should have allocation data for GPU_A100
	if len(system.allocationByType) == 0 {
		t.Error("Expected allocationByType entries after AllocateByType, got 0")
	}

	// Verify GPU_A100 allocation by type exists and has valid data
	a100AllocByType := system.allocationByType["GPU_A100"]
	if a100AllocByType == nil {
		t.Error("Expected GPU_A100 allocationByType entry")
	} else {
		if a100AllocByType.count < 0 {
			t.Errorf("Expected non-negative count for GPU_A100, got %d", a100AllocByType.count)
		}
		if a100AllocByType.limit != 4 {
			t.Errorf("Expected limit 4 for GPU_A100 (from capacity), got %d", a100AllocByType.limit)
		}
		if a100AllocByType.cost < 0 {
			t.Errorf("Expected non-negative cost for GPU_A100, got %f", a100AllocByType.cost)
		}
	}
}

func TestSystem_GenerateSolution(t *testing.T) {
	system := NewSystem()
	TheSystem = system // Set global reference

	// Add accelerator
	system.AddAcceleratorFromSpec(config.AcceleratorSpec{
		Name: "A100",
		Type: "GPU_A100",
		Power: config.PowerSpec{
			Idle:     50,
			MidPower: 150,
			Full:     350,
			MidUtil:  0.4,
		},
		Cost:         1.0,
		Multiplicity: 1,
		MemSize:      40,
	})

	// Add model
	model := system.AddModel("test-model")
	model.AddPerfDataFromSpec(&config.ModelAcceleratorPerfData{
		Name:         "test-model",
		Acc:          "A100",
		AccCount:     1,
		MaxBatchSize: 16,
		AtTokens:     100,
		ServiceParms: config.ServiceParms{
			Alpha: 10.0,
			Beta:  2.0,
			Gamma: 0.1,
		},
	})

	// Add service class with target
	system.AddServiceClass("default", 1)
	serviceClass := system.ServiceClass("default")
	if serviceClass != nil {
		serviceClass.AddModelTarget(&config.ModelTarget{
			Model:    "test-model",
			SLO_ITL:  100,
			SLO_TTFT: 1000,
			SLO_TPS:  50,
		})
	}

	// Add server
	system.AddServerFromSpec(config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
			Accelerator: "A100",
			NumReplicas: 2,
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	})

	// Calculate to prepare allocations
	system.Calculate()

	// Get the server and create an allocation for it
	server := system.Server("test-server")
	if server != nil {
		alloc := CreateAllocation("test-server", "A100")
		if alloc != nil {
			server.SetAllocation(alloc)
		}
	}

	solution := system.GenerateSolution()

	if solution == nil {
		t.Fatal("GenerateSolution should return a solution")
	}

	if system.allocationSolution != solution {
		t.Error("System should store the generated solution")
	}

	// Validate solution content
	if len(solution.Spec) == 0 {
		t.Error("Expected solution to contain server allocations")
	}

	// Check that the test-server is in the solution
	serverAlloc, exists := solution.Spec["test-server"]
	if !exists {
		t.Error("Expected test-server to be in solution")
	} else {
		if serverAlloc.Accelerator == "" {
			t.Error("Expected server allocation to have an accelerator")
		}
		if serverAlloc.NumReplicas <= 0 {
			t.Errorf("Expected positive NumReplicas in solution, got %d", serverAlloc.NumReplicas)
		}
	}
}

func TestSystem_String(t *testing.T) {
	system := NewSystem()

	// Add basic components
	system.AddAcceleratorFromSpec(config.AcceleratorSpec{
		Name: "A100",
		Type: "GPU_A100",
		Power: config.PowerSpec{
			Idle:     50,
			MidPower: 150,
			Full:     350,
			MidUtil:  0.4,
		},
		Cost:         1.0,
		Multiplicity: 1,
		MemSize:      40,
	})

	system.AddServiceClass("default", 1)
	serviceClass := system.ServiceClass("default")
	if serviceClass != nil {
		serviceClass.AddModelTarget(&config.ModelTarget{
			Model:    "test-model",
			SLO_ITL:  100,
			SLO_TTFT: 1000,
			SLO_TPS:  50,
		})
	}

	system.AddServerFromSpec(config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
			Accelerator: "A100",
			NumReplicas: 2,
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	})

	result := system.String()

	// Should contain solution information
	if !strings.Contains(result, "Solution:") {
		t.Error("String should contain Solution section")
	}

	// Should contain allocation by type information
	if !strings.Contains(result, "AllocationByType:") {
		t.Error("String should contain AllocationByType section")
	}

	// Should contain total cost
	if !strings.Contains(result, "totalCost=") {
		t.Error("String should contain totalCost")
	}
}

func TestAllocationByType_String(t *testing.T) {
	alloc := &AllocationByType{
		name:  "GPU_A100",
		count: 4,
		limit: 8,
		cost:  100.5,
	}

	result := alloc.String()

	if !strings.Contains(result, "name=GPU_A100") {
		t.Error("String should contain allocation type name")
	}

	if !strings.Contains(result, "count=4") {
		t.Error("String should contain count")
	}

	if !strings.Contains(result, "limit=8") {
		t.Error("String should contain limit")
	}

	if !strings.Contains(result, "cost=100.5") {
		t.Error("String should contain cost")
	}
}

// Test global functions
func TestGetModels(t *testing.T) {
	// Create a test system and set it as TheSystem
	system := NewSystem()
	system.AddModel("test-model")
	TheSystem = system

	models := GetModels()

	if len(models) != 1 {
		t.Errorf("Expected 1 model, got %d", len(models))
	}

	if models["test-model"] == nil {
		t.Error("test-model should exist")
	}
}

func TestGetServers(t *testing.T) {
	// Create a test system and set it as TheSystem
	system := NewSystem()
	system.AddServerFromSpec(config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  30,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		MinNumReplicas: 1,
		MaxBatchSize:   16,
	})
	TheSystem = system

	servers := GetServers()

	if len(servers) != 1 {
		t.Errorf("Expected 1 server, got %d", len(servers))
	}

	if servers["test-server"] == nil {
		t.Error("test-server should exist")
	}
}

func TestGetCapacities(t *testing.T) {
	// Create a test system and set it as TheSystem
	system := NewSystem()
	system.SetCountFromSpec(config.AcceleratorCount{Type: "GPU_A100", Count: 4})
	TheSystem = system

	capacities := GetCapacities()

	if len(capacities) != 1 {
		t.Errorf("Expected 1 capacity entry, got %d", len(capacities))
	}

	if capacities["GPU_A100"] != 4 {
		t.Errorf("Expected GPU_A100 capacity 4, got %d", capacities["GPU_A100"])
	}
}
