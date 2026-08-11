package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/config"
	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

const (
	DefaultMaxScheduleLag = 100 * time.Millisecond
	maxErrorMessageBytes  = 160
)

var errDispatchLate = errors.New("request dispatch exceeded schedule lag")

type DropReason string

const (
	DropLate      DropReason = "late"
	DropSaturated DropReason = "saturated"
)

type StopReason string

const (
	StopNone                  StopReason = ""
	StopRequestLimit          StopReason = "request_limit"
	StopDurationLimit         StopReason = "duration_limit"
	StopCostLimit             StopReason = "cost_limit"
	StopCostReservationBreach StopReason = "cost_reservation_breach"
)

type ErrorClass string

const (
	ErrorNone      ErrorClass = ""
	ErrorCancelled ErrorClass = "cancelled"
	ErrorTimeout   ErrorClass = "timeout"
	ErrorHTTP      ErrorClass = "http"
	ErrorStream    ErrorClass = "stream"
	ErrorUsage     ErrorClass = "usage"
)

type Config struct {
	Load                  config.Load
	Safety                config.Safety
	Pricing               config.Pricing
	Probe                 probe.Config
	MaxScheduleLag        time.Duration
	ResponseHeaderTimeout time.Duration

	clock  clock
	runner func(context.Context, probe.Config) (probe.Result, error)
}

type Counts struct {
	Scheduled int
	Started   int
	Completed int
	Succeeded int
	Failed    int
	Cancelled int
	Dropped   int
}

type Outcome struct {
	Sequence        int
	IntendedArrival time.Time
	Dispatch        time.Time
	ScheduleLag     time.Duration
	Dropped         bool
	DropReason      DropReason
	Cancelled       bool
	ErrorClass      ErrorClass
	Result          probe.Result
	Error           string
}

type Result struct {
	StartedAt       time.Time
	FinishedAt      time.Time
	Counts          Counts
	Outcomes        []Outcome
	StopReason      StopReason
	KnownCostUSD    float64
	SafetyBudgetUSD float64
}

type clock interface {
	Now() time.Time
	WaitUntil(context.Context, time.Time) error
}

type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}

func (realClock) WaitUntil(ctx context.Context, deadline time.Time) error {
	delay := time.Until(deadline)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Schedule returns absolute offsets from a run's start. Each stage linearly
// moves from the previous stage's target RPS to its own target RPS.
func Schedule(stages []config.Stage) ([]time.Duration, error) {
	schedule, err := newSchedule(stages)
	if err != nil {
		return nil, err
	}
	var offsets []time.Duration
	for {
		offset, ok, err := schedule.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return offsets, nil
		}
		offsets = append(offsets, offset)
	}
}

type rampSchedule struct {
	stages       []config.Stage
	stage        int
	stageStart   time.Duration
	previousRate float64
	cumulative   float64
	nextArrival  float64
}

func newSchedule(stages []config.Stage) (*rampSchedule, error) {
	if len(stages) == 0 {
		return nil, errors.New("load stages must not be empty")
	}
	for i, stage := range stages {
		if stage.Duration.Duration <= 0 {
			return nil, fmt.Errorf("load stage %d duration must be positive", i)
		}
		if stage.TargetRPS <= 0 || math.IsNaN(stage.TargetRPS) || math.IsInf(stage.TargetRPS, 0) {
			return nil, fmt.Errorf("load stage %d target RPS must be finite and positive", i)
		}
	}
	return &rampSchedule{stages: stages, nextArrival: 1}, nil
}

func (s *rampSchedule) next() (time.Duration, bool, error) {
	for s.stage < len(s.stages) {
		stage := s.stages[s.stage]
		duration := stage.Duration.Duration
		seconds := duration.Seconds()
		slope := (stage.TargetRPS - s.previousRate) / seconds
		stageVolume := (s.previousRate + stage.TargetRPS) * seconds / 2
		stageEndVolume := s.cumulative + stageVolume
		if s.nextArrival <= stageEndVolume+1e-9 {
			withinStage := s.nextArrival - s.cumulative
			secondsFromStart := arrivalTime(s.previousRate, slope, withinStage)
			if secondsFromStart < 0 || secondsFromStart > seconds+1e-9 {
				return 0, false, fmt.Errorf("compute arrival %.0f in load stage %d", s.nextArrival, s.stage)
			}
			offset := s.stageStart + time.Duration(secondsFromStart*float64(time.Second))
			if offset > s.stageStart+duration {
				offset = s.stageStart + duration
			}
			s.nextArrival++
			return offset, true, nil
		}

		s.cumulative = stageEndVolume
		s.stageStart += duration
		s.previousRate = stage.TargetRPS
		s.stage++
	}
	return 0, false, nil
}

