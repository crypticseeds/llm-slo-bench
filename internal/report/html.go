package report

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/metrics"
	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

const ttftStripPointCap = 120

type htmlData struct {
	Summary       metrics.RunSummary
	Metadata      metadataView
	Metrics       []metricView
	Outcomes      []outcomeView
	UsageState    string
	UsageNote     string
	Cost          string
	PromptTokens  string
	OutputTokens  string
	TotalTokens   string
	ZeroScheduled bool
	HasRequests   bool
	RequestSource string
	RequestCount  int
	TTFTStrip     ttftStripView
	UPlotJS       template.JS
	UPlotCSS      template.CSS
	UPlotLicense  string
}

type metadataView struct {
	RunID             string
	Scenario          string
	ConfigFile        string
	Target            string
	Model             string
	StartedAt         string
	Duration          string
	ToolVersion       string
	ConfigFingerprint string
}

type metricView struct {
	Name             string
	ID               string
	Summary          *metrics.HistogramSummary
	Bars             []percentileBar
	DistributionJSON template.JS
}

type percentileBar struct {
	Label string
	Value string
	Width string
}

type outcomeView struct {
	Name       string
	Count      int64
	Percentage string
	Width      string
	Class      string
}

type ttftStripView struct {
	HasData  bool
	Observed int
	Min      string
	Max      string
	XMin     string
	XMax     string
	YMin     string
	YMax     string
	DataJSON template.JS
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

type requestSnapshot struct {
	file  *os.File
	path  string
	count int
	ttft  ttftSamples
}

type ttftSample struct {
	Request int
	Micros  int64
}

type ttftSamples struct {
	Points   []ttftSample
	Observed int
	Min      int64
	Max      int64
	stride   int
}

var htmlReportTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"number":  formatNumber,
	"percent": func(value float64) string { return strconv.FormatFloat(value*100, 'f', 2, 64) + "%" },
}).Parse(htmlTemplate))

// WriteHTML writes a self-contained report. Request JSONL is strictly validated
// and copied to a stable temporary snapshot before any destination bytes are written.
func WriteHTML(writer io.Writer, summary metrics.RunSummary, options HTMLOptions) (returnErr error) {
	var snapshot *requestSnapshot
	if options.RequestJSONLPath != "" {
		var err error
		snapshot, err = snapshotRequestJSONL(options.RequestJSONLPath)
		if err != nil {
			return err
		}
		defer func() {
			if err := snapshot.close(); err != nil && returnErr == nil {
				returnErr = fmt.Errorf("remove request JSONL snapshot: %w", err)
			}
		}()
	}

	data := newHTMLData(summary, options, snapshot)
	if err := htmlReportTemplate.ExecuteTemplate(writer, "header", data); err != nil {
		return fmt.Errorf("write HTML report header: %w", err)
	}

	if snapshot != nil {
		reader := metrics.NewJSONLReader(snapshot.file)
		for number := 1; ; number++ {
			record, readErr := reader.Next()
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return fmt.Errorf("read request JSONL snapshot record %d: %w", number, readErr)
			}
			if err := htmlReportTemplate.ExecuteTemplate(writer, "request", newRequestView(number, record)); err != nil {
				return fmt.Errorf("write HTML request record %d: %w", number, err)
			}
		}
	}

	if err := htmlReportTemplate.ExecuteTemplate(writer, "footer", data); err != nil {
		return fmt.Errorf("write HTML report footer: %w", err)
	}
	return nil
}

