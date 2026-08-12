package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

func TestQuickstartConfigLoadsStrictly(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "quickstart.yaml")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cfg, err := config.LoadReader(file)
	if err != nil {
		t.Fatalf("config.LoadReader(%s) error = %v", path, err)
	}
	if cfg.Target.BaseURL != "http://127.0.0.1:8080/v1" || cfg.Target.APIKeyEnv != "" || len(cfg.Load.Stages) != 2 {
		t.Fatalf("quickstart config = %#v, want loopback mock target with two stages and no API key", cfg)
	}
	if cfg.SLO.P99TTFTMS == nil || cfg.SLO.MaxCostUSD == nil {
		t.Fatalf("quickstart SLO = %#v, want TTFT and cost gates", cfg.SLO)
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

func TestRunRampWritesHTMLWithRunMetadata(t *testing.T) {
	server := newRampMock(t, time.Millisecond)
	defer server.Close()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "ramp.yaml")
	htmlPath := filepath.Join(directory, "report.html")
	writeRampConfig(t, configPath, server.URL+"/v1", "100ms", 40)

	var stdout, stderr bytes.Buffer
	if err := runRamp(context.Background(), []string{"--config", configPath, "--html", htmlPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&stdout)
	var summary metrics.RunSummary
	if err := decoder.Decode(&summary); err != nil {
		t.Fatalf("decode stdout summary: %v\nstdout:\n%s", err, stdout.String())
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout contains more than one RunSummary: %v\nstdout:\n%s", err, stdout.String())
	}
	if len(summary.SLOOutcomes) == 0 {
		t.Fatal("stdout summary has no SLO outcomes")
	}
	content, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<span class="label">Scenario</span><span class="value">ramp</span>`,
		`<span class="label">Config file</span><span class="value"><code>ramp.yaml</code></span>`,
		`<span class="label">Target</span><span class="value">` + server.URL + `/v1</span>`,
		`<span class="label">Model</span><span class="value">mock-model</span>`,
		`<span class="label">Started</span><span class="value">20`,
		`<span class="label">Tool version</span><span class="value">dev</span>`,
	} {
		if !bytes.Contains(content, []byte(want)) {
			t.Errorf("HTML does not contain %q", want)
		}
	}
	for _, outcome := range summary.SLOOutcomes {
		result := "pending"
		if outcome.Pass != nil && *outcome.Pass {
			result = "pass"
		} else if outcome.Pass != nil {
			result = "fail"
		}
		want := fmt.Sprintf(`<tr><td data-label="Metric">%s</td><td data-label="Threshold">%s %s</td><td data-label="Observed">%s</td><td data-label="Samples">%d</td><td data-label="Status" class="%s">%s</td><td data-label="Result" class="%s">%s</td></tr>`,
			html.EscapeString(outcome.Metric), html.EscapeString(outcome.Operator), strconv.FormatFloat(outcome.Threshold, 'f', -1, 64), strconv.FormatFloat(outcome.Observed, 'f', -1, 64), outcome.SampleCount,
			outcome.Status, outcome.Status, result, result)
		if !bytes.Contains(content, []byte(want)) {
			t.Errorf("HTML does not reflect stdout SLO outcome %#v", outcome)
		}
	}
	if bytes.Contains(content, []byte("SLO evaluation is not attached")) {
		t.Fatal("HTML reports missing SLO evaluation despite stdout outcomes")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".report.html.tmp-") {
			t.Fatalf("atomic HTML write left temporary file %q", entry.Name())
		}
	}
}

func TestRunRampRejectsAliasedOutputPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	err := runRamp(context.Background(), []string{"--config", "unused.yaml", "--out", path, "--html", path}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--out and --html must use different paths") || exitCode(err) != exitConfig {
		t.Fatalf("runRamp() error = %v, want aliased output config error", err)
	}
}

func TestRunRampRejectsExistingRawJSONLForHTML(t *testing.T) {
	directory := t.TempDir()
	rawPath := filepath.Join(directory, "requests.jsonl")
	if err := os.WriteFile(rawPath, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runRamp(context.Background(), []string{
		"--config", "unused.yaml",
		"--html", filepath.Join(directory, "report.html"),
		"--raw-jsonl", rawPath,
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "must be new or empty") || exitCode(err) != exitConfig {
		t.Fatalf("runRamp() error = %v, want existing raw JSONL config error", err)
	}
}

func TestRunReportRendersExistingSummaryWithOptionalRawJSONL(t *testing.T) {
	directory := t.TempDir()
	inputPath := filepath.Join(directory, "summary.json")
	htmlPath := filepath.Join(directory, "report.html")
	rawPath := filepath.Join(directory, "requests.jsonl")
	summary := metrics.RunSummary{SchemaVersion: metrics.SchemaVersion, Counts: metrics.Counts{Scheduled: 1, Started: 1, Success: 1}}
	if err := writeSummary(io.Discard, inputPath, summary); err != nil {
		t.Fatal(err)
	}
	writeValidRequestJSONL(t, rawPath)

	var stderr bytes.Buffer
	if err := runReport([]string{"--input", inputPath, "--html", htmlPath, "--raw-jsonl", rawPath}, &stderr); err != nil {
		t.Fatalf("runReport() error = %v\nstderr:\n%s", err, stderr.String())
	}
	content, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"REQUEST RECORDS", "requests.jsonl", "Tool version", "dev"} {
		if !bytes.Contains(content, []byte(want)) {
			t.Errorf("HTML does not contain %q", want)
		}
	}
}

func TestRunReportRequiresInputAndHTMLFlags(t *testing.T) {
	for name, args := range map[string][]string{
		"input": {"--html", "report.html"},
		"html":  {"--input", "summary.json"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runReport(args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "--"+name+" is required") || exitCode(err) != exitConfig {
				t.Fatalf("runReport() error = %v, want missing --%s config error", err, name)
			}
		})
	}
}

func TestRunReportRejectsInputOutputPathCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.json")
	err := runReport([]string{"--input", path, "--html", path}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--input and --html must use different paths") || exitCode(err) != exitConfig {
		t.Fatalf("runReport() error = %v, want input/output collision config error", err)
	}
}

func TestRunReportLeavesExistingHTMLUntouchedOnRenderFailure(t *testing.T) {
	directory := t.TempDir()
	inputPath := filepath.Join(directory, "summary.json")
	htmlPath := filepath.Join(directory, "report.html")
	rawPath := filepath.Join(directory, "invalid.jsonl")
	if err := writeSummary(io.Discard, inputPath, metrics.RunSummary{SchemaVersion: metrics.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(htmlPath, []byte("existing report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawPath, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runReport([]string{"--input", inputPath, "--html", htmlPath, "--raw-jsonl", rawPath}, io.Discard)
	if err == nil || exitCode(err) != exitExecution {
		t.Fatalf("runReport() error = %v, want execution error", err)
	}
	content, readErr := os.ReadFile(htmlPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "existing report" {
		t.Fatalf("HTML after failed atomic write = %q, want existing content", content)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".report.html.tmp-") {
			t.Fatalf("failed atomic HTML write left temporary file %q", entry.Name())
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

func writeValidRequestJSONL(t *testing.T, path string) {
	t.Helper()
	line := `{"schema_version":1,"outcome":"success","status_code":200,"ttfb_micros":2500,"ttft_micros":12500,"chunk_itl_micros":[3000,4000],"duration_micros":112500,"usage_status":"available","usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}
