package slo

import (
	"math"
	"strings"
	"testing"

	"github.com/crypticseeds/llm-slo-bench/internal/config"
)

func TestComparePercentileContractForFemi(t *testing.T) {
	tests := []struct {
		name      string
		metric    string
		observed  float64
		threshold float64
		wantPass  bool
		wantErr   bool
	}{
		{name: "below passes", metric: "p99_ttft_ms", observed: 799, threshold: 800, wantPass: true},
		{name: "equal passes", metric: "p99_ttft_ms", observed: 800, threshold: 800, wantPass: true},
		{name: "above fails", metric: "p99_ttft_ms", observed: 801, threshold: 800},
		{name: "empty metric", observed: 1, threshold: 2, wantErr: true},
		{name: "negative observed", metric: "p99_ttft_ms", observed: -1, threshold: 2, wantErr: true},
		{name: "negative threshold", metric: "p99_ttft_ms", observed: 1, threshold: -2, wantErr: true},
		{name: "nan", metric: "p99_ttft_ms", observed: math.NaN(), threshold: 2, wantErr: true},
		{name: "infinity", metric: "p99_ttft_ms", observed: 1, threshold: math.Inf(1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ComparePercentile(test.metric, test.observed, test.threshold)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ComparePercentile() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Metric != test.metric || result.Observed != test.observed || result.Threshold != test.threshold || result.Operator != "<=" || result.Pass != test.wantPass {
				t.Fatalf("ComparePercentile() = %#v, want pass=%t and populated result", result, test.wantPass)
			}
		})
	}
}

func TestEvaluateGatesSemanticTTFTAndNotTTFB(t *testing.T) {
	threshold := 800.0
	observed := 801.0
	results, err := Evaluate(config.SLO{P99TTFTMS: &threshold}, Summary{P99TTFTMS: &observed, TTFTSamples: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Metric != "p99_ttft_ms" || results[0].Pass {
		t.Fatalf("Evaluate() = %#v, want one failing semantic TTFT result", results)
	}
}

func TestEvaluateRejectsMissingConfiguredObservation(t *testing.T) {
	threshold := 0.50
	_, err := Evaluate(config.SLO{MaxCostUSD: &threshold}, Summary{})
	if err == nil || !strings.Contains(err.Error(), "max_cost_usd") {
		t.Fatalf("Evaluate() error = %v, want missing cost observation", err)
	}
}

func TestEvaluateRejectsZeroSampleRate(t *testing.T) {
	threshold := 0.01
	_, err := Evaluate(config.SLO{MaxErrorRate: &threshold}, Summary{})
	if err == nil || !strings.Contains(err.Error(), "max_error_rate") {
		t.Fatalf("Evaluate() error = %v, want missing rate observation", err)
	}
}

func TestEvaluateIncludesAllConfiguredGatesAndSampleCounts(t *testing.T) {
	ttftThreshold, itlThreshold := 800.0, 120.0
	errorThreshold, droppedThreshold, costThreshold := 0.01, 0.02, 0.50
	ttft, itl, cost := 700.0, 100.0, 0.25
	results, err := Evaluate(config.SLO{
		P99TTFTMS:      &ttftThreshold,
		P99ChunkITLMS:  &itlThreshold,
		MaxErrorRate:   &errorThreshold,
		MaxDroppedRate: &droppedThreshold,
		MaxCostUSD:     &costThreshold,
	}, Summary{
		P99TTFTMS:         &ttft,
		P99ChunkITLMS:     &itl,
		TTFTSamples:       10,
		ChunkITLSamples:   20,
		ErrorRate:         0,
		DroppedRate:       0,
		ScheduledRequests: 10,
		CostUSD:           &cost,
		UsageSamples:      10,
		CostComplete:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 {
		t.Fatalf("len(results) = %d, want 5", len(results))
	}
	for _, result := range results {
		if !result.Pass || result.SampleCount == 0 {
			t.Fatalf("result = %#v, want passing populated gate", result)
		}
	}
}

func TestEvaluateRejectsPartialCostUsage(t *testing.T) {
	threshold, partialCost := 0.50, 0.10
	_, err := Evaluate(config.SLO{MaxCostUSD: &threshold}, Summary{
		CostUSD:      &partialCost,
		UsageSamples: 1,
		CostComplete: false,
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete usage") {
		t.Fatalf("Evaluate() error = %v, want incomplete usage error", err)
	}
}
