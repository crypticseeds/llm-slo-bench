# llm-slo-bench

[![CI](https://github.com/crypticseeds/llm-slo-bench/actions/workflows/ci.yml/badge.svg)](https://github.com/crypticseeds/llm-slo-bench/actions/workflows/ci.yml)

A single-binary Go load generator and SLO gate for OpenAI-compatible LLM endpoints. It opens real streaming requests under open-loop concurrent load, parses the SSE wire itself, and measures the numbers that actually describe a user's experience of an inference endpoint: time-to-first-token, inter-chunk latency, request throughput, and cost from server-reported usage. Those measurements are then compared against SLOs you declare in YAML (`p99_ttft_ms: 800`), and the process exits non-zero when a gate fails, so the same command works as a local investigation tool and as a CI merge gate. It ships with a deterministic mock SSE server in the same binary, so you can run the full pipeline with no API key and no spend.

Generic HTTP load tools read the whole response body before returning, which makes TTFT unmeasurable. LLM benchmark suites measure TTFT but produce a report for a human to read, not a process status a pipeline can trust. This does both, out of one static binary.

## Quickstart

Three commands, no API key, no external service:

```sh
git clone https://github.com/crypticseeds/llm-slo-bench.git
cd llm-slo-bench
make demo-html
```

`make demo-html` builds the binary, starts the built-in mock on `127.0.0.1:8080`, waits for it to answer a `probe`, runs a ramp against it using [`examples/quickstart.yaml`](examples/quickstart.yaml), and prints the path to a self-contained HTML report. It also writes `artifacts/demo.json`, the canonical run summary.

Requirements: Go 1.26 or newer (see [`go.mod`](go.mod)), a free TCP port 8080, and `python3` (used only for the port check in the Makefile).

To drive the pieces yourself:

```sh
# terminal 1 - deterministic streaming endpoint
./bin/llm-slo-bench mock --profile fast

# terminal 2 - one request, semantic timings as JSON
./bin/llm-slo-bench probe

# terminal 2 - full ramp with SLO gate, JSON and HTML artifacts
./bin/llm-slo-bench ramp --config examples/quickstart.yaml \
  --out artifacts/run.json --html artifacts/run.html
echo "exit: $?"
```

### Subcommands

| Command | What it does |
| --- | --- |
| `mock` | Runs a deterministic OpenAI-compatible SSE server in-process. Flags: `--listen`, `--profile` (`fast`/`steady`/`slow`), `--first-token-delay`, `--chunk-delay`, `--chunks`, `--fault` (`none`/`http-error`/`malformed`/`disconnect`/`stall`), `--fault-every`, `--fault-after`, `--stall-duration`. |
| `probe` | Sends one streaming request and prints TTFB, semantic TTFT, per-chunk ITL, content event count, duration, and usage as JSON. Flags: `--endpoint`, `--model`, `--prompt`, `--max-completion-tokens`, `--timeout`, `--stream-idle-timeout`, `--api-key-env`. |
| `ramp` | Runs the open-loop ramp scenario from a YAML config, aggregates metrics, evaluates SLOs, and sets the exit code. Flags: `--config` (required), `--out`, `--html`, `--raw-jsonl`. |
| `report` | Re-renders an HTML report from an existing summary JSON without re-running load. Flags: `--input` (required), `--html` (required), `--raw-jsonl`. |

`ramp` writes summary JSON to stdout when `--out` is omitted. All file artifacts are written atomically (temp file in the destination directory, fsync, rename), so a killed run never leaves a half-written report. `--config`, `--out`, `--html`, and `--raw-jsonl` must all resolve to different paths.

## Configuration

YAML decoding is strict: unknown keys are rejected, multiple YAML documents are rejected, and all durations are strings parsed by Go's `time.ParseDuration` (`5s`, `2m`, `500ms`). This is the complete v1 schema, matching [`examples/quickstart.yaml`](examples/quickstart.yaml).

```yaml
version: 1                                  # int, must be 1

target:
  base_url: http://127.0.0.1:8080/v1        # string; "/chat/completions" is appended
  model: mock-model                         # string
  api_key_env: OPENAI_API_KEY               # string; NAME of an env var, never the key itself
                                            # optional for loopback targets, required otherwise

request:
  prompt: Explain why p99 latency matters.  # string
  max_completion_tokens: 16                 # int
  timeout: 5s                               # duration; total per-request budget
  stream_idle_timeout: 2s                   # duration; max gap between SSE events

load:
  max_in_flight: 4                          # int; hard ceiling on concurrent requests
  stages:                                   # list; piecewise ramp, executed in order
    - duration: 1s                          # duration
      target_rps: 4                         # float; arrivals per second at end of stage
    - duration: 1s
      target_rps: 8

slo:                                        # every key is optional; omit to skip that gate
  p99_ttft_ms: 250                          # float; p99 semantic TTFT in milliseconds
  p99_chunk_itl_ms: 120                     # float; p99 inter-chunk latency in milliseconds
  max_error_rate: 0.01                      # float in [0,1]; failures / scheduled arrivals
  max_dropped_rate: 0.00                    # float in [0,1]; drops / scheduled arrivals
  max_cost_usd: 0.01                        # float; total run cost from server usage

safety:                                     # hard ceilings; the run stops before breaching them
  max_requests: 20                          # int
  max_duration: 5s                          # duration; must be >= sum of all stage durations
  max_cost_usd: 0.10                        # float; spend ceiling for the whole run
  reserve_cost_per_request_usd: 0.001        # float; worst-case cost reserved before admission

pricing:                                    # used to compute cost from server-reported usage
  input_usd_per_million_tokens: 0           # float
  output_usd_per_million_tokens: 0          # float

output:                                     # accepted for forward compatibility
  json: ""                                  # currently IGNORED - use --out
  html: ""                                  # currently IGNORED - use --html
  raw_jsonl: ""                             # currently IGNORED - use --raw-jsonl
```

Two things that look redundant but are not. `slo.max_cost_usd` is a verdict: "the run finished and it cost too much, fail the build". `safety.max_cost_usd` is a brake: "stop admitting requests before spend reaches this". Set both if you want both behaviours.

API keys are read from the environment variable named by `api_key_env` and are never written to YAML, JSON, JSONL, HTML, or logs.

## How the measurements are defined

This is the part that determines whether the numbers mean anything. Fuller detail is in [`docs/MEASUREMENT.md`](docs/MEASUREMENT.md).

**Semantic TTFT, not TTFB.** Time-to-first-byte is when the transport delivers the first byte of the response body. On a streaming LLM endpoint that byte is frequently a role-only chunk, an empty-content chunk, or a keepalive comment - nothing a user would call a token. TTFT here is measured from dispatch (stamped immediately before `http.Client.Do`, so client-side request construction is excluded) to the arrival of the first SSE event carrying non-empty `choices[].delta.content`. TTFB is still recorded, but only as a transport diagnostic. It is never gated. The two can differ by tens of milliseconds, and gating the wrong one produces an SLO that passes while users wait.

All durations derive from `time.Now()` values that retain Go's monotonic clock component, so an NTP step mid-run cannot skew a percentile.

**`chunk_itl`, not "inter-token latency".** An SSE event may contain zero, one, or many model tokens; nothing in the protocol guarantees one token per chunk. Calling the interval between chunks "inter-token latency" is a claim the wire does not support. The metric is therefore named `chunk_itl_ms` throughout the schema, the config, and the reports: the arrival interval between consecutive non-empty content events. If you need true per-token latency, divide the request duration by the server-reported completion token count.

**Open-loop arrivals with absolute deadlines.** Every scheduled arrival gets an absolute deadline computed from the run's monotonic start, not from "now plus an interval" after the previous request. That is the difference between measuring the system and measuring yourself. In a closed-loop generator, a slow server delays the next request, offered load silently collapses, and the latencies you record exclude exactly the requests that would have been slowest - coordinated omission. Here, when a deadline arrives and the in-flight ceiling is saturated, the arrival is **dropped and counted**, never queued. Drops surface as `counts.dropped` and `counts.dropped_rate`, and can be gated with `max_dropped_rate`. A high drop rate is a real signal that the target could not absorb the offered load; hiding it behind a client-side queue would turn a capacity failure into a latency graph that looks fine.

**Cost from server usage only.** Token counts come exclusively from the final usage object the server reports. There is no bundled tokenizer and no estimation from chunk counts. `cost = prompt_tokens * input_price + completion_tokens * output_price`, using the prices you declare under `pricing`. If a stream is interrupted before its usage chunk arrives, that request contributes no tokens, `usage.complete` is `false`, and `usage.cost_usd` reflects only the requests that did report. An unknown number is reported as unknown rather than guessed.

**Failures.** A stream that produces content and then dies - malformed JSON, premature EOF, idle timeout, disconnect - is a failed request. It counts against the error budget and contributes no latency or usage samples. Partial success is not success.

## CI gate

`ramp` is designed to be the last step of a pipeline. It prints every configured gate with observed value, operator, threshold, and sample count, then returns a status code.

```
slo: pass p99_ttft_ms observed=41.2 threshold=250 samples=12
slo: fail p99_chunk_itl_ms observed=138 threshold=120 samples=48
```

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Run completed and every configured SLO passed. |
| `1` | Run completed and at least one SLO failed. All gates are evaluated and reported before exiting. |
| `2` | Config or CLI error: unparseable YAML, unknown key, missing `--config`, empty API key env var, conflicting output paths. Nothing ran. |
| `3` | Invalid execution: the load generator, aggregator, or a report writer failed, or the run produced no usable TTFT samples. The result is not a verdict, so it is not reported as one. |
| `130` | Interrupted by SIGINT or SIGTERM. |

Exit `3` is deliberately distinct from exit `1`. A run that could not measure anything is not a passing run and is not a failing run - it is a broken run, and a pipeline should treat it differently from a genuine regression.

### GitHub Actions

```yaml
name: llm-slo

on: [push, pull_request]

jobs:
  slo-gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: Build
        run: go build -o bin/llm-slo-bench ./cmd/llm-slo-bench

      - name: Start mock endpoint
        run: |
          ./bin/llm-slo-bench mock --profile fast &
          for _ in $(seq 1 20); do
            ./bin/llm-slo-bench probe --timeout 500ms >/dev/null 2>&1 && exit 0
            sleep 0.2
          done
          echo "mock did not become ready" >&2
          exit 1

      - name: Run SLO gate
        run: |
          mkdir -p artifacts
          ./bin/llm-slo-bench ramp \
            --config examples/quickstart.yaml \
            --out artifacts/run.json \
            --html artifacts/run.html

      - name: Upload evidence
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: slo-report
          path: artifacts/
```

Point `target.base_url` at a real endpoint and set `api_key_env` to gate a live service instead of the mock. Keep `safety.max_requests`, `safety.max_duration`, and `safety.max_cost_usd` tight when you do: they are enforced before admission, so a runaway config cannot spend beyond the ceiling. Upload the artifacts unconditionally - the HTML report is most useful precisely on the runs that failed.

## Measured results

<!--
  COORDINATOR: fill from the joint gateway run. Replace each {{TOKEN}} with the
  measured value and delete this comment. Do not populate from a local mock run.
-->

> **TBD - not yet measured.** The placeholders below are filled from a joint run against
> [`sre-inference-gateway`](https://github.com/crypticseeds/sre-inference-gateway) and are unpopulated
> until that run completes. No number in this section is estimated, extrapolated, or taken from a mock.

| Metric | Value | Source |
| --- | --- | --- |
| p99 TTFT | `{{P99_TTFT_MS}}` ms | ramp scenario, gateway target |
| Request throughput | `{{THROUGHPUT_RPS}}` req/s | same run, sustained stage |
| Failover recovery | `{{FAILOVER_RECOVERY_S}}` s | provider failure to first successful stream |

## Limitations

Read this before trusting a number.

- **Scenarios: ramp only.** Burst and the gateway failover drill are not implemented in this release. The `ramp` scenario is the whole load model today.
- **The measurement is client-side.** Everything is observed from the load generator's perspective over the network. Network jitter, TLS handshakes, and any proxy between you and the model are inside the numbers. Run from a host that resembles where your users are, and say where you ran it.
- **The SLO gate is not yet armed.** Gates are collected, printed, and written to the summary JSON with their observed values, thresholds, and sample counts, but the comparison itself is still being implemented: outcomes currently report `status: pending` and a run cannot yet exit `1`. The measurement pipeline below the gate is complete.
- **Config validation is not yet fully enforced.** Strict YAML decoding rejects unknown keys and bad types, but the documented cross-field invariants (positivity, ranges, `max_duration` vs stage sum, `api_key_env` required for non-loopback targets) are still being implemented. Invalid-but-parseable configs may currently run.
- **The `output:` config block is ignored.** Artifact paths come from CLI flags only. The keys are accepted so configs stay forward-compatible.
- **Token counts depend entirely on the server.** An endpoint that does not emit a final usage object yields no token throughput and no cost, and cannot pass a configured `max_cost_usd` gate. That is by design, but it means cost gating does not work everywhere.
- **Percentiles need samples.** A p99 over a handful of requests is a description of those requests, not of the service. The sample count is printed next to every gate; look at it.
- **Single process, single host.** No distributed load generation. Offered load is bounded by what one machine and one `http.Transport` can sustain, and at high rates the generator itself can become the bottleneck. Drop counts are the first place that shows up.
- **OpenAI-compatible Chat Completions only.** One HTTP adapter, parameterized by base URL, model, and headers. No provider SDKs, no plugin system, no Responses API.
- **Not a correctness benchmark.** This measures latency, throughput, failure behaviour, and cost. It says nothing about output quality.

Prior art worth knowing: [llmperf](https://github.com/ray-project/llmperf) (archived) and [GenAI-Perf](https://github.com/triton-inference-server/perf_analyzer) both measure LLM streaming latency and predate this. The differences here are a single static Go binary with no runtime stack, open-loop arrivals with explicit drop accounting, and SLO evaluation that ends in a process exit code rather than a report.

## License

MIT. See [LICENSE](LICENSE).
