package loadgen_test

import (
	"fmt"
	"time"

	"github.com/crypticseeds/llm-slo-bench/internal/config"
	"github.com/crypticseeds/llm-slo-bench/internal/loadgen"
)

func ExampleSchedule() {
	offsets, err := loadgen.Schedule([]config.Stage{{
		Duration:  config.Duration{Duration: 2 * time.Second},
		TargetRPS: 2,
	}})
	if err != nil {
		panic(err)
	}
	for _, offset := range offsets {
		fmt.Printf("%.3fs\n", offset.Seconds())
	}
	// Output:
	// 1.414s
	// 2.000s
}
