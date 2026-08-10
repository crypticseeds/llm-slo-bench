package loadgen

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/config"
	"github.com/crypticseeds/llm-slo-bench/internal/mockserver"
	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

func TestScheduleUsesPiecewiseLinearAbsoluteOffsets(t *testing.T) {
	offsets, err := Schedule([]config.Stage{
		{Duration: duration(4 * time.Second), TargetRPS: 2},
		{Duration: duration(2 * time.Second), TargetRPS: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{
		2 * time.Second,
		2828427124 * time.Nanosecond,
		3464101615 * time.Nanosecond,
		4 * time.Second,
		4500 * time.Millisecond,
		5 * time.Second,
		5500 * time.Millisecond,
		6 * time.Second,
	}
	if len(offsets) != len(want) {
		t.Fatalf("Schedule() returned %d arrivals, want %d: %v", len(offsets), len(want), offsets)
	}
	for i := range want {
		if difference(offsets[i], want[i]) > time.Nanosecond {
			t.Errorf("arrival %d offset = %s, want %s", i+1, offsets[i], want[i])
		}
	}
}

func TestRunAbsoluteDeadlinesDoNotDrift(t *testing.T) {
	clk := &fakeClock{now: time.Unix(100, 0), lateness: 10 * time.Millisecond}
	cfg := testConfig(clk)
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 4}}
	cfg.Load.MaxInFlight = 4
	cfg.Safety.MaxRequests = 10

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts.Started != 2 || result.Counts.Succeeded != 2 {
		t.Fatalf("counts = %+v, want two successful requests", result.Counts)
	}
	for _, outcome := range result.Outcomes {
		if outcome.ScheduleLag != 10*time.Millisecond {
			t.Errorf("request %d schedule lag = %s, want 10ms", outcome.Sequence, outcome.ScheduleLag)
		}
	}
}

func TestRunDropsInsteadOfQueueingWhenSaturated(t *testing.T) {
	clk := &fakeClock{
		now:         time.Unix(100, 0),
		blockAtWait: 4,
		waitBlocked: make(chan struct{}),
		waitRelease: make(chan struct{}),
		lateAtWait:  4,
		lateBy:      200 * time.Millisecond,
	}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	cfg := testConfig(clk)
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 8}}
	cfg.Load.MaxInFlight = 1
	cfg.runner = func(context.Context, probe.Config) (probe.Result, error) {
		started <- struct{}{}
		<-release
		return probe.Result{}, nil
	}

	done := make(chan Result, 1)
	go func() {
		result, _ := Run(context.Background(), cfg)
		done <- result
	}()
	<-started
	<-clk.waitBlocked
	close(release)
	close(clk.waitRelease)
	result := <-done

	if result.Counts.Scheduled != 4 || result.Counts.Started != 1 || result.Counts.Dropped != 3 {
		t.Fatalf("counts = %+v, want scheduled=4 started=1 dropped=3", result.Counts)
	}
	for _, outcome := range result.Outcomes[1:3] {
		if !outcome.Dropped || outcome.DropReason != DropSaturated {
			t.Fatalf("outcome = %+v, want saturated drop", outcome)
		}
	}
	if result.Outcomes[3].DropReason != DropLate {
		t.Fatalf("final outcome = %+v, want controlled late drop", result.Outcomes[3])
	}
}

func TestRunDropsArrivalsBeyondLatenessBound(t *testing.T) {
	clk := &fakeClock{now: time.Unix(100, 0), lateness: 25 * time.Millisecond}
	cfg := testConfig(clk)
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 4}}
	cfg.MaxScheduleLag = 20 * time.Millisecond
	called := false
	cfg.runner = func(context.Context, probe.Config) (probe.Result, error) {
		called = true
		return probe.Result{}, nil
	}

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("probe ran for arrivals beyond the lateness bound")
	}
	if result.Counts.Dropped != 2 {
		t.Fatalf("counts = %+v, want two drops", result.Counts)
	}
	for _, outcome := range result.Outcomes {
		if outcome.DropReason != DropLate {
			t.Fatalf("drop reason = %q, want %q", outcome.DropReason, DropLate)
		}
	}
}

func TestRunSafetyLimitsPreventNewDispatch(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Config)
		wantStarts int
		wantReason StopReason
	}{
		{
			name: "request limit",
			mutate: func(cfg *Config) {
				cfg.Safety.MaxRequests = 2
			},
			wantStarts: 2,
			wantReason: StopRequestLimit,
		},
		{
			name: "cost reservation",
			mutate: func(cfg *Config) {
				cfg.Safety.MaxCostUSD = 0.02
				cfg.Safety.ReserveCostPerRequestUSD = 0.01
			},
			wantStarts: 2,
			wantReason: StopCostLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clk := &fakeClock{now: time.Unix(100, 0)}
			block := make(chan struct{})
			started := make(chan struct{}, 20)
			cfg := testConfig(clk)
			cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 20}}
			cfg.Load.MaxInFlight = 20
			cfg.runner = func(context.Context, probe.Config) (probe.Result, error) {
				started <- struct{}{}
				<-block
				return probe.Result{}, nil
			}
			test.mutate(&cfg)

			done := make(chan Result, 1)
			go func() {
				result, _ := Run(context.Background(), cfg)
				done <- result
			}()
			for i := 0; i < test.wantStarts; i++ {
				<-started
			}
			close(block)
			result := <-done

			if result.Counts.Started != test.wantStarts || result.StopReason != test.wantReason {
				t.Fatalf("started=%d stop=%q, want started=%d stop=%q", result.Counts.Started, result.StopReason, test.wantStarts, test.wantReason)
			}
		})
	}
}

