package metrics

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

func TestAggregatorSummaryUsesScheduledDenominatorsAndExplicitUsage(t *testing.T) {
	aggregator := newTestAggregator(t)
	withUsage := successfulResult(10 * time.Millisecond)
	withUsage.Usage = &probe.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}
	withoutUsage := successfulResult(20 * time.Millisecond)

	for _, request := range []struct {
		result probe.Result
		err    error
	}{
		{result: withUsage},
		{result: withoutUsage},
		{result: probe.Result{StatusCode: 503}, err: errors.New("service unavailable")},
		{err: context.DeadlineExceeded},
		{result: probe.Result{StatusCode: 200}, err: errors.New("stream ended before [DONE]")},
	} {
		if err := aggregator.Record(request.result, request.err); err != nil {
			t.Fatal(err)
		}
	}
	if err := aggregator.RecordDropped(); err != nil {
		t.Fatal(err)
	}

	summary := aggregator.Summary()
	if summary.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", summary.SchemaVersion, SchemaVersion)
	}
	wantCounts := Counts{Scheduled: 6, Started: 5, Success: 2, Dropped: 1, Timeout: 1, StreamError: 1, HTTPError: 1}
	if summary.Counts.Scheduled != wantCounts.Scheduled || summary.Counts.Started != wantCounts.Started ||
		summary.Counts.Success != wantCounts.Success || summary.Counts.Dropped != wantCounts.Dropped ||
		summary.Counts.Timeout != wantCounts.Timeout || summary.Counts.StreamError != wantCounts.StreamError ||
		summary.Counts.HTTPError != wantCounts.HTTPError {
		t.Fatalf("Counts = %#v, want core counters %#v", summary.Counts, wantCounts)
	}
	assertNear(t, summary.Counts.ErrorRate, 3.0/6.0, 1e-12)
	assertNear(t, summary.Counts.DroppedRate, 1.0/6.0, 1e-12)

	if summary.Metrics.TTFT == nil || summary.Metrics.TTFT.Count != 2 {
		t.Fatalf("TTFT = %#v, want two successful samples", summary.Metrics.TTFT)
	}
	if summary.Metrics.TTFT.LowestTrackable != 0.001 || summary.Metrics.TTFT.HighestTrackable != 3_600_000 ||
		summary.Metrics.TTFT.SignificantFigures != 3 {
		t.Fatalf("TTFT histogram contract = %#v, want 1us..1h and 3 significant figures", summary.Metrics.TTFT)
	}
	if summary.Metrics.ChunkITL == nil || summary.Metrics.ChunkITL.Count != 4 {
		t.Fatalf("ChunkITL = %#v, want four pooled samples", summary.Metrics.ChunkITL)
	}
	if summary.Metrics.TokensPerSecond == nil || summary.Metrics.TokensPerSecond.Count != 1 {
		t.Fatalf("TokensPerSecond = %#v, want one usage-backed sample", summary.Metrics.TokensPerSecond)
	}
	if summary.Usage.Samples != 1 || summary.Usage.Complete {
		t.Fatalf("Usage = %#v, want one incomplete sample", summary.Usage)
	}
	if summary.Usage.CostUSD == nil {
		t.Fatal("CostUSD = nil, want partial cost")
	}
	wantCost := (100*0.15 + 20*0.60) / 1_000_000.0
	assertNear(t, *summary.Usage.CostUSD, wantCost, 1e-12)
}

