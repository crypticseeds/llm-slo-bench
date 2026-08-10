package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/mockserver"
	"github.com/crypticseeds/llm-slo-bench/internal/probe"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: llm-slo-bench <mock|probe> [flags]")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "mock":
		err = runMock(ctx, args[1:])
	case "probe":
		err = runProbe(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Println("usage: llm-slo-bench <mock|probe> [flags]")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func runMock(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mock", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "address to listen on")
	profileName := fs.String("profile", "steady", "latency profile: fast, steady, or slow")
	firstTokenDelay := fs.Duration("first-token-delay", -1, "override delay before first content event")
	chunkDelay := fs.Duration("chunk-delay", -1, "override delay between content events")
	chunks := fs.Int("chunks", 4, "number of non-empty content events")
	fault := fs.String("fault", "none", "fault mode: none, http-error, malformed, disconnect, or stall")
	faultEvery := fs.Int("fault-every", 1, "apply the configured fault to every Nth request")
	faultAfter := fs.Int("fault-after", 1, "content event after which a stream fault occurs")
	stallDuration := fs.Duration("stall-duration", 30*time.Second, "duration of the stall fault")
	if err := fs.Parse(args); err != nil {
		return err
	}

	profile, err := mockserver.LookupProfile(*profileName)
	if err != nil {
		return err
	}
	if *firstTokenDelay >= 0 {
		profile.FirstTokenDelay = *firstTokenDelay
	}
	if *chunkDelay >= 0 {
		profile.ChunkDelay = *chunkDelay
	}

	cfg := mockserver.Config{
		Address:       *listen,
		Profile:       profile,
		ChunkCount:    *chunks,
		Fault:         mockserver.Fault(*fault),
		FaultEvery:    *faultEvery,
		FaultAfter:    *faultAfter,
		StallDuration: *stallDuration,
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	fmt.Printf("mock listening on http://%s/v1/chat/completions (profile=%s, first_token=%s, chunk=%s)\n", cfg.Address, profile.Name, profile.FirstTokenDelay, profile.ChunkDelay)
	return mockserver.Serve(ctx, cfg)
}

func runProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "http://127.0.0.1:8080/v1/chat/completions", "OpenAI-compatible chat completions endpoint")
	model := fs.String("model", "mock-model", "model name")
	promptText := fs.String("prompt", "Explain coordinated omission in two sentences.", "user prompt")
	maxTokens := fs.Int("max-completion-tokens", 64, "maximum completion tokens")
	timeout := fs.Duration("timeout", 30*time.Second, "total request timeout")
	idleTimeout := fs.Duration("stream-idle-timeout", 5*time.Second, "maximum time without an SSE event")
	apiKeyEnv := fs.String("api-key-env", "", "environment variable containing the API key")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var apiKey string
	if *apiKeyEnv != "" {
		apiKey = os.Getenv(*apiKeyEnv)
		if apiKey == "" {
			return fmt.Errorf("environment variable %s is empty", *apiKeyEnv)
		}
	}

	result, err := probe.Run(ctx, probe.Config{
		Endpoint:            *endpoint,
		Model:               *model,
		Prompt:              *promptText,
		MaxCompletionTokens: *maxTokens,
		APIKey:              apiKey,
		Timeout:             *timeout,
		StreamIdleTimeout:   *idleTimeout,
	})
	if err != nil {
		return err
	}

	out := struct {
		StatusCode    int          `json:"status_code"`
		TTFBMS        float64      `json:"ttfb_ms"`
		TTFTMS        float64      `json:"ttft_ms"`
		ChunkITLMS    []float64    `json:"chunk_itl_ms"`
		ContentEvents int          `json:"content_events"`
		DurationMS    float64      `json:"duration_ms"`
		Usage         *probe.Usage `json:"usage,omitempty"`
	}{
		StatusCode:    result.StatusCode,
		TTFBMS:        milliseconds(result.TTFB),
		TTFTMS:        milliseconds(result.TTFT),
		ChunkITLMS:    durationsMS(result.ChunkITL),
		ContentEvents: result.ContentEvents,
		DurationMS:    milliseconds(result.Duration),
		Usage:         result.Usage,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func durationsMS(values []time.Duration) []float64 {
	result := make([]float64, len(values))
	for i, value := range values {
		result[i] = milliseconds(value)
	}
	return result
}