func newHTMLData(summary metrics.RunSummary, options HTMLOptions, snapshot *requestSnapshot) htmlData {
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

	metricsViews := []metricView{
		newMetricView("TTFB", summary.Metrics.TTFB),
		newMetricView("TTFT", summary.Metrics.TTFT),
		newMetricView("Chunk ITL", summary.Metrics.ChunkITL),
		newMetricView("Request duration", summary.Metrics.RequestDuration),
		newMetricView("Tokens per second", summary.Metrics.TokensPerSecond),
	}
	data := htmlData{
		Summary:  summary,
		Metadata: metadataFor(options.Metadata),
		Metrics:  metricsViews,
		Outcomes: []outcomeView{
			newOutcomeView("Success", summary.Counts.Success, summary.Counts.Scheduled, "success"),
			newOutcomeView("Dropped", summary.Counts.Dropped, summary.Counts.Scheduled, "dropped"),
			newOutcomeView("Canceled", summary.Counts.Canceled, summary.Counts.Scheduled, "canceled"),
			newOutcomeView("Timeout", summary.Counts.Timeout, summary.Counts.Scheduled, "failed"),
			newOutcomeView("Stream error", summary.Counts.StreamError, summary.Counts.Scheduled, "failed"),
			newOutcomeView("HTTP error", summary.Counts.HTTPError, summary.Counts.Scheduled, "failed"),
		},
		UsageState:    usageState,
		UsageNote:     usageNote,
		Cost:          cost,
		PromptTokens:  promptTokens,
		OutputTokens:  outputTokens,
		TotalTokens:   totalTokens,
		ZeroScheduled: summary.Counts.Scheduled == 0,
		HasRequests:   snapshot != nil,
		RequestSource: filepath.Base(options.RequestJSONLPath),
		UPlotJS:       template.JS(uPlotJS),
		UPlotCSS:      template.CSS(uPlotCSS),
		UPlotLicense:  uPlotLicense,
	}
	if snapshot != nil {
		data.RequestCount = snapshot.count
		data.TTFTStrip = newTTFTStripView(snapshot.ttft, snapshot.count)
	}
	return data
}

func newMetricView(name string, summary *metrics.HistogramSummary) metricView {
	view := metricView{Name: name, ID: strings.NewReplacer(" ", "-", "_", "-").Replace(strings.ToLower(name)), Summary: summary}
	if summary == nil {
		return view
	}
	view.Bars = []percentileBar{
		newPercentileBar("p50", summary.P50, summary.Min, summary.Max),
		newPercentileBar("p90", summary.P90, summary.Min, summary.Max),
		newPercentileBar("p95", summary.P95, summary.Min, summary.Max),
		newPercentileBar("p99", summary.P99, summary.Min, summary.Max),
	}
	if summary.Unit == "ms" {
		percentiles := make([]float64, len(summary.Distribution))
		values := make([]float64, len(summary.Distribution))
		for index, point := range summary.Distribution {
			percentiles[index] = point.Percentile
			values[index] = point.Value
		}
		view.DistributionJSON = mustJSONForScript([][]float64{percentiles, values})
	}
	return view
}

func newPercentileBar(label string, value, min, max float64) percentileBar {
	width := 100.0
	if max > min {
		width = (value - min) / (max - min) * 100
	}
	width = math.Max(0, math.Min(100, width))
	return percentileBar{Label: label, Value: formatNumber(value), Width: strconv.FormatFloat(width, 'f', 3, 64)}
}

func newOutcomeView(name string, count, scheduled int64, class string) outcomeView {
	percentage := "n/a"
	width := "0.000"
	if scheduled > 0 {
		value := float64(count) / float64(scheduled) * 100
		percentage = strconv.FormatFloat(value, 'f', 2, 64) + "%"
		width = strconv.FormatFloat(math.Max(0, math.Min(100, value)), 'f', 3, 64)
	}
	return outcomeView{Name: name, Count: count, Percentage: percentage, Width: width, Class: class}
}

func newTTFTStripView(samples ttftSamples, requestCount int) ttftStripView {
	view := ttftStripView{Observed: samples.Observed}
	if len(samples.Points) == 0 {
		return view
	}
	requests := make([]float64, len(samples.Points))
	values := make([]float64, len(samples.Points))
	for index, point := range samples.Points {
		requests[index] = float64(point.Request)
		values[index] = float64(point.Micros) / 1000
	}
	view.HasData = true
	view.Min = microsValue(samples.Min)
	view.Max = microsValue(samples.Max)
	view.XMin = "1"
	view.XMax = strconv.Itoa(max(1, requestCount))
	view.YMin = strconv.FormatFloat(float64(samples.Min)/1000, 'f', 3, 64)
	view.YMax = strconv.FormatFloat(float64(samples.Max)/1000, 'f', 3, 64)
	view.DataJSON = mustJSONForScript([][]float64{requests, values})
	return view
}

