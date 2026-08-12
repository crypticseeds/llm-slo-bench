package report

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/metrics"
	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

var update = flag.Bool("update", false, "update golden files")

func TestWriteJSONGolden(t *testing.T) {
	var output bytes.Buffer
	if err := WriteJSON(&output, fixtureSummary()); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "summary.golden.json", output.Bytes())
}

func TestWriteHTMLGolden(t *testing.T) {
	jsonlPath := filepath.Join(t.TempDir(), "requests.jsonl")
	jsonl := strings.Join([]string{
		`{"schema_version":1,"outcome":"success","status_code":200,"ttfb_micros":2500,"ttft_micros":12500,"chunk_itl_micros":[3000,4000],"duration_micros":112500,"usage_status":"available","usage":{"prompt_tokens":80,"completion_tokens":20,"total_tokens":100}}`,
		`{"schema_version":1,"outcome":"canceled","status_code":0,"ttfb_micros":null,"ttft_micros":null,"chunk_itl_micros":null,"duration_micros":null,"usage_status":"unavailable","usage":null}`,
	}, "\n") + "\n"
	if err := os.WriteFile(jsonlPath, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := WriteHTML(&output, fixtureSummary(), HTMLOptions{
		Metadata: Metadata{
			RunID:             "run-20260811-01",
			Scenario:          "ramp",
			ConfigFile:        "quickstart.yaml",
			Target:            "mock.local",
			Model:             "mock-model",
			StartedAt:         time.Date(2026, time.August, 11, 14, 30, 0, 0, time.FixedZone("BST", 3600)),
			Duration:          45 * time.Second,
			ToolVersion:       "v0.1.0",
			ConfigFingerprint: "sha256:abc123",
		},
		RequestJSONLPath: jsonlPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "report.golden.html", output.Bytes())
}

func TestWriteHTMLRendersWiringMetadata(t *testing.T) {
	var output bytes.Buffer
	err := WriteHTML(&output, metrics.RunSummary{SchemaVersion: metrics.SchemaVersion}, HTMLOptions{Metadata: Metadata{
		ConfigFile:  "quickstart.yaml",
		Target:      "http://127.0.0.1:8080/v1",
		Model:       "mock-model",
		StartedAt:   time.Date(2026, time.August, 12, 15, 4, 5, 0, time.UTC),
		ToolVersion: "v0.1.0",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"quickstart.yaml", "http://127.0.0.1:8080/v1", "mock-model", "2026-08-12T15:04:05Z", "v0.1.0"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("HTML does not contain metadata value %q", want)
		}
	}
}

func TestWriteHTMLRendersCompleteHistogramContractAndVisualizations(t *testing.T) {
	var output bytes.Buffer
	if err := WriteHTML(&output, fixtureSummary(), HTMLOptions{}); err != nil {
		t.Fatal(err)
	}
	report := output.String()
	for _, want := range []string{
		`<th>Lowest trackable</th>`,
		`<th>Highest trackable</th>`,
		`<th>Significant figures</th>`,
		`data-chart="percentiles"`,
		`data-chart="outcomes"`,
		`LATENCY DISTRIBUTIONS`,
		`uPlot license · MIT`,
		`Canceled`,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("HTML does not contain %q", want)
		}
	}
}

func TestWriteHTMLHasNoExternalResourceHooks(t *testing.T) {
	var output bytes.Buffer
	if err := WriteHTML(&output, fixtureSummary(), HTMLOptions{}); err != nil {
		t.Fatal(err)
	}
	report := strings.ToLower(output.String())
	for _, forbidden := range []string{"<link", " src=", " href=", "url(", "@import"} {
		if strings.Contains(report, forbidden) {
			t.Errorf("HTML contains external-resource hook %q", forbidden)
		}
	}
}

func TestWriteHTMLEmptySummaryUsesHonestUnknowns(t *testing.T) {
	var output bytes.Buffer
	if err := WriteHTML(&output, metrics.RunSummary{SchemaVersion: 1}, HTMLOptions{}); err != nil {
		t.Fatal(err)
	}
	report := output.String()
	for _, want := range []string{
		`<strong class="pending">PENDING</strong>`,
		`Rates are not applicable because no arrivals were scheduled.`,
		`<span class="value">n/a</span>`,
		`<td class="empty" colspan="12">no samples</td>`,
		`<span class="value unavailable">unavailable</span>`,
		`token totals and cost are unavailable`,
		`<span class="value">not provided</span>`,
		`<span class="label">Prompt tokens</span><span class="value">unavailable</span>`,
		`<span class="label">Completion tokens</span><span class="value">unavailable</span>`,
		`<span class="label">Total tokens</span><span class="value">unavailable</span>`,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("HTML does not contain %q", want)
		}
	}
	if strings.Contains(report, "Per-request details") {
		t.Fatal("HTML contains per-request section without a JSONL path")
	}
}

func TestWriteHTMLMarksPartialUsageAndNullCost(t *testing.T) {
	summary := metrics.RunSummary{
		SchemaVersion: 1,
		Counts:        metrics.Counts{Scheduled: 2, Started: 2, Success: 2},
		Usage:         metrics.UsageSummary{Samples: 1, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	var output bytes.Buffer
	if err := WriteHTML(&output, summary, HTMLOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<span class="value partial">partial</span>`,
		`Usage is incomplete`,
		`<span class="label">Cost</span><span class="value">unavailable</span>`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("HTML does not contain %q", want)
		}
	}
}

func TestWriteHTMLEscapesMetadataAndRequestValues(t *testing.T) {
	jsonlPath := filepath.Join(t.TempDir(), `<script>alert(1)<script>.jsonl`)
	jsonl := validRequestJSONL()
	if err := os.WriteFile(jsonlPath, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := WriteHTML(&output, metrics.RunSummary{SchemaVersion: 1}, HTMLOptions{
		Metadata:         Metadata{RunID: `<img src=x onerror=alert(1)>`},
		RequestJSONLPath: jsonlPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	report := output.String()
	if strings.Contains(report, `<img src=x`) || strings.Contains(report, `<script>alert`) {
		t.Fatalf("HTML contains unescaped input: %s", report)
	}
	if !strings.Contains(report, `&lt;img src=x onerror=alert(1)&gt;`) || !strings.Contains(report, `&lt;script&gt;alert(1)&lt;script&gt;.jsonl`) {
		t.Fatal("HTML does not contain escaped input")
	}
}

func TestWriteHTMLReturnsJSONLReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	if err := os.WriteFile(path, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := WriteHTML(&output, metrics.RunSummary{}, HTMLOptions{RequestJSONLPath: path})
	if err == nil || !strings.Contains(err.Error(), "record 1") {
		t.Fatalf("WriteHTML() error = %v, want record decode context", err)
	}
	if output.Len() != 0 {
		t.Fatalf("WriteHTML() wrote %d bytes before rejecting malformed input", output.Len())
	}
}

func TestWriteHTMLRejectsMalformedJSONLContractsBeforeOutput(t *testing.T) {
	valid := strings.TrimSuffix(validRequestJSONL(), "\n")
	tests := map[string]string{
		"missing required field":    strings.Replace(valid, `,"outcome":"success"`, "", 1) + "\n",
		"null required field":       strings.Replace(valid, `"status_code":200`, `"status_code":null`, 1) + "\n",
		"unsupported schema":        strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1) + "\n",
		"unknown field":             strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"extra":true`, 1) + "\n",
		"duplicate top-level field": strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1) + "\n",
		"unknown usage field":       strings.Replace(valid, `"total_tokens":100`, `"total_tokens":100,"extra":true`, 1) + "\n",
		"duplicate usage field":     strings.Replace(valid, `"prompt_tokens":80`, `"prompt_tokens":80,"prompt_tokens":80`, 1) + "\n",
		"invalid outcome":           strings.Replace(valid, `"outcome":"success"`, `"outcome":"other"`, 1) + "\n",
		"invalid status":            strings.Replace(valid, `"status_code":200`, `"status_code":500`, 1) + "\n",
		"http status zero":          httpErrorJSONL(valid, 0),
		"http status success":       httpErrorJSONL(valid, 200),
		"ttfb timing overflow":      strings.Replace(valid, `"ttfb_micros":2500`, `"ttfb_micros":3600000001`, 1) + "\n",
		"ttft timing overflow":      strings.Replace(valid, `"ttft_micros":12500`, `"ttft_micros":3600000001`, 1) + "\n",
		"chunk itl timing overflow": strings.Replace(valid, `"chunk_itl_micros":[3000,4000]`, `"chunk_itl_micros":[3600000001]`, 1) + "\n",
		"duration timing overflow":  strings.Replace(valid, `"duration_micros":112500`, `"duration_micros":3600000001`, 1) + "\n",
		"invalid usage status":      strings.Replace(valid, `"usage_status":"available"`, `"usage_status":"partial"`, 1) + "\n",
		"usage mismatch":            strings.Replace(valid, `"usage_status":"available"`, `"usage_status":"unavailable"`, 1) + "\n",
		"missing usage field":       strings.Replace(valid, `"prompt_tokens":80,`, "", 1) + "\n",
		"null prompt tokens":        strings.Replace(valid, `"prompt_tokens":80`, `"prompt_tokens":null`, 1) + "\n",
		"null completion tokens":    strings.Replace(valid, `"completion_tokens":20`, `"completion_tokens":null`, 1) + "\n",
		"null total tokens":         strings.Replace(valid, `"total_tokens":100`, `"total_tokens":null`, 1) + "\n",
		"two records on one line":   valid + valid + "\n",
		"blank line":                "\n",
		"unterminated final line":   valid,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "requests.jsonl")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err := WriteHTML(&output, fixtureSummary(), HTMLOptions{RequestJSONLPath: path})
			if err == nil || !strings.Contains(err.Error(), "record 1") {
				t.Fatalf("WriteHTML() error = %v, want record 1 contract error", err)
			}
			if output.Len() != 0 {
				t.Fatalf("WriteHTML() wrote %d bytes before rejecting malformed contract", output.Len())
			}
		})
	}
}

