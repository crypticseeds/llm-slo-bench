package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

type RequestRecord struct {
	SchemaVersion  int          `json:"schema_version"`
	Outcome        Outcome      `json:"outcome"`
	StatusCode     int          `json:"status_code"`
	TTFBMicros     *int64       `json:"ttfb_micros"`
	TTFTMicros     *int64       `json:"ttft_micros"`
	ChunkITLMicros []int64      `json:"chunk_itl_micros"`
	DurationMicros *int64       `json:"duration_micros"`
	UsageStatus    string       `json:"usage_status"`
	Usage          *probe.Usage `json:"usage"`
}

type JSONLWriter struct {
	mu      sync.Mutex
	encoder *json.Encoder
	closer  io.Closer
}

func NewJSONLWriter(writer io.Writer) *JSONLWriter {
	return &JSONLWriter{encoder: json.NewEncoder(writer)}
}

// OpenJSONL opens path in append mode so an existing request log is preserved.
func OpenJSONL(path string) (*JSONLWriter, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open request JSONL: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat request JSONL: %w", err)
	}
	if info.Size() > 0 {
		var last [1]byte
		if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
			file.Close()
			return nil, fmt.Errorf("inspect request JSONL: %w", err)
		}
		if last[0] != '\n' {
			file.Close()
			return nil, errors.New("request JSONL ends with an incomplete record")
		}
	}
	return &JSONLWriter{encoder: json.NewEncoder(file), closer: file}, nil
}

func (w *JSONLWriter) Write(record RequestRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.encoder.Encode(record); err != nil {
		return fmt.Errorf("write request JSONL: %w", err)
	}
	return nil
}

func (w *JSONLWriter) WriteResult(result probe.Result, requestErr error) error {
	record, err := RequestRecordFromResult(result, requestErr)
	if err != nil {
		return err
	}
	return w.Write(record)
}

func (w *JSONLWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closer == nil {
		return nil
	}
	err := w.closer.Close()
	w.closer = nil
	if err != nil {
		return fmt.Errorf("close request JSONL: %w", err)
	}
	return nil
}

func RequestRecordFromResult(result probe.Result, requestErr error) (RequestRecord, error) {
	record := RequestRecord{
		SchemaVersion: SchemaVersion,
		Outcome:       ClassifyOutcome(result, requestErr),
		StatusCode:    result.StatusCode,
		UsageStatus:   "unavailable",
		Usage:         result.Usage,
	}
	if result.Usage != nil {
		record.UsageStatus = "available"
	}
	var err error
	if result.TTFB > 0 {
		if record.TTFBMicros, err = durationPointer("ttfb", result.TTFB); err != nil {
			return RequestRecord{}, err
		}
	}
	if result.TTFT > 0 {
		if record.TTFTMicros, err = durationPointer("ttft", result.TTFT); err != nil {
			return RequestRecord{}, err
		}
	}
	if result.Duration > 0 {
		if record.DurationMicros, err = durationPointer("request duration", result.Duration); err != nil {
			return RequestRecord{}, err
		}
	}
	if len(result.ChunkITL) > 0 {
		record.ChunkITLMicros = make([]int64, len(result.ChunkITL))
		for i, duration := range result.ChunkITL {
			if record.ChunkITLMicros[i], err = durationMicros("chunk itl", duration); err != nil {
				return RequestRecord{}, err
			}
		}
	}
	return record, nil
}

func durationPointer(name string, duration time.Duration) (*int64, error) {
	value, err := durationMicros(name, duration)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

type JSONLReader struct {
	decoder *json.Decoder
}

func NewJSONLReader(reader io.Reader) *JSONLReader {
	return &JSONLReader{decoder: json.NewDecoder(reader)}
}

// Next decodes one record without loading the complete file into memory.
func (r *JSONLReader) Next() (RequestRecord, error) {
	var record RequestRecord
	if err := r.decoder.Decode(&record); err != nil {
		return RequestRecord{}, err
	}
	return record, nil
}