func mustJSONForScript(value any) template.JS {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return template.JS(encoded)
}

func snapshotRequestJSONL(path string) (*requestSnapshot, error) {
	source, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open request JSONL for report: %w", err)
	}
	defer source.Close()

	spool, err := os.CreateTemp("", "llm-slo-bench-report-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create request JSONL snapshot: %w", err)
	}
	snapshot := &requestSnapshot{file: spool, path: spool.Name()}
	failed := true
	defer func() {
		if failed {
			_ = snapshot.close()
		}
	}()

	reader := bufio.NewReader(source)
	for number := 1; ; number++ {
		line, readErr := reader.ReadBytes('\n')
		if errors.Is(readErr, io.EOF) && len(line) == 0 {
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("read request JSONL record %d: %w", number, readErr)
		}
		if errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("read request JSONL record %d: record is not newline-terminated", number)
		}
		record, err := decodeRequestRecordLine(line)
		if err != nil {
			return nil, fmt.Errorf("read request JSONL record %d: %w", number, err)
		}
		if _, err := spool.Write(line); err != nil {
			return nil, fmt.Errorf("write request JSONL snapshot record %d: %w", number, err)
		}
		snapshot.count++
		if record.TTFTMicros != nil {
			snapshot.ttft.add(number, *record.TTFTMicros)
		}
	}
	if err := source.Close(); err != nil {
		return nil, fmt.Errorf("close request JSONL source: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind request JSONL snapshot: %w", err)
	}
	failed = false
	return snapshot, nil
}

func (s *requestSnapshot) close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	removeErr := os.Remove(s.path)
	if err != nil {
		return err
	}
	return removeErr
}

func (s *ttftSamples) add(request int, micros int64) {
	s.Observed++
	if s.Observed == 1 || micros < s.Min {
		s.Min = micros
	}
	if s.Observed == 1 || micros > s.Max {
		s.Max = micros
	}
	if s.stride == 0 {
		s.stride = 1
	}
	if (s.Observed-1)%s.stride != 0 {
		return
	}
	if len(s.Points) == ttftStripPointCap {
		compacted := s.Points[:0]
		for index, point := range s.Points {
			if index%2 == 0 {
				compacted = append(compacted, point)
			}
		}
		s.Points = compacted
		s.stride *= 2
		if (s.Observed-1)%s.stride != 0 {
			return
		}
	}
	s.Points = append(s.Points, ttftSample{Request: request, Micros: micros})
}

func decodeRequestRecordLine(line []byte) (metrics.RequestRecord, error) {
	if len(bytes.TrimSpace(line)) == 0 {
		return metrics.RequestRecord{}, errors.New("blank lines are not valid records")
	}
	fields, err := decodeUniqueObject(line)
	if err != nil {
		return metrics.RequestRecord{}, fmt.Errorf("decode JSON object: %w", err)
	}
	required := []string{"schema_version", "outcome", "status_code", "ttfb_micros", "ttft_micros", "chunk_itl_micros", "duration_micros", "usage_status", "usage"}
	for _, name := range required {
		value, ok := fields[name]
		if !ok {
			return metrics.RequestRecord{}, fmt.Errorf("missing required field %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) && name != "ttfb_micros" && name != "ttft_micros" && name != "chunk_itl_micros" && name != "duration_micros" && name != "usage" {
			return metrics.RequestRecord{}, fmt.Errorf("required field %q must not be null", name)
		}
	}

	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var record metrics.RequestRecord
	if err := decoder.Decode(&record); err != nil {
		return metrics.RequestRecord{}, fmt.Errorf("decode request record: %w", err)
	}
	if err := requireDecoderEOF(decoder); err != nil {
		return metrics.RequestRecord{}, err
	}
	if err := validateRequestRecord(record, fields["usage"]); err != nil {
		return metrics.RequestRecord{}, err
	}
	return record, nil
}

func requireDecoderEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("line contains more than one JSON value")
		}
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return nil
}