func TestRequestSnapshotIsStableAfterSourceMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	if err := os.WriteFile(path, []byte(validRequestJSONL()), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotRequestJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.close() })

	if err := os.WriteFile(path, []byte(validRequestJSONL()+"{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(snapshot.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), validRequestJSONL(); got != want {
		t.Fatalf("snapshot after source mutation = %q, want %q", got, want)
	}
}

func TestWriteHTMLRendersSnapshotWhenSourceChangesOnFirstOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	if err := os.WriteFile(path, []byte(validRequestJSONL()), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer := &mutatingWriter{
		writer: &output,
		mutate: func() error {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if _, err := file.WriteString("{not-json}\n"); err != nil {
				_ = file.Close()
				return err
			}
			return file.Close()
		},
	}
	if err := WriteHTML(writer, fixtureSummary(), HTMLOptions{RequestJSONLPath: path}); err != nil {
		t.Fatal(err)
	}
	if writer.err != nil {
		t.Fatal(writer.err)
	}
	if strings.Count(output.String(), `<td data-label="Outcome">success</td>`) != 1 {
		t.Fatal("report did not render exactly the preflighted request snapshot")
	}
}

func TestRequestSnapshotCapsTTFTStrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	var input strings.Builder
	for i := 0; i < ttftStripPointCap+17; i++ {
		input.WriteString(strings.Replace(validRequestJSONL(), `"ttft_micros":12500`, `"ttft_micros":`+strconv.Itoa(12500+i), 1))
	}
	if err := os.WriteFile(path, []byte(input.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotRequestJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.close() })
	if got := len(snapshot.ttft.Points); got > ttftStripPointCap {
		t.Fatalf("TTFT strip points = %d, exceeds hard cap %d", got, ttftStripPointCap)
	}
	if snapshot.ttft.Observed != ttftStripPointCap+17 {
		t.Fatalf("TTFT observations = %d, want %d", snapshot.ttft.Observed, ttftStripPointCap+17)
	}
}

