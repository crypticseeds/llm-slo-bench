package mockserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandlerStreamsRoleContentUsageAndDone(t *testing.T) {
	cfg := Config{
		Profile:       Profile{Name: "test", FirstTokenDelay: time.Millisecond, ChunkDelay: time.Millisecond},
		ChunkCount:    2,
		Fault:         FaultNone,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	server := httptest.NewServer(NewHandler(cfg))
	defer server.Close()

	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"mock-model","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	text := string(body)
	for _, want := range []string{`"role":"assistant"`, `"content":"chunk-1 "`, `"content":"chunk-2 "`, `"completion_tokens":2`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q:\n%s", want, text)
		}
	}
}

func TestConfigRejectsUnknownFault(t *testing.T) {
	cfg := Config{
		Address:       "127.0.0.1:8080",
		Profile:       Profile{Name: "test"},
		ChunkCount:    1,
		Fault:         Fault("surprise"),
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil for an unknown fault")
	}
}

func TestFaultEveryAppliesOnlyToNthRequest(t *testing.T) {
	cfg := Config{
		Profile:       Profile{Name: "test"},
		ChunkCount:    1,
		Fault:         FaultHTTPError,
		FaultEvery:    2,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	server := httptest.NewServer(NewHandler(cfg))
	defer server.Close()

	for i, wantStatus := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusOK, http.StatusServiceUnavailable} {
		response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"mock-model","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != wantStatus {
			t.Fatalf("request %d status = %d, want %d", i+1, response.StatusCode, wantStatus)
		}
	}
}

func TestConfigRejectsFaultAfterPastChunks(t *testing.T) {
	cfg := Config{
		Address:       "127.0.0.1:8080",
		Profile:       Profile{Name: "test"},
		ChunkCount:    1,
		Fault:         FaultDisconnect,
		FaultEvery:    1,
		FaultAfter:    2,
		StallDuration: time.Second,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() returned nil for an unreachable fault position")
	}
}

func TestConfigRejectsInvalidFaultBounds(t *testing.T) {
	valid := Config{
		Address:       "127.0.0.1:8080",
		Profile:       Profile{Name: "test"},
		ChunkCount:    2,
		Fault:         FaultStall,
		FaultEvery:    1,
		FaultAfter:    1,
		StallDuration: time.Second,
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "zero fault cadence", mutate: func(c *Config) { c.FaultEvery = 0 }},
		{name: "zero fault position", mutate: func(c *Config) { c.FaultAfter = 0 }},
		{name: "zero stall duration", mutate: func(c *Config) { c.StallDuration = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() returned nil for invalid fault bounds")
			}
		})
	}
}
