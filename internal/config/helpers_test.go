package config

import (
	"testing"
)

func TestParseBoolFromConfig(t *testing.T) {
	tests := []struct {
		name         string
		data         map[string]string
		defaultValue bool
		want         bool
	}{
		{
			name:         "missing key keeps true default",
			data:         map[string]string{},
			defaultValue: true,
			want:         true,
		},
		{
			name:         "missing key keeps false default",
			data:         map[string]string{},
			defaultValue: false,
			want:         false,
		},
		{
			name:         "invalid value keeps true default",
			data:         map[string]string{"enabled": "maybe"},
			defaultValue: true,
			want:         true,
		},
		{
			name:         "invalid value keeps false default",
			data:         map[string]string{"enabled": "maybe"},
			defaultValue: false,
			want:         false,
		},
		{
			name:         "literal true",
			data:         map[string]string{"enabled": "true"},
			defaultValue: false,
			want:         true,
		},
		{
			name:         "numeric true",
			data:         map[string]string{"enabled": "1"},
			defaultValue: false,
			want:         true,
		},
		{
			name:         "yes true",
			data:         map[string]string{"enabled": "yes"},
			defaultValue: false,
			want:         true,
		},
		{
			name:         "literal false",
			data:         map[string]string{"enabled": "false"},
			defaultValue: true,
			want:         false,
		},
		{
			name:         "numeric false",
			data:         map[string]string{"enabled": "0"},
			defaultValue: true,
			want:         false,
		},
		{
			name:         "no false",
			data:         map[string]string{"enabled": "no"},
			defaultValue: true,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseBoolFromConfig(tt.data, "enabled", tt.defaultValue); got != tt.want {
				t.Fatalf("ParseBoolFromConfig() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQMAnalyzerConfigMapName_Default(t *testing.T) {
	t.Setenv("QUEUEING_MODEL_CONFIG_MAP_NAME", "")
	if got := QMAnalyzerConfigMapName(); got != "wva-queueing-model-config" {
		t.Errorf("QMAnalyzerConfigMapName() = %q, want %q", got, "wva-queueing-model-config")
	}
}

func TestQMAnalyzerConfigMapName_EnvOverride(t *testing.T) {
	t.Setenv("QUEUEING_MODEL_CONFIG_MAP_NAME", "custom-qm-config")
	if got := QMAnalyzerConfigMapName(); got != "custom-qm-config" {
		t.Errorf("QMAnalyzerConfigMapName() = %q, want %q", got, "custom-qm-config")
	}
}