func TestTTFTStripTracksCompleteExtremaAndRequestRange(t *testing.T) {
	var samples ttftSamples
	for request := 1; request <= ttftStripPointCap*3; request++ {
		value := int64(20_000 + request)
		if request == 2 {
			value = 1_000
		}
		if request == ttftStripPointCap*3-1 {
			value = 90_000
		}
		samples.add(request, value)
	}
	view := newTTFTStripView(samples, ttftStripPointCap*3)
	if view.Min != "1.000 ms" || view.Max != "90.000 ms" {
		t.Fatalf("TTFT bounds = %s..%s, want complete-stream extrema", view.Min, view.Max)
	}
	if view.XMin != "1" || view.XMax != strconv.Itoa(ttftStripPointCap*3) || view.YMin != "1.000" || view.YMax != "90.000" {
		t.Fatalf("TTFT plot range = x %s..%s, y %s..%s, want complete request/extrema range", view.XMin, view.XMax, view.YMin, view.YMax)
	}
}

func TestStrictContractAcceptsProducerStreamFailureStatus(t *testing.T) {
	for _, outcome := range []string{"timeout", "canceled", "stream_error"} {
		line := strings.Replace(validRequestJSONL(), `"outcome":"success"`, `"outcome":"`+outcome+`"`, 1)
		line = strings.Replace(line, `"usage_status":"available"`, `"usage_status":"unavailable"`, 1)
		line = strings.Replace(line, `"usage":{"prompt_tokens":80,"completion_tokens":20,"total_tokens":100}`, `"usage":null`, 1)
		if _, err := decodeRequestRecordLine([]byte(line)); err != nil {
			t.Errorf("%s status 200 producer record rejected: %v", outcome, err)
		}
	}
}

