package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"

	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

const (
	SchemaVersion           = 1
	LowestTrackableMicros   = int64(1)
	HighestTrackableMicros  = int64(time.Hour / time.Microsecond)
	SignificantFigures      = 3
	tokensPerSecondScale    = 1000
	histogramDurationUnit   = "ms"
	histogramThroughputUnit = "tokens_per_second"
)

type Outcome string

const (
	OutcomeSuccess     Outcome = "success"
	OutcomeDropped     Outcome = "dropped"
	OutcomeCanceled    Outcome = "canceled"
	OutcomeTimeout     Outcome = "timeout"
	OutcomeStreamError Outcome = "stream_error"
	OutcomeHTTPError   Outcome = "http_error"
)

type Pricing struct {
	InputUSDPerMillionTokens  float64
	OutputUSDPerMillionTokens float64
}

type Counts struct {
	Scheduled   int64   `json:"scheduled"`
	Started     int64   `json:"started"`
	Success     int64   `json:"success"`
	Dropped     int64   `json:"dropped"`
	Canceled    int64   `json:"canceled"`
	Timeout     int64   `json:"timeout"`
	StreamError int64   `json:"stream_error"`
	HTTPError   int64   `json:"http_error"`
	ErrorRate   float64 `json:"error_rate"`
	DroppedRate float64 `json:"dropped_rate"`
}

type HistogramSummary struct {
	Count              int64   `json:"count"`
	Min                float64 `json:"min"`
	Max                float64 `json:"max"`
	Mean               float64 `json:"mean"`
	P50                float64 `json:"p50"`
	P90                float64 `json:"p90"`
	P95                float64 `json:"p95"`
	P99                float64 `json:"p99"`
	LowestTrackable    float64 `json:"lowest_trackable"`
	HighestTrackable   float64 `json:"highest_trackable"`
	SignificantFigures int     `json:"significant_figures"`
	Unit               string  `json:"unit"`
}

type MetricSummaries struct {
	TTFB            *HistogramSummary `json:"ttfb"`
	TTFT            *HistogramSummary `json:"ttft"`
	ChunkITL        *HistogramSummary `json:"chunk_itl"`
	RequestDuration *HistogramSummary `json:"request_duration"`
	TokensPerSecond *HistogramSummary `json:"tokens_per_second"`
}

type UsageSummary struct {
	Samples          int64    `json:"samples"`
	Complete         bool     `json:"complete"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	TotalTokens      int64    `json:"total_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
}

type SLOStatus string

const (
	SLOStatusPending SLOStatus = "pending"
	SLOStatusPass    SLOStatus = "pass"
	SLOStatusFail    SLOStatus = "fail"
)

type SLOOutcome struct {
	Metric      string    `json:"metric"`
	Observed    float64   `json:"observed"`
	Operator    string    `json:"operator"`
	Threshold   float64   `json:"threshold"`
	SampleCount int       `json:"sample_count"`
	Status      SLOStatus `json:"status"`
	Pass        *bool     `json:"pass"`
}

type RunSummary struct {
	SchemaVersion int             `json:"schema_version"`
	Counts        Counts          `json:"counts"`
	Metrics       MetricSummaries `json:"metrics"`
	Usage         UsageSummary    `json:"usage"`
	SLOOutcomes   []SLOOutcome    `json:"slo_outcomes"`
}

type Aggregator struct {
	commands chan any

	lifecycle sync.RWMutex
	closed    bool
	final     RunSummary
}

type aggregateState struct {
	pricing Pricing
	counts  Counts

	ttfb            *hdrhistogram.Histogram
	ttft            *hdrhistogram.Histogram
	chunkITL        *hdrhistogram.Histogram
	requestDuration *hdrhistogram.Histogram
	tokensPerSecond *hdrhistogram.Histogram

	usageSamples     int64
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	costUSD          float64
}

type recordCommand struct {
	result  probe.Result
	outcome Outcome
	values  metricValues
	done    chan struct{}
}