func validateRequestRecord(record metrics.RequestRecord, rawUsage json.RawMessage) error {
	if record.SchemaVersion != metrics.SchemaVersion {
		return fmt.Errorf("schema_version %d is unsupported; expected %d", record.SchemaVersion, metrics.SchemaVersion)
	}
	validOutcome := map[metrics.Outcome]bool{
		metrics.OutcomeSuccess: true, metrics.OutcomeDropped: true, metrics.OutcomeCanceled: true,
		metrics.OutcomeTimeout: true, metrics.OutcomeStreamError: true, metrics.OutcomeHTTPError: true,
	}
	if !validOutcome[record.Outcome] {
		return fmt.Errorf("outcome %q is invalid", record.Outcome)
	}
	if record.Outcome == metrics.OutcomeSuccess && record.StatusCode != 200 {
		return errors.New("success outcome requires status_code 200")
	}
	if record.Outcome == metrics.OutcomeHTTPError && (record.StatusCode == 0 || record.StatusCode == 200) {
		return errors.New("http_error outcome requires a nonzero, non-200 status_code")
	}
	if record.Outcome == metrics.OutcomeDropped && record.StatusCode != 0 {
		return errors.New("dropped outcome requires status_code 0")
	}
	if record.Outcome != metrics.OutcomeSuccess && record.Outcome != metrics.OutcomeHTTPError && record.Outcome != metrics.OutcomeDropped && record.StatusCode != 0 && record.StatusCode != 200 {
		return fmt.Errorf("%s outcome requires status_code 0 or 200", record.Outcome)
	}
	for name, value := range map[string]*int64{
		"ttfb_micros": record.TTFBMicros, "ttft_micros": record.TTFTMicros, "duration_micros": record.DurationMicros,
	} {
		if value != nil && (*value < metrics.LowestTrackableMicros || *value > metrics.HighestTrackableMicros) {
			return fmt.Errorf("%s must be between %d and %d when present", name, metrics.LowestTrackableMicros, metrics.HighestTrackableMicros)
		}
	}
	for _, value := range record.ChunkITLMicros {
		if value < metrics.LowestTrackableMicros || value > metrics.HighestTrackableMicros {
			return fmt.Errorf("chunk_itl_micros values must be between %d and %d", metrics.LowestTrackableMicros, metrics.HighestTrackableMicros)
		}
	}
	if record.UsageStatus != "available" && record.UsageStatus != "unavailable" {
		return fmt.Errorf("usage_status %q is invalid", record.UsageStatus)
	}
	if record.UsageStatus == "available" && record.Usage == nil || record.UsageStatus == "unavailable" && record.Usage != nil {
		return errors.New("usage_status must match usage presence")
	}
	if record.Usage != nil {
		if err := validateUsage(rawUsage, record.Usage); err != nil {
			return err
		}
	}
	return nil
}

func validateUsage(raw json.RawMessage, usage *probe.Usage) error {
	fields, err := decodeUniqueObject(raw)
	if err != nil {
		return fmt.Errorf("decode usage: %w", err)
	}
	for _, name := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		value, ok := fields[name]
		if !ok {
			return fmt.Errorf("usage missing required field %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("usage field %q must not be null", name)
		}
	}
	if len(fields) != 3 {
		return errors.New("usage contains an unknown field")
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return errors.New("usage token counts must be non-negative")
	}
	return nil
}

func decodeUniqueObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, errors.New("value must be an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("object field name must be a string")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := requireDecoderEOF(decoder); err != nil {
		return nil, err
	}
	return fields, nil
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
		ConfigFile:        valueOrNotProvided(metadata.ConfigFile),
		Target:            valueOrNotProvided(metadata.Target),
		Model:             valueOrNotProvided(metadata.Model),
		StartedAt:         startedAt,
		Duration:          duration,
		ToolVersion:       valueOrNotProvided(metadata.ToolVersion),
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
			values[i] = microsValue(value)
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
	return microsValue(*value)
}

func microsValue(value int64) string {
	return formatNumber(float64(value)/1000) + " ms"
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
