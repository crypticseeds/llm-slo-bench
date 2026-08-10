package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const validYAML = `
version: 1
target:
  base_url: http://127.0.0.1:8080
  model: mock-model
request:
  prompt: hello
  max_completion_tokens: 32
  timeout: 30s
  stream_idle_timeout: 5s
load:
  max_in_flight: 4
  stages:
    - duration: 10s
      target_rps: 2
slo:
  p99_ttft_ms: 800
  max_error_rate: 0.01
safety:
  max_requests: 100
  max_duration: 20s
  max_cost_usd: 1
  reserve_cost_per_request_usd: 0.01
pricing:
  input_usd_per_million_tokens: 0.15
  output_usd_per_million_tokens: 0.60
output:
  json: artifacts/run.json
  html: artifacts/run.html
  raw_jsonl: ""
`

func TestLoadReaderParsesStrictYAML(t *testing.T) {
	cfg, err := LoadReader(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 || cfg.Target.Model != "mock-model" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Request.Timeout.String() != "30s" {
		t.Fatalf("Timeout = %s, want 30s", cfg.Request.Timeout)
	}
}

func TestLoadReaderRejectsUnknownField(t *testing.T) {
	input := strings.Replace(validYAML, "  model: mock-model", "  model: mock-model\n  surprise: true", 1)
	_, err := LoadReader(strings.NewReader(input))
	if err == nil || !strings.Contains(err.Error(), "field surprise not found") {
		t.Fatalf("LoadReader() error = %v, want unknown field error", err)
	}
}

func TestValidateContractForFemi(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid loopback"},
		{name: "unsupported version", mutate: func(c *Config) { c.Version = 2 }, wantErr: "version"},
		{name: "empty base URL", mutate: func(c *Config) { c.Target.BaseURL = "" }, wantErr: "base_url"},
		{name: "empty model", mutate: func(c *Config) { c.Target.Model = "" }, wantErr: "model"},
		{name: "empty prompt", mutate: func(c *Config) { c.Request.Prompt = "" }, wantErr: "prompt"},
		{name: "non-positive output bound", mutate: func(c *Config) { c.Request.MaxCompletionTokens = 0 }, wantErr: "max_completion_tokens"},
		{name: "non-positive request timeout", mutate: func(c *Config) { c.Request.Timeout.Duration = 0 }, wantErr: "timeout"},
		{name: "non-positive idle timeout", mutate: func(c *Config) { c.Request.StreamIdleTimeout.Duration = 0 }, wantErr: "stream_idle_timeout"},
		{name: "non-positive concurrency", mutate: func(c *Config) { c.Load.MaxInFlight = 0 }, wantErr: "max_in_flight"},
		{name: "empty stages", mutate: func(c *Config) { c.Load.Stages = nil }, wantErr: "stages"},
		{name: "non-positive stage duration", mutate: func(c *Config) { c.Load.Stages[0].Duration.Duration = 0 }, wantErr: "stages"},
		{name: "non-positive stage rate", mutate: func(c *Config) { c.Load.Stages[0].TargetRPS = 0 }, wantErr: "target_rps"},
		{name: "invalid error rate", mutate: func(c *Config) { value := 1.1; c.SLO.MaxErrorRate = &value }, wantErr: "max_error_rate"},
		{name: "invalid dropped rate", mutate: func(c *Config) { value := -0.1; c.SLO.MaxDroppedRate = &value }, wantErr: "max_dropped_rate"},
		{name: "negative TTFT ceiling", mutate: func(c *Config) { value := -1.0; c.SLO.P99TTFTMS = &value }, wantErr: "p99_ttft_ms"},
		{name: "negative cost ceiling", mutate: func(c *Config) { value := -1.0; c.SLO.MaxCostUSD = &value }, wantErr: "max_cost_usd"},
		{name: "negative price", mutate: func(c *Config) { c.Pricing.InputUSDPerMillionTokens = -1 }, wantErr: "input_usd_per_million_tokens"},
		{name: "non-positive max requests", mutate: func(c *Config) { c.Safety.MaxRequests = 0 }, wantErr: "max_requests"},
		{name: "non-positive safety cost", mutate: func(c *Config) { c.Safety.MaxCostUSD = 0 }, wantErr: "max_cost_usd"},
		{name: "non-positive reserve cost", mutate: func(c *Config) { c.Safety.ReserveCostPerRequestUSD = 0 }, wantErr: "reserve_cost_per_request_usd"},
		{name: "remote target needs key env", mutate: func(c *Config) { c.Target.BaseURL = "https://api.openai.com" }, wantErr: "api_key_env"},
		{name: "max duration covers stages", mutate: func(c *Config) { c.Safety.MaxDuration.Duration = 5 * time.Second }, wantErr: "max_duration"},
	}

	base, err := decodeWithoutValidation(validYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Load.Stages = append([]Stage(nil), base.Load.Stages...)
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			err := cfg.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want field %q", err, test.wantErr)
			}
		})
	}
}

func decodeWithoutValidation(input string) (Config, error) {
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(input))
	decoder.KnownFields(true)
	err := decoder.Decode(&cfg)
	return cfg, err
}
