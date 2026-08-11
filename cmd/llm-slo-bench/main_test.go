package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/crypticseeds/llm-slo-bench/internal/config"
	"github.com/crypticseeds/llm-slo-bench/internal/metrics"
	"github.com/crypticseeds/llm-slo-bench/internal/mockserver"
	"github.com/crypticseeds/llm-slo-bench/internal/slo"
)

func TestRunRejectsUnknownCommand(t *testing.T) {
	err := run([]string{"surprise"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run() error = %v, want unknown command error", err)
	}
}

func TestRunMockRejectsUnknownProfile(t *testing.T) {
	err := runMock(context.Background(), []string{"--profile", "surprise"})
	if err == nil || !strings.Contains(err.Error(), "unknown latency profile") {
		t.Fatalf("runMock() error = %v, want profile error", err)
	}
}

func TestRunHelpIsSuccessful(t *testing.T) {
	if err := run([]string{"probe", "--help"}); err != nil {
		t.Fatalf("run(probe --help) error = %v", err)
	}
}

func TestRunRampAgainstBuiltInMockProducesTTFTHistogram(t *testing.T) {
	server := newRampMock(t, 5*time.Millisecond)
	defer server.Close()

	directory := t.TempDir()
	configPath := filepath.Join(directory, "ramp.yaml")
	rawPath := filepath.Join(directory, "requests.jsonl")
	writeRampConfig(t, configPath, server.URL+"/v1", "250ms", 40)

	var stdout, stderr bytes.Buffer
	if err := runRamp(context.Background(), []string{"--config", configPath, "--raw-jsonl", rawPath}, &stdout, &stderr); err != nil {
		t.Fatalf("runRamp() error = %v\nstderr:\n%s", err, stderr.String())
	}
	var summary metrics.RunSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v\nstdout:\n%s", err, stdout.String())
	}
	if summary.Metrics.TTFT == nil || summary.Metrics.TTFT.Count == 0 {
		t.Fatalf("TTFT summary = %#v, want nonzero histogram samples", summary.Metrics.TTFT)
	}
	t.Logf("Day 2 gate: scheduled=%d success=%d ttft_samples=%d p99_ttft_ms=%.3f", summary.Counts.Scheduled, summary.Counts.Success, summary.Metrics.TTFT.Count, summary.Metrics.TTFT.P99)
	if len(summary.SLOOutcomes) != 1 {
		t.Fatalf("SLO outcomes = %#v, want one p99 TTFT gate", summary.SLOOutcomes)
	}
	outcome := summary.SLOOutcomes[0]
	if outcome.Metric != "p99_ttft_ms" || outcome.Operator != "<=" || outcome.Threshold != 1000 ||
		outcome.Observed != summary.Metrics.TTFT.P99 || outcome.SampleCount != int(summary.Metrics.TTFT.Count) {
		t.Fatalf("SLO outcome = %#v, want populated p99 TTFT gate", outcome)
	}
	switch outcome.Status {
	case metrics.SLOStatusPending:
		if outcome.Pass != nil || !strings.Contains(stderr.String(), "slo: pending owner evaluator") {
			t.Fatalf("pending SLO outcome = %#v, stderr = %q", outcome, stderr.String())
		}
	case metrics.SLOStatusPass:
		if outcome.Pass == nil || !*outcome.Pass || !strings.Contains(stderr.String(), "slo: pass p99_ttft_ms") {
			t.Fatalf("passing SLO outcome = %#v, stderr = %q", outcome, stderr.String())
		}
	default:
		t.Fatalf("SLO status = %q, want pending or pass", outcome.Status)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); int64(lines) != summary.Counts.Started {
		t.Fatalf("raw JSONL records = %d, want %d started requests", lines, summary.Counts.Started)
	}
}

func TestRunRampWritesOutAtomicallyAndLeavesRawJSONLOffByDefault(t *testing.T) {
	server := newRampMock(t, time.Millisecond)
	defer server.Close()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "ramp.yaml")
	outPath := filepath.Join(directory, "summary.json")
	writeRampConfig(t, configPath, server.URL+"/v1", "100ms", 40)

	var stdout, stderr bytes.Buffer
	if err := runRamp(context.Background(), []string{"--config", configPath, "--out", outPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q with --out, want empty", stdout.String())
	}
	content, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var summary metrics.RunSummary
	if err := json.Unmarshal(content, &summary); err != nil {
		t.Fatalf("decode --out summary: %v", err)
	}
	if summary.Metrics.TTFT == nil || summary.Metrics.TTFT.Count == 0 {
		t.Fatalf("TTFT summary = %#v, want samples", summary.Metrics.TTFT)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".summary.json.tmp-") || strings.HasSuffix(entry.Name(), ".jsonl") {
			t.Fatalf("unexpected output artifact %q", entry.Name())
		}
	}
}

