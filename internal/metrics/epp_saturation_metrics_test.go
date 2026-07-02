package metrics

import (
	"context"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gaugeSeries returns the value and labels of the single series of the
// named gauge metric family, or ok=false if the family is absent.
func gaugeSeries(t *testing.T, mfs []*dto.MetricFamily, name string) (*dto.Metric, bool) {
	t.Helper()
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("metric %s: expected 1 series, got %d", name, len(mf.GetMetric()))
		}
		return mf.GetMetric()[0], true
	}
	return nil, false
}

func TestRecordEPPSaturationMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	// Latest Set wins (gauge), and raw/smoothed are emitted independently.
	emitter.RecordEPPSaturationMetrics(context.Background(), "variant-a", "ns1", "model-x", 1.2, 0.9)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	raw, ok := gaugeSeries(t, mfs, constants.WVAEppSaturationRaw)
	if !ok {
		t.Fatalf("metric %s not found", constants.WVAEppSaturationRaw)
	}
	if v := raw.GetGauge().GetValue(); v != 1.2 {
		t.Errorf("raw saturation: expected 1.2, got %f", v)
	}
	if got := getLabelValue(raw, constants.LabelVariantName); got != "variant-a" {
		t.Errorf("raw saturation variant_name: expected variant-a, got %q", got)
	}
	if got := getLabelValue(raw, constants.LabelModelName); got != "model-x" {
		t.Errorf("raw saturation model_name: expected model-x, got %q", got)
	}
	if got := getLabelValue(raw, constants.LabelNamespace); got != "ns1" {
		t.Errorf("raw saturation namespace: expected ns1, got %q", got)
	}

	smoothed, ok := gaugeSeries(t, mfs, constants.WVAEppSaturationSmoothed)
	if !ok {
		t.Fatalf("metric %s not found", constants.WVAEppSaturationSmoothed)
	}
	if v := smoothed.GetGauge().GetValue(); v != 0.9 {
		t.Errorf("smoothed saturation: expected 0.9, got %f", v)
	}
}

func TestRecordScaleCappedMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordScaleCappedMetric(context.Background(), "variant-a", "ns1", "model-x", true)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}
	capped, ok := gaugeSeries(t, mfs, constants.WVAScaleCapped)
	if !ok {
		t.Fatalf("metric %s not found", constants.WVAScaleCapped)
	}
	if v := capped.GetGauge().GetValue(); v != 1 {
		t.Errorf("scale_capped: expected 1 (capped), got %f", v)
	}

	// Toggling back to not-capped updates the same series to 0.
	emitter.RecordScaleCappedMetric(context.Background(), "variant-a", "ns1", "model-x", false)
	mfs, err = registry.Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}
	capped, _ = gaugeSeries(t, mfs, constants.WVAScaleCapped)
	if v := capped.GetGauge().GetValue(); v != 0 {
		t.Errorf("scale_capped: expected 0 (not capped), got %f", v)
	}
}

func TestEPPSaturationMetrics_NilSafety(t *testing.T) {
	savedRaw, savedSmoothed, savedCapped := eppSaturationRaw, eppSaturationSmoothed, scaleCapped
	eppSaturationRaw, eppSaturationSmoothed, scaleCapped = nil, nil, nil
	defer func() {
		eppSaturationRaw, eppSaturationSmoothed, scaleCapped = savedRaw, savedSmoothed, savedCapped
	}()

	emitter := NewMetricsEmitter()
	// Should not panic when metrics are not initialized.
	emitter.RecordEPPSaturationMetrics(context.Background(), "v", "ns", "m", 1.0, 1.0)
	emitter.RecordScaleCappedMetric(context.Background(), "v", "ns", "m", true)
}
