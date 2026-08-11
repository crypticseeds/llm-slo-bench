# llm-slo-bench

Run the deterministic Day 2 ramp gate without an API key or external service:

```sh
go test -run TestRunRampAgainstBuiltInMockProducesTTFTHistogram -v ./cmd/llm-slo-bench
```

The test starts the built-in mock in-process, runs `ramp --config`, and prints the resulting TTFT sample count and p99.
