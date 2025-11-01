package controller

import (
	"math"
	"os"
	"testing"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
)

func TestMain(m *testing.M) {
	// Initialize logger for all interface tests
	_, err := logger.InitLogger()
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}

	// Run all tests
	code := m.Run()
	os.Exit(code)
}

func TestNewVariantMetrics(t *testing.T) {
	tests := []struct {
		name        string
		load        LoadProfile
		ttft        string
		itl         string
		expectError bool
		validate    func(*testing.T, *VariantMetrics)
	}{
		{
			name: "valid metrics",
			load: LoadProfile{
				ArrivalRate:     "10.5",
				AvgInputTokens:  "100",
				AvgOutputTokens: "200",
			},
			ttft:        "50.0",
			itl:         "25.5",
			expectError: false,
			validate: func(t *testing.T, m *VariantMetrics) {
				if m.Load.ArrivalRate != 10.5 {
					t.Errorf("Expected ArrivalRate 10.5, got %f", m.Load.ArrivalRate)
				}
				if m.Load.AvgInputTokens != 100 {
					t.Errorf("Expected AvgInputTokens 100, got %d", m.Load.AvgInputTokens)
				}
				if m.Load.AvgOutputTokens != 200 {
					t.Errorf("Expected AvgOutputTokens 200, got %d", m.Load.AvgOutputTokens)
				}
				if m.TTFTAverage != 50.0 {
					t.Errorf("Expected TTFTAverage 50.0, got %f", m.TTFTAverage)
				}
				if m.ITLAverage != 25.5 {
					t.Errorf("Expected ITLAverage 25.5, got %f", m.ITLAverage)
				}
			},
		},
		{
			name: "empty strings default to zero",
			load: LoadProfile{
				ArrivalRate:     "",
				AvgInputTokens:  "",
				AvgOutputTokens: "",
			},
			ttft:        "",
			itl:         "",
			expectError: false,
			validate: func(t *testing.T, m *VariantMetrics) {
				if m.Load.ArrivalRate != 0 {
					t.Errorf("Expected ArrivalRate 0, got %f", m.Load.ArrivalRate)
				}
				if m.TTFTAverage != 0 {
					t.Errorf("Expected TTFTAverage 0, got %f", m.TTFTAverage)
				}
				if m.ITLAverage != 0 {
					t.Errorf("Expected ITLAverage 0, got %f", m.ITLAverage)
				}
			},
		},
		{
			name: "invalid values default to zero (resilient behavior)",
			load: LoadProfile{
				ArrivalRate:     "invalid",
				AvgInputTokens:  "not-a-number",
				AvgOutputTokens: "xyz",
			},
			ttft:        "bad-value",
			itl:         "also-bad",
			expectError: false,
			validate: func(t *testing.T, m *VariantMetrics) {
				if m.Load.ArrivalRate != 0 {
					t.Errorf("Expected ArrivalRate 0 for invalid input, got %f", m.Load.ArrivalRate)
				}
				if m.Load.AvgInputTokens != 0 {
					t.Errorf("Expected AvgInputTokens 0 for invalid input, got %d", m.Load.AvgInputTokens)
				}
				if m.TTFTAverage != 0 {
					t.Errorf("Expected TTFTAverage 0 for invalid input, got %f", m.TTFTAverage)
				}
			},
		},
		{
			name: "negative values clamped to zero",
			load: LoadProfile{
				ArrivalRate:     "-10.5",
				AvgInputTokens:  "-100",
				AvgOutputTokens: "-200",
			},
			ttft:        "-50.0",
			itl:         "-25.5",
			expectError: false,
			validate: func(t *testing.T, m *VariantMetrics) {
				if m.Load.ArrivalRate != 0 {
					t.Errorf("Expected negative ArrivalRate to be clamped to 0, got %f", m.Load.ArrivalRate)
				}
				if m.Load.AvgInputTokens != 0 {
					t.Errorf("Expected negative AvgInputTokens to be clamped to 0, got %d", m.Load.AvgInputTokens)
				}
				if m.TTFTAverage != 0 {
					t.Errorf("Expected negative TTFTAverage to be clamped to 0, got %f", m.TTFTAverage)
				}
			},
		},
		{
			name: "very large token values clamped to MaxInt32",
			load: LoadProfile{
				ArrivalRate:     "100.0",
				AvgInputTokens:  "99999999999999",
				AvgOutputTokens: "99999999999999",
			},
			ttft:        "50.0",
			itl:         "25.0",
			expectError: false,
			validate: func(t *testing.T, m *VariantMetrics) {
				if m.Load.AvgInputTokens != math.MaxInt32 {
					t.Errorf("Expected large AvgInputTokens to be clamped to MaxInt32, got %d", m.Load.AvgInputTokens)
				}
				if m.Load.AvgOutputTokens != math.MaxInt32 {
					t.Errorf("Expected large AvgOutputTokens to be clamped to MaxInt32, got %d", m.Load.AvgOutputTokens)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics, err := NewVariantMetrics(tt.load, tt.ttft, tt.itl)

			if tt.expectError && err == nil {
				t.Error("Expected error but got nil")
				return
			}

			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if !tt.expectError && metrics == nil {
				t.Error("Expected metrics but got nil")
				return
			}

			if tt.validate != nil {
				tt.validate(t, metrics)
			}
		})
	}
}

func TestParseLoadProfile(t *testing.T) {
	tests := []struct {
		name     string
		load     LoadProfile
		validate func(*testing.T, LoadMetrics)
	}{
		{
			name: "valid load profile",
			load: LoadProfile{
				ArrivalRate:     "15.5",
				AvgInputTokens:  "512",
				AvgOutputTokens: "1024",
			},
			validate: func(t *testing.T, m LoadMetrics) {
				if m.ArrivalRate != 15.5 {
					t.Errorf("Expected ArrivalRate 15.5, got %f", m.ArrivalRate)
				}
				if m.AvgInputTokens != 512 {
					t.Errorf("Expected AvgInputTokens 512, got %d", m.AvgInputTokens)
				}
				if m.AvgOutputTokens != 1024 {
					t.Errorf("Expected AvgOutputTokens 1024, got %d", m.AvgOutputTokens)
				}
			},
		},
		{
			name: "zero values",
			load: LoadProfile{
				ArrivalRate:     "0",
				AvgInputTokens:  "0",
				AvgOutputTokens: "0",
			},
			validate: func(t *testing.T, m LoadMetrics) {
				if m.ArrivalRate != 0 {
					t.Errorf("Expected ArrivalRate 0, got %f", m.ArrivalRate)
				}
				if m.AvgInputTokens != 0 {
					t.Errorf("Expected AvgInputTokens 0, got %d", m.AvgInputTokens)
				}
			},
		},
		{
			name: "fractional tokens rounded down",
			load: LoadProfile{
				ArrivalRate:     "10.0",
				AvgInputTokens:  "100.7",
				AvgOutputTokens: "200.9",
			},
			validate: func(t *testing.T, m LoadMetrics) {
				if m.AvgInputTokens != 100 {
					t.Errorf("Expected AvgInputTokens 100 (truncated), got %d", m.AvgInputTokens)
				}
				if m.AvgOutputTokens != 200 {
					t.Errorf("Expected AvgOutputTokens 200 (truncated), got %d", m.AvgOutputTokens)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics, err := ParseLoadProfile(tt.load)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if tt.validate != nil {
				tt.validate(t, metrics)
			}
		})
	}
}