type droppedCommand struct {
	done chan struct{}
}

type summaryCommand struct {
	result chan RunSummary
}

type closeCommand struct {
	result chan RunSummary
}

func NewAggregator(pricing Pricing) (*Aggregator, error) {
	if !validPrice(pricing.InputUSDPerMillionTokens) || !validPrice(pricing.OutputUSDPerMillionTokens) {
		return nil, errors.New("token prices must be finite and non-negative")
	}
	aggregator := &Aggregator{commands: make(chan any)}
	go aggregator.run(pricing)
	return aggregator, nil
}

// Record adds one started request. Calls may be made concurrently; the owner
// goroutine serializes histogram mutation and counter updates.
func (a *Aggregator) Record(result probe.Result, requestErr error) error {
	outcome := ClassifyOutcome(result, requestErr)
	values, err := valuesFor(result, outcome)
	if err != nil {
		return err
	}

	a.lifecycle.RLock()
	defer a.lifecycle.RUnlock()
	if a.closed {
		return errors.New("metrics aggregator is closed")
	}
	done := make(chan struct{})
	a.commands <- recordCommand{result: result, outcome: outcome, values: values, done: done}
	<-done
	return nil
}

// RecordDropped adds an arrival rejected before a request was started.
func (a *Aggregator) RecordDropped() error {
	a.lifecycle.RLock()
	defer a.lifecycle.RUnlock()
	if a.closed {
		return errors.New("metrics aggregator is closed")
	}
	done := make(chan struct{})
	a.commands <- droppedCommand{done: done}
	<-done
	return nil
}

// Summary returns a detached value snapshot. Empty histograms and unavailable
// cost remain nil so SLO evaluation can distinguish zero from no observation.
func (a *Aggregator) Summary() RunSummary {
	a.lifecycle.RLock()
	defer a.lifecycle.RUnlock()
	if a.closed {
		return cloneSummary(a.final)
	}
	result := make(chan RunSummary)
	a.commands <- summaryCommand{result: result}
	return <-result
}

// Close stops the owner goroutine and returns the final immutable summary.
func (a *Aggregator) Close() RunSummary {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	if a.closed {
		return cloneSummary(a.final)
	}
	result := make(chan RunSummary)
	a.commands <- closeCommand{result: result}
	a.final = cloneSummary(<-result)
	a.closed = true
	return cloneSummary(a.final)
}

func (a *Aggregator) run(pricing Pricing) {
	state := aggregateState{
		pricing:         pricing,
		ttfb:            newHistogram(),
		ttft:            newHistogram(),
		chunkITL:        newHistogram(),
		requestDuration: newHistogram(),
		tokensPerSecond: newHistogram(),
	}
	for command := range a.commands {
		switch command := command.(type) {
		case recordCommand:
			state.record(command.result, command.outcome, command.values)
			close(command.done)
		case droppedCommand:
			state.counts.Scheduled++
			state.counts.Dropped++
			close(command.done)
		case summaryCommand:
			command.result <- state.summary()
		case closeCommand:
			command.result <- state.summary()
			return
		}
	}
}

func (s *aggregateState) record(result probe.Result, outcome Outcome, values metricValues) {
	s.counts.Scheduled++
	s.counts.Started++
	s.incrementOutcome(outcome)
	if outcome != OutcomeSuccess {
		return
	}

	recordValues(s.ttfb, values.ttfb)
	recordValues(s.ttft, values.ttft)
	recordValues(s.chunkITL, values.chunkITL...)
	recordValues(s.requestDuration, values.duration)
	if values.tokensPerSecond > 0 {
		recordValues(s.tokensPerSecond, values.tokensPerSecond)
	}
	if result.Usage != nil {
		s.usageSamples++
		s.promptTokens += int64(result.Usage.PromptTokens)
		s.completionTokens += int64(result.Usage.CompletionTokens)
		s.totalTokens += int64(result.Usage.TotalTokens)
		s.costUSD += tokenCost(result.Usage, s.pricing)
	}
}

