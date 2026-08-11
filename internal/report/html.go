package report

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/metrics"
)

type htmlData struct {
	Summary       metrics.RunSummary
	Metadata      metadataView
	Metrics       []metricView
	UsageState    string
	UsageNote     string
	Cost          string
	PromptTokens  string
	OutputTokens  string
	TotalTokens   string
	ZeroScheduled bool
	HasRequests   bool
	RequestSource string
}

type metadataView struct {
	RunID             string
	Scenario          string
	Target            string
	Model             string
	StartedAt         string
	Duration          string
	ConfigFingerprint string
}

type metricView struct {
	Name    string
	Summary *metrics.HistogramSummary
}

type requestView struct {
	Number           int
	Record           metrics.RequestRecord
	TTFB             string
	TTFT             string
	ChunkITL         string
	Duration         string
	PromptTokens     string
	CompletionTokens string
	TotalTokens      string
}

var htmlReportTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"number":  formatNumber,
	"percent": func(value float64) string { return strconv.FormatFloat(value*100, 'f', 2, 64) + "%" },
}).Parse(htmlTemplate))

// WriteHTML writes a self-contained report. When RequestJSONLPath is set,
// records are decoded and rendered one at a time rather than loaded into memory.
func WriteHTML(writer io.Writer, summary metrics.RunSummary, options HTMLOptions) error {
	var requestFile *os.File
	if options.RequestJSONLPath != "" {
		var err error
		requestFile, err = openValidatedJSONL(options.RequestJSONLPath)
		if err != nil {
			return err
		}
	}
	closeRequestFile := func() {
		if requestFile != nil {
			_ = requestFile.Close()
		}
	}

	data := newHTMLData(summary, options)
	if err := htmlReportTemplate.ExecuteTemplate(writer, "header", data); err != nil {
		closeRequestFile()
		return fmt.Errorf("write HTML report header: %w", err)
	}

	if requestFile != nil {
		reader := metrics.NewJSONLReader(requestFile)
		for number := 1; ; number++ {
			record, readErr := reader.Next()
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				closeRequestFile()
				return fmt.Errorf("read request JSONL record %d: %w", number, readErr)
			}
			if err := htmlReportTemplate.ExecuteTemplate(writer, "request", newRequestView(number, record)); err != nil {
				closeRequestFile()
				return fmt.Errorf("write HTML request record %d: %w", number, err)
			}
		}
		if err := requestFile.Close(); err != nil {
			return fmt.Errorf("close request JSONL after report: %w", err)
		}
		requestFile = nil
	}

	if err := htmlReportTemplate.ExecuteTemplate(writer, "footer", data); err != nil {
		return fmt.Errorf("write HTML report footer: %w", err)
	}
	return nil
}

func newHTMLData(summary metrics.RunSummary, options HTMLOptions) htmlData {
	usageState := "complete"
	usageNote := "Authoritative usage is present for every successful request."
	if summary.Usage.Samples == 0 {
		usageState = "unavailable"
		usageNote = "No authoritative usage samples were reported; token totals and cost are unavailable."
	} else if !summary.Usage.Complete {
		usageState = "partial"
		usageNote = "Usage is incomplete; token and cost totals include only requests with authoritative usage."
	}
	cost := "unavailable"
	promptTokens := "unavailable"
	outputTokens := "unavailable"
	totalTokens := "unavailable"
	if summary.Usage.CostUSD != nil {
		cost = "$" + strconv.FormatFloat(*summary.Usage.CostUSD, 'f', 6, 64)
	}
	if summary.Usage.Samples > 0 {
		promptTokens = strconv.FormatInt(summary.Usage.PromptTokens, 10)
		outputTokens = strconv.FormatInt(summary.Usage.CompletionTokens, 10)
		totalTokens = strconv.FormatInt(summary.Usage.TotalTokens, 10)
	}

	return htmlData{
		Summary:  summary,
		Metadata: metadataFor(options.Metadata),
		Metrics: []metricView{
			{Name: "TTFB", Summary: summary.Metrics.TTFB},
			{Name: "TTFT", Summary: summary.Metrics.TTFT},
			{Name: "Chunk ITL", Summary: summary.Metrics.ChunkITL},
			{Name: "Request duration", Summary: summary.Metrics.RequestDuration},
			{Name: "Tokens per second", Summary: summary.Metrics.TokensPerSecond},
		},
		UsageState:    usageState,
		UsageNote:     usageNote,
		Cost:          cost,
		PromptTokens:  promptTokens,
		OutputTokens:  outputTokens,
		TotalTokens:   totalTokens,
		ZeroScheduled: summary.Counts.Scheduled == 0,
		HasRequests:   options.RequestJSONLPath != "",
		RequestSource: filepath.Base(options.RequestJSONLPath),
	}
}

func openValidatedJSONL(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open request JSONL for report: %w", err)
	}
	reader := metrics.NewJSONLReader(file)
	for number := 1; ; number++ {
		_, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("read request JSONL record %d: %w", number, readErr)
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("rewind request JSONL for report: %w", err)
	}
	return file, nil
}

func metadataFor(metadata Metadata) metadataView {
	startedAt := "not provided"
	if !metadata.StartedAt.IsZero() {
		startedAt = metadata.StartedAt.UTC().Format(time.RFC3339)
	}
	duration := "not provided"
	if metadata.Duration != 0 {
		duration = metadata.Duration.String()
	}
	return metadataView{
		RunID:             valueOrNotProvided(metadata.RunID),
		Scenario:          valueOrNotProvided(metadata.Scenario),
		Target:            valueOrNotProvided(metadata.Target),
		Model:             valueOrNotProvided(metadata.Model),
		StartedAt:         startedAt,
		Duration:          duration,
		ConfigFingerprint: valueOrNotProvided(metadata.ConfigFingerprint),
	}
}

func newRequestView(number int, record metrics.RequestRecord) requestView {
	view := requestView{
		Number:           number,
		Record:           record,
		TTFB:             micros(record.TTFBMicros),
		TTFT:             micros(record.TTFTMicros),
		ChunkITL:         "unavailable",
		Duration:         micros(record.DurationMicros),
		PromptTokens:     "unavailable",
		CompletionTokens: "unavailable",
		TotalTokens:      "unavailable",
	}
	if len(record.ChunkITLMicros) > 0 {
		values := make([]string, len(record.ChunkITLMicros))
		for i, value := range record.ChunkITLMicros {
			values[i] = formatNumber(float64(value)/1000) + " ms"
		}
		view.ChunkITL = strings.Join(values, ", ")
	}
	if record.Usage != nil {
		view.PromptTokens = strconv.Itoa(record.Usage.PromptTokens)
		view.CompletionTokens = strconv.Itoa(record.Usage.CompletionTokens)
		view.TotalTokens = strconv.Itoa(record.Usage.TotalTokens)
	}
	return view
}

func micros(value *int64) string {
	if value == nil {
		return "unavailable"
	}
	return formatNumber(float64(*value)/1000) + " ms"
}

func valueOrNotProvided(value string) string {
	if value == "" {
		return "not provided"
	}
	return value
}

func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', 3, 64)
}
