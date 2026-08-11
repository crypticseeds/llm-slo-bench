// Package report renders metrics summaries as machine-readable JSON and
// self-contained HTML.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/metrics"
)

// Metadata contains optional run context that is not part of metrics.RunSummary.
// Empty fields are rendered as "not provided" rather than inferred.
type Metadata struct {
	RunID             string
	Scenario          string
	Target            string
	Model             string
	StartedAt         time.Time
	Duration          time.Duration
	ConfigFingerprint string
}

// HTMLOptions controls optional presentation data.
type HTMLOptions struct {
	Metadata         Metadata
	RequestJSONLPath string
}

// WriteJSON writes the canonical, indented JSON representation of summary.
func WriteJSON(writer io.Writer, summary metrics.RunSummary) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(summary); err != nil {
		return fmt.Errorf("write summary JSON: %w", err)
	}
	return nil
}
