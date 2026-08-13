package slo

import (
	"fmt"
	"math"

	"github.com/crypticseeds/llm-slo-bench/internal/config"
)

type Summary struct {
	P99TTFTMS         *float64
	P99ChunkITLMS     *float64
	TTFTSamples       int
	ChunkITLSamples   int
	ErrorRate         float64
	DroppedRate       float64
	ScheduledRequests int
	CostUSD           *float64
	UsageSamples      int
	CostComplete      bool
}

type Result struct {
	Metric      string
	Observed    float64
	Operator    string
	Threshold   float64
	SampleCount int
	Pass        bool
}

func Evaluate(declared config.SLO, summary Summary) ([]Result, error) {
	var results []Result
	checks := []struct {
		metric    string
		threshold *float64
		observed  *float64
		samples   int
	}{
		{metric: "p99_ttft_ms", threshold: declared.P99TTFTMS, observed: summary.P99TTFTMS, samples: summary.TTFTSamples},
		{metric: "p99_chunk_itl_ms", threshold: declared.P99ChunkITLMS, observed: summary.P99ChunkITLMS, samples: summary.ChunkITLSamples},
		{metric: "max_error_rate", threshold: declared.MaxErrorRate, observed: &summary.ErrorRate, samples: summary.ScheduledRequests},
		{metric: "max_dropped_rate", threshold: declared.MaxDroppedRate, observed: &summary.DroppedRate, samples: summary.ScheduledRequests},
		{metric: "max_cost_usd", threshold: declared.MaxCostUSD, observed: summary.CostUSD, samples: summary.UsageSamples},
	}
	for _, check := range checks {
		if check.threshold == nil {
			continue
		}
		if check.observed == nil || check.samples == 0 {
			return nil, fmt.Errorf("configured metric %s has no observation", check.metric)
		}
		if check.metric == "max_cost_usd" && !summary.CostComplete {
			return nil, fmt.Errorf("configured metric %s has incomplete usage", check.metric)
		}
		result, err := ComparePercentile(check.metric, *check.observed, *check.threshold)
		if err != nil {
			return nil, err
		}
		result.SampleCount = check.samples
		results = append(results, result)
	}
	return results, nil
}

// ComparePercentile evaluates an upper-bound SLO using observed <= threshold.
//
// It returns a Result with operator "<=" and rejects an empty metric name,
// NaN, infinity, or negative observed/threshold values. Equality passes.
// It is used for percentile latency and scalar rate/cost ceilings; despite the
// name, it does not calculate a percentile.
//
// Examples: ("p99_ttft_ms", 799, 800) passes; ("p99_ttft_ms", 801, 800)
// fails; ("p99_ttft_ms", 800, 800) passes.
func ComparePercentile(metric string, observed, threshold float64) (Result, error) {
	if metric == "" {
		return Result{}, fmt.Errorf("metric must not be empty")
	}
	if math.IsNaN(observed) || math.IsInf(observed, 0) || observed < 0 {
		return Result{}, fmt.Errorf("observed value for %s must be finite and non-negative", metric)
	}
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 {
		return Result{}, fmt.Errorf("threshold for %s must be finite and non-negative", metric)
	}

	return Result{
		Metric:    metric,
		Observed:  observed,
		Operator:  "<=",
		Threshold: threshold,
		Pass:      observed <= threshold,
	}, nil
}