func (s *aggregateState) summary() RunSummary {
	counts := s.counts
	if counts.Scheduled > 0 {
		failed := counts.Timeout + counts.StreamError + counts.HTTPError
		counts.ErrorRate = float64(failed) / float64(counts.Scheduled)
		counts.DroppedRate = float64(counts.Dropped) / float64(counts.Scheduled)
	}

	usage := UsageSummary{
		Samples:          s.usageSamples,
		Complete:         counts.Success > 0 && s.usageSamples == counts.Success,
		PromptTokens:     s.promptTokens,
		CompletionTokens: s.completionTokens,
		TotalTokens:      s.totalTokens,
	}
	if s.usageSamples > 0 {
		cost := s.costUSD
		usage.CostUSD = &cost
	}

	return RunSummary{
		SchemaVersion: SchemaVersion,
		Counts:        counts,
		Metrics: MetricSummaries{
			TTFB:            summarize(s.ttfb, 1000, histogramDurationUnit),
			TTFT:            summarize(s.ttft, 1000, histogramDurationUnit),
			ChunkITL:        summarize(s.chunkITL, 1000, histogramDurationUnit),
			RequestDuration: summarize(s.requestDuration, 1000, histogramDurationUnit),
			TokensPerSecond: summarize(s.tokensPerSecond, tokensPerSecondScale, histogramThroughputUnit),
		},
		Usage:       usage,
		SLOOutcomes: make([]SLOOutcome, 0),
	}
}

func ClassifyOutcome(result probe.Result, requestErr error) Outcome {
	if requestErr == nil && result.StatusCode == 200 {
		return OutcomeSuccess
	}
	if result.StatusCode != 0 && result.StatusCode != 200 {
		return OutcomeHTTPError
	}
	var netErr net.Error
	if errors.Is(requestErr, context.DeadlineExceeded) || errors.As(requestErr, &netErr) && netErr.Timeout() ||
		requestErr != nil && strings.HasPrefix(requestErr.Error(), "stream idle timeout after ") {
		return OutcomeTimeout
	}
	if errors.Is(requestErr, context.Canceled) {
		return OutcomeCanceled
	}
	return OutcomeStreamError
}

func validPrice(price float64) bool {
	return price >= 0 && !math.IsNaN(price) && !math.IsInf(price, 0)
}

type metricValues struct {
	ttfb            int64
	ttft            int64
	chunkITL        []int64
	duration        int64
	tokensPerSecond int64
}

func valuesFor(result probe.Result, outcome Outcome) (metricValues, error) {
	if outcome != OutcomeSuccess {
		return metricValues{}, nil
	}
	if result.Usage != nil && (result.Usage.PromptTokens < 0 || result.Usage.CompletionTokens < 0 || result.Usage.TotalTokens < 0) {
		return metricValues{}, errors.New("token counts must not be negative")
	}

	values := metricValues{chunkITL: make([]int64, len(result.ChunkITL))}
	var err error
	if values.ttfb, err = durationMicros("ttfb", result.TTFB); err != nil {
		return metricValues{}, err
	}
	if values.ttft, err = durationMicros("ttft", result.TTFT); err != nil {
		return metricValues{}, err
	}
	for i, duration := range result.ChunkITL {
		if values.chunkITL[i], err = durationMicros("chunk itl", duration); err != nil {
			return metricValues{}, err
		}
	}
	if values.duration, err = durationMicros("request duration", result.Duration); err != nil {
		return metricValues{}, err
	}
	if result.Usage != nil && result.Usage.CompletionTokens > 0 {
		generationDuration := result.Duration - result.TTFT
		if generationDuration > 0 {
			tokensPerSecond := float64(result.Usage.CompletionTokens) / generationDuration.Seconds()
			values.tokensPerSecond = int64(math.Round(tokensPerSecond * tokensPerSecondScale))
			if values.tokensPerSecond < LowestTrackableMicros || values.tokensPerSecond > HighestTrackableMicros {
				return metricValues{}, fmt.Errorf("tokens per second %.3f is outside histogram range", tokensPerSecond)
			}
		}
	}
	return values, nil
}