func arrivalTime(initialRate, slope, volume float64) float64 {
	if math.Abs(slope) < 1e-12 {
		return volume / initialRate
	}
	discriminant := initialRate*initialRate + 2*slope*volume
	if discriminant < 0 && discriminant > -1e-9 {
		discriminant = 0
	}
	return 2 * volume / (initialRate + math.Sqrt(discriminant))
}

type completion struct {
	outcome Outcome
	costUSD float64
	hasCost bool
}

// Run offers the configured schedule independently of request completion.
// Admission is immediate: an arrival that cannot acquire an in-flight slot is
// recorded as dropped instead of waiting in a work queue.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.MaxScheduleLag == 0 {
		cfg.MaxScheduleLag = DefaultMaxScheduleLag
	}
	if cfg.ResponseHeaderTimeout == 0 {
		cfg.ResponseHeaderTimeout = cfg.Probe.Timeout
	}
	if err := validate(cfg); err != nil {
		return Result{}, err
	}

	clk := cfg.clock
	if clk == nil {
		clk = realClock{}
	}
	runner := cfg.runner
	if runner == nil {
		runner = probe.Run
	}
	if cfg.Probe.HTTPClient == nil {
		client := newHTTPClient(cfg.Load.MaxInFlight, cfg.ResponseHeaderTimeout)
		cfg.Probe.HTTPClient = client
		defer client.CloseIdleConnections()
	}
	schedule, err := newSchedule(cfg.Load.Stages)
	if err != nil {
		return Result{}, err
	}

	start := clk.Now()
	result := Result{StartedAt: start}
	semaphore := make(chan struct{}, cfg.Load.MaxInFlight)
	completions := make(chan completion, cfg.Load.MaxInFlight)
	var inFlight int
	var heldCost float64

	collect := func(item completion) {
		inFlight--
		<-semaphore
		if item.outcome.Dropped {
			result.Counts.Started--
			result.Counts.Dropped++
			heldCost -= cfg.Safety.ReserveCostPerRequestUSD
			result.Outcomes = append(result.Outcomes, item.outcome)
			return
		}
		result.Counts.Completed++
		if item.outcome.Cancelled {
			result.Counts.Cancelled++
		} else if item.outcome.Error != "" {
			result.Counts.Failed++
		} else {
			result.Counts.Succeeded++
		}
		if item.hasCost {
			result.KnownCostUSD += item.costUSD
			heldCost += item.costUSD - cfg.Safety.ReserveCostPerRequestUSD
			if item.costUSD > cfg.Safety.ReserveCostPerRequestUSD {
				result.StopReason = StopCostReservationBreach
			}
		}
		result.Outcomes = append(result.Outcomes, item.outcome)
	}
	drain := func() {
		for {
			select {
			case item := <-completions:
				collect(item)
			default:
				return
			}
		}
	}

