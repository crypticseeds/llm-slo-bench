package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/config"
	"github.com/crypticseeds/llm-slo-bench/internal/loadgen"
	"github.com/crypticseeds/llm-slo-bench/internal/metrics"
	"github.com/crypticseeds/llm-slo-bench/internal/mockserver"
	"github.com/crypticseeds/llm-slo-bench/internal/probe"
	"github.com/crypticseeds/llm-slo-bench/internal/slo"
)

const (
	exitSLOFail   = 1
	exitConfig    = 2
	exitExecution = 3
	exitInterrupt = 130
)

type commandError struct {
	code int
	err  error
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitCode(err))
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return configCommandError(errors.New("usage: llm-slo-bench <mock|probe|ramp> [flags]"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "mock":
		err = runMock(ctx, args[1:])
	case "probe":
		err = runProbe(ctx, args[1:])
	case "ramp":
		err = runRamp(ctx, args[1:], os.Stdout, os.Stderr)
	case "help", "-h", "--help":
		fmt.Println("usage: llm-slo-bench <mock|probe|ramp> [flags]")
		return nil
	default:
		return configCommandError(fmt.Errorf("unknown command %q", args[0]))
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func runMock(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mock", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "address to listen on")
	profileName := fs.String("profile", "steady", "latency profile: fast, steady, or slow")
	firstTokenDelay := fs.Duration("first-token-delay", -1, "override delay before first content event")
	chunkDelay := fs.Duration("chunk-delay", -1, "override delay between content events")
	chunks := fs.Int("chunks", 4, "number of non-empty content events")
	fault := fs.String("fault", "none", "fault mode: none, http-error, malformed, disconnect, or stall")
	faultEvery := fs.Int("fault-every", 1, "apply the configured fault to every Nth request")
	faultAfter := fs.Int("fault-after", 1, "content event after which a stream fault occurs")
	stallDuration := fs.Duration("stall-duration", 30*time.Second, "duration of the stall fault")
	if err := fs.Parse(args); err != nil {
		return configCommandError(err)
	}

	profile, err := mockserver.LookupProfile(*profileName)
	if err != nil {
		return configCommandError(err)
	}
	if *firstTokenDelay >= 0 {
		profile.FirstTokenDelay = *firstTokenDelay
	}
	if *chunkDelay >= 0 {
		profile.ChunkDelay = *chunkDelay
	}

	cfg := mockserver.Config{
		Address:       *listen,
		Profile:       profile,
		ChunkCount:    *chunks,
		Fault:         mockserver.Fault(*fault),
		FaultEvery:    *faultEvery,
		FaultAfter:    *faultAfter,
		StallDuration: *stallDuration,
	}
	if err := cfg.Validate(); err != nil {
		return configCommandError(err)
	}

	fmt.Printf("mock listening on http://%s/v1/chat/completions (profile=%s, first_token=%s, chunk=%s)\n", cfg.Address, profile.Name, profile.FirstTokenDelay, profile.ChunkDelay)
	if err := mockserver.Serve(ctx, cfg); err != nil {
		return executionCommandError(err)
	}
	return nil
}

func runProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "http://127.0.0.1:8080/v1/chat/completions", "OpenAI-compatible chat completions endpoint")
	model := fs.String("model", "mock-model", "model name")
	promptText := fs.String("prompt", "Explain coordinated omission in two sentences.", "user prompt")
	maxTokens := fs.Int("max-completion-tokens", 64, "maximum completion tokens")
	timeout := fs.Duration("timeout", 30*time.Second, "total request timeout")
	idleTimeout := fs.Duration("stream-idle-timeout", 5*time.Second, "maximum time without an SSE event")
	apiKeyEnv := fs.String("api-key-env", "", "environment variable containing the API key")
	if err := fs.Parse(args); err != nil {
		return configCommandError(err)
	}

	var apiKey string
	if *apiKeyEnv != "" {
		apiKey = os.Getenv(*apiKeyEnv)
		if apiKey == "" {
			return configCommandError(fmt.Errorf("environment variable %s is empty", *apiKeyEnv))
		}
	}

	result, err := probe.Run(ctx, probe.Config{
		Endpoint:            *endpoint,
		Model:               *model,
		Prompt:              *promptText,
		MaxCompletionTokens: *maxTokens,
		APIKey:              apiKey,
		Timeout:             *timeout,
		StreamIdleTimeout:   *idleTimeout,
	})
	if err != nil {
		return executionCommandError(err)
	}

	out := struct {
		StatusCode    int          `json:"status_code"`
		TTFBMS        float64      `json:"ttfb_ms"`
		TTFTMS        float64      `json:"ttft_ms"`
		ChunkITLMS    []float64    `json:"chunk_itl_ms"`
		ContentEvents int          `json:"content_events"`
		DurationMS    float64      `json:"duration_ms"`
		Usage         *probe.Usage `json:"usage,omitempty"`
	}{
		StatusCode:    result.StatusCode,
		TTFBMS:        milliseconds(result.TTFB),
		TTFTMS:        milliseconds(result.TTFT),
		ChunkITLMS:    durationsMS(result.ChunkITL),
		ContentEvents: result.ContentEvents,
		DurationMS:    milliseconds(result.Duration),
		Usage:         result.Usage,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func runRamp(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ramp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to the ramp YAML config")
	outPath := fs.String("out", "", "write summary JSON atomically to this path instead of stdout")
	rawJSONLPath := fs.String("raw-jsonl", "", "append one bounded record per started request to this path")
	if err := fs.Parse(args); err != nil {
		return configCommandError(err)
	}
	if *configPath == "" {
		return configCommandError(errors.New("--config is required"))
	}
	if fs.NArg() != 0 {
		return configCommandError(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return configCommandError(err)
	}
	apiKey := ""
	if cfg.Target.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Target.APIKeyEnv)
		if apiKey == "" {
			return configCommandError(fmt.Errorf("environment variable %s is empty", cfg.Target.APIKeyEnv))
		}
	}

	var rawWriter *metrics.JSONLWriter
	if *rawJSONLPath != "" {
		rawWriter, err = metrics.OpenJSONL(*rawJSONLPath)
		if err != nil {
			return executionCommandError(err)
		}
	}

	loadResult, runErr := loadgen.Run(ctx, loadgen.Config{
		Load:    cfg.Load,
		Safety:  cfg.Safety,
		Pricing: cfg.Pricing,
		Probe: probe.Config{
			Endpoint:            strings.TrimRight(cfg.Target.BaseURL, "/") + "/chat/completions",
			Model:               cfg.Target.Model,
			Prompt:              cfg.Request.Prompt,
			MaxCompletionTokens: cfg.Request.MaxCompletionTokens,
			APIKey:              apiKey,
			Timeout:             cfg.Request.Timeout.Duration,
			StreamIdleTimeout:   cfg.Request.StreamIdleTimeout.Duration,
		},
	})
	if runErr != nil && loadResult.StartedAt.IsZero() {
		if rawWriter != nil {
			_ = rawWriter.Close()
		}
		return configCommandError(runErr)
	}

	aggregator, err := metrics.NewAggregator(metrics.Pricing{
		InputUSDPerMillionTokens:  cfg.Pricing.InputUSDPerMillionTokens,
		OutputUSDPerMillionTokens: cfg.Pricing.OutputUSDPerMillionTokens,
	})
	if err != nil {
		if rawWriter != nil {
			_ = rawWriter.Close()
		}
		return executionCommandError(err)
	}

	var processingErr error
	for _, outcome := range loadResult.Outcomes {
		if outcome.Dropped {
			if err := aggregator.RecordDropped(); err != nil {
				processingErr = errors.Join(processingErr, err)
			}
			continue
		}
		requestErr := requestError(outcome)
		if err := aggregator.Record(outcome.Result, requestErr); err != nil {
			processingErr = errors.Join(processingErr, err)
		}
		if rawWriter != nil {
			if err := rawWriter.WriteResult(outcome.Result, requestErr); err != nil {
				processingErr = errors.Join(processingErr, err)
			}
		}
	}
	if rawWriter != nil {
		if err := rawWriter.Close(); err != nil {
			processingErr = errors.Join(processingErr, err)
		}
	}
	summary := aggregator.Close()

	sloResults, sloErr := slo.Evaluate(cfg.SLO, sloSummary(summary))
	pending := evaluatorPending(cfg.SLO, sloResults, sloErr)
	if pending {
		fmt.Fprintln(stderr, "slo: pending owner evaluator")
	} else if sloErr == nil {
		printSLOResults(stderr, sloResults)
	}
	if err := writeSummary(stdout, *outPath, summary); err != nil {
		processingErr = errors.Join(processingErr, err)
	}

	if errors.Is(runErr, context.Canceled) || ctx.Err() != nil {
		return context.Canceled
	}
	if runErr != nil {
		return executionCommandError(runErr)
	}
	if processingErr != nil {
		return executionCommandError(processingErr)
	}
	if sloErr != nil {
		return executionCommandError(fmt.Errorf("evaluate SLOs: %w", sloErr))
	}
	if summary.Metrics.TTFT == nil || summary.Metrics.TTFT.Count == 0 {
		return executionCommandError(errors.New("run produced no usable TTFT samples"))
	}
	if !pending {
		for _, result := range sloResults {
			if !result.Pass {
				return &commandError{code: exitSLOFail, err: errors.New("one or more SLOs failed")}
			}
		}
	}
	return nil
}

func loadConfig(path string) (config.Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	cfg, err := config.LoadReader(file)
	if err != nil {
		return config.Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

func requestError(outcome loadgen.Outcome) error {
	if outcome.Cancelled {
		return context.Canceled
	}
	switch outcome.ErrorClass {
	case loadgen.ErrorNone:
		return nil
	case loadgen.ErrorTimeout:
		return context.DeadlineExceeded
	case loadgen.ErrorHTTP:
		return probe.ErrHTTPStatus
	default:
		if outcome.Error != "" {
			return errors.New(outcome.Error)
		}
		return errors.New("stream request failed")
	}
}

func sloSummary(summary metrics.RunSummary) slo.Summary {
	result := slo.Summary{
		ErrorRate:         summary.Counts.ErrorRate,
		DroppedRate:       summary.Counts.DroppedRate,
		ScheduledRequests: int(summary.Counts.Scheduled),
		CostUSD:           summary.Usage.CostUSD,
		UsageSamples:      int(summary.Usage.Samples),
		CostComplete:      summary.Usage.Complete,
	}
	if summary.Metrics.TTFT != nil {
		value := summary.Metrics.TTFT.P99
		result.P99TTFTMS = &value
		result.TTFTSamples = int(summary.Metrics.TTFT.Count)
	}
	if summary.Metrics.ChunkITL != nil {
		value := summary.Metrics.ChunkITL.P99
		result.P99ChunkITLMS = &value
		result.ChunkITLSamples = int(summary.Metrics.ChunkITL.Count)
	}
	return result
}

func evaluatorPending(declared config.SLO, results []slo.Result, evaluateErr error) bool {
	if evaluateErr != nil || len(results) == 0 || len(results) != configuredSLOCount(declared) {
		return false
	}
	for _, result := range results {
		if result.Metric != "" || result.Operator != "" {
			return false
		}
	}
	return true
}

func configuredSLOCount(declared config.SLO) int {
	count := 0
	for _, threshold := range []*float64{
		declared.P99TTFTMS,
		declared.P99ChunkITLMS,
		declared.MaxErrorRate,
		declared.MaxDroppedRate,
		declared.MaxCostUSD,
	} {
		if threshold != nil {
			count++
		}
	}
	return count
}

func printSLOResults(writer io.Writer, results []slo.Result) {
	if len(results) == 0 {
		fmt.Fprintln(writer, "slo: pass (no gates configured)")
		return
	}
	for _, result := range results {
		status := "fail"
		if result.Pass {
			status = "pass"
		}
		fmt.Fprintf(writer, "slo: %s %s observed=%g threshold=%g samples=%d\n", status, result.Metric, result.Observed, result.Threshold, result.SampleCount)
	}
}

func writeSummary(stdout io.Writer, path string, summary metrics.RunSummary) error {
	var content bytes.Buffer
	encoder := json.NewEncoder(&content)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(summary); err != nil {
		return fmt.Errorf("encode summary JSON: %w", err)
	}
	if path == "" {
		if _, err := stdout.Write(content.Bytes()); err != nil {
			return fmt.Errorf("write summary JSON: %w", err)
		}
		return nil
	}

	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create summary temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return fmt.Errorf("set summary permissions: %w", err)
	}
	if _, err := temporary.Write(content.Bytes()); err != nil {
		temporary.Close()
		return fmt.Errorf("write summary temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync summary temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close summary temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace summary JSON: %w", err)
	}
	return nil
}

func configCommandError(err error) error {
	return &commandError{code: exitConfig, err: err}
}

func executionCommandError(err error) error {
	return &commandError{code: exitExecution, err: err}
}

func exitCode(err error) int {
	if errors.Is(err, context.Canceled) {
		return exitInterrupt
	}
	var commandErr *commandError
	if errors.As(err, &commandErr) {
		return commandErr.code
	}
	return exitExecution
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func durationsMS(values []time.Duration) []float64 {
	result := make([]float64, len(values))
	for i, value := range values {
		result[i] = milliseconds(value)
	}
	return result
}
