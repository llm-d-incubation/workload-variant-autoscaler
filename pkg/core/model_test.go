package core

import (
	"testing"

	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
)

func TestNewModel(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		wantName  string
	}{
		{
			name:      "valid model name",
			modelName: "llama-7b",
			wantName:  "llama-7b",
		},
		{
			name:      "empty model name",
			modelName: "",
			wantName:  "",
		},
		{
			name:      "model name with special chars",
			modelName: "model-v1.2.3",
			wantName:  "model-v1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(tt.modelName)
			if model == nil {
				t.Fatal("NewModel() returned nil")
			}
			if got := model.Name(); got != tt.wantName {
				t.Errorf("Model.Name() = %v, want %v", got, tt.wantName)
			}
		})
	}
}

func TestModel_AddAndRemovePerfDataFromSpec(t *testing.T) {
	tests := []struct {
		name      string
		modelName string
		spec      *config.ModelAcceleratorPerfData
		wantCount int32
	}{
		{
			name:      "valid perf data",
			modelName: "llama-7b",
			spec: &config.ModelAcceleratorPerfData{
				Name:     "llama-7b",
				Acc:      "H100",
				AccCount: 2,
			},
			wantCount: 2,
		},
		{
			name:      "zero accelerator count defaults to 1",
			modelName: "llama-7b",
			spec: &config.ModelAcceleratorPerfData{
				Name:     "llama-7b",
				Acc:      "A100",
				AccCount: 0,
			},
			wantCount: 1,
		},
		{
			name:      "negative accelerator count defaults to 1",
			modelName: "llama-7b",
			spec: &config.ModelAcceleratorPerfData{
				Name:     "llama-7b",
				Acc:      "V100",
				AccCount: -1,
			},
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewModel(tt.modelName)
			model.AddPerfDataFromSpec(tt.spec)

			if got := model.NumInstances(tt.spec.Acc); got != tt.wantCount {
				t.Errorf("Model.NumInstances() = %v, want %v", got, tt.wantCount)
			}

			if got := model.PerfData(tt.spec.Acc); got != tt.spec {
				t.Errorf("Model.PerfData() = %v, want %v", got, tt.spec)
			}

			if model.Spec() == nil {
				t.Errorf("Model.Spec() is nil")
			}

			if model.RemovePerfData(tt.spec.Acc); model.PerfData(tt.spec.Acc) != nil {
				t.Errorf("Model.RemovePerfData() failed, PerfData still exists")
			}
		})
	}
}

func TestModel_AddPerfDataFromSpec_WrongModel(t *testing.T) {
	model := NewModel("llama-7b")
	spec := &config.ModelAcceleratorPerfData{
		Name:     "different-model",
		Acc:      "H100",
		AccCount: 2,
	}

	model.AddPerfDataFromSpec(spec)

	// Should not add perf data for different model
	if got := model.NumInstances("H100"); got != 0 {
		t.Errorf("Model.NumInstances() = %v, want 0 for wrong model", got)
	}

	if got := model.PerfData("H100"); got != nil {
		t.Errorf("Model.PerfData() = %v, want nil for wrong model", got)
	}

	if len(model.Spec().PerfData) != 0 {
		t.Errorf("Model.Spec() has perf data, want empty for wrong model")
	}
}