func TestAggregatorPercentilesStayWithinDeclaredPrecision(t *testing.T) {
	aggregator := newTestAggregator(t)
	for i := 1; i <= 100; i++ {
		result := successfulResult(time.Duration(i) * time.Millisecond)
		result.Duration = result.TTFT + 200*time.Millisecond
		if err := aggregator.Record(result, nil); err != nil {
			t.Fatal(err)
		}
	}

	ttft := aggregator.Summary().Metrics.TTFT
	if ttft == nil {
		t.Fatal("TTFT = nil, want histogram summary")
	}
	for name, check := range map[string]struct {
		got  float64
		want float64
	}{
		"p50": {got: ttft.P50, want: 50},
		"p90": {got: ttft.P90, want: 90},
		"p95": {got: ttft.P95, want: 95},
		"p99": {got: ttft.P99, want: 99},
	} {
		if relativeError(check.got, check.want) > 0.001 {
			t.Errorf("%s = %.6fms, want %.6fms within 0.1%%", name, check.got, check.want)
		}
	}
}

func TestHistogramDistributionExportIsRealAndBounded(t *testing.T) {
	empty := summarize(newHistogram(), 1000, histogramDurationUnit)
	if empty != nil {
		t.Fatalf("empty summary = %#v, want nil", empty)
	}

	single := newHistogram()
	recordValues(single, 12_500)
	singleDistribution := summarize(single, 1000, histogramDurationUnit).Distribution
	singleValue := float64(single.ValueAtPercentile(100)) / 1000
	if len(singleDistribution) != 2 || singleDistribution[0].Percentile != 0 || singleDistribution[1].Percentile != 100 ||
		singleDistribution[0].Value != singleValue || singleDistribution[1].Value != singleValue {
		t.Fatalf("single-sample distribution = %#v, want 0%% and 100%% at %.3fms", singleDistribution, singleValue)
	}

	histogram := newHistogram()
	for value := int64(1); value <= 10_000; value++ {
		recordValues(histogram, value)
	}
	distribution := summarize(histogram, 1000, histogramDurationUnit).Distribution
	if len(distribution) > distributionPointCap {
		t.Fatalf("distribution points = %d, exceeds cap %d", len(distribution), distributionPointCap)
	}
	if len(distribution) < 2 || distribution[len(distribution)-1].Value != float64(histogram.Max())/1000 {
		t.Fatalf("distribution endpoint = %#v, want histogram max %.3f", distribution[len(distribution)-1], float64(histogram.Max())/1000)
	}
	for index := 1; index < len(distribution); index++ {
		if distribution[index].Percentile < distribution[index-1].Percentile || distribution[index].Value < distribution[index-1].Value {
			t.Fatalf("distribution is not monotonic at %d: %#v", index, distribution)
		}
	}
}

func TestAggregatorConcurrentRecordIsRaceFreeAndLossless(t *testing.T) {
	aggregator := newTestAggregator(t)
	const workers = 64
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wait.Done()
			<-start
			if err := aggregator.Record(successfulResult(10*time.Millisecond), nil); err != nil {
				t.Errorf("Record() error = %v", err)
			}
		}()
	}
	close(start)
	wait.Wait()

	summary := aggregator.Summary()
	if summary.Counts.Success != workers || summary.Metrics.TTFT == nil || summary.Metrics.TTFT.Count != workers {
		t.Fatalf("Summary = %#v, want %d successes and TTFT samples", summary, workers)
	}
}

func TestAggregatorLeavesEmptyMetricsAndCostNull(t *testing.T) {
	aggregator := newTestAggregator(t)
	if err := aggregator.Record(successfulResult(10*time.Millisecond), nil); err != nil {
		t.Fatal(err)
	}

	summary := aggregator.Summary()
	if summary.Usage.CostUSD != nil || summary.Usage.Complete || summary.Usage.Samples != 0 {
		t.Fatalf("Usage = %#v, want unavailable", summary.Usage)
	}
	if summary.Metrics.TokensPerSecond != nil {
		t.Fatalf("TokensPerSecond = %#v, want nil", summary.Metrics.TokensPerSecond)
	}
}

func TestAggregatorRejectsHistogramOverflowWithoutMutation(t *testing.T) {
	aggregator := newTestAggregator(t)
	result := successfulResult(10 * time.Millisecond)
	result.Duration = time.Hour + time.Microsecond
	if err := aggregator.Record(result, nil); err == nil {
		t.Fatal("Record() error = nil, want histogram overflow")
	}
	if summary := aggregator.Summary(); summary.Counts.Scheduled != 0 {
		t.Fatalf("Counts = %#v after rejected record, want zero", summary.Counts)
	}
}

