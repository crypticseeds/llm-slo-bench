package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"
)

type Config struct {
	Endpoint            string
	Model               string
	Prompt              string
	MaxCompletionTokens int
	APIKey              string
	Timeout             time.Duration
	StreamIdleTimeout   time.Duration
	HTTPClient          *http.Client
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Result struct {
	StatusCode    int
	TTFB          time.Duration
	TTFT          time.Duration
	ChunkITL      []time.Duration
	ContentEvents int
	Duration      time.Duration
	Usage         *Usage
}

func Run(parent context.Context, cfg Config) (Result, error) {
	if err := validateConfig(cfg); err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()

	payload, err := json.Marshal(map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "user", "content": cfg.Prompt},
		},
		"max_completion_tokens": cfg.MaxCompletionTokens,
		"stream":                true,
		"stream_options": map[string]bool{
			"include_usage": true,
		},
	})
	if err != nil {
		return Result{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	if cfg.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	firstByte := make(chan time.Time, 1)
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			select {
			case firstByte <- time.Now():
			default:
			}
		},
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{DisableCompression: true}}
	}
	// time.Now carries a monotonic component. Stamping immediately before Do
	// keeps DNS, connect, TLS, and server wait inside both diagnostic TTFB and
	// semantic TTFT without exposure to wall-clock adjustments.
	start := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("send request: %w", err)
	}
	defer response.Body.Close()

	result := Result{StatusCode: response.StatusCode}
	select {
	case at := <-firstByte:
		result.TTFB = at.Sub(start)
	default:
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return result, fmt.Errorf("endpoint returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	contentType := response.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return result, fmt.Errorf("endpoint returned Content-Type %q, want text/event-stream", contentType)
	}

	idleExpired := make(chan struct{}, 1)
	idleTimer := time.AfterFunc(cfg.StreamIdleTimeout, func() {
		select {
		case idleExpired <- struct{}{}:
		default:
		}
		cancel()
	})
	defer idleTimer.Stop()

	decoder := newEventDecoder(response.Body)
	var previousContent time.Time
	for {
		event, err := decoder.Next()
		if err != nil {
			select {
			case <-idleExpired:
				return result, fmt.Errorf("stream idle timeout after %s", cfg.StreamIdleTimeout)
			default:
			}
			if errors.Is(err, io.EOF) {
				return result, errors.New("stream ended before [DONE]")
			}
			return result, fmt.Errorf("read SSE stream: %w", err)
		}
		idleTimer.Reset(cfg.StreamIdleTimeout)
		now := time.Now()
		if event.Data == "[DONE]" {
			result.Duration = now.Sub(start)
			if result.ContentEvents == 0 {
				return result, errors.New("stream completed without a non-empty content event")
			}
			return result, nil
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
			return result, fmt.Errorf("decode OpenAI stream event: %w", err)
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			// OpenAI streams role and usage events that are not generated content.
			// Only non-empty content starts semantic TTFT and contributes chunk ITL.
			result.ContentEvents++
			if previousContent.IsZero() {
				result.TTFT = now.Sub(start)
			} else {
				result.ChunkITL = append(result.ChunkITL, now.Sub(previousContent))
			}
			previousContent = now
		}
	}
}

func validateConfig(cfg Config) error {
	if cfg.Endpoint == "" || cfg.Model == "" || cfg.Prompt == "" {
		return errors.New("endpoint, model, and prompt are required")
	}
	if cfg.MaxCompletionTokens < 1 {
		return errors.New("max completion tokens must be positive")
	}
	if cfg.Timeout <= 0 || cfg.StreamIdleTimeout <= 0 {
		return errors.New("timeouts must be positive")
	}
	return nil
}

type event struct {
	Data string
}

type eventDecoder struct {
	reader     *bufio.Reader
	skipNextLF bool
}

func newEventDecoder(reader io.Reader) *eventDecoder {
	return &eventDecoder{reader: bufio.NewReader(reader)}
}

// Next follows SSE framing rather than HTTP read boundaries: blank lines
// dispatch events, comments are ignored, and repeated data fields are joined.
func (d *eventDecoder) Next() (event, error) {
	var data []string
	for {
		line, err := d.readLine()
		if err != nil {
			return event{}, err
		}
		if line == "" {
			if len(data) == 0 {
				continue
			}
			return event{Data: strings.Join(data, "\n")}, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		if field == "data" {
			data = append(data, value)
		}
	}
}

func (d *eventDecoder) readLine() (string, error) {
	var line []byte
	for {
		value, err := d.reader.ReadByte()
		if err != nil {
			return "", err
		}
		if d.skipNextLF {
			d.skipNextLF = false
			if value == '\n' {
				continue
			}
		}
		switch value {
		case '\n':
			return string(line), nil
		case '\r':
			// Return immediately for a valid bare CR. If this was CRLF, the next
			// call skips the LF without blocking this event's dispatch.
			d.skipNextLF = true
			return string(line), nil
		default:
			line = append(line, value)
		}
	}
}