func TestStrictContractAcceptsHTTPStatusBoundaries(t *testing.T) {
	valid := strings.TrimSuffix(validRequestJSONL(), "\n")
	for _, statusCode := range []int{-1, 1, 99, 100, 199, 201, 599, 600, 999, 1000} {
		if _, err := decodeRequestRecordLine([]byte(httpErrorJSONL(valid, statusCode))); err != nil {
			t.Errorf("http_error status %d rejected: %v", statusCode, err)
		}
	}
}

func TestProducerRequestRecordPassesReportValidation(t *testing.T) {
	var jsonl bytes.Buffer
	result := probe.Result{
		StatusCode: 699,
		TTFB:       time.Hour,
		TTFT:       time.Hour,
		ChunkITL:   []time.Duration{time.Hour},
		Duration:   time.Hour,
	}
	if err := metrics.NewJSONLWriter(&jsonl).WriteResult(result, errors.New("provider error")); err != nil {
		t.Fatal(err)
	}
	record, err := decodeRequestRecordLine(jsonl.Bytes())
	if err != nil {
		t.Fatalf("report rejected producer record: %v\n%s", err, jsonl.String())
	}
	if record.Outcome != metrics.OutcomeHTTPError || record.StatusCode != 699 {
		t.Fatalf("validated record = %#v, want producer http_error status 699", record)
	}
}

func TestWriteHTMLReturnsJSONLOpenError(t *testing.T) {
	var output bytes.Buffer
	err := WriteHTML(&output, metrics.RunSummary{}, HTMLOptions{RequestJSONLPath: filepath.Join(t.TempDir(), "missing.jsonl")})
	if err == nil || !strings.Contains(err.Error(), "open request JSONL") {
		t.Fatalf("WriteHTML() error = %v, want open context", err)
	}
	if output.Len() != 0 {
		t.Fatalf("WriteHTML() wrote %d bytes before rejecting missing input", output.Len())
	}
}

func TestWritersPropagateOutputErrors(t *testing.T) {
	wantErr := errors.New("disk full")
	for name, write := range map[string]func() error{
		"JSON": func() error { return WriteJSON(errorWriter{wantErr}, fixtureSummary()) },
		"HTML": func() error { return WriteHTML(errorWriter{wantErr}, fixtureSummary(), HTMLOptions{}) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); !errors.Is(err, wantErr) {
				t.Fatalf("writer error = %v, want %v", err, wantErr)
			}
		})
	}
}

