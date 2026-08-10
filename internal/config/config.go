package config

import (
	"errors"
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version int     `yaml:"version"`
	Target  Target  `yaml:"target"`
	Request Request `yaml:"request"`
	Load    Load    `yaml:"load"`
	SLO     SLO     `yaml:"slo"`
	Safety  Safety  `yaml:"safety"`
	Pricing Pricing `yaml:"pricing"`
	Output  Output  `yaml:"output"`
}

type Target struct {
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
}

type Request struct {
	Prompt              string   `yaml:"prompt"`
	MaxCompletionTokens int      `yaml:"max_completion_tokens"`
	Timeout             Duration `yaml:"timeout"`
	StreamIdleTimeout   Duration `yaml:"stream_idle_timeout"`
}

type Load struct {
	MaxInFlight int     `yaml:"max_in_flight"`
	Stages      []Stage `yaml:"stages"`
}

type Stage struct {
	Duration  Duration `yaml:"duration"`
	TargetRPS float64  `yaml:"target_rps"`
}

type SLO struct {
	P99TTFTMS      *float64 `yaml:"p99_ttft_ms"`
	P99ChunkITLMS  *float64 `yaml:"p99_chunk_itl_ms"`
	MaxErrorRate   *float64 `yaml:"max_error_rate"`
	MaxDroppedRate *float64 `yaml:"max_dropped_rate"`
	MaxCostUSD     *float64 `yaml:"max_cost_usd"`
}

type Safety struct {
	MaxRequests              int      `yaml:"max_requests"`
	MaxDuration              Duration `yaml:"max_duration"`
	MaxCostUSD               float64  `yaml:"max_cost_usd"`
	ReserveCostPerRequestUSD float64  `yaml:"reserve_cost_per_request_usd"`
}

type Pricing struct {
	InputUSDPerMillionTokens  float64 `yaml:"input_usd_per_million_tokens"`
	OutputUSDPerMillionTokens float64 `yaml:"output_usd_per_million_tokens"`
}

type Output struct {
	JSON     string `yaml:"json"`
	HTML     string `yaml:"html"`
	RawJSONL string `yaml:"raw_jsonl"`
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var value string
	if err := node.Decode(&value); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", value, err)
	}
	d.Duration = duration
	return nil
}

func LoadReader(reader io.Reader) (Config, error) {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode config: multiple YAML documents are not allowed")
		}
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

// Validate enforces the v1 config's cross-field invariants.
//
// Positivity and ceiling checks apply to every target. Only api_key_env is
// conditional: a loopback target may leave it empty, while a non-loopback
// target must name an API key environment variable. Validate rejects unsupported
// versions, empty target/model/prompt values, non-positive request, duration,
// output-token, concurrency, stage, and cost safety values, rate SLOs outside
// [0,1], negative p99_ttft_ms or slo.max_cost_usd values, negative prices, and
// a safety max_duration shorter than the sum of all load stages.
//
// Examples: http://127.0.0.1:8080 may omit api_key_env; https://api.openai.com
// may not. Two 30s stages require max_duration >= 1m.
func (c Config) Validate() error {
	// TODO(Femi): implement the contract above. Keep this pure and return
	// descriptive errors that name the invalid field.
	return nil
}