func TestRunRampPersistsEvaluatedSLOOutcomesAndExitStatus(t *testing.T) {
	server := newRampMock(t, time.Millisecond)
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "ramp.yaml")
	writeRampConfig(t, configPath, server.URL+"/v1", "100ms", 40)

	for _, test := range []struct {
		name     string
		pass     bool
		wantCode int
		status   metrics.SLOStatus
	}{
		{name: "pass", pass: true, wantCode: 0, status: metrics.SLOStatusPass},
		{name: "fail", pass: false, wantCode: exitSLOFail, status: metrics.SLOStatusFail},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			evaluate := func(declared config.SLO, summary slo.Summary) ([]slo.Result, error) {
				return []slo.Result{{
					Metric: "p99_ttft_ms", Observed: *summary.P99TTFTMS, Operator: "<=",
					Threshold: *declared.P99TTFTMS, SampleCount: summary.TTFTSamples, Pass: test.pass,
				}}, nil
			}
			err := runRampWithEvaluator(context.Background(), []string{"--config", configPath}, &stdout, &stderr, evaluate)
			if got := exitCodeOrZero(err); got != test.wantCode {
				t.Fatalf("exit code = %d, want %d; error=%v", got, test.wantCode, err)
			}
			var summary metrics.RunSummary
			if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
				t.Fatalf("decode summary: %v", err)
			}
			if len(summary.SLOOutcomes) != 1 || summary.SLOOutcomes[0].Status != test.status ||
				summary.SLOOutcomes[0].Pass == nil || *summary.SLOOutcomes[0].Pass != test.pass {
				t.Fatalf("SLO outcomes = %#v, want %s with pass=%t", summary.SLOOutcomes, test.status, test.pass)
			}
			if !strings.Contains(stderr.String(), "slo: "+string(test.status)+" p99_ttft_ms") {
				t.Fatalf("stderr = %q, want %s gate", stderr.String(), test.status)
			}
		})
	}
}

func TestRunRampCancellationReturnsInterruptAndStillEmitsSummary(t *testing.T) {
	server := newRampMock(t, 2*time.Second)
	defer server.Close()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "ramp.yaml")
	writeRampConfig(t, configPath, server.URL+"/v1", "2s", 20)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(600*time.Millisecond, cancel)
	var stdout, stderr bytes.Buffer
	err := runRamp(ctx, []string{"--config", configPath}, &stdout, &stderr)
	if got := exitCode(err); got != exitInterrupt {
		t.Fatalf("exitCode(runRamp cancellation) = %d, want %d; error=%v", got, exitInterrupt, err)
	}
	var summary metrics.RunSummary
	if decodeErr := json.Unmarshal(stdout.Bytes(), &summary); decodeErr != nil {
		t.Fatalf("decode interrupted summary: %v\nstdout:\n%s", decodeErr, stdout.String())
	}
	if summary.Counts.Canceled == 0 {
		t.Fatalf("counts = %#v, want cleanly canceled in-flight request", summary.Counts)
	}
}

func TestWriteSummaryMatchesVersionedGolden(t *testing.T) {
	summary := metrics.RunSummary{
		SchemaVersion: metrics.SchemaVersion,
		Counts:        metrics.Counts{Scheduled: 1, Started: 1, Success: 1},
		SLOOutcomes: []metrics.SLOOutcome{{
			Metric: "p99_ttft_ms", Observed: 12.5, Operator: "<=", Threshold: 800,
			SampleCount: 1, Status: metrics.SLOStatusPending,
		}},
	}
	var output bytes.Buffer
	if err := writeSummary(&output, "", summary); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "run-summary.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("summary JSON does not match golden\ngot:\n%s\nwant:\n%s", output.Bytes(), want)
	}
}

func TestExitCodeTaxonomy(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want int
	}{
		"SLO failure":     {err: &commandError{code: exitSLOFail, err: errors.New("failed")}, want: 1},
		"config error":    {err: configCommandError(errors.New("invalid")), want: 2},
		"execution error": {err: executionCommandError(errors.New("failed")), want: 3},
		"interrupt":       {err: context.Canceled, want: 130},
	} {
		t.Run(name, func(t *testing.T) {
			if got := exitCode(test.err); got != test.want {
				t.Fatalf("exitCode() = %d, want %d", got, test.want)
			}
		})
	}
}

func exitCodeOrZero(err error) int {
	if err == nil {
		return 0
	}
	return exitCode(err)
}

func newRampMock(t *testing.T, firstTokenDelay time.Duration) *httptest.Server {
	t.Helper()
	cfg := mockserver.Config{
		Address:       "127.0.0.1:0",
		Profile:       mockserver.Profile{Name: "test", FirstTokenDelay: firstTokenDelay, ChunkDelay: time.Millisecond},
		ChunkCount:    2,
		Fault:         mockserver.FaultNone,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(mockserver.NewHandler(cfg))
}

func writeRampConfig(t *testing.T, path, baseURL, duration string, targetRPS int) {
	t.Helper()
	configYAML := fmt.Sprintf(`version: 1
target:
  base_url: %s
  model: mock-model
request:
  prompt: Day 2 gate
  max_completion_tokens: 8
  timeout: 1s
  stream_idle_timeout: 750ms
load:
  max_in_flight: 4
  stages:
    - duration: %s
      target_rps: %d
slo:
  p99_ttft_ms: 1000
safety:
  max_requests: 100
  max_duration: 3s
  max_cost_usd: 1
  reserve_cost_per_request_usd: 0.001
pricing:
  input_usd_per_million_tokens: 0.15
  output_usd_per_million_tokens: 0.60
output: {}
`, baseURL, duration, targetRPS)
	if err := os.WriteFile(path, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}