scheduleLoop:
	for sequence := 1; ; sequence++ {
		offset, ok, err := schedule.next()
		if err != nil {
			return result, err
		}
		if !ok {
			break
		}
		if result.Counts.Started >= cfg.Safety.MaxRequests {
			result.StopReason = StopRequestLimit
			break
		}
		intended := start.Add(offset)
		if intended.After(start.Add(cfg.Safety.MaxDuration.Duration)) {
			result.StopReason = StopDurationLimit
			break
		}
		if err := clk.WaitUntil(ctx, intended); err != nil {
			break
		}
		drain()
		if result.StopReason == StopCostReservationBreach {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if clk.Now().After(start.Add(cfg.Safety.MaxDuration.Duration)) {
			result.StopReason = StopDurationLimit
			break
		}
		if heldCost+cfg.Safety.ReserveCostPerRequestUSD > cfg.Safety.MaxCostUSD {
			result.StopReason = StopCostLimit
			break
		}

		result.Counts.Scheduled++
		now := clk.Now()
		lag := now.Sub(intended)
		if lag > cfg.MaxScheduleLag {
			result.Counts.Dropped++
			result.Outcomes = append(result.Outcomes, droppedOutcome(sequence, intended, now, lag, DropLate))
			continue
		}
		if ctx.Err() != nil {
			break
		}
		select {
		case semaphore <- struct{}{}:
		default:
			drain()
			if result.StopReason == StopCostReservationBreach {
				break scheduleLoop
			}
			if ctx.Err() != nil {
				break scheduleLoop
			}
			select {
			case semaphore <- struct{}{}:
			default:
				result.Counts.Dropped++
				result.Outcomes = append(result.Outcomes, droppedOutcome(sequence, intended, now, lag, DropSaturated))
				continue
			}
		}
		drain()
		if result.StopReason == StopCostReservationBreach {
			<-semaphore
			break
		}
		if ctx.Err() != nil {
			<-semaphore
			break
		}

		heldCost += cfg.Safety.ReserveCostPerRequestUSD
		workerReady := make(chan time.Time, 1)
		proceed := make(chan bool, 1)
		rejected := make(chan struct{})
		go func(sequence int, intended time.Time) {
			var probeResult probe.Result
			var runErr error
			dispatch := clk.Now()
			workerReady <- dispatch
			if !<-proceed {
				<-semaphore
				close(rejected)
				return
			}
			if err := ctx.Err(); err != nil {
				runErr = err
			} else {
				probeCfg := cfg.Probe
				probeCfg.HTTPClient = clientWithDispatchDeadline(probeCfg.HTTPClient, intended.Add(cfg.MaxScheduleLag))
				probeResult, runErr = runner(ctx, probeCfg)
			}
			if !probeResult.Dispatch.IsZero() {
				dispatch = probeResult.Dispatch
			}
			outcome := Outcome{
				Sequence:        sequence,
				IntendedArrival: intended,
				Dispatch:        dispatch,
				ScheduleLag:     dispatch.Sub(intended),
				Result:          probeResult,
			}
			if (!probeResult.Dispatch.IsZero() && outcome.ScheduleLag > cfg.MaxScheduleLag) || errors.Is(runErr, errDispatchLate) {
				outcome.Dropped = true
				outcome.DropReason = DropLate
			}
			if errors.Is(runErr, errDispatchLate) {
				outcome.ErrorClass = ErrorNone
			} else if runErr != nil {
				outcome.Cancelled = errors.Is(runErr, context.Canceled) || (errors.Is(runErr, context.DeadlineExceeded) && ctx.Err() != nil)
				outcome.ErrorClass = classifyError(runErr, outcome.Cancelled, probeResult.StatusCode)
				outcome.Error = sanitizedErrorMessage(outcome.ErrorClass, probeResult.StatusCode)
			} else if probeResult.Usage != nil && !validUsage(probeResult.Usage) {
				outcome.Error = "invalid usage token counts"
				outcome.ErrorClass = ErrorUsage
			}
			item := completion{outcome: outcome}
			if validUsage(probeResult.Usage) {
				item.hasCost = true
				item.costUSD = requestCost(probeResult.Usage, cfg.Pricing)
			}
			completions <- item
		}(sequence, intended)
		workerDispatch := <-workerReady
		workerLag := workerDispatch.Sub(intended)
		if workerLag > cfg.MaxScheduleLag || ctx.Err() != nil {
			proceed <- false
			<-rejected
			heldCost -= cfg.Safety.ReserveCostPerRequestUSD
			if ctx.Err() != nil {
				break
			}
			result.Counts.Dropped++
			result.Outcomes = append(result.Outcomes, droppedOutcome(sequence, intended, workerDispatch, workerLag, DropLate))
			continue
		}
		result.Counts.Started++
		inFlight++
		proceed <- true
	}

	for inFlight > 0 {
		collect(<-completions)
	}
	result.SafetyBudgetUSD = heldCost
	result.FinishedAt = clk.Now()
	sort.Slice(result.Outcomes, func(i, j int) bool {
		return result.Outcomes[i].Sequence < result.Outcomes[j].Sequence
	})
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func droppedOutcome(sequence int, intended, dispatch time.Time, lag time.Duration, reason DropReason) Outcome {
	return Outcome{
		Sequence:        sequence,
		IntendedArrival: intended,
		Dispatch:        dispatch,
		ScheduleLag:     lag,
		Dropped:         true,
		DropReason:      reason,
	}
}

func requestCost(usage *probe.Usage, pricing config.Pricing) float64 {
	return float64(usage.PromptTokens)*pricing.InputUSDPerMillionTokens/1_000_000 +
		float64(usage.CompletionTokens)*pricing.OutputUSDPerMillionTokens/1_000_000
}

func validUsage(usage *probe.Usage) bool {
	return usage != nil && usage.PromptTokens >= 0 && usage.CompletionTokens >= 0 && usage.TotalTokens >= 0
}

func classifyError(err error, cancelled bool, statusCode int) ErrorClass {
	if cancelled {
		return ErrorCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTimeout
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return ErrorTimeout
	}
	if errors.Is(err, probe.ErrStreamIdleTimeout) {
		return ErrorTimeout
	}
	if errors.Is(err, probe.ErrHTTPStatus) || statusCode != 0 && statusCode != http.StatusOK {
		return ErrorHTTP
	}
	return ErrorStream
}

func sanitizedErrorMessage(class ErrorClass, statusCode int) string {
	var message string
	switch class {
	case ErrorCancelled:
		message = "request cancelled"
	case ErrorTimeout:
		message = "request timed out"
	case ErrorHTTP:
		if statusCode != 0 {
			message = fmt.Sprintf("endpoint returned HTTP status %d", statusCode)
		} else {
			message = "endpoint returned an HTTP error"
		}
	case ErrorUsage:
		message = "invalid usage token counts"
	default:
		message = "stream request failed"
	}
	if len(message) > maxErrorMessageBytes {
		return message[:maxErrorMessageBytes]
	}
	return message
}

func newHTTPClient(maxInFlight int, responseHeaderTimeout time.Duration) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableCompression:    true,
		MaxIdleConns:          maxInFlight,
		MaxIdleConnsPerHost:   maxInFlight,
		MaxConnsPerHost:       maxInFlight,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}}
}

