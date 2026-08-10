package mockserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type Fault string

const (
	FaultNone       Fault = "none"
	FaultHTTPError  Fault = "http-error"
	FaultMalformed  Fault = "malformed"
	FaultDisconnect Fault = "disconnect"
	FaultStall      Fault = "stall"
)

type Profile struct {
	Name            string
	FirstTokenDelay time.Duration
	ChunkDelay      time.Duration
}

type Config struct {
	Address       string
	Profile       Profile
	ChunkCount    int
	Fault         Fault
	FaultEvery    int
	FaultAfter    int
	StallDuration time.Duration
}

var profiles = map[string]Profile{
	"fast":   {Name: "fast", FirstTokenDelay: 10 * time.Millisecond, ChunkDelay: 5 * time.Millisecond},
	"steady": {Name: "steady", FirstTokenDelay: 100 * time.Millisecond, ChunkDelay: 40 * time.Millisecond},
	"slow":   {Name: "slow", FirstTokenDelay: 500 * time.Millisecond, ChunkDelay: 150 * time.Millisecond},
}

func LookupProfile(name string) (Profile, error) {
	profile, ok := profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown latency profile %q", name)
	}
	return profile, nil
}

func (c Config) Validate() error {
	if c.Address == "" {
		return errors.New("listen address must not be empty")
	}
	if c.Profile.FirstTokenDelay < 0 || c.Profile.ChunkDelay < 0 {
		return errors.New("latency values must not be negative")
	}
	if c.ChunkCount < 1 {
		return errors.New("chunks must be at least 1")
	}
	if c.FaultEvery < 1 {
		return errors.New("fault-every must be at least 1")
	}
	switch c.Fault {
	case FaultNone, FaultHTTPError, FaultMalformed, FaultDisconnect, FaultStall:
	default:
		return fmt.Errorf("unknown fault mode %q", c.Fault)
	}
	if c.Fault == FaultStall && c.StallDuration <= 0 {
		return errors.New("stall-duration must be positive")
	}
	if c.Fault == FaultMalformed || c.Fault == FaultDisconnect || c.Fault == FaultStall {
		if c.FaultAfter < 1 || c.FaultAfter > c.ChunkCount {
			return errors.New("fault-after must select an emitted content event")
		}
	}
	return nil
}

func Serve(ctx context.Context, cfg Config) error {
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           NewHandler(cfg),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The server runs in one goroutine while the caller waits on either its
	// result channel or cancellation. This keeps shutdown deterministic.
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func NewHandler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	var requestNumber atomic.Uint64
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		applyFault := requestNumber.Add(1)%uint64(cfg.FaultEvery) == 0
		handleChatCompletion(w, r, cfg, applyFault)
	})
	return mux
}

func handleChatCompletion(w http.ResponseWriter, r *http.Request, cfg Config, applyFault bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if applyFault && cfg.Fault == FaultHTTPError {
		http.Error(w, "configured mock error", http.StatusServiceUnavailable)
		return
	}

	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)
		return
	}
	if !request.Stream {
		http.Error(w, "mock requires stream=true", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeJSONEvent(w, map[string]any{
		"id": "mock-completion",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant"},
		}},
	})
	flusher.Flush()

	if !sleep(r.Context(), cfg.Profile.FirstTokenDelay) {
		return
	}

	for i := 1; i <= cfg.ChunkCount; i++ {
		writeJSONEvent(w, map[string]any{
			"id": "mock-completion",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": fmt.Sprintf("chunk-%d ", i)},
			}},
		})
		flusher.Flush()

		if applyFault && i == cfg.FaultAfter {
			switch cfg.Fault {
			case FaultMalformed:
				fmt.Fprint(w, "data: {not-json}\n\n")
				flusher.Flush()
				return
			case FaultDisconnect:
				return
			case FaultStall:
				if !sleep(r.Context(), cfg.StallDuration) {
					return
				}
			}
		}

		if i < cfg.ChunkCount && !sleep(r.Context(), cfg.Profile.ChunkDelay) {
			return
		}
	}

	writeJSONEvent(w, map[string]any{
		"id":      "mock-completion",
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     8,
			"completion_tokens": cfg.ChunkCount,
			"total_tokens":      8 + cfg.ChunkCount,
		},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeJSONEvent(w http.ResponseWriter, value any) {
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