func TestRunDurationLimitPreventsDispatch(t *testing.T) {
	called := false
	cfg := testConfig(&fakeClock{now: time.Unix(100, 0)})
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 20}}
	cfg.Safety.MaxDuration = duration(100 * time.Millisecond)
	cfg.runner = func(context.Context, probe.Config) (probe.Result, error) {
		called = true
		return probe.Result{}, nil
	}

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if called || result.Counts.Started != 0 || result.StopReason != StopDurationLimit {
		t.Fatalf("called=%t result=%+v, want no dispatch and duration stop", called, result)
	}
}

func TestRunStopsWhenSchedulerReachesDurationDeadlineLate(t *testing.T) {
	called := false
	cfg := testConfig(&fakeClock{now: time.Unix(100, 0), lateness: 20 * time.Millisecond})
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 2}}
	cfg.Safety.MaxDuration = duration(700 * time.Millisecond)
	cfg.MaxScheduleLag = 100 * time.Millisecond
	cfg.runner = func(context.Context, probe.Config) (probe.Result, error) {
		called = true
		return probe.Result{}, nil
	}

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if called || result.Counts.Scheduled != 0 || result.StopReason != StopDurationLimit {
		t.Fatalf("called=%t result=%+v, want duration stop before dispatch", called, result)
	}
}

func TestNewHTTPClientIsBoundedAndHasHeaderTimeout(t *testing.T) {
	client := newHTTPClient(7, 250*time.Millisecond)
	transport := client.Transport.(*http.Transport)
	if transport.MaxConnsPerHost != 7 || transport.MaxIdleConnsPerHost != 7 || transport.ResponseHeaderTimeout != 250*time.Millisecond {
		t.Fatalf("transport = %+v, want bounded pool and 250ms header timeout", transport)
	}
}

func TestRunReconcilesReservationsWithKnownCost(t *testing.T) {
	cfg := testConfig(&fakeClock{now: time.Unix(100, 0)})
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 4}}
	cfg.Safety.MaxCostUSD = 0.02
	cfg.Safety.ReserveCostPerRequestUSD = 0.01
	cfg.Pricing.InputUSDPerMillionTokens = 1
	cfg.runner = func(context.Context, probe.Config) (probe.Result, error) {
		return probe.Result{Usage: &probe.Usage{PromptTokens: 100}}, nil
	}

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts.Started != 2 || result.StopReason != StopNone || differenceCost(result.KnownCostUSD, 0.0002) > 1e-12 || differenceCost(result.SafetyBudgetUSD, 0.0002) > 1e-12 {
		t.Fatalf("result = %+v, want two starts reconciled to known cost 0.0002", result)
	}
}

func TestRunInvalidUsageDoesNotReleaseReservation(t *testing.T) {
	cfg := testConfig(&fakeClock{now: time.Unix(100, 0)})
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 2}}
	cfg.Load.MaxInFlight = 1
	cfg.Safety.MaxCostUSD = 0.01
	cfg.Safety.ReserveCostPerRequestUSD = 0.01
	cfg.runner = func(context.Context, probe.Config) (probe.Result, error) {
		return probe.Result{Usage: &probe.Usage{PromptTokens: -1}}, nil
	}

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts.Started != 1 || result.Counts.Failed != 1 || result.SafetyBudgetUSD != 0.01 || result.Outcomes[0].ErrorClass != ErrorUsage {
		t.Fatalf("result = %+v, want invalid usage failure retaining reservation", result)
	}
}

func TestRunClassifiesRequestErrors(t *testing.T) {
	tests := []struct {
		name       string
		runErr     error
		statusCode int
		want       ErrorClass
	}{
		{name: "timeout", runErr: context.DeadlineExceeded, want: ErrorTimeout},
		{name: "network timeout", runErr: timeoutError{}, want: ErrorTimeout},
		{name: "stream idle timeout", runErr: errors.New("stream idle timeout after 1s"), want: ErrorTimeout},
		{name: "http", runErr: errors.New("503"), statusCode: http.StatusServiceUnavailable, want: ErrorHTTP},
		{name: "stream", runErr: errors.New("malformed event"), statusCode: http.StatusOK, want: ErrorStream},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(&fakeClock{now: time.Unix(100, 0)})
			cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 2}}
			cfg.runner = func(context.Context, probe.Config) (probe.Result, error) {
				return probe.Result{StatusCode: test.statusCode}, test.runErr
			}
			result, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcomes[0].ErrorClass != test.want {
				t.Fatalf("error class = %q, want %q", result.Outcomes[0].ErrorClass, test.want)
			}
		})
	}
}

func TestRunRejectsNonFiniteMoneyValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "NaN ceiling", mutate: func(cfg *Config) { cfg.Safety.MaxCostUSD = math.NaN() }},
		{name: "infinite reservation", mutate: func(cfg *Config) { cfg.Safety.ReserveCostPerRequestUSD = math.Inf(1) }},
		{name: "NaN input price", mutate: func(cfg *Config) { cfg.Pricing.InputUSDPerMillionTokens = math.NaN() }},
		{name: "infinite output price", mutate: func(cfg *Config) { cfg.Pricing.OutputUSDPerMillionTokens = math.Inf(1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(&fakeClock{now: time.Unix(100, 0)})
			test.mutate(&cfg)
			if _, err := Run(context.Background(), cfg); err == nil {
				t.Fatal("Run() accepted a non-finite monetary value")
			}
		})
	}
}

func TestRunCancellationDrainsProbeAgainstMockServer(t *testing.T) {
	mock := mockserver.NewHandler(mockserver.Config{
		Profile:       mockserver.Profile{Name: "test"},
		ChunkCount:    2,
		Fault:         mockserver.FaultStall,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Hour,
	})
	requestStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		mock.ServeHTTP(w, r)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(&fakeClock{now: time.Unix(100, 0)})
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 2}}
	cfg.Load.MaxInFlight = 1
	cfg.Probe = probe.Config{
		Endpoint:            server.URL + "/v1/chat/completions",
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             time.Hour,
		StreamIdleTimeout:   time.Hour,
	}
	cfg.runner = nil

	type runReturn struct {
		result Result
		err    error
	}
	done := make(chan runReturn, 1)
	go func() {
		result, err := Run(ctx, cfg)
		done <- runReturn{result: result, err: err}
	}()
	<-requestStarted
	cancel()
	returned := <-done

	if !errors.Is(returned.err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", returned.err)
	}
	if returned.result.Counts.Started != 1 || returned.result.Counts.Cancelled != 1 || returned.result.Counts.Completed != 1 {
		t.Fatalf("counts = %+v, want one drained cancellation", returned.result.Counts)
	}
}

func TestRunExecutesProbeAgainstMockServer(t *testing.T) {
	server := httptest.NewServer(mockserver.NewHandler(mockserver.Config{
		Profile:       mockserver.Profile{Name: "test"},
		ChunkCount:    2,
		Fault:         mockserver.FaultNone,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}))
	defer server.Close()

	cfg := testConfig(&fakeClock{now: time.Unix(100, 0)})
	cfg.Load.Stages = []config.Stage{{Duration: duration(time.Second), TargetRPS: 2}}
	cfg.Probe = probe.Config{
		Endpoint:            server.URL + "/v1/chat/completions",
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             time.Second,
		StreamIdleTimeout:   time.Second,
	}
	cfg.runner = nil

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts.Succeeded != 1 || result.Outcomes[0].Result.ContentEvents != 2 {
		t.Fatalf("result = %+v, want one successful two-content-event probe", result)
	}
}

type fakeClock struct {
	mu          sync.Mutex
	now         time.Time
	lateness    time.Duration
	waited      chan time.Time
	waitCount   int
	blockAtWait int
	waitBlocked chan struct{}
	waitRelease chan struct{}
	lateAtWait  int
	lateBy      time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) WaitUntil(ctx context.Context, deadline time.Time) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.mu.Lock()
	c.waitCount++
	wait := c.waitCount
	c.mu.Unlock()
	if wait == c.blockAtWait {
		close(c.waitBlocked)
		<-c.waitRelease
	}
	c.mu.Lock()
	lateness := c.lateness
	if wait == c.lateAtWait {
		lateness = c.lateBy
	}
	c.now = deadline.Add(lateness)
	c.mu.Unlock()
	if c.waited != nil {
		c.waited <- deadline
	}
	return nil
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "network timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func testConfig(clk clock) Config {
	return Config{
		Load: config.Load{
			MaxInFlight: 4,
			Stages:      []config.Stage{{Duration: duration(time.Second), TargetRPS: 2}},
		},
		Safety: config.Safety{
			MaxRequests:              100,
			MaxDuration:              duration(10 * time.Second),
			MaxCostUSD:               10,
			ReserveCostPerRequestUSD: 0.01,
		},
		Probe:          validProbeConfig(),
		MaxScheduleLag: DefaultMaxScheduleLag,
		clock:          clk,
		runner: func(context.Context, probe.Config) (probe.Result, error) {
			return probe.Result{}, nil
		},
	}
}

func validProbeConfig() probe.Config {
	return probe.Config{
		Endpoint:            "http://127.0.0.1/v1/chat/completions",
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             time.Second,
		StreamIdleTimeout:   time.Second,
	}
}

func duration(value time.Duration) config.Duration {
	return config.Duration{Duration: value}
}

func difference(left, right time.Duration) time.Duration {
	if left < right {
		return right - left
	}
	return left - right
}

func differenceCost(left, right float64) float64 {
	if left < right {
		return right - left
	}
	return left - right
}