type dispatchDeadlineTransport struct {
	base     http.RoundTripper
	deadline time.Time
	checked  *atomic.Bool
}

func (t dispatchDeadlineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.checked.CompareAndSwap(false, true) {
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		if time.Now().After(t.deadline) {
			return nil, errDispatchLate
		}
	}
	return t.base.RoundTrip(request)
}

func clientWithDispatchDeadline(client *http.Client, deadline time.Time) *http.Client {
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = dispatchDeadlineTransport{base: base, deadline: deadline, checked: &atomic.Bool{}}
	return &clone
}

func validate(cfg Config) error {
	if cfg.Load.MaxInFlight < 1 {
		return errors.New("load max_in_flight must be positive")
	}
	if cfg.MaxScheduleLag <= 0 {
		return errors.New("max schedule lag must be positive")
	}
	if cfg.ResponseHeaderTimeout <= 0 {
		return errors.New("response header timeout must be positive")
	}
	if cfg.Safety.MaxRequests < 1 {
		return errors.New("safety max_requests must be positive")
	}
	if cfg.Safety.MaxDuration.Duration <= 0 {
		return errors.New("safety max_duration must be positive")
	}
	if !finitePositive(cfg.Safety.MaxCostUSD) {
		return errors.New("safety max_cost_usd must be finite and positive")
	}
	if !finitePositive(cfg.Safety.ReserveCostPerRequestUSD) {
		return errors.New("safety reserve_cost_per_request_usd must be finite and positive")
	}
	if !finiteNonNegative(cfg.Pricing.InputUSDPerMillionTokens) || !finiteNonNegative(cfg.Pricing.OutputUSDPerMillionTokens) {
		return errors.New("pricing values must be finite and non-negative")
	}
	if cfg.Probe.Endpoint == "" || cfg.Probe.Model == "" || cfg.Probe.Prompt == "" {
		return errors.New("probe endpoint, model, and prompt are required")
	}
	if cfg.Probe.MaxCompletionTokens < 1 {
		return errors.New("probe max completion tokens must be positive")
	}
	if cfg.Probe.Timeout <= 0 || cfg.Probe.StreamIdleTimeout <= 0 {
		return errors.New("probe timeouts must be positive")
	}
	return nil
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