func durationMicros(name string, duration time.Duration) (int64, error) {
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	micros := int64(math.Ceil(float64(duration) / float64(time.Microsecond)))
	if micros > HighestTrackableMicros {
		return 0, fmt.Errorf("%s %s exceeds histogram maximum %s", name, duration, time.Hour)
	}
	return micros, nil
}

func newHistogram() *hdrhistogram.Histogram {
	return hdrhistogram.New(LowestTrackableMicros, HighestTrackableMicros, SignificantFigures)
}

func recordValues(histogram *hdrhistogram.Histogram, values ...int64) {
	for _, value := range values {
		// Values are range-checked before they reach the owner goroutine.
		_ = histogram.RecordValue(value)
	}
}

func summarize(histogram *hdrhistogram.Histogram, scale float64, unit string) *HistogramSummary {
	if histogram.TotalCount() == 0 {
		return nil
	}
	return &HistogramSummary{
		Count:              histogram.TotalCount(),
		Min:                float64(histogram.Min()) / scale,
		Max:                float64(histogram.Max()) / scale,
		Mean:               histogram.Mean() / scale,
		P50:                float64(histogram.ValueAtPercentile(50)) / scale,
		P90:                float64(histogram.ValueAtPercentile(90)) / scale,
		P95:                float64(histogram.ValueAtPercentile(95)) / scale,
		P99:                float64(histogram.ValueAtPercentile(99)) / scale,
		LowestTrackable:    float64(LowestTrackableMicros) / scale,
		HighestTrackable:   float64(HighestTrackableMicros) / scale,
		SignificantFigures: SignificantFigures,
		Unit:               unit,
	}
}

func tokenCost(usage *probe.Usage, pricing Pricing) float64 {
	return (float64(usage.PromptTokens)*pricing.InputUSDPerMillionTokens +
		float64(usage.CompletionTokens)*pricing.OutputUSDPerMillionTokens) / 1_000_000
}

func cloneSummary(summary RunSummary) RunSummary {
	clone := summary
	clone.Metrics.TTFB = cloneHistogramSummary(summary.Metrics.TTFB)
	clone.Metrics.TTFT = cloneHistogramSummary(summary.Metrics.TTFT)
	clone.Metrics.ChunkITL = cloneHistogramSummary(summary.Metrics.ChunkITL)
	clone.Metrics.RequestDuration = cloneHistogramSummary(summary.Metrics.RequestDuration)
	clone.Metrics.TokensPerSecond = cloneHistogramSummary(summary.Metrics.TokensPerSecond)
	if summary.Usage.CostUSD != nil {
		cost := *summary.Usage.CostUSD
		clone.Usage.CostUSD = &cost
	}
	clone.SLOOutcomes = make([]SLOOutcome, len(summary.SLOOutcomes))
	copy(clone.SLOOutcomes, summary.SLOOutcomes)
	for i, outcome := range summary.SLOOutcomes {
		if outcome.Pass != nil {
			pass := *outcome.Pass
			clone.SLOOutcomes[i].Pass = &pass
		}
	}
	return clone
}

func cloneHistogramSummary(summary *HistogramSummary) *HistogramSummary {
	if summary == nil {
		return nil
	}
	clone := *summary
	return &clone
}

func (s *aggregateState) incrementOutcome(outcome Outcome) {
	switch outcome {
	case OutcomeSuccess:
		s.counts.Success++
	case OutcomeCanceled:
		s.counts.Canceled++
	case OutcomeTimeout:
		s.counts.Timeout++
	case OutcomeHTTPError:
		s.counts.HTTPError++
	default:
		s.counts.StreamError++
	}
}
