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

	"github.com/crypticseeds/llm-slo-bench/internal/metrics"
	"github.com/crypticseeds/llm-slo-bench/internal/mockserver"
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
	if !strings.Contains(stderr.String(), "slo: pending owner evaluator") {
		t.Fatalf("stderr = %q, want pending evaluator status", stderr.String())
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
