package metrics

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

func TestJSONLWriterAndReaderStreamConcurrentRecords(t *testing.T) {
	var output bytes.Buffer
	writer := NewJSONLWriter(&output)
	const records = 32
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(records)
	for i := 0; i < records; i++ {
		go func() {
			defer wait.Done()
			<-start
			result := successfulResult(10 * time.Millisecond)
			result.Usage = &probe.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}
			if err := writer.WriteResult(result, nil); err != nil {
				t.Errorf("WriteResult() error = %v", err)
			}
		}()
	}
	close(start)
	wait.Wait()

	if lines := strings.Count(output.String(), "\n"); lines != records {
		t.Fatalf("JSONL lines = %d, want %d", lines, records)
	}
	reader := NewJSONLReader(bytes.NewReader(output.Bytes()))
	for i := 0; i < records; i++ {
		record, err := reader.Next()
		if err != nil {
			t.Fatalf("Next() record %d error = %v", i, err)
		}
		if record.Outcome != OutcomeSuccess || record.UsageStatus != "available" || record.Usage == nil {
			t.Fatalf("record = %#v, want successful usage-backed record", record)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() final error = %v, want io.EOF", err)
	}
}

func TestOpenJSONLAppendsInsteadOfTruncating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	for i := 0; i < 2; i++ {
		writer, err := OpenJSONL(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteResult(successfulResult(10*time.Millisecond), nil); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(contents), "\n"); lines != 2 {
		t.Fatalf("JSONL lines = %d, want 2 appended records", lines)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestOpenJSONLRejectsIncompleteExistingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	if err := os.WriteFile(path, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONL(path); err == nil || !strings.Contains(err.Error(), "incomplete record") {
		t.Fatalf("OpenJSONL() error = %v, want incomplete record error", err)
	}
}

func TestRequestRecordContainsOnlyBoundedDiagnosticFields(t *testing.T) {
	result := probe.Result{
		StatusCode: 200,
		TTFB:       2 * time.Millisecond,
		TTFT:       5 * time.Millisecond,
		ChunkITL:   []time.Duration{3 * time.Millisecond},
		Duration:   12 * time.Millisecond,
	}
	record, err := RequestRecordFromResult(result, errors.New("stream disconnected with secret body"))
	if err != nil {
		t.Fatal(err)
	}
	if record.Outcome != OutcomeStreamError || record.UsageStatus != "unavailable" || record.Usage != nil {
		t.Fatalf("record = %#v, want stream error with unavailable usage", record)
	}
	var output bytes.Buffer
	if err := NewJSONLWriter(&output).Write(record); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "secret body") || strings.Contains(output.String(), "prompt") || strings.Contains(output.String(), "content") {
		t.Fatalf("JSONL leaked request/response text: %s", output.String())
	}
}

func TestJSONLReaderReturnsDecodeErrorAtMalformedRecord(t *testing.T) {
	reader := NewJSONLReader(strings.NewReader("{not-json}\n"))
	if _, err := reader.Next(); err == nil {
		t.Fatal("Next() error = nil, want malformed JSON error")
	}
}
