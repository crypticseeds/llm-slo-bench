package probe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/mockserver"
)

func TestRunMeasuresTTFBAndSemanticTTFTSeparately(t *testing.T) {
	cfg := mockserver.Config{
		Profile:       mockserver.Profile{Name: "test", FirstTokenDelay: 40 * time.Millisecond, ChunkDelay: 15 * time.Millisecond},
		ChunkCount:    3,
		Fault:         mockserver.FaultNone,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	server := httptest.NewServer(mockserver.NewHandler(cfg))
	defer server.Close()

	result, err := Run(context.Background(), Config{
		Endpoint:            server.URL + "/v1/chat/completions",
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             time.Second,
		StreamIdleTimeout:   500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TTFB <= 0 {
		t.Fatalf("TTFB = %s, want a positive duration", result.TTFB)
	}
	if result.TTFT < 35*time.Millisecond {
		t.Fatalf("TTFT = %s, want semantic delay near 40ms", result.TTFT)
	}
	if result.TTFB >= result.TTFT {
		t.Fatalf("TTFB = %s, TTFT = %s; role event should make TTFB earlier", result.TTFB, result.TTFT)
	}
	if result.ContentEvents != 3 {
		t.Fatalf("ContentEvents = %d, want 3", result.ContentEvents)
	}
	if len(result.ChunkITL) != 2 {
		t.Fatalf("len(ChunkITL) = %d, want 2", len(result.ChunkITL))
	}
	if result.Usage == nil || result.Usage.CompletionTokens != 3 {
		t.Fatalf("Usage = %#v, want 3 completion tokens", result.Usage)
	}
}

func TestRunRejectsMalformedStream(t *testing.T) {
	cfg := mockserver.Config{
		Profile:       mockserver.Profile{Name: "test"},
		ChunkCount:    2,
		Fault:         mockserver.FaultMalformed,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	server := httptest.NewServer(mockserver.NewHandler(cfg))
	defer server.Close()

	result, err := Run(context.Background(), Config{
		Endpoint:            server.URL + "/v1/chat/completions",
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             time.Second,
		StreamIdleTimeout:   500 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "decode OpenAI stream event") {
		t.Fatalf("Run() error = %v, want malformed event error", err)
	}
	if !errors.Is(err, ErrDecodeEvent) || result.Dispatch.IsZero() || result.Duration <= 0 {
		t.Fatalf("result=%+v error=%v, want typed decode error with failure timing", result, err)
	}
}

func TestEventDecoderHandlesSSELineEndingsCommentsAndMultilineData(t *testing.T) {
	input := ": keepalive\rdata: first\r\ndata: second\n\n"
	decoder := newEventDecoder(strings.NewReader(input))
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Data != "first\nsecond" {
		t.Fatalf("Data = %q, want %q", event.Data, "first\nsecond")
	}
}

func TestEventDecoderDiscardsIncompleteEventAtEOF(t *testing.T) {
	decoder := newEventDecoder(strings.NewReader("data: incomplete"))
	_, err := decoder.Next()
	if err != io.EOF {
		t.Fatalf("Next() error = %v, want io.EOF", err)
	}
}

func TestEventDecoderDispatchesBareCRWithoutWaitingForNextByte(t *testing.T) {
	reader, writer := io.Pipe()
	decoder := newEventDecoder(reader)
	done := make(chan event, 1)
	go func() {
		event, _ := decoder.Next()
		done <- event
	}()

	if _, err := io.WriteString(writer, "data: ready\r\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.Data != "ready" {
			t.Fatalf("Data = %q, want ready", got.Data)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("bare CR event waited for another byte")
	}
	writer.Close()
}

func TestRunReturnsIdleTimeoutForStalledStream(t *testing.T) {
	cfg := mockserver.Config{
		Profile:       mockserver.Profile{Name: "test"},
		ChunkCount:    2,
		Fault:         mockserver.FaultStall,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	server := httptest.NewServer(mockserver.NewHandler(cfg))
	defer server.Close()

	result, err := Run(context.Background(), Config{
		Endpoint:            server.URL + "/v1/chat/completions",
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             time.Second,
		StreamIdleTimeout:   20 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "stream idle timeout") {
		t.Fatalf("Run() error = %v, want idle timeout", err)
	}
	if !errors.Is(err, ErrStreamIdleTimeout) || result.Dispatch.IsZero() || result.Duration <= 0 {
		t.Fatalf("result=%+v error=%v, want typed idle error with failure timing", result, err)
	}
}

func TestRunRejectsPrematureDisconnect(t *testing.T) {
	cfg := mockserver.Config{
		Profile:       mockserver.Profile{Name: "test"},
		ChunkCount:    2,
		Fault:         mockserver.FaultDisconnect,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	server := httptest.NewServer(mockserver.NewHandler(cfg))
	defer server.Close()

	_, err := Run(context.Background(), Config{
		Endpoint:            server.URL + "/v1/chat/completions",
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             time.Second,
		StreamIdleTimeout:   time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "before [DONE]") {
		t.Fatalf("Run() error = %v, want premature EOF", err)
	}
}

func TestRunRejectsInvalidEventStreamMediaType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-streaming")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	_, err := Run(context.Background(), Config{
		Endpoint:            server.URL,
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             time.Second,
		StreamIdleTimeout:   time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "Content-Type") {
		t.Fatalf("Run() error = %v, want media type error", err)
	}
}

func TestRunHonorsParentCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Config{
			Endpoint:            server.URL,
			Model:               "mock-model",
			Prompt:              "hello",
			MaxCompletionTokens: 8,
			Timeout:             time.Second,
			StreamIdleTimeout:   time.Second,
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request to start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run() returned nil after parent cancellation")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run() did not return after parent cancellation")
	}
}

func TestRunHonorsTotalRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = io.WriteString(w, ": keepalive\n\n")
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer server.Close()

	_, err := Run(context.Background(), Config{
		Endpoint:            server.URL,
		Model:               "mock-model",
		Prompt:              "hello",
		MaxCompletionTokens: 8,
		Timeout:             25 * time.Millisecond,
		StreamIdleTimeout:   time.Second,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context deadline exceeded", err)
	}
}

func TestEventDecoderHandlesFragmentedReads(t *testing.T) {
	decoder := newEventDecoder(&fragmentedReader{chunks: [][]byte{
		[]byte("da"),
		[]byte("ta: frag"),
		[]byte("mented\r"),
		[]byte("\n"),
		[]byte("\r"),
		[]byte("\n"),
	}})
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Data != "fragmented" {
		t.Fatalf("Data = %q, want fragmented", event.Data)
	}
}

type fragmentedReader struct {
	chunks [][]byte
}

func (r *fragmentedReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(p, chunk), nil
}
