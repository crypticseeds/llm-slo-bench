package report

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/metrics"
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
			Target:            "mock.local",
			Model:             "mock-model",
			StartedAt:         time.Date(2026, time.August, 11, 14, 30, 0, 0, time.FixedZone("BST", 3600)),
			Duration:          45 * time.Second,
			ConfigFingerprint: "sha256:abc123",
		},
		RequestJSONLPath: jsonlPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "report.golden.html", output.Bytes())
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
		`<td class="empty" colspan="9">no samples</td>`,
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
	jsonlPath := filepath.Join(t.TempDir(), "requests.jsonl")
	jsonl := `{"schema_version":1,"outcome":"<script>alert(1)</script>","status_code":500,"ttfb_micros":null,"ttft_micros":null,"chunk_itl_micros":null,"duration_micros":null,"usage_status":"unavailable","usage":null}` + "\n"
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
	if !strings.Contains(report, `&lt;img src=x onerror=alert(1)&gt;`) || !strings.Contains(report, `&lt;script&gt;alert(1)&lt;/script&gt;`) {
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
	//   }
	// }
}

type errorWriter struct {
	err error
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
			TokensPerSecond: &metrics.HistogramSummary{Count: 5, Min: 44, Max: 60, Mean: 52, P50: 52, P90: 59, P95: 60, P99: 60, LowestTrackable: 0.001, HighestTrackable: 3600000, SignificantFigures: 3, Unit: "tokens_per_second"},
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
	}
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