func ExampleWriteJSON() {
	summary := metrics.RunSummary{
		SchemaVersion: metrics.SchemaVersion,
		Counts:        metrics.Counts{Scheduled: 1, Started: 1, Success: 1},
	}
	_ = WriteJSON(os.Stdout, summary)
	// Output:
	// {
	//   "schema_version": 1,
	//   "counts": {
	//     "scheduled": 1,
	//     "started": 1,
	//     "success": 1,
	//     "dropped": 0,
	//     "canceled": 0,
	//     "timeout": 0,
	//     "stream_error": 0,
	//     "http_error": 0,
	//     "error_rate": 0,
	//     "dropped_rate": 0
	//   },
	//   "metrics": {
	//     "ttfb": null,
	//     "ttft": null,
	//     "chunk_itl": null,
	//     "request_duration": null,
	//     "tokens_per_second": null
	//   },
	//   "usage": {
	//     "samples": 0,
	//     "complete": false,
	//     "prompt_tokens": 0,
	//     "completion_tokens": 0,
	//     "total_tokens": 0,
	//     "cost_usd": null
	//   },
	//   "slo_outcomes": null
	// }
}

type errorWriter struct {
	err error
}

type mutatingWriter struct {
	writer  *bytes.Buffer
	mutate  func() error
	mutated bool
	err     error
}

func (w *mutatingWriter) Write(data []byte) (int, error) {
	if !w.mutated {
		w.mutated = true
		w.err = w.mutate()
		if w.err != nil {
			return 0, w.err
		}
	}
	return w.writer.Write(data)
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func fixtureSummary() metrics.RunSummary {
	cost := 0.000042
	return metrics.RunSummary{
		SchemaVersion: 1,
		Counts: metrics.Counts{
			Scheduled: 10, Started: 9, Success: 5, Dropped: 1, Canceled: 1,
			Timeout: 1, StreamError: 1, HTTPError: 1, ErrorRate: 0.3, DroppedRate: 0.1,
		},
		Metrics: metrics.MetricSummaries{
			TTFB:            histogram(5, 2.5, 4.25, 3.3, 3.5, 4, 4.1, 4.2),
			TTFT:            histogram(5, 12.5, 40, 24.75, 25, 38, 39, 40),
			ChunkITL:        histogram(15, 3, 10, 5.4, 5, 8, 9, 10),
			RequestDuration: histogram(5, 112.5, 160, 134.2, 130, 155, 158, 160),
			TokensPerSecond: &metrics.HistogramSummary{Count: 5, Min: 44, Max: 60, Mean: 52, P50: 52, P90: 59, P95: 60, P99: 60, LowestTrackable: 0.001, HighestTrackable: 3600000, SignificantFigures: 3, Unit: "tokens_per_second", Distribution: []metrics.DistributionPoint{{Percentile: 0, Value: 44}, {Percentile: 50, Value: 52}, {Percentile: 90, Value: 59}, {Percentile: 100, Value: 60}}},
		},
		Usage: metrics.UsageSummary{
			Samples: 4, Complete: false, PromptTokens: 320, CompletionTokens: 80,
			TotalTokens: 400, CostUSD: &cost,
		},
	}
}

func histogram(count int64, min, max, mean, p50, p90, p95, p99 float64) *metrics.HistogramSummary {
	return &metrics.HistogramSummary{
		Count: count, Min: min, Max: max, Mean: mean, P50: p50, P90: p90, P95: p95, P99: p99,
		LowestTrackable: 0.001, HighestTrackable: 3600000, SignificantFigures: 3, Unit: "ms",
		Distribution: []metrics.DistributionPoint{{Percentile: 0, Value: min}, {Percentile: 50, Value: p50}, {Percentile: 90, Value: p90}, {Percentile: 95, Value: p95}, {Percentile: 99, Value: p99}, {Percentile: 100, Value: max}},
	}
}

func validRequestJSONL() string {
	return `{"schema_version":1,"outcome":"success","status_code":200,"ttfb_micros":2500,"ttft_micros":12500,"chunk_itl_micros":[3000,4000],"duration_micros":112500,"usage_status":"available","usage":{"prompt_tokens":80,"completion_tokens":20,"total_tokens":100}}` + "\n"
}

func httpErrorJSONL(valid string, statusCode int) string {
	line := strings.Replace(valid, `"outcome":"success"`, `"outcome":"http_error"`, 1)
	line = strings.Replace(line, `"status_code":200`, `"status_code":`+strconv.Itoa(statusCode), 1)
	return line + "\n"
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s; run go test ./internal/report -update", path)
	}
}
