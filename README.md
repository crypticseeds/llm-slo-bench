# llm-slo-bench

[![CI](https://github.com/crypticseeds/llm-slo-bench/actions/workflows/ci.yml/badge.svg)](https://github.com/crypticseeds/llm-slo-bench/actions/workflows/ci.yml)

A single-binary Go load generator and SLO gate for OpenAI-compatible LLM endpoints. It opens real streaming requests under open-loop concurrent load, parses the SSE wire itself, and measures the numbers that actually describe a user's experience of an inference endpoint: time-to-first-token, inter-chunk latency, request outcomes, token throughput, and cost from server-reported usage. Those measurements are then compared against SLOs you declare in YAML (`p99_ttft_ms: 800`), and the process exits non-zero when a gate fails, so the same command works as a local investigation tool and as a CI merge gate. It ships with a deterministic mock SSE server in the same binary, so you can run the full pipeline with no API key and no spend.

Generic HTTP load tools read the whole response body before returning, which makes TTFT unmeasurable. LLM benchmark suites measure TTFT but produce a report for a human to read, not a process status a pipeline can trust. This does both, out of one static binary.

## Quickstart

Three commands, no API key, no external service:

```sh
git clone https://github.com/crypticseeds/llm-slo-bench.git
cd llm-slo-bench
make demo-html
```

`make demo-html` builds the binary, starts the built-in mock on `127.0.0.1:8080`, waits for it to answer a `probe`, runs a ramp against it using [`examples/quickstart.yaml`](examples/quickstart.yaml), and prints the path to a self-contained HTML report. It also writes `artifacts/demo.json`, the canonical run summary.

Requirements: Go 1.26.2 or newer (see [`go.mod`](go.mod)), a free TCP port 8080, and `python3` (used only for the port check in the Makefile).

For tagged releases, download the archive for your OS and architecture from [GitHub Releases](https://github.com/crypticseeds/llm-slo-bench/releases). Release configuration builds the static `llm-slo-bench` binary for macOS and Linux on amd64 and arm64. To build locally, run `make build`; contributor checks are `make test` and `make race`. A minimal runtime container can be built with `docker build -t llm-slo-bench .`.

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

`ramp` writes summary JSON to stdout when `--out` is omitted. Summary JSON and HTML files are written atomically (temp file in the destination directory, fsync, rename), so a killed run never leaves a half-written report. Raw JSONL is appended directly so completed request records survive an interrupted run. `--config`, `--out`, `--html`, and `--raw-jsonl` must all resolve to different paths. When `--html` and `--raw-jsonl` are used together, the JSONL path must be new or empty so the report contains only the current run.

## Configuration

YAML decoding is strict: unknown keys are rejected, multiple YAML documents are rejected, and all durations are strings parsed by Go's `time.ParseDuration` (`5s`, `2m`, `500ms`). This is the complete v1 schema; [`examples/quickstart.yaml`](examples/quickstart.yaml) is a runnable example using a subset of the optional SLO keys.

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

safety:                                     # admission limits; see reservation semantics below
  max_requests: 20                          # int
  max_duration: 5s                          # duration; must be >= sum of all stage durations
  max_cost_usd: 0.10                        # float; maximum reserved budget for admitted requests
  reserve_cost_per_request_usd: 0.001        # float; conservative cost reserved before admission

pricing:                                    # used to compute cost from server-reported usage
  input_usd_per_million_tokens: 0           # float
  output_usd_per_million_tokens: 0          # float

output:                                     # accepted for forward compatibility
  json: ""                                  # currently IGNORED - use --out
  html: ""                                  # currently IGNORED - use --html
  raw_jsonl: ""                             # currently IGNORED - use --raw-jsonl
```

Two things that look redundant but are not. `slo.max_cost_usd` is a verdict: "the run finished and its complete server-reported cost exceeded this value, fail the build". `safety.max_cost_usd` is reservation-based admission control: before starting a request, the runner reserves `reserve_cost_per_request_usd` and stops admitting requests when the next reservation would exceed the budget. Set the reservation conservatively at or above the worst expected request cost. If an admitted request costs more than its reservation, actual spend can overshoot `safety.max_cost_usd`; the breach is detected only after the server reports usage and the cost has already been incurred.

API keys are read from the environment variable named by `api_key_env` and are never written to YAML, JSON, JSONL, HTML, or logs.

## How the measurements are defined

This is the part that determines whether the numbers mean anything. Fuller detail is in [`docs/MEASUREMENT.md`](docs/MEASUREMENT.md).

**Semantic TTFT: the gate fires on the first real token.** TTFT is measured from dispatch (stamped immediately before `http.Client.Do`, so client-side request construction is excluded) to the arrival of the first SSE event carrying non-empty `choices[].delta.content` - the first thing a user would actually call a token. Time-to-first-byte is recorded too, as a transport diagnostic, but it is deliberately not the gated number: on a streaming LLM endpoint the first byte is frequently a role-only chunk, an empty-content chunk, or a keepalive comment, and the two can differ by tens of milliseconds. Gating the byte instead of the token produces an SLO that passes while users wait.

All durations derive from `time.Now()` values that retain Go's monotonic clock component, so an NTP step mid-run cannot skew a percentile.

**`chunk_itl`: named for what the wire actually delivers.** The metric is the arrival interval between consecutive non-empty content events, and it carries the name `chunk_itl_ms` throughout the schema, the config, and the reports. An SSE event may contain zero, one, or many model tokens - nothing in the protocol guarantees one token per chunk - so presenting chunk intervals as "inter-token latency", as many tools do, is a claim the wire does not support. True per-token arrival latency cannot be recovered from SSE chunks plus aggregate usage. The reported tokens-per-second metric is average generation throughput: completion tokens divided by request duration after TTFT.

**Open-loop arrivals: measures the system, not itself.** Every scheduled arrival gets an absolute deadline computed from the run's monotonic start, not from "now plus an interval" after the previous request. This is the defence against coordinated omission: in a closed-loop generator, a slow server delays the next request, offered load silently collapses, and the recorded latencies exclude exactly the requests that would have been slowest. Here, when a deadline arrives and the in-flight ceiling is saturated, the arrival is **dropped and counted**, never queued. Drops surface as `counts.dropped` and `counts.dropped_rate`, and can be gated with `max_dropped_rate`. A high drop rate is a real signal that the target could not absorb the offered load; hiding it behind a client-side queue would turn a capacity failure into a latency graph that looks fine.

**Cost from server-reported usage.** Token counts come exclusively from the final usage object the server reports - no bundled tokenizer, no estimation from chunk counts. `cost = (prompt_tokens * input_price + completion_tokens * output_price) / 1,000,000`, using the per-million-token prices you declare under `pricing`. Failed requests contribute no aggregate usage. `usage.complete` means every successful request reported usage; when it is false, `usage.cost_usd` reflects only the successful requests that did report. An unknown number is reported as unknown rather than guessed.

**Failure accounting protects the error budget.** A stream that produces content and then dies - malformed JSON, premature EOF, idle timeout, disconnect - counts as a failed request: it charges the error budget and contributes no latency or usage samples. Partial success is not success.

## CI gate

`ramp` is designed to be the last step of a pipeline. It prints every configured gate with observed value, operator, threshold, and sample count to stderr, writes the summary separately, then returns a status code.

```text
# Illustrative output; values depend on the target and run.
slo: pass p99_ttft_ms observed=41.2 threshold=250 samples=12
slo: fail p99_chunk_itl_ms observed=138 threshold=120 samples=48
```

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Run completed and every configured SLO passed. |
| `1` | Run completed and at least one SLO failed. All gates are evaluated and reported before exiting. |
| `2` | Config or CLI error: unparseable YAML, unknown key, missing `--config`, empty API key env var, conflicting output paths. Nothing ran. |
| `3` | Invalid execution: the load generator, aggregator, SLO evaluator, or a report writer failed, or the run produced no usable TTFT samples. The result is not a verdict, so it is not reported as one. |
| `130` | Interrupted by SIGINT or SIGTERM. |

Exit `3` is deliberately distinct from exit `1`. A run that could not measure anything is not a passing run and is not a failing run - it is a broken run, and a pipeline should treat it differently from a genuine regression.

### GitHub Actions

This repository's [CI workflow](.github/workflows/ci.yml) runs formatting, vet, build, and race-test checks. Add a job like the following when you want CI to exercise an endpoint and enforce its SLOs:

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

      - name: Run SLO gate
        run: |
          mkdir -p bin artifacts
          go build -o bin/llm-slo-bench ./cmd/llm-slo-bench
          ./bin/llm-slo-bench mock --profile fast &
          mock_pid=$!
          trap 'kill "$mock_pid" 2>/dev/null || true' EXIT
          for _ in $(seq 1 20); do
            if ./bin/llm-slo-bench probe --timeout 500ms >/dev/null 2>&1; then
              ready=1
              break
            fi
            sleep 0.2
          done
          if [ "${ready:-0}" -ne 1 ]; then
            echo "mock did not become ready" >&2
            exit 1
          fi
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

Point `target.base_url` at a real endpoint and set `api_key_env` to gate a live service instead of the mock. Keep `safety.max_requests` and `safety.max_duration` tight, and set `reserve_cost_per_request_usd` conservatively: cost admission is based on reservations, not a provider-side spend cap, so an underestimated request can overshoot `safety.max_cost_usd` before the breach is detected. Upload the artifacts unconditionally - the HTML report is most useful precisely on the runs that failed.

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
| p99 TTFT | 135.9 ms | ramp 5-10 req/s, streaming through gateway |
| Sustained throughput | 10 req/s | zero drops, zero errors (175/175); 20 req/s attempt was admission-bound by `max_in_flight: 4` in the run config, not by the gateway |
| Failover behaviour | 0 client-visible errors (180/180) | provider killed mid-run; circuit breaker re-closed 13.7 s after the kill (1.8 s after provider restore); no request failed during the outage |

## Limitations

Read this before trusting a number.

- **Scenarios: ramp only.** Burst and the gateway failover drill are not implemented in this release. The `ramp` scenario is the whole load model today.
- **The measurement is client-side.** Everything is observed from the load generator's perspective over the network. Network jitter, TLS handshakes, and any proxy between you and the model are inside the numbers. Run from a host that resembles where your users are, and say where you ran it.
- **The `output:` config block is ignored.** Artifact paths come from CLI flags only. The keys are accepted so configs stay forward-compatible.
- **Token counts depend entirely on the server.** An endpoint that does not emit a final usage object yields no token throughput and no cost, and cannot pass a configured `max_cost_usd` gate. That is by design, but it means cost gating does not work everywhere.
- **Percentiles need samples.** A p99 over a handful of requests is a description of those requests, not of the service. The sample count is printed next to every gate; look at it.
- **Single process, single host.** No distributed load generation. Offered load is bounded by what one machine and one `http.Transport` can sustain, and at high rates the generator itself can become the bottleneck. Drop counts are the first place that shows up.
- **OpenAI-compatible Chat Completions only.** One HTTP adapter, parameterized by base URL, model, and headers. No provider SDKs, no plugin system, no Responses API.
- **Not a correctness benchmark.** This measures latency, throughput, failure behaviour, and cost. It says nothing about output quality.

Prior art worth knowing: [llmperf](https://github.com/ray-project/llmperf) (archived) and [GenAI-Perf](https://github.com/triton-inference-server/perf_analyzer) both measure LLM streaming latency and predate this. The differences here are a single static Go binary with no runtime stack, open-loop arrivals with explicit drop accounting, and SLO evaluation that ends in a process exit code rather than a report.

## License

MIT. See [LICENSE](LICENSE).