func TestNewAggregatorRejectsInvalidPricing(t *testing.T) {
	tests := map[string]Pricing{
		"negative input":  {InputUSDPerMillionTokens: -1},
		"NaN input":       {InputUSDPerMillionTokens: math.NaN()},
		"infinite input":  {InputUSDPerMillionTokens: math.Inf(1)},
		"NaN output":      {OutputUSDPerMillionTokens: math.NaN()},
		"infinite output": {OutputUSDPerMillionTokens: math.Inf(1)},
	}
	for name, pricing := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAggregator(pricing); err == nil {
				t.Fatal("NewAggregator() error = nil, want invalid price error")
			}
		})
	}
}

func TestClassifyOutcomeRecognizesProbeIdleTimeout(t *testing.T) {
	got := ClassifyOutcome(probe.Result{StatusCode: 200}, errors.New("stream idle timeout after 5s"))
	if got != OutcomeTimeout {
		t.Fatalf("ClassifyOutcome() = %q, want %q", got, OutcomeTimeout)
	}
}

func TestAggregatorTracksCancellationWithoutCountingItAsError(t *testing.T) {
	aggregator := newTestAggregator(t)
	for _, requestErr := range []error{context.Canceled, context.DeadlineExceeded} {
		if err := aggregator.Record(probe.Result{}, requestErr); err != nil {
			t.Fatal(err)
		}
	}
	if err := aggregator.RecordDropped(); err != nil {
		t.Fatal(err)
	}

	summary := aggregator.Summary()
	if summary.Counts.Canceled != 1 || summary.Counts.Timeout != 1 {
		t.Fatalf("Counts = %#v, want one cancellation and one timeout", summary.Counts)
	}
	assertNear(t, summary.Counts.ErrorRate, 1.0/3.0, 1e-12)
	assertNear(t, summary.Counts.DroppedRate, 1.0/3.0, 1e-12)
}

func TestAggregatorCloseReturnsFinalSummaryAndRejectsNewRecords(t *testing.T) {
	aggregator := newTestAggregator(t)
	if err := aggregator.Record(successfulResult(10*time.Millisecond), nil); err != nil {
		t.Fatal(err)
	}
	final := aggregator.Close()
	if final.Counts.Success != 1 || aggregator.Summary().Counts.Success != 1 {
		t.Fatalf("final summary = %#v, want one success", final)
	}
	final.Metrics.TTFT.P99 = -1
	if got := aggregator.Summary().Metrics.TTFT.P99; got < 0 {
		t.Fatalf("closed Summary() shared mutable histogram pointer, P99 = %f", got)
	}
	if err := aggregator.RecordDropped(); err == nil {
		t.Fatal("RecordDropped() after Close error = nil")
	}
}

func successfulResult(ttft time.Duration) probe.Result {
	return probe.Result{
		StatusCode:    200,
		TTFB:          2 * time.Millisecond,
		TTFT:          ttft,
		ChunkITL:      []time.Duration{3 * time.Millisecond, 4 * time.Millisecond},
		ContentEvents: 3,
		Duration:      ttft + 100*time.Millisecond,
	}
}

func newTestAggregator(t *testing.T) *Aggregator {
	t.Helper()
	aggregator, err := NewAggregator(Pricing{
		InputUSDPerMillionTokens:  0.15,
		OutputUSDPerMillionTokens: 0.60,
	})
	if err != nil {
		t.Fatal(err)
	}
	return aggregator
}

func assertNear(t *testing.T, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("got %.12f, want %.12f (+/- %.12f)", got, want, tolerance)
	}
}

func relativeError(got, want float64) float64 {
	return math.Abs(got-want) / want
}
